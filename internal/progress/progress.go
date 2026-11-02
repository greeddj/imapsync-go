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

const updateFrequency = 100 * time.Millisecond

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

	// A line that fills the terminal width wraps; the redraw erases only the wrapped part, so the first row stays on screen as a "zombie".
	terminalWidth := getTerminalWidth()
	trackerBarLength := 30
	statsReserved := 80 // bar + percent + time + ETA + separators, generous

	// The margin for colour codes, double-width glyphs and format changes keeps the line below the terminal width.
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
	pw.SetUpdateFrequency(updateFrequency)

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
		// go-pretty calls this for Total 0 or a value above Total, and nil panics the render goroutine. The generator keeps state, so each Writer builds its own.
		Indeterminate: progress.IndeterminateIndicatorMovingBackAndForth("⣿⣿⣿", updateFrequency/2),
		Unfinished:    "⣀",
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

// Stop waits for the last render pass: finished trackers and logs queued before the last tracker ends stay on screen. Safe to call again.
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
