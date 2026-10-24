package progress

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestWriter_StopAndClear_emitsOneEscapePerTracker asserts that the count of
// "\033[K" escape sequences matches the numTrackers passed to NewWriter, and
// that they go to the configured output. The escape erases one terminal line;
// one per tracker is what visually clears the rendered bars.
func TestWriter_StopAndClear_emitsOneEscapePerTracker(t *testing.T) {
	t.Parallel()

	const want = 3

	var out bytes.Buffer
	pw := NewWriter(want, false)
	pw.SetOutputWriter(&out)
	pw.Start()
	pw.StopAndClear()

	got := out.String()
	if n := strings.Count(got, "\033[A\033[K"); n != want {
		t.Errorf("erase sequences = %d, want %d (output: %q)", n, want, got)
	}
	if !strings.HasSuffix(got, "\r") {
		t.Errorf("output does not end with a carriage return: %q", got)
	}
}

// Sequential: swaps the process-wide os.Stdout to catch writes that bypass
// the configured output.
func TestWriter_StopAndClear_whenQuiet_writesNothingToStdout(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	pw := NewWriter(3, true)
	pw.Start()
	pw.StopAndClear()

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("quiet StopAndClear wrote %q to stdout, want nothing", buf.String())
	}
}
