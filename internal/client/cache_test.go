package client

import (
	"slices"
	"sync"
	"testing"
)

// newTestClient builds a Client with the bare minimum needed to exercise the
// cache helpers — no real IMAP connection.
func newTestClient() *Client {
	c := &Client{
		folderLocks: make(map[string]*sync.Mutex),
	}
	return c
}

func TestMailboxCacheLookup(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}, "Sent": {}, "Archive/2025": {}},
		delimiter: "/",
		loaded:    true,
	}

	for _, name := range []string{"INBOX", "Sent", "Archive/2025"} {
		ok, err := c.hasMailbox(t.Context(), name)
		if err != nil || !ok {
			t.Errorf("hasMailbox(%q) = %v,%v", name, ok, err)
		}
	}
	if ok, _ := c.hasMailbox(t.Context(), "missing"); ok {
		t.Error("hasMailbox(missing) = true")
	}
}

func TestAddMailboxToCache(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	// Adding before load is a no-op — the next ensureMailboxCache will fetch
	// authoritative data and we don't want to half-populate the set.
	c.addMailboxToCache("X")
	if len(c.mailboxCache.folders) != 0 {
		t.Errorf("addMailboxToCache before load populated cache: %v", c.mailboxCache.folders)
	}

	c.mailboxCache = mailboxCache{
		folders: map[string]struct{}{"A": {}},
		loaded:  true,
	}
	c.addMailboxToCache("B")
	if _, ok := c.mailboxCache.folders["B"]; !ok {
		t.Error("addMailboxToCache after load did not insert")
	}
}

func TestListSubfoldersFromCache(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	c.delimiter = "/"
	c.mailboxCache = mailboxCache{
		folders: map[string]struct{}{
			"Archive":            {},
			"Archive/2024":       {},
			"Archive/2025":       {},
			"Archive/2025/Q1":    {},
			"Inbox":              {},
			"InboxNotASubfolder": {}, // must not be matched by prefix-without-delim
			"Archive2024":        {}, // also a sibling of Archive, not a child
		},
		delimiter: "/",
		loaded:    true,
	}

	got, err := c.listSubfoldersFromCache(t.Context(), "Archive", "")
	if err != nil {
		t.Fatalf("listSubfoldersFromCache: %v", err)
	}
	slices.Sort(got)
	want := []string{"Archive/2024", "Archive/2025", "Archive/2025/Q1"}
	if !slices.Equal(got, want) {
		t.Errorf("subfolders = %v, want %v", got, want)
	}

	// Folder with no children.
	got2, _ := c.listSubfoldersFromCache(t.Context(), "Inbox", "")
	if len(got2) != 0 {
		t.Errorf("Inbox subfolders = %v, want []", got2)
	}
}
