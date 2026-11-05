package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-imap"
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

func Test_mailboxCache_add(t *testing.T) {
	t.Parallel()

	c := newTestClient()
	// An account with no mailboxes leaves a nil set; the first add creates it.
	c.mailboxCache.add("X")
	if _, ok := c.mailboxCache.folders["X"]; !ok {
		t.Errorf("add on an empty cache did not insert: %v", c.mailboxCache.folders)
	}

	c.mailboxCache = mailboxCache{
		folders: map[string]struct{}{"A": {}},
	}
	c.mailboxCache.add("B")
	if _, ok := c.mailboxCache.folders["B"]; !ok {
		t.Error("add after load did not insert")
	}
}

func Test_mailboxCache_add_When_TwoSpellingsShareOneCanonicalForm_Then_SmallerWinsInAnyOrder(t *testing.T) {
	t.Parallel()

	for _, order := range [][]string{{"inbox/New", "Inbox/New"}, {"Inbox/New", "inbox/New"}} {
		c := newTestClient()
		c.mailboxCache = mailboxCache{folders: map[string]struct{}{"INBOX": {}}, delimiter: "/"}

		for _, name := range order {
			c.mailboxCache.add(name)
		}

		if got := c.ResolveName("INBOX/New"); got != "Inbox/New" {
			t.Errorf("after adding %q, ResolveName(INBOX/New) = %q, want Inbox/New", order, got)
		}
		if got, want := c.mailboxCache.folders, map[string]struct{}{"INBOX": {}, "inbox/New": {}, "Inbox/New": {}}; !maps.Equal(got, want) {
			t.Errorf("after adding %q, folders = %v, want %v", order, got, want)
		}
	}
}

func Test_markRooted(t *testing.T) {
	t.Parallel()

	type mark struct{ level, parent string }
	tests := []struct {
		cache mailboxCache
		name  string
		marks []mark
		want  []string
	}{
		{
			name:  "a first level that the cache holds",
			cache: mailboxCache{folders: map[string]struct{}{"A": {}}},
			marks: []mark{{"A", ""}},
			want:  []string{"A"},
		},
		{
			name:  "a level that the cache lacks",
			cache: mailboxCache{folders: map[string]struct{}{"A": {}}},
			marks: []mark{{"B", ""}},
		},
		{
			name:  "an empty cache",
			marks: []mark{{"A", ""}},
		},
		{
			name:  "a child before its parent",
			cache: mailboxCache{folders: map[string]struct{}{"A": {}, "A/B": {}}, delimiter: "/"},
			marks: []mark{{"A/B", "A"}},
		},
		{
			name:  "a child after its parent",
			cache: mailboxCache{folders: map[string]struct{}{"A": {}, "A/B": {}}, delimiter: "/"},
			marks: []mark{{"A", ""}, {"A/B", "A"}},
			want:  []string{"A", "A/B"},
		},
		{
			name:  "a child that the cache lacks under a rooted parent",
			cache: mailboxCache{folders: map[string]struct{}{"A": {}}, delimiter: "/"},
			marks: []mark{{"A", ""}, {"A/B", "A"}},
			want:  []string{"A"},
		},
		{
			name:  "a listed child under a parent that is not rooted",
			cache: mailboxCache{folders: map[string]struct{}{"A/B": {}}, delimiter: "/"},
			marks: []mark{{"A/B", "A"}},
		},
		{
			name:  "an INBOX spelling that folds to a listed name",
			cache: mailboxCache{folders: map[string]struct{}{"INBOX": {}}, delimiter: "/"},
			marks: []mark{{"inbox", ""}},
			want:  []string{"inbox"},
		},
		{
			name: "an INBOX child that the index maps to its listed spelling",
			cache: mailboxCache{
				folders:        map[string]struct{}{"INBOX": {}, "Inbox/Old": {}},
				inboxSpellings: map[string]string{"INBOX/Old": "Inbox/Old"},
				delimiter:      "/",
			},
			marks: []mark{{"INBOX", ""}, {"INBOX/Old", "INBOX"}},
			want:  []string{"INBOX", "INBOX/Old"},
		},
		{
			name: "a name that cannot hold messages",
			cache: mailboxCache{
				folders:  map[string]struct{}{"[Gmail]": {}},
				noSelect: map[string]struct{}{"[Gmail]": {}},
			},
			marks: []mark{{"[Gmail]", ""}},
			want:  []string{"[Gmail]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := tt.cache
			for _, m := range tt.marks {
				cache.markRooted(m.level, m.parent)
			}

			if got := slices.Sorted(maps.Keys(cache.rooted)); !slices.Equal(got, tt.want) {
				t.Errorf("rooted = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_addRooted(t *testing.T) {
	t.Parallel()

	t.Run("a name under a rooted parent is cached and rooted", func(t *testing.T) {
		t.Parallel()

		c := newTestClient()
		c.mailboxCache = mailboxCache{folders: map[string]struct{}{"A": {}}, delimiter: "/"}
		c.markRooted("A", "")

		c.addRooted("A/B", "A")

		if !c.hasMailbox("A/B") {
			t.Error("hasMailbox(A/B) = false, want true")
		}
		if got := slices.Sorted(maps.Keys(c.mailboxCache.rooted)); !slices.Equal(got, []string{"A", "A/B"}) {
			t.Errorf("rooted = %q, want [A A/B]", got)
		}
	})

	t.Run("a name under a parent that is not rooted is cached only", func(t *testing.T) {
		t.Parallel()

		c := newTestClient()
		c.mailboxCache = mailboxCache{folders: map[string]struct{}{}, delimiter: "/"}

		c.addRooted("A/B", "A")

		if !c.hasMailbox("A/B") {
			t.Error("hasMailbox(A/B) = false, want true")
		}
		if len(c.mailboxCache.rooted) != 0 {
			t.Errorf("rooted = %v, want none", c.mailboxCache.rooted)
		}
	})
}

func Test_loadMailboxCache_When_ReloadedAfterACreateWalk_Then_RootedIsEmpty(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}}})
	if _, _, err := c.CreateMailbox(t.Context(), "A/B"); err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if got := slices.Sorted(maps.Keys(c.mailboxCache.rooted)); !slices.Equal(got, []string{"A", "A/B"}) {
		t.Fatalf("rooted after the walk = %q, want [A A/B]", got)
	}

	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("loadMailboxCache: %v", err)
	}

	if len(c.mailboxCache.rooted) != 0 {
		t.Errorf("rooted after the reload = %v, want none: the new LIST decides what the cache holds", c.mailboxCache.rooted)
	}
	if c.hasMailbox("A") {
		t.Error("hasMailbox(A) = true after a LIST that lacks it")
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

	t.Run("no delimiter means no children", func(t *testing.T) {
		t.Parallel()

		flat := newTestClient()
		flat.mailboxCache = mailboxCache{folders: c.mailboxCache.folders}

		for _, parent := range []string{"Archive", "Inbox"} {
			if got := flat.listSubfoldersFromCache(parent, ""); len(got) != 0 {
				t.Errorf("%s subfolders = %v, want none: a name that only shares the prefix is not a child", parent, got)
			}
		}
	})
}

func Test_New_When_ListStalls_Then_ContextCancelReturns(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LIST"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var got *Client
	errc := make(chan error, 1)
	go func() {
		var err error
		got, err = New(ctx, srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
		errc <- err
	}()

	awaitSignal(t, st.reached, "the server to receive LIST")
	cancel()

	err := awaitError(t, errc, "New")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("New error = %v, want context.Canceled", err)
	}
	if got != nil {
		got.Cancel()
		t.Error("New returned a client alongside the error")
	}
}

// listAnsweredContext reports cancellation once the server has seen LIST. Its Done
// channel stays nil, so the client is not cancelled and the end lands after the reply.
type listAnsweredContext struct {
	context.Context
	srv *fakeServer
}

func (c listAnsweredContext) Done() <-chan struct{} { return nil }

func (c listAnsweredContext) Err() error {
	if c.srv.callCount("LIST") > 0 {
		return context.Canceled
	}
	return nil
}

func Test_loadMailboxCache_When_ContextEndsAfterList_Then_ReturnsErrorAndKeepsCache(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{folders: map[string]struct{}{"Sentinel": {}}, delimiter: "."}

	err := c.loadMailboxCache(listAnsweredContext{Context: t.Context(), srv: srv})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("loadMailboxCache error = %v, want context.Canceled", err)
	}
	if got := srv.callCount("LIST"); got != 1 {
		t.Errorf("LIST count = %d, want 1 (the context must end after the reply)", got)
	}
	if !c.hasMailbox("Sentinel") || c.hasMailbox("INBOX") {
		t.Error("cache folders changed although the context ended before the load finished")
	}
	if c.mailboxCache.delimiter != "." {
		t.Errorf("delimiter = %q, want %q", c.mailboxCache.delimiter, ".")
	}
}

// endedContext reports cancellation through Err only. Its Done channel stays nil, so
// nothing cancels the client and only the entry check can stop the LIST.
type endedContext struct{ context.Context }

func (endedContext) Done() <-chan struct{} { return nil }

func (endedContext) Err() error { return context.Canceled }

func Test_loadMailboxCache_When_ContextAlreadyEnded_Then_SendsNoList(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{folders: map[string]struct{}{"Sentinel": {}}, delimiter: "."}

	err := c.loadMailboxCache(endedContext{Context: t.Context()})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("loadMailboxCache error = %v, want context.Canceled", err)
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST count = %d, want 0", got)
	}
	if !c.hasMailbox("Sentinel") || c.mailboxCache.delimiter != "." {
		t.Error("cache changed although the context had ended before the load started")
	}
}

func Test_loadMailboxCache_When_ContextExpiresDuringList_Then_ReturnsItsError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(verbIs("LIST"))
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newClientWithFake(t, srv)

	ctx := newFiredContext(t.Context())
	errc := make(chan error, 1)
	go func() {
		errc <- c.loadMailboxCache(ctx)
	}()

	awaitSignal(t, st.reached, "the server to receive LIST")
	ctx.fire()

	err := awaitError(t, errc, "loadMailboxCache")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("loadMailboxCache error = %v, want %v, not the context.Canceled safeCall reports for a cancelled client", err, context.DeadlineExceeded)
	}
}

func Test_loadMailboxCache_When_ListNamesNoDelimiter_Then_DelimiterStaysEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    string
		folders []scriptFolder
	}{
		{
			name:    "every entry answers NIL",
			folders: []scriptFolder{{name: "INBOX", delim: "NIL"}, {name: "Work/2023", delim: "NIL"}},
		},
		{
			name:    "NIL first and a dot later",
			folders: []scriptFolder{{name: "INBOX", delim: "NIL"}, {name: "Work.2023", delim: `"."`}},
			want:    ".",
		},
		{
			name:    "a dot first and NIL later",
			folders: []scriptFolder{{name: "Work.2023", delim: `"."`}, {name: "INBOX", delim: "NIL"}},
			want:    ".",
		},
		{
			name: "no entries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newLoadedClient(t, &mailboxScript{folders: tt.folders})

			if c.mailboxCache.delimiter != tt.want {
				t.Errorf("cache delimiter = %q, want %q", c.mailboxCache.delimiter, tt.want)
			}
			if got := c.GetDelimiter(); got != tt.want {
				t.Errorf("GetDelimiter = %q, want %q", got, tt.want)
			}
			if got, want := len(c.mailboxCache.folders), len(tt.folders); got != want {
				t.Errorf("folders = %v, want %d names", c.mailboxCache.folders, want)
			}
			if n := srv.callCount("LIST"); n != 1 {
				t.Errorf("LIST count = %d, want 1: the delimiter comes from the same LIST", n)
			}
		})
	}
}

// newLoadedClient connects through New, so the mailbox cache comes from the LIST
// that production code sends.
func newLoadedClient(t *testing.T, script *mailboxScript) (*fakeServer, *Client) {
	t.Helper()

	srv := newFakeServer(t)
	srv.addConnHandler(script.handler(srv))
	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})
	return srv, c
}

func Test_noSelectAttr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		attrs []string
		want  bool
	}{
		{name: "no attributes"},
		{name: "empty list", attrs: []string{}},
		{name: "only unrelated attributes", attrs: []string{`\HasNoChildren`, `\Marked`, `\Sent`}},
		{name: "Noselect", attrs: []string{`\Noselect`}, want: true},
		{name: "Noselect in another case", attrs: []string{`\NoSelect`}, want: true},
		{name: "NonExistent", attrs: []string{`\NonExistent`}, want: true},
		{name: "NonExistent in lower case", attrs: []string{`\nonexistent`}, want: true},
		{name: "Noselect among others", attrs: []string{`\HasChildren`, `\Noselect`}, want: true},
		{name: "a custom attribute that only starts alike", attrs: []string{`\NoselectLater`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := noSelectAttr(tt.attrs); got != tt.want {
				t.Errorf("noSelectAttr(%q) = %t, want %t", tt.attrs, got, tt.want)
			}
		})
	}
}

func Test_loadMailboxCache_When_ListMarksNoSelect_Then_IsNoSelectReportsIt(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "[Gmail]", attrs: `\HasChildren \Noselect`},
		{name: "Dir", attrs: `\NoSelect`},
		{name: "Ghost", attrs: `\NonExistent \Noselect`},
		{name: "ghost2", attrs: `\nonexistent`},
		{name: "INBOX", attrs: `\HasNoChildren`},
		{name: "Plain"},
	}})

	for name, want := range map[string]bool{
		"[Gmail]": true, "Dir": true, "Ghost": true, "ghost2": true,
		"INBOX": false, "Plain": false, "Unlisted": false,
	} {
		if got := c.IsNoSelect(name); got != want {
			t.Errorf("IsNoSelect(%q) = %t, want %t", name, got, want)
		}
	}
	listed := []string{"Dir", "Ghost", "INBOX", "Plain", "[Gmail]", "ghost2"}
	for _, name := range listed {
		if !c.hasMailbox(name) {
			t.Errorf("hasMailbox(%q) = false, want true: a \\Noselect name stays in the cache", name)
		}
	}
	if got := c.MailboxNames(); !slices.Equal(got, listed) {
		t.Errorf("MailboxNames = %v, want %v", got, listed)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1: the attributes come from the same LIST", n)
	}
}

func Test_loadMailboxCache_When_NoNameIsMarked_Then_NoSelectSetStaysNil(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}, {name: "Sent"}}})

	if c.mailboxCache.noSelect != nil {
		t.Errorf("noSelect = %v, want nil when the server marks no name", c.mailboxCache.noSelect)
	}
	if c.IsNoSelect("INBOX") {
		t.Error("IsNoSelect(INBOX) = true, want false")
	}
}

func Test_loadMailboxCache_When_Reloaded_Then_NoSelectSetIsReplaced(t *testing.T) {
	t.Parallel()

	_, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{{name: "Old"}}})
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"Old": {}},
		noSelect:  map[string]struct{}{"Old": {}},
		delimiter: "/",
	}
	if !c.IsNoSelect("Old") {
		t.Fatal("precondition: IsNoSelect(Old) = false")
	}

	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("loadMailboxCache: %v", err)
	}

	if c.IsNoSelect("Old") {
		t.Error("IsNoSelect(Old) = true after a reload that lists it without attributes")
	}
	if !c.hasMailbox("Old") {
		t.Error("hasMailbox(Old) = false after the reload")
	}
}

func Test_loadMailboxCache_When_ListIsRetried_Then_NamesOfTheFailedAttemptAreDropped(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		// The connection drops after one marked name, before the tagged reply.
		writeLine(conn, `* LIST (\Noselect) "/" "Stale"`)
		return true, false
	}))
	srv.addConnHandler(interceptHandler(srv, serveNormally))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if n := srv.callCount("LIST"); n != 2 {
		t.Fatalf("LIST count = %d, want 2 (the dropped attempt and its retry)", n)
	}
	if c.IsNoSelect("Stale") || c.hasMailbox("Stale") {
		t.Error("the cache holds Stale from the attempt that failed")
	}
	if !c.hasMailbox("INBOX") {
		t.Error("hasMailbox(INBOX) = false after the retry")
	}
}

func Test_loadMailboxCache_When_ListGivesMixedCaseInboxChild_Then_CacheKeepsListedName(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX"},
		{name: "Inbox/Work"},
		{name: "Inbox Archive"},
	}})

	if got, want := c.MailboxNames(), []string{"INBOX", "Inbox Archive", "Inbox/Work"}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %q, want %q: every name keeps the bytes that LIST gave", got, want)
	}
	for _, parent := range []string{"INBOX", "inbox"} {
		got, err := c.ListSubfolders(t.Context(), parent, "")
		if err != nil {
			t.Fatalf("ListSubfolders(%q): %v", parent, err)
		}
		if want := []string{"Inbox/Work"}; !slices.Equal(got, want) {
			t.Errorf("ListSubfolders(%q) = %q, want %q", parent, got, want)
		}
	}
	if exists, err := c.MailboxExists(t.Context(), "INBOX/Work"); err != nil || !exists {
		t.Errorf("MailboxExists(INBOX/Work) = %t, %v, want true", exists, err)
	}
	if got := c.ResolveName("INBOX/Work"); got != "Inbox/Work" {
		t.Errorf("ResolveName(INBOX/Work) = %q, want the listed Inbox/Work", got)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
}

func Test_loadMailboxCache_When_ListGivesTwoSpellingsOfAnInboxChild_Then_CacheKeepsAllNames(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX"},
		{name: "INBOX/Sub"},
		{name: "Inbox/Sub", attrs: `\Noselect`},
		{name: "Inbox/Sub/Deep"},
	}})

	all := []string{"INBOX", "INBOX/Sub", "Inbox/Sub", "Inbox/Sub/Deep"}
	if got := c.MailboxNames(); !slices.Equal(got, all) {
		t.Errorf("MailboxNames = %q, want %q: two spellings are two folders", got, all)
	}
	got, err := c.ListSubfolders(t.Context(), "INBOX", "")
	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	slices.Sort(got)
	if want := []string{"INBOX/Sub", "Inbox/Sub", "Inbox/Sub/Deep"}; !slices.Equal(got, want) {
		t.Errorf("ListSubfolders(INBOX) = %q, want %q", got, want)
	}
	if !c.IsNoSelect("Inbox/Sub") {
		t.Error("IsNoSelect(Inbox/Sub) = false, want true: the server marked that spelling")
	}
	if c.IsNoSelect("INBOX/Sub") {
		t.Error("IsNoSelect(INBOX/Sub) = true, want false: the mark belongs to the other spelling")
	}
	for _, name := range all {
		if got := c.ResolveName(name); got != name {
			t.Errorf("ResolveName(%q) = %q, want the listed spelling unchanged", name, got)
		}
	}
	if got := c.ResolveName("inbox/Sub"); got != "INBOX/Sub" {
		t.Errorf("ResolveName(inbox/Sub) = %q, want INBOX/Sub: the canonical spelling wins", got)
	}
}

func Test_loadMailboxCache_When_EntryNamesAnotherDelimiter_Then_InboxLevelFollowsTheCacheDelimiter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		folders []scriptFolder
	}{
		{
			name: "NIL entry after a dot entry",
			folders: []scriptFolder{
				{name: "Work", delim: `"."`},
				{name: "inbox.Sub", delim: "NIL", attrs: `\Noselect`},
			},
		},
		{
			name: "NIL entry before a dot entry",
			folders: []scriptFolder{
				{name: "inbox.Sub", delim: "NIL", attrs: `\Noselect`},
				{name: "Work", delim: `"."`},
			},
		},
		{
			name: "slash entry on a dot server",
			folders: []scriptFolder{
				{name: "Work", delim: `"."`},
				{name: "inbox.Sub", delim: `"/"`, attrs: `\Noselect`},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newLoadedClient(t, &mailboxScript{folders: tt.folders})

			if c.mailboxCache.delimiter != "." {
				t.Fatalf("cache delimiter = %q, want a dot", c.mailboxCache.delimiter)
			}
			for _, input := range []string{"inbox.Sub", "INBOX.Sub", "Inbox.Sub"} {
				if exists, err := c.MailboxExists(t.Context(), input); err != nil || !exists {
					t.Errorf("MailboxExists(%q) = %t, %v, want true", input, exists, err)
				}
				if got := c.ResolveName(input); got != "inbox.Sub" {
					t.Errorf("ResolveName(%q) = %q, want the listed inbox.Sub", input, got)
				}
				if !c.IsNoSelect(input) {
					t.Errorf("IsNoSelect(%q) = false, want true: the server marked inbox.Sub", input)
				}
				if c.folderLock(input) != c.folderLock("inbox.Sub") {
					t.Errorf("folderLock(%q) is not the lock of inbox.Sub: two creates could race", input)
				}
			}
			got, err := c.ListSubfolders(t.Context(), "INBOX", "")
			if err != nil {
				t.Fatalf("ListSubfolders: %v", err)
			}
			if want := []string{"inbox.Sub"}; !slices.Equal(got, want) {
				t.Errorf("ListSubfolders(INBOX) = %q, want %q", got, want)
			}
			if got, want := c.MailboxNames(), []string{"Work", "inbox.Sub"}; !slices.Equal(got, want) {
				t.Errorf("MailboxNames = %q, want %q: every name keeps the bytes that LIST gave", got, want)
			}
			if n := srv.callCount("LIST"); n != 1 {
				t.Errorf("LIST count = %d, want 1", n)
			}
		})
	}
}

func Test_loadMailboxCache_When_SlashChildIsListedOnADotServer_Then_InboxLevelDoesNotFold(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "Work", delim: `"."`},
		{name: "inbox/Sub", delim: `"/"`},
	}})

	if got := c.ResolveName("INBOX/Sub"); got != "INBOX/Sub" {
		t.Errorf("ResolveName(INBOX/Sub) = %q, want INBOX/Sub: the dot server does not fold a slash child", got)
	}
	if exists, err := c.MailboxExists(t.Context(), "INBOX/Sub"); err != nil || exists {
		t.Errorf("MailboxExists(INBOX/Sub) = %t, %v, want false", exists, err)
	}
	if exists, err := c.MailboxExists(t.Context(), "inbox/Sub"); err != nil || !exists {
		t.Errorf("MailboxExists(inbox/Sub) = %t, %v, want true: the listed bytes match", exists, err)
	}
	if got := c.ResolveName("inbox/Sub"); got != "inbox/Sub" {
		t.Errorf("ResolveName(inbox/Sub) = %q, want the listed inbox/Sub", got)
	}
	got, err := c.ListSubfolders(t.Context(), "INBOX", "")
	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSubfolders(INBOX) = %q, want none: the slash child is not a child under the dot", got)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
}

func Test_loadMailboxCache_When_ListIsRetried_Then_SpellingsOfTheFailedAttemptAreDropped(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		// The connection drops after one name in another letter case, before the tagged reply.
		writeLine(conn, `* LIST () "/" "Inbox/Stale"`)
		return true, false
	}))
	srv.addConnHandler(interceptHandler(srv, serveNormally))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if n := srv.callCount("LIST"); n != 2 {
		t.Fatalf("LIST count = %d, want 2 (the dropped attempt and its retry)", n)
	}
	if c.hasMailbox("INBOX/Stale") {
		t.Error("the cache finds INBOX/Stale through the spelling of the attempt that failed")
	}
	if got := c.ResolveName("INBOX/Stale"); got != "INBOX/Stale" {
		t.Errorf("ResolveName(INBOX/Stale) = %q, want the canonical form: the retry did not list it", got)
	}
}

func Test_canonicalName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		delimiter string
		want      string
	}{
		{name: "upper case on a slash server", input: "INBOX", delimiter: "/", want: "INBOX"},
		{name: "lower case on a slash server", input: "inbox", delimiter: "/", want: "INBOX"},
		{name: "title case on a dot server", input: "Inbox", delimiter: ".", want: "INBOX"},
		{name: "mixed case on a flat server", input: "iNbOx", delimiter: "", want: "INBOX"},
		{name: "child after the delimiter", input: "inbox/Work", delimiter: "/", want: "INBOX/Work"},
		{name: "child of a dot server", input: "inbox.Work", delimiter: ".", want: "INBOX.Work"},
		{name: "child that uses the other delimiter", input: "inbox/Work", delimiter: ".", want: "inbox/Work"},
		{name: "child on a flat server", input: "inbox/Work", delimiter: "", want: "inbox/Work"},
		{name: "delimiter with nothing after it", input: "inbox/", delimiter: "/", want: "INBOX/"},
		{name: "second level keeps its letter case", input: "inbox/work", delimiter: "/", want: "INBOX/work"},
		{name: "name that continues the word", input: "INBOXES", delimiter: "/", want: "INBOXES"},
		{name: "lower case name that continues the word", input: "inboxes", delimiter: "/", want: "inboxes"},
		{name: "name with a space after INBOX", input: "Inbox Archive", delimiter: "/", want: "Inbox Archive"},
		{name: "INBOX below another folder", input: "Archive/inbox", delimiter: "/", want: "Archive/inbox"},
		{name: "name shorter than INBOX", input: "inbo", delimiter: "/", want: "inbo"},
		{name: "empty name", input: "", delimiter: "/", want: ""},
		{name: "dotless i", input: "ınbox", delimiter: "/", want: "ınbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := canonicalName(tt.input, tt.delimiter); got != tt.want {
				t.Errorf("canonicalName(%q, %q) = %q, want %q", tt.input, tt.delimiter, got, tt.want)
			}
		})
	}
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func Test_canonicalName_When_NameNeedsNoChange_Then_AllocatesNothing(t *testing.T) {
	for _, name := range []string{"INBOX", "INBOX/Work", "Archive/2024"} {
		var got string
		allocs := testing.AllocsPerRun(100, func() { got = canonicalName(name, "/") })
		if allocs != 0 {
			t.Errorf("canonicalName(%q) allocates %v times, want 0", name, allocs)
		}
		if got != name {
			t.Errorf("canonicalName(%q) = %q, want the input", name, got)
		}
	}
}

func Test_ResolveName(t *testing.T) {
	t.Parallel()

	type resolution struct{ input, want string }
	slashNames := []string{"INBOX", "Inbox/Work", "inbox/Work", "INBOX/Both", "Inbox/Both", "Sent"}
	slashWant := []resolution{
		{input: "Inbox/Work", want: "Inbox/Work"},
		{input: "inbox/Work", want: "inbox/Work"},
		{input: "INBOX/Work", want: "Inbox/Work"},
		{input: "iNbOx/Work", want: "Inbox/Work"},
		{input: "inbox/Both", want: "INBOX/Both"},
		{input: "inbox/New", want: "INBOX/New"},
		{input: "sent", want: "sent"},
		{input: "inbox", want: "INBOX"},
	}
	reversedNames := slices.Clone(slashNames)
	slices.Reverse(reversedNames)
	tests := []struct {
		name   string
		delim  string
		listed []string
		want   []resolution
	}{
		{
			name: "dot server folds only dot levels", delim: `"."`, listed: []string{"INBOX"},
			want: []resolution{
				{input: "inbox.Work", want: "INBOX.Work"},
				{input: "inbox/Work", want: "inbox/Work"},
				{input: "Inbox", want: "INBOX"},
			},
		},
		{name: "slash server with two spellings", listed: slashNames, want: slashWant},
		{name: "slash server listing the spellings in reverse order", listed: reversedNames, want: slashWant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			folders := make([]scriptFolder, len(tt.listed))
			for i, name := range tt.listed {
				folders[i] = scriptFolder{name: name, delim: tt.delim}
			}
			_, c := newLoadedClient(t, &mailboxScript{folders: folders})

			for _, r := range tt.want {
				if got := c.ResolveName(r.input); got != r.want {
					t.Errorf("ResolveName(%q) = %q, want %q", r.input, got, r.want)
				}
			}
		})
	}
}

// Not parallel: AllocsPerRun counts allocations of the whole process. MailboxExists is left out because withCancel allocates; hasMailbox is the lookup it runs.
func Test_ResolveName_When_NameNeedsNoFolding_Then_LookupsAllocateNothing(t *testing.T) {
	c := newTestClient()
	c.mailboxCache = mailboxCache{
		folders:        map[string]struct{}{"INBOX": {}, "Inbox/Work": {}, "Archive/2024": {}},
		noSelect:       map[string]struct{}{"Archive/2024": {}},
		inboxSpellings: map[string]string{"INBOX/Work": "Inbox/Work"},
		delimiter:      "/",
	}

	for _, name := range []string{"INBOX", "Inbox/Work", "Archive/2024", "Missing/Name", "Archive"} {
		var resolved string
		var found, noSelect bool
		allocs := testing.AllocsPerRun(100, func() {
			resolved = c.ResolveName(name)
			found = c.hasMailbox(name)
			noSelect = c.IsNoSelect(name)
		})

		if allocs != 0 {
			t.Errorf("lookups of %q allocate %v times, want 0", name, allocs)
		}
		if resolved != name {
			t.Errorf("ResolveName(%q) = %q, want the input", name, resolved)
		}
		if wantFound := name != "Missing/Name" && name != "Archive"; found != wantFound {
			t.Errorf("hasMailbox(%q) = %t, want %t", name, found, wantFound)
		}
		if wantNoSelect := name == "Archive/2024"; noSelect != wantNoSelect {
			t.Errorf("IsNoSelect(%q) = %t, want %t", name, noSelect, wantNoSelect)
		}
	}
}

// Test_New_When_ListHoldsNamesThatAreNotModifiedUTF7_Then_CacheLeavesThemOut sends each bad name before a good one, so a later good name proves the LIST went on.
func Test_New_When_ListHoldsNamesThatAreNotModifiedUTF7_Then_CacheLeavesThemOut(t *testing.T) {
	t.Parallel()

	const (
		shifted = "&BBIERQQ+BDQETwRJBDgENQ-"
		decoded = "Входящие"
	)
	srv := newFakeServer(t)
	srv.addConnHandler((&mailboxScript{folders: []scriptFolder{
		{name: "Q&A"},
		{name: "INBOX"},
		{name: "Входящие"},
		{name: "AT&T"},
		{name: "R&D-x"},
		{name: "Entw\xfcrfe"},
		{name: "\x1b[2J\u202eevil"},
		{name: shifted},
		{name: "Work"},
		{name: "Work/Q&A"},
	}}).handler(srv))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{Label: "src", DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if got := srv.callCount("LIST"); got != 1 {
		t.Errorf("LIST count = %d, want 1", got)
	}
	if got := srv.callCount("LOGOUT"); got != 0 {
		t.Errorf("LOGOUT count = %d, want 0: New must not drop a connection over a bad name", got)
	}
	if got, want := c.MailboxNames(), []string{"INBOX", "Work", decoded}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %q, want %q", got, want)
	}
	want := []string{"Q&A", "Входящие", "AT&T", "R&D-x", "Entw\ufffdrfe", "\x1b[2J\u202eevil", "Work/Q&A"}
	slices.Sort(want)
	if got := c.InvalidMailboxNames(); !slices.Equal(got, want) {
		t.Errorf("InvalidMailboxNames = %q, want %q", got, want)
	}
	if exists, err := c.MailboxExists(t.Context(), "Q&A"); err != nil || exists {
		t.Errorf("MailboxExists(Q&A) = %t, %v, want false", exists, err)
	}
	if got, err := c.ListSubfolders(t.Context(), "Work", ""); err != nil || len(got) != 0 {
		t.Errorf("ListSubfolders(Work) = %q, %v, want none: Work/Q&A is left out", got, err)
	}
	if got := c.GetDelimiter(); got != "/" {
		t.Errorf("GetDelimiter = %q, want /", got)
	}
}

func Test_loadMailboxCache_When_ListSendsInvalidNameAsLiteral_Then_LeavesItOutAndKeepsTheNextEntry(t *testing.T) {
	t.Parallel()

	const literal = "Entw\xfcrfe"
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		writeLine(conn, "* LIST () \"/\" {%d}\r\n%s", len(literal), literal)
		writeLine(conn, `* LIST () "/" "INBOX"`)
		writeLine(conn, "%s OK LIST completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("loadMailboxCache: %v", err)
	}

	if got := c.InvalidMailboxNames(); !slices.Equal(got, []string{literal}) {
		t.Errorf("InvalidMailboxNames = %q, want [%q] byte for byte", got, literal)
	}
	if got := c.MailboxNames(); !slices.Equal(got, []string{"INBOX"}) {
		t.Errorf("MailboxNames = %q, want [INBOX]", got)
	}
}

func Test_loadMailboxCache_When_ListIsRetried_Then_NamesAndInvalidNamesOfTheFailedAttemptAreDropped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fail func(conn net.Conn, tag string) (keep bool)
		name string
	}{
		{
			name: "the connection drops before the tagged reply",
			fail: func(net.Conn, string) bool { return false },
		},
		{
			name: "the server refuses with a transient reply",
			fail: func(conn net.Conn, tag string) bool {
				writeLine(conn, "%s NO Not logged in", tag)
				return true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
				if cmd.verb != "LIST" {
					return false, true
				}
				writeLine(conn, `* LIST () "/" "Stale"`)
				writeLine(conn, `* LIST () "/" "Q&A"`)
				return true, tt.fail(conn, cmd.tag)
			}))
			srv.addConnHandler(interceptHandler(srv, serveNormally))

			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				c.Cancel()
				_ = c.Logout()
			})

			if n := srv.callCount("LIST"); n != 2 {
				t.Fatalf("LIST count = %d, want 2 (the failed attempt and its retry)", n)
			}
			if got := c.InvalidMailboxNames(); len(got) != 0 {
				t.Errorf("InvalidMailboxNames = %q, want none: the retry lists only INBOX", got)
			}
			if got := c.MailboxNames(); !slices.Equal(got, []string{"INBOX"}) {
				t.Errorf("MailboxNames = %q, want [INBOX]: the retry lists only INBOX", got)
			}
			if !c.hasMailbox("INBOX") {
				t.Error("hasMailbox(INBOX) = false after the retry")
			}
		})
	}
}

func Test_New_When_ListEntryLacksAName_Then_FailsAsBefore(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		writeLine(conn, `* LIST () "/"`)
		writeLine(conn, "%s OK LIST completed", cmd.tag)
		return true, true
	}))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{Label: "src", DialTimeout: 5 * time.Second})
	if err == nil {
		c.Cancel()
		t.Fatal("New returned nil error, want the malformed entry to stop the LIST")
	}
	for _, want := range []string{"failed to load mailbox list", "[src] list mailboxes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func Test_InvalidMailboxNames_When_ListNamesOneInvalidNameTwice_Then_ReportsItOnceAndReturnsACopy(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX"}, {name: "Q&A"}, {name: "AT&T"}, {name: "Q&A"},
	}})

	got := c.InvalidMailboxNames()
	if want := []string{"AT&T", "Q&A"}; !slices.Equal(got, want) {
		t.Fatalf("InvalidMailboxNames = %q, want %q", got, want)
	}
	got[0] = "changed"
	if again := c.InvalidMailboxNames(); again[0] != "AT&T" {
		t.Errorf("InvalidMailboxNames after a caller edit = %q, want the cache untouched", again)
	}
}

func Test_loadMailboxCache_When_Reloaded_Then_InvalidNamesAreReplaced(t *testing.T) {
	t.Parallel()

	var lists atomic.Int32
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		writeLine(conn, `* LIST () "/" "INBOX"`)
		if lists.Add(1) == 1 {
			writeLine(conn, `* LIST () "/" "Q&A"`)
		}
		writeLine(conn, "%s OK LIST completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)
	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("first loadMailboxCache: %v", err)
	}
	if got := c.InvalidMailboxNames(); !slices.Equal(got, []string{"Q&A"}) {
		t.Fatalf("InvalidMailboxNames = %q, want [Q&A]", got)
	}

	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("second loadMailboxCache: %v", err)
	}

	if got := c.InvalidMailboxNames(); len(got) != 0 {
		t.Errorf("InvalidMailboxNames after a reload = %q, want none", got)
	}
}

func Test_New_When_ListEntryIsMalformed_Then_FailsInsteadOfLeavingItOut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry string
	}{
		{name: "attributes are not a list", entry: `* LIST X "/" "Work"`},
		{name: "delimiter is a list", entry: `* LIST () (x) "Work"`},
		{name: "name is a list", entry: `* LIST () "/" (a b)`},
		{name: "name is NIL", entry: `* LIST () "/" NIL`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
				if cmd.verb != "LIST" {
					return false, true
				}
				writeLine(conn, "%s", tt.entry)
				writeLine(conn, "%s OK LIST completed", cmd.tag)
				return true, true
			}))

			c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{Label: "src", DialTimeout: 5 * time.Second})

			if err == nil {
				c.Cancel()
				t.Fatal("New returned nil error, want the malformed entry to stop the LIST")
			}
			for _, want := range []string{"failed to load mailbox list", "[src] list mailboxes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if n := srv.callCount("LIST"); n != 1 {
				t.Errorf("LIST count = %d, want 1: a parse failure is not transient", n)
			}
		})
	}
}

func Test_New_When_ListIsInterleavedWithOtherUntaggedResponses_Then_CacheKeepsEveryName(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		writeLine(conn, `* OK [ALERT] maintenance tonight`)
		writeLine(conn, `* LIST () "/" "INBOX"`)
		writeLine(conn, `* CAPABILITY IMAP4rev1 UIDPLUS`)
		writeLine(conn, `* LIST () "/" "Work"`)
		writeLine(conn, "%s OK LIST completed", cmd.tag)
		return true, true
	}))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if got, want := c.MailboxNames(), []string{"INBOX", "Work"}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %q, want %q: a response that is not a LIST entry must not end the listing", got, want)
	}
	if got := c.InvalidMailboxNames(); len(got) != 0 {
		t.Errorf("InvalidMailboxNames = %q, want none", got)
	}
	// go-imap stores the capabilities of an untagged CAPABILITY response only when the LIST handler passes it on.
	if supported, err := c.c.Load().Support("UIDPLUS"); err != nil || !supported {
		t.Errorf("Support(UIDPLUS) = %t, %v, want true: the response during LIST must reach go-imap", supported, err)
	}
}

func FuzzCanonicalName(f *testing.F) {
	seeds := []struct{ name, delimiter string }{
		{"INBOX", "/"}, {"inbox", "/"}, {"iNbOx", ""}, {"inbox/Work", "/"}, {"inbox.Work", "."},
		{"inbox/Work", "."}, {"inbox/Work", ""}, {"INBOXES", "/"}, {"inbox/", "/"}, {"", "/"}, {"inbo", "/"},
		{"INBO\xc3\xa9", "/"}, {"INB\xff", "/"}, {"inbox\xff", "/"}, {"inbox\xc3\xa9/x", "/"}, {"ınbox", "/"},
		{"inbox//x", "//"}, {"inbox\xe2\x80\xa8x", "\xe2\x80\xa8"},
	}
	for _, s := range seeds {
		f.Add(s.name, s.delimiter)
	}
	prefix := len(imap.InboxName)
	f.Fuzz(func(t *testing.T, name, delimiter string) {
		got := canonicalName(name, delimiter)

		if again := canonicalName(got, delimiter); again != got {
			t.Errorf("canonicalName(%q, %q) = %q, but a second pass gives %q", name, delimiter, got, again)
		}
		if len(got) != len(name) {
			t.Fatalf("canonicalName(%q, %q) = %q, want the same length", name, delimiter, got)
		}
		if len(name) > prefix && got[prefix:] != name[prefix:] {
			t.Errorf("canonicalName(%q, %q) = %q, want only the first %d bytes changed", name, delimiter, got, prefix)
		}
		if len(name) <= prefix && got != name && got != imap.InboxName {
			t.Errorf("canonicalName(%q, %q) = %q, want the input or INBOX", name, delimiter, got)
		}
		levelFollows := len(name) > prefix && delimiter != "" && strings.HasPrefix(name[prefix:], delimiter)
		if !levelFollows {
			if want := imap.CanonicalMailboxName(name); got != want {
				t.Errorf("canonicalName(%q, %q) = %q, want %q like the go-imap rule", name, delimiter, got, want)
			}
			return
		}
		if want := imap.InboxName + name[prefix:]; strings.EqualFold(name[:prefix], imap.InboxName) && got != want {
			t.Errorf("canonicalName(%q, %q) = %q, want %q", name, delimiter, got, want)
		}
		if !strings.EqualFold(name[:prefix], imap.InboxName) && got != name {
			t.Errorf("canonicalName(%q, %q) = %q, want the input: the first level is not INBOX", name, delimiter, got)
		}
	})
}

func Test_loadMailboxCache_When_ListIsRetried_Then_DelimiterOfTheFailedAttemptIsDropped(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		// The connection drops after a name that carries a delimiter, before the tagged reply.
		writeLine(conn, `* LIST () "." "Work.Sub"`)
		return true, false
	}))
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		writeLine(conn, `* LIST () NIL "Flat"`)
		writeLine(conn, "%s OK LIST completed", cmd.tag)
		return true, true
	}))

	c, err := New(t.Context(), srv.ln.Addr().String(), "user", "pass", Options{DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})

	if n := srv.callCount("LIST"); n != 2 {
		t.Fatalf("LIST count = %d, want 2 (the dropped attempt and its retry)", n)
	}
	if got := c.GetDelimiter(); got != "" {
		t.Errorf("GetDelimiter = %q, want empty: the retry names no delimiter", got)
	}
	if got, want := c.MailboxNames(), []string{"Flat"}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %q, want %q", got, want)
	}
}

// gatedWriteConn forwards a write that carries LIST, then waits for gate and fails that write with a connection reset, so the server answers a command whose write failed.
type gatedWriteConn struct {
	net.Conn
	gate <-chan struct{}
}

func (c *gatedWriteConn) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("LIST")) {
		return c.Conn.Write(p)
	}
	if _, err := c.Conn.Write(p); err != nil {
		return 0, err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.gate:
	case <-timer.C:
	}
	return 0, &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
}

func Test_listAll_When_CommandWriteFails_Then_ReturnsWhileCallbackStillRuns(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		// No tagged reply: the entry reaches the client while its command write fails.
		writeLine(conn, `* LIST () "/" "Late"`)
		return true, true
	}))

	entered := make(chan struct{})
	c := newDisconnectedClient(t, srv)
	dial := c.dialFn
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := dial(ctx, addr)
		if err != nil {
			return nil, err
		}
		return &gatedWriteConn{Conn: conn, gate: entered}, nil
	}
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("connectAndLogin: %v", err)
	}
	cli := c.c.Load()

	// Registered after the client, so it runs first and never leaves the reader blocked under the handler lock.
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	var name string
	exited := make(chan struct{})
	enteredOnce := sync.OnceFunc(func() { close(entered) })
	exitedOnce := sync.OnceFunc(func() { close(exited) })
	each := func(m *imap.MailboxInfo) {
		name = m.Name
		enteredOnce()
		<-release
		exitedOnce()
	}

	errc := make(chan error, 1)
	go func() {
		_, err := listAll(cli, each)
		errc <- err
	}()

	err := awaitError(t, errc, "listAll while its callback blocks (go-imap now waits for the handler, so audit the listAll comment and the docs again)")
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("listAll error = %v, want the connection reset of the failed write", err)
	}
	select {
	case <-entered:
	default:
		t.Error("the callback had not started when listAll returned")
	}
	select {
	case <-exited:
		t.Error("the callback had ended when listAll returned")
	default:
	}

	releaseOnce()
	awaitSignal(t, exited, "the callback that runs after listAll returned")
	if name != "Late" {
		t.Errorf("callback name = %q, want %q", name, "Late")
	}
}
