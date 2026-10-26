package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

// Test_AppendMessage_rejectsWithoutAppend asserts that every precondition
// failure of AppendMessage is reported without an APPEND or a LOGIN reaching the
// server, also when the client has no session.
func Test_AppendMessage_rejectsWithoutAppend(t *testing.T) {
	t.Parallel()

	noBody := func() Message { return Message{m: imap.NewMessage(1, []imap.FetchItem{imap.FetchEnvelope})} }
	tests := []struct {
		message      func() Message
		wantErrIs    error
		name         string
		wantErrText  string
		cancelCtx    bool
		cancelClient bool
		noSession    bool
	}{
		{
			name:        "message has no body section",
			message:     noBody,
			wantErrText: "no body",
		},
		{
			name:        "zero message",
			message:     func() Message { return Message{} },
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
			name:        "message has no body section and the client has no session",
			message:     noBody,
			noSession:   true,
			wantErrText: "no body",
		},
		{
			name:         "client already canceled and has no session",
			message:      buildTestIMAPMessage,
			noSession:    true,
			cancelClient: true,
			wantErrIs:    context.Canceled,
		},
		{
			name:      "context already canceled and the client has no session",
			message:   buildTestIMAPMessage,
			noSession: true,
			cancelCtx: true,
			wantErrIs: context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c := newClientWithFake(t, srv)
			if tt.noSession {
				_ = loseSession(t, srv, c)
			}
			if tt.cancelClient {
				c.Cancel()
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.cancelCtx {
				cancel()
			}
			loginsBefore := srv.callCount("LOGIN")

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
			if got := srv.callCount("LOGIN"); got != loginsBefore {
				t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
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
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (no reconnect for a permanent refusal)", got)
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

// testMessage returns a message whose full body is body.
func testMessage(body string) Message {
	msg := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	msg.Body[&imap.BodySectionName{}] = strings.NewReader(body)
	msg.Envelope = &imap.Envelope{MessageId: "test-id@example.com"}
	return Message{m: msg}
}

// testMessageBody returns the body that buildTestIMAPMessage sends.
func testMessageBody(t *testing.T) string {
	t.Helper()

	body, err := io.ReadAll(buildTestIMAPMessage().m.GetBody(fullBodyPeekSection))
	if err != nil {
		t.Fatalf("read the test message body: %v", err)
	}
	return string(body)
}

// awaitAppend returns the next APPEND that appendCaptureHandler reported.
func awaitAppend(t *testing.T, appends <-chan capturedAppend) capturedAppend {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case got := <-appends:
		return got
	case <-timer.C:
		t.Fatal("the server received no APPEND")
		return capturedAppend{}
	}
}

// endSessionAtClient puts the session of c into go-imap's logout state while its
// reader still runs: the window between the BYE handler and the exit of the reader.
func endSessionAtClient(t *testing.T, c *Client) {
	t.Helper()

	cli := c.c.Load()
	if cli == nil {
		t.Fatal("the client holds no session")
	}
	cli.SetState(imap.LogoutState, nil)
}

// resetConn fails a Write with a connection reset once armed. A marker limits the
// failure to the Writes whose bytes contain it, and an empty marker fails every Write.
type resetConn struct {
	net.Conn
	marker []byte
	armed  atomic.Bool
}

func (c *resetConn) Write(p []byte) (int, error) {
	if c.armed.Load() && bytes.Contains(p, c.marker) {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return c.Conn.Write(p)
}

// wrapDialedConn makes the nth connection (counting from 1) that c dials a resetConn.
func wrapDialedConn(c *Client, nth int32, marker string) *resetConn {
	target := &resetConn{marker: []byte(marker)}
	var dials atomic.Int32
	dial := c.dialFn
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := dial(ctx, addr)
		if err != nil || dials.Add(1) != nth {
			return conn, err
		}
		target.Conn = conn
		return target, nil
	}
	return target
}

// Test_AppendMessage_When_SessionEndedWhileIdle_Then_ReconnectsAndAppends asserts
// that an APPEND on a session the server ended, with or without a BYE, logs in
// again and sends the message in the same call.
func Test_AppendMessage_When_SessionEndedWhileIdle_Then_ReconnectsAndAppends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		farewell string
	}{
		{name: "the server sent a BYE", farewell: "* BYE server shutting down"},
		{name: "the server hung up without a BYE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)

			appends := make(chan capturedAppend, 2)
			srv := newFakeServer(t)
			srv.addConnHandler(byeAfterLoginHandler(srv, release, tt.farewell))
			srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
			c := newClientWithFake(t, srv)

			loggedOut := c.c.Load().LoggedOut()
			releaseOnce()
			awaitSignal(t, loggedOut, "the client to notice the server ending the session")

			err := awaitCall(t, "AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
			})
			if err != nil {
				t.Fatalf("first AppendMessage: %v, want the message sent on a new session", err)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2", got)
			}
			if got := srv.callCount("APPEND"); got != 1 {
				t.Errorf("APPEND count = %d, want 1", got)
			}
			if got, want := string(awaitAppend(t, appends).literal), testMessageBody(t); got != want {
				t.Errorf("literal = %q, want %q", got, want)
			}

			err = awaitCall(t, "second AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
			})
			if err != nil {
				t.Fatalf("second AppendMessage: %v", err)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count after the second call = %d, want it to stay 2", got)
			}
			if got := srv.callCount("APPEND"); got != 2 {
				t.Errorf("APPEND count after the second call = %d, want 2", got)
			}
		})
	}
}

// Test_AppendMessage_When_FailsBeforeBodyIsRead_Then_RetriesOnce asserts that an
// APPEND that fails with a retryable error before go-imap reads the body reconnects
// once and sends the message, and that the first connection received no APPEND.
func Test_AppendMessage_When_FailsBeforeBodyIsRead_Then_RetriesOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fail func(t *testing.T, c *Client, first *resetConn)
		name string
	}{
		{
			name: "go-imap is in the logout state while its reader still runs",
			fail: func(t *testing.T, c *Client, _ *resetConn) { endSessionAtClient(t, c) },
		},
		{
			name: "the socket write fails before the literal",
			fail: func(_ *testing.T, _ *Client, first *resetConn) { first.armed.Store(true) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			appends := make(chan capturedAppend, 2)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, serveNormally))
			srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
			c := newDisconnectedClient(t, srv)
			first := wrapDialedConn(c, 1, "")
			if err := c.connectAndLogin(t.Context()); err != nil {
				t.Fatalf("connectAndLogin: %v", err)
			}
			tt.fail(t, c, first)

			err := awaitCall(t, "AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
			})
			if err != nil {
				t.Fatalf("AppendMessage: %v, want the message sent on a new session", err)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2", got)
			}
			if got := srv.callCount("APPEND"); got != 1 {
				t.Errorf("APPEND count = %d, want 1 (the first connection received none)", got)
			}
			if got, want := string(awaitAppend(t, appends).literal), testMessageBody(t); got != want {
				t.Errorf("literal = %q, want %q", got, want)
			}
		})
	}
}

// Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry asserts that a
// connection that drops after go-imap sent the body is repaired but not retried,
// so the destination gets no duplicate. An empty body counts as sent.
func Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message func() Message
		name    string
	}{
		{name: "a normal body", message: buildTestIMAPMessage},
		{name: "an empty body", message: func() Message { return testMessage("") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
				if cmd.verb != "APPEND" {
					return false, true
				}
				_ = readAppendLiteral(conn, rd, cmd)
				return true, false
			}))
			srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
			c := newClientWithFake(t, srv)

			err := awaitCall(t, "AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", tt.message())
			})
			if err == nil {
				t.Fatal("AppendMessage returned nil, want the error of the dropped connection")
			}
			if got := classifyError(err); got != ClassTransient {
				t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
			}
			if got := srv.callCount("APPEND"); got != 1 {
				t.Errorf("APPEND count = %d, want 1 (a sent body must not be sent again)", got)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2 (the failed APPEND repaired the connection)", got)
			}
		})
	}
}

// Test_AppendMessage_When_RetryReconnectIsRefused_Then_ReturnsReconnectError asserts
// that a refused login for the retry returns the reconnect error as it is, and that
// the next call returns the same refusal without a new login.
func Test_AppendMessage_When_RetryReconnectIsRefused_Then_ReturnsReconnectError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Authentication failed."))
	c := newClientWithFake(t, srv)
	endSessionAtClient(t, c)

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if err == nil {
		t.Fatal("AppendMessage returned nil, want the refused login")
	}
	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassPermanent)
	}
	if strings.Contains(err.Error(), "append:") {
		t.Errorf("error = %q, want the reconnect error without the append wrapper", err)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (initial and the refused retry)", got)
	}
	if c.c.Load() != nil {
		t.Fatal("the client holds a session although the login was refused")
	}

	second := awaitCall(t, "second AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	checkStoredGiveUp(t, second, err, ClassPermanent)
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want it to stay 2", got)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
}

// Test_AppendMessage_When_RetryGoesSilent_Then_WatchdogTripsWithoutAnotherReconnect
// asserts that the retry runs under the new session's watchdog, that its failure
// starts no further reconnect, and that the next call detects the ended session.
func Test_AppendMessage_When_RetryGoesSilent_Then_WatchdogTripsWithoutAnotherReconnect(t *testing.T) {
	t.Parallel()

	hold := newStall(verbIs("APPEND"))
	hold.before = readAppendLiteral
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(interceptHandler(srv, hold.intercept))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newWatchedClient(t, srv, tripLimit)
	endSessionAtClient(t, c)

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("error = %v, want it to wrap errIdleTimeout", err)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (no repair after the failed retry)", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}

	err = awaitCall(t, "second AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if err != nil {
		t.Fatalf("second AppendMessage: %v, want success on a new session", err)
	}
	if got := srv.callCount("LOGIN"); got != 3 {
		t.Errorf("LOGIN count = %d, want 3", got)
	}
	if got := srv.callCount("APPEND"); got != 2 {
		t.Errorf("APPEND count = %d, want 2", got)
	}
}

// Test_AppendMessage_When_ContextEndsDuringRetry_Then_ReturnsContextError asserts
// that a context that ends while the retry waits for the server returns its error,
// without a further reconnect.
func Test_AppendMessage_When_ContextEndsDuringRetry_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	hold := newStall(verbIs("APPEND"))
	hold.before = readAppendLiteral
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(interceptHandler(srv, hold.intercept))
	c := newClientWithFake(t, srv)
	endSessionAtClient(t, c)

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(ctx, "INBOX", buildTestIMAPMessage()) }()
	awaitSignal(t, hold.reached, "the server to receive the retried literal")
	ctx.fire()

	err := awaitError(t, errc, "AppendMessage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (a canceled client must not reconnect again)", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1 (the first attempt sent nothing, the retry sent the message once)", got)
	}
}

// dropAfterLiteral is an interceptHandler hook that reads the whole APPEND literal and
// hangs up without a reply, so go-imap has sent the body when the connection fails.
func dropAfterLiteral(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
	if cmd.verb != "APPEND" {
		return false, true
	}
	_ = readAppendLiteral(conn, rd, cmd)
	return true, false
}

// Test_AppendMessage_When_RepairWasRefusedPermanently_Then_NextAppendSendsNoLogin asserts
// that an APPEND on a client whose repair was refused returns that refusal and sends neither
// a LOGIN nor an APPEND.
func Test_AppendMessage_When_RepairWasRefusedPermanently_Then_NextAppendSendsNoLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, dropAfterLiteral))
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Authentication failed."))
	c := newClientWithFake(t, srv)

	err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	if err == nil {
		t.Fatal("first AppendMessage returned nil, want the error of the dropped connection")
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if c.c.Load() != nil {
		t.Fatal("the client holds a session although the repair was refused")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Fatalf("LOGIN count after the first call = %d, want 2 (initial and the refused repair)", got)
	}
	refusal := c.giveUpErr
	if refusal == nil {
		t.Fatal("the refused repair left no give-up")
	}

	err = c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	checkStoredGiveUp(t, err, refusal, ClassPermanent)
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want it to stay 2", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1 (only the first connection received one)", got)
	}
}

// Test_AppendMessage_When_RepairGaveUpTransiently_Then_NextAppendProbesAfterCooldown asserts
// that APPENDs inside the cool-down return the stored give-up with no LOGIN or APPEND, and that
// the first one after it logs in once and uploads.
func Test_AppendMessage_When_RepairGaveUpTransiently_Then_NextAppendProbesAfterCooldown(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, dropAfterLiteral))
	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newClientWithFake(t, srv)
	c.backoff = 0
	c.reviveDur = time.Hour
	dials := countDials(c)

	err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if c.c.Load() != nil {
		t.Fatal("the client holds a session although every repair login was refused")
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Fatalf("LOGIN count after the first call = %d, want %d", got, want)
	}
	giveUp := c.giveUpErr
	if giveUp == nil {
		t.Fatal("the repair that gave up left no give-up")
	}
	dialsBefore := dials.Load()

	err = c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	checkStoredGiveUp(t, err, giveUp, ClassTransient)
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Errorf("LOGIN count inside the cool-down = %d, want it to stay %d", got, want)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count inside the cool-down = %d, want it to stay 1", got)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d inside the cool-down, want no dial", dialsBefore, got)
	}

	expireCooldown(c)
	if err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage()); err != nil {
		t.Fatalf("AppendMessage after the cool-down: %v, want success after one login", err)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts+1; got != want {
		t.Errorf("LOGIN count = %d, want %d", got, want)
	}
	if got := srv.callCount("APPEND"); got != 2 {
		t.Errorf("APPEND count = %d, want 2 (one per connection)", got)
	}
	if got := dials.Load() - dialsBefore; got != 1 {
		t.Errorf("the probe dialed %d times, want 1", got)
	}
	if c.giveUpErr != nil {
		t.Errorf("giveUpErr = %v after the login, want nil", c.giveUpErr)
	}
}

// Test_AppendMessage_When_ReconnectWasRefusedPermanently_Then_LaterAppendsSendNoLogin asserts
// that a permanent refusal ends all logins of the client, also when the cool-down is zero.
func Test_AppendMessage_When_ReconnectWasRefusedPermanently_Then_LaterAppendsSendNoLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	dials := countDials(c)
	refusal := loseSession(t, srv, c)
	c.reviveDur = 0
	// The server would refuse again, so a stray LOGIN shows in the count.
	for range laterCalls {
		srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Authentication failed."))
	}
	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")

	for range laterCalls {
		err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
		checkStoredGiveUp(t, err, refusal, ClassPermanent)
	}

	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
	}
}

// Test_AppendMessage_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain
// asserts that an APPEND that had to reconnect first does not reconnect again after a
// transient failure.
func Test_AppendMessage_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	c.reviveDur = 0
	srv.addConnHandler(interceptHandler(srv, dropAfterLiteral))

	err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	if err == nil {
		t.Fatal("AppendMessage returned nil, want the error of the dropped connection")
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts+1; got != want {
		t.Errorf("LOGIN count = %d, want %d (initial, the refusals, revive) and no repair", got, want)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
}

// Test_AppendMessage_When_RevivedSessionFailsBeforeBodyIsRead_Then_DoesNotReconnectAgain
// asserts that an APPEND that reconnected first does not reconnect a second time to
// retry when the new session fails before go-imap reads the body.
func Test_AppendMessage_When_RevivedSessionFailsBeforeBodyIsRead_Then_DoesNotReconnectAgain(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	srv := newFakeServer(t)
	srv.addConnHandler(byeAfterLoginHandler(srv, release, "* BYE server shutting down"))
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newDisconnectedClient(t, srv)
	wrapDialedConn(c, 2, " APPEND ").armed.Store(true)
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	awaitSignal(t, loggedOut, "the client to notice the server ending the session")

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if err == nil {
		t.Fatal("AppendMessage returned nil, want the error of the failed write")
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if !strings.Contains(err.Error(), "append:") {
		t.Errorf("error = %q, want it to contain %q", err, "append:")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (initial and the revive, no second reconnect)", got)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
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

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(ctx, "INBOX", buildTestIMAPMessage()) }()
	awaitSignal(t, hold.reached, "the server to receive the whole literal")
	ctx.fire()

	err := awaitError(t, errc, "AppendMessage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (a canceled client must not reconnect)", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1 (the literal must not be sent again)", got)
	}
}

func Test_AppendMessage_whenEnvelopeMissing(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(appendHandler(srv))
	c := newClientWithFake(t, srv)
	c.verbose = true // the success log reads the Message-Id as well

	msg := buildTestIMAPMessage()
	msg.m.Envelope = nil
	if err := c.AppendMessage(t.Context(), "INBOX", msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
}

// Test_AppendMessage_When_ServerNeverAnswersAfterLiteral_Then_WatchdogTripsAndRepairs
// asserts that an APPEND whose tagged reply never comes fails as a transient
// idle timeout, and that the repair leaves a session the next APPEND can use.
func Test_AppendMessage_When_ServerNeverAnswersAfterLiteral_Then_WatchdogTripsAndRepairs(t *testing.T) {
	t.Parallel()

	hold := newStall(verbIs("APPEND"))
	hold.before = readAppendLiteral
	hungUp := make(chan struct{})
	srv := newFakeServer(t)
	srv.addConnHandler(endedSignal(interceptHandler(srv, hold.intercept), hungUp))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newWatchedClient(t, srv, tripLimit)

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if err == nil {
		t.Fatal("first AppendMessage returned nil, want the idle timeout")
	}
	if !errors.Is(err, errIdleTimeout) {
		t.Errorf("error = %v, want it to wrap errIdleTimeout", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, a watchdog trip must not read as a cancellation", err)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the failed APPEND repaired the connection)", got)
	}
	awaitSignal(t, hungUp, "the client to close the stalled connection")

	if err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage()); err != nil {
		t.Fatalf("second AppendMessage: %v, want success on the repaired session", err)
	}
	if got := srv.callCount("APPEND"); got != 2 {
		t.Errorf("APPEND count = %d, want 2 (one per connection)", got)
	}
}

// zeroLiteral is a literal of n zero bytes that is never held in memory.
type zeroLiteral struct{ n, read int }

func (l *zeroLiteral) Len() int { return l.n }

func (l *zeroLiteral) Read(p []byte) (int, error) {
	if l.read >= l.n {
		return 0, io.EOF
	}
	n := min(len(p), l.n-l.read)
	clear(p[:n])
	l.read += n
	return n, nil
}

// Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs
// asserts that a literal write blocked on a server that stopped reading ends as
// an idle timeout, as a dead network does mid-upload.
func Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs(t *testing.T) {
	t.Parallel()

	// More than any loopback socket buffer holds, so the write must block.
	const literalSize = 64 << 20

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "APPEND" {
			return false, true
		}
		writeLine(conn, "+ Ready for literal data")
		<-release
		return true, false
	}))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newWatchedClient(t, srv, tripLimit)

	huge := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	huge.Body[&imap.BodySectionName{}] = &zeroLiteral{n: literalSize}
	huge.Envelope = &imap.Envelope{MessageId: "huge-id@example.com"}

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", Message{m: huge})
	})
	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("error = %v, want it to wrap errIdleTimeout", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the failed APPEND repaired the connection)", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1 (a partly sent literal must not be sent again)", got)
	}

	if err := c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage()); err != nil {
		t.Fatalf("second AppendMessage: %v, want success on the repaired session", err)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2", got)
	}
	if got := srv.callCount("APPEND"); got != 2 {
		t.Errorf("APPEND count = %d, want 2 (one per call)", got)
	}
}

// Test_AppendMessage_When_ReconnectBeforeAppendIsRefused_Then_ReturnsReconnectError
// asserts that a session the server ended while idle, whose reconnect every
// attempt refuses, returns the reconnect error and sends no APPEND.
// Sequential - swaps the package-level sleepCtx var.
func Test_AppendMessage_When_ReconnectBeforeAppendIsRefused_Then_ReturnsReconnectError(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	srv := newFakeServer(t)
	srv.addConnHandler(byeAfterLoginHandler(srv, release, "* BYE server shutting down"))
	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [UNAVAILABLE] Server busy"))
	}
	c := newClientWithFake(t, srv)
	rec := recordSleeps(t, srv)

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	awaitSignal(t, loggedOut, "the client to notice the server ending the session")

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if err == nil {
		t.Fatal("AppendMessage returned nil, want the reconnect error")
	}
	if !strings.Contains(err.Error(), "failed to reconnect after retries") {
		t.Errorf("error = %q, want it to contain %q", err, "failed to reconnect after retries")
	}
	if strings.Contains(err.Error(), "append:") {
		t.Errorf("error = %q, want the reconnect error without the append wrapper", err)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Errorf("LOGIN count = %d, want %d (the initial login and every attempt)", got, want)
	}
	if want := transientBackoffs(); !slices.Equal(rec.waits, want) {
		t.Errorf("waits = %v, want %v", rec.waits, want)
	}
	if !c.mu.TryLock() {
		t.Fatal("c.mu is still held after AppendMessage returned")
	}
	c.mu.Unlock()
}

// Test_AppendMessage_When_ContextEndsDuringReconnectBeforeAppend_Then_ReturnsContextError
// asserts that a context that ends while the login before the APPEND waits for
// the server returns the context error, not the reconnect error.
func Test_AppendMessage_When_ContextEndsDuringReconnectBeforeAppend_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	login := newStall(verbIs("LOGIN"))
	srv := newFakeServer(t)
	srv.addConnHandler(byeAfterLoginHandler(srv, release, "* BYE server shutting down"))
	srv.addConnHandler(interceptHandler(srv, login.intercept))
	c := newClientWithFake(t, srv)

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	awaitSignal(t, loggedOut, "the client to notice the server ending the session")

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(ctx, "INBOX", buildTestIMAPMessage()) }()
	awaitSignal(t, login.reached, "the server to receive the LOGIN of the reconnect")
	ctx.fire()

	err := awaitError(t, errc, "AppendMessage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the initial login and the stalled one)", got)
	}
}

// Test_AppendMessage_When_ContextEndsDuringRetryReconnect_Then_ReturnsContextError
// asserts that a context that ends while the reconnect of the retry waits for
// the server returns the context error, and that no APPEND reaches any session.
func Test_AppendMessage_When_ContextEndsDuringRetryReconnect_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	login := newStall(verbIs("LOGIN"))
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(interceptHandler(srv, login.intercept))
	c := newClientWithFake(t, srv)
	endSessionAtClient(t, c)

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(ctx, "INBOX", buildTestIMAPMessage()) }()
	awaitSignal(t, login.reached, "the server to receive the LOGIN of the retry")
	ctx.fire()

	err := awaitError(t, errc, "AppendMessage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0 (the first attempt failed before the send, the retry never got a session)", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the initial login and the stalled one)", got)
	}
}

// Test_AppendMessage_When_CancelledRightAfterReconnect_Then_SendsNoAppend asserts
// that a client cancelled between the login before the APPEND and the APPEND
// itself puts nothing on the new session.
func Test_AppendMessage_When_CancelledRightAfterReconnect_Then_SendsNoAppend(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	srv := newFakeServer(t)
	srv.addConnHandler(byeAfterLoginHandler(srv, release, "* BYE server shutting down"))
	srv.addConnHandler(appendOutcomeHandler(srv, "OK APPEND completed"))
	c := newClientWithFake(t, srv)
	c.SetProgressTracker(cancelOnReconnect{c: c})

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	awaitSignal(t, loggedOut, "the client to notice the server ending the session")

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage())
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got := srv.callCount("APPEND"); got != 0 {
		t.Errorf("APPEND count = %d, want 0", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the reconnect did log in)", got)
	}
}

// Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend
// asserts that the retry sends the same flags, date and bytes as an APPEND that
// needed no retry, and that exactly one complete literal reaches the server.
func Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend(t *testing.T) {
	t.Parallel()

	dated := func() Message {
		msg := buildTestIMAPMessage()
		msg.m.Envelope.Date = time.Date(2026, 10, 25, 9, 8, 7, 0, time.UTC)
		return msg
	}

	directOut := make(chan capturedAppend, 2)
	directSrv := newFakeServer(t)
	directSrv.addConnHandler(appendCaptureHandler(directSrv, "OK APPEND completed", directOut))
	direct := newClientWithFake(t, directSrv)
	if err := direct.AppendMessage(t.Context(), "INBOX", dated()); err != nil {
		t.Fatalf("direct AppendMessage: %v", err)
	}
	want := awaitAppend(t, directOut)

	retryOut := make(chan capturedAppend, 2)
	retrySrv := newFakeServer(t)
	retrySrv.addConnHandler(interceptHandler(retrySrv, serveNormally))
	retrySrv.addConnHandler(appendCaptureHandler(retrySrv, "OK APPEND completed", retryOut))
	retried := newClientWithFake(t, retrySrv)
	endSessionAtClient(t, retried)
	err := awaitCall(t, "AppendMessage", func() error {
		return retried.AppendMessage(t.Context(), "INBOX", dated())
	})
	if err != nil {
		t.Fatalf("retried AppendMessage: %v", err)
	}

	if got := retrySrv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the retry used one new session)", got)
	}
	if got := retrySrv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
	got := awaitAppend(t, retryOut)
	for _, part := range []string{`(\Seen)`, "25-Oct-2026 09:08:07 +0000"} {
		if !strings.Contains(want.arg, part) {
			t.Errorf("direct APPEND arguments %q do not contain %q", want.arg, part)
		}
	}
	if got.arg != want.arg {
		t.Errorf("retried APPEND arguments = %q, want %q (same flags and date as a direct APPEND)", got.arg, want.arg)
	}
	if !bytes.Equal(got.literal, want.literal) {
		t.Errorf("retried literal = %q, want %q", got.literal, want.literal)
	}
}

// Test_readTracker asserts that readTracker marks a body as read only through
// Read, and that it does not offer WriteTo, which would let io.Copy move the
// body without a Read.
func Test_readTracker(t *testing.T) {
	t.Parallel()

	t.Run("it hides the WriteTo of the wrapped literal", func(t *testing.T) {
		t.Parallel()

		wrapped := strings.NewReader("body")
		if _, ok := any(wrapped).(io.WriterTo); !ok {
			t.Fatal("test setup: the wrapped literal has no WriteTo")
		}
		if _, ok := any(&readTracker{Literal: wrapped}).(io.WriterTo); ok {
			t.Error("readTracker has WriteTo, so io.Copy could skip Read")
		}
	})

	t.Run("it stays unread until the first Read", func(t *testing.T) {
		t.Parallel()

		rt := &readTracker{Literal: strings.NewReader("body")}
		if got := rt.Len(); got != len("body") {
			t.Errorf("Len = %d, want %d", got, len("body"))
		}
		if rt.read {
			t.Error("read = true before any Read")
		}
	})

	copyTests := []struct {
		name string
		body string
	}{
		{name: "a non-empty body", body: "body"},
		{name: "an empty body", body: ""},
	}
	for _, tt := range copyTests {
		t.Run("io.Copy marks "+tt.name+" as read", func(t *testing.T) {
			t.Parallel()

			rt := &readTracker{Literal: strings.NewReader(tt.body)}
			var got bytes.Buffer
			if _, err := io.Copy(&got, rt); err != nil {
				t.Fatalf("io.Copy: %v", err)
			}
			if got.String() != tt.body {
				t.Errorf("copied %q, want %q", got.String(), tt.body)
			}
			if !rt.read {
				t.Error("read = false after io.Copy")
			}
		})
	}
}
