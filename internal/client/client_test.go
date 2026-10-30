package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/greeddj/imapsync-go/internal/termsafe"
	"golang.org/x/time/rate"
)

// Test_safeCall_retriesTransient asserts that safeCall reconnects and retries fn
// when fn returns a transient (io.EOF) error, and returns nil on the retry.
// Sequential - swaps the package-level sleepCtx var (used by reconnect).
func Test_safeCall_retriesTransient(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { sleepCtx = orig })

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

// Test_reconnect_throttledBackoff asserts that when connectAndLogin returns a
// throttled error, reconnect calls sleepCtx with throttledBackoff (5m).
// Sequential - swaps the package-level sleepCtx var.
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

const dovecotTempAuthFailure = "NO [UNAVAILABLE] Temporary authentication failure. [mx1:2026-10-25 10:00:00]"

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_ReconnectLoginFailsTemporarily_Then_RetriesAndRecovers(t *testing.T) {
	srv := newFakeServer(t)
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	c := newDisconnectedClient(t, srv)
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("initial connectAndLogin: %v", err)
	}

	var recorded []time.Duration
	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error {
		recorded = append(recorded, d)
		return nil
	}
	t.Cleanup(func() { sleepCtx = orig })

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
	if got := srv.callCount("LOGIN"); got != 3 {
		t.Errorf("LOGIN count = %d, want 3", got)
	}
	if want := []time.Duration{initialBackoff}; !slices.Equal(recorded, want) {
		t.Errorf("sleeps = %v, want %v", recorded, want)
	}
	if c.c.Load() == nil {
		t.Error("no imap client after the second reconnect attempt")
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_reconnect_When_LoginAlwaysFailsTemporarily_Then_StopsAfterMaxAttempts(t *testing.T) {
	srv := newFakeServer(t)
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	c := newDisconnectedClient(t, srv)
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("initial connectAndLogin: %v", err)
	}

	var recorded []time.Duration
	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error {
		recorded = append(recorded, d)
		return nil
	}
	t.Cleanup(func() { sleepCtx = orig })

	err := c.reconnect()
	if err == nil {
		t.Fatal("reconnect returned nil, want error")
	}
	for _, want := range []string{"failed to reconnect after retries", "Temporary authentication failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reconnect error %q does not contain %q", err, want)
		}
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Errorf("LOGIN count = %d, want %d", got, want)
	}
	if slices.Contains(recorded, throttledBackoff) {
		t.Errorf("sleeps %v hold throttledBackoff, want only the transient backoff", recorded)
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_reconnect_When_LoginFailsWithBadCredentials_Then_GivesUpAfterOneAttempt(t *testing.T) {
	var recorded []time.Duration
	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error {
		recorded = append(recorded, d)
		return nil
	}
	t.Cleanup(func() { sleepCtx = orig })

	tests := []struct {
		name  string
		reply string
	}{
		{name: "dovecot authentication failed", reply: "NO [AUTHENTICATIONFAILED] Authentication failed."},
		{name: "gmail invalid credentials", reply: "NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded = nil
			srv := newFakeServer(t)
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			srv.addConnHandler(connHandlerWithLoginReply(srv, tt.reply))
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			c := newDisconnectedClient(t, srv)
			if err := c.connectAndLogin(t.Context()); err != nil {
				t.Fatalf("initial connectAndLogin: %v", err)
			}

			err := c.reconnect()
			if err == nil {
				t.Fatal("reconnect returned nil, want error")
			}
			if got := classifyError(err); got != ClassPermanent {
				t.Errorf("classifyError(%v) = %s, want %s", err, got, ClassPermanent)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2", got)
			}
			if len(recorded) != 0 {
				t.Errorf("sleeps = %v, want none", recorded)
			}
		})
	}
}

// sleepLog holds every wait that reconnect asked for, with the LOGIN count the
// server had seen when the wait began.
type sleepLog struct {
	waits  []time.Duration
	logins []int
}

// recordSleeps swaps the package-level sleepCtx var, so the caller must be
// sequential. The swap is undone when the test ends.
func recordSleeps(t *testing.T, srv *fakeServer) *sleepLog {
	t.Helper()

	rec := &sleepLog{}
	orig := sleepCtx
	sleepCtx = func(_ context.Context, d time.Duration) error {
		rec.waits = append(rec.waits, d)
		rec.logins = append(rec.logins, srv.callCount("LOGIN"))
		return nil
	}
	t.Cleanup(func() { sleepCtx = orig })
	return rec
}

// transientBackoffs returns the waits between maxReconnectAttempts attempts
// when every failure takes the exponential branch.
func transientBackoffs() []time.Duration {
	waits := make([]time.Duration, 0, maxReconnectAttempts-1)
	for k := range maxReconnectAttempts - 1 {
		waits = append(waits, initialBackoff<<k)
	}
	return waits
}

// Sequential - swaps the package-level sleepCtx var.
func Test_reconnect_When_EveryAttemptFails_Then_NoWaitFollowsTheLastAttempt(t *testing.T) {
	transientWaits := transientBackoffs()
	throttledWaits := slices.Repeat([]time.Duration{throttledBackoff}, maxReconnectAttempts-1)

	tests := []struct {
		name      string
		reply     string
		wantWaits []time.Duration
		wantClass ErrClass
	}{
		{name: "transient failure", reply: dovecotTempAuthFailure, wantClass: ClassTransient, wantWaits: transientWaits},
		{name: "unknown failure", reply: "NO [UNAVAILABLE] Server busy", wantClass: ClassUnknown, wantWaits: transientWaits},
		{name: "throttled failure", reply: "NO [ALERT] Too many simultaneous connections", wantClass: ClassThrottled, wantWaits: throttledWaits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t)
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			for range maxReconnectAttempts {
				srv.addConnHandler(connHandlerWithLoginReply(srv, tt.reply))
			}
			c := newDisconnectedClient(t, srv)
			if err := c.connectAndLogin(t.Context()); err != nil {
				t.Fatalf("initial connectAndLogin: %v", err)
			}
			tr := &recordingTracker{}
			c.SetProgressTracker(tr)
			rec := recordSleeps(t, srv)

			err := c.reconnect()
			if err == nil {
				t.Fatal("reconnect returned nil, want error")
			}
			if !strings.Contains(err.Error(), "failed to reconnect after retries") {
				t.Errorf("reconnect error %q does not contain %q", err, "failed to reconnect after retries")
			}
			if got := classifyError(err); got != tt.wantClass {
				t.Errorf("classifyError(%v) = %s, want %s", err, got, tt.wantClass)
			}
			if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
				t.Errorf("LOGIN count = %d, want %d", got, want)
			}
			if !slices.Equal(rec.waits, tt.wantWaits) {
				t.Errorf("waits = %v, want %v", rec.waits, tt.wantWaits)
			}
			total := srv.callCount("LOGIN")
			for k, got := range rec.logins {
				if want := 2 + k; got != want {
					t.Errorf("LOGIN count at wait %d = %d, want %d", k, got, want)
				}
				if got >= total {
					t.Errorf("no LOGIN follows wait %d: %d at the wait, %d in total", k, got, total)
				}
			}

			if len(tr.messages) == 0 {
				t.Fatal("tracker got no message")
			}
			last := tr.messages[len(tr.messages)-1]
			if want := fmt.Sprintf("giving up after %d", maxReconnectAttempts); !strings.Contains(last, want) {
				t.Errorf("last tracker message = %q, want it to contain %q", last, want)
			}
			var announced int
			for _, m := range tr.messages {
				if strings.Contains(m, "retrying in") || strings.Contains(m, "backing off") {
					announced++
				}
			}
			if want := maxReconnectAttempts - 1; announced != want {
				t.Errorf("%d tracker messages announce a wait, want %d: %q", announced, want, tr.messages)
			}

			if c.c.Load() != nil {
				t.Error("imap client is set after every attempt failed")
			}
			if !c.mu.TryLock() {
				t.Fatal("c.mu is still locked after reconnect returned")
			}
			c.mu.Unlock()
		})
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_reconnect_When_LastAttemptFailsPermanently_Then_ReturnsTheRefusal(t *testing.T) {
	srv := newFakeServer(t)
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	for range maxReconnectAttempts - 1 {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Authentication failed."))
	c := newDisconnectedClient(t, srv)
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("initial connectAndLogin: %v", err)
	}
	tr := &recordingTracker{}
	c.SetProgressTracker(tr)
	rec := recordSleeps(t, srv)

	err := c.reconnect()
	if err == nil {
		t.Fatal("reconnect returned nil, want error")
	}
	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(%v) = %s, want %s", err, got, ClassPermanent)
	}
	if strings.Contains(err.Error(), "failed to reconnect after retries") {
		t.Errorf("reconnect error %q reports an exhausted budget, want the refusal itself", err)
	}
	if want := transientBackoffs(); !slices.Equal(rec.waits, want) {
		t.Errorf("waits = %v, want %v", rec.waits, want)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Errorf("LOGIN count = %d, want %d", got, want)
	}
	if len(tr.messages) == 0 {
		t.Fatal("tracker got no message")
	}
	if last, want := tr.messages[len(tr.messages)-1], "Permanent error, giving up"; !strings.Contains(last, want) {
		t.Errorf("last tracker message = %q, want it to contain %q", last, want)
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_reconnect_When_LastAttemptSucceeds_Then_Reconnects(t *testing.T) {
	srv := newFakeServer(t)
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	for range maxReconnectAttempts - 1 {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
	c := newDisconnectedClient(t, srv)
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("initial connectAndLogin: %v", err)
	}
	rec := recordSleeps(t, srv)

	if err := c.reconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if c.c.Load() == nil {
		t.Error("no imap client after the last attempt succeeded")
	}
	if want := transientBackoffs(); !slices.Equal(rec.waits, want) {
		t.Errorf("waits = %v, want %v", rec.waits, want)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
		t.Errorf("LOGIN count = %d, want %d", got, want)
	}
	if c.backoff != initialBackoff {
		t.Errorf("backoff = %s, want %s", c.backoff, initialBackoff)
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

// Sequential - swaps the package-level sleepCtx var.
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

func Test_selectIfNeeded_When_SessionEndedByBye_Then_DoesNotShortCircuit(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeAfterExamine(release)))
	c := newWatchedClient(t, srv, 0)
	cli := c.c.Load()

	if _, err := c.selectIfNeeded(cli, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded #1: %v", err)
	}
	releaseOnce()
	awaitSignal(t, cli.LoggedOut(), "the session to end after the BYE")

	mbox, err := c.selectIfNeeded(cli, "INBOX")
	if mbox != nil {
		t.Errorf("selectIfNeeded #2 returned mbox=%v, want nil", mbox)
	}
	if !errors.Is(err, imapclient.ErrNotLoggedIn) {
		t.Fatalf("selectIfNeeded #2 error = %v, want %v", err, imapclient.ErrNotLoggedIn)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if got := srv.callCount("EXAMINE"); got != 1 {
		t.Errorf("EXAMINE count = %d, want 1 (a dead session sends nothing)", got)
	}
}

func Test_selectIfNeeded_When_FolderDiffers_Then_SelectsAgain(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newClientWithFake(t, srv)
	cli := c.c.Load()

	for _, folder := range []string{"INBOX", "Archive"} {
		if _, err := c.selectIfNeeded(cli, folder); err != nil {
			t.Fatalf("selectIfNeeded %s: %v", folder, err)
		}
	}

	if got, want := srv.capturedNames("EXAMINE"), []string{"INBOX", "Archive"}; !slices.Equal(got, want) {
		t.Errorf("EXAMINE names = %v, want %v", got, want)
	}
}

func Test_safeCall_When_ByeLandsBetweenSelectAndFetch_Then_ReconnectsAndRetries(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeAfterExamine(release)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)

	attempts := 0
	err := c.safeCall(func(cli *imapclient.Client) error {
		attempts++
		if _, err := c.selectIfNeeded(cli, "INBOX"); err != nil {
			return err
		}
		if attempts == 1 {
			releaseOnce()
			awaitSignal(t, cli.LoggedOut(), "the session to end after the BYE")
		}
		uidSet := new(imap.SeqSet)
		uidSet.AddNum(1)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() { done <- cli.UidFetch(uidSet, []imap.FetchItem{fullBodyPeekSection.FetchItem()}, messages) }()
		for range messages {
		}
		return <-done
	})
	if err != nil {
		t.Fatalf("safeCall: %v, want success after the reconnect", err)
	}

	if attempts != 2 {
		t.Errorf("closure ran %d times, want 2", attempts)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session for the retry)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the retry selects again)", got)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1 (the first attempt sends nothing)", got)
	}
}

// Test_Cancel_interruptsBlockingCall asserts that calling Cancel() on a client
// blocked inside FetchMessageMap causes it to return within 100ms with a
// non-nil error.
func Test_Cancel_interruptsBlockingCall(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	fetch := newStall(verbIs("FETCH"))
	srv.addConnHandler(interceptHandler(srv, fetch.intercept))

	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
	}

	errc := make(chan error, 1)
	go func() {
		_, _, err := c.FetchMessageMap(context.Background(), "INBOX")
		errc <- err
	}()

	awaitSignal(t, fetch.reached, "the FETCH to reach the server")
	c.Cancel()

	err := awaitError(t, errc, "FetchMessageMap after Cancel")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FetchMessageMap error = %v, want %v", err, context.Canceled)
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

func Test_connectAndLogin_CancelInterruptsLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newDisconnectedClient(t, srv)

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(context.Background())
	}()

	awaitSignal(t, st.reached, "the server to receive LOGIN")
	c.Cancel()

	err := awaitError(t, errc, "connectAndLogin")
	if err == nil {
		t.Error("connectAndLogin returned nil, want non-nil error after Cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("connectAndLogin error = %v, want context.Canceled", err)
	}
	if c.c.Load() != nil {
		t.Error("c.c still holds a connection after the login was cancelled")
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
	t.Parallel()

	srv := newFakeServer(t)

	// reconnectDur and backoff at zero keep reconnect from sleeping.
	c := newClientWithFake(t, srv)
	c.reconnectDur = 0
	c.backoff = 0

	login := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, login.intercept))

	errc := make(chan error, 1)
	go func() {
		errc <- c.safeCall(func(_ *imapclient.Client) error {
			return io.EOF
		})
	}()

	awaitSignal(t, login.reached, "the reconnect LOGIN to reach the server")
	c.Cancel()

	err := awaitError(t, errc, "safeCall after Cancel during the reconnect LOGIN")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want %v", err, context.Canceled)
	}
}

func Test_New_initialLoginInterruptibleByContextCancel(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		_, err := New(ctx, srv.ln.Addr().String(), "user", "pass", Options{
			DialTimeout: 5 * time.Second,
		})
		errc <- err
	}()

	awaitSignal(t, st.reached, "the server to receive LOGIN")
	cancel()

	err := awaitError(t, errc, "New")
	if err == nil {
		t.Error("New returned nil, want non-nil error after context cancel during LOGIN")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("New error = %v, want context.Canceled", err)
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
		{name: "mixed case mechanism", auth: "Cram-Md5"},
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

			if got := srv.callCount("AUTHENTICATE"); got != 1 {
				t.Errorf("AUTHENTICATE count = %d, want 1", got)
			}
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

// A dial would run the server handler before New returns, so the handler shows whether New dialed.
func Test_New_When_AuthIsUnsupported_Then_RefusesBeforeDialing(t *testing.T) {
	t.Parallel()

	const password = "secret-pass"

	for _, auth := range []string{"cram_md5", "crammd5", "xoauth2", "plain", "abcdefghijklmnop", "abcd efgh ijkl mnop", " login", "login ", " cram-md5", "cram-md5 "} {
		t.Run(auth, func(t *testing.T) {
			t.Parallel()

			var dialed atomic.Bool
			srv := newFakeServer(t)
			serve := cramMD5Handler(srv, "user", password)
			srv.addConnHandler(func(conn net.Conn) {
				dialed.Store(true)
				serve(conn)
			})

			c, err := New(t.Context(), srv.ln.Addr().String(), "user", password, Options{
				Auth:        auth,
				DialTimeout: 5 * time.Second,
			})

			if c != nil {
				c.Cancel()
				t.Fatal("New returned a client, want nil")
			}
			if !errors.Is(err, errUnsupportedAuth) {
				t.Fatalf("New error = %v, want %v", err, errUnsupportedAuth)
			}
			if strings.Contains(err.Error(), strconv.Quote(auth)) {
				t.Errorf("New error = %q, want no quoted value %s", err, strconv.Quote(auth))
			}
			if strings.Contains(err.Error(), auth) {
				t.Errorf("New error = %q, want no value %q", err, auth)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("New error = %q, want no password", err)
			}
			if dialed.Load() {
				t.Error("server accepted a connection, want no dial")
			}
			if got := srv.callCount("LOGIN"); got != 0 {
				t.Errorf("LOGIN count = %d, want 0", got)
			}
			if got := srv.callCount("AUTHENTICATE"); got != 0 {
				t.Errorf("AUTHENTICATE count = %d, want 0", got)
			}
		})
	}
}

func Test_New_When_AuthIsLoginInAnyCase_Then_SendsLogin(t *testing.T) {
	t.Parallel()

	for _, auth := range []string{"", "login", "LOGIN", "Login"} {
		t.Run(strconv.Quote(auth), func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(cramMD5Handler(srv, "user", "pass"))

			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{
				Auth:        auth,
				DialTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})

			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1", got)
			}
			if got := srv.callCount("AUTHENTICATE"); got != 0 {
				t.Errorf("AUTHENTICATE count = %d, want 0", got)
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

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{Label: "src", DialTimeout: 5 * time.Second})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the mailbox list failure")
	}
	if !strings.Contains(err.Error(), "failed to load mailbox list") {
		t.Errorf("error = %q, want it to mention the mailbox list load", err)
	}
	if !strings.Contains(err.Error(), "[src] list mailboxes") {
		t.Errorf("error = %q, want it to name the side that failed", err)
	}
	if got := srv.callCount("LOGOUT"); got != 1 {
		t.Errorf("LOGOUT count = %d, want 1 (half-built client is closed)", got)
	}
}

// newReadyClient runs New against srv and stops the client when the test ends.
func newReadyClient(t *testing.T, srv *fakeServer, opts Options) *Client {
	t.Helper()

	opts.DialTimeout = 5 * time.Second
	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})
	return c
}

func Test_New_When_ServerAdvertisesGmailExtensionAfterLogin_Then_KeepsOneCopyPerMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		loginCode string
	}{
		{name: "LOGIN OK carries the list", loginCode: "[CAPABILITY IMAP4rev1 X-GM-EXT-1]"},
		{name: "CAPABILITY reply lists it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(loginAs(tt.loginCode), capabilityAs("IMAP4rev1 X-GM-EXT-1"))))

			c := newReadyClient(t, srv, Options{})

			if !c.KeepsOneCopyPerMessageID() {
				t.Error("KeepsOneCopyPerMessageID = false, want true")
			}
			if got := srv.callCount("CAPABILITY"); got != 1 {
				t.Errorf("CAPABILITY count = %d, want 1", got)
			}
		})
	}
}

func Test_New_When_ServerSpellsTheGmailExtensionInAnotherLetterCase_Then_KeepsOneCopyPerMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		loginCode string
		spelling  string
	}{
		{name: "lower case in the CAPABILITY reply", spelling: "x-gm-ext-1"},
		{name: "mixed case in the CAPABILITY reply", spelling: "X-Gm-Ext-1"},
		{name: "lower case in LOGIN OK and the CAPABILITY reply", spelling: "x-gm-ext-1", loginCode: "[CAPABILITY IMAP4rev1 x-gm-ext-1]"},
		{name: "mixed case in LOGIN OK and the CAPABILITY reply", spelling: "X-Gm-Ext-1", loginCode: "[CAPABILITY IMAP4rev1 X-Gm-Ext-1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(loginAs(tt.loginCode), capabilityAs("IMAP4rev1 "+tt.spelling))))

			c := newReadyClient(t, srv, Options{})

			if !c.KeepsOneCopyPerMessageID() {
				t.Error("KeepsOneCopyPerMessageID = false, want true: letter case carries no meaning in a capability name")
			}
			if got := srv.callCount("CAPABILITY"); got != 1 {
				t.Errorf("CAPABILITY count = %d, want 1", got)
			}
		})
	}
}

func Test_New_When_ServerDoesNotAdvertiseGmailExtensionAfterLogin_Then_ReportsFalse(t *testing.T) {
	t.Parallel()

	const (
		plainGreeting = "* OK [CAPABILITY IMAP4rev1] fake ready"
		gmailGreeting = "* OK [CAPABILITY IMAP4rev1 X-GM-EXT-1] fake ready"
	)
	tests := []struct {
		name      string
		greeting  string
		loginCode string
	}{
		{name: "nowhere", greeting: plainGreeting},
		{name: "only in the greeting, which go-imap drops at LOGIN", greeting: gmailGreeting},
		{name: "in the greeting, and LOGIN OK carries a list without it", greeting: gmailGreeting, loginCode: "[CAPABILITY IMAP4rev1]"},
		{name: "LOGIN OK lists it, the CAPABILITY reply does not", greeting: plainGreeting, loginCode: "[CAPABILITY IMAP4rev1 X-GM-EXT-1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandlerGreeting(srv, tt.greeting, chainHooks(loginAs(tt.loginCode), capabilityAs("IMAP4rev1"))))

			c := newReadyClient(t, srv, Options{})

			if c.KeepsOneCopyPerMessageID() {
				t.Error("KeepsOneCopyPerMessageID = true, want false")
			}
			if got := srv.callCount("CAPABILITY"); got != 1 {
				t.Errorf("CAPABILITY count = %d, want 1", got)
			}
		})
	}
}

func Test_New_When_ContextEndsDuringCapabilityCheck_Then_ReturnsCanceled(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("CAPABILITY"))
	ended := make(chan struct{})
	srv.addConnHandler(endedSignal(interceptHandler(srv, st.intercept), ended))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var got *Client
	errc := make(chan error, 1)
	go func() {
		var err error
		got, err = New(ctx, srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
		errc <- err
	}()

	awaitSignal(t, st.reached, "the server to receive CAPABILITY")
	cancel()

	err := awaitError(t, errc, "New")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("New error = %v, want context.Canceled", err)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
	awaitSignal(t, ended, "the client to close the socket")
	if n := srv.callCount("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1: a cancel must not reconnect", n)
	}
}

func Test_New_When_CapabilityIsRefused_Then_FailsAndNamesTheLabel(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "CAPABILITY" {
			return false, true
		}
		writeLine(conn, "%s NO capability refused", cmd.tag)
		return true, true
	}))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{Label: "dst", DialTimeout: 5 * time.Second})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the capability failure")
	}

	if c != nil {
		t.Error("New returned a client alongside the error")
	}
	for _, want := range []string{"failed to read server capabilities", "[dst] capability", "capability refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	for verb, want := range map[string]int{"CAPABILITY": 1, "LOGIN": 1, "LOGOUT": 1} {
		if got := srv.callCount(verb); got != want {
			t.Errorf("%s count = %d, want %d", verb, got, want)
		}
	}
}

func Test_New_When_ConnectionDropsDuringCapabilityCheck_Then_ReconnectsAndReadsTheNewSession(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(_ net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		return cmd.verb == "CAPABILITY", false
	}))
	srv.addConnHandler(interceptHandler(srv, chainHooks(loginAs("[CAPABILITY IMAP4rev1 X-GM-EXT-1]"), capabilityAs("IMAP4rev1 X-GM-EXT-1"))))

	c := newReadyClient(t, srv, Options{})

	if !c.KeepsOneCopyPerMessageID() {
		t.Error("KeepsOneCopyPerMessageID = false, want true: the answer comes from the new session")
	}
	for verb, want := range map[string]int{"LOGIN": 2, "LIST": 1, "CAPABILITY": 2} {
		if got := srv.callCount(verb); got != want {
			t.Errorf("%s count = %d, want %d", verb, got, want)
		}
	}
}

func Test_detectOneCopyPerID_When_ContextAlreadyEnded_Then_SendsNoCommand(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := c.detectOneCopyPerID(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("detectOneCopyPerID error = %v, want context.Canceled", err)
	}
	if got := srv.callCount("CAPABILITY"); got != 0 {
		t.Errorf("CAPABILITY count = %d, want 0", got)
	}
}

// capabilityAnsweredContext reports cancellation once the server has seen CAPABILITY. Its Done
// channel stays nil, so the client is not cancelled and the end lands after the reply.
type capabilityAnsweredContext struct {
	context.Context
	srv *fakeServer
}

func (c capabilityAnsweredContext) Done() <-chan struct{} { return nil }

func (c capabilityAnsweredContext) Err() error {
	if c.srv.callCount("CAPABILITY") > 0 {
		return context.Canceled
	}
	return nil
}

func Test_detectOneCopyPerID_When_ContextEndsAfterTheReply_Then_ReturnsErrorAndStoresNothing(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, capabilityAs("IMAP4rev1 X-GM-EXT-1")))
	c := newClientWithFake(t, srv)

	err := c.detectOneCopyPerID(capabilityAnsweredContext{Context: t.Context(), srv: srv})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("detectOneCopyPerID error = %v, want context.Canceled", err)
	}
	if got := srv.callCount("CAPABILITY"); got != 1 {
		t.Errorf("CAPABILITY count = %d, want 1 (the context must end after the reply)", got)
	}
	if c.KeepsOneCopyPerMessageID() {
		t.Error("KeepsOneCopyPerMessageID = true, want false: a cancelled check stores nothing")
	}
}

func Test_hasCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		caps map[string]bool
		name string
		find string
		want bool
	}{
		{name: "exact name", caps: map[string]bool{"X-GM-EXT-1": true}, want: true},
		{name: "lower case", caps: map[string]bool{"x-gm-ext-1": true}, want: true},
		{name: "mixed case", caps: map[string]bool{"X-Gm-Ext-1": true}, want: true},
		{name: "among other names", caps: map[string]bool{"IMAP4rev1": true, "UIDPLUS": true, "x-gm-ext-1": true, "IDLE": true}, want: true},
		{name: "nil map"},
		{name: "empty map", caps: map[string]bool{}},
		{name: "longer name with the same prefix", caps: map[string]bool{"X-GM-EXT-10": true}},
		{name: "shorter name", caps: map[string]bool{"X-GM-EXT": true}},
		{name: "other version", caps: map[string]bool{"X-GM-EXT-2": true}},
		{name: "extra leading letter", caps: map[string]bool{"XX-GM-EXT-1": true}},
		{name: "Cyrillic look-alike", caps: map[string]bool{"\u0425-GM-EXT-1": true}},
		{name: "matching key with a false value", caps: map[string]bool{"X-GM-EXT-1": false}},
		{name: "another name in another letter case", caps: map[string]bool{"UidPlus": true}, find: "UIDPLUS", want: true},
		{name: "another name that the map lacks", caps: map[string]bool{"X-GM-EXT-1": true}, find: "UIDPLUS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			find := tt.find
			if find == "" {
				find = gmailExtension
			}
			if got := hasCapability(tt.caps, find); got != tt.want {
				t.Errorf("hasCapability(%v, %q) = %t, want %t", tt.caps, find, got, tt.want)
			}
		})
	}
}

// Test_hasCapability_When_Called_Then_AllocatesNothing does not run in parallel, because AllocsPerRun counts the allocations of the whole process.
func Test_hasCapability_When_Called_Then_AllocatesNothing(t *testing.T) {
	caps := make(map[string]bool, 20)
	for _, name := range []string{
		"IMAP4rev1", "STARTTLS", "AUTH=PLAIN", "AUTH=LOGIN", "UIDPLUS", "IDLE", "NAMESPACE", "CHILDREN", "ID", "MOVE",
		"UNSELECT", "LITERAL+", "SASL-IR", "ENABLE", "QUOTA", "CONDSTORE", "ESEARCH", "SPECIAL-USE", "LIST-STATUS", "x-gm-ext-1",
	} {
		caps[name] = true
	}
	missing := maps.Clone(caps)
	delete(missing, "x-gm-ext-1")

	var sink bool
	if got := testing.AllocsPerRun(100, func() { sink = hasCapability(caps, gmailExtension) }); got != 0 {
		t.Errorf("allocations on a match = %v, want 0", got)
	}
	if got := testing.AllocsPerRun(100, func() { sink = hasCapability(missing, gmailExtension) }); got != 0 {
		t.Errorf("allocations on a miss = %v, want 0", got)
	}
	_ = sink
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

func Test_New_When_ServerNeverGreets_Then_ContextCancelReturns(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	accepted, closed := make(chan struct{}), make(chan struct{})
	srv.addConnHandler(silentHandler(accepted, closed))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var got *Client
	errc := make(chan error, 1)
	go func() {
		var err error
		// The long dial timeout keeps the greeting timer from being what ends the wait.
		got, err = New(ctx, srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: time.Minute})
		errc <- err
	}()

	awaitSignal(t, accepted, "the server to accept the connection")
	cancel()

	err := awaitError(t, errc, "New")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("New error = %v, want context.Canceled", err)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
	awaitSignal(t, closed, "the client to close the socket")
}

func Test_New_When_ServerNeverGreets_Then_FailsAfterDialTimeout(t *testing.T) {
	t.Parallel()

	const dialTimeout = 200 * time.Millisecond

	srv := newFakeServer(t)
	accepted, closed := make(chan struct{}), make(chan struct{})
	srv.addConnHandler(silentHandler(accepted, closed))

	var got *Client
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		var err error
		got, err = New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: dialTimeout})
		errc <- err
	}()

	err := awaitError(t, errc, "New")
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("New error = %v, want os.ErrDeadlineExceeded", err)
	}
	if class := classifyError(err); class != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s so a reconnect retries it", err, class, ClassTransient)
	}
	if elapsed < dialTimeout {
		t.Errorf("New failed after %s, want at least the dial timeout %s", elapsed, dialTimeout)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
	awaitSignal(t, closed, "the client to close the socket")
}

// Test_New_When_CapabilityProbeStalls_Then_FailsAfterDialTimeout pins the greeting
// bound to a timer: the greeting has no capability list, so go-imap probes
// CAPABILITY, which a connection deadline would not survive.
func Test_New_When_CapabilityProbeStalls_Then_FailsAfterDialTimeout(t *testing.T) {
	t.Parallel()

	const dialTimeout = 200 * time.Millisecond

	srv := newFakeServer(t)
	st := newStall(verbIs("CAPABILITY"))
	srv.addConnHandler(func(conn net.Conn) {
		serveCommands(srv, conn, "* OK fake ready", func(rd *bufio.Reader, cmd command) bool {
			_, keep := st.intercept(conn, rd, cmd)
			return keep
		})
	})

	var got *Client
	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		var err error
		got, err = New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: dialTimeout})
		errc <- err
	}()

	err := awaitError(t, errc, "New")
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("New error = %v, want os.ErrDeadlineExceeded", err)
	}
	if class := classifyError(err); class != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s so a reconnect retries it", err, class, ClassTransient)
	}
	if elapsed < dialTimeout {
		t.Errorf("New failed after %s, want at least the dial timeout %s", elapsed, dialTimeout)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
	awaitSignal(t, st.reached, "the server to receive CAPABILITY")
}

func Test_safeCall_When_ReconnectGreetingStalls_Then_CancelReturns(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)

	accepted, closed := make(chan struct{}), make(chan struct{})
	srv.addConnHandler(silentHandler(accepted, closed))
	c.reconnectDur = 0
	c.backoff = 0
	// The long dial timeout keeps the greeting timer from being what ends the wait.
	c.dialTimeout = time.Minute

	errc := make(chan error, 1)
	go func() {
		errc <- c.safeCall(func(*imapclient.Client) error { return io.EOF })
	}()

	awaitSignal(t, accepted, "the reconnect to reach the server")
	c.Cancel()

	if err := awaitError(t, errc, "safeCall"); !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want context.Canceled", err)
	}
	awaitSignal(t, closed, "the client to close the reconnect socket")
}

func Test_connectAndLogin_When_CancelledBeforeSessionStored_Then_ReturnsCanceled(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newDisconnectedClient(t, srv)

	c.Cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(context.Background())
	}()

	if err := awaitError(t, errc, "connectAndLogin"); !errors.Is(err, context.Canceled) {
		t.Errorf("connectAndLogin error = %v, want context.Canceled", err)
	}
	if c.c.Load() != nil {
		t.Error("c.c holds a connection after a login that never started")
	}
	if got := srv.callCount("LOGIN"); got != 0 {
		t.Errorf("LOGIN count = %d, want 0 (a cancelled client must not log in)", got)
	}
}

func Test_Logout_When_ServerStallsLogout_Then_ReturnsWithinBound(t *testing.T) {
	t.Parallel()

	const bound = 100 * time.Millisecond

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGOUT"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newClientWithFake(t, srv)
	c.logoutDur = bound

	errc := make(chan error, 1)
	start := time.Now()
	go func() {
		errc <- c.Logout()
	}()

	err := awaitError(t, errc, "Logout")
	elapsed := time.Since(start)

	if !errors.Is(err, errLogoutTimeout) {
		t.Errorf("Logout error = %v, want errLogoutTimeout", err)
	}
	if elapsed < bound {
		t.Errorf("Logout returned after %s, want at least the bound %s", elapsed, bound)
	}
	awaitSignal(t, st.reached, "the server to receive LOGOUT")
}

func Test_reconnect_When_OldSessionStallsLogout_Then_ReconnectsWithinBound(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGOUT"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newClientWithFake(t, srv)
	c.logoutDur = 100 * time.Millisecond
	c.reconnectDur = 0
	c.backoff = 0

	calls := 0
	errc := make(chan error, 1)
	go func() {
		errc <- c.safeCall(func(*imapclient.Client) error {
			calls++
			if calls == 1 {
				return io.EOF
			}
			return nil
		})
	}()

	if err := awaitError(t, errc, "safeCall"); err != nil {
		t.Fatalf("safeCall error = %v, want nil after a reconnect", err)
	}
	awaitSignal(t, st.reached, "the server to receive LOGOUT")

	if calls != 2 {
		t.Errorf("fn called %d times, want 2", calls)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the old session is replaced)", got)
	}
}

func Test_connectAndLogin_When_LoginRefusedAndLogoutStalls_Then_ReturnsAuthError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGOUT"))
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb == "LOGIN" {
			writeLine(conn, "%s NO [AUTHENTICATIONFAILED] Authentication failed", cmd.tag)
			return true, true
		}
		return st.intercept(conn, rd, cmd)
	}))
	c := newDisconnectedClient(t, srv)
	c.logoutDur = 100 * time.Millisecond

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(t.Context())
	}()

	err := awaitError(t, errc, "connectAndLogin")
	awaitSignal(t, st.reached, "the server to receive LOGOUT")

	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassPermanent)
	}
	if c.c.Load() != nil {
		t.Error("c.c still holds a connection after a refused LOGIN")
	}
}

func Test_Logout_When_ServerReplies_Then_ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.logoutDur = time.Minute

	if err := c.Logout(); err != nil {
		t.Errorf("Logout error = %v, want nil", err)
	}
	if got := srv.callCount("LOGOUT"); got != 1 {
		t.Errorf("LOGOUT count = %d, want 1", got)
	}
}

// go-imap closes the socket only on BYE, so Logout must close it itself.
func Test_Logout_When_ServerAnswersWithoutBye_Then_ClosesSocket(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	closed := make(chan struct{})
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LOGOUT" {
			return false, true
		}
		writeLine(conn, "%s OK LOGOUT completed", cmd.tag)
		_, _ = io.Copy(io.Discard, rd)
		close(closed)
		return true, false
	}))
	c := newDisconnectedClient(t, srv)
	retainDialed(t, c)
	// The long bound keeps the logout timer from being what closes the socket.
	c.logoutDur = time.Minute

	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}

	if err := c.Logout(); err != nil {
		t.Errorf("Logout error = %v, want nil", err)
	}
	awaitSignal(t, closed, "the client to close the socket")
}

// Test_connectAndLogin_When_GreetingIsBye_Then_ClosesSocket covers the session
// go-imap already marks as logged out, whose LOGOUT never reaches the wire.
func Test_connectAndLogin_When_GreetingIsBye_Then_ClosesSocket(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	closed := make(chan struct{})
	srv.addConnHandler(holdHandler("* BYE server shutting down", closed))
	c := newDisconnectedClient(t, srv)
	retainDialed(t, c)
	// The long bound keeps the logout timer from being what closes the socket.
	c.logoutDur = time.Minute

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(t.Context())
	}()

	if err := awaitError(t, errc, "connectAndLogin"); err == nil {
		t.Error("connectAndLogin returned nil error after a BYE greeting")
	}
	if c.c.Load() != nil {
		t.Error("c.c holds a connection after a BYE greeting")
	}
	awaitSignal(t, closed, "the client to close the socket")
}

func Test_New_When_ServerClosesBeforeGreeting_Then_ErrorWrapsEOF(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(func(conn net.Conn) { _ = conn.Close() })

	var got *Client
	errc := make(chan error, 1)
	go func() {
		var err error
		// The long dial timeout keeps the greeting timer from being what ends the wait.
		got, err = New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: time.Minute})
		errc <- err
	}()

	err := awaitError(t, errc, "New")
	if err == nil {
		got.Cancel()
		t.Fatal("New returned nil error, want the greeting failure")
	}

	if !strings.HasPrefix(err.Error(), "imap greeting: ") {
		t.Errorf("New error = %q, want the imap greeting prefix", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("New error = %v, want it to wrap io.EOF", err)
	}
	if class := classifyError(err); class != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s so a reconnect retries it", err, class, ClassTransient)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
}

func Test_connectAndLogin_When_GreetingIsRefused_Then_ClosesSocketAndWrapsError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	closed := make(chan struct{})
	// go-imap accepts only OK, PREAUTH and BYE as a greeting status.
	srv.addConnHandler(holdHandler("* NO try again later", closed))
	c := newDisconnectedClient(t, srv)
	retainDialed(t, c)
	// The long dial timeout keeps the greeting timer from being what ends the wait.
	c.dialTimeout = time.Minute

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(t.Context())
	}()

	err := awaitError(t, errc, "connectAndLogin")
	if err == nil {
		t.Fatal("connectAndLogin returned nil error, want the greeting failure")
	}

	if !strings.HasPrefix(err.Error(), "imap greeting: ") {
		t.Errorf("connectAndLogin error = %q, want the imap greeting prefix", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("connectAndLogin error = %v, want a refusal that is neither a cancellation nor a timeout", err)
	}
	if c.c.Load() != nil {
		t.Error("c.c holds a connection after a refused greeting")
	}
	awaitSignal(t, closed, "the client to close the socket")
}

func Test_connectAndLogin_When_DialTimeoutIsZero_Then_GreetingHasNoTimer(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	serve := interceptHandler(srv, func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
		return false, true
	})
	srv.addConnHandler(func(conn net.Conn) {
		<-gate
		serve(conn)
	})
	c := newDisconnectedClient(t, srv)
	c.dialTimeout = 0

	dial, dialed := c.dialFn, make(chan struct{})
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		defer close(dialed)
		return dial(ctx, addr)
	}

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(t.Context())
	}()

	awaitSignal(t, dialed, "the client to dial")
	// Hold the greeting back and yield, so a timer armed despite the zero timeout fires first.
	for range 100 {
		runtime.Gosched()
	}
	release()

	if err := awaitError(t, errc, "connectAndLogin"); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}
	if c.c.Load() == nil {
		t.Error("c.c is nil after a successful login")
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1", got)
	}
}

func Test_greet_When_ContextEndsDuringCapabilityProbe_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("CAPABILITY"))
	srv.addConnHandler(func(conn net.Conn) {
		serveCommands(srv, conn, "* OK fake ready", func(rd *bufio.Reader, cmd command) bool {
			_, keep := st.intercept(conn, rd, cmd)
			return keep
		})
	})
	c := newDisconnectedClient(t, srv)
	// The long dial timeout keeps the greeting timer from being what ends the wait.
	c.dialTimeout = time.Minute

	conn, err := c.dialFn(t.Context(), c.serverAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var got *imapclient.Client
	errc := make(chan error, 1)
	go func() {
		var err error
		// go-imap ignores a failed CAPABILITY probe, so only the context tells the caller.
		got, err = c.greet(ctx, conn)
		errc <- err
	}()

	awaitSignal(t, st.reached, "the server to receive CAPABILITY")
	cancel()

	if err := awaitError(t, errc, "greet"); !errors.Is(err, context.Canceled) {
		t.Errorf("greet error = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Error("greet returned a client alongside the error")
	}
}

func Test_connectAndLogin_When_CancelledBeforeSessionStored_Then_ClosesSocket(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	closed := make(chan struct{})
	srv.addConnHandler(holdHandler(scriptedGreeting, closed))
	c := newDisconnectedClient(t, srv)
	retainDialed(t, c)

	c.Cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(context.Background())
	}()

	if err := awaitError(t, errc, "connectAndLogin"); !errors.Is(err, context.Canceled) {
		t.Errorf("connectAndLogin error = %v, want context.Canceled", err)
	}
	awaitSignal(t, closed, "the client to close the socket")
}

func Test_connectAndLogin_When_ContextExpiresDuringLogin_Then_ReturnsItsError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newDisconnectedClient(t, srv)

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() {
		errc <- c.connectAndLogin(ctx)
	}()

	awaitSignal(t, st.reached, "the server to receive LOGIN")
	ctx.fire()

	err := awaitError(t, errc, "connectAndLogin")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("connectAndLogin error = %v, want %v, not the context.Canceled of a cancelled client", err, context.DeadlineExceeded)
	}
	if c.c.Load() != nil {
		t.Error("c.c still holds a connection after the login was cut short")
	}
}

func Test_New_When_LoginSucceeds_Then_LogoutIsBounded(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if c.logoutDur != logoutTimeout {
		t.Errorf("logoutDur = %s, want %s so a silent server cannot hold Logout", c.logoutDur, logoutTimeout)
	}
	if c.reviveDur != reviveInterval {
		t.Errorf("reviveDur = %s, want %s so a dead server gets one probe per interval", c.reviveDur, reviveInterval)
	}
}

func Test_New_When_ContextEndsAfterReturn_Then_ClientStaysUsable(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	c, err := New(ctx, srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	cancel()
	// Yield so an AfterFunc callback that New failed to unregister gets to run.
	for range 100 {
		runtime.Gosched()
	}

	if c.isCancelled() {
		t.Fatal("client cancelled by a context that ended after New returned")
	}
	if err := c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }); err != nil {
		t.Errorf("NOOP after the context ended: %v, want nil", err)
	}
}

func Test_safeCall_When_ServerStopsAnswering_Then_WatchdogReconnectsAndRetries(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	examine := newStall(verbIs("EXAMINE"))
	hungUp := make(chan struct{})
	srv.addConnHandler(endedSignal(interceptHandler(srv, examine.intercept), hungUp))
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newWatchedClient(t, srv, tripLimit)
	first := c.idle.Load()

	start := time.Now()
	err := awaitCall(t, "FetchMessageMap", func() error {
		_, _, err := c.FetchMessageMap(t.Context(), "INBOX")
		return err
	})
	if err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after the watchdog reconnect", err)
	}
	elapsed := time.Since(start)

	if elapsed < tripLimit {
		t.Errorf("FetchMessageMap returned after %s, want at least the %s limit", elapsed, tripLimit)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the trip)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the stalled command is retried)", got)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	if !first.tripped() {
		t.Error("the first session's watchdog did not trip")
	}
	if second := c.idle.Load(); second == first || second.tripped() {
		t.Error("the retry did not run on a fresh, untripped watchdog")
	}
	awaitSignal(t, hungUp, "the client to close the stalled connection")
}

func Test_safeCall_When_ReplyTricklesWithinLimit_Then_WatchdogDoesNotCut(t *testing.T) {
	t.Parallel()

	const gap = holdLimit / 10

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "EXAMINE" {
			return false, true
		}
		return true, trickle(conn, cmd.tag+" OK [READ-ONLY] EXAMINE completed\r\n", gap)
	}))
	c := newWatchedClient(t, srv, holdLimit)

	start := time.Now()
	if _, _, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil {
		t.Fatalf("FetchMessageMap: %v, want the trickled reply to complete", err)
	}
	if elapsed := time.Since(start); elapsed < 3*holdLimit {
		t.Fatalf("the reply took %s, want a trickle of at least 3 limits", elapsed)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (no reconnect while bytes keep arriving)", got)
	}
	if c.idle.Load().tripped() {
		t.Error("the watchdog tripped on a reply that kept moving")
	}
}

func Test_Client_When_IdleBetweenCommands_Then_WatchdogDoesNotClose(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	hungUp := make(chan struct{})
	srv.addConnHandler(endedSignal(interceptHandler(srv, serveNormally), hungUp))
	c := newWatchedClient(t, srv, holdLimit)

	stayOpen(t, hungUp, 3*holdLimit+holdLimit/2, "the client closed an idle session")
	if err := c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }); err != nil {
		t.Fatalf("safeCall after the idle period: %v", err)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (the idle session is reused)", got)
	}
	if got := srv.callCount("NOOP"); got != 1 {
		t.Errorf("NOOP count = %d, want 1", got)
	}
}

func Test_safeCall_When_ReconnectLoginStalls_Then_WatchdogTripsAndNextAttemptSucceeds(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	login := newStall(verbIs("LOGIN"))
	hungUp := make(chan struct{})
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	srv.addConnHandler(endedSignal(interceptHandler(srv, login.intercept), hungUp))
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newWatchedClient(t, srv, tripLimit)

	calls := 0
	err := awaitCall(t, "safeCall", func() error {
		return c.safeCall(func(*imapclient.Client) error {
			calls++
			if calls == 1 {
				return io.EOF
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("safeCall: %v, want success on the third connection", err)
	}
	if calls != 2 {
		t.Errorf("fn ran %d times, want 2", calls)
	}
	if got := srv.callCount("LOGIN"); got != 3 {
		t.Errorf("LOGIN count = %d, want 3 (initial, stalled, healthy)", got)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	awaitSignal(t, hungUp, "the client to close the connection whose LOGIN stalled")
}

func Test_connectAndLogin_When_AuthenticationStalls_Then_WatchdogFailsTransiently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    string
		stalled string
	}{
		{name: "LOGIN gets no reply", stalled: "LOGIN"},
		{name: "CRAM-MD5 gets no challenge", auth: "cram-md5", stalled: "AUTHENTICATE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			stuck := newStall(verbIs(tt.stalled))
			hungUp := make(chan struct{})
			srv.addConnHandler(endedSignal(interceptHandler(srv, stuck.intercept), hungUp))
			c := newDisconnectedClient(t, srv)
			c.idleDur = tripLimit
			c.auth = tt.auth

			err := awaitCall(t, "connectAndLogin", func() error {
				return c.connectAndLogin(t.Context())
			})
			if !errors.Is(err, errIdleTimeout) {
				t.Errorf("connectAndLogin error = %v, want it to wrap errIdleTimeout", err)
			}
			if got := classifyError(err); got != ClassTransient {
				t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
			}
			if c.c.Load() != nil {
				t.Error("c.c still holds a connection after the stalled authentication")
			}
			awaitSignal(t, hungUp, "the client to close the connection")
		})
	}
}

func Test_FetchMessageMap_When_CancelledDuringStall_Then_ReturnsCanceledNotIdleTimeout(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	examine := newStall(verbIs("EXAMINE"))
	srv.addConnHandler(interceptHandler(srv, examine.intercept))
	c := newWatchedClient(t, srv, time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() {
		_, _, err := c.FetchMessageMap(ctx, "INBOX")
		errc <- err
	}()
	awaitSignal(t, examine.reached, "the server to receive EXAMINE")
	cancel()

	err := awaitError(t, errc, "FetchMessageMap")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, errIdleTimeout) {
		t.Errorf("error = %v, a cancellation must not read as an idle timeout", err)
	}
	if c.idle.Load().tripped() {
		t.Error("the watchdog tripped on a cancellation")
	}
}

func Test_FetchMessageMap_When_CancelledDuringReconnectAfterTrip_Then_ReturnsCanceledNotIdleTimeout(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	examine := newStall(verbIs("EXAMINE"))
	login := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, examine.intercept))
	srv.addConnHandler(interceptHandler(srv, login.intercept))
	c := newWatchedClient(t, srv, tripLimit)
	// The connection the reconnect dials must not trip, so only Ctrl-C ends its LOGIN.
	c.idleDur = time.Minute

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() {
		_, _, err := c.FetchMessageMap(ctx, "INBOX")
		errc <- err
	}()
	awaitSignal(t, login.reached, "the reconnect LOGIN to reach the server")
	cancel()

	err := awaitError(t, errc, "FetchMessageMap")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, errIdleTimeout) {
		t.Errorf("error = %v, a cancellation must not read as an idle timeout", err)
	}
	if !c.isCancelled() {
		t.Error("isCancelled() = false after the context ended")
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (initial and the stalled reconnect)", got)
	}
}

func Test_safeCall_When_ReadRateLimitWaitExceedsIdleLimit_Then_WatchdogDoesNotTrip(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newDisconnectedClient(t, srv)
	c.idleDur = holdLimit
	c.backoff = 0
	// Unlimited while connecting; the burst must stay above zero because waitN chunks by it.
	lim := rate.NewLimiter(rate.Inf, 64)
	c.readLimiter = lim
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}

	// 20 B/s makes the reply of a NOOP (about 26 bytes) wait over a second for tokens.
	lim.SetLimit(rate.Limit(20))
	if !lim.AllowN(time.Now(), lim.Burst()) {
		t.Fatal("could not drain the token bucket")
	}

	start := time.Now()
	if err := c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }); err != nil {
		t.Fatalf("safeCall: %v, want the throttled reply to complete", err)
	}
	if elapsed := time.Since(start); elapsed < 3*holdLimit {
		t.Errorf("the reply took %s, want a rate-limit wait of at least 3 limits", elapsed)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (a token wait is not server silence)", got)
	}
	if c.idle.Load().tripped() {
		t.Error("the watchdog tripped during a rate-limit wait")
	}
}

func Test_New_When_Built_Then_WatchdogUsesIdleTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
	}{
		{name: "without rate limits", opts: Options{DialTimeout: 5 * time.Second}},
		{name: "with rate limits", opts: Options{
			DialTimeout:  5 * time.Second,
			ReadLimiter:  rate.NewLimiter(rate.Inf, 1<<20),
			WriteLimiter: rate.NewLimiter(rate.Inf, 1<<20),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", tt.opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})

			if c.idleDur != idleTimeout {
				t.Errorf("idleDur = %s, want %s", c.idleDur, idleTimeout)
			}
			if c.idle.Load() == nil {
				t.Error("no watchdog is armed on the session")
			}
		})
	}
}

func Test_newClientWithFake_When_IdleDurIsZero_Then_NoWatchdogAndClientWorks(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)

	if c.idle.Load() != nil {
		t.Error("a zero idleDur still produced a watchdog")
	}
	if err := c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }); err != nil {
		t.Fatalf("safeCall without a watchdog: %v", err)
	}
}

func Test_safeCall_When_RetryAfterWatchdogReconnectStalls_Then_FailsAfterOneRetry(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	firstStall := newStall(verbIs("NOOP"))
	secondStall := newStall(verbIs("NOOP"))
	firstHungUp := make(chan struct{})
	secondHungUp := make(chan struct{})
	srv.addConnHandler(endedSignal(interceptHandler(srv, firstStall.intercept), firstHungUp))
	srv.addConnHandler(endedSignal(interceptHandler(srv, secondStall.intercept), secondHungUp))
	c := newWatchedClient(t, srv, tripLimit)
	first := c.idle.Load()

	calls := 0
	err := awaitCall(t, "safeCall", func() error {
		return c.safeCall(func(cli *imapclient.Client) error {
			calls++
			return cli.Noop()
		})
	})
	if err == nil {
		t.Fatal("safeCall returned nil, want the idle timeout of the retry")
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
	if calls != 2 {
		t.Errorf("fn ran %d times, want 2 (the call and exactly one retry)", calls)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one reconnect, no third attempt)", got)
	}
	if got := srv.callCount("NOOP"); got != 2 {
		t.Errorf("NOOP count = %d, want 2 (the stalled command is seen once per connection)", got)
	}
	if second := c.idle.Load(); second == first || !second.tripped() {
		t.Error("the retry did not run under the watchdog of the fresh session")
	}
	awaitSignal(t, firstHungUp, "the client to close the first stalled connection")
	awaitSignal(t, secondHungUp, "the client to close the second stalled connection")
}

func Test_safeCall_When_EveryReconnectLoginStalls_Then_WatchdogGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	hungUp := make([]chan struct{}, maxReconnectAttempts)
	for i := range hungUp {
		hungUp[i] = make(chan struct{})
		login := newStall(verbIs("LOGIN"))
		srv.addConnHandler(endedSignal(interceptHandler(srv, login.intercept), hungUp[i]))
	}
	c := newWatchedClient(t, srv, tripLimit)

	calls := 0
	err := awaitCall(t, "safeCall", func() error {
		return c.safeCall(func(*imapclient.Client) error {
			calls++
			return io.EOF
		})
	})
	if err == nil {
		t.Fatal("safeCall returned nil, want the reconnect failure")
	}
	if !errors.Is(err, errIdleTimeout) {
		t.Errorf("error = %v, want it to wrap errIdleTimeout", err)
	}
	if !strings.Contains(err.Error(), "failed to reconnect after retries") {
		t.Errorf("error = %v, want the reconnect give-up message", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", err, got, ClassTransient)
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	if calls != 1 {
		t.Errorf("fn ran %d times, want 1 (a failed reconnect leaves nothing to retry on)", calls)
	}
	if got := srv.callCount("LOGIN"); got != 1+maxReconnectAttempts {
		t.Errorf("LOGIN count = %d, want %d (initial plus every reconnect attempt)", got, 1+maxReconnectAttempts)
	}
	for i, ch := range hungUp {
		awaitSignal(t, ch, fmt.Sprintf("the client to close the connection of reconnect attempt %d", i+1))
	}
	if !c.mu.TryLock() {
		t.Fatal("c.mu is still held after the reconnect gave up")
	}
	c.mu.Unlock()
}

func Test_safeCall_When_CancelledAfterWatchdogTrip_Then_ReturnsCanceledWithoutReconnect(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	noop := newStall(verbIs("NOOP"))
	srv.addConnHandler(interceptHandler(srv, noop.intercept))
	c := newWatchedClient(t, srv, tripLimit)
	w := c.idle.Load()

	calls := 0
	err := awaitCall(t, "safeCall", func() error {
		return c.safeCall(func(cli *imapclient.Client) error {
			calls++
			err := cli.Noop()
			// The watchdog has cut the connection by now; Ctrl-C lands before safeCall looks at the error.
			c.Cancel()
			return err
		})
	})
	if !w.tripped() {
		t.Fatal("the watchdog did not trip, so the test did not reach the trip-then-cancel order")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, errIdleTimeout) {
		t.Errorf("error = %v, a cancellation must win over the idle timeout", err)
	}
	if calls != 1 {
		t.Errorf("fn ran %d times, want 1 (no retry after Ctrl-C)", calls)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (no reconnect after Ctrl-C)", got)
	}
}

func Test_Client_When_OperationReturns_Then_NoWatchdogSpanStaysOpen(t *testing.T) {
	t.Parallel()

	normal := func(srv *fakeServer) func(net.Conn) { return interceptHandler(srv, serveNormally) }
	tests := []struct {
		handler func(srv *fakeServer) func(net.Conn)
		run     func(c *Client) error
		name    string
		wantErr bool
	}{
		{
			name:    "safeCall succeeds",
			handler: normal,
			run:     func(c *Client) error { return c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }) },
		},
		{
			name:    "safeCall fails permanently",
			handler: normal,
			run: func(c *Client) error {
				return c.safeCall(func(*imapclient.Client) error { return errors.New("refused") })
			},
			wantErr: true,
		},
		{
			name:    "FetchMessageMap succeeds",
			handler: normal,
			run: func(c *Client) error {
				_, _, err := c.FetchMessageMap(context.Background(), "INBOX")
				return err
			},
		},
		{
			name:    "AppendMessage accepted",
			handler: func(srv *fakeServer) func(net.Conn) { return appendOutcomeHandler(srv, "OK APPEND completed") },
			run: func(c *Client) error {
				return c.AppendMessage(context.Background(), "INBOX", buildTestIMAPMessage())
			},
		},
		{
			name:    "AppendMessage refused",
			handler: func(srv *fakeServer) func(net.Conn) { return appendOutcomeHandler(srv, "NO [OVERQUOTA] mailbox full") },
			run: func(c *Client) error {
				return c.AppendMessage(context.Background(), "INBOX", buildTestIMAPMessage())
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(tt.handler(srv))
			c := newWatchedClient(t, srv, holdLimit)
			w := c.idle.Load()

			if err := tt.run(c); (err != nil) != tt.wantErr {
				t.Fatalf("operation error = %v, want an error: %t", err, tt.wantErr)
			}
			if got := w.active.Load(); got != 0 {
				t.Errorf("open spans after the operation = %d, want 0 (an idle session would trip)", got)
			}
			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1", got)
			}
		})
	}
}

func Test_New_ErrorLogFollowsVerbose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check   func(t *testing.T, got io.Writer)
		name    string
		verbose bool
	}{
		{
			name: "quiet session discards go-imap error lines",
			check: func(t *testing.T, got io.Writer) {
				t.Helper()
				if got != io.Discard {
					t.Errorf("ErrorLog writer = %T, want io.Discard", got)
				}
			},
		},
		{
			name:    "verbose session escapes them on the way to stderr",
			verbose: true,
			check: func(t *testing.T, got io.Writer) {
				t.Helper()
				if _, ok := got.(*termsafe.Writer); !ok {
					t.Errorf("ErrorLog writer = %T, want *termsafe.Writer", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second, Verbose: tt.verbose})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})

			l, ok := c.c.Load().ErrorLog.(*log.Logger)
			if !ok {
				t.Fatalf("ErrorLog is %T, want *log.Logger", c.c.Load().ErrorLog)
			}
			tt.check(t, l.Writer())
		})
	}
}

func Test_Client_log_When_MessageCarriesControlCharacters_Then_EverySinkGetsEscapedText(t *testing.T) {
	t.Parallel()

	const (
		hostile = "Evil\x1b[2J\r\n\u202egpj\xff"
		want    = `[p] Fetching folder Evil\x1b[2J\r\n\u202egpj\xff...`
	)

	t.Run("tracker", func(t *testing.T) {
		t.Parallel()

		c := newTestClient()
		tr := &recordingTracker{}
		c.SetProgressTracker(tr)

		c.log("[%s] Fetching folder %s...", "p", hostile)

		if !slices.Equal(tr.messages, []string{want}) {
			t.Errorf("tracker messages = %q, want [%q]", tr.messages, want)
		}
	})

	t.Run("progress writer when verbose", func(t *testing.T) {
		t.Parallel()

		c := newTestClient()
		c.verbose = true
		var logged []string
		c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

		c.log("[%s] Fetching folder %s...", "p", hostile)

		if !slices.Equal(logged, []string{want}) {
			t.Errorf("logged = %q, want [%q]", logged, want)
		}
	})

	t.Run("percent signs in a name are not read as a format", func(t *testing.T) {
		t.Parallel()

		c := newTestClient()
		c.verbose = true
		var logged []string
		c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

		c.log("[%s] Fetching folder %s...", "p", "100%s%d")

		if want := "[p] Fetching folder 100%s%d..."; !slices.Equal(logged, []string{want}) {
			t.Errorf("logged = %q, want [%q]", logged, want)
		}
	})
}

type silentLogger struct{}

func (silentLogger) Printf(string, ...any) {}
func (silentLogger) Println(...any)        {}

func Test_routeErrorLog(t *testing.T) {
	t.Parallel()

	t.Run("verbose escapes what go-imap logs and keeps the line break", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		cli := &imapclient.Client{ErrorLog: log.New(&buf, "", 0)}
		c := &Client{verbose: true}

		c.routeErrorLog(cli)
		cli.ErrorLog.Println("cannot handle response ", "\x1b]0;pwned\x07 refused\r now \u202e")

		want := `cannot handle response  \x1b]0;pwned\a refused\r now \u202e` + "\n"
		if got := buf.String(); got != want {
			t.Errorf("logged = %q, want %q", got, want)
		}
	})

	t.Run("quiet discards what go-imap logs", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		l := log.New(&buf, "", 0)
		cli := &imapclient.Client{ErrorLog: l}
		c := &Client{}

		c.routeErrorLog(cli)
		l.Println("error reading response:", "boom")

		if l.Writer() != io.Discard {
			t.Errorf("logger writer = %T, want io.Discard", l.Writer())
		}
		if buf.Len() != 0 {
			t.Errorf("logged %q, want nothing", buf.String())
		}
	})

	t.Run("a logger that is not a log.Logger is left alone", func(t *testing.T) {
		t.Parallel()

		for _, verbose := range []bool{false, true} {
			cli := &imapclient.Client{ErrorLog: silentLogger{}}
			c := &Client{verbose: verbose}

			c.routeErrorLog(cli)

			if _, ok := cli.ErrorLog.(silentLogger); !ok {
				t.Errorf("verbose=%t: ErrorLog is %T, want the original silentLogger", verbose, cli.ErrorLog)
			}
		}
	})
}

func Test_safeCall_When_WatchdogTrips_Then_LogsWhyItReconnects(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	examine := newStall(verbIs("EXAMINE"))
	srv.addConnHandler(interceptHandler(srv, examine.intercept))
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newWatchedClient(t, srv, tripLimit)
	c.prefix = "src"
	var (
		mu    sync.Mutex
		lines []string
	)
	c.SetProgressWriter(&logCapture{fn: func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}})

	err := awaitCall(t, "FetchMessageMap", func() error {
		_, _, err := c.FetchMessageMap(t.Context(), "INBOX")
		return err
	})
	if err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after the watchdog reconnect", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := fmt.Sprintf("[src] No data from server for %s, reconnecting", tripLimit)
	if !slices.Contains(lines, want) {
		t.Errorf("log lines = %q, want them to include %q", lines, want)
	}
}

func Test_noteIdleTrip_When_ErrorIsNotATrip_Then_LogsNothing(t *testing.T) {
	t.Parallel()

	c := &Client{prefix: "src"}
	var lines []string
	c.SetProgressWriter(&logCapture{fn: func(s string) { lines = append(lines, s) }})
	c.noteIdleTrip(errors.New("imap: connection closed"))
	if len(lines) != 0 {
		t.Errorf("log lines = %q, want none for an error the watchdog did not cause", lines)
	}

	tr := &recordingTracker{}
	c.SetProgressWriter(nil)
	c.SetProgressTracker(tr)
	c.idleDur = time.Minute
	c.noteIdleTrip(fmt.Errorf("read: %w", errIdleTimeout))
	if want := []string{"[src] No data from server for 1m0s, reconnecting"}; !slices.Equal(tr.messages, want) {
		t.Errorf("tracker messages = %q, want %q", tr.messages, want)
	}
}

func checkStoredGiveUp(t *testing.T, err, stored error, want ErrClass) {
	t.Helper()

	if !errors.Is(err, stored) {
		t.Errorf("error = %v, want the stored give-up %v", err, stored)
	}
	if got := classifyError(err); got != want {
		t.Errorf("classifyError(%v) = %s, want %s", err, got, want)
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_EarlierReconnectGaveUp_Then_NextCallWaitsAndReconnects(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{name: "temporary failures use the whole budget", reply: dovecotTempAuthFailure},
		{name: "throttled refusals use the whole budget", reply: "NO [ALERT] Too many simultaneous connections"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t)
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			for range maxReconnectAttempts {
				srv.addConnHandler(connHandlerWithLoginReply(srv, tt.reply))
			}
			c := newClientWithFake(t, srv)
			c.reconnectDur = reconnectInterval
			c.reviveDur = 0
			rec := recordSleeps(t, srv)

			if err := c.safeCall(func(*imapclient.Client) error { return io.EOF }); err == nil {
				t.Fatal("first safeCall returned nil, want the reconnect to give up")
			}
			if c.c.Load() != nil {
				t.Fatal("the client still holds a session after the give-up")
			}
			if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts; got != want {
				t.Fatalf("LOGIN count after the give-up = %d, want %d", got, want)
			}
			waitsBefore := len(rec.waits)

			runs := 0
			err := c.safeCall(func(*imapclient.Client) error {
				runs++
				return nil
			})
			if err != nil {
				t.Fatalf("second safeCall error = %v, want nil", err)
			}
			if runs != 1 {
				t.Errorf("fn ran %d times, want 1", runs)
			}
			if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts+1; got != want {
				t.Errorf("LOGIN count = %d, want %d", got, want)
			}
			waits := rec.waits[waitsBefore:]
			if len(waits) != 1 {
				t.Fatalf("second safeCall waited %v, want exactly one wait", waits)
			}
			if waits[0] <= 0 || waits[0] > reconnectInterval {
				t.Errorf("wait = %s, want within (0, %s]", waits[0], reconnectInterval)
			}
			if got, want := rec.logins[waitsBefore], 1+maxReconnectAttempts; got != want {
				t.Errorf("LOGIN count at the wait = %d, want %d (the wait comes before the new LOGIN)", got, want)
			}
			if c.c.Load() == nil {
				t.Error("the client holds no session after the second safeCall")
			}
			if c.giveUpErr != nil {
				t.Errorf("giveUpErr = %v after the login, want nil", c.giveUpErr)
			}
		})
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_ServerStillRefusesAfterGiveUp_Then_EachCallSpendsOneAttempt(t *testing.T) {
	tests := []struct {
		name       string
		reply      string
		wantText   string
		refusals   int
		wantLogins int
		wantWaits  int
		wantClass  ErrClass
	}{
		{
			name:       "temporary failures",
			reply:      dovecotTempAuthFailure,
			refusals:   maxReconnectAttempts + 1,
			wantLogins: 1,
			wantWaits:  1,
			wantText:   "failed to reconnect after retries",
			wantClass:  ClassTransient,
		},
		{
			name:       "permanent refusal",
			reply:      "NO [AUTHENTICATIONFAILED] Authentication failed.",
			refusals:   1,
			wantLogins: 0,
			wantWaits:  0,
			wantClass:  ClassPermanent,
		},
		{
			name:       "account lock",
			reply:      "NO Authentication failed: account temporarily locked",
			refusals:   1,
			wantLogins: 0,
			wantWaits:  0,
			wantText:   "account temporarily locked",
			wantClass:  ClassPermanent,
		},
		{
			name:       "account lockout",
			reply:      "NO Account in lockout state",
			refusals:   1,
			wantLogins: 0,
			wantWaits:  0,
			wantText:   "lockout state",
			wantClass:  ClassPermanent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t)
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			for range tt.refusals {
				srv.addConnHandler(connHandlerWithLoginReply(srv, tt.reply))
			}
			c := newClientWithFake(t, srv)
			c.reconnectDur = reconnectInterval
			c.reviveDur = 0
			rec := recordSleeps(t, srv)

			if err := c.safeCall(func(*imapclient.Client) error { return io.EOF }); err == nil {
				t.Fatal("first safeCall returned nil, want the reconnect to give up")
			}
			loginsBefore := srv.callCount("LOGIN")
			waitsBefore := len(rec.waits)

			runs := 0
			err := c.safeCall(func(*imapclient.Client) error {
				runs++
				return nil
			})
			if err == nil {
				t.Fatal("second safeCall returned nil, want the refusal")
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
			if got := classifyError(err); got != tt.wantClass {
				t.Errorf("classifyError(%v) = %s, want %s", err, got, tt.wantClass)
			}
			if runs != 0 {
				t.Errorf("fn ran %d times, want 0 (there was no session to run it on)", runs)
			}
			if got := srv.callCount("LOGIN") - loginsBefore; got != tt.wantLogins {
				t.Errorf("the second call sent %d LOGINs, want %d", got, tt.wantLogins)
			}
			if got, want := srv.callCount("LOGIN"), 1+tt.refusals; got != want {
				t.Errorf("LOGIN count = %d, want %d (the initial login and every refusal)", got, want)
			}
			waits := rec.waits[waitsBefore:]
			if len(waits) != tt.wantWaits {
				t.Fatalf("second safeCall waited %v, want %d wait", waits, tt.wantWaits)
			}
			for _, w := range waits {
				if w <= 0 || w > reconnectInterval {
					t.Errorf("wait = %s, want the interval wait within (0, %s] and no backoff", w, reconnectInterval)
				}
			}
			if c.c.Load() != nil {
				t.Error("the client holds a session although every login was refused")
			}
			if !c.mu.TryLock() {
				t.Fatal("c.mu is still held after the reconnect gave up")
			}
			c.mu.Unlock()
		})
	}
}

func Test_safeCall_When_ReconnectWasRefusedPermanently_Then_LaterCallsSendNoLogin(t *testing.T) {
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

	runs := 0
	for range laterCalls {
		err := c.safeCall(func(*imapclient.Client) error {
			runs++
			return nil
		})
		checkStoredGiveUp(t, err, refusal, ClassPermanent)
	}

	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
	}
}

// Unknown class is final too: a credential refusal can use words that no rule matches.
func Test_safeCall_When_ReconnectGaveUpOnUnknownRefusal_Then_LaterCallsSendNoLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	dials := countDials(c)
	giveUp := giveUpOn(t, srv, c, "NO [UNAVAILABLE] Server busy", ClassUnknown)
	c.reviveDur = 0
	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")

	runs := 0
	for range laterCalls {
		err := c.safeCall(func(*imapclient.Client) error {
			runs++
			return nil
		})
		checkStoredGiveUp(t, err, giveUp, ClassUnknown)
	}

	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_WithinReviveInterval_Then_FailsFastWithoutDial(t *testing.T) {
	tests := []struct {
		name      string
		reply     string
		wantClass ErrClass
	}{
		{name: "transient give-up", reply: dovecotTempAuthFailure, wantClass: ClassTransient},
		{name: "throttled give-up", reply: "NO [ALERT] Too many simultaneous connections", wantClass: ClassThrottled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t)
			c := newClientWithFake(t, srv)
			rec := recordSleeps(t, srv)
			dials := countDials(c)
			giveUp := giveUpOn(t, srv, c, tt.reply, tt.wantClass)
			c.reviveDur = time.Hour
			tr := &recordingTracker{}
			c.SetProgressTracker(tr)
			dialsBefore, loginsBefore, waitsBefore := dials.Load(), srv.callCount("LOGIN"), len(rec.waits)
			stamp := c.lastReconnect

			runs := 0
			for range laterCalls {
				err := c.safeCall(func(*imapclient.Client) error {
					runs++
					return nil
				})
				checkStoredGiveUp(t, err, giveUp, tt.wantClass)
			}

			if runs != 0 {
				t.Errorf("fn ran %d times, want 0", runs)
			}
			if got := dials.Load(); got != dialsBefore {
				t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, got)
			}
			if got := srv.callCount("LOGIN"); got != loginsBefore {
				t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
			}
			if got := len(rec.waits); got != waitsBefore {
				t.Errorf("the calls waited %v, want no wait", rec.waits[waitsBefore:])
			}
			if len(tr.messages) != 0 {
				t.Errorf("tracker messages = %q, want none", tr.messages)
			}
			if !c.lastReconnect.Equal(stamp) {
				t.Error("a call that failed fast moved lastReconnect, so the cool-down would never end")
			}
			if !c.mu.TryLock() {
				t.Fatal("c.mu is still held after the calls failed fast")
			}
			c.mu.Unlock()
		})
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_ReviveIntervalPassed_Then_OneLoginRestoresTheSession(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	rec := recordSleeps(t, srv)
	dials := countDials(c)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	expireCooldown(c)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")

	runs := 0
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return nil
	})
	if err != nil {
		t.Fatalf("safeCall error = %v, want nil", err)
	}
	if runs != 1 {
		t.Errorf("fn ran %d times, want 1", runs)
	}
	if got := dials.Load() - dialsBefore; got != 1 {
		t.Errorf("the call dialed %d times, want 1", got)
	}
	if got := srv.callCount("LOGIN") - loginsBefore; got != 1 {
		t.Errorf("the call sent %d LOGINs, want 1", got)
	}
	if c.c.Load() == nil {
		t.Error("the client holds no session after the login")
	}
	if c.giveUpErr != nil {
		t.Errorf("giveUpErr = %v after the login, want nil", c.giveUpErr)
	}

	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	loginsBefore, waitsBefore := srv.callCount("LOGIN"), len(rec.waits)
	err = c.safeCall(func(*imapclient.Client) error { return io.EOF })
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%v) = %s, want %s", err, got, ClassTransient)
	}
	if got := srv.callCount("LOGIN") - loginsBefore; got != maxReconnectAttempts {
		t.Errorf("the next reconnect sent %d LOGINs, want the whole budget of %d", got, maxReconnectAttempts)
	}
	if got, want := rec.waits[waitsBefore:], transientBackoffs(); !slices.Equal(got, want) {
		t.Errorf("the next reconnect waited %v, want %v", got, want)
	}
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_ReviveProbeFails_Then_OneAttemptAndCooldownRestarts(t *testing.T) {
	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	rec := recordSleeps(t, srv)
	dials := countDials(c)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	expireCooldown(c)
	c.backoff = initialBackoff
	srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	tr := &recordingTracker{}
	c.SetProgressTracker(tr)
	loginsBefore, waitsBefore := srv.callCount("LOGIN"), len(rec.waits)

	runs := 0
	probeErr := c.safeCall(func(*imapclient.Client) error {
		runs++
		return nil
	})
	if got := classifyError(probeErr); got != ClassTransient {
		t.Errorf("classifyError(%v) = %s, want %s", probeErr, got, ClassTransient)
	}
	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := srv.callCount("LOGIN") - loginsBefore; got != 1 {
		t.Errorf("the probe sent %d LOGINs, want 1", got)
	}
	if got := rec.waits[waitsBefore:]; len(got) != 0 {
		t.Errorf("the probe waited %v, want no backoff", got)
	}
	if len(tr.messages) == 0 {
		t.Fatal("tracker got no message")
	}
	for _, m := range tr.messages {
		if strings.Contains(m, "1 attempts") {
			t.Errorf("tracker message %q counts one attempt in the plural", m)
		}
	}
	if last := tr.messages[len(tr.messages)-1]; !strings.Contains(last, "giving up") {
		t.Errorf("last tracker message = %q, want it to contain %q", last, "giving up")
	}

	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return nil
	})
	checkStoredGiveUp(t, err, probeErr, ClassTransient)
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial inside the new cool-down", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN inside the new cool-down", loginsBefore, got)
	}
	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
}

func Test_safeCall_When_ReviveProbeIsRefusedPermanently_Then_NoFurtherLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	dials := countDials(c)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	expireCooldown(c)
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Authentication failed."))
	loginsBefore := srv.callCount("LOGIN")

	runs := 0
	probeErr := c.safeCall(func(*imapclient.Client) error {
		runs++
		return nil
	})
	if got := classifyError(probeErr); got != ClassPermanent {
		t.Fatalf("classifyError(%v) = %s, want %s", probeErr, got, ClassPermanent)
	}
	if got := srv.callCount("LOGIN") - loginsBefore; got != 1 {
		t.Errorf("the probe sent %d LOGINs, want 1", got)
	}

	expireCooldown(c)
	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")
	for range laterCalls {
		err := c.safeCall(func(*imapclient.Client) error {
			runs++
			return nil
		})
		checkStoredGiveUp(t, err, probeErr, ClassPermanent)
	}

	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, got)
	}
}

func Test_safeCall_When_CancelledDuringReviveProbe_Then_ReturnsCanceledAndKeepsGiveUp(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	giveUp := giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	c.reviveDur = 0
	login := newStall(verbIs("LOGIN"))
	srv.addConnHandler(interceptHandler(srv, login.intercept))

	errc := make(chan error, 1)
	go func() { errc <- c.safeCall(func(*imapclient.Client) error { return nil }) }()
	awaitSignal(t, login.reached, "the probe LOGIN to reach the server")
	c.Cancel()

	if err := awaitError(t, errc, "safeCall"); !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want context.Canceled", err)
	}
	if !errors.Is(c.giveUpErr, giveUp) || errors.Is(c.giveUpErr, context.Canceled) {
		t.Errorf("giveUpErr = %v, want the earlier give-up %v", c.giveUpErr, giveUp)
	}
}

func Test_safeCall_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	c.reviveDur = 0

	runs := 0
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return io.EOF
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("safeCall error = %v, want io.EOF", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError(%v) = %s, want %s", err, got, ClassTransient)
	}
	if runs != 1 {
		t.Errorf("fn ran %d times, want 1", runs)
	}
	if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts+1; got != want {
		t.Errorf("LOGIN count = %d, want %d (initial, the refusals, one revive and no more)", got, want)
	}
}

func Test_safeCall_When_CancelledWithoutSession_Then_ReturnsCanceledWithoutDial(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	dials := countDials(c)
	giveUp := giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	c.reviveDur = 0
	c.Cancel()
	dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")

	runs := 0
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want context.Canceled", err)
	}
	if errors.Is(err, giveUp) {
		t.Errorf("safeCall error = %v, want the cancellation, not the stored give-up", err)
	}
	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := dials.Load(); got != dialsBefore {
		t.Errorf("dial count went from %d to %d, want no dial after Cancel", dialsBefore, got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN after Cancel", loginsBefore, got)
	}
}

func Test_reconnect_When_CancelledWhileWaitingForLock_Then_ReturnsCanceledNotTheGiveUp(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	refusal := loseSession(t, srv, c)
	dials := countDials(c)
	loginsBefore := srv.callCount("LOGIN")

	errc := make(chan error, 1)
	c.mu.Lock()
	go func() { errc <- c.reconnect() }()
	c.Cancel()
	c.mu.Unlock()

	err := awaitError(t, errc, "reconnect")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("reconnect error = %v, want context.Canceled", err)
	}
	if errors.Is(err, refusal) {
		t.Errorf("reconnect error = %v, want the cancellation, not the stored give-up", err)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("dial count = %d, want no dial after Cancel", got)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN after Cancel", loginsBefore, got)
	}
	if !errors.Is(c.giveUpErr, refusal) || errors.Is(c.giveUpErr, context.Canceled) {
		t.Errorf("giveUpErr = %v, want it unchanged at %v", c.giveUpErr, refusal)
	}
}

func Test_safeCall_When_CancelledDuringWaitBeforeReconnect_Then_ReturnsCanceledWithoutLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
	c.reviveDur = 0
	c.reconnectDur = reconnectInterval
	c.verbose = true
	waiting := make(chan struct{})
	var once sync.Once
	c.SetProgressWriter(&logCapture{fn: func(msg string) {
		if strings.Contains(msg, "Reconnecting in") {
			once.Do(func() { close(waiting) })
		}
	}})
	loginsBefore := srv.callCount("LOGIN")

	runs := 0
	errc := make(chan error, 1)
	go func() {
		errc <- c.safeCall(func(*imapclient.Client) error {
			runs++
			return nil
		})
	}()
	awaitSignal(t, waiting, "the call to wait before it reconnects")
	c.Cancel()

	if err := awaitError(t, errc, "safeCall"); !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want context.Canceled", err)
	}
	if runs != 0 {
		t.Errorf("fn ran %d times, want 0", runs)
	}
	if got := srv.callCount("LOGIN"); got != loginsBefore {
		t.Errorf("LOGIN count went from %d to %d, want no LOGIN after Cancel", loginsBefore, got)
	}
}

func Test_Client_When_EarlierReconnectGaveUp_Then_OperationsReconnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		handler      func(srv *fakeServer) func(net.Conn)
		run          func(t *testing.T, c *Client) error
		wantCommands map[string]int
		name         string
	}{
		{
			name:         "FetchMessageMap",
			handler:      func(srv *fakeServer) func(net.Conn) { return interceptHandler(srv, serveNormally) },
			wantCommands: map[string]int{"EXAMINE": 1},
			run: func(t *testing.T, c *Client) error {
				t.Helper()
				_, _, err := c.FetchMessageMap(t.Context(), "INBOX")
				return err
			},
		},
		{
			name:         "FetchMessageCopies",
			handler:      func(srv *fakeServer) func(net.Conn) { return interceptHandler(srv, serveNormally) },
			wantCommands: map[string]int{"EXAMINE": 1},
			run: func(t *testing.T, c *Client) error {
				t.Helper()
				_, err := c.FetchMessageCopies(t.Context(), "INBOX")
				return err
			},
		},
		{
			name: "StreamMessagesByUIDs",
			handler: func(srv *fakeServer) func(net.Conn) {
				return (&mailboxScript{uidMessages: 3}).handler(srv)
			},
			wantCommands: map[string]int{"UID FETCH": 1},
			run: func(t *testing.T, c *Client) error {
				t.Helper()
				delivered := make(map[uint32]int)
				err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, func(msg Message) error {
					delivered[msg.m.Uid]++
					return nil
				})
				for _, uid := range []uint32{1, 2, 3} {
					if delivered[uid] != 1 {
						t.Errorf("UID %d reached onMessage %d times, want 1", uid, delivered[uid])
					}
				}
				return err
			},
		},
		{
			name:         "ListMailboxes",
			handler:      listMailboxesHandler,
			wantCommands: map[string]int{"LIST": 1},
			run: func(t *testing.T, c *Client) error {
				t.Helper()
				infos, err := c.ListMailboxes(t.Context())
				if err == nil && len(infos) != 2 {
					t.Errorf("ListMailboxes returned %d folders, want 2", len(infos))
				}
				return err
			},
		},
		{
			name:         "CreateMailbox",
			handler:      func(srv *fakeServer) func(net.Conn) { return interceptHandler(srv, serveNormally) },
			wantCommands: map[string]int{"CREATE": 1},
			run: func(t *testing.T, c *Client) error {
				t.Helper()
				created, err := c.CreateMailbox(t.Context(), "Archive")
				if err == nil && !created {
					t.Error("CreateMailbox returned created=false, want true")
				}
				if err == nil && !c.hasMailbox("Archive") {
					t.Error("the mailbox cache does not hold Archive")
				}
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c := newClientWithFake(t, srv)
			_ = giveUpOn(t, srv, c, dovecotTempAuthFailure, ClassTransient)
			c.reviveDur = 0
			srv.addConnHandler(tt.handler(srv))

			if err := tt.run(t, c); err != nil {
				t.Fatalf("operation error = %v, want nil", err)
			}
			if got, want := srv.callCount("LOGIN"), 1+maxReconnectAttempts+1; got != want {
				t.Errorf("LOGIN count = %d, want %d (initial, the refusals, one revive)", got, want)
			}
			for verb, want := range tt.wantCommands {
				if got := srv.callCount(verb); got != want {
					t.Errorf("%s count = %d, want %d", verb, got, want)
				}
			}
		})
	}
}

// blockedSleep stands in for sleepCtx. It reports its first wait, then blocks
// until the context ends, as a real wait does.
type blockedSleep struct {
	reached chan struct{}
	wait    time.Duration
	calls   atomic.Int32
	once    sync.Once
}

// blockSleeps swaps the package-level sleepCtx var, so the caller must be
// sequential. The swap is undone when the test ends.
func blockSleeps(t *testing.T) *blockedSleep {
	t.Helper()

	b := &blockedSleep{reached: make(chan struct{})}
	orig := sleepCtx
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		b.calls.Add(1)
		b.once.Do(func() {
			b.wait = d
			close(b.reached)
		})
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { sleepCtx = orig })
	return b
}

// Sequential - swaps the package-level sleepCtx var.
func Test_safeCall_When_CancelledDuringReconnectWait_Then_ReturnsCanceledWithoutAnotherLogin(t *testing.T) {
	tests := []struct {
		name     string
		reply    string
		wantWait time.Duration
	}{
		{name: "throttle cool-down", reply: "NO [ALERT] Too many simultaneous connections", wantWait: throttledBackoff},
		{name: "transient backoff", reply: dovecotTempAuthFailure, wantWait: initialBackoff},
		{name: "unknown backoff", reply: "NO [UNAVAILABLE] Server busy", wantWait: initialBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t)
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			srv.addConnHandler(connHandlerWithLoginReply(srv, tt.reply))
			// A LOGIN that must never happen: it would succeed.
			srv.addConnHandler(connHandlerWithLoginReply(srv, "OK LOGIN completed"))
			c := newClientWithFake(t, srv)
			sleep := blockSleeps(t)

			runs := 0
			errc := make(chan error, 1)
			go func() {
				errc <- c.safeCall(func(*imapclient.Client) error {
					runs++
					return io.EOF
				})
			}()
			awaitSignal(t, sleep.reached, "the reconnect to wait before its next attempt")
			c.Cancel()

			err := awaitError(t, errc, "safeCall")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("safeCall error = %v, want context.Canceled", err)
			}
			if runs != 1 {
				t.Errorf("fn ran %d times, want 1 (no session was restored)", runs)
			}
			if sleep.wait != tt.wantWait {
				t.Errorf("wait = %s, want %s", sleep.wait, tt.wantWait)
			}
			if got := sleep.calls.Load(); got != 1 {
				t.Errorf("sleepCtx ran %d times, want 1", got)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2 (the initial login and the refused attempt)", got)
			}
			if !c.mu.TryLock() {
				t.Fatal("c.mu is still held after the cancelled reconnect returned")
			}
			c.mu.Unlock()
		})
	}
}

// The cancel lands between a successful login and the first use of the session.
type cancelOnReconnect struct{ c *Client }

func (r cancelOnReconnect) UpdateMessage(msg string) {
	if strings.Contains(msg, "Reconnected successfully") {
		r.c.Cancel()
	}
}
func (cancelOnReconnect) UpdateTotal(int64) {}
func (cancelOnReconnect) Increment(int64)   {}
func (cancelOnReconnect) MarkAsErrored()    {}

func Test_safeCall_When_CancelledRightAfterReconnect_Then_ReturnsCanceledWithoutRunningFn(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.SetProgressTracker(cancelOnReconnect{c: c})

	runs := 0
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return io.EOF
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, want context.Canceled", err)
	}
	if runs != 1 {
		t.Errorf("fn ran %d times, want 1 (the first attempt only)", runs)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the reconnect did log in)", got)
	}
	if !c.mu.TryLock() {
		t.Fatal("c.mu is still held after safeCall returned")
	}
	c.mu.Unlock()
}

func Test_reconnect_When_ClientIsCancelled_Then_ReturnsCanceledWithoutDial(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	var dials atomic.Int32
	dial := c.dialFn
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, addr)
	}
	c.Cancel()

	err := c.reconnect()

	if !errors.Is(err, context.Canceled) {
		t.Errorf("reconnect error = %v, want context.Canceled", err)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("dial count = %d, want 0", got)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (the initial login only)", got)
	}
	if !c.mu.TryLock() {
		t.Fatal("c.mu is still held after reconnect returned")
	}
	c.mu.Unlock()
}

// The session disappears between a successful login and the check of the session.
type dropSessionOnReconnect struct{ c *Client }

func (r dropSessionOnReconnect) UpdateMessage(msg string) {
	if !strings.Contains(msg, "Reconnected successfully") {
		return
	}
	// Terminate keeps the reader goroutine of the dropped session from leaking.
	if cli := r.c.c.Swap(nil); cli != nil {
		_ = cli.Terminate()
	}
}
func (dropSessionOnReconnect) UpdateTotal(int64) {}
func (dropSessionOnReconnect) Increment(int64)   {}
func (dropSessionOnReconnect) MarkAsErrored()    {}

func Test_safeCall_When_SessionMissingAfterReconnect_Then_ReturnsErrorWithoutRunningFn(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.SetProgressTracker(dropSessionOnReconnect{c: c})

	runs := 0
	err := c.safeCall(func(*imapclient.Client) error {
		runs++
		return io.EOF
	})

	if err == nil || !strings.Contains(err.Error(), "imap client not connected after reconnect") {
		t.Errorf("safeCall error = %v, want text %q", err, "imap client not connected after reconnect")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("safeCall error = %v, must not be context.Canceled", err)
	}
	if runs != 1 {
		t.Errorf("fn ran %d times, want 1 (the first attempt only)", runs)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the reconnect did log in)", got)
	}
	if !c.mu.TryLock() {
		t.Fatal("c.mu is still held after safeCall returned")
	}
	c.mu.Unlock()
}

func Test_folderLock_When_NamesDifferOnlyInInboxCase_Then_ShareOneMutex(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}}})

	if c.folderLock("inbox/X") != c.folderLock("INBOX/X") {
		t.Error("folderLock gave two mutexes for names that the cache treats as one mailbox")
	}
	if c.folderLock("Archive") == c.folderLock("archive") {
		t.Error("folderLock gave one mutex for two names that differ in letter case outside INBOX")
	}
}
