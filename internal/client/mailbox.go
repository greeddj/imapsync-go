package client

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/greeddj/imapsync-go/internal/termsafe"
)

// MailboxInfo describes message counts and sizes for a single folder.
type MailboxInfo struct {
	// Err is the reason the statistics could not be read. Messages and Size
	// are then zero.
	Err      error
	Name     string
	Size     uint64
	Messages uint32
	// NoSelect marks a name that LIST flags \Noselect or \NonExistent. It gets
	// no command, and Err stays nil.
	NoSelect bool
}

// MailboxExists reports whether the mailbox cache holds name, so planning costs one LIST. A \Noselect name counts as existing, and a leading INBOX level matches in any letter case.
func (c *Client) MailboxExists(ctx context.Context, name string) (bool, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.hasMailbox(name), nil
}

// CreateMailbox ensures name and its parent folders exist. created reports a CREATE of name. parents counts the parents that a CREATE made, also on error.
func (c *Client) CreateMailbox(ctx context.Context, name string) (created bool, parents int, err error) {
	stop := c.withCancel(ctx)
	defer stop()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, 0, ctxErr
	}

	lock := c.folderLock(name)
	lock.Lock()
	defer lock.Unlock()

	if c.hasMailbox(name) {
		return false, 0, nil
	}

	parents, err = c.createParentFolders(ctx, name, c.delimiter)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, parents, ctxErr
		}
		return false, parents, err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, parents, ctxErr
	}

	err = c.safeCall(func(cli *imapclient.Client) error {
		return cli.Create(name)
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, parents, ctxErr
		}
		// Another client (or our own stale cache) created the folder
		// between hasMailbox() and Create(). Treat as idempotent success,
		// and record the folder so later lookups see it.
		if isAlreadyExistsErr(err) {
			c.addMailboxToCache(name)
			return false, parents, nil
		}
		return false, parents, fmt.Errorf("[%s] failed to create mailbox %s: %w", c.prefix, name, err)
	}
	c.addMailboxToCache(name)

	return true, parents, nil
}

// MissingLevels returns the names that CreateMailbox(name) sends a CREATE for while the cache stays unchanged: missing parents first, then name, or nothing when the cache holds name.
func (c *Client) MissingLevels(name string) []string {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	if _, ok := c.mailboxCache.resolve(name); ok {
		return nil
	}
	var missing []string
	for level := range parentLevels(name, c.delimiter) {
		if _, ok := c.mailboxCache.resolve(level); !ok {
			missing = append(missing, level)
		}
	}
	return append(missing, name)
}

// parentLevels yields each prefix of name that ends before a delimiter, shortest first. An empty delimiter marks a flat server, so it yields nothing.
func parentLevels(name, delimiter string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if delimiter == "" {
			return
		}
		end := 0
		for {
			i := strings.Index(name[end:], delimiter)
			if i < 0 {
				return
			}
			end += i
			if !yield(name[:end]) {
				return
			}
			end += len(delimiter)
		}
	}
}

// createParentFolders walks down the hierarchy, creating any missing parent folder. made counts the CREATE commands that the server accepted, also when err is set.
func (c *Client) createParentFolders(ctx context.Context, name, delimiter string) (made int, err error) {
	for parentPath := range parentLevels(name, delimiter) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return made, ctxErr
		}

		lock := c.folderLock(parentPath)
		lock.Lock()

		if !c.hasMailbox(parentPath) {
			c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
			createErr := c.safeCall(func(cli *imapclient.Client) error {
				return cli.Create(parentPath)
			})
			exists := createErr != nil && isAlreadyExistsErr(createErr)
			if createErr != nil && !exists {
				lock.Unlock()
				return made, fmt.Errorf("[%s] failed to create parent folder %s: %w", c.prefix, parentPath, createErr)
			}
			c.addMailboxToCache(parentPath)
			if exists {
				c.log("[%s] Parent folder already exists: %s", c.prefix, parentPath)
			} else {
				made++
				c.log("[%s] Created parent folder: %s", c.prefix, parentPath)
			}
		}

		lock.Unlock()
	}

	return made, nil
}

// MailboxNames returns every cached name as LIST gave it, sorted, \Noselect names included, at no round-trip. A bare INBOX is upper case, as go-imap folds it.
func (c *Client) MailboxNames() []string {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	names := make([]string, 0, len(c.mailboxCache.folders))
	for name := range c.mailboxCache.folders {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// InvalidMailboxNames returns the sorted raw names that the latest LIST left out because they are not valid modified UTF-7.
func (c *Client) InvalidMailboxNames() []string {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	return slices.Clone(c.mailboxCache.invalid)
}

// IsNoSelect reports whether the listed name that name resolves to is marked \Noselect or \NonExistent. An exact listed spelling wins over a fold of the INBOX level.
func (c *Client) IsNoSelect(name string) bool {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	listed, ok := c.mailboxCache.resolve(name)
	if !ok {
		return false
	}
	_, noSelect := c.mailboxCache.noSelect[listed]
	return noSelect
}

// ListSubfolders returns listed names under folder; an empty delimiter means the server's. A leading INBOX level can differ in letter case, so callers cut the relative path by length.
func (c *Client) ListSubfolders(ctx context.Context, folder, delimiter string) ([]string, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.listSubfoldersFromCache(folder, delimiter), nil
}

// ListMailboxes lists all folders with their statistics. It fails only when
// ctx ends or LIST fails; a folder whose statistics cannot be read carries
// the reason in Err.
func (c *Client) ListMailboxes(ctx context.Context) ([]*MailboxInfo, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Getting mailbox list...", c.prefix)

	var (
		result  []*MailboxInfo
		invalid []string
	)
	err := c.safeCall(func(cli *imapclient.Client) error {
		var listed []*MailboxInfo
		inv, err := listAll(cli, func(m *imap.MailboxInfo) {
			listed = append(listed, &MailboxInfo{Name: m.Name, NoSelect: noSelectAttr(m.Attributes)})
		})
		if err != nil {
			return fmt.Errorf("[%s] list mailboxes: %w", c.prefix, err)
		}
		result, invalid = listed, inv
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

	c.mailboxCacheMu.Lock()
	c.mailboxCache.setInvalid(invalid)
	c.mailboxCacheMu.Unlock()

	c.log("[%s] Getting mailbox statistics...", c.prefix)

	tr := c.progressTracker()
	if tr != nil {
		tr.UpdateTotal(int64(len(result)))
	}

	for i, mbox := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tr != nil {
			tr.UpdateMessage(fmt.Sprintf("[%s] %d/%d %s ", c.prefix, i+1, len(result), termsafe.Escape(mbox.Name)))
		}

		if !mbox.NoSelect {
			if err := c.readStats(ctx, mbox); err != nil {
				return nil, err
			}
		}

		if tr != nil {
			tr.Increment(1)
		}
	}

	if tr != nil {
		tr.UpdateMessage(fmt.Sprintf("[%s] Done (%d mailboxes)", c.prefix, len(result)))
	}

	return result, nil
}

// A failed read goes to mbox.Err and leaves Messages and Size at zero; only an
// ended ctx is returned as an error.
func (c *Client) readStats(ctx context.Context, mbox *MailboxInfo) error {
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
			return ctx.Err()
		}
		mbox.Err = fmt.Errorf("read message count: %w", err)
		return nil
	}

	var size uint64
	if status.Messages > 0 {
		size, err = c.getFolderSize(ctx, mbox.Name)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			mbox.Err = fmt.Errorf("read folder size: %w", err)
			return nil
		}
	}
	mbox.Messages = status.Messages
	mbox.Size = size
	return nil
}

// getFolderSize calculates the total size of all messages in a folder.
func (c *Client) getFolderSize(ctx context.Context, folder string) (uint64, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var totalSize uint64
	err := c.safeCall(func(cli *imapclient.Client) error {
		totalSize = 0
		mbox, err := c.selectIfNeeded(cli, folder)
		if err != nil {
			return err
		}
		// selectIfNeeded returns nil when the folder is already selected
		// on this connection - fall back to STATUS for the count, same as
		// FetchMessageMap does.
		var total uint32
		if mbox != nil {
			total = mbox.Messages
		} else {
			st, serr := cli.Status(folder, []imap.StatusItem{imap.StatusMessages})
			if serr != nil {
				return serr
			}
			total = st.Messages
		}
		if total == 0 {
			return nil
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, total)

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
