package progress

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForRenders(t *testing.T, out *lockedBuffer, marker string, renders int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), marker) < renders {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d renders of %q within the deadline: %q", renders, marker, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Sequential: swaps the process-wide os.Stdout to catch writes that bypass
// the configured output.
func TestWriter_Stop_whenQuiet_writesNothingToStdout(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	pw := NewWriter(3, true)
	tr := NewTracker("quiet", 1)
	pw.AppendTracker(tr)
	pw.Log("queued")
	tr.MarkAsDone()
	pw.Start()
	pw.Stop()

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("quiet Stop wrote %q to stdout, want nothing", buf.String())
	}
}

func TestWriter_Stop_whenCalledRightAfterStart_endsTheRenderGoroutine(t *testing.T) {
	t.Parallel()

	for i := range 500 {
		w := NewWriter(1, true)
		w.Start()
		w.Stop()

		// Give a render goroutine that missed the stop a chance to start looping.
		for range 4 {
			runtime.Gosched()
		}
		if w.pw.IsRenderInProgress() {
			t.Fatalf("iteration %d: render loop still running after Stop returned", i)
		}
	}
}

func TestWriter_Stop_isSafeBeforeStartAndWhenRepeated(t *testing.T) {
	t.Parallel()

	w := NewWriter(1, true)
	w.Stop()

	w.Start()
	w.Stop()
	w.Stop()

	if w.pw.IsRenderInProgress() {
		t.Error("render loop still running after Stop")
	}
}

func TestWriter_Stop_When_LogIsQueuedBeforeTheLastTrackerEnds_Then_OutputEndsWithTheLog(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	pw := NewWriter(1, false)
	pw.SetOutputWriter(&out)
	tr := NewTracker("work", 1)
	pw.AppendTracker(tr)
	pw.Log("late log")
	tr.MarkAsDone()
	pw.Start()
	pw.Stop()

	got := out.String()
	if !strings.HasSuffix(got, "late log\n") {
		t.Errorf("output does not end with the log line: %q", got)
	}
	if !strings.Contains(got, "✓ done") {
		t.Errorf("output does not show the finished tracker: %q", got)
	}
	if strings.Contains(got, "\x1b[A") {
		t.Errorf("output moves the cursor up: %q", got)
	}
}

func TestNewWriter_When_Built_Then_StyleHasAnIndeterminateIndicator(t *testing.T) {
	t.Parallel()

	for _, quiet := range []bool{false, true} {
		w := NewWriter(1, quiet)
		if w.pw.Style().Chars.Indeterminate == nil {
			t.Errorf("quiet=%v: Indeterminate is nil, go-pretty would panic on an indeterminate tracker", quiet)
		}
	}
}

func TestWriter_Start_When_TrackerIsIndeterminate_Then_DrawsTheIndicatorWithoutPanic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup            func(*Tracker)
		name             string
		message          string
		wantPercent      string
		total            int64
		autoStopDisabled bool
	}{
		{
			name:        "total is zero",
			message:     "total-is-zero",
			total:       0,
			wantPercent: "???",
		},
		{
			name:             "value is above total and auto stop is disabled",
			message:          "value-above-total",
			total:            5,
			autoStopDisabled: true,
			setup:            func(tr *Tracker) { tr.Increment(7) },
			wantPercent:      "140.00%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out lockedBuffer
			pw := NewWriter(1, false)
			pw.SetOutputWriter(&out)
			t.Cleanup(pw.Stop)

			tr := NewTracker(tt.message, tt.total)
			tr.AutoStopDisabled = tt.autoStopDisabled
			pw.AppendTracker(tr)
			if tt.setup != nil {
				tt.setup(tr)
			}
			pw.Start()

			waitForRenders(t, &out, tt.message, 2)
			pw.Stop()

			got := out.String()
			for _, want := range []string{tt.message, tt.wantPercent, "⡇", "⢸", "⣿⣿⣿"} {
				if !strings.Contains(got, want) {
					t.Errorf("output lacks %q: %q", want, got)
				}
			}
		})
	}
}
