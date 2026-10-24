package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// Test_CreateMailbox_walksParents asserts that CreateMailbox creates every
// ancestor folder before creating the target, sending exactly 3 CREATE
// commands (A, A/B, A/B/C) in that order.
func Test_CreateMailbox_walksParents(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.delimiter = "/"
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{},
		delimiter: "/",
		loaded:    true,
	}

	created, err := c.CreateMailbox(context.Background(), "A/B/C")
	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if !created {
		t.Error("CreateMailbox returned created=false, want true")
	}

	if got := srv.callCount("CREATE"); got != 3 {
		t.Errorf("CREATE count = %d, want 3 (A, A/B, A/B/C)", got)
	}

	names := srv.capturedNames("CREATE")
	want := []string{"A", "A/B", "A/B/C"}
	if len(names) != len(want) {
		t.Fatalf("captured CREATE names = %v, want %v", names, want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("CREATE[%d] = %q, want %q", i, names[i], w)
		}
	}
}

// Test_CreateMailbox_idempotent asserts that CreateMailbox returns
// created=false and issues no CREATE or LIST commands when the target mailbox
// already appears in the cache.
func Test_CreateMailbox_idempotent(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.delimiter = "/"
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"X": {}},
		delimiter: "/",
		loaded:    true,
	}

	created, err := c.CreateMailbox(context.Background(), "X")
	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if created {
		t.Error("CreateMailbox returned created=true, want false")
	}
	if got := srv.callCount("CREATE"); got != 0 {
		t.Errorf("CREATE count = %d, want 0", got)
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST count = %d, want 0 (cache served the lookup)", got)
	}
}

// Test_MailboxExists_cachedHit asserts that MailboxExists returns true for a
// name that is already in the cache without issuing a LIST to the server.
func Test_MailboxExists_cachedHit(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}, "Sent": {}},
		delimiter: "/",
		loaded:    true,
	}

	ok, err := c.MailboxExists(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("MailboxExists: %v", err)
	}
	if !ok {
		t.Error("MailboxExists(INBOX) = false, want true")
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST issued unexpectedly: count=%d, want 0", got)
	}
}

// Test_MailboxExists_miss asserts that MailboxExists returns false for an
// unknown name when the cache is already loaded.
func Test_MailboxExists_miss(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
		loaded:    true,
	}

	ok, err := c.MailboxExists(context.Background(), "DoesNotExist")
	if err != nil {
		t.Fatalf("MailboxExists: %v", err)
	}
	if ok {
		t.Error("MailboxExists(DoesNotExist) = true, want false")
	}
}

// Test_ListSubfolders_fromCache asserts that ListSubfolders returns children
// of the given folder using the cache, without a server round-trip.
func Test_ListSubfolders_fromCache(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.delimiter = "/"
	c.mailboxCache = mailboxCache{
		folders: map[string]struct{}{
			"Archive":      {},
			"Archive/2024": {},
			"Archive/2025": {},
			"INBOX":        {},
		},
		delimiter: "/",
		loaded:    true,
	}

	got, err := c.ListSubfolders(context.Background(), "Archive", "/")
	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListSubfolders = %v, want 2 entries", got)
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST issued unexpectedly: count=%d, want 0", got)
	}
}

// Test_loadMailboxCache_populatesDelimiter asserts that when the cache is not
// yet loaded, ensureMailboxCache issues a LIST and populates the cache
// including the hierarchy delimiter from the server response.
func Test_loadMailboxCache_populatesDelimiter(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(listDelimiterHandler(srv))

	c := newClientWithFake(t, srv)
	ok, err := c.hasMailbox(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("hasMailbox: %v", err)
	}
	if !ok {
		t.Error("hasMailbox(INBOX) = false after cache load, want true")
	}
	if c.mailboxCache.delimiter != "/" {
		t.Errorf("delimiter = %q, want /", c.mailboxCache.delimiter)
	}
	if got := srv.callCount("LIST"); got < 1 {
		t.Errorf("LIST count = %d, want ≥ 1", got)
	}
}

// Test_ListMailboxes_returnsNames asserts that ListMailboxes returns a slice
// of MailboxInfo entries with the names from the server's LIST response.
func Test_ListMailboxes_returnsNames(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(listMailboxesHandler(srv))

	c := newClientWithFake(t, srv)

	infos, err := c.ListMailboxes(context.Background())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("ListMailboxes returned %d entries, want 2", len(infos))
	}
	names := map[string]bool{}
	for _, m := range infos {
		names[m.Name] = true
	}
	for _, want := range []string{"INBOX", "Sent"} {
		if !names[want] {
			t.Errorf("missing mailbox %q in result %v", want, infos)
		}
	}
}

// Test_AppendMessage_sendsAppend asserts that AppendMessage issues an APPEND
// command to the server and returns nil on success.
func Test_AppendMessage_sendsAppend(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(appendHandler(srv))

	c := newClientWithFake(t, srv)

	msg := buildTestIMAPMessage()
	err := c.AppendMessage(context.Background(), "INBOX", msg)
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if got := srv.callCount("APPEND"); got != 1 {
		t.Errorf("APPEND count = %d, want 1", got)
	}
}

// Test_CreateMailbox_serverReplies asserts how CreateMailbox maps CREATE replies
// to its result, the commands it sends and the cache it leaves behind.
func Test_CreateMailbox_serverReplies(t *testing.T) {
	t.Parallel()

	const (
		alreadyExists = "NO [ALREADYEXISTS] Mailbox already exists"
		denied        = "NO [CANNOT] permission denied"
	)
	tests := []struct {
		replies     map[string]string
		name        string
		target      string
		wantErr     string
		wantCreates []string
		wantCached  []string
		wantAbsent  []string
		wantCreated bool
	}{
		{
			name:        "target already exists on the server",
			target:      "Archive",
			replies:     map[string]string{"Archive": alreadyExists},
			wantCreates: []string{"Archive"},
			wantCached:  []string{"Archive"},
		},
		{
			name:        "target refused for another reason",
			target:      "Archive",
			replies:     map[string]string{"Archive": denied},
			wantErr:     "failed to create mailbox Archive: permission denied",
			wantCreates: []string{"Archive"},
			wantAbsent:  []string{"Archive"},
		},
		{
			name:        "parent refused for another reason",
			target:      "A/B",
			replies:     map[string]string{"A": denied},
			wantErr:     "failed to create parent folder A: permission denied",
			wantCreates: []string{"A"},
			wantAbsent:  []string{"A", "A/B"},
		},
		{
			name:        "parent already exists on the server",
			target:      "A/B",
			replies:     map[string]string{"A": alreadyExists},
			wantCreated: true,
			wantCreates: []string{"A", "A/B"},
			wantCached:  []string{"A", "A/B"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newScriptedClient(t, &mailboxScript{createReplies: tt.replies})
			c.delimiter = "/"
			c.mailboxCache = mailboxCache{
				folders:   map[string]struct{}{},
				delimiter: "/",
				loaded:    true,
			}

			created, err := c.CreateMailbox(t.Context(), tt.target)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("CreateMailbox: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("CreateMailbox error = %v, want it to contain %q", err, tt.wantErr)
			}
			if created != tt.wantCreated {
				t.Errorf("created = %v, want %v", created, tt.wantCreated)
			}
			if got := srv.capturedNames("CREATE"); !slices.Equal(got, tt.wantCreates) {
				t.Errorf("CREATE names = %v, want %v", got, tt.wantCreates)
			}
			for _, name := range tt.wantCached {
				if ok, _ := c.MailboxExists(t.Context(), name); !ok {
					t.Errorf("MailboxExists(%q) = false, want true", name)
				}
			}
			for _, name := range tt.wantAbsent {
				if ok, _ := c.MailboxExists(t.Context(), name); ok {
					t.Errorf("MailboxExists(%q) = true, want false", name)
				}
			}
			if got := srv.callCount("LIST"); got != 0 {
				t.Errorf("LIST count = %d, want 0 (the cache answered every lookup)", got)
			}
		})
	}
}

// Test_CreateMailbox_whenListRefused asserts that a mailbox cache that cannot
// be loaded stops CreateMailbox before it sends any CREATE.
func Test_CreateMailbox_whenListRefused(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{listRefused: true})
	c.delimiter = "/"

	created, err := c.CreateMailbox(t.Context(), "A/B")
	if err == nil {
		t.Fatal("CreateMailbox returned nil error, want the LIST failure")
	}
	if created {
		t.Error("created = true, want false")
	}
	if got := srv.callCount("LIST"); got != 1 {
		t.Errorf("LIST count = %d, want 1", got)
	}
	if got := srv.callCount("CREATE"); got != 0 {
		t.Errorf("CREATE count = %d, want 0", got)
	}
}

// Test_CreateMailbox_whenContextCanceled asserts that an already canceled
// context is reported as such and nothing reaches the server.
func Test_CreateMailbox_whenContextCanceled(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{})
	c.mailboxCache = mailboxCache{folders: map[string]struct{}{}, delimiter: "/", loaded: true}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	created, err := c.CreateMailbox(ctx, "Archive")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CreateMailbox error = %v, want %v", err, context.Canceled)
	}
	if created {
		t.Error("created = true, want false")
	}
	if got := srv.callCount("CREATE"); got != 0 {
		t.Errorf("CREATE count = %d, want 0", got)
	}
}

// Test_CreateMailbox_whenContextExpiresInFlight asserts that a context ending while a
// LIST or CREATE is pending is reported as its error, not a creation failure.
func Test_CreateMailbox_whenContextExpiresInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hold        *stall
		name        string
		target      string
		wantCreates []string
	}{
		{name: "mailbox list is pending", hold: newStall(verbIs("LIST")), target: "Archive"},
		{name: "parent CREATE is pending", hold: newStall(verbOn("CREATE", "A")), target: "A/B", wantCreates: []string{"A"}},
		{name: "target CREATE is pending", hold: newStall(verbOn("CREATE", "Archive")), target: "Archive", wantCreates: []string{"Archive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.hold.intercept))
			c := newClientWithFake(t, srv)
			c.delimiter = "/"

			ctx := newFiredContext(t.Context())
			var created bool
			errc := make(chan error, 1)
			go func() {
				var err error
				created, err = c.CreateMailbox(ctx, tt.target)
				errc <- err
			}()
			awaitSignal(t, tt.hold.reached, "the server to stop answering")
			ctx.fire()

			err := awaitError(t, errc, "CreateMailbox")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
			}
			if created {
				t.Error("created = true, want false")
			}
			if got := srv.capturedNames("CREATE"); !slices.Equal(got, tt.wantCreates) {
				t.Errorf("CREATE names = %v, want %v", got, tt.wantCreates)
			}
		})
	}
}

// Test_ListMailboxes_sumsMessageSizes asserts that every folder carries its
// message count and the sum of its RFC822.SIZE values, and that an empty
// folder is neither examined nor fetched.
func Test_ListMailboxes_sumsMessageSizes(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX", sizes: []uint32{100, 250}},
		{name: "Empty"},
	}})

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	got := make(map[string]MailboxInfo, len(infos))
	for _, m := range infos {
		got[m.Name] = *m
	}
	want := map[string]MailboxInfo{
		"INBOX": {Name: "INBOX", Messages: 2, Size: 350},
		"Empty": {Name: "Empty"},
	}
	if !maps.Equal(got, want) {
		t.Errorf("ListMailboxes = %+v, want %+v", got, want)
	}
	if names := srv.capturedNames("EXAMINE"); !slices.Equal(names, []string{"INBOX"}) {
		t.Errorf("EXAMINE names = %v, want only INBOX", names)
	}
	if got := srv.callCount("FETCH"); got != 1 {
		t.Errorf("FETCH count = %d, want 1 (the empty folder needs none)", got)
	}
}

// Test_ListMailboxes_whenStatusRefused asserts that one folder whose STATUS
// fails is listed without statistics while the others are still populated.
func Test_ListMailboxes_whenStatusRefused(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{
		statusRefused: map[string]bool{"Broken": true},
		folders: []scriptFolder{
			{name: "INBOX", sizes: []uint32{100, 250}},
			{name: "Broken", sizes: []uint32{5}},
			{name: "Sent", sizes: []uint32{10}},
		},
	})

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	got := make(map[string]MailboxInfo, len(infos))
	for _, m := range infos {
		got[m.Name] = *m
	}
	want := map[string]MailboxInfo{
		"INBOX":  {Name: "INBOX", Messages: 2, Size: 350},
		"Broken": {Name: "Broken"},
		"Sent":   {Name: "Sent", Messages: 1, Size: 10},
	}
	if !maps.Equal(got, want) {
		t.Errorf("ListMailboxes = %+v, want %+v", got, want)
	}
	if slices.Contains(srv.capturedNames("EXAMINE"), "Broken") {
		t.Error("the folder whose STATUS failed was examined anyway")
	}
}

// Test_ListMailboxes_whenSizingFails asserts that a folder whose size cannot
// be read keeps its message count and reports the failure through the log.
func Test_ListMailboxes_whenSizingFails(t *testing.T) {
	t.Parallel()

	_, c := newScriptedClient(t, &mailboxScript{
		examineRefused: map[string]bool{"INBOX": true},
		folders:        []scriptFolder{{name: "INBOX", sizes: []uint32{100, 250}}},
	})
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})
	c.verbose = true

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if len(infos) != 1 || infos[0].Messages != 2 || infos[0].Size != 0 {
		t.Errorf("ListMailboxes = %+v, want INBOX with 2 messages and size 0", infos)
	}
	if !slices.ContainsFunc(logged, func(msg string) bool { return strings.Contains(msg, "failed to size INBOX") }) {
		t.Errorf("log = %v, want a line about failing to size INBOX", logged)
	}
}

// Test_ListMailboxes_reportsProgress asserts that the tracker learns the folder
// total once and advances once per folder, including a folder that was skipped.
func Test_ListMailboxes_reportsProgress(t *testing.T) {
	t.Parallel()

	_, c := newScriptedClient(t, &mailboxScript{
		statusRefused: map[string]bool{"Broken": true},
		folders: []scriptFolder{
			{name: "INBOX", sizes: []uint32{100, 250}},
			{name: "Broken", sizes: []uint32{5}},
			{name: "Sent", sizes: []uint32{10}},
		},
	})
	tr := &recordingTracker{}
	c.SetProgressTracker(tr)

	if _, err := c.ListMailboxes(t.Context()); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	if !slices.Equal(tr.totals, []int64{3}) {
		t.Errorf("UpdateTotal calls = %v, want [3]", tr.totals)
	}
	if !slices.Equal(tr.increments, []int64{1, 1, 1}) {
		t.Errorf("Increment calls = %v, want [1 1 1] (one per folder, skipped one included)", tr.increments)
	}
	if last := tr.messages[len(tr.messages)-1]; !strings.Contains(last, "Done (3 mailboxes)") {
		t.Errorf("last message = %q, want it to report the 3 mailboxes", last)
	}
}

// Test_ListMailboxes_whenListRefused asserts that a refused LIST is returned
// as an error and no statistics are requested.
func Test_ListMailboxes_whenListRefused(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{listRefused: true})

	infos, err := c.ListMailboxes(t.Context())
	if err == nil {
		t.Fatalf("ListMailboxes = %+v, want an error", infos)
	}
	if !strings.Contains(err.Error(), "list mailboxes") {
		t.Errorf("error = %q, want it to mention listing mailboxes", err)
	}
	if got := srv.callCount("STATUS"); got != 0 {
		t.Errorf("STATUS count = %d, want 0", got)
	}
}

// Test_ListMailboxes_whenContextExpiresInFlight asserts that a context ending while a
// LIST or STATUS is pending is returned, not treated as a folder without statistics.
func Test_ListMailboxes_whenContextExpiresInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hold *stall
		name string
	}{
		{name: "mailbox list is pending", hold: newStall(verbIs("LIST"))},
		{name: "STATUS is pending", hold: newStall(verbOn("STATUS", "INBOX"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.hold.intercept))
			c := newClientWithFake(t, srv)

			ctx := newFiredContext(t.Context())
			errc := make(chan error, 1)
			go func() {
				_, err := c.ListMailboxes(ctx)
				errc <- err
			}()
			awaitSignal(t, tt.hold.reached, "the server to stop answering")
			ctx.fire()

			if err := awaitError(t, errc, "ListMailboxes"); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
			}
		})
	}
}

// expiringTracker ends a context when told the folder total, which ListMailboxes
// does between its LIST and its first STATUS.
type expiringTracker struct {
	expire func()
}

func (e *expiringTracker) UpdateMessage(string) {}
func (e *expiringTracker) UpdateTotal(int64)    { e.expire() }
func (e *expiringTracker) Increment(int64)      {}
func (e *expiringTracker) MarkAsErrored()       {}

// Test_ListMailboxes_whenContextExpiresAfterList asserts that a context ending after
// the LIST stops ListMailboxes before it sends any STATUS.
func Test_ListMailboxes_whenContextExpiresAfterList(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(net.Conn, *bufio.Reader, command) (bool, bool) { return false, true }))
	c := newClientWithFake(t, srv)
	ctx := newFiredContext(t.Context())
	c.SetProgressTracker(&expiringTracker{expire: ctx.fire})

	_, err := c.ListMailboxes(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if got := srv.callCount("STATUS"); got != 0 {
		t.Errorf("STATUS count = %d, want 0 after the cancel", got)
	}
}

// Test_getFolderSize_whenFolderIsEmpty asserts that an empty folder is sized 0
// without a FETCH, which would ask for the invalid range 1:0.
func Test_getFolderSize_whenFolderIsEmpty(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{{name: "Empty"}}})

	size, err := c.getFolderSize(t.Context(), "Empty")
	if err != nil {
		t.Fatalf("getFolderSize: %v", err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
	if got := srv.callCount("FETCH"); got != 0 {
		t.Errorf("FETCH count = %d, want 0", got)
	}
}

// Test_getFolderSize_whenFetchFails asserts that a refused FETCH is returned
// without a retry and a context ending during a pending FETCH is returned.
func Test_getFolderSize_whenFetchFails(t *testing.T) {
	t.Parallel()

	refuse := func(conn net.Conn, _ *bufio.Reader, cmd command) (bool, bool) {
		if cmd.verb != "FETCH" {
			return false, true
		}
		writeLine(conn, "%s NO [CANNOT] fetch refused", cmd.tag)
		return true, true
	}
	hold := newStall(verbIs("FETCH"))
	tests := []struct {
		hold      *stall
		intercept func(net.Conn, *bufio.Reader, command) (bool, bool)
		name      string
		wantErr   string
	}{
		{name: "fetch refused", intercept: refuse, wantErr: "fetch refused"},
		{name: "context expires while fetching", intercept: hold.intercept, hold: hold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.intercept))
			c := newClientWithFake(t, srv)

			ctx := newFiredContext(t.Context())
			var size uint64
			errc := make(chan error, 1)
			go func() {
				var err error
				size, err = c.getFolderSize(ctx, "INBOX")
				errc <- err
			}()
			if tt.hold != nil {
				awaitSignal(t, tt.hold.reached, "the server to stop answering FETCH")
				ctx.fire()
			}

			err := awaitError(t, errc, "getFolderSize")
			switch {
			case tt.hold != nil && !errors.Is(err, context.DeadlineExceeded):
				t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
			case tt.hold == nil && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if size != 0 {
				t.Errorf("size = %d, want 0 alongside an error", size)
			}
			if tt.hold == nil {
				if got := srv.callCount("FETCH"); got != 1 {
					t.Errorf("FETCH count = %d, want 1 (a refusal is not retried)", got)
				}
			}
		})
	}
}

// Test_getFolderSize_reusesSelectedFolder asserts that a second sizing of the
// same folder on one connection skips the EXAMINE and takes the message count
// from STATUS instead, with the same result.
func Test_getFolderSize_reusesSelectedFolder(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX", sizes: []uint32{100, 250}},
	}})

	tests := []struct {
		name         string
		wantExamines int
		wantStatuses int
		wantFetches  int
	}{
		{name: "first call selects the folder", wantExamines: 1, wantStatuses: 0, wantFetches: 1},
		{name: "second call reuses the selection", wantExamines: 1, wantStatuses: 1, wantFetches: 2},
	}
	for _, tt := range tests {
		size, err := c.getFolderSize(t.Context(), "INBOX")
		if err != nil {
			t.Fatalf("%s: getFolderSize: %v", tt.name, err)
		}
		if size != 350 {
			t.Errorf("%s: size = %d, want 350", tt.name, size)
		}
		if got := srv.callCount("EXAMINE"); got != tt.wantExamines {
			t.Errorf("%s: EXAMINE count = %d, want %d", tt.name, got, tt.wantExamines)
		}
		if got := srv.callCount("STATUS"); got != tt.wantStatuses {
			t.Errorf("%s: STATUS count = %d, want %d", tt.name, got, tt.wantStatuses)
		}
		if got := srv.callCount("FETCH"); got != tt.wantFetches {
			t.Errorf("%s: FETCH count = %d, want %d", tt.name, got, tt.wantFetches)
		}
	}
}

// Test_getFolderSize_whenServerRefuses asserts that a refused EXAMINE, or a
// refused STATUS on an already selected folder, is returned as an error.
func Test_getFolderSize_whenServerRefuses(t *testing.T) {
	t.Parallel()

	inbox := []scriptFolder{{name: "INBOX", sizes: []uint32{100, 250}}}
	tests := []struct {
		script  *mailboxScript
		name    string
		wantErr string
		prime   bool
	}{
		{
			name: "examine refused",
			script: &mailboxScript{
				examineRefused: map[string]bool{"INBOX": true},
				folders:        inbox,
			},
			wantErr: "examine refused",
		},
		{
			name: "status refused after the folder is selected",
			script: &mailboxScript{
				statusRefused: map[string]bool{"INBOX": true},
				folders:       inbox,
			},
			prime:   true,
			wantErr: "status refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newScriptedClient(t, tt.script)
			if tt.prime {
				if _, err := c.getFolderSize(t.Context(), "INBOX"); err != nil {
					t.Fatalf("priming getFolderSize: %v", err)
				}
			}

			size, err := c.getFolderSize(t.Context(), "INBOX")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("getFolderSize = (%d, %v), want an error containing %q", size, err, tt.wantErr)
			}
			if size != 0 {
				t.Errorf("size = %d, want 0 alongside an error", size)
			}
			if got := srv.callCount("EXAMINE"); got != 1 {
				t.Errorf("EXAMINE count = %d, want 1 (a refusal is not retried)", got)
			}
		})
	}
}

// Test_statsMethods_whenContextCanceled asserts that an already canceled
// context is reported as such, with no command sent, by the methods that
// gather folder statistics.
func Test_statsMethods_whenContextCanceled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		call func(ctx context.Context, c *Client) error
		name string
	}{
		{
			name: "ListMailboxes",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListMailboxes(ctx)
				return err
			},
		},
		{
			name: "getFolderSize",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.getFolderSize(ctx, "INBOX")
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newScriptedClient(t, &mailboxScript{
				folders: []scriptFolder{{name: "INBOX", sizes: []uint32{100}}},
			})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			if err := tt.call(ctx, c); !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want %v", err, context.Canceled)
			}
			for _, verb := range []string{"LIST", "STATUS", "EXAMINE", "FETCH"} {
				if got := srv.callCount(verb); got != 0 {
					t.Errorf("%s count = %d, want 0", verb, got)
				}
			}
		})
	}
}

// recordingTracker is a ProgressTracker that remembers every call.
type recordingTracker struct {
	messages   []string
	totals     []int64
	increments []int64
	errored    int
}

func (r *recordingTracker) UpdateMessage(msg string) { r.messages = append(r.messages, msg) }
func (r *recordingTracker) UpdateTotal(total int64)  { r.totals = append(r.totals, total) }
func (r *recordingTracker) Increment(value int64)    { r.increments = append(r.increments, value) }
func (r *recordingTracker) MarkAsErrored()           { r.errored++ }

// --- additional per-connection handlers ---

// listDelimiterHandler serves a greeting + LOGIN + LIST with INBOX + "/"
// delimiter, then handles LOGOUT.
func listDelimiterHandler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" INBOX\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

// listMailboxesHandler returns a handler that serves LIST (INBOX, Sent) and
// STATUS (0 messages each) so that ListMailboxes can complete normally.
func listMailboxesHandler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			arg := ""
			if len(parts) == 3 {
				arg = parts[2]
			}
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" INBOX\r\n")
				_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" Sent\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 0)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

// appendHandler returns a handler that serves LOGIN normally and handles an
// APPEND command by sending a continuation request, consuming the literal
// body, and replying OK.
func appendHandler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 4)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "APPEND":
				_, _ = fmt.Fprintf(conn, "+ Ready for literal data\r\n")
				bodyLine, _ := reader.ReadString('\n')
				_ = bodyLine
				_, _ = fmt.Fprintf(conn, "%s OK APPEND completed\r\n", tag)
			case "LOGOUT":
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

func Test_MailboxNames_returnsCachedNamesSorted(t *testing.T) {
	t.Parallel()

	script := &mailboxScript{folders: []scriptFolder{{name: "Sent"}, {name: "INBOX"}, {name: "Archive/2024"}}}
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

	got := c.MailboxNames()

	if want := []string{"Archive/2024", "INBOX", "Sent"}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %v, want %v", got, want)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1 (the cache load in New)", n)
	}
}
