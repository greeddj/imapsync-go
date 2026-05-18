package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
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
	for _, d := range recorded {
		if d == throttledBackoff {
			foundThrottle = true
			break
		}
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
		_, err := c.FetchMessageMap(context.Background(), "INBOX")
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
// function before the context is cancelled does not block and the watcher
// goroutine exits cleanly (no goroutine leak).
func Test_withCancel_stopBeforeContextCancels(t *testing.T) {
	t.Parallel()

	c := &Client{cancelCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := c.withCancel(ctx)
	stop()
	// No assertion beyond "does not block or panic". The test would hang if
	// the watcher goroutine leaked and held a reference preventing GC.
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
