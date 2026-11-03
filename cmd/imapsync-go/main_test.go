//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	appkg "github.com/greeddj/imapsync-go/internal/app"
)

// Not parallel: the test sends SIGQUIT to its own process, and
// installQuitDump must be the only SIGQUIT listener while it runs.
func TestInstallQuitDump_When_SIGQUITArrives_Then_DumpsEveryGoroutineAndExits131(t *testing.T) {
	var dump bytes.Buffer
	codes := make(chan int, 1)
	stop := installQuitDump(&dump, func(code int) { codes <- code })
	t.Cleanup(stop)

	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatalf("send SIGQUIT: %v", err)
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var code int
	select {
	case code = <-codes:
	case <-timer.C:
		t.Fatal("no exit within 5s of SIGQUIT")
	}

	if code != exitQuit {
		t.Errorf("exit code = %d, want %d", code, exitQuit)
	}
	// The test goroutine is blocked in the select above, not in the handler,
	// so finding it proves the dump covers other goroutines too.
	if got := dump.String(); !strings.Contains(got, t.Name()) {
		t.Errorf("dump does not include the waiting test goroutine %q:\n%s", t.Name(), got)
	}
}

// otherSignal is an os.Signal that is not a syscall.Signal.
type otherSignal struct{}

func (otherSignal) Signal()        {}
func (otherSignal) String() string { return "other" }

func TestReportError(t *testing.T) {
	t.Parallel()

	cancelled := fmt.Errorf("sync: %w", context.Canceled)
	tests := []struct {
		err      error
		cause    error
		name     string
		want     string
		wantCode int
	}{
		{
			name:     "wrapped cancellation prints Cancelled and exits 130",
			err:      cancelled,
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation after SIGTERM exits 143",
			err:      cancelled,
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "Cancelled.\n",
			wantCode: 143,
		},
		{
			name:     "cancellation after SIGINT exits 130",
			err:      cancelled,
			cause:    signalError{sig: os.Interrupt},
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation with a plain cancel cause exits 130",
			err:      cancelled,
			cause:    context.Canceled,
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation with a wrapped signal cause finds the signal",
			err:      cancelled,
			cause:    fmt.Errorf("stop: %w", signalError{sig: syscall.SIGTERM}),
			want:     "Cancelled.\n",
			wantCode: 143,
		},
		{
			name:     "cancellation with a signal that is not a syscall signal exits 130",
			err:      cancelled,
			cause:    signalError{sig: otherSignal{}},
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "wrapped silent exit prints nothing and exits 1",
			err:      fmt.Errorf("sync: %w", appkg.ErrSilentExit),
			want:     "",
			wantCode: 1,
		},
		{
			name:     "silent exit after SIGTERM still exits 1",
			err:      fmt.Errorf("sync: %w", appkg.ErrSilentExit),
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "",
			wantCode: 1,
		},
		{
			name:     "plain error is printed with the Error prefix",
			err:      errors.New("connection refused"),
			want:     "Error: connection refused\n",
			wantCode: 1,
		},
		{
			name:     "plain error after SIGTERM is printed and exits 1",
			err:      errors.New("connection refused"),
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "Error: connection refused\n",
			wantCode: 1,
		},
		{
			name:     "server text with control characters is escaped",
			err:      errors.New("\x1b]0;pwned\x07\r\n\u202e"),
			want:     "Error: " + `\x1b]0;pwned\a\r\n\u202e` + "\n",
			wantCode: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			code := reportError(&out, tt.err, tt.cause)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func waitDone(t *testing.T, ctx context.Context) {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		t.Fatal("context not done within 5s")
	}
}

func Test_cancelOnSignal_When_SignalArrives_Then_CauseNamesIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sig      os.Signal
		name     string
		wantText string
		wantCode int
	}{
		{
			name:     "SIGTERM exits 143",
			sig:      syscall.SIGTERM,
			wantText: "terminated signal received",
			wantCode: 143,
		},
		{
			name:     "SIGINT exits 130",
			sig:      os.Interrupt,
			wantText: "interrupt signal received",
			wantCode: 130,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signals := make(chan os.Signal, 1)
			ctx, stop := cancelOnSignal(context.Background(), signals)
			t.Cleanup(stop)

			signals <- tt.sig
			waitDone(t, ctx)

			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
			cause := context.Cause(ctx)
			if _, ok := errors.AsType[signalError](cause); !ok {
				t.Fatalf("cause = %T %v, want signalError", cause, cause)
			}
			if got := cause.Error(); got != tt.wantText {
				t.Errorf("cause text = %q, want %q", got, tt.wantText)
			}
			if code := cancelExitCode(cause); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func Test_cancelOnSignal_When_StopRunsBeforeAnySignal_Then_LaterSignalChangesNothing(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(context.Background(), signals)
	stop()
	waitDone(t, ctx)

	signals <- syscall.SIGTERM

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, context.Canceled) {
		t.Errorf("cause = %v, want context.Canceled", cause)
	}
	if code := cancelExitCode(cause); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func Test_cancelOnSignal_When_TwoSignalsArrive_Then_FirstWins(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(context.Background(), signals)
	t.Cleanup(stop)

	signals <- syscall.SIGTERM
	waitDone(t, ctx)
	signals <- os.Interrupt

	if code := cancelExitCode(context.Cause(ctx)); code != 143 {
		t.Errorf("exit code = %d, want 143", code)
	}
}

func Test_cancelOnSignal_When_ParentEnds_Then_CauseIsACancelWithNoSignal(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(parent, signals)
	t.Cleanup(stop)

	cancelParent()
	waitDone(t, ctx)

	cause := context.Cause(ctx)
	if !errors.Is(cause, context.Canceled) {
		t.Errorf("cause = %v, want context.Canceled", cause)
	}
	if code := cancelExitCode(cause); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

// Not parallel: the test sends SIGTERM and SIGINT to its own process, and
// notifySignals must hold both signals while it runs.
func Test_notifySignals_When_SignalArrives_Then_ExitCodeNamesIt(t *testing.T) {
	tests := []struct {
		name     string
		sig      syscall.Signal
		wantCode int
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 143},
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, stop := notifySignals()
			t.Cleanup(stop)

			if err := syscall.Kill(os.Getpid(), tt.sig); err != nil {
				t.Fatalf("send %v: %v", tt.sig, err)
			}
			waitDone(t, ctx)

			var out bytes.Buffer
			code := reportError(&out, fmt.Errorf("sync: %w", ctx.Err()), context.Cause(ctx))

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := out.String(); got != "Cancelled.\n" {
				t.Errorf("output = %q, want %q", got, "Cancelled.\n")
			}
		})
	}
}
