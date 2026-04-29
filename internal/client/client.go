// Package client wraps go-imap with reconnect logic and higher-level helpers.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/greeddj/imapsync-go/internal/progress"
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

// Global lock map for folder creation to prevent race conditions across goroutines
// that share the destination Client semantics (each goroutine has its own *Client,
// but they may concurrently create the same folder path).
var (
	folderLocksMu sync.Mutex
	folderLocks   = make(map[string]*sync.Mutex)
)

// getFolderLock returns a mutex for a specific folder path.
func getFolderLock(path string) *sync.Mutex {
	folderLocksMu.Lock()
	defer folderLocksMu.Unlock()

	if lock, exists := folderLocks[path]; exists {
		return lock
	}

	lock := &sync.Mutex{}
	folderLocks[path] = lock
	return lock
}

// ProgressWriter is a minimal interface for progress tracking and logging.
// This avoids circular dependency with the progress package.
type ProgressWriter interface {
	Log(msg string, a ...interface{})
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
	serverAddr    string
	username      string
	password      string
	prefix        string
	delimiter     string
	pw            ProgressWriter
	tracker       ProgressTracker
	c             atomic.Pointer[imapclient.Client]
	dialFn        func(addr string) (net.Conn, error)
	tlsConfig     *tls.Config
	mu            sync.Mutex
	backoff       time.Duration
	reconnectDur  time.Duration
	cancelled     atomic.Bool
	useTLS        bool
	verbose       bool
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

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// withCancel bridges context.Context to the underlying connection. When the
// context is canceled, the connection is Terminate()d so blocked IMAP calls
// return immediately. The returned function must be called to stop the watcher.
func (c *Client) withCancel(ctx context.Context) func() {
	ctx = normalizeContext(ctx)
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

// mailboxExists checks if a mailbox with the given name exists on the server.
// Channel is fully drained to avoid blocking the producer goroutine.
func mailboxExists(cli *imapclient.Client, name string) (bool, error) {
	mailboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
	done := make(chan error, 1)

	go func() { done <- cli.List("", name, mailboxes) }()

	exists := false
	for range mailboxes {
		exists = true
	}
	if err := <-done; err != nil {
		return false, fmt.Errorf("list mailbox %q: %w", name, err)
	}
	return exists, nil
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

// MailboxExists reports whether a mailbox with the given name exists on the server.
func (c *Client) MailboxExists(ctx context.Context, name string) (bool, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}

	var exists bool
	err := c.safeCall(func(cli *imapclient.Client) error {
		ok, e := mailboxExists(cli, name)
		if e != nil {
			return e
		}
		exists = ok
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	return exists, nil
}

// CreateMailbox ensures the destination folder (and parents) exist on the server.
func (c *Client) CreateMailbox(ctx context.Context, name string) (bool, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}

	lock := getFolderLock(name)
	lock.Lock()
	defer lock.Unlock()

	var existed bool
	err := c.safeCall(func(cli *imapclient.Client) error {
		ok, e := mailboxExists(cli, name)
		if e != nil {
			return e
		}
		existed = ok
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	if existed {
		return false, nil
	}

	if c.delimiter != "" && strings.Contains(name, c.delimiter) {
		if err := c.createParentFolders(ctx, name, c.delimiter); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}

	err = c.safeCall(func(cli *imapclient.Client) error {
		return cli.Create(name)
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("[%s] failed to create mailbox %s: %w", c.prefix, name, err)
	}

	return true, nil
}

// createParentFolders walks down the hierarchy, creating any missing parent folder.
func (c *Client) createParentFolders(ctx context.Context, name, delimiter string) error {
	parts := strings.Split(name, delimiter)

	for i := 1; i < len(parts); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		parentPath := strings.Join(parts[:i], delimiter)

		lock := getFolderLock(parentPath)
		lock.Lock()

		var exists bool
		err := c.safeCall(func(cli *imapclient.Client) error {
			ok, e := mailboxExists(cli, parentPath)
			if e != nil {
				return e
			}
			exists = ok
			return nil
		})
		if err != nil {
			lock.Unlock()
			return fmt.Errorf("[%s] failed to check parent folder %s: %w", c.prefix, parentPath, err)
		}

		if !exists {
			c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
			err = c.safeCall(func(cli *imapclient.Client) error {
				return cli.Create(parentPath)
			})
			if err != nil {
				lock.Unlock()
				return fmt.Errorf("[%s] failed to create parent folder %s: %w", c.prefix, parentPath, err)
			}
			c.log("[%s] Created parent folder: %s", c.prefix, parentPath)
		}

		lock.Unlock()
	}

	return nil
}

// FetchMessageIDs scans a folder and returns all message IDs.
func (c *Client) FetchMessageIDs(ctx context.Context, folder string) (map[string]bool, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	var ids map[string]bool
	err := c.safeCall(func(cli *imapclient.Client) error {
		ids = make(map[string]bool)
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)
		if mbox.Messages == 0 {
			return nil
		}
		c.log("[%s] Fetching %d message IDs from %s...", c.prefix, mbox.Messages, folder)

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() {
			done <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
		}()

		for msg := range messages {
			if ctx.Err() == nil && msg.Envelope != nil && msg.Envelope.MessageId != "" {
				msgID := strings.Trim(msg.Envelope.MessageId, "<>")
				if msgID != "" {
					ids[msgID] = true
				}
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch IDs: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// FetchMessages retrieves full message envelopes and bodies for a folder.
func (c *Client) FetchMessages(ctx context.Context, folder string) ([]*imap.Message, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var all []*imap.Message
	err := c.safeCall(func(cli *imapclient.Client) error {
		all = nil
		c.log("[%s] Fetching folder %s...", c.prefix, folder)
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)
		if mbox.Messages == 0 {
			return nil
		}
		c.log("[%s] Fetching %d messages from %s...", c.prefix, mbox.Messages, folder)

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() {
			done <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
		}()

		count := 0
		for msg := range messages {
			if ctx.Err() == nil {
				all = append(all, msg)
			}
			count++
			if count%progressUpdateInterval == 0 {
				c.log("[%s] Processed %d/%d messages from %s...", c.prefix, count, mbox.Messages, folder)
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return all, nil
}

// FetchMessagesByIDs retrieves full messages that match the given Message-IDs.
//
// The first stage maps Message-Id to UID by scanning envelopes; the second
// stage fetches bodies in batches of uidFetchBatchSize. Each stage is wrapped
// in its own safeCall so that a reconnect mid-fetch re-Selects the folder
// before retrying just the failing batch.
func (c *Client) FetchMessagesByIDs(ctx context.Context, folder string, targetIDs map[string]bool, tracker *progress.Tracker, _ int) ([]*imap.Message, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(targetIDs) == 0 {
		return []*imap.Message{}, nil
	}

	c.log("[%s] Fetching %d specific messages from %s...", c.prefix, len(targetIDs), folder)

	var targetUIDs []uint32
	err := c.safeCall(func(cli *imapclient.Client) error {
		targetUIDs = nil
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		if mbox.Messages == 0 {
			return nil
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)

		envMessages := make(chan *imap.Message, messageChanBuffer)
		envDone := make(chan error, 1)
		go func() {
			envDone <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, envMessages)
		}()

		for msg := range envMessages {
			if ctx.Err() == nil && msg.Envelope != nil && msg.Envelope.MessageId != "" {
				msgID := strings.Trim(msg.Envelope.MessageId, "<>")
				if targetIDs[msgID] {
					targetUIDs = append(targetUIDs, msg.Uid)
				}
			}
		}
		if err := <-envDone; err != nil {
			return fmt.Errorf("[%s] envelope fetch: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(targetUIDs) == 0 {
		return []*imap.Message{}, nil
	}

	c.log("[%s] Found %d messages to fetch from %s", c.prefix, len(targetUIDs), folder)
	slices.Sort(targetUIDs)

	var result []*imap.Message
	for start := 0; start < len(targetUIDs); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+uidFetchBatchSize, len(targetUIDs))
		uids := targetUIDs[start:end]

		var batch []*imap.Message
		err := c.safeCall(func(cli *imapclient.Client) error {
			batch = nil
			// Re-Select the folder so a reconnect-then-retry has the right state.
			if _, err := cli.Select(folder, true); err != nil {
				return fmt.Errorf("[%s] reselect folder %s: %w", c.prefix, folder, err)
			}
			uidSet := new(imap.SeqSet)
			for _, uid := range uids {
				uidSet.AddNum(uid)
			}
			messages := make(chan *imap.Message, messageChanBuffer)
			batchDone := make(chan error, 1)
			go func() {
				batchDone <- cli.UidFetch(uidSet, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
			}()

			for msg := range messages {
				if ctx.Err() == nil {
					batch = append(batch, msg)
				}
				if tracker != nil && ctx.Err() == nil {
					tracker.UpdateMessage(fmt.Sprintf("[%s] Fetching from %s (%d/%d)", c.prefix, folder, len(result)+len(batch), len(targetUIDs)))
				}
			}
			if err := <-batchDone; err != nil {
				return fmt.Errorf("[%s] body fetch: %w", c.prefix, err)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		result = append(result, batch...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// AppendMessage uploads a single message to the destination folder.
func (c *Client) AppendMessage(ctx context.Context, folder string, msg *imap.Message) error {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}

	body := msg.GetBody(&imap.BodySectionName{})
	if body == nil {
		return fmt.Errorf("[%s] message has no body", c.prefix)
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("[%s] read body: %w", c.prefix, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	flags := []string{imap.SeenFlag}
	date := msg.Envelope.Date

	err = c.safeCall(func(cli *imapclient.Client) error {
		return cli.Append(folder, flags, date, bytes.NewReader(raw))
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("[%s] append: %w", c.prefix, err)
	}
	if c.verbose {
		c.log("[%s] Message %q appended to %s", c.prefix, msg.Envelope.MessageId, folder)
	}
	return nil
}

// MailboxInfo describes message counts and sizes for a single folder.
type MailboxInfo struct {
	Name     string
	Messages uint32
	Size     uint64
}

// ListSubfolders returns subfolders matching the given folder and delimiter.
func (c *Client) ListSubfolders(ctx context.Context, folder, delimiter string) ([]string, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pattern := folder
	if delimiter != "" {
		pattern = folder + delimiter + "*"
	} else {
		pattern = folder + "/*"
	}

	var subfolders []string
	err := c.safeCall(func(cli *imapclient.Client) error {
		subfolders = nil
		mailboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
		done := make(chan error, 1)
		go func() { done <- cli.List("", pattern, mailboxes) }()

		for mbox := range mailboxes {
			if ctx.Err() == nil && mbox.Name != folder {
				subfolders = append(subfolders, mbox.Name)
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] list subfolders: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return subfolders, nil
}

// ListMailboxes fetches all folders plus lightweight statistics for each.
func (c *Client) ListMailboxes(ctx context.Context) ([]*MailboxInfo, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Getting mailbox list...", c.prefix)

	var result []*MailboxInfo
	err := c.safeCall(func(cli *imapclient.Client) error {
		result = nil
		mailboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
		done := make(chan error, 1)
		go func() { done <- cli.List("", "*", mailboxes) }()

		for m := range mailboxes {
			if ctx.Err() == nil {
				result = append(result, &MailboxInfo{Name: m.Name})
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] list mailboxes: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Getting mailbox statistics...", c.prefix)

	if c.tracker != nil {
		c.tracker.UpdateTotal(int64(len(result)))
	}

	for i, mbox := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.tracker != nil {
			c.tracker.UpdateMessage(fmt.Sprintf("[%s] %d/%d %s ", c.prefix, i+1, len(result), mbox.Name))
		}

		var status *imap.MailboxStatus
		err := c.safeCall(func(cli *imapclient.Client) error {
			s, e := cli.Status(mbox.Name, []imap.StatusItem{imap.StatusMessages})
			if e != nil {
				return e
			}
			status = s
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if c.tracker != nil {
				c.tracker.Increment(1)
			}
			continue
		}

		mbox.Messages = status.Messages
		if status.Messages > 0 {
			size, err := c.getFolderSize(ctx, mbox.Name)
			if err == nil {
				mbox.Size = size
			}
		}

		if c.tracker != nil {
			c.tracker.Increment(1)
		}
	}

	if c.tracker != nil {
		c.tracker.UpdateMessage(fmt.Sprintf("[%s] Done (%d mailboxes)", c.prefix, len(result)))
	}

	return result, nil
}

// getFolderSize calculates the total size of all messages in a folder.
func (c *Client) getFolderSize(ctx context.Context, folder string) (uint64, error) {
	ctx = normalizeContext(ctx)
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var totalSize uint64
	err := c.safeCall(func(cli *imapclient.Client) error {
		totalSize = 0
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return err
		}
		if mbox.Messages == 0 {
			return nil
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)

		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() {
			done <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchRFC822Size}, messages)
		}()

		for msg := range messages {
			if ctx.Err() == nil {
				totalSize += uint64(msg.Size)
			}
		}
		if err := <-done; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return totalSize, nil
}
