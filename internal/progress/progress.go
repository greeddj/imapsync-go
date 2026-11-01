// Package progress provides pre-configured progress bar utilities.
package progress

import (
	"io"
	"os"
	"runtime"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

// Writer is a wrapper around progress.Writer with pre-configured settings.
type Writer struct {
	pw       progress.Writer
	rendered chan struct{}
}

// getTerminalWidth returns the current terminal width, defaulting to 120 if detection fails.
func getTerminalWidth() int {
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return width
	}
	return 120 // default width
}

// NewWriter creates a new progress writer with pre-configured settings and colors.
func NewWriter(numTrackers int, quiet bool) *Writer {
	pw := progress.NewWriter()
	pw.SetAutoStop(false)
	var out io.Writer = os.Stdout
	if quiet {
		out = io.Discard
	}
	pw.SetOutputWriter(out)

	// Calculate optimal lengths based on terminal width.
	//
	// The line layout is "message  PCT  bar  [time; ~ETA: time]" and the
	// total rendered width MUST stay strictly below the terminal width.
	// If it equals terminalWidth, some terminals wrap the trailing space
	// to the next line, and go-pretty's cursor-up + erase-line redraw
	// cycle then strips only the wrapped portion - the original line
	// becomes a "zombie" on screen that the redraw cannot erase.
	//
	// The stats portion (percent + bar + time + separators) is bounded
	// above by ~80 chars for a long sync ("99.99% ⡇...⢸ [1h12m1s; ~ETA: 12m34s]");
	// reserve a margin on top of that so colour codes, double-width braille
	// glyphs on misconfigured fonts, and any future format changes cannot
	// push us over.
	terminalWidth := getTerminalWidth()
	trackerBarLength := 30
	statsReserved := 80 // bar + percent + time + ETA + separators, generous

	const safetyMargin = 4

	messageLength := terminalWidth - statsReserved - safetyMargin
	if messageLength < 40 {
		messageLength = 40    // minimum message length
		trackerBarLength = 20 // shrink tracker if terminal is narrow
	}

	pw.SetTrackerLength(trackerBarLength)
	pw.SetMessageLength(messageLength)
	pw.SetNumTrackersExpected(numTrackers)
	pw.SetStyle(progress.StyleDefault)
	pw.SetTrackerPosition(progress.PositionRight)
	pw.SetUpdateFrequency(time.Millisecond * 100)

	// Configure colors
	pw.Style().Colors = progress.StyleColors{
		Message: text.Colors{text.FgHiCyan},
		Error:   text.Colors{text.BgRed, text.FgBlack},
		Percent: text.Colors{text.FgHiGreen},
		Stats:   text.Colors{text.FgHiBlack},
		Time:    text.Colors{text.FgHiBlack},
		Tracker: text.Colors{text.FgYellow},
		Value:   text.Colors{text.FgCyan},
	}

	// Configure progress bar characters (using Braille dots)
	pw.Style().Chars = progress.StyleChars{
		BoxLeft:    "⡇",
		BoxRight:   "⢸",
		Finished:   "⣿",
		Finished25: "⣀",
		Finished50: "⣤",
		Finished75: "⣶",
		Unfinished: "⣀",
	}

	// Configure visibility
	pw.Style().Visibility.ETA = true
	pw.Style().Visibility.ETAOverall = false
	pw.Style().Visibility.TrackerOverall = false
	pw.Style().Visibility.Time = true
	pw.Style().Visibility.Value = false
	pw.Style().Visibility.Percentage = true
	pw.Style().Options.SnipIndicator = "..."

	// Configure options
	pw.Style().Options.Separator = " "
	pw.Style().Options.DoneString = text.Colors{text.FgGreen}.Sprint("✓ done")
	pw.Style().Options.ErrorString = text.Colors{text.FgRed}.Sprint("✗ error")
	pw.Style().Options.PercentFormat = "%5.2f%%"
	pw.Style().Options.TimeInProgressPrecision = time.Millisecond
	pw.Style().Options.TimeDonePrecision = time.Millisecond

	return &Writer{pw: pw}
}

// SetOutputWriter redirects rendered output to out.
func (w *Writer) SetOutputWriter(out io.Writer) {
	w.pw.SetOutputWriter(out)
}

// WaitForRenderDone spins until the render goroutine finishes its final pass.
func (w *Writer) WaitForRenderDone() {
	for w.pw.IsRenderInProgress() {
		runtime.Gosched()
	}
}

// AppendTracker adds a tracker to the progress writer.
func (w *Writer) AppendTracker(tracker *progress.Tracker) {
	w.pw.AppendTracker(tracker)
}

// Log prints a message above the progress bars.
func (w *Writer) Log(msg string, args ...any) {
	w.pw.Log(msg, args...)
}

// Start begins rendering the progress bars in a goroutine.
func (w *Writer) Start() {
	w.rendered = make(chan struct{})
	go func() {
		defer close(w.rendered)
		w.pw.Render()
	}()
}

// Stop waits for the last render pass. Finished trackers and logs queued
// before the last tracker ends stay on screen. Calling it again is safe.
func (w *Writer) Stop() {
	if w.rendered == nil {
		return
	}
	// go-pretty creates its stop context inside Render, so a Stop that lands
	// before Render starts is lost and the loop would run forever. Repeat it.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		w.pw.Stop()
		select {
		case <-w.rendered:
			return
		case <-tick.C:
		}
	}
}

// NewTracker creates a new tracker with the given message and total.
func NewTracker(message string, total int64) *progress.Tracker {
	return &progress.Tracker{
		Message: message,
		Total:   total,
		Units:   progress.UnitsDefault,
	}
}

// Tracker is an alias for the underlying progress.Tracker type.
type Tracker = progress.Tracker
