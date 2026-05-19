package app

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/jedib0t/go-pretty/v6/text"
)

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
	for _, w := range p.all {
		_ = w.src.Logout()
		_ = w.dst.Logout()
	}
}

// newSyncWorkerPool constructs n workers up-front. If any single worker fails
// to connect, every preceding worker is closed before the error returns —
// otherwise we'd leak partially-open IMAP sessions.
func newSyncWorkerPool(ctx context.Context, cfg *config.Config, srcOpts, dstOpts client.Options, n int) (*syncWorkerPool, error) {
	pool := &syncWorkerPool{all: make([]*syncWorker, 0, n)}

	for i := range n {
		if err := ctx.Err(); err != nil {
			pool.close()
			return nil, err
		}
		s, err := client.New(ctx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
		if err != nil {
			pool.close()
			return nil, fmt.Errorf("worker %d source connect: %w", i+1, err)
		}
		s.SetPrefix(fmt.Sprintf("%s-w%d", cfg.Src.Label, i+1))

		d, err := client.New(ctx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
		if err != nil {
			_ = s.Logout()
			pool.close()
			return nil, fmt.Errorf("worker %d destination connect: %w", i+1, err)
		}
		d.SetPrefix(fmt.Sprintf("%s-w%d", cfg.Dst.Label, i+1))

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
	if err := ctx.Err(); err != nil {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Canceled", planIdx+1, planCount))
		tr.MarkAsErrored()
		return 0, 0
	}

	tr.UpdateTotal(int64(p.NewMessages))
	tr.UpdateMessage(fmt.Sprintf("%d/%d %s → %s", planIdx+1, planCount, p.SourceFolder, p.DestinationFolder))

	// lastUpdate throttles the per-message message-string update — it lives
	// on the worker goroutine, so an unsynchronized time.Time is fine.
	var lastUpdate time.Time
	var lastErrMsg string

	updateTrackerMsg := func() {
		syncedPart := trackerSyncedStyle.Sprintf("%d↑", synced)
		totalPart := trackerTotalStyle.Sprintf("Σ%d", p.NewMessages)
		base := fmt.Sprintf("%d/%d (%s %s) %s → %s",
			planIdx+1, planCount, syncedPart, totalPart, p.SourceFolder, p.DestinationFolder)
		if errors == 0 {
			tr.UpdateMessage(base)
			return
		}
		// Trim the inline reason so a long server message doesn't wrap
		// the tracker line and break the rendered bar.
		reason := lastErrMsg
		if len(reason) > 40 {
			reason = reason[:37] + "..."
		}
		errPart := trackerErrorStyle.Sprintf("%d✗", errors)
		// reason rendered without Sprintf %q so it reads as the original
		// server message — no \"escaped\" quoting, no inline colour. The
		// preceding ANSI reset from errPart hands control back to the
		// terminal's default foreground.
		tr.UpdateMessage(fmt.Sprintf("%s (%s %s)", base, errPart, reason))
	}

	streamErr := w.src.StreamMessagesByUIDs(ctx, p.SourceFolder, p.SrcUIDs, func(msg *imap.Message) error {
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
				pw.Log("Failed to append message to %s: %v", p.DestinationFolder, err)
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
				synced, p.NewMessages, p.DestinationFolder, msg.Envelope.MessageId)
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
			planIdx+1, planCount, p.SourceFolder, p.DestinationFolder))
		tr.MarkAsErrored()
		return synced, errors
	}
	if streamErr != nil {
		pw.Log("Stream error for folder %s: %v", p.SourceFolder, streamErr)
		errors++
	}

	if errors > 0 {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d (errors: %d) %s → %s",
			planIdx+1, planCount, synced, errors, p.SourceFolder, p.DestinationFolder))
		tr.MarkAsErrored()
	} else {
		tr.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d %s → %s",
			planIdx+1, planCount, synced, p.SourceFolder, p.DestinationFolder))
		tr.MarkAsDone()
	}
	return synced, errors
}
