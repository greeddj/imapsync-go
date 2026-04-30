// Package client wraps go-imap with reconnect logic and higher-level helpers.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
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
	uidFetchBatchSize = 500
)

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

// Options carries the optional knobs for New. Zero-value is fine for plain
// TLS connections without throttling.
//
// ReadLimiter and WriteLimiter, when non-nil, are typically shared across
// every Client that talks to the same account so that the byte budget is a
// global cap, not a per-connection cap.
type Options struct {
	TLSConfig    *tls.Config
	ReadLimiter  *rate.Limiter
	WriteLimiter *rate.Limiter
	DialTimeout  time.Duration
	UseTLS       bool
	Verbose      bool
}

// dialFunc captures how a fresh connection is produced. It is overridable in
// tests; production wiring is set up in New.
type dialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Client wraps a go-imap client with reconnect/retry logic.
//
// The underlying *imapclient.Client is held in an atomic.Pointer so that
// Reconnect can swap it without racing with the watcher goroutine spawned by
// withCancel. All IMAP operations go through safeCall, which receives the
// current client as an argument; on a transient error it transparently
// reconnects and retries the closure once with the fresh client.
type Client struct {
	lastReconnect  time.Time
	tlsConfig      *tls.Config
	readLimiter    *rate.Limiter
	writeLimiter   *rate.Limiter
	dialFn         dialFunc
	folderLocks    map[string]*sync.Mutex
	cancelCh       chan struct{}
	c              atomic.Pointer[imapclient.Client]
	selectedFolder atomic.Pointer[string]
	pw             atomic.Pointer[progressWriterRef]
	tracker        atomic.Pointer[progressTrackerRef]
	delimiter      string
	prefix         string
	password       string
	username       string
	serverAddr     string
	mailboxCache   mailboxCache
	dialTimeout    time.Duration
	reconnectDur   time.Duration
	backoff        time.Duration
	connGen        atomic.Uint64
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

// New establishes a connection and logs into the IMAP server.
//
// ctx is used only for the initial dial and login; once Client is returned,
// reconnects are driven by the internal cancellation channel (closed by
// Cancel) so that long-running consumers do not need to keep the original
// context alive.
func New(ctx context.Context, addr, username, password string, opts Options) (*Client, error) {
	timeout := opts.DialTimeout
	if timeout == 0 {
		timeout = defaultDialTimeout
	}

	c := &Client{
		serverAddr:   addr,
		useTLS:       opts.UseTLS,
		tlsConfig:    opts.TLSConfig,
		username:     username,
		password:     password,
		backoff:      initialBackoff,
		reconnectDur: reconnectInterval,
		dialTimeout:  timeout,
		verbose:      opts.Verbose,
		folderLocks:  make(map[string]*sync.Mutex),
		readLimiter:  opts.ReadLimiter,
		writeLimiter: opts.WriteLimiter,
		cancelCh:     make(chan struct{}),
	}

	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		nd := &net.Dialer{Timeout: c.dialTimeout}
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
		if c.readLimiter != nil || c.writeLimiter != nil {
			conn = ratelimit.New(conn, c.readLimiter, c.writeLimiter)
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

// Logout terminates the IMAP session.
func (c *Client) Logout() error {
	cli := c.c.Load()
	if cli == nil {
		return nil
	}
	return cli.Logout()
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

// withCancel bridges context.Context to the underlying connection. When the
// context is canceled, the connection is Terminate()d so blocked IMAP calls
// return immediately. The returned function must be called to stop the watcher.
//
// ctx must be non-nil; passing nil is a programmer error and will panic on
// the first ctx.Done() — preferred over silently substituting Background().
func (c *Client) withCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Cancel()
		case <-done:
		}
	}()
	return func() { close(done) }
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

// sleepCtx sleeps for d, returning early with ctx.Err() if the context is
// cancelled. d <= 0 returns nil immediately.
func sleepCtx(ctx context.Context, d time.Duration) error {
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

// connectAndLogin establishes a new IMAP connection and authenticates the user.
func (c *Client) connectAndLogin(ctx context.Context) error {
	conn, err := c.dialFn(ctx, c.serverAddr)
	if err != nil {
		return err
	}

	cli, err := imapclient.New(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}

	if err := cli.Login(c.username, c.password); err != nil {
		_ = cli.Logout()
		return err
	}

	c.c.Store(cli)
	return nil
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
		_ = cli.Logout()
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

	err := fn(cli)
	if err == nil {
		return nil
	}
	if c.isCancelled() {
		return context.Canceled
	}
	if !isRetryable(err) {
		return err
	}

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
	return fn(cli)
}
