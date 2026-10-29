package app

import (
	"context"
	"fmt"
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

// runFolderSync executes one FolderSyncPlan on the given worker and returns
// (synced, errors). It owns the lifecycle of one tracker and posts log lines
// to pw on a per-message error.
//
// planIdx is zero-based; planCount is len(activePlans) for human display.
func runFolderSync(ctx context.Context, w *syncWorker, p FolderSyncPlan, tr *progress.Tracker, planIdx, planCount int, pw *progress.Writer, verbose bool) (synced, errors int) {
	// Display only: the IMAP calls below keep the exact names from the plan.
	srcShown, dstShown := termsafe.Escape(p.SourceFolder), termsafe.Escape(p.DestinationFolder)

	if err := ctx.Err(); err != nil {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled", planIdx+1, planCount))
		tr.MarkAsErrored()
		return 0, 0
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

	streamErr := w.src.StreamMessagesByUIDs(ctx, p.SourceFolder, p.SrcUIDValidity, p.SrcUIDs, func(msg client.Message) error {
		if err := w.dst.AppendMessage(ctx, p.DestinationFolder, msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errors++
			lastErrMsg = err.Error()
			// Without --verbose we never persist per-message failures to
			// the log writer: at high error rates that floods the screen
			// with hundreds of lines and scrolls the progress bars out.
			// Operators still see the counter and the last reason via the
			// tracker line below.
			if verbose {
				pw.Log("Failed to append message to %s: %s", dstShown, termsafe.Escape(err.Error()))
			}
			if now := time.Now(); now.Sub(lastUpdate) > 100*time.Millisecond {
				lastUpdate = now
				updateTrackerMsg()
			}
			return nil
		}
		synced++
		tr.Increment(1)
		if now := time.Now(); now.Sub(lastUpdate) > 100*time.Millisecond {
			lastUpdate = now
			updateTrackerMsg()
		}
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
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled %s → %s",
			planIdx+1, planCount, srcShown, dstShown))
		tr.MarkAsErrored()
		return synced, errors
	}
	if streamErr != nil {
		pw.Log("Stream error for folder %s: %s", srcShown, termsafe.Escape(streamErr.Error()))
		errors++
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
	return synced, errors
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
