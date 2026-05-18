// Package progress provides pre-configured progress bar utilities.
package progress

import (
	"fmt"
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
	pw          progress.Writer
	numTrackers int
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
	if quiet {
		pw.SetOutputWriter(io.Discard)
	} else {
		pw.SetOutputWriter(os.Stdout)
	}

	// Calculate optimal lengths based on terminal width.
	//
	// The line layout is "message  PCT  bar  [time; ~ETA: time]" and the
	// total rendered width MUST stay strictly below the terminal width.
	// If it equals terminalWidth, some terminals wrap the trailing space
	// to the next line, and go-pretty's cursor-up + erase-line redraw
	// cycle then strips only the wrapped portion — the original line
	// becomes a "zombie" on screen that StopAndClear cannot erase.
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

	return &Writer{pw: pw, numTrackers: numTrackers}
}

// SetOutputWriter redirects rendered output to out.
func (w *Writer) SetOutputWriter(out io.Writer) { w.pw.SetOutputWriter(out) }

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
	go w.pw.Render()
}

// Stop stops the progress writer without clearing.
func (w *Writer) Stop() {
	w.pw.Stop()
}

// StopAndClear stops the progress writer and erases the rendered trackers
// from the terminal. Line count is taken from the NewWriter argument so
// callers never have to keep that number in sync by hand.
func (w *Writer) StopAndClear() {
	// Wait for final rendering
	time.Sleep(300 * time.Millisecond)

	// Stop the writer
	w.pw.Stop()

	// After Stop, the cursor sits on a blank line one below the last
	// rendered tracker. Go up numTrackers times, erasing each tracker
	// line as we pass, then return to column 0 of the topmost erased
	// row so the next caller-printed line starts there cleanly.
	for range w.numTrackers {
		fmt.Print("\033[A\033[K")
	}
	fmt.Print("\r")
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
