package client

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// mailboxCache holds the result of a single LIST "" "*" call so that the
// many existence checks we do during sync planning don't each cost a
// round-trip. New loads it before returning the Client and CreateMailbox
// adds to it, so readers never see an unloaded cache.
type mailboxCache struct {
	folders   map[string]struct{}
	delimiter string
}

// loadMailboxCache populates the cache via a single LIST "" "*". The
// hierarchy delimiter is captured from the first reply, replacing the older
// dedicated refreshDelimiter call.
//
// loadMailboxCache is safe to call concurrently — the inner safeCall
// serializes the IMAP traffic on c.mu, and the cache mutation is guarded
// by c.mailboxCacheMu.
func (c *Client) loadMailboxCache(_ context.Context) error {
	folders := make(map[string]struct{})
	delim := ""

	err := c.safeCall(func(cli *imapclient.Client) error {
		folders = make(map[string]struct{})
		delim = ""
		mboxes := make(chan *imap.MailboxInfo, mailboxChanBuffer)
		done := make(chan error, 1)
		go func() { done <- cli.List("", "*", mboxes) }()
		for m := range mboxes {
			folders[m.Name] = struct{}{}
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
		return err
	}
	if delim == "" {
		delim = "/"
	}

	c.mailboxCacheMu.Lock()
	c.mailboxCache.folders = folders
	c.mailboxCache.delimiter = delim
	c.mailboxCacheMu.Unlock()
	return nil
}

// hasMailbox reports whether name exists, consulting the cache.
func (c *Client) hasMailbox(name string) bool {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	_, ok := c.mailboxCache.folders[name]
	return ok
}

// addMailboxToCache records a freshly created mailbox.
func (c *Client) addMailboxToCache(name string) {
	c.mailboxCacheMu.Lock()
	defer c.mailboxCacheMu.Unlock()
	if c.mailboxCache.folders == nil {
		c.mailboxCache.folders = make(map[string]struct{})
	}
	c.mailboxCache.folders[name] = struct{}{}
}

// listSubfoldersFromCache returns mailboxes that look like children of parent.
// "Children" means the cached name starts with parent+delimiter. Unlike a
// LIST pattern this does not require a server round-trip.
func (c *Client) listSubfoldersFromCache(parent, delimiter string) []string {
	if delimiter == "" {
		delimiter = c.delimiter
	}
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	prefix := parent + delimiter
	out := make([]string, 0)
	for name := range c.mailboxCache.folders {
		if name == parent {
			continue
		}
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			out = append(out, name)
		}
	}
	return out
}
