// Package client wraps go-imap with reconnect logic and higher-level helpers.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
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
	// progressUpdateInterval defines how often to update progress during batch operations.
	progressUpdateInterval = 10
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

// Client wraps a go-imap client with reconnect/retry logic.
//
// The underlying *imapclient.Client is held in an atomic.Pointer so that
// Reconnect can swap it without racing with the watcher goroutine spawned by
// withCancel. All IMAP operations go through safeCall, which receives the
// current client as an argument; on a connection error it transparently
// reconnects and retries the closure once with the fresh client.
type Client struct {
	lastReconnect time.Time
	pw            ProgressWriter
	tracker       ProgressTracker
	tlsConfig     *tls.Config
	dialFn        func(addr string) (net.Conn, error)
	folderLocks   map[string]*sync.Mutex
	c             atomic.Pointer[imapclient.Client]
	prefix        string
	delimiter     string
	password      string
	username      string
	serverAddr    string
	backoff       time.Duration
	reconnectDur  time.Duration
	mu            sync.Mutex
	folderLocksMu sync.Mutex
	cancelled     atomic.Bool
	useTLS        bool
	verbose       bool
}

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
func New(addr, username, password string, _ int, verbose, useTLS bool, tlsConfig *tls.Config) (*Client, error) {
	c := &Client{
		serverAddr:   addr,
		useTLS:       useTLS,
		tlsConfig:    tlsConfig,
		username:     username,
		password:     password,
		backoff:      initialBackoff,
		reconnectDur: reconnectInterval,
		verbose:      verbose,
		folderLocks:  make(map[string]*sync.Mutex),
	}

	c.dialFn = func(addr string) (net.Conn, error) {
		if useTLS {
			return tls.Dial("tcp", addr, tlsConfig)
		}
		return net.Dial("tcp", addr)
	}

	if err := c.connectAndLogin(); err != nil {
		return nil, err
	}

	delimiter, err := c.refreshDelimiter()
	if err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("failed to get delimiter: %w", err)
	}
	c.delimiter = delimiter

	return c, nil
}

// SetPrefix configures the log prefix used in progress messages.
func (c *Client) SetPrefix(p string) { c.prefix = p }

// SetProgressWriter sets the progress writer for logging.
func (c *Client) SetProgressWriter(pw ProgressWriter) { c.pw = pw }

// SetProgressTracker sets an optional progress tracker for updating progress.
func (c *Client) SetProgressTracker(t ProgressTracker) { c.tracker = t }

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
func (c *Client) Cancel() {
	c.cancelled.Store(true)
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

// log routes a formatted message to the tracker (preferred) or progress writer.
func (c *Client) log(format string, args ...any) {
	if c.tracker != nil {
		c.tracker.UpdateMessage(fmt.Sprintf(format, args...))
	} else if c.verbose && c.pw != nil {
		c.pw.Log(format, args...)
	}
}

// connectAndLogin establishes a new IMAP connection and authenticates the user.
func (c *Client) connectAndLogin() error {
	conn, err := c.dialFn(c.serverAddr)
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
func (c *Client) reconnect() error {
	if c.isCancelled() {
		return context.Canceled
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	sinceLast := now.Sub(c.lastReconnect)
	if sinceLast < c.reconnectDur {
		if c.isCancelled() {
			return context.Canceled
		}
		wait := c.reconnectDur - sinceLast
		c.log("[%s] 🔄 Reconnecting in %s...", c.prefix, wait)
		time.Sleep(wait)
	}

	if cli := c.c.Load(); cli != nil {
		_ = cli.Logout()
		c.c.Store(nil)
	}

	var err error
	delay := c.backoff

	for i := 1; i <= maxReconnectAttempts; i++ {
		if c.isCancelled() {
			return context.Canceled
		}
		c.log("[%s] 🔄 Reconnect attempt %d...", c.prefix, i)
		err = c.connectAndLogin()
		if err == nil {
			c.log("[%s] 🔄 Reconnected successfully", c.prefix)
			c.lastReconnect = time.Now()
			c.backoff = initialBackoff
			return nil
		}

		if c.isCancelled() {
			return context.Canceled
		}
		c.log("[%s] 🔄 Failed: %v, retrying in %s", c.prefix, err, delay)
		time.Sleep(delay)
		delay *= 2
	}

	c.lastReconnect = time.Now()
	return fmt.Errorf("[%s] failed to reconnect after retries: %w", c.prefix, err)
}

// safeCall runs fn with the current IMAP client. If fn returns a connection
// error, safeCall reconnects and retries fn once with the fresh client.
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
	if !isConnError(err) {
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

// isConnError determines if an error is connection-related and warrants a reconnect attempt.
func isConnError(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.As(err, &netErr)
}

// refreshDelimiter retrieves the hierarchy delimiter used by the IMAP server.
func (c *Client) refreshDelimiter() (string, error) {
	var delimiter string
	err := c.safeCall(func(cli *imapclient.Client) error {
		mailboxes := make(chan *imap.MailboxInfo, 1)
		done := make(chan error, 1)
		go func() { done <- cli.List("", "", mailboxes) }()

		delimiter = "/"
		for mbox := range mailboxes {
			if mbox.Delimiter != "" {
				delimiter = mbox.Delimiter
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] list delimiter: %w", c.prefix, err)
		}
		return nil
	})
	return delimiter, err
}
