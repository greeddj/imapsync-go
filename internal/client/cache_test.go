package client

import (
	"slices"
	"sync"
	"testing"
)

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
	}

	for _, name := range []string{"INBOX", "Sent", "Archive/2025"} {
		if !c.hasMailbox(name) {
			t.Errorf("hasMailbox(%q) = false, want true", name)
		}
	}
	if c.hasMailbox("missing") {
		t.Error("hasMailbox(missing) = true")
	}
}

func TestAddMailboxToCache(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	// An account with no mailboxes leaves a nil set; the first add creates it.
	c.addMailboxToCache("X")
	if _, ok := c.mailboxCache.folders["X"]; !ok {
		t.Errorf("addMailboxToCache on an empty cache did not insert: %v", c.mailboxCache.folders)
	}

	c.mailboxCache = mailboxCache{
		folders: map[string]struct{}{"A": {}},
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
	}

	got := c.listSubfoldersFromCache("Archive", "")
	slices.Sort(got)
	want := []string{"Archive/2024", "Archive/2025", "Archive/2025/Q1"}
	if !slices.Equal(got, want) {
		t.Errorf("subfolders = %v, want %v", got, want)
	}

	// Folder with no children.
	got2 := c.listSubfoldersFromCache("Inbox", "")
	if len(got2) != 0 {
		t.Errorf("Inbox subfolders = %v, want []", got2)
	}
}
