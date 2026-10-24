//go:build unix

package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
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
