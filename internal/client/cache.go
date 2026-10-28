package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/utf7"
)

// nonExistentAttr is the RFC 5258 attribute for a name that cannot be selected.
// go-imap v1 has no constant for it.
const nonExistentAttr = `\NonExistent`

// mailboxCache holds the result of a single LIST "" "*" call so that the
// many existence checks we do during sync planning don't each cost a
// round-trip. New loads it before returning the Client and CreateMailbox
// adds to it, so readers never see an unloaded cache.
type mailboxCache struct {
	// Each key is the name byte for byte as LIST gave it.
	folders map[string]struct{}
	// Each name here also stays in folders, so parent creation never sends CREATE for it.
	noSelect map[string]struct{}
	// Maps the canonical form of a listed name under the cache delimiter to its listed spelling, only where the two differ. Nil when none differs.
	inboxSpellings map[string]string
	// Empty when no LIST entry names a delimiter: NIL marks a flat namespace (RFC 3501).
	delimiter string
	// Raw names of the latest LIST that are not valid modified UTF-7, sorted. They are not in folders.
	invalid []string
}

// The caller holds mailboxCacheMu. A server can list a name twice, and it gets one warning.
func (m *mailboxCache) setInvalid(names []string) {
	slices.Sort(names)
	m.invalid = slices.Compact(names)
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

// A canonical form with several listed spellings keeps the smallest in byte order, so the answer does not depend on LIST order.
func noteSpelling(spellings map[string]string, listed, delimiter string) map[string]string {
	key := canonicalName(listed, delimiter)
	if key == listed {
		return spellings
	}
	if prior, ok := spellings[key]; ok && prior <= listed {
		return spellings
	}
	if spellings == nil {
		spellings = make(map[string]string)
	}
	spellings[key] = listed
	return spellings
}

// An exact listed name wins, then the listed canonical form, then the indexed spelling. A miss returns the canonical form. The caller holds mailboxCacheMu.
func (m *mailboxCache) resolve(name string) (listed string, ok bool) {
	if _, found := m.folders[name]; found {
		return name, true
	}
	key := canonicalName(name, m.delimiter)
	if key != name {
		if _, found := m.folders[key]; found {
			return key, true
		}
	}
	if spelling, found := m.inboxSpellings[key]; found {
		return spelling, true
	}
	return key, false
}

// ResolveName returns the listed spelling of name, or its canonical form when no listed name matches.
func (c *Client) ResolveName(name string) string {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	listed, _ := c.mailboxCache.resolve(name)
	return listed
}

// RFC 3501 makes attributes case-insensitive.
func noSelectAttr(attrs []string) bool {
	return slices.ContainsFunc(attrs, func(attr string) bool {
		return strings.EqualFold(attr, imap.NoSelectAttr) || strings.EqualFold(attr, nonExistentAttr)
	})
}

// go-imap v1 aborts LIST on one name that is not modified UTF-7. each runs on the reader goroutine under the handler lock: do not block or call the client.
type listHandler struct {
	each    func(*imap.MailboxInfo)
	invalid []string
}

func (h *listHandler) Handle(resp imap.Resp) error {
	name, fields, ok := imap.ParseNamedResp(resp)
	if !ok || name != "LIST" {
		return responses.ErrUnhandled
	}
	if len(fields) < 3 {
		return (&imap.MailboxInfo{}).Parse(fields)
	}

	raw, err := imap.ParseString(fields[2])
	if err != nil {
		return err
	}
	info := &imap.MailboxInfo{}
	if err := info.Parse([]any{fields[0], fields[1], raw}); err != nil {
		if errors.Is(err, utf7.ErrInvalidUTF7) {
			h.invalid = append(h.invalid, raw)
			return nil
		}
		return err
	}
	h.each(info)
	return nil
}

func listAll(cli *imapclient.Client, each func(*imap.MailboxInfo)) (invalid []string, err error) {
	if state := cli.State(); state != imap.AuthenticatedState && state != imap.SelectedState {
		return nil, imapclient.ErrNotLoggedIn
	}
	h := &listHandler{each: each}
	status, err := cli.Execute(&commands.List{Reference: "", Mailbox: "*"}, h)
	if err != nil {
		return nil, err
	}
	return h.invalid, status.Err()
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
	var invalid []string
	delim := ""

	err := c.safeCall(func(cli *imapclient.Client) error {
		folders = make(map[string]struct{})
		noSelect = nil
		invalid = nil
		delim = ""
		var err error
		invalid, err = listAll(cli, func(m *imap.MailboxInfo) {
			folders[m.Name] = struct{}{}
			if noSelectAttr(m.Attributes) {
				if noSelect == nil {
					noSelect = make(map[string]struct{})
				}
				noSelect[m.Name] = struct{}{}
			}
			if delim == "" && m.Delimiter != "" {
				delim = m.Delimiter
			}
		})
		if err != nil {
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

	// Lookups and folder locks fold INBOX with the cache delimiter. It is known only after LIST, because an entry before the first named delimiter can answer NIL.
	var spellings map[string]string
	for name := range folders {
		spellings = noteSpelling(spellings, name, delim)
	}

	c.mailboxCacheMu.Lock()
	c.mailboxCache.folders = folders
	c.mailboxCache.noSelect = noSelect
	c.mailboxCache.inboxSpellings = spellings
	c.mailboxCache.delimiter = delim
	c.mailboxCache.setInvalid(invalid)
	c.mailboxCacheMu.Unlock()
	return nil
}

// hasMailbox reports whether name exists, consulting the cache.
func (c *Client) hasMailbox(name string) bool {
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	_, ok := c.mailboxCache.resolve(name)
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
	c.mailboxCache.inboxSpellings = noteSpelling(c.mailboxCache.inboxSpellings, name, c.mailboxCache.delimiter)
}

// listSubfoldersFromCache returns the listed names that are children of parent. It
// costs no round-trip and returns nil when neither delimiter is set, as on a flat server.
func (c *Client) listSubfoldersFromCache(parent, delimiter string) []string {
	if delimiter == "" {
		delimiter = c.delimiter
	}
	if delimiter == "" {
		return nil
	}
	c.mailboxCacheMu.RLock()
	defer c.mailboxCacheMu.RUnlock()
	prefix := canonicalName(parent, delimiter) + delimiter
	out := make([]string, 0)
	for name := range c.mailboxCache.folders {
		canonical := canonicalName(name, delimiter)
		if len(canonical) > len(prefix) && canonical[:len(prefix)] == prefix {
			out = append(out, name)
		}
	}
	return out
}
