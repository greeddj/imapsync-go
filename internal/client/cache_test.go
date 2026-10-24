package client

import (
	"context"
	"errors"
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

func Test_loadMailboxCache_When_ServerListsNoMailboxes_Then_DelimiterDefaultsToSlash(t *testing.T) {
	t.Parallel()

	_, c := newScriptedClient(t, &mailboxScript{})

	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("loadMailboxCache: %v", err)
	}
	if c.mailboxCache.delimiter != "/" {
		t.Errorf("delimiter = %q, want %q when the server names none", c.mailboxCache.delimiter, "/")
	}
	if len(c.mailboxCache.folders) != 0 {
		t.Errorf("folders = %v, want none", c.mailboxCache.folders)
	}
}
