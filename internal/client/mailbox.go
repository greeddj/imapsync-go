package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// MailboxInfo describes message counts and sizes for a single folder.
type MailboxInfo struct {
	Name     string
	Messages uint32
	Size     uint64
}

// MailboxExists reports whether a mailbox with the given name exists on the
// server. Backed by the per-Client mailbox cache so that the many existence
// checks done at planning time amortize to a single LIST.
func (c *Client) MailboxExists(ctx context.Context, name string) (bool, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.hasMailbox(ctx, name)
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

	existed, err := c.hasMailbox(ctx, name)
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
		// Another client (or our own stale cache) created the folder
		// between hasMailbox() and Create(). Treat as idempotent success
		// — but still refresh the cache so subsequent lookups skip the
		// LIST round-trip.
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

		exists, err := c.hasMailbox(ctx, parentPath)
		if err != nil {
			lock.Unlock()
			return fmt.Errorf("[%s] failed to check parent folder %s: %w", c.prefix, parentPath, err)
		}

		if !exists {
			c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
			err = c.safeCall(func(cli *imapclient.Client) error {
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

// ListSubfolders returns subfolders of folder, derived from the cached
// mailbox list. delimiter is used to compute the prefix; when empty the
// client's cached server delimiter is substituted.
func (c *Client) ListSubfolders(ctx context.Context, folder, delimiter string) ([]string, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.listSubfoldersFromCache(ctx, folder, delimiter)
}

// ListMailboxes fetches all folders plus lightweight statistics for each.
func (c *Client) ListMailboxes(ctx context.Context) ([]*MailboxInfo, error) {
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

	tr := c.progressTracker()
	if tr != nil {
		tr.UpdateTotal(int64(len(result)))
	}

	for i, mbox := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tr != nil {
			tr.UpdateMessage(fmt.Sprintf("[%s] %d/%d %s ", c.prefix, i+1, len(result), mbox.Name))
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
			if tr != nil {
				tr.Increment(1)
			}
			continue
		}

		mbox.Messages = status.Messages
		if status.Messages > 0 {
			size, err := c.getFolderSize(ctx, mbox.Name)
			switch {
			case err == nil:
				mbox.Size = size
			case ctx.Err() != nil:
				// canceled — outer loop will bail
			case c.verbose:
				c.log("[%s] failed to size %s: %v", c.prefix, mbox.Name, err)
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
		// on this connection — fall back to STATUS for the count, same as
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
