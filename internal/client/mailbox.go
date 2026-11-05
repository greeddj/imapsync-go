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

	hint := 0
	if c.delimiter != "" {
		// A level needs a non-empty segment and a delimiter, which bounds the buffer for a name made of delimiters.
		hint = min(strings.Count(name, c.delimiter), len(name)/(len(c.delimiter)+1))
	}
	levels := slices.AppendSeq(make([]string, 0, hint), parentLevels(name, c.delimiter))
	parent := ""
	if len(levels) > 0 {
		parent = levels[len(levels)-1]
	}

	parents, err = c.createParentFolders(ctx, levels)
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
			c.addRooted(name, parent)
			return false, parents, nil
		}
		return false, parents, fmt.Errorf("[%s] failed to create mailbox %s: %w", c.prefix, name, err)
	}
	c.addRooted(name, parent)

	return true, parents, nil
}

// MissingLevels lists the CREATE targets of CreateMailbox for resolved names in order: missing parents from the top, then the name. A shared level appears once. A cached name adds none.
func (c *Client) MissingLevels(ctx context.Context, names ...string) ([]string, error) {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	return missingLevels(ctx, names, c.delimiter, func(name string) bool {
		_, ok := c.mailboxCache.resolve(name)
		return ok
	})
}

func missingLevels(ctx context.Context, names []string, delimiter string, listed func(string) bool) ([]string, error) {
	var (
		out []string
		// seen holds each unlisted name and all its parent levels, so firstUnmarked finds a prefix. This bounds cache lookups to one per distinct level, plus one per name.
		seen   map[string]struct{}
		levels []string
	)
	inSeen := func(level string) bool {
		_, ok := seen[level]
		return ok
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inSeen(name) || listed(name) {
			continue
		}
		if seen == nil {
			seen = make(map[string]struct{}, len(names))
			out = make([]string, 0, len(names))
		}
		levels = slices.AppendSeq(levels[:0], parentLevels(name, delimiter))
		for _, level := range levels[firstUnmarked(levels, inSeen):] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !listed(level) {
				out = append(out, level)
			}
			seen[level] = struct{}{}
		}
		out = append(out, name)
		seen[name] = struct{}{}
	}
	return out, nil
}

// marked must hold for a prefix of levels and then fail. firstUnmarked probes from the deepest level: a growing chain costs one check, a gap a logarithmic number.
func firstUnmarked(levels []string, marked func(string) bool) int {
	n := len(levels)
	if n == 0 {
		return 0
	}
	if marked(levels[n-1]) {
		return n
	}
	// levels[hi] is unmarked and levels[:lo] are marked.
	lo, hi := 0, n-1
	for step := 2; step <= n; step *= 2 {
		i := n - step
		if marked(levels[i]) {
			lo = i + 1
			break
		}
		hi = i
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if marked(levels[mid]) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// parentLevels yields each prefix of name that ends before a delimiter and has a non-empty last level, shortest first. An empty level names no mailbox. An empty delimiter yields nothing.
func parentLevels(name, delimiter string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if delimiter == "" {
			return
		}
		start := 0
		for {
			i := strings.Index(name[start:], delimiter)
			if i < 0 {
				return
			}
			end := start + i
			if i > 0 && !yield(name[:end]) {
				return
			}
			start = end + len(delimiter)
		}
	}
}

// made counts the CREATE commands that the server accepted, also when err is set. A rooted level has every level above it cached, so the walk skips them.
func (c *Client) createParentFolders(ctx context.Context, levels []string) (made int, err error) {
	for i := c.firstUnrooted(levels); i < len(levels); i++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return made, ctxErr
		}
		parentPath, upper := levels[i], ""
		if i > 0 {
			upper = levels[i-1]
		}

		lock := c.folderLock(parentPath)
		lock.Lock()

		if c.hasMailbox(parentPath) {
			c.markRooted(parentPath, upper)
			lock.Unlock()
			continue
		}

		c.log("[%s] Creating parent folder: %s", c.prefix, parentPath)
		createErr := c.safeCall(func(cli *imapclient.Client) error {
			return cli.Create(parentPath)
		})
		exists := createErr != nil && isAlreadyExistsErr(createErr)
		if createErr != nil && !exists {
			lock.Unlock()
			return made, fmt.Errorf("[%s] failed to create parent folder %s: %w", c.prefix, parentPath, createErr)
		}
		c.addRooted(parentPath, upper)
		if exists {
			c.log("[%s] Parent folder already exists: %s", c.prefix, parentPath)
		} else {
			made++
			c.log("[%s] Created parent folder: %s", c.prefix, parentPath)
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
