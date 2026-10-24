package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

// Test_AppendMessage_rejectsWithoutAppend asserts that every precondition
// failure of AppendMessage is reported without an APPEND reaching the server.
func Test_AppendMessage_rejectsWithoutAppend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message      func() *imap.Message
		wantErrIs    error
		name         string
		wantErrText  string
		cancelCtx    bool
		cancelClient bool
		dropConn     bool
	}{
		{
			name: "message has no body section",
			message: func() *imap.Message {
				return imap.NewMessage(1, []imap.FetchItem{imap.FetchEnvelope})
			},
			wantErrText: "no body",
		},
		{
			name:      "context already canceled",
			message:   buildTestIMAPMessage,
			cancelCtx: true,
			wantErrIs: context.Canceled,
		},
		{
			name:         "client already canceled",
			message:      buildTestIMAPMessage,
			cancelClient: true,
			wantErrIs:    context.Canceled,
		},
		{
			name:        "no live connection",
			message:     buildTestIMAPMessage,
			dropConn:    true,
			wantErrText: "not connected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c := newClientWithFake(t, srv)
			if tt.cancelClient {
				c.Cancel()
			}
			if tt.dropConn {
				c.c.Store(nil)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.cancelCtx {
				cancel()
			}

			err := c.AppendMessage(ctx, "INBOX", tt.message())
			if err == nil {
				t.Fatal("AppendMessage returned nil, want an error")
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Errorf("error = %v, want %v", err, tt.wantErrIs)
			}
			if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrText)
			}
			if got := srv.callCount("APPEND"); got != 0 {
				t.Errorf("APPEND count = %d, want 0", got)
			}
		})
	}
}

// Test_AppendMessage_whenServerRefuses asserts that a tagged NO after the full
// literal was sent is returned wrapped, and that it does not trigger a
// reconnect because the failure is not transient.
func Test_AppendMessage_whenServerRefuses(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(appendOutcomeHandler(srv, "NO [CANNOT] message rejected by policy"))
	c := newClientWithFake(t, srv)
	genBefore := c.connGen.Load()

	err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	if err == nil {
		t.Fatal("AppendMessage returned nil, want the server's refusal")
	}

	const serverText = "message rejected by policy"
	cause := errors.Unwrap(err)
	if cause == nil || cause.Error() != serverText {
		t.Errorf("wrapped cause = %v, want the server text %q", cause, serverText)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
	if got := c.connGen.Load(); got != genBefore {
		t.Errorf("connGen = %d, want %d (no reconnect for a permanent refusal)", got, genBefore)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1", got)
	}
}

// Test_AppendMessage_logsWhenVerbose asserts that a successful APPEND reports
// the Message-Id and the destination folder in verbose mode.
func Test_AppendMessage_logsWhenVerbose(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newClientWithFake(t, srv)
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})
	c.verbose = true

	if err := c.AppendMessage(t.Context(), "Archive", buildTestIMAPMessage()); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if want := `Message "test-id@example.com" appended to Archive`; !slices.ContainsFunc(logged, func(msg string) bool {
		return strings.Contains(msg, want)
	}) {
		t.Errorf("log = %q, want a line containing %q", logged, want)
	}
}

// Test_AppendMessage_repairsConnectionAfterServerLogout asserts that when the
// server has dropped the session, the failing APPEND reconnects so the next
// APPEND in the same plan succeeds instead of cascading "Not logged in".
func Test_AppendMessage_repairsConnectionAfterServerLogout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	srv := newFakeServer(t)
	srv.addConnHandler(byeAfterLoginHandler(srv, release))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newClientWithFake(t, srv)

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-loggedOut:
	case <-timer.C:
		t.Fatal("client did not notice the server closing the session")
	}

	err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	if err == nil {
		t.Fatal("first AppendMessage returned nil, want the logged-out error")
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if got := c.connGen.Load(); got != 1 {
		t.Errorf("connGen = %d, want 1 (the failed APPEND repaired the connection)", got)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count after the failed call = %d, want 0", got)
	}

	if err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage()); err != nil {
		t.Fatalf("second AppendMessage: %v", err)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
	if got := c.connGen.Load(); got != 1 {
		t.Errorf("connGen = %d after the second call, want it to stay 1", got)
	}
}

// Test_AppendMessage_whenContextExpiresMidAppend asserts that a context ending while
// the server has not answered the APPEND returns its error, without a reconnect.
func Test_AppendMessage_whenContextExpiresMidAppend(t *testing.T) {
	t.Parallel()

	hold := newStall(verbIs("APPEND"))
	hold.before = func(conn net.Conn, rd *bufio.Reader, cmd command) bool {
		size, ok := literalSize(cmd.arg)
		if !ok {
			return false
		}
		writeLine(conn, "+ Ready for literal data")
		if _, err := io.CopyN(io.Discard, rd, size); err != nil {
			return false
		}
		_, err := rd.ReadString('\n')
		return err == nil
	}
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, hold.intercept))
	c := newClientWithFake(t, srv)
	genBefore := c.connGen.Load()

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(ctx, "INBOX", buildTestIMAPMessage()) }()
	awaitSignal(t, hold.reached, "the server to receive the whole literal")
	ctx.fire()

	err := awaitError(t, errc, "AppendMessage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := c.connGen.Load(); got != genBefore {
		t.Errorf("connGen = %d, want %d (a canceled client must not reconnect)", got, genBefore)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1", got)
	}
}
