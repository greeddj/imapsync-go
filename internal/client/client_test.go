package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
	"golang.org/x/time/rate"
)

// TestSelectIfNeeded_shortCircuitsWhenSameFolder asserts the invariant that
// callers (FetchMessageMap, StreamMessagesByUIDs, getFolderSize) all rely on:
// a second call for the same folder on the same connection generation does
// not touch the underlying imapclient.Client. The test passes a nil cli on
// purpose — if short-circuit breaks, the call will panic on nil deref.
func TestSelectIfNeeded_shortCircuitsWhenSameFolder(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	f := "INBOX"
	c.selectedFolder.Store(&f)

	mbox, err := c.selectIfNeeded(nil, "INBOX")
	if err != nil {
		t.Fatalf("selectIfNeeded short-circuit returned err=%v", err)
	}
	if mbox != nil {
		t.Errorf("short-circuit returned mbox=%v, want nil", mbox)
	}
}

// TestSelectIfNeeded_differentFolderRequiresFreshSelect asserts that the
// short-circuit only fires for the cached folder; a different name must
// flow through to cli.Select. A nil cli triggers a panic, which the test
// recovers as proof that the short-circuit was *not* taken.
func TestSelectIfNeeded_differentFolderRequiresFreshSelect(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	f := "INBOX"
	c.selectedFolder.Store(&f)

	defer func() {
		if r := recover(); r == nil {
			t.Error("selectIfNeeded for a different folder did not call cli.Select")
		}
	}()
	_, _ = c.selectIfNeeded(nil, "Archive")
}

// Test_safeCall_retriesTransient asserts that safeCall reconnects and retries fn
// when fn returns a transient (io.EOF) error, and returns nil on the retry.
// Sequential — swaps the package-level sleepCtx var (used by reconnect).
func Test_safeCall_retriesTransient(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

	gen0 := c.connGen.Load()
	loginsBefore := srv.callCount("LOGIN")

	calls := 0
	err := c.safeCall(func(_ *imapclient.Client) error {
		calls++
		if calls == 1 {
			return io.EOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("safeCall returned err=%v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("fn called %d times, want 2", calls)
	}
	if got := srv.callCount("LOGIN"); got <= loginsBefore {
		t.Errorf("LOGIN count did not increase after reconnect (before=%d after=%d)", loginsBefore, got)
	}
	if c.connGen.Load() <= gen0 {
		t.Errorf("connGen not bumped: before=%d after=%d", gen0, c.connGen.Load())
	}
}

// Test_safeCall_doesNotRetryPermanent asserts that fn is called exactly once
// and the permanent error is returned to the caller without a reconnect.
func Test_safeCall_doesNotRetryPermanent(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)

	loginsBefore := srv.callCount("LOGIN")
	calls := 0
	err := c.safeCall(func(_ *imapclient.Client) error {
		calls++
		return fmt.Errorf("Invalid credentials")
	})
	if err == nil {
		t.Fatal("safeCall returned nil, want error")
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count changed (before=%d after=%d): reconnect must not fire for permanent error", loginsBefore, got)
	}
}

// Test_safeCall_doesNotRetryThrottled asserts that fn is called exactly once
// and a throttled error surfaces unchanged (not retried, not swallowed).
func Test_safeCall_doesNotRetryThrottled(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)

	loginsBefore := srv.callCount("LOGIN")
	calls := 0
	err := c.safeCall(func(_ *imapclient.Client) error {
		calls++
		return errors.New("Too many simultaneous connections")
	})
	if err == nil {
		t.Fatal("safeCall returned nil, want error")
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count changed (before=%d after=%d): reconnect must not fire for throttled error", loginsBefore, got)
	}
}

// Test_reconnect_bumpsConnGen asserts that every successful reconnect
// increments the generation counter.
// Sequential — swaps the package-level sleepCtx var.
func Test_reconnect_bumpsConnGen(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

	gen0 := c.connGen.Load()
	if err := c.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if c.connGen.Load() != gen0+1 {
		t.Errorf("connGen = %d, want %d", c.connGen.Load(), gen0+1)
	}
}

// Test_reconnect_clearsSelectedFolder asserts that reconnect resets the
// cached selected-folder pointer so the next safeCall re-issues SELECT.
// Sequential — swaps the package-level sleepCtx var.
func Test_reconnect_clearsSelectedFolder(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

	f := "INBOX"
	c.selectedFolder.Store(&f)

	if err := c.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if c.selectedFolder.Load() != nil {
		t.Errorf("selectedFolder not cleared after reconnect")
	}
}

// Test_reconnect_throttledBackoff asserts that when connectAndLogin returns a
// throttled error, reconnect calls sleepCtx with throttledBackoff (5m).
// Sequential — swaps the package-level sleepCtx var.
func Test_reconnect_throttledBackoff(t *testing.T) {
	srv := newFakeServer(t)

	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO Account exceeded bandwidth limits"))

	c := &Client{
		serverAddr:   srv.ln.Addr().String(),
		username:     "user",
		password:     "pass",
		backoff:      0,
		reconnectDur: 0,
		dialTimeout:  5 * time.Second,
		folderLocks:  make(map[string]*sync.Mutex),
		cancelCh:     make(chan struct{}),
	}
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	t.Cleanup(func() { c.Cancel(); _ = c.Logout() })

	if err := c.connectAndLogin(context.Background()); err != nil {
		t.Fatalf("initial connectAndLogin: %v", err)
	}

	var recorded []time.Duration
	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error {
		recorded = append(recorded, d)
		return nil
	}
	t.Cleanup(func() { sleepCtx = orig })

	if err := c.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	var foundThrottle bool
	if slices.Contains(recorded, throttledBackoff) {
		foundThrottle = true
	}
	if !foundThrottle {
		t.Errorf("throttledBackoff (%s) not recorded in sleepCtx calls: %v", throttledBackoff, recorded)
	}
}

// Test_selectIfNeeded_shortCircuitsOnNetwork asserts that a second call to
// selectIfNeeded for the same folder on the same connection does not send
// another EXAMINE to the server.
func Test_selectIfNeeded_shortCircuitsOnNetwork(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)

	cli := c.c.Load()
	if cli == nil {
		t.Fatal("no imap client after connectAndLogin")
	}

	if _, err := c.selectIfNeeded(cli, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded #1: %v", err)
	}
	examineAfterFirst := srv.callCount("EXAMINE")

	if _, err := c.selectIfNeeded(cli, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded #2: %v", err)
	}
	if got := srv.callCount("EXAMINE"); got != examineAfterFirst {
		t.Errorf("EXAMINE count changed on second call: before=%d after=%d (should short-circuit)", examineAfterFirst, got)
	}
}

// Test_selectIfNeeded_afterReconnect_reSelects asserts that after a reconnect
// (which clears selectedFolder), selectIfNeeded issues a fresh EXAMINE.
// Sequential — swaps the package-level sleepCtx var.
func Test_selectIfNeeded_afterReconnect_reSelects(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

	cli := c.c.Load()
	if cli == nil {
		t.Fatal("no imap client after connectAndLogin")
	}

	if _, err := c.selectIfNeeded(cli, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded #1: %v", err)
	}
	examineAfterFirst := srv.callCount("EXAMINE")
	if examineAfterFirst < 1 {
		t.Fatalf("expected at least 1 EXAMINE after first selectIfNeeded, got %d", examineAfterFirst)
	}

	if err := c.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	cli2 := c.c.Load()
	if cli2 == nil {
		t.Fatal("no imap client after reconnect")
	}

	if _, err := c.selectIfNeeded(cli2, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded #2: %v", err)
	}
	if got := srv.callCount("EXAMINE"); got != examineAfterFirst+1 {
		t.Errorf("EXAMINE count = %d, want %d after reconnect+reselect", got, examineAfterFirst+1)
	}
}

// Test_Cancel_interruptsBlockingCall asserts that calling Cancel() on a client
// blocked inside FetchMessageMap causes it to return within 100ms with a
// non-nil error.
func Test_Cancel_interruptsBlockingCall(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(stallingFetchHandler(srv))

	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
		loaded:    true,
	}

	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		_, _, err := c.FetchMessageMap(context.Background(), "INBOX")
		ch <- result{err}
	}()

	time.Sleep(10 * time.Millisecond)
	c.Cancel()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err == nil {
			t.Error("FetchMessageMap returned nil error after Cancel, want non-nil")
		}
	case <-timer.C:
		t.Error("FetchMessageMap did not return within 100ms after Cancel")
	}
}

// Test_ErrClass_String covers the String() method on ErrClass, including the
// unknown/zero-value case that has no named constant.
func Test_ErrClass_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		want string
		c    ErrClass
	}{
		{"transient", ClassTransient},
		{"permanent", ClassPermanent},
		{"throttled", ClassThrottled},
		{"unknown", ErrClass(99)}, // zero and any unrecognized value → "unknown"
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("ErrClass(%d).String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// Test_Logout_nilClient asserts that Logout returns nil without panicking when
// the underlying imap client pointer is nil (e.g. before connectAndLogin or
// after a failed dial).
func Test_Logout_nilClient(t *testing.T) {
	t.Parallel()

	c := &Client{cancelCh: make(chan struct{})}
	if err := c.Logout(); err != nil {
		t.Errorf("Logout on nil client returned err=%v, want nil", err)
	}
}

// Test_withCancel_stopBeforeContextCancels asserts that calling the stop
// function before the context is cancelled does not block or panic.
func Test_withCancel_stopBeforeContextCancels(t *testing.T) {
	t.Parallel()

	c := &Client{cancelCh: make(chan struct{})}
	ctx := t.Context()

	stop := c.withCancel(ctx)
	stop()
}

// Test_newDialer_setsTCPKeepalive asserts that newDialer returns a dialer with
// KeepAlive set to tcpKeepAlivePeriod and Timeout set to the provided value.
func Test_newDialer_setsTCPKeepalive(t *testing.T) {
	t.Parallel()

	d := newDialer(5 * time.Second)
	if d.KeepAlive != tcpKeepAlivePeriod {
		t.Errorf("KeepAlive = %v, want %v", d.KeepAlive, tcpKeepAlivePeriod)
	}
	if d.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want %v", d.Timeout, 5*time.Second)
	}
}

// Test_connectAndLogin_CancelInterruptsLogin asserts that calling Cancel() on a
// client whose fake server accepted the TCP connection and sent the IMAP banner
// but never replied to LOGIN causes connectAndLogin to return within 100ms with
// a non-nil error.
func Test_connectAndLogin_CancelInterruptsLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(stallingLoginHandler(srv))

	c := &Client{
		serverAddr:  srv.ln.Addr().String(),
		username:    "user",
		password:    "pass",
		dialTimeout: 5 * time.Second,
		folderLocks: make(map[string]*sync.Mutex),
		cancelCh:    make(chan struct{}),
	}
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	t.Cleanup(func() { c.Cancel(); _ = c.Logout() })

	ch := make(chan error, 1)
	go func() {
		ch <- c.connectAndLogin(context.Background())
	}()

	time.Sleep(10 * time.Millisecond)
	c.Cancel()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-ch:
		if err == nil {
			t.Error("connectAndLogin returned nil, want non-nil error after Cancel")
		}
	case <-timer.C:
		t.Error("connectAndLogin did not return within 100ms after Cancel")
	}
}

// Test_safeCall_CancelDuringReconnectLogin asserts the user-visible Ctrl+C path:
// when the first fn call returns io.EOF (forcing a reconnect) and the
// reconnect's LOGIN stalls, calling Cancel() causes safeCall to return within
// 100ms with a non-nil error.
//
// The stalling handler is registered after the initial connection is established
// so that the initial connectAndLogin consumes the default handler, and only the
// reconnect attempt hits the stalling handler.
func Test_safeCall_CancelDuringReconnectLogin(t *testing.T) {
	srv := newFakeServer(t)

	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	srv.addConnHandler(stallingLoginHandler(srv))

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

	ch := make(chan error, 1)
	go func() {
		ch <- c.safeCall(func(_ *imapclient.Client) error {
			return io.EOF
		})
	}()

	time.Sleep(10 * time.Millisecond)
	c.Cancel()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-ch:
		if err == nil {
			t.Error("safeCall returned nil, want non-nil error after Cancel during reconnect LOGIN")
		}
	case <-timer.C:
		t.Error("safeCall did not return within 100ms after Cancel during reconnect LOGIN")
	}
}

// Test_New_initialLoginInterruptibleByContextCancel asserts that passing a
// context that is cancelled mid-LOGIN to client.New causes it to return within
// 100ms with a non-nil error.
func Test_New_initialLoginInterruptibleByContextCancel(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(stallingLoginHandler(srv))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan error, 1)
	go func() {
		_, err := New(ctx, srv.ln.Addr().String(), "user", "pass", Options{
			DialTimeout: 5 * time.Second,
		})
		ch <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-ch:
		if err == nil {
			t.Error("New returned nil, want non-nil error after context cancel during LOGIN")
		}
	case <-timer.C:
		t.Error("New did not return within 100ms after context cancel during LOGIN")
	}
}

// Test_connectAndLogin_cramMD5Refused asserts that a refused AUTHENTICATE
// surfaces the server's error, leaves no connection behind and sends no LOGIN.
func Test_connectAndLogin_cramMD5Refused(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(cramMD5Handler(srv, "user", "server-side-secret"))

	c := newDisconnectedClient(t, srv)
	c.auth = "cram-md5"

	err := c.connectAndLogin(t.Context())
	if err == nil {
		t.Fatal("connectAndLogin returned nil, want the authentication error")
	}
	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassPermanent)
	}
	if c.c.Load() != nil {
		t.Error("c.c still holds a connection after a failed AUTHENTICATE")
	}
	if got := srv.callCount("AUTHENTICATE"); got != 1 {
		t.Errorf("AUTHENTICATE count = %d, want 1", got)
	}
	if got := srv.callCount("LOGIN"); got != 0 {
		t.Errorf("LOGIN count = %d, want 0 (CRAM-MD5 must not fall back to LOGIN)", got)
	}
	if got := srv.callCount("LOGOUT"); got != 1 {
		t.Errorf("LOGOUT count = %d, want 1 (failed session is closed)", got)
	}
}

// Test_New_cramMD5Refused asserts that New reports the authentication error
// itself, not the follow-up failure of loading the mailbox list on a client
// that has no connection.
func Test_New_cramMD5Refused(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(cramMD5Handler(srv, "user", "server-side-secret"))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{
		Auth:        "cram-md5",
		DialTimeout: 5 * time.Second,
	})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the authentication error")
	}
	if c != nil {
		t.Error("New returned a non-nil client alongside the error")
	}
	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassPermanent)
	}
	if strings.Contains(err.Error(), "failed to load mailbox list") {
		t.Errorf("error %q comes from the mailbox cache load, want the authentication error", err)
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST count = %d, want 0", got)
	}
}

// Test_New_cramMD5Succeeds asserts that the CRAM-MD5 digest sent by the client
// matches the one the server computes on its own, for any letter case of the
// configured mechanism.
func Test_New_cramMD5Succeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth string
	}{
		{name: "lower case mechanism", auth: "cram-md5"},
		{name: "upper case mechanism", auth: "CRAM-MD5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(cramMD5Handler(srv, "user", "pass"))

			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{
				Auth:        tt.auth,
				DialTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})

			if got := srv.callCount("CRAM-MD5 VERIFIED"); got != 1 {
				t.Errorf("verified digests = %d, want 1", got)
			}
			if got := srv.callCount("LOGIN"); got != 0 {
				t.Errorf("LOGIN count = %d, want 0", got)
			}
			if got := srv.callCount("LIST"); got != 1 {
				t.Errorf("LIST count = %d, want 1 (mailbox cache load after authentication)", got)
			}
		})
	}
}

// Test_connectAndLogin_whenDialFails asserts that a dial error is returned as
// is and leaves no connection behind.
func Test_connectAndLogin_whenDialFails(t *testing.T) {
	t.Parallel()

	errDial := errors.New("dial refused")
	c := newDisconnectedClient(t, newFakeServer(t))
	c.dialFn = func(context.Context, string) (net.Conn, error) { return nil, errDial }

	if err := c.connectAndLogin(t.Context()); !errors.Is(err, errDial) {
		t.Errorf("connectAndLogin error = %v, want %v", err, errDial)
	}
	if c.c.Load() != nil {
		t.Error("c.c holds a connection after a failed dial")
	}
}

// Test_New_whenDialFails asserts that New returns the dial error, classified
// as transient, without a client. The address has no port so the dial fails
// without touching the network.
func Test_New_whenDialFails(t *testing.T) {
	t.Parallel()

	c, err := New(t.Context(), "127.0.0.1", "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the dial error")
	}
	if c != nil {
		t.Error("New returned a non-nil client alongside the error")
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
}

// Test_New_whenMailboxListRefused asserts that a failing LIST after a
// successful login is reported as a cache load failure, which is what
// Test_New_cramMD5Refused must not see.
func Test_New_whenMailboxListRefused(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler((&mailboxScript{listRefused: true}).handler(srv))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the mailbox list failure")
	}
	if !strings.Contains(err.Error(), "failed to load mailbox list") {
		t.Errorf("error = %q, want it to mention the mailbox list load", err)
	}
	if got := srv.callCount("LOGOUT"); got != 1 {
		t.Errorf("LOGOUT count = %d, want 1 (half-built client is closed)", got)
	}
}

// Test_cramMD5Auth covers the SASL client the CRAM-MD5 login hands to go-imap.
func Test_cramMD5Auth(t *testing.T) {
	t.Parallel()

	a := &cramMD5Auth{username: "tim", password: "tanstaaftanstaaf"}

	t.Run("start names the mechanism and sends no initial response", func(t *testing.T) {
		t.Parallel()

		mech, ir, err := a.Start()
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if mech != "CRAM-MD5" {
			t.Errorf("mechanism = %q, want CRAM-MD5", mech)
		}
		if ir != nil {
			t.Errorf("initial response = %q, want nil", ir)
		}
	})

	t.Run("next rejects an empty challenge", func(t *testing.T) {
		t.Parallel()

		for name, challenge := range map[string][]byte{"nil": nil, "empty": {}} {
			if resp, err := a.Next(challenge); err == nil {
				t.Errorf("Next(%s challenge) = %q, want an error", name, resp)
			}
		}
	})

	t.Run("next answers the RFC 2195 example", func(t *testing.T) {
		t.Parallel()

		got, err := a.Next([]byte(cramMD5Challenge))
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if want := "tim b913a602c7eda7a495b4e6e7334d3890"; string(got) != want {
			t.Errorf("Next = %q, want %q", got, want)
		}
	})
}

// --- per-connection handler helpers ---

// connHandlerWithLoginReply returns a handler that sends the greeting, replies
// to LOGIN with the given reply string, and then handles LOGOUT. For non-OK
// login replies it closes immediately after the reply.
func connHandlerWithLoginReply(srv *fakeServer, loginReply string) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, loginReply)
				if !strings.HasPrefix(strings.ToUpper(loginReply), "OK") {
					return
				}
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

// stallingFetchHandler returns a per-connection handler that serves LOGIN and
// EXAMINE normally but never replies to FETCH, causing go-imap to block until
// the connection is closed by Cancel().
func stallingFetchHandler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			arg := ""
			if len(parts) == 3 {
				arg = parts[2]
			}
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "SELECT", "EXAMINE":
				// Return 1 message so FetchMessageMap proceeds to the FETCH step.
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n")
				_, _ = fmt.Fprintf(conn, "* 0 RECENT\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 1)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "FETCH":
				// conn.Read returns io.EOF / net.ErrClosed when Cancel()
				// terminates the connection, so this goroutine exits with
				// the test instead of leaking until the test binary does.
				b := make([]byte, 1)
				_, _ = conn.Read(b)
				return
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

func Test_withCancel_When_StoppedBeforeCancel_Then_ClientStaysUsable(t *testing.T) {
	t.Parallel()

	for i := range 1000 {
		c := &Client{cancelCh: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())

		stop := c.withCancel(ctx)
		stop()
		cancel()

		// Yield so an AfterFunc callback that stop failed to unregister gets to run.
		for range 4 {
			runtime.Gosched()
		}

		if c.isCancelled() {
			t.Fatalf("iteration %d: client cancelled by a context cancelled after stop", i)
		}
		select {
		case <-c.cancelCh:
			t.Fatalf("iteration %d: cancelCh closed by a context cancelled after stop", i)
		default:
		}
	}
}

func Test_withCancel_When_CanceledInFlight_Then_ClientIsCancelled(t *testing.T) {
	t.Parallel()

	c := &Client{cancelCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())

	stop := c.withCancel(ctx)
	t.Cleanup(stop)
	cancel()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.cancelCh:
	case <-timer.C:
		t.Fatal("cancelCh not closed within 5s of cancelling the context in flight")
	}
	if !c.isCancelled() {
		t.Error("isCancelled() = false after the context was cancelled in flight")
	}
}

// Test_New_whenClientsShareLimiters_Then_TheyDrawOneBudget asserts that clients built
// with the same limiters charge their traffic to those limiters, so the budget is
// global and each direction has its own bucket.
func Test_New_whenClientsShareLimiters_Then_TheyDrawOneBudget(t *testing.T) {
	t.Parallel()

	const burst = 1 << 20
	spend := func(clients int) (read, write float64) {
		t.Helper()

		// A near-zero refill rate keeps the bucket from refilling during the test.
		readLim := rate.NewLimiter(rate.Limit(1e-9), burst)
		writeLim := rate.NewLimiter(rate.Limit(1e-9), burst)
		srv := newFakeServer(t)
		for range clients {
			srv.addConnHandler(listDelimiterHandler(srv))
		}
		for range clients {
			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{
				DialTimeout:  5 * time.Second,
				ReadLimiter:  readLim,
				WriteLimiter: writeLim,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})
		}
		return burst - readLim.Tokens(), burst - writeLim.Tokens()
	}

	read1, write1 := spend(1)
	read3, write3 := spend(3)

	if read1 <= 0 || write1 <= 0 {
		t.Fatalf("one client spent read=%v write=%v tokens, want both > 0", read1, write1)
	}
	if diff := read3 - 3*read1; diff < -1 || diff > 1 {
		t.Errorf("three clients spent %v read tokens, want 3 x %v", read3, read1)
	}
	if diff := write3 - 3*write1; diff < -1 || diff > 1 {
		t.Errorf("three clients spent %v write tokens, want 3 x %v", write3, write1)
	}
}

func Test_safeCall_When_ServerClosesMidCommand_Then_ReconnectsAndRetries(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(_ net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		// Drop the socket with EXAMINE unanswered, as a server restart would.
		return cmd.verb == "EXAMINE", false
	}))
	srv.addConnHandler(interceptHandler(srv, func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
		return false, true
	}))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if _, _, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after a reconnect", err)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the drop)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the dropped command is retried)", got)
	}
}
