// Package client wraps go-imap with reconnect logic and higher-level helpers.
package client

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"golang.org/x/time/rate"
)

const (
	// mailboxChanBuffer is the buffer size for mailbox listing channels.
	mailboxChanBuffer = 10
	// messageChanBuffer is the buffer size for message fetching channels.
	messageChanBuffer = 10
	// initialBackoff is the initial delay before reconnect attempts.
	initialBackoff = 2 * time.Second
	// reconnectInterval is the minimum time between reconnection attempts.
	reconnectInterval = 10 * time.Second
	// maxReconnectAttempts is the maximum number of reconnection retries.
	maxReconnectAttempts = 5
	// throttledBackoff is the wait imposed after the server signals a rate
	// limit. Long enough that an aggressive caller cools down rather than
	// drilling into the same throttle.
	throttledBackoff = 5 * time.Minute
	// defaultDialTimeout is the maximum time spent on a single TCP/TLS dial.
	defaultDialTimeout = 30 * time.Second
	// uidFetchBatchSize limits UID FETCH requests to avoid "Too long argument" errors.
	uidFetchBatchSize  = 500
	tcpKeepAlivePeriod = 30 * time.Second
	// logoutTimeout bounds the wait for the LOGOUT reply. LOGOUT is the last
	// command of a session, so closing the socket early loses nothing.
	logoutTimeout = 2 * time.Second
	// idleTimeout bounds a command's wait on a silent server. Generous: a server
	// may think for minutes after a large APPEND, and moving bytes never trip it.
	idleTimeout = 5 * time.Minute
)

var errLogoutTimeout = errors.New("imap logout: no reply from server")

// ProgressWriter is a minimal interface for progress tracking and logging.
// This avoids circular dependency with the progress package.
type ProgressWriter interface {
	Log(msg string, a ...any)
}

// ProgressTracker is a minimal interface for updating tracker messages.
type ProgressTracker interface {
	UpdateMessage(msg string)
	UpdateTotal(total int64)
	Increment(value int64)
	MarkAsErrored()
}

// Options carries the optional knobs for New. The zero value dials plaintext
// TCP: production callers must set UseTLS. Limiters shared across Clients make
// the byte budget global, not per-connection.
type Options struct {
	TLSConfig    *tls.Config
	ReadLimiter  *rate.Limiter
	WriteLimiter *rate.Limiter
	Auth         string
	DialTimeout  time.Duration
	UseTLS       bool
	Verbose      bool
}

// dialFunc captures how a fresh connection is produced. It is overridable in
// tests; production wiring is set up in New.
type dialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Client wraps a go-imap client with reconnect/retry logic. The client sits in
// an atomic.Pointer so Reconnect can swap it while the Cancel callback from
// withCancel runs.
type Client struct {
	lastReconnect  time.Time
	tlsConfig      *tls.Config
	readLimiter    *rate.Limiter
	writeLimiter   *rate.Limiter
	dialFn         dialFunc
	folderLocks    map[string]*sync.Mutex
	cancelCh       chan struct{}
	c              atomic.Pointer[imapclient.Client]
	idle           atomic.Pointer[idleConn]
	selectedFolder atomic.Pointer[string]
	pw             atomic.Pointer[progressWriterRef]
	tracker        atomic.Pointer[progressTrackerRef]
	username       string
	prefix         string
	password       string
	delimiter      string
	serverAddr     string
	auth           string
	mailboxCache   mailboxCache
	connGen        atomic.Uint64
	backoff        time.Duration
	reconnectDur   time.Duration
	dialTimeout    time.Duration
	logoutDur      time.Duration
	idleDur        time.Duration
	mailboxCacheMu sync.RWMutex
	mu             sync.Mutex
	folderLocksMu  sync.Mutex
	cancelled      atomic.Bool
	useTLS         bool
	verbose        bool
}

// progressWriterRef and progressTrackerRef carry an interface value through
// atomic.Pointer. Storing an interface directly would require atomic.Value
// (which loses compile-time typing) and the wrapper costs only a tiny heap
// allocation per Set call, which happens at most a handful of times per run.
type progressWriterRef struct{ pw ProgressWriter }
type progressTrackerRef struct{ t ProgressTracker }

// folderLock returns the mutex guarding creation of the given folder path.
// Locks are per-Client; concurrent CreateMailbox calls on the same instance
// for nested paths can race on parent creation without this.
func (c *Client) folderLock(path string) *sync.Mutex {
	c.folderLocksMu.Lock()
	defer c.folderLocksMu.Unlock()

	if lock, ok := c.folderLocks[path]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	c.folderLocks[path] = lock
	return lock
}

// newDialer builds the *net.Dialer used by Client. 30 s idle before the first
// keepalive probe; dead-peer detection then falls within the OS-default
// keepalive envelope (typically several minutes on macOS/Linux) — fast enough
// that a Wi-Fi flap doesn't strand the sync indefinitely, slow enough to not
// break legitimate long fetches.
func newDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: tcpKeepAlivePeriod}
}

// New logs in to the IMAP server. ctx bounds the dial, greeting, login and first
// mailbox list. Afterwards reconnects stop on Cancel, so callers need not keep
// ctx alive.
func New(ctx context.Context, addr, username, password string, opts Options) (*Client, error) {
	timeout := opts.DialTimeout
	if timeout == 0 {
		timeout = defaultDialTimeout
	}

	c := &Client{
		serverAddr:   addr,
		useTLS:       opts.UseTLS,
		auth:         opts.Auth,
		tlsConfig:    opts.TLSConfig,
		username:     username,
		password:     password,
		backoff:      initialBackoff,
		reconnectDur: reconnectInterval,
		dialTimeout:  timeout,
		logoutDur:    logoutTimeout,
		idleDur:      idleTimeout,
		verbose:      opts.Verbose,
		folderLocks:  make(map[string]*sync.Mutex),
		readLimiter:  opts.ReadLimiter,
		writeLimiter: opts.WriteLimiter,
		cancelCh:     make(chan struct{}),
	}

	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		nd := newDialer(c.dialTimeout)
		var (
			conn net.Conn
			err  error
		)
		if c.useTLS {
			td := &tls.Dialer{NetDialer: nd, Config: c.tlsConfig}
			conn, err = td.DialContext(ctx, "tcp", addr)
		} else {
			conn, err = nd.DialContext(ctx, "tcp", addr)
		}
		if err != nil {
			return nil, err
		}
		return conn, nil
	}

	if err := c.connectAndLogin(ctx); err != nil {
		return nil, err
	}

	// One LIST "" "*" populates both the existence cache and the
	// hierarchy delimiter — the older refreshDelimiter call is gone.
	if err := c.loadMailboxCache(ctx); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("failed to load mailbox list: %w", err)
	}
	c.delimiter = c.mailboxCache.delimiter

	return c, nil
}

// selectIfNeeded selects folder on cli unless that folder is already selected
// on the current connection. Each successful reconnect bumps connGen and
// clears selectedFolder via Cancel/reconnect, so a stale state never causes
// us to skip a needed Select.
func (c *Client) selectIfNeeded(cli *imapclient.Client, folder string) (*imap.MailboxStatus, error) {
	cur := c.selectedFolder.Load()
	if cur != nil && *cur == folder {
		// Server-side state is unchanged — re-issuing Select would be a
		// wasted round-trip. Caller doesn't get the MailboxStatus back, but
		// nobody downstream needs it on the cached path.
		return nil, nil
	}
	mbox, err := cli.Select(folder, true)
	if err != nil {
		return nil, err
	}
	f := folder
	c.selectedFolder.Store(&f)
	return mbox, nil
}

// SetPrefix configures the log prefix used in progress messages.
func (c *Client) SetPrefix(p string) { c.prefix = p }

// SetProgressWriter sets the progress writer for logging. Safe to call from
// any goroutine, including while another goroutine is using the writer.
func (c *Client) SetProgressWriter(pw ProgressWriter) {
	if pw == nil {
		c.pw.Store(nil)
		return
	}
	c.pw.Store(&progressWriterRef{pw: pw})
}

// SetProgressTracker sets an optional progress tracker for updating progress.
// Safe for concurrent use; see SetProgressWriter.
func (c *Client) SetProgressTracker(t ProgressTracker) {
	if t == nil {
		c.tracker.Store(nil)
		return
	}
	c.tracker.Store(&progressTrackerRef{t: t})
}

// progressWriter returns the currently configured ProgressWriter, or nil.
func (c *Client) progressWriter() ProgressWriter {
	if r := c.pw.Load(); r != nil {
		return r.pw
	}
	return nil
}

// progressTracker returns the currently configured ProgressTracker, or nil.
func (c *Client) progressTracker() ProgressTracker {
	if r := c.tracker.Load(); r != nil {
		return r.t
	}
	return nil
}

// GetDelimiter returns the cached hierarchy delimiter for this server.
func (c *Client) GetDelimiter() string { return c.delimiter }

// Logout ends the IMAP session. It closes the socket and returns an error when
// the server does not answer LOGOUT within logoutTimeout.
func (c *Client) Logout() error {
	cli := c.c.Load()
	if cli == nil {
		return nil
	}
	return c.logout(cli)
}

// logout bypasses safeCall so it never reconnects. The timer decides the error
// because go-imap reports a closed socket as success. Terminate always runs
// because go-imap closes only on BYE.
func (c *Client) logout(cli *imapclient.Client) error {
	defer func() { _ = cli.Terminate() }()

	if c.logoutDur <= 0 {
		return cli.Logout()
	}
	t := time.AfterFunc(c.logoutDur, func() { _ = cli.Terminate() })
	err := cli.Logout()
	if !t.Stop() {
		return fmt.Errorf("%w within %s", errLogoutTimeout, c.logoutDur)
	}
	return err
}

// Cancel marks the client as canceled and terminates the underlying connection.
// It is safe to call multiple times.
func (c *Client) Cancel() {
	if c.cancelled.CompareAndSwap(false, true) {
		close(c.cancelCh)
	}
	if cli := c.c.Load(); cli != nil {
		_ = cli.Terminate()
	}
}

func (c *Client) isCancelled() bool { return c.cancelled.Load() }

// withCancel terminates the connection if ctx is cancelled while the call runs.
// After the returned func is called, a later cancellation leaves the client
// untouched. ctx must be non-nil.
func (c *Client) withCancel(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, c.Cancel)
	return func() { stop() }
}

// internalContext returns a context that is cancelled when the client itself
// is cancelled. Used for reconnect dialing where the original caller context
// may already be gone.
func (c *Client) internalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.cancelCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// log routes a formatted message to the tracker (preferred) or progress writer.
func (c *Client) log(format string, args ...any) {
	// Tracker drives the live UX, so it updates regardless of verbose.
	// The verbose check below only gates the secondary log-writer fallback.
	if t := c.progressTracker(); t != nil {
		t.UpdateMessage(fmt.Sprintf(format, args...))
		return
	}
	if !c.verbose {
		return
	}
	if pw := c.progressWriter(); pw != nil {
		pw.Log(format, args...)
	}
}

// realSleepCtx sleeps for d, returning early with ctx.Err() if the context is
// cancelled. d <= 0 returns nil immediately.
func realSleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// indirection so tests can observe / fast-forward the reconnect backoff without sleeping.
var sleepCtx = realSleepCtx

// connectAndLogin establishes a new IMAP connection and authenticates the user.
func (c *Client) connectAndLogin(ctx context.Context) error {
	stop := c.withCancel(ctx)
	defer stop()

	conn, err := c.dialFn(ctx, c.serverAddr)
	if err != nil {
		return err
	}
	conn, w := c.guardConn(conn)

	cli, err := c.greet(ctx, conn)
	if err != nil {
		return err
	}
	c.muteErrorLog(cli)

	// Stored before cli so a reader that sees the new cli also sees its watchdog.
	c.idle.Store(w)

	// Store cli before Login so Cancel() can Terminate the underlying
	// connection mid-LOGIN — without this, a half-open TCP after a Wi-Fi
	// flap leaves Login blocked until the kernel times the socket out.
	c.c.Store(cli)

	// Cancel sets cancelled before loading c.c; storing first and checking second
	// guarantees one side sees the other, so an early Cancel cannot leave Login blocked.
	if cerr := c.cancelErr(ctx); cerr != nil {
		_ = cli.Terminate()
		c.c.Store(nil)
		return cerr
	}

	w.begin()
	switch strings.ToLower(c.auth) {
	case "cram-md5":
		err = cli.Authenticate(&cramMD5Auth{
			username: c.username,
			password: c.password,
		})
	default:
		err = cli.Login(c.username, c.password)
	}
	w.end()
	if err = w.idleErr(err); err != nil {
		return c.abortSession(ctx, cli, err)
	}

	return nil
}

// guardConn keeps the watchdog innermost, so rate-limit token waits never run
// inside it. The watchdog is nil when idleDur is zero.
func (c *Client) guardConn(conn net.Conn) (net.Conn, *idleConn) {
	var w *idleConn
	if c.idleDur > 0 {
		w = newIdleConn(conn, c.idleDur)
		conn = w
	}
	if c.readLimiter != nil || c.writeLimiter != nil {
		conn = ratelimit.New(conn, c.readLimiter, c.writeLimiter)
	}
	return conn, w
}

// greet replaces a bare imapclient.New, which waits forever when the server
// accepts the connection but never sends a greeting.
func (c *Client) greet(ctx context.Context, conn net.Conn) (*imapclient.Client, error) {
	var timer *time.Timer
	if c.dialTimeout > 0 {
		// Closing from a timer instead of conn.SetDeadline: go-imap's implicit
		// CAPABILITY probe clears the deadline.
		timer = time.AfterFunc(c.dialTimeout, func() { _ = conn.Close() })
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	cli, err := imapclient.New(conn)

	// go-imap ignores the error of its CAPABILITY probe, so err alone cannot
	// tell a closed connection from a good one.
	ctxFired := !stop()
	timeFired := timer != nil && !timer.Stop()
	if err == nil && !ctxFired && !timeFired {
		return cli, nil
	}

	_ = conn.Close()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case timeFired:
		return nil, fmt.Errorf("imap greeting: none within %s: %w", c.dialTimeout, os.ErrDeadlineExceeded)
	default:
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
}

// muteErrorLog keeps go-imap's read-error lines, which every Terminate causes,
// off stderr unless verbose. SetOutput locks the logger, so the running reader
// goroutine does not race.
func (c *Client) muteErrorLog(cli *imapclient.Client) {
	if c.verbose {
		return
	}
	if l, ok := cli.ErrorLog.(*log.Logger); ok {
		l.SetOutput(io.Discard)
	}
}

func (c *Client) cancelErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.isCancelled() {
		return context.Canceled
	}
	return nil
}

// abortSession prefers the cancellation error over authErr, so Ctrl-C during
// LOGIN exits as Cancelled and not as an authentication failure.
func (c *Client) abortSession(ctx context.Context, cli *imapclient.Client, authErr error) error {
	_ = c.logout(cli)
	c.c.Store(nil)
	if cerr := c.cancelErr(ctx); cerr != nil {
		return cerr
	}
	return authErr
}

// reconnect tears down and rebuilds the underlying IMAP session with backoff.
// It honours error classification: permanent errors abort immediately, server
// throttling triggers a long cool-down between attempts.
func (c *Client) reconnect() error {
	if c.isCancelled() {
		return context.Canceled
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := c.internalContext()
	defer cancel()

	now := time.Now()
	sinceLast := now.Sub(c.lastReconnect)
	if sinceLast < c.reconnectDur {
		wait := c.reconnectDur - sinceLast
		c.log("[%s] 🔄 Reconnecting in %s...", c.prefix, wait)
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}

	if cli := c.c.Load(); cli != nil {
		_ = c.logout(cli)
		c.c.Store(nil)
	}
	// Anything held about the previous connection's state — selected folder,
	// in-flight literals — is gone with that socket. Bump the generation so
	// later code knows to re-Select before issuing fetches.
	c.selectedFolder.Store(nil)
	c.connGen.Add(1)

	var lastErr error
	delay := c.backoff

	for i := 1; i <= maxReconnectAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.log("[%s] 🔄 Reconnect attempt %d...", c.prefix, i)
		err := c.connectAndLogin(ctx)
		if err == nil {
			c.log("[%s] 🔄 Reconnected successfully", c.prefix)
			c.lastReconnect = time.Now()
			c.backoff = initialBackoff
			return nil
		}
		lastErr = err

		switch classifyError(err) {
		case ClassPermanent:
			// Auth fails won't get better with retries; bail out fast.
			c.log("[%s] 🔄 Permanent error, giving up: %v", c.prefix, err)
			c.lastReconnect = time.Now()
			return err
		case ClassThrottled:
			c.log("[%s] 🔄 Server throttled, backing off %s", c.prefix, throttledBackoff)
			if serr := sleepCtx(ctx, throttledBackoff); serr != nil {
				return serr
			}
			continue
		case ClassTransient, ClassUnknown:
			c.log("[%s] 🔄 Failed: %v, retrying in %s", c.prefix, err, delay)
			if serr := sleepCtx(ctx, delay); serr != nil {
				return serr
			}
			delay *= 2
		}
	}

	c.lastReconnect = time.Now()
	return fmt.Errorf("[%s] failed to reconnect after retries: %w", c.prefix, lastErr)
}

// safeCall runs fn with the current IMAP client. If fn returns a transient
// error, safeCall reconnects and retries fn once with the fresh client.
// Permanent and throttled errors are surfaced to the caller as-is.
//
// The closure must be re-runnable: any state that fn accumulates on the first
// attempt (slices, maps, counters) has to be reset at the top of the closure.
func (c *Client) safeCall(fn func(cli *imapclient.Client) error) error {
	if c.isCancelled() {
		return context.Canceled
	}
	cli := c.c.Load()
	if cli == nil {
		return errors.New("imap client not connected")
	}

	err := c.call(cli, fn)
	if err == nil {
		return nil
	}
	if c.isCancelled() {
		return context.Canceled
	}
	if !isRetryable(err) {
		return err
	}

	c.noteIdleTrip(err)
	if rerr := c.reconnect(); rerr != nil {
		return rerr
	}
	if c.isCancelled() {
		return context.Canceled
	}
	cli = c.c.Load()
	if cli == nil {
		return errors.New("imap client not connected after reconnect")
	}
	return c.call(cli, fn)
}

// noteIdleTrip logs why a reconnect follows a watchdog trip; the reconnect
// messages alone do not say. Unlike log it ignores verbose, because a trip is
// rare and worth a line.
func (c *Client) noteIdleTrip(err error) {
	if !errors.Is(err, errIdleTimeout) {
		return
	}
	msg := fmt.Sprintf("[%s] No data from server for %s, reconnecting", c.prefix, c.idleDur)
	if pw := c.progressWriter(); pw != nil {
		pw.Log("%s", msg)
		return
	}
	if t := c.progressTracker(); t != nil {
		t.UpdateMessage(msg)
	}
}

// call runs one attempt of fn inside a watchdog span. It loads the watchdog on
// every attempt so a retry after a reconnect uses the fresh session's.
func (c *Client) call(cli *imapclient.Client, fn func(*imapclient.Client) error) error {
	w := c.idle.Load()
	w.begin()
	err := fn(cli)
	w.end()
	return w.idleErr(err)
}

// cramMD5Auth implements the Auth interface for CRAM-MD5 authentication.
type cramMD5Auth struct {
	username string
	password string
}

// Start returns the CRAM-MD5 mechanism
func (a *cramMD5Auth) Start() (mech string, ir []byte, err error) {
	return "CRAM-MD5", nil, nil
}

// Next returns the response to the CRAM-MD5 challenge.
func (a *cramMD5Auth) Next(challenge []byte) (response []byte, err error) {
	if len(challenge) == 0 {
		return nil, errors.New("empty challenge received")
	}

	mac := hmac.New(md5.New, []byte(a.password))
	mac.Write(challenge)
	digest := hex.EncodeToString(mac.Sum(nil))

	respStr := a.username + " " + digest
	return []byte(respStr), nil
}
