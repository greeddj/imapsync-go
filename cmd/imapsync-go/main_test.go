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

func TestReportError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err      error
		name     string
		want     string
		wantCode int
	}{
		{
			name:     "wrapped cancellation prints Cancelled and exits 130",
			err:      fmt.Errorf("sync: %w", context.Canceled),
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
			name:     "plain error is printed with the Error prefix",
			err:      errors.New("connection refused"),
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
			code := reportError(&out, tt.err)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}
