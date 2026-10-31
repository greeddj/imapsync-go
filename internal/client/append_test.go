package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/greeddj/imapsync-go/internal/ratelimit"
)

func Test_AppendMessage_rejectsWithoutAppend(t *testing.T) {
	t.Parallel()

	noBody := func() Message { return Message{m: imap.NewMessage(1, []imap.FetchItem{imap.FetchEnvelope})} }
	nilBody := func() Message { return messageWithBody(nil) }
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
			wantErrIs:   ErrMissingBody,
			wantErrText: "no body",
		},
		{
			name:        "zero message",
			message:     func() Message { return Message{} },
			wantErrIs:   ErrMissingBody,
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
			wantErrIs:   ErrMissingBody,
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
		{
			name:        "body is NIL",
			message:     nilBody,
			wantErrIs:   ErrEmptyBody,
			wantErrText: "source returned an empty body",
		},
		{
			name:      "body is a zero-length literal",
			message:   func() Message { return messageWithBody(bytes.NewBuffer(nil)) },
			wantErrIs: ErrEmptyBody,
		},
		{
			name:      "body is NIL and the client has no session",
			message:   nilBody,
			noSession: true,
			wantErrIs: ErrEmptyBody,
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

func Test_AppendMessage_When_SourceBodyIsEmpty_Then_SendsNoAppend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{name: "NIL", value: "NIL"},
		{name: "a zero-length literal", value: "{0}\r\n"},
		{name: "an empty quoted string", value: `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newFakeServer(t)
			src.addConnHandler(interceptHandler(src, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
				if !isUIDFetch(cmd) {
					return false, true
				}
				writeLine(conn, "* 1 FETCH (UID 1 BODY[] %s)", tt.value)
				writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
				return true, true
			}))
			dst := newFakeServer(t)
			dst.addConnHandler(appendOutcomeHandler(dst, "OK APPEND completed"))
			srcClient := newClientWithFake(t, src)
			dstClient := newClientWithFake(t, dst)

			var (
				calls     int
				appendErr error
			)
			err := awaitCall(t, "StreamMessagesByUIDs", func() error {
				return srcClient.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(msg Message) error {
					calls++
					appendErr = dstClient.AppendMessage(t.Context(), "INBOX", msg)
					return nil
				})
			})
			if err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v", err)
			}

			if calls != 1 {
				t.Errorf("callback ran %d times, want 1", calls)
			}
			if !errors.Is(appendErr, ErrEmptyBody) {
				t.Fatalf("AppendMessage error = %v, want %v", appendErr, ErrEmptyBody)
			}
			if want := "source returned an empty body"; !strings.Contains(appendErr.Error(), want) {
				t.Errorf("AppendMessage error = %q, want it to contain %q", appendErr, want)
			}
			if got := dst.callCount("APPEND"); got != 0 {
				t.Errorf("destination APPEND count = %d, want 0", got)
			}
			if got := dst.callCount("LOGIN"); got != 1 {
				t.Errorf("destination LOGIN count = %d, want 1", got)
			}
		})
	}
}

func testMessage(body string) Message {
	msg := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	msg.Body[&imap.BodySectionName{}] = strings.NewReader(body)
	msg.Envelope = &imap.Envelope{MessageId: "test-id@example.com"}
	return Message{m: msg}
}

func messageWithBody(lit imap.Literal) Message {
	msg := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	msg.Body[&imap.BodySectionName{}] = lit
	msg.Envelope = &imap.Envelope{MessageId: "test-id@example.com"}
	return Message{m: msg}
}

func testMessageBody(t *testing.T) string {
	t.Helper()

	body, err := io.ReadAll(buildTestIMAPMessage().m.GetBody(fullBodyPeekSection))
	if err != nil {
		t.Fatalf("read the test message body: %v", err)
	}
	return string(body)
}

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

// A debt that other workers leave on the shared write limiter stays out of the greeting
// limit, so the reconnect succeeds and the Client keeps no give-up.
func Test_AppendMessage_When_SessionDropsWhileSharedWriteLimiterIsInDebt_Then_ReconnectsAndAppends(t *testing.T) {
	t.Parallel()

	const (
		bareGreeting = "* OK Gimap ready for requests from 127.0.0.1"
		// The debt must outlast every reconnect attempt, so a greeting that waits for tokens gives up.
		debtBursts  = 3
		dialTimeout = 300 * time.Millisecond
	)
	lim := ratelimit.NewLimiter(64 << 10)
	debt := debtBursts * time.Second

	appendOK := func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "APPEND" {
			return false, true
		}
		if !readAppendLiteral(conn, rd, cmd) {
			return true, false
		}
		writeLine(conn, "%s OK APPEND completed", cmd.tag)
		return true, true
	}
	srv := newFakeServer(t)
	for range 1 + maxReconnectAttempts {
		srv.addConnHandler(interceptHandlerGreeting(srv, bareGreeting, chainHooks(capabilityAs("IMAP4rev1"), appendOK)))
	}
	c := newDisconnectedClient(t, srv)
	c.writeLimiter = lim
	c.backoff = 0
	c.dialTimeout = dialTimeout
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}

	// The first reservation drains what the bucket still holds.
	for range 1 + debtBursts {
		lim.ReserveN(time.Now(), lim.Burst())
	}
	cli := c.c.Load()
	loggedOut := cli.LoggedOut()
	_ = cli.Terminate()
	awaitSignal(t, loggedOut, "the client to notice the closed session")

	start := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- c.AppendMessage(t.Context(), "INBOX", buildTestIMAPMessage()) }()
	var err error
	select {
	case err = <-errc:
	case <-time.After(30 * time.Second):
		t.Fatal("AppendMessage still blocked after 30s")
	}
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("AppendMessage: %v, want the message sent on a new session", err)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2", got)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
	if c.giveUpErr != nil {
		t.Errorf("giveUpErr = %v, want none", c.giveUpErr)
	}
	if elapsed < debt/2 {
		t.Errorf("AppendMessage took %s, want LOGIN to wait for the debt of the shared limiter", elapsed)
	}
}

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

// A retry after the body was sent would duplicate the message. A one-byte body counts as sent.
func Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message func() Message
		name    string
	}{
		{name: "a normal body", message: buildTestIMAPMessage},
		{name: "a one-byte body", message: func() Message { return testMessage("x") }},
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

func Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend(t *testing.T) {
	t.Parallel()

	dated := func() Message {
		msg := buildTestIMAPMessage()
		msg.m.Envelope.Date = time.Date(2026, 10, 25, 9, 8, 7, 0, time.UTC)
		msg.m.InternalDate = time.Date(2012, 7, 17, 9, 30, 0, 0, time.FixedZone("", 2*3600))
		msg.m.Flags = []string{`\Flagged`, "$Forwarded", `\Recent`}
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
	for _, part := range []string{`(\Flagged $Forwarded)`, `"17-Jul-2012 09:30:00 +0200"`} {
		if !strings.Contains(want.arg, part) {
			t.Errorf("direct APPEND arguments %q do not contain %q", want.arg, part)
		}
	}
	for _, part := range []string{`\Seen`, `\Recent`, "25-Oct-2026"} {
		if strings.Contains(want.arg, part) {
			t.Errorf("direct APPEND arguments %q contain %q", want.arg, part)
		}
	}
	if got.arg != want.arg {
		t.Errorf("retried APPEND arguments = %q, want %q (same flags and date as a direct APPEND)", got.arg, want.arg)
	}
	if !bytes.Equal(got.literal, want.literal) {
		t.Errorf("retried literal = %q, want %q", got.literal, want.literal)
	}
}

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

// go-imap keeps BODY[] only from a literal, so a quoted string arrives empty.
func Test_AppendMessage_When_SourceBodyIsANonEmptyQuotedString_Then_RefusesAsEmpty(t *testing.T) {
	t.Parallel()

	src := newFakeServer(t)
	src.addConnHandler(interceptHandler(src, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		writeLine(conn, `* 1 FETCH (UID 1 BODY[] "abc")`)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	dst := newFakeServer(t)
	dst.addConnHandler(appendOutcomeHandler(dst, "OK APPEND completed"))
	srcClient := newClientWithFake(t, src)
	dstClient := newClientWithFake(t, dst)

	var (
		calls     int
		bodyLen   = -1
		appendErr error
	)
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return srcClient.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(msg Message) error {
			calls++
			if lit := msg.m.GetBody(fullBodyPeekSection); lit != nil {
				bodyLen = lit.Len()
			}
			appendErr = dstClient.AppendMessage(t.Context(), "INBOX", msg)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if calls != 1 {
		t.Fatalf("callback ran %d times, want 1", calls)
	}
	if bodyLen != 0 {
		t.Errorf("body length = %d, want 0: go-imap keeps no quoted string, so a change here needs a new look at the guard", bodyLen)
	}
	if !errors.Is(appendErr, ErrEmptyBody) {
		t.Errorf("AppendMessage error = %v, want %v", appendErr, ErrEmptyBody)
	}
	if got := dst.callCount("APPEND"); got != 0 {
		t.Errorf("destination APPEND count = %d, want 0", got)
	}
}

// The guard runs before reconnectSession, because a reconnect for a message that cannot be sent costs a login attempt.
func Test_AppendMessage_When_BodyIsEmptyAndSessionIsGone_Then_SendsNoDialLoginOrAppend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup func(t *testing.T, srv *fakeServer) *Client
		name  string
	}{
		{
			name: "the client gave up its reconnect",
			setup: func(t *testing.T, srv *fakeServer) *Client {
				t.Helper()
				c := newClientWithFake(t, srv)
				_ = loseSession(t, srv, c)
				return c
			},
		},
		{
			name: "the server sent a BYE while idle",
			setup: func(t *testing.T, srv *fakeServer) *Client {
				t.Helper()
				return endIdleSession(t, srv, "* BYE server shutting down")
			},
		},
		{
			name: "the server hung up while idle",
			setup: func(t *testing.T, srv *fakeServer) *Client {
				t.Helper()
				return endIdleSession(t, srv, "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c := tt.setup(t, srv)
			dials := countDials(c)
			loginsBefore := srv.callCount("LOGIN")

			err := awaitCall(t, "AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", messageWithBody(nil))
			})

			if !errors.Is(err, ErrEmptyBody) {
				t.Fatalf("AppendMessage error = %v, want %v", err, ErrEmptyBody)
			}
			if got := dials.Load(); got != 0 {
				t.Errorf("dial count = %d, want 0", got)
			}
			if got := srv.callCount("LOGIN"); got != loginsBefore {
				t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
			}
			if got := srv.callCount("APPEND"); got != 0 {
				t.Errorf("APPEND count = %d, want 0", got)
			}
		})
	}
}

// endIdleSession returns a Client whose only session the server ended with farewell. An empty farewell means a plain hang-up.
func endIdleSession(t *testing.T, srv *fakeServer, farewell string) *Client {
	t.Helper()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	srv.addConnHandler(byeAfterLoginHandler(srv, release, farewell))
	c := newClientWithFake(t, srv)

	loggedOut := c.c.Load().LoggedOut()
	releaseOnce()
	awaitSignal(t, loggedOut, "the client to notice the server ending the session")
	return c
}

func Test_AppendMessage_When_BodyHasOneByte_Then_Appends(t *testing.T) {
	t.Parallel()

	appends := make(chan capturedAppend, 1)
	srv := newFakeServer(t)
	srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
	c := newClientWithFake(t, srv)

	err := awaitCall(t, "AppendMessage", func() error {
		return c.AppendMessage(t.Context(), "INBOX", testMessage("x"))
	})

	if err != nil {
		t.Fatalf("AppendMessage: %v, want a one-byte body to pass the empty-body guard", err)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
	if got := string(awaitAppend(t, appends).literal); got != "x" {
		t.Errorf("literal = %q, want %q", got, "x")
	}
}

func Test_validFlag(t *testing.T) {
	t.Parallel()

	const punctuation = "!#$&'+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[^_`abcdefghijklmnopqrstuvwxyz|}~"
	tests := []struct {
		name string
		flag string
		want bool
	}{
		{name: "Seen", flag: `\Seen`, want: true},
		{name: "Flagged in capitals", flag: `\FLAGGED`, want: true},
		{name: "Answered", flag: `\Answered`, want: true},
		{name: "Draft", flag: `\Draft`, want: true},
		{name: "Recent", flag: `\Recent`, want: true},
		{name: "Deleted", flag: `\Deleted`, want: true},
		{name: "extension flag", flag: `\X-Ext`, want: true},
		{name: "keyword with a dollar sign", flag: "$Forwarded", want: true},
		{name: "keyword with a digit", flag: "$MailFlagBit0", want: true},
		{name: "plain keyword", flag: "NonJunk", want: true},
		{name: "open bracket", flag: "[x", want: true},
		{name: "every other allowed punctuation character", flag: punctuation, want: true},
		{name: "the same characters after a backslash", flag: `\` + punctuation, want: true},
		{name: "empty", flag: ""},
		{name: "backslash only", flag: `\`},
		{name: "two backslashes", flag: `\\Seen`},
		{name: "backslash and star", flag: `\*`},
		{name: "star", flag: "*"},
		{name: "percent", flag: "%"},
		{name: "space", flag: "a b"},
		{name: "close parenthesis", flag: "x)"},
		{name: "open parenthesis", flag: "(x"},
		{name: "literal marker", flag: "{5}"},
		{name: "quote", flag: `a"b`},
		{name: "backslash inside", flag: `a\b`},
		{name: "close bracket", flag: "a]"},
		{name: "injected command", flag: "x\r\nA1 DELETE INBOX"},
		{name: "NUL", flag: "\x00"},
		{name: "control byte before a letter", flag: "\x01a"},
		{name: "DEL", flag: "\x7f"},
		{name: "non-ASCII keyword", flag: "caf\u00e9"},
		{name: "lone high byte", flag: "\xff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := validFlag(tt.flag); got != tt.want {
				t.Errorf("validFlag(%q) = %t, want %t", tt.flag, got, tt.want)
			}
		})
	}

	t.Run("every single byte, with and without a backslash", func(t *testing.T) {
		t.Parallel()

		for b := range 256 {
			want := strings.IndexByte(punctuation, byte(b)) >= 0
			if got := validFlag(string([]byte{byte(b)})); got != want {
				t.Errorf("validFlag(%#x) = %t, want %t", b, got, want)
			}
			if got := validFlag(`\` + string([]byte{byte(b)})); got != want {
				t.Errorf("validFlag(backslash and %#x) = %t, want %t", b, got, want)
			}
		}
	})
}

func Test_appendFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "empty", in: []string{}, want: nil},
		{name: "order and spelling are kept", in: []string{`\Seen`, `\FLAGGED`, "$Forwarded", "NonJunk"}, want: []string{`\Seen`, `\FLAGGED`, "$Forwarded", "NonJunk"}},
		{name: "Recent and Deleted in any letter case are dropped", in: []string{`\Recent`, `\Seen`, `\RECENT`, `\Flagged`, `\recent`, `\Deleted`, "$a", `\DELETED`}, want: []string{`\Seen`, `\Flagged`, "$a"}},
		{name: "only Recent and Deleted", in: []string{`\Recent`, `\Deleted`}, want: nil},
		{name: "invalid entries are dropped", in: []string{`\Seen`, "a b", "x)", "", `\`, "$ok", "{5}"}, want: []string{`\Seen`, "$ok"}},
		{name: "only invalid entries", in: []string{"x)\r\nA1 DELETE INBOX", `"q"`}, want: nil},
		{name: "duplicates and letter case are kept", in: []string{"$a", "$A", "$a"}, want: []string{"$a", "$A", "$a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := slices.Clone(tt.in)

			got := appendFlags(tt.in)

			if !slices.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
				t.Errorf("appendFlags(%q) = %#v, want %#v", before, got, tt.want)
			}
			if !slices.Equal(tt.in, before) || (tt.in == nil) != (before == nil) {
				t.Errorf("input after the call = %#v, want it unchanged %#v", tt.in, before)
			}
		})
	}

	t.Run("a list that drops nothing is returned as it is", func(t *testing.T) {
		t.Parallel()

		in := []string{`\Seen`, "$Forwarded"}
		got := appendFlags(in)
		if len(got) != len(in) || &got[0] != &in[0] {
			t.Errorf("appendFlags(%q) = %q, want the same backing array", in, got)
		}
	})

	t.Run("a list that drops something is a new slice", func(t *testing.T) {
		t.Parallel()

		in := []string{`\Seen`, `\Recent`, "$Forwarded"}
		got := appendFlags(in)
		if len(got) != 2 || &got[0] == &in[0] {
			t.Errorf("appendFlags(%q) = %q, want a new slice of two flags", in, got)
		}
	})
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func Test_appendFlags_When_NothingIsDropped_Then_AllocatesNothing(t *testing.T) {
	valid := []string{`\Seen`, `\Flagged`, "$Forwarded", "NonJunk"}

	tests := []struct {
		call func()
		name string
	}{
		{name: "a valid list", call: func() { _ = appendFlags(valid) }},
		{name: "nil", call: func() { _ = appendFlags(nil) }},
		{name: "validFlag", call: func() { _ = validFlag("$Forwarded") }},
		{name: "validFlag on a rejected flag", call: func() { _ = validFlag("a b") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(100, tt.call); got != 0 {
				t.Errorf("allocations per call = %v, want 0", got)
			}
		})
	}
}

func FuzzAppendFlags(f *testing.F) {
	const sep = "\n"
	for _, seed := range []string{
		"",
		`\Seen` + sep + `\FLAGGED` + sep + "$Forwarded" + sep + "NonJunk",
		`\Recent` + sep + `\Seen` + sep + `\DELETED`,
		`\Recent` + sep + `\Deleted`,
		`\Seen` + sep + "a b" + sep + "x)" + sep + "" + sep + `\`,
		"[x" + sep + "a]" + sep + `a"b` + sep + `a\b` + sep + "{5}",
		"caf\u00e9" + sep + "\x00" + sep + "\x7f",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, joined string) {
		in := strings.Split(joined, sep)
		before := slices.Clone(in)

		out := appendFlags(in)

		if !slices.Equal(in, before) {
			t.Fatalf("input changed from %q to %q", before, in)
		}
		if out != nil && len(out) == 0 {
			t.Fatalf("output is empty but not nil: %#v", out)
		}
		next := 0
		for _, flag := range out {
			if !validFlag(flag) || strings.EqualFold(flag, `\Recent`) || strings.EqualFold(flag, `\Deleted`) {
				t.Errorf("output flag %q is not allowed", flag)
			}
			found := slices.Index(in[next:], flag)
			if found < 0 {
				t.Fatalf("output %q is not an in-order subsequence of %q", out, in)
			}
			next += found + 1
		}
		allowed := 0
		for _, flag := range in {
			if validFlag(flag) && !strings.EqualFold(flag, `\Recent`) && !strings.EqualFold(flag, `\Deleted`) {
				allowed++
			}
		}
		if len(out) != allowed {
			t.Errorf("output %q holds %d flags, want the %d allowed flags of %q (every dropped flag fails a check)", out, len(out), allowed, in)
		}
	})
}

func messageWithFlags(flags []string) Message {
	msg := buildTestIMAPMessage()
	msg.m.Flags = flags
	return msg
}

// seenVerbs returns the sorted command verbs that srv received.
func seenVerbs(srv *fakeServer) []string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	verbs := make([]string, 0, len(srv.counts))
	for verb, n := range srv.counts {
		if n > 0 {
			verbs = append(verbs, verb)
		}
	}
	slices.Sort(verbs)
	return verbs
}

func Test_AppendMessage_When_SourceHasFlags_Then_AppendCarriesThemWithoutRecentOrDeleted(t *testing.T) {
	t.Parallel()

	appends := make(chan capturedAppend, 1)
	srv := newFakeServer(t)
	srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
	c := newClientWithFake(t, srv)
	msg := messageWithFlags([]string{`\Seen`, `\FLAGGED`, `\Recent`, "$Forwarded", `\deleted`, "NonJunk"})

	err := awaitCall(t, "AppendMessage", func() error { return c.AppendMessage(t.Context(), "INBOX", msg) })

	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	want := fmt.Sprintf(`INBOX (\Seen \FLAGGED $Forwarded NonJunk) {%d}`, len(testMessageBody(t)))
	if got := awaitAppend(t, appends).arg; got != want {
		t.Errorf("APPEND arguments = %q, want %q", got, want)
	}
	if want := []string{`\Seen`, `\FLAGGED`, `\Recent`, "$Forwarded", `\deleted`, "NonJunk"}; !slices.Equal(msg.m.Flags, want) {
		t.Errorf("flags of the message = %q, want them unchanged %q", msg.m.Flags, want)
	}
}

func Test_AppendMessage_When_NoFlagRemains_Then_AppendHasNoFlagList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags []string
	}{
		{name: "nil", flags: nil},
		{name: "empty", flags: []string{}},
		{name: "only Recent and Deleted", flags: []string{`\Recent`, `\Deleted`}},
		{name: "only invalid flags", flags: []string{"a b", "x)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			appends := make(chan capturedAppend, 1)
			srv := newFakeServer(t)
			srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
			c := newClientWithFake(t, srv)

			err := awaitCall(t, "AppendMessage", func() error {
				return c.AppendMessage(t.Context(), "INBOX", messageWithFlags(tt.flags))
			})

			if err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
			want := fmt.Sprintf("INBOX {%d}", len(testMessageBody(t)))
			if got := awaitAppend(t, appends).arg; got != want {
				t.Errorf("APPEND arguments = %q, want %q", got, want)
			}
		})
	}
}

func Test_AppendMessage_When_FlagWouldBreakTheCommandLine_Then_DropsItAndSendsOneCleanAppend(t *testing.T) {
	t.Parallel()

	appends := make(chan capturedAppend, 2)
	srv := newFakeServer(t)
	srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
	c := newClientWithFake(t, srv)
	msg := messageWithFlags([]string{`\Flagged`, "x)\r\nA1 DELETE INBOX", "y[a) {5}]", `"q"`})

	err := awaitCall(t, "AppendMessage", func() error { return c.AppendMessage(t.Context(), "INBOX", msg) })

	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	got := awaitAppend(t, appends)
	if want := fmt.Sprintf(`INBOX (\Flagged) {%d}`, len(testMessageBody(t))); got.arg != want {
		t.Errorf("APPEND arguments = %q, want %q", got.arg, want)
	}
	if string(got.literal) != testMessageBody(t) {
		t.Errorf("literal = %q, want the message body %q", got.literal, testMessageBody(t))
	}
	if n := srv.callCount("APPEND"); n != 1 {
		t.Errorf("APPEND count = %d, want 1", n)
	}
	if n := srv.callCount("DELETE"); n != 0 {
		t.Errorf("DELETE count = %d, want 0", n)
	}
	if verbs := seenVerbs(srv); !slices.Equal(verbs, []string{"APPEND", "LOGIN"}) {
		t.Errorf("the server saw the commands %q, want only LOGIN and APPEND", verbs)
	}
}

func Test_appendDate(t *testing.T) {
	t.Parallel()

	plus2 := time.FixedZone("", 2*3600)
	minus7 := time.FixedZone("", -7*3600)
	arrival := time.Date(2012, 7, 17, 9, 30, 0, 0, plus2)
	header := time.Date(2026, 10, 25, 9, 8, 7, 0, minus7)
	tests := []struct {
		internal time.Time
		want     time.Time
		envelope *imap.Envelope
		name     string
	}{
		{name: "INTERNALDATE and the Date header are both set", internal: arrival, envelope: &imap.Envelope{Date: header}, want: arrival},
		{name: "INTERNALDATE is set and there is no envelope", internal: arrival, want: arrival},
		{name: "INTERNALDATE is zero and the Date header is set", envelope: &imap.Envelope{Date: header}, want: header},
		{name: "INTERNALDATE and the Date header are both zero", envelope: &imap.Envelope{}},
		{name: "INTERNALDATE is zero and there is no envelope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := appendDate(&imap.Message{InternalDate: tt.internal, Envelope: tt.envelope})

			if !got.Equal(tt.want) {
				t.Errorf("appendDate = %v, want %v", got, tt.want)
			}
			_, gotOffset := got.Zone()
			_, wantOffset := tt.want.Zone()
			if gotOffset != wantOffset {
				t.Errorf("offset = %d, want %d", gotOffset, wantOffset)
			}
		})
	}
}

func Test_AppendMessage_When_SourceHasInternalDate_Then_AppendCarriesItNotTheDateHeader(t *testing.T) {
	t.Parallel()

	plus2 := time.FixedZone("", 2*3600)
	header := time.Date(2026, 10, 25, 9, 8, 7, 0, time.UTC)
	tests := []struct {
		internalDate time.Time
		headerDate   time.Time
		name         string
		wantArg      string
		flags        []string
		noEnvelope   bool
	}{
		{
			name:         "INTERNALDATE wins over the Date header",
			internalDate: time.Date(2012, 7, 17, 9, 30, 0, 0, plus2),
			headerDate:   header,
			wantArg:      `INBOX "17-Jul-2012 09:30:00 +0200"`,
		},
		{
			name:       "the Date header is the fallback",
			headerDate: header,
			wantArg:    `INBOX "25-Oct-2026 09:08:07 +0000"`,
		},
		{
			name:         "a day below 10 keeps the leading space of go-imap",
			internalDate: time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*3600)),
			wantArg:      `INBOX " 2-Jan-2006 15:04:05 -0700"`,
		},
		{name: "no date at all sends no date", wantArg: "INBOX"},
		{name: "no INTERNALDATE and no envelope sends no date", noEnvelope: true, wantArg: "INBOX"},
		{
			name:         "flags and INTERNALDATE come in the order of the grammar",
			flags:        []string{`\Flagged`},
			internalDate: time.Date(2012, 7, 17, 9, 30, 0, 0, plus2),
			wantArg:      `INBOX (\Flagged) "17-Jul-2012 09:30:00 +0200"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			appends := make(chan capturedAppend, 1)
			srv := newFakeServer(t)
			srv.addConnHandler(appendCaptureHandler(srv, "OK APPEND completed", appends))
			c := newClientWithFake(t, srv)
			msg := buildTestIMAPMessage()
			msg.m.Flags = tt.flags
			msg.m.InternalDate = tt.internalDate
			msg.m.Envelope.Date = tt.headerDate
			if tt.noEnvelope {
				msg.m.Envelope = nil
			}

			err := awaitCall(t, "AppendMessage", func() error { return c.AppendMessage(t.Context(), "INBOX", msg) })

			if err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
			want := fmt.Sprintf("%s {%d}", tt.wantArg, len(testMessageBody(t)))
			if got := awaitAppend(t, appends).arg; got != want {
				t.Errorf("APPEND arguments = %q, want %q", got, want)
			}
		})
	}
}
