package client

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// nonExistentAttr is the RFC 5258 attribute for a name that cannot be selected.
// go-imap v1 has no constant for it.
const nonExistentAttr = `\NonExistent`

// mailboxCache holds the result of a single LIST "" "*" call so that the
// many existence checks we do during sync planning don't each cost a
// round-trip. New loads it before returning the Client and CreateMailbox
// adds to it, so readers never see an unloaded cache.
type mailboxCache struct {
	folders map[string]struct{}
	// Each name here also stays in folders, so parent creation never sends CREATE for it.
	noSelect map[string]struct{}
	// Empty when no LIST entry names a delimiter: NIL marks a flat namespace (RFC 3501).
	delimiter string
}

// RFC 3501 makes INBOX case-insensitive, and a leading INBOX level follows the same rule, so cache keys and lookups agree.
func canonicalName(name, delimiter string) string {
	n := len(imap.InboxName)
	if len(name) < n || !strings.EqualFold(name[:n], imap.InboxName) {
		return name
	}
	rest := name[n:]
	if rest != "" && (delimiter == "" || !strings.HasPrefix(rest, delimiter)) {
		return name
	}
	if name[:n] == imap.InboxName {
		return name
	}
	return imap.InboxName + rest
}

// CanonicalName returns name as the mailbox cache holds it: INBOX in upper case, including a leading INBOX level.
func (c *Client) CanonicalName(name string) string {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	return canonicalName(name, c.mailboxCache.delimiter)
}

// RFC 3501 makes attributes case-insensitive.
func noSelectAttr(attrs []string) bool {
	return slices.ContainsFunc(attrs, func(attr string) bool {
		return strings.EqualFold(attr, imap.NoSelectAttr) || strings.EqualFold(attr, nonExistentAttr)
	})
}

// loadMailboxCache fills the mailbox cache and the hierarchy delimiter from one
// LIST "" "*".
func (c *Client) loadMailboxCache(ctx context.Context) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}

	folders := make(map[string]struct{})
	var noSelect map[string]struct{}
	delim := ""

	err := c.safeCall(func(cli *imapclient.Client) error {
		folders = make(map[string]struct{})
		noSelect = nil
		delim = ""
		mboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
		done := make(chan error, 1)
		go func() { done <- cli.List("", "*", mboxes) }()
		for m := range mboxes {
			key := canonicalName(m.Name, m.Delimiter)
			folders[key] = struct{}{}
			if noSelectAttr(m.Attributes) {
				if noSelect == nil {
					noSelect = make(map[string]struct{})
				}
				noSelect[key] = struct{}{}
			}
			if delim == "" && m.Delimiter != "" {
				delim = m.Delimiter
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] list mailboxes: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mailboxCacheMu.Lock()
	c.mailboxCache.folders = folders
	c.mailboxCache.noSelect = noSelect
	c.mailboxCache.delimiter = delim
	c.mailboxCacheMu.Unlock()
	return nil
}

// hasMailbox reports whether name exists, consulting the cache.
func (c *Client) hasMailbox(name string) bool {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	_, ok := c.mailboxCache.folders[canonicalName(name, c.mailboxCache.delimiter)]
	return ok
}

// addMailboxToCache records a freshly created mailbox.
func (c *Client) addMailboxToCache(name string) {
	c.mailboxCacheMu.Lock()
	defer c.mailboxCacheMu.Unlock()
	if c.mailboxCache.folders == nil {
		c.mailboxCache.folders = make(map[string]struct{})
	}
	c.mailboxCache.folders[canonicalName(name, c.mailboxCache.delimiter)] = struct{}{}
}

// listSubfoldersFromCache returns mailboxes that look like children of parent.
// "Children" means the cached name starts with parent+delimiter. Unlike a
// LIST pattern this does not require a server round-trip. It returns nil when
// neither delimiter is set, as on a flat server.
func (c *Client) listSubfoldersFromCache(parent, delimiter string) []string {
	if delimiter == "" {
		delimiter = c.delimiter
	}
	if delimiter == "" {
		return nil
	}
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	canonicalParent := canonicalName(parent, delimiter)
	prefix := canonicalParent + delimiter
	out := make([]string, 0)
	for name := range c.mailboxCache.folders {
		if name == canonicalParent {
			continue
		}
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			out = append(out, name)
		}
	}
	return out
}
