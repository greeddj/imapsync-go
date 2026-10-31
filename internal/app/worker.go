package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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

// pendingCause says why a planned UID waits for a standby copy.
type pendingCause uint8

const (
	causeRefusedBody  pendingCause = iota // the source delivered the message, but its body was empty or missing
	causeNotDelivered                     // the source did not deliver the message, and it still holds it
	causeGone                             // the source no longer holds the message
)

// runFolderSync copies one plan on the worker. Each planned UID ends as exactly one of synced, errors or gone, unless the context ends. planIdx is zero-based.
func runFolderSync(ctx context.Context, w *syncWorker, p FolderSyncPlan, tr *progress.Tracker, planIdx, planCount int, pw *progress.Writer, verbose bool) (synced, errors, gone int) {
	// Display only: the IMAP calls below keep the exact names from the plan.
	srcShown, dstShown := termsafe.Escape(p.SourceFolder), termsafe.Escape(p.DestinationFolder)

	if err := ctx.Err(); err != nil {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled", planIdx+1, planCount))
		tr.MarkAsErrored()
		return 0, 0, 0
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

	streamErr := w.src.StreamMessagesByUIDs(ctx, p.SourceFolder, p.SrcUIDValidity, p.SrcUIDs, func(msg client.Message) error {
		if i, found := slices.BinarySearch(sortedUIDs, msg.UID()); found && !delivered[i] {
			delivered[i] = true
			deliveredCount++
		}
		if err := w.dst.AppendMessage(ctx, p.DestinationFolder, msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if refusedBody(err) && len(p.standbysOf(msg.UID())) > 0 {
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
			return nil
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
	noteStreamFailure := func(shown string, err error) bool {
		batches, throttled, first := streamFailure(err)
		if planFail.FailedBatches == 0 {
			planFail.First, failShown = first, shown
		}
		planFail.FailedBatches += batches
		return throttled
	}
	throttled := streamErr != nil && noteStreamFailure(srcShown, streamErr)

	// A UID that the stream did not deliver ends here as an error or as gone.
	var strandedErrors, strandedGone int
	if throttled {
		// A throttled source gets no UID SEARCH. A UIDVALIDITY stop takes the check below, which fails before UID SEARCH, so a standby from another folder can still replace each UID.
		strandedErrors = len(sortedUIDs) - deliveredCount
	} else if deliveredCount < len(sortedUIDs) {
		undelivered := make([]uint32, 0, len(sortedUIDs)-deliveredCount)
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

	// The main FETCH has ended, so the source session can run the next command.
	for source, standby := range p.standbySources {
		uids, owner := standbyRequest(&p, pending, uint32(source))
		if len(uids) == 0 || ctx.Err() != nil {
			continue
		}
		standbyShown := termsafe.Escape(standby.folder)
		err := w.src.StreamMessagesByUIDs(ctx, standby.folder, standby.uidValidity, uids, func(msg client.Message) error {
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
			}
			return nil
		})
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			noteStreamFailure(standbyShown, err)
		}
	}
	if ctx.Err() != nil {
		reportCanceled()
		return synced, errors, 0
	}
	errors += strandedErrors
	gone += strandedGone
	for _, cause := range pending {
		if cause == causeGone {
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
	if errors > 0 {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d (errors: %d) %s → %s",
			planIdx+1, planCount, synced, errors, srcShown, dstShown))
		tr.MarkAsErrored()
	} else {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d %s → %s",
			planIdx+1, planCount, synced, srcShown, dstShown))
		tr.MarkAsDone()
	}
	return synced, errors, gone
}

// streamFailure reads a StreamMessagesByUIDs error: the failed batches, whether a throttled reply ended the stream and the first failure. Any other error counts as one batch.
func streamFailure(err error) (batches int, throttled bool, first error) {
	if se, ok := errors.AsType[*client.StreamError](err); ok {
		return se.FailedBatches, se.Stop != nil && !errors.Is(se.Stop, client.ErrUIDValidityChanged), se.First
	}
	return 1, false, err
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
