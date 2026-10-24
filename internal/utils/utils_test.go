package utils

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		bytes    uint64
	}{
		{
			name:     "bytes",
			bytes:    512,
			expected: "512 B",
		},
		{
			name:     "kilobytes",
			bytes:    2048,
			expected: "2.00 KB",
		},
		{
			name:     "megabytes",
			bytes:    5242880,
			expected: "5.00 MB",
		},
		{
			name:     "gigabytes",
			bytes:    3221225472,
			expected: "3.00 GB",
		},
		{
			name:     "zero bytes",
			bytes:    0,
			expected: "0 B",
		},
		{
			name:     "1 KB exactly",
			bytes:    1024,
			expected: "1.00 KB",
		},
		{
			name:     "1 MB exactly",
			bytes:    1048576,
			expected: "1.00 MB",
		},
		{
			name:     "1 GB exactly",
			bytes:    1073741824,
			expected: "1.00 GB",
		},
		{
			name:     "fractional KB",
			bytes:    1536,
			expected: "1.50 KB",
		},
		{
			name:     "fractional MB",
			bytes:    1572864,
			expected: "1.50 MB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSize(tt.bytes)
			if result != tt.expected {
				t.Errorf("FormatSize(%d) = %s; want %s", tt.bytes, result, tt.expected)
			}
		})
	}
}

// promptNotifier is a goroutine-safe io.Writer that signals ready once the
// prompt marker has been written.
type promptNotifier struct {
	ready chan struct{}
	buf   bytes.Buffer
	mu    sync.Mutex
	once  sync.Once
}

func newPromptNotifier() *promptNotifier {
	return &promptNotifier{ready: make(chan struct{})}
}

func (p *promptNotifier) Write(b []byte) (int, error) {
	p.mu.Lock()
	n, err := p.buf.Write(b)
	seen := strings.Contains(p.buf.String(), "[y/N]")
	p.mu.Unlock()
	if seen {
		p.once.Do(func() { close(p.ready) })
	}
	return n, err
}

func TestAskConfirm_When_AnswerGiven_Then_ReturnsDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "lowercase y confirms", input: "y\n", want: true},
		{name: "full word yes confirms", input: "yes\n", want: true},
		{name: "uppercase Y confirms", input: "Y\n", want: true},
		{name: "mixed case Yes with spaces confirms", input: "  Yes  \n", want: true},
		{name: "lowercase n declines", input: "n\n", want: false},
		{name: "full word no declines", input: "no\n", want: false},
		{name: "bare newline declines", input: "\n", want: false},
		{name: "invalid answer then yes confirms", input: "maybe\ny\n", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			got, err := AskConfirm(context.Background(), bufio.NewReader(strings.NewReader(tt.input)), &out, "Go on?")
			if err != nil {
				t.Fatalf("AskConfirm: %v", err)
			}
			if got != tt.want {
				t.Errorf("AskConfirm = %v, want %v", got, tt.want)
			}
			if !strings.Contains(out.String(), "Go on? [y/N]: ") {
				t.Errorf("output %q does not show the prompt", out.String())
			}
		})
	}
}

func TestAskConfirm_When_AllAnswersInvalid_Then_DeclinesAfterThreePrompts(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	got, err := AskConfirm(context.Background(), bufio.NewReader(strings.NewReader("a\nb\nc\n")), &out, "Go on?")
	if err != nil {
		t.Fatalf("AskConfirm: %v", err)
	}
	if got {
		t.Error("AskConfirm = true, want false after three invalid answers")
	}
	if n := strings.Count(out.String(), "Go on? [y/N]: "); n != 3 {
		t.Errorf("prompt shown %d times, want 3\noutput: %q", n, out.String())
	}
	if n := strings.Count(out.String(), "Please answer with"); n != 2 {
		t.Errorf("retry hint shown %d times, want 2 (none after the last attempt)\noutput: %q", n, out.String())
	}
}

func TestAskConfirm_When_InputEnds_Then_ReturnsReadError(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	got, err := AskConfirm(context.Background(), bufio.NewReader(strings.NewReader("")), &out, "Go on?")
	if err == nil {
		t.Fatal("AskConfirm returned nil error on empty input")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("error %v does not wrap io.EOF", err)
	}
	if got {
		t.Error("AskConfirm = true on read error, want false")
	}
}

func TestAskConfirm_When_PromptBlank_Then_ShowsDefaultPrompt(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if _, err := AskConfirm(context.Background(), bufio.NewReader(strings.NewReader("n\n")), &out, "  \t "); err != nil {
		t.Fatalf("AskConfirm: %v", err)
	}
	if !strings.Contains(out.String(), "Proceed? [y/N]: ") {
		t.Errorf("output %q does not show the default prompt", out.String())
	}
}

func TestAskConfirm_When_ContextAlreadyCanceled_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	got, err := AskConfirm(ctx, bufio.NewReader(strings.NewReader("y\n")), &out, "Go on?")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got {
		t.Error("AskConfirm = true on canceled context, want false")
	}
}

func TestAskConfirm_When_ContextCanceledWhileReading_Then_ReturnsPromptly(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	out := newPromptNotifier()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	type result struct {
		err error
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		ok, err := AskConfirm(ctx, bufio.NewReader(pr), out, "Go on?")
		done <- result{ok: ok, err: err}
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt was never written")
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", res.err)
		}
		if res.ok {
			t.Error("AskConfirm = true after cancel, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AskConfirm did not return after the context was canceled")
	}
}

func TestAskConfirm_When_CalledTwiceOnOneReader_Then_BothAnswersAreRead(t *testing.T) {
	t.Parallel()

	in := bufio.NewReader(strings.NewReader("y\nn\n"))
	var out bytes.Buffer

	first, err := AskConfirm(context.Background(), in, &out, "First?")
	if err != nil {
		t.Fatalf("first AskConfirm: %v", err)
	}
	second, err := AskConfirm(context.Background(), in, &out, "Second?")
	if err != nil {
		t.Fatalf("second AskConfirm: %v", err)
	}
	if !first || second {
		t.Errorf("answers = (%v, %v), want (true, false)", first, second)
	}
}
