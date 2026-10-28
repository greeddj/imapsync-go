package client

import (
	"context"
	"fmt"
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

// MailboxExists reports whether the mailbox cache holds name, so planning
// costs one LIST. A leading INBOX level matches in any letter case. A
// \Noselect name counts as existing.
func (c *Client) MailboxExists(ctx context.Context, name string) (bool, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.hasMailbox(name), nil
}

// CreateMailbox ensures the destination folder (and any missing parents) exist
// on the server. Returns true when a new mailbox was created on this call,
// false when it already existed.
func (c *Client) CreateMailbox(ctx context.Context, name string) (bool, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}

	lock := c.folderLock(name)
	lock.Lock()
	defer lock.Unlock()

	if c.hasMailbox(name) {
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

	err := c.safeCall(func(cli *imapclient.Client) error {
		return cli.Create(name)
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Another client (or our own stale cache) created the folder
		// between hasMailbox() and Create(). Treat as idempotent success,
		// and record the folder so later lookups see it.
		if isAlreadyExistsErr(err) {
			c.addMailboxToCache(name)
			return false, nil
		}
		return false, fmt.Errorf("[%s] failed to create mailbox %s: %w", c.prefix, name, err)
	}
	c.addMailboxToCache(name)

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

		lock := c.folderLock(parentPath)
		lock.Lock()

		if !c.hasMailbox(parentPath) {
			c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
			err := c.safeCall(func(cli *imapclient.Client) error {
				return cli.Create(parentPath)
			})
			if err != nil && !isAlreadyExistsErr(err) {
				lock.Unlock()
				return fmt.Errorf("[%s] failed to create parent folder %s: %w", c.prefix, parentPath, err)
			}
			c.addMailboxToCache(parentPath)
			c.log("[%s] Created parent folder: %s", c.prefix, parentPath)
		}

		lock.Unlock()
	}

	return nil
}

// MailboxNames returns every cached mailbox name byte for byte as the server
// listed it, sorted, \Noselect names included. IsNoSelect tells whether a name
// can hold messages. It costs no round-trip.
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

// IsNoSelect reports whether the listed name that name resolves to is marked
// \Noselect or \NonExistent. An exact listed spelling wins over a fold of the INBOX level.
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

// ListSubfolders returns the cached listed names under folder; an empty
// delimiter means the server's. Only a leading INBOX level can differ in
// letter case from folder, so cut by length.
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
