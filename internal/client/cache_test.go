package client

import (
	"bufio"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
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
