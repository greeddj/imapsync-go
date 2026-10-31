package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/termsafe"
	"github.com/jedib0t/go-pretty/v6/text"
)

// maxTrackerReason counts bytes and includes the trailing ellipsis.
const maxTrackerReason = 40

// Tracker-line glyph palette. go-pretty does not intercept inline ANSI in the
// tracker Message, so colouring the counters here just works.
var (
	trackerSyncedStyle = text.Colors{text.FgGreen}
	trackerTotalStyle  = text.Colors{text.FgHiBlack}
	trackerErrorStyle  = text.Colors{text.FgRed}
)

// syncWorker pairs one source and one destination connection. Workers are
// pre-allocated once per sync and reused across every plan handed to them,
// so we pay the TLS handshake + LOGIN + LIST cost exactly once per worker
// instead of once per plan-chunk.
type syncWorker struct {
	src *client.Client
	dst *client.Client
}

// syncWorkerPool owns a fixed-size set of syncWorkers. close() Logs out of
// every worker; partial failures during construction are cleaned up by
// newSyncWorkerPool itself.
type syncWorkerPool struct {
	all []*syncWorker
}

func (p *syncWorkerPool) close() {
	clients := make([]*client.Client, 0, 2*len(p.all))
	for _, w := range p.all {
		clients = append(clients, w.src, w.dst)
	}
	logoutAll(clients...)
}

// logoutAll logs the clients out at the same time, so a silent server costs
// one logout limit in total, not one per client. Nil entries are skipped.
func logoutAll(clients ...*client.Client) {
	var wg sync.WaitGroup
	for _, c := range clients {
		if c != nil {
			wg.Go(func() { _ = c.Logout() })
		}
	}
	wg.Wait()
}

// newSyncWorkerPool constructs n workers up-front. If any single worker fails
// to connect, every preceding worker is closed before the error returns -
// otherwise we'd leak partially-open IMAP sessions.
func newSyncWorkerPool(ctx context.Context, cfg *config.Config, srcOpts, dstOpts client.Options, n int) (*syncWorkerPool, error) {
	pool := &syncWorkerPool{all: make([]*syncWorker, 0, n)}

	for i := range n {
		if err := ctx.Err(); err != nil {
			pool.close()
			return nil, err
		}
		srcOpts.Label = fmt.Sprintf("%s-w%d", cfg.Src.Label, i+1)
		s, err := client.New(ctx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
		if err != nil {
			pool.close()
			return nil, fmt.Errorf("worker %d source connect: %w", i+1, err)
		}

		dstOpts.Label = fmt.Sprintf("%s-w%d", cfg.Dst.Label, i+1)
		d, err := client.New(ctx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
		if err != nil {
			pool.all = append(pool.all, &syncWorker{src: s})
			pool.close()
			return nil, fmt.Errorf("worker %d destination connect: %w", i+1, err)
		}

		pool.all = append(pool.all, &syncWorker{src: s, dst: d})
	}
	return pool, nil
}

// stopSide names the server whose throttled reply stopped the run.
type stopSide uint8

const (
	sideSource stopSide = iota + 1
	sideDestination
)

func (s stopSide) String() string {
	if s == sideSource {
		return "source"
	}
	return "destination"
}

// throttleCause is the first throttled reply of a run.
type throttleCause struct {
	err  error
	side stopSide
}

// throttleStop is the stop of one run. The first throttled reply wins, and every worker reads it before a plan, a batch and an APPEND.
type throttleStop struct {
	cause atomic.Pointer[throttleCause]
}

func (s *throttleStop) record(side stopSide, err error) {
	s.cause.CompareAndSwap(nil, &throttleCause{err: err, side: side})
}

// recordThrottled records err as the stop and reports true only when err is a throttled reply.
func (s *throttleStop) recordThrottled(side stopSide, err error) bool {
	if !client.IsThrottled(err) {
		return false
	}
	s.record(side, err)
	return true
}

func (s *throttleStop) get() *throttleCause { return s.cause.Load() }

func (s *throttleStop) stopped() bool { return s.cause.Load() != nil }

// halt is the halt hook of StreamMessagesByUIDs.
func (s *throttleStop) halt() error {
	if s.stopped() {
		return errPlanStopped
	}
	return nil
}

func isPlanStopped(err error) bool { return errors.Is(err, errPlanStopped) }

// markPlanStopped shows a plan that the run stopped before it copied anything.
func markPlanStopped(tr *progress.Tracker, p FolderSyncPlan, planIdx, planCount int) {
	tr.UpdateMessage(fmt.Sprintf("%d/%d Stopped %s → %s",
		planIdx+1, planCount, termsafe.Escape(p.SourceFolder), termsafe.Escape(p.DestinationFolder)))
	tr.MarkAsErrored()
}

// pendingCause says why a planned UID waits for a standby copy.
type pendingCause uint8

const (
	causeRefusedBody  pendingCause = iota // the source delivered the message, but its body was empty or missing
	causeNotDelivered                     // the source did not deliver the message, and it still holds it
	causeGone                             // the source no longer holds the message
)

// runFolderSync copies one plan. Each planned UID ends as synced, errors or gone, unless the context ends. After a stop, every UID not copied is an error. planIdx is zero-based.
func runFolderSync(ctx context.Context, w *syncWorker, p FolderSyncPlan, stop *throttleStop, tr *progress.Tracker, planIdx, planCount int, pw *progress.Writer, verbose bool) (synced, errors, gone int) {
	// Display only: the IMAP calls below keep the exact names from the plan.
	srcShown, dstShown := termsafe.Escape(p.SourceFolder), termsafe.Escape(p.DestinationFolder)

	if err := ctx.Err(); err != nil {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled", planIdx+1, planCount))
		tr.MarkAsErrored()
		return 0, 0, 0
	}
	if stop.stopped() {
		markPlanStopped(tr, p, planIdx, planCount)
		return 0, p.NewMessages, 0
	}

	tr.UpdateTotal(int64(p.NewMessages))
	tr.UpdateMessage(fmt.Sprintf("%d/%d %s → %s", planIdx+1, planCount, srcShown, dstShown))

	// lastUpdate throttles the per-message message-string update - it lives
	// on the worker goroutine, so an unsynchronized time.Time is fine.
	var lastUpdate time.Time
	var lastErrMsg string

	updateTrackerMsg := func() {
		syncedPart := trackerSyncedStyle.Sprintf("%d↑", synced)
		totalPart := trackerTotalStyle.Sprintf("Σ%d", p.NewMessages)
		base := fmt.Sprintf("%d/%d (%s %s) %s → %s",
			planIdx+1, planCount, syncedPart, totalPart, srcShown, dstShown)
		if errors == 0 {
			tr.UpdateMessage(base)
			return
		}
		errPart := trackerErrorStyle.Sprintf("%d✗", errors)
		// The reason is escaped but neither %q-quoted nor coloured, so it reads like
		// the server message. The ANSI reset from errPart restores the default foreground.
		tr.UpdateMessage(fmt.Sprintf("%s (%s %s)", base, errPart, trackerReason(lastErrMsg)))
	}

	refreshTracker := func() {
		if now := time.Now(); now.Sub(lastUpdate) > 100*time.Millisecond {
			lastUpdate = now
			updateTrackerMsg()
		}
	}
	reportCanceled := func() {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled %s → %s",
			planIdx+1, planCount, srcShown, dstShown))
		tr.MarkAsErrored()
	}
	finishTracker := func() {
		if errors > 0 {
			tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d (errors: %d) %s → %s",
				planIdx+1, planCount, synced, errors, srcShown, dstShown))
			tr.MarkAsErrored()
		} else {
			tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d %s → %s",
				planIdx+1, planCount, synced, srcShown, dstShown))
			tr.MarkAsDone()
		}
	}

	// A destination that gave up would make the plan download bodies that it cannot append, so the source is not read.
	if err := w.dst.EnsureSession(ctx); err != nil {
		if ctx.Err() != nil {
			reportCanceled()
			return 0, 0, 0
		}
		stop.recordThrottled(sideDestination, err)
		errors = p.NewMessages
		finishTracker()
		return 0, errors, 0
	}

	// destinationUsable ends the plan when a failed APPEND left the destination without a session that the gate allows.
	destinationUsable := func() error {
		if stop.stopped() {
			return errPlanStopped
		}
		err := w.dst.EnsureSession(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stop.recordThrottled(sideDestination, err)
		return errPlanStopped
	}

	// pending holds the planned UIDs that a standby copy can still replace. The cause decides whether one that no copy replaces counts as an error or as gone.
	var pending map[uint32]pendingCause
	addPending := func(uid uint32, cause pendingCause) {
		if pending == nil {
			pending = make(map[uint32]pendingCause)
		}
		pending[uid] = cause
	}

	sortedUIDs := slices.Clone(p.SrcUIDs)
	slices.Sort(sortedUIDs)
	delivered := make([]bool, len(sortedUIDs))
	deliveredCount := 0

	streamErr := w.src.StreamMessagesByUIDs(ctx, p.SourceFolder, p.SrcUIDValidity, p.SrcUIDs, stop.halt, func(msg client.Message) error {
		// A stop comes before the mark, so the stream counts this message once among the stranded UIDs.
		if stop.stopped() {
			return errPlanStopped
		}
		if i, found := slices.BinarySearch(sortedUIDs, msg.UID()); found && !delivered[i] {
			delivered[i] = true
			deliveredCount++
		}
		if err := w.dst.AppendMessage(ctx, p.DestinationFolder, msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			throttled := stop.recordThrottled(sideDestination, err)
			refused := refusedBody(err)
			if refused && len(p.standbysOf(msg.UID())) > 0 {
				addPending(msg.UID(), causeRefusedBody)
			} else {
				errors++
				lastErrMsg = err.Error()
			}
			// Without --verbose we never persist per-message failures to
			// the log writer: at high error rates that floods the screen
			// with hundreds of lines and scrolls the progress bars out.
			// Operators still see the counter and the last reason via the
			// tracker line below.
			if verbose {
				pw.Log("Failed to append message to %s: %s", dstShown, termsafe.Escape(err.Error()))
			}
			refreshTracker()
			switch {
			case throttled:
				return errPlanStopped
			case refused:
				return nil
			default:
				return destinationUsable()
			}
		}
		synced++
		tr.Increment(1)
		refreshTracker()
		if verbose {
			pw.Log("Synced %d/%d to %s, processed msg id %s",
				synced, p.NewMessages, dstShown, termsafe.Escape(msg.MessageID()))
		}
		// AppendMessage that completed in flight after ctx cancel must not
		// continue iterating; the next message would race with Cancel() racing
		// against the worker.
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	})
	if ctx.Err() != nil {
		reportCanceled()
		return synced, errors, 0
	}

	// The plan prints one stream line: the first failure of any stream, with the number of batches that failed in all streams.
	var planFail client.StreamError
	var failShown string
	noteStreamFailure := func(shown string, err error) error {
		batches, throttle, first := streamFailure(err)
		if planFail.FailedBatches == 0 {
			planFail.First, failShown = first, shown
		}
		planFail.FailedBatches += batches
		return throttle
	}
	// planStopped ends the plan: no UID SEARCH, no standby stream, and every UID not copied is an error.
	planStopped := isPlanStopped(streamErr)
	if streamErr != nil && !planStopped {
		if throttle := noteStreamFailure(srcShown, streamErr); throttle != nil {
			stop.record(sideSource, throttle)
			planStopped = true
		}
	}

	// A UID that the stream did not deliver ends here as an error or as gone.
	var strandedErrors, strandedGone int
	undeliveredCount := len(sortedUIDs) - deliveredCount
	if undeliveredCount > 0 && stop.stopped() {
		planStopped = true
	}
	if planStopped {
		strandedErrors = undeliveredCount
	} else if undeliveredCount > 0 {
		undelivered := make([]uint32, 0, undeliveredCount)
		for i, uid := range sortedUIDs {
			if !delivered[i] {
				undelivered = append(undelivered, uid)
			}
		}
		held, err := w.src.ExistingUIDs(ctx, p.SourceFolder, p.SrcUIDValidity, undelivered)
		if ctx.Err() != nil {
			reportCanceled()
			return synced, errors, 0
		}
		if err != nil && stop.recordThrottled(sideSource, err) {
			planStopped = true
			strandedErrors = undeliveredCount
		} else {
			// A failed check proves nothing, so every UID of the call counts as still held.
			if err != nil {
				held = undelivered
			}
			next := 0
			for _, uid := range undelivered {
				for next < len(held) && held[next] < uid {
					next++
				}
				cause := causeGone
				if next < len(held) && held[next] == uid {
					cause = causeNotDelivered
				}
				switch {
				case len(p.standbysOf(uid)) > 0:
					addPending(uid, cause)
				case cause == causeGone:
					strandedGone++
				default:
					strandedErrors++
				}
			}
		}
	}

	// The main FETCH has ended, so the source session can run the next command.
	for source, standby := range p.standbySources {
		if planStopped || stop.stopped() {
			planStopped = true
			break
		}
		uids, owner := standbyRequest(&p, pending, uint32(source))
		if len(uids) == 0 || ctx.Err() != nil {
			continue
		}
		standbyShown := termsafe.Escape(standby.folder)
		err := w.src.StreamMessagesByUIDs(ctx, standby.folder, standby.uidValidity, uids, stop.halt, func(msg client.Message) error {
			if stop.stopped() {
				return errPlanStopped
			}
			planned := owner[msg.UID()]
			if _, waiting := pending[planned]; !waiting {
				return nil
			}
			err := w.dst.AppendMessage(ctx, p.DestinationFolder, msg)
			switch {
			case err == nil:
				delete(pending, planned)
				synced++
				tr.Increment(1)
				refreshTracker()
				if verbose {
					pw.Log("Copied message %s to %s from %s: the copy in %s has no usable body",
						termsafe.Escape(msg.MessageID()), dstShown, standbyShown, srcShown)
				}
				return ctx.Err()
			case ctx.Err() != nil:
				return ctx.Err()
			case refusedBody(err):
				// The planned UID stays pending, so a later source can still replace it.
			default:
				delete(pending, planned)
				errors++
				lastErrMsg = err.Error()
				if verbose {
					pw.Log("Failed to append message to %s: %s", dstShown, termsafe.Escape(err.Error()))
				}
				refreshTracker()
				if stop.recordThrottled(sideDestination, err) {
					return errPlanStopped
				}
				return destinationUsable()
			}
			return nil
		})
		if ctx.Err() != nil {
			break
		}
		if isPlanStopped(err) {
			planStopped = true
			break
		}
		if err != nil {
			if throttle := noteStreamFailure(standbyShown, err); throttle != nil {
				stop.record(sideSource, throttle)
				planStopped = true
				break
			}
		}
	}
	if ctx.Err() != nil {
		reportCanceled()
		return synced, errors, 0
	}
	errors += strandedErrors
	gone += strandedGone
	for _, cause := range pending {
		if cause == causeGone && !planStopped {
			gone++
		} else {
			errors++
		}
	}

	// The logs come before MarkAsDone: go-pretty drops a queued log line once no tracker is active.
	if planFail.FailedBatches > 0 {
		pw.Log("Stream error for folder %s: %s", failShown, termsafe.Escape(planFail.Error()))
	}
	if gone > 0 {
		pw.Log("  ℹ️  Source folder %s no longer holds %d of the %d planned messages for %s, so they are not copied",
			srcShown, gone, p.NewMessages, dstShown)
	}
	finishTracker()
	return synced, errors, gone
}

// streamFailure reads a StreamMessagesByUIDs error: the failed batches, the throttled reply that ended the stream, if any, and the first failure. Any other error counts as one batch.
func streamFailure(err error) (batches int, throttle error, first error) {
	if se, ok := errors.AsType[*client.StreamError](err); ok {
		if client.IsThrottled(se.Stop) {
			throttle = se.Stop
		}
		return se.FailedBatches, throttle, se.First
	}
	return 1, nil, err
}

func refusedBody(err error) bool {
	return errors.Is(err, client.ErrEmptyBody) || errors.Is(err, client.ErrMissingBody)
}

// owner maps each returned standby UID to the planned UID that it can replace.
func standbyRequest(p *FolderSyncPlan, pending map[uint32]pendingCause, source uint32) (uids []uint32, owner map[uint32]uint32) {
	if len(pending) == 0 {
		return nil, nil
	}
	uids = make([]uint32, 0, len(pending))
	owner = make(map[uint32]uint32, len(pending))
	for planned := range pending {
		for _, c := range p.standbysOf(planned) {
			if c.source == source {
				uids = append(uids, c.uid)
				owner[c.uid] = planned
			}
		}
	}
	return uids, owner
}

// A long server message would wrap the tracker line and break the rendered bar.
func trackerReason(msg string) string {
	reason := termsafe.Escape(msg)
	if len(reason) <= maxTrackerReason {
		return reason
	}
	cut := maxTrackerReason - len("...")
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + "..."
}
