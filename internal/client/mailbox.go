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

	lock := c.folderLock(name)
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

		lock := c.folderLock(parentPath)
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
