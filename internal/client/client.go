// Package client wraps go-imap with reconnect logic and higher-level helpers.
package client

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
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
)

// Global lock map for folder creation to prevent race conditions
var (
	folderLocksMu sync.Mutex
	folderLocks   = make(map[string]*sync.Mutex)
)

// getFolderLock returns a mutex for a specific folder path
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

// Client embeds an IMAP client with retry-friendly helpers.
type Client struct {
	*client.Client

	serverAddr    string                              // IMAP server address.
	useTLS        bool                                // Whether to use TLS for connections.
	tlsConfig     *tls.Config                         // TLS configuration.
	username      string                              // IMAP username.
	password      string                              // IMAP password.
	dialFn        func(addr string) (net.Conn, error) // Connection dialer function.
	mu            sync.Mutex                          // Protects reconnection state.
	backoff       time.Duration                       // Current reconnection backoff duration.
	lastReconnect time.Time                           // Timestamp of last reconnection attempt.
	reconnectDur  time.Duration                       // Minimum duration between reconnects.
	prefix        string                              // Log message prefix.
	verbose       bool                                // Enable verbose logging.
	pw            ProgressWriter                      // Optional progress writer for logging.
	tracker       ProgressTracker                     // Optional progress tracker for updates.
	delimiter     string                              // Cached hierarchy delimiter from server.
}

// New establishes a connection and logs into the IMAP server.
func New(addr, username, password string, workers int, verbose, useTLS bool, tlsConfig *tls.Config) (*Client, error) {
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

	// Get and cache the delimiter
	delimiter, err := c.getDelimiter()
	if err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("failed to get delimiter: %w", err)
	}
	c.delimiter = delimiter

	return c, nil
}

// SetPrefix configures the log prefix used in progress messages.
func (c *Client) SetPrefix(p string) {
	c.prefix = p
}

// SetProgressWriter sets the progress writer for logging.
func (c *Client) SetProgressWriter(pw ProgressWriter) {
	c.pw = pw
}

// SetProgressTracker sets an optional progress tracker for updating progress.
func (c *Client) SetProgressTracker(tracker ProgressTracker) {
	c.tracker = tracker
}

// GetDelimiter returns the cached hierarchy delimiter for this server.
func (c *Client) GetDelimiter() string {
	return c.delimiter
}

// log outputs a message either to progress writer or stdout based on availability.
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

	client, err := client.New(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}

	if err := client.Login(c.username, c.password); err != nil {
		_ = client.Logout()
		return err
	}

	c.Client = client
	return nil
}

// Reconnect tears down and rebuilds the underlying IMAP session with backoff.
func (c *Client) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	sinceLast := now.Sub(c.lastReconnect)
	if sinceLast < c.reconnectDur {
		wait := c.reconnectDur - sinceLast
		c.log("[%s] 🔄 Reconnecting in %s...", c.prefix, wait)
		time.Sleep(wait)
	}

	if c.Client != nil {
		_ = c.Logout()
	}

	var err error
	delay := c.backoff

	for i := 1; i <= maxReconnectAttempts; i++ {
		c.log("[%s] 🔄 Reconnect attempt %d...", c.prefix, i)
		err = c.connectAndLogin()
		if err == nil {
			c.log("[%s] 🔄 Reconnected successfully", c.prefix)
			c.lastReconnect = time.Now()
			c.backoff = 2 * time.Second
			return nil
		}

		c.log("[%s] 🔄 Failed: %v, retrying in %s", c.prefix, err, delay)
		time.Sleep(delay)
		delay *= 2
	}

	c.lastReconnect = time.Now()
	return fmt.Errorf("[%s] failed to reconnect after retries: %w", c.prefix, err)
}

// safeCall wraps an IMAP operation with automatic reconnection on connection errors.
func (c *Client) safeCall(fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}

	if isConnError(err) {
		if rerr := c.Reconnect(); rerr != nil {
			return rerr
		}
		return fn()
	}

	return err
}

// isConnError determines if an error is connection-related and warrants a reconnect attempt.
func isConnError(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.As(err, &netErr)
}

// SafeSelect selects a mailbox and retries on transient connection errors.
func (c *Client) SafeSelect(mailbox string, readOnly bool) (*imap.MailboxStatus, error) {
	var mbox *imap.MailboxStatus
	err := c.safeCall(func() error {
		var e error
		mbox, e = c.Select(mailbox, readOnly)
		return e
	})
	return mbox, err
}

// SafeSearch wraps IMAP search with automatic reconnects.
func (c *Client) SafeSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	var ids []uint32
	err := c.safeCall(func() error {
		var e error
		ids, e = c.Search(criteria)
		return e
	})
	return ids, err
}

// CreateMailbox ensures the destination folder (and parents) exist on the server.
func (c *Client) CreateMailbox(name string) (bool, error) {
	// Lock this specific folder path to prevent concurrent creation
	lock := getFolderLock(name)
	lock.Lock()
	defer lock.Unlock()

	if exists, err := c.mailboxExists(name); err != nil {
		return false, err
	} else if exists {
		return false, nil
	}

	delimiter, err := c.getDelimiter()
	if err != nil {
		return false, fmt.Errorf("[%s] failed to get delimiter: %w", c.prefix, err)
	}

	if delimiter != "" && strings.Contains(name, delimiter) {
		if err := c.createParentFolders(name, delimiter); err != nil {
			return false, err
		}
	}

	err = c.safeCall(func() error {
		return c.Create(name)
	})

	if err != nil {
		return false, fmt.Errorf("[%s] failed to create mailbox %s: %w", c.prefix, name, err)
	}

	return true, nil
}

// mailboxExists checks if a mailbox with the given name exists on the server.
func (c *Client) mailboxExists(name string) (bool, error) {
	mailboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
	done := make(chan error, 1)

	go func() {
		done <- c.List("", name, mailboxes)
	}()

	exists := false
	for range mailboxes {
		exists = true
		break
	}

	if err := <-done; err != nil {
		return false, fmt.Errorf("[%s] failed to check mailbox existence: %w", c.prefix, err)
	}

	return exists, nil
}

// getDelimiter retrieves the hierarchy delimiter used by the IMAP server.
func (c *Client) getDelimiter() (string, error) {
	mailboxes := make(chan *imap.MailboxInfo, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.List("", "", mailboxes)
	}()

	delimiter := "/"
	for mbox := range mailboxes {
		if mbox.Delimiter != "" {
			delimiter = mbox.Delimiter
			break
		}
	}

	if err := <-done; err != nil {
		return "", fmt.Errorf("[%s] failed to get delimiter: %w", c.prefix, err)
	}

	return delimiter, nil
}

// createParentFolders recursively creates all parent folders in a hierarchy.
func (c *Client) createParentFolders(name, delimiter string) error {
	parts := strings.Split(name, delimiter)

	for i := 1; i < len(parts); i++ {
		parentPath := strings.Join(parts[:i], delimiter)

		// Get a lock specific to this folder path to prevent race conditions
		lock := getFolderLock(parentPath)
		lock.Lock()

		exists, err := c.mailboxExists(parentPath)
		if err != nil {
			lock.Unlock()
			return fmt.Errorf("[%s] failed to check parent folder %s: %w", c.prefix, parentPath, err)
		}

		if !exists {
			c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
			err = c.safeCall(func() error {
				return c.Create(parentPath)
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
func (c *Client) FetchMessageIDs(folder string) (map[string]bool, error) {
	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	mbox, err := c.Select(folder, true)
	if err != nil {
		return nil, fmt.Errorf("[%s] cannot select folder %s: %v", c.prefix, folder, err)
	}
	c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)

	ids := make(map[string]bool)
	if mbox.Messages == 0 {
		return ids, nil
	}

	c.log("[%s] Fetching %d message IDs from %s...", c.prefix, mbox.Messages, folder)

	seqset := new(imap.SeqSet)
	seqset.AddRange(1, mbox.Messages)
	items := []imap.FetchItem{imap.FetchEnvelope}
	messages := make(chan *imap.Message, messageChanBuffer)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, items, messages) }()

	count := 0
	for msg := range messages {
		if msg.Envelope != nil && msg.Envelope.MessageId != "" {
			msgID := strings.Trim(msg.Envelope.MessageId, "<>")
			if msgID != "" {
				ids[msgID] = true
			}
		}
		count++
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("[%s] fetch IDs error: %v", c.prefix, err)
	}
	return ids, nil
}

// FetchMessages retrieves full message envelopes and bodies for a folder.
func (c *Client) FetchMessages(folder string) ([]*imap.Message, error) {
	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	mbox, err := c.Select(folder, true)
	if err != nil {
		return nil, fmt.Errorf("[%s] cannot select folder %s: %v", c.prefix, folder, err)
	}
	c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)
	if mbox.Messages == 0 {
		return []*imap.Message{}, nil
	}

	c.log("[%s] Fetching %d messages from %s...", c.prefix, mbox.Messages, folder)

	seqset := new(imap.SeqSet)
	seqset.AddRange(1, mbox.Messages)
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}

	messages := make(chan *imap.Message, messageChanBuffer)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, items, messages) }()

	var all []*imap.Message
	count := 0
	for msg := range messages {
		all = append(all, msg)
		count++
		if count%progressUpdateInterval == 0 {
			c.log("[%s] Processed %d/%d messages from %s...", c.prefix, count, mbox.Messages, folder)
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("[%s] fetch error: %v", c.prefix, err)
	}
	return all, nil
}

// FetchMessagesByIDs retrieves full messages that match the given Message-IDs.
func (c *Client) FetchMessagesByIDs(folder string, targetIDs map[string]bool, tracker *progress.Tracker, totalToFetch int) ([]*imap.Message, error) {
	if len(targetIDs) == 0 {
		return []*imap.Message{}, nil
	}

	c.log("[%s] Fetching %d specific messages from %s...", c.prefix, len(targetIDs), folder)

	mbox, err := c.Select(folder, true)
	if err != nil {
		return nil, fmt.Errorf("[%s] cannot select folder %s: %v", c.prefix, folder, err)
	}

	if mbox.Messages == 0 {
		return []*imap.Message{}, nil
	}

	// First pass: find UIDs of messages we need
	seqset := new(imap.SeqSet)
	seqset.AddRange(1, mbox.Messages)

	envMessages := make(chan *imap.Message, messageChanBuffer)
	done := make(chan error, 1)
	go func() { done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, envMessages) }()

	var targetUIDs []uint32
	for msg := range envMessages {
		if msg.Envelope != nil && msg.Envelope.MessageId != "" {
			msgID := strings.Trim(msg.Envelope.MessageId, "<>")
			if targetIDs[msgID] {
				targetUIDs = append(targetUIDs, msg.Uid)
			}
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("[%s] envelope fetch error: %v", c.prefix, err)
	}

	if len(targetUIDs) == 0 {
		return []*imap.Message{}, nil
	}

	c.log("[%s] Found %d messages to fetch from %s", c.prefix, len(targetUIDs), folder)

	// Second pass: fetch full bodies for target UIDs only
	uidSet := new(imap.SeqSet)
	for _, uid := range targetUIDs {
		uidSet.AddNum(uid)
	}

	messages := make(chan *imap.Message, messageChanBuffer)
	done = make(chan error, 1)
	go func() { done <- c.UidFetch(uidSet, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages) }()

	var result []*imap.Message
	for msg := range messages {
		result = append(result, msg)
		if tracker != nil {
			tracker.UpdateMessage(fmt.Sprintf("[%s] Fetching from %s (%d/%d)", c.prefix, folder, len(result), len(targetUIDs)))
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("[%s] body fetch error: %v", c.prefix, err)
	}

	return result, nil
}

// AppendMessage uploads a single message to the destination folder.
func (c *Client) AppendMessage(folder string, msg *imap.Message) error {
	body := msg.GetBody(&imap.BodySectionName{})
	if body == nil {
		return fmt.Errorf("[%s] message has no body", c.prefix)
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("[%s] read body: %v", c.prefix, err)
	}

	flags := []string{imap.SeenFlag}
	date := msg.Envelope.Date

	err = c.safeCall(func() error {
		literal := bytes.NewReader(raw)
		return c.Append(folder, flags, date, literal)
	})

	if err != nil {
		return fmt.Errorf("[%s] append failed: %v", c.prefix, err)
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

// ListMailboxes fetches all folders plus lightweight statistics for each.
func (c *Client) ListMailboxes() ([]*MailboxInfo, error) {
	c.log("[%s] Getting mailbox list...", c.prefix)

	mailboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	var result []*MailboxInfo
	for m := range mailboxes {
		result = append(result, &MailboxInfo{
			Name: m.Name,
		})
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("[%s] list mailboxes error: %v", c.prefix, err)
	}

	c.log("[%s] Getting mailbox statistics...", c.prefix)

	// Update tracker total based on actual mailbox count
	if c.tracker != nil {
		c.tracker.UpdateTotal(int64(len(result)))
	}

	for i, mbox := range result {
		if c.tracker != nil {
			c.tracker.UpdateMessage(fmt.Sprintf("[%s] %d/%d %s ", c.prefix, i+1, len(result), mbox.Name))
		}

		status, err := c.Status(mbox.Name, []imap.StatusItem{
			imap.StatusMessages,
		})

		if err != nil {
			if c.tracker != nil {
				c.tracker.Increment(1)
			}
			continue
		}

		mbox.Messages = status.Messages

		if status.Messages > 0 {
			size, err := c.getFolderSize(mbox.Name)
			if err == nil {
				mbox.Size = size
			}
		}

		if c.tracker != nil {
			c.tracker.Increment(1)
		}
	}

	// Update final message
	if c.tracker != nil {
		c.tracker.UpdateMessage(fmt.Sprintf("[%s] Done (%d mailboxes)", c.prefix, len(result)))
	}

	return result, nil
}

// getFolderSize calculates the total size of all messages in a folder.
func (c *Client) getFolderSize(folder string) (uint64, error) {
	mbox, err := c.Select(folder, true)
	if err != nil {
		return 0, err
	}

	if mbox.Messages == 0 {
		return 0, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(1, mbox.Messages)

	messages := make(chan *imap.Message, messageChanBuffer)
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchRFC822Size}, messages)
	}()

	var totalSize uint64
	for msg := range messages {
		totalSize += uint64(msg.Size)
	}

	if err := <-done; err != nil {
		return 0, err
	}

	return totalSize, nil
}
