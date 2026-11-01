package progress

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

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
