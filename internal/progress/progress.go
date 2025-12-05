// Package progress provides pre-configured progress bar utilities.
package progress

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

// Writer is a wrapper around progress.Writer with pre-configured settings.
type Writer struct {
	pw progress.Writer
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

	// Calculate optimal lengths based on terminal width
	terminalWidth := getTerminalWidth()

	// Reserve space for: percentage (8 chars), tracker bar (30 chars), stats/time (~25 chars), separators (6 chars)
	// Total reserved: ~69 chars
	trackerBarLength := 30
	statsReserved := 69

	// Remaining space goes to message
	messageLength := terminalWidth - statsReserved
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

// StopAndClear stops the progress writer and clears the output.
func (w *Writer) StopAndClear(numLines int) {
	// Wait for final rendering
	time.Sleep(300 * time.Millisecond)

	// Stop the writer
	w.pw.Stop()

	// Clear progress output
	fmt.Print("\r")
	for i := 0; i < numLines; i++ {
		fmt.Print("\033[K\r")
	}
	fmt.Println()
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
