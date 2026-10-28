package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

// Test_loadMailboxCache_populatesDelimiter asserts that one LIST fills the
// cache, including the hierarchy delimiter from the server response.
func Test_loadMailboxCache_populatesDelimiter(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(listDelimiterHandler(srv))

	c := newClientWithFake(t, srv)
	if err := c.loadMailboxCache(t.Context()); err != nil {
		t.Fatalf("loadMailboxCache: %v", err)
	}
	if !c.hasMailbox("INBOX") {
		t.Error("hasMailbox(INBOX) = false after cache load, want true")
	}
	if c.mailboxCache.delimiter != "/" {
		t.Errorf("delimiter = %q, want /", c.mailboxCache.delimiter)
	}
	if got := srv.callCount("LIST"); got != 1 {
		t.Errorf("LIST count = %d, want 1", got)
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
		{
			name:        "target answered with Gmail's duplicate folder name",
			target:      "Archive",
			replies:     map[string]string{"Archive": "NO [ALREADYEXISTS] Duplicate folder name Archive (Failure)"},
			wantCreates: []string{"Archive"},
			wantCached:  []string{"Archive"},
		},
		{
			name:        "parent answered with Gmail's duplicate folder name",
			target:      "A/B",
			replies:     map[string]string{"A": "NO [ALREADYEXISTS] Duplicate folder name A (Failure)"},
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

// The server applied the CREATE before it dropped the connection, so the retry
// gets Gmail's duplicate refusal.
func Test_CreateMailbox_When_RetriedCreateAnswersDuplicateFolderName_Then_ReportsExisting(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	createArchive := verbOn("CREATE", "Archive")
	srv.addConnHandler(interceptHandler(srv, func(_ net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if createArchive(cmd) {
			return true, false
		}
		return false, true
	}))
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if createArchive(cmd) {
			writeLine(conn, "%s NO [ALREADYEXISTS] Duplicate folder name Archive (Failure)", cmd.tag)
			return true, true
		}
		return false, true
	}))
	c := newClientWithFake(t, srv)
	c.delimiter = "/"
	c.mailboxCache = mailboxCache{folders: map[string]struct{}{}, delimiter: "/"}

	created, err := c.CreateMailbox(t.Context(), "Archive")
	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if created {
		t.Error("created = true, want false (the folder existed already)")
	}
	if got := srv.capturedNames("CREATE"); !slices.Equal(got, []string{"Archive", "Archive"}) {
		t.Errorf("CREATE names = %v, want [Archive Archive]", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the drop)", got)
	}
	if ok, _ := c.MailboxExists(t.Context(), "Archive"); !ok {
		t.Error("MailboxExists(Archive) = false, want true")
	}
	if got := srv.callCount("LIST"); got != 0 {
		t.Errorf("LIST count = %d, want 0 (the cache answered every lookup)", got)
	}
}

// Test_CreateMailbox_whenContextCanceled asserts that an already canceled
// context is reported as such and nothing reaches the server.
func Test_CreateMailbox_whenContextCanceled(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{})
	c.mailboxCache = mailboxCache{folders: map[string]struct{}{}, delimiter: "/"}

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

func infosByName(t *testing.T, infos []*MailboxInfo) map[string]*MailboxInfo {
	t.Helper()

	byName := make(map[string]*MailboxInfo, len(infos))
	for _, m := range infos {
		byName[m.Name] = m
	}
	return byName
}

func assertReadable(t *testing.T, got map[string]*MailboxInfo, name string, messages uint32, size uint64) {
	t.Helper()

	m := got[name]
	if m == nil {
		t.Errorf("ListMailboxes has no folder %q", name)
		return
	}
	if m.Err != nil || m.NoSelect || m.Messages != messages || m.Size != size {
		t.Errorf("folder %q = %+v, want %d messages, size %d and no error", name, *m, messages, size)
	}
}

func assertUnreadable(t *testing.T, got map[string]*MailboxInfo, name string, reasons ...string) {
	t.Helper()

	m := got[name]
	if m == nil {
		t.Errorf("ListMailboxes has no folder %q", name)
		return
	}
	if m.Err == nil {
		t.Errorf("folder %q has no error, want one that contains %q", name, reasons)
	} else {
		for _, reason := range reasons {
			if !strings.Contains(m.Err.Error(), reason) {
				t.Errorf("folder %q error = %q, want it to contain %q", name, m.Err, reason)
			}
		}
	}
	if m.NoSelect || m.Messages != 0 || m.Size != 0 {
		t.Errorf("folder %q = %+v, want zero statistics and NoSelect false", name, *m)
	}
}

func Test_ListMailboxes_When_StatusRefused_Then_FolderCarriesTheReason(t *testing.T) {
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

	got := infosByName(t, infos)
	assertReadable(t, got, "INBOX", 2, 350)
	assertUnreadable(t, got, "Broken", "read message count", "status refused")
	assertReadable(t, got, "Sent", 1, 10)
	if slices.Contains(srv.capturedNames("EXAMINE"), "Broken") {
		t.Error("the folder whose STATUS failed was examined anyway")
	}
}

func Test_ListMailboxes_When_SizingFails_Then_FolderCarriesTheReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		wired bool
	}{
		{name: "no writer and no tracker"},
		{name: "writer, tracker and verbose as show sets them", wired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, c := newScriptedClient(t, &mailboxScript{
				examineRefused: map[string]bool{"INBOX": true},
				folders:        []scriptFolder{{name: "INBOX", sizes: []uint32{100, 250}}},
			})
			var logged []string
			tr := &recordingTracker{}
			if tt.wired {
				c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})
				c.SetProgressTracker(tr)
				c.verbose = true
			}

			infos, err := c.ListMailboxes(t.Context())
			if err != nil {
				t.Fatalf("ListMailboxes: %v", err)
			}

			got := infosByName(t, infos)
			assertUnreadable(t, got, "INBOX", "read folder size", "examine refused")
			if m := got["INBOX"]; m != nil {
				found := false
				for e := m.Err; e != nil; e = errors.Unwrap(e) {
					if e.Error() == "examine refused" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("error = %v, want a link in its chain with the reply %q", m.Err, "examine refused")
				}
			}
			if len(infos) != 1 {
				t.Errorf("ListMailboxes returned %d folders, want 1", len(infos))
			}
			sizingLine := func(msg string) bool { return strings.Contains(msg, "failed to size") }
			if slices.ContainsFunc(logged, sizingLine) {
				t.Errorf("writer log = %q, want no line about sizing: the reason is in Err", logged)
			}
			if slices.ContainsFunc(tr.messages, sizingLine) {
				t.Errorf("tracker messages = %q, want no line about sizing: the reason is in Err", tr.messages)
			}
		})
	}
}

func Test_ListMailboxes_When_ListMarksNoSelect_Then_ContainerIsSkippedWithoutError(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "[Gmail]", attrs: `\HasChildren \Noselect`},
		{name: "Ghost", attrs: `\NonExistent`},
		{name: "INBOX", sizes: []uint32{100, 250}},
		{name: "[Gmail]/Sent", sizes: []uint32{10}},
	}})
	tr := &recordingTracker{}
	c.SetProgressTracker(tr)

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	got := infosByName(t, infos)
	for _, name := range []string{"[Gmail]", "Ghost"} {
		m := got[name]
		if m == nil {
			t.Errorf("ListMailboxes has no folder %q", name)
			continue
		}
		if !m.NoSelect || m.Err != nil || m.Messages != 0 || m.Size != 0 {
			t.Errorf("folder %q = %+v, want NoSelect with no error and zero statistics", name, *m)
		}
	}
	assertReadable(t, got, "INBOX", 2, 350)
	assertReadable(t, got, "[Gmail]/Sent", 1, 10)
	if names := srv.capturedNames("STATUS"); !slices.Equal(names, []string{"INBOX", "[Gmail]/Sent"}) {
		t.Errorf("STATUS names = %q, want only the folders that hold messages", names)
	}
	if names := srv.capturedNames("EXAMINE"); !slices.Equal(names, []string{"INBOX", "[Gmail]/Sent"}) {
		t.Errorf("EXAMINE names = %q, want only the folders that hold messages", names)
	}
	if !slices.Equal(tr.increments, []int64{1, 1, 1, 1}) {
		t.Errorf("Increment calls = %v, want one per folder, containers included", tr.increments)
	}
}

// Zero backoff and zero reconnectDur keep the reconnect from sleeping, so the test needs no sleepCtx swap.
func Test_ListMailboxes_When_ReconnectGivesUpMidList_Then_EveryRemainingFolderCarriesTheGiveUp(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	statusA, statusB := verbOn("STATUS", "A"), verbOn("STATUS", "B")
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		switch {
		case cmd.verb == "LIST":
			for _, name := range []string{"A", "B", "C", "D"} {
				writeLine(conn, `* LIST (\HasNoChildren) "/" "%s"`, name)
			}
			writeLine(conn, "%s OK LIST completed", cmd.tag)
			return true, true
		case statusA(cmd):
			writeLine(conn, `* STATUS "A" (MESSAGES 0)`)
			writeLine(conn, "%s OK STATUS completed", cmd.tag)
			return true, true
		case statusB(cmd):
			return true, false
		}
		return false, true
	}))
	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	c := newClientWithFake(t, srv)
	c.backoff = 0
	c.reviveDur = time.Hour

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	if c.giveUpErr == nil {
		t.Fatal("the client did not give up")
	}
	got := infosByName(t, infos)
	assertReadable(t, got, "A", 0, 0)
	for _, name := range []string{"B", "C", "D"} {
		assertUnreadable(t, got, name, "read message count")
		if m := got[name]; m != nil && m.Err != nil {
			if !errors.Is(m.Err, c.giveUpErr) {
				t.Errorf("folder %q error = %v, want it to wrap the give-up error %v", name, m.Err, c.giveUpErr)
			}
			if !strings.HasPrefix(m.Err.Error(), "read message count: ") {
				t.Errorf("folder %q error = %q, want the prefix %q", name, m.Err, "read message count: ")
			}
		}
	}
	if names := srv.capturedNames("STATUS"); !slices.Equal(names, []string{"A", "B"}) {
		t.Errorf("STATUS names = %q, want [A B]: the give-up gate stops the rest before the wire", names)
	}
	if n := srv.callCount("LOGIN"); n != 1+maxReconnectAttempts {
		t.Errorf("LOGIN count = %d, want %d (the first session and the whole reconnect budget)", n, 1+maxReconnectAttempts)
	}
}

func Test_ListMailboxes_When_ListIsRetried_Then_ResultAndInvalidNamesOfTheFailedAttemptAreDropped(t *testing.T) {
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
			c := newClientWithFake(t, srv)

			infos, err := c.ListMailboxes(t.Context())
			if err != nil {
				t.Fatalf("ListMailboxes: %v", err)
			}

			names := make([]string, 0, len(infos))
			for _, m := range infos {
				names = append(names, m.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{"INBOX"}) {
				t.Errorf("ListMailboxes names = %q, want [INBOX]: the retry lists only INBOX", names)
			}
			if got := c.InvalidMailboxNames(); len(got) != 0 {
				t.Errorf("InvalidMailboxNames = %q, want none: the retry lists only INBOX", got)
			}
			if n := srv.callCount("LIST"); n != 2 {
				t.Errorf("LIST count = %d, want 2 (the failed attempt and its retry)", n)
			}
			if n := srv.callCount("LOGIN"); n != 2 {
				t.Errorf("LOGIN count = %d, want 2 (the first session and the reconnect)", n)
			}
		})
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

func Test_ListMailboxes_whenContextExpiresInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hold *stall
		name string
	}{
		{name: "mailbox list is pending", hold: newStall(verbIs("LIST"))},
		{name: "STATUS is pending", hold: newStall(verbOn("STATUS", "INBOX"))},
		{name: "size FETCH is pending", hold: newStall(verbIs("FETCH"))},
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

// The wire name is the modified UTF-7 form of hostileName; go-imap decodes it on
// the way in and encodes it back byte for byte on the way out.
const (
	hostileWireName  = "Evil&ABs-[2J&AA0-x&IC4-gpj"
	hostileName      = "Evil\x1b[2J\rx\u202egpj"
	hostileShownName = `Evil\x1b[2J\rx\u202egpj`
)

// assertGraphic fails the test when s holds invalid UTF-8 or a rune that a
// terminal could act on.
func assertGraphic(t *testing.T, what, s string) {
	t.Helper()

	if !utf8.ValidString(s) {
		t.Errorf("%s %q is not valid UTF-8", what, s)
		return
	}
	for _, r := range s {
		if !strconv.IsGraphic(r) {
			t.Errorf("%s %q holds the non-graphic rune %U", what, s, r)
			return
		}
	}
}

func Test_ListMailboxes_When_FolderNameCarriesControlCharacters_Then_TrackerShowsEscapesAndServerGetsExactName(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: hostileWireName, sizes: []uint32{100, 250}},
	}})
	tr := &recordingTracker{}
	c.SetProgressTracker(tr)

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	want := []MailboxInfo{{Name: hostileName, Messages: 2, Size: 350}}
	if len(infos) != 1 || *infos[0] != want[0] {
		t.Errorf("ListMailboxes = %+v, want %+v (the exact name and the STATUS count prove the server saw the exact name)", infos, want)
	}
	for _, verb := range []string{"STATUS", "EXAMINE"} {
		if got := srv.capturedNames(verb); !slices.Equal(got, []string{hostileWireName}) {
			t.Errorf("%s names = %q, want [%q]", verb, got, hostileWireName)
		}
	}
	for _, msg := range tr.messages {
		assertGraphic(t, "tracker message", msg)
	}
	if !slices.ContainsFunc(tr.messages, func(msg string) bool { return strings.Contains(msg, hostileShownName) }) {
		t.Errorf("no tracker message holds %q; got %q", hostileShownName, tr.messages)
	}
}

func Test_CreateMailbox_When_ParentIsNoSelect_Then_CreatesOnlyTheChild(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "[Gmail]", attrs: `\HasChildren \Noselect`},
	}})

	created, err := c.CreateMailbox(t.Context(), "[Gmail]/New")

	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if !created {
		t.Error("CreateMailbox returned created=false, want true")
	}
	if got := srv.capturedNames("CREATE"); !slices.Equal(got, []string{"[Gmail]/New"}) {
		t.Errorf("CREATE names = %q, want only the child: the listed parent exists", got)
	}
	if !c.IsNoSelect("[Gmail]") {
		t.Error("IsNoSelect([Gmail]) = false after the create")
	}
	if c.IsNoSelect("[Gmail]/New") {
		t.Error("IsNoSelect([Gmail]/New) = true for a folder this client created")
	}
}

func Test_CreateMailbox_When_ServerIsFlat_Then_SendsOneCreateWithoutParents(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX", delim: "NIL"}}})

	created, err := c.CreateMailbox(t.Context(), "Work/2023")

	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if !created {
		t.Error("CreateMailbox returned created=false, want true")
	}
	if got := srv.capturedNames("CREATE"); !slices.Equal(got, []string{"Work/2023"}) {
		t.Errorf("CREATE names = %q, want only Work/2023: a flat server has no parent folder", got)
	}
	if c.hasMailbox("Work") {
		t.Error("hasMailbox(Work) = true, want false")
	}
	if !c.hasMailbox("Work/2023") {
		t.Error("hasMailbox(Work/2023) = false after the create")
	}
}

func Test_ListSubfolders_When_ServerIsFlat_Then_ReturnsNoChildren(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "Work", delim: "NIL"},
		{name: "Workshop", delim: "NIL"},
		{name: "Work/2023", delim: "NIL"},
	}})

	got, err := c.ListSubfolders(t.Context(), "Work", "")

	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSubfolders = %q, want none: a flat server has no hierarchy", got)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
}

func Test_IsNoSelect_When_CacheListsNoSelectNames_Then_CacheReadsSendNoCommand(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "[Gmail]", attrs: `\HasChildren \Noselect`},
		{name: "Ghost", attrs: `\NonExistent`},
		{name: "INBOX", sizes: []uint32{10}},
		{name: "[Gmail]/Sent", sizes: []uint32{10}},
	}})

	for name, want := range map[string]bool{"[Gmail]": true, "Ghost": true, "INBOX": false, "[Gmail]/Sent": false} {
		if got := c.IsNoSelect(name); got != want {
			t.Errorf("IsNoSelect(%q) = %t, want %t", name, got, want)
		}
		exists, err := c.MailboxExists(t.Context(), name)
		if err != nil {
			t.Fatalf("MailboxExists(%q): %v", name, err)
		}
		if !exists {
			t.Errorf("MailboxExists(%q) = false, want true: a listed name exists", name)
		}
	}
	if got := c.MailboxNames(); !slices.Equal(got, []string{"Ghost", "INBOX", "[Gmail]", "[Gmail]/Sent"}) {
		t.Errorf("MailboxNames = %q, want every listed name", got)
	}
	subfolders, err := c.ListSubfolders(t.Context(), "[Gmail]", "/")
	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	if want := []string{"[Gmail]/Sent"}; !slices.Equal(subfolders, want) {
		t.Errorf("ListSubfolders = %q, want %q", subfolders, want)
	}

	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
	for _, verb := range []string{"EXAMINE", "SELECT", "STATUS", "CREATE", "FETCH"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("%s count = %d, want 0", verb, n)
		}
	}
}

func Test_ListMailboxes_When_ReconnectGivesUpWhileSizing_Then_ErrWrapsTheGiveUpError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		switch cmd.verb {
		case "LIST":
			writeLine(conn, `* LIST (\HasNoChildren) "/" "INBOX"`)
			writeLine(conn, "%s OK LIST completed", cmd.tag)
			return true, true
		case "STATUS":
			writeLine(conn, `* STATUS "INBOX" (MESSAGES 3)`)
			writeLine(conn, "%s OK STATUS completed", cmd.tag)
			return true, true
		case "EXAMINE":
			return true, false
		}
		return false, true
	}))
	for range maxReconnectAttempts {
		srv.addConnHandler(connHandlerWithLoginReply(srv, dovecotTempAuthFailure))
	}
	c := newClientWithFake(t, srv)
	c.backoff = 0
	c.reviveDur = time.Hour

	infos, err := c.ListMailboxes(t.Context())
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	if c.giveUpErr == nil {
		t.Fatal("the client did not give up")
	}
	got := infosByName(t, infos)
	assertUnreadable(t, got, "INBOX", "read folder size")
	if m := got["INBOX"]; m != nil && !errors.Is(m.Err, c.giveUpErr) {
		t.Errorf("error = %v, want it to wrap the give-up error %v", m.Err, c.giveUpErr)
	}
}

func Test_MailboxExists_When_NameIsInboxInAnyCase_Then_CacheFindsIt(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}, {name: "INBOX/Work"}}})

	tests := []struct {
		name string
		want bool
	}{
		{name: "Inbox", want: true},
		{name: "inbox", want: true},
		{name: "iNbOx", want: true},
		{name: "inbox/Work", want: true},
		{name: "Inbox/Work", want: true},
		{name: "inbox.Work"},
		{name: "INBOX/work"},
		{name: "INBOXWork"},
	}
	for _, tt := range tests {
		got, err := c.MailboxExists(t.Context(), tt.name)
		if err != nil {
			t.Fatalf("MailboxExists(%q): %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("MailboxExists(%q) = %t, want %t", tt.name, got, tt.want)
		}
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1: the cache serves each lookup", n)
	}
}

func Test_ListSubfolders_When_ParentIsInboxInAnyCase_Then_ReturnsInboxChildren(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX"},
		{name: "INBOX/Work"},
		{name: "INBOX/Work/Deep"},
		{name: "Archive/2024"},
	}})

	tests := []struct {
		parent string
		want   []string
	}{
		{parent: "INBOX", want: []string{"INBOX/Work", "INBOX/Work/Deep"}},
		{parent: "inbox", want: []string{"INBOX/Work", "INBOX/Work/Deep"}},
		{parent: "Inbox", want: []string{"INBOX/Work", "INBOX/Work/Deep"}},
		{parent: "inbox/Work", want: []string{"INBOX/Work/Deep"}},
		{parent: "Archive", want: []string{"Archive/2024"}},
		{parent: "archive"},
	}
	for _, tt := range tests {
		got, err := c.ListSubfolders(t.Context(), tt.parent, "")
		if err != nil {
			t.Fatalf("ListSubfolders(%q): %v", tt.parent, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, tt.want) {
			t.Errorf("ListSubfolders(%q) = %q, want %q", tt.parent, got, tt.want)
		}
	}
}

func Test_IsNoSelect_When_NameDiffersOnlyInInboxCase_Then_ReportsIt(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX"},
		{name: "INBOX/Container", attrs: `\Noselect`},
	}})

	for name, want := range map[string]bool{
		"INBOX/Container": true,
		"inbox/Container": true,
		"Inbox/Container": true,
		"INBOX/container": false,
		"inbox":           false,
	} {
		if got := c.IsNoSelect(name); got != want {
			t.Errorf("IsNoSelect(%q) = %t, want %t", name, got, want)
		}
	}
}

func Test_CreateMailbox_When_NameIsInboxInAnotherCase_Then_SendsNoCreateForInbox(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}}})

	created, err := c.CreateMailbox(t.Context(), "Inbox")
	if err != nil {
		t.Fatalf("CreateMailbox(Inbox): %v", err)
	}
	if created {
		t.Error("CreateMailbox(Inbox) returned created=true, want false: INBOX exists")
	}
	if got := srv.capturedNames("CREATE"); len(got) != 0 {
		t.Fatalf("CREATE names = %q after CreateMailbox(Inbox), want none", got)
	}

	created, err = c.CreateMailbox(t.Context(), "inbox/New")
	if err != nil {
		t.Fatalf("CreateMailbox(inbox/New): %v", err)
	}
	if !created {
		t.Error("CreateMailbox(inbox/New) returned created=false, want true")
	}
	if got := srv.capturedNames("CREATE"); !slices.Equal(got, []string{"inbox/New"}) {
		t.Errorf("CREATE names = %q, want only the child, byte for byte: INBOX is the existing parent", got)
	}
	exists, err := c.MailboxExists(t.Context(), "INBOX/New")
	if err != nil {
		t.Fatalf("MailboxExists: %v", err)
	}
	if !exists {
		t.Error("MailboxExists(INBOX/New) = false after the create")
	}
	if got := c.MailboxNames(); !slices.Contains(got, "inbox/New") {
		t.Errorf("MailboxNames = %q, want inbox/New among them byte for byte", got)
	}
	if got := c.ResolveName("INBOX/New"); got != "inbox/New" {
		t.Errorf("ResolveName(INBOX/New) = %q, want the created inbox/New", got)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
}

func Test_MailboxExists_When_ServerIsFlat_Then_OnlyTheBareInboxNameFolds(t *testing.T) {
	t.Parallel()

	_, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "INBOX", delim: "NIL"},
		{name: "inbox/Work", delim: "NIL"},
	}})

	if got, want := c.MailboxNames(), []string{"INBOX", "inbox/Work"}; !slices.Equal(got, want) {
		t.Errorf("MailboxNames = %q, want %q: a flat name keeps its letter case", got, want)
	}
	for name, want := range map[string]bool{"inbox": true, "INBOX/Work": false, "inbox/Work": true} {
		got, err := c.MailboxExists(t.Context(), name)
		if err != nil {
			t.Fatalf("MailboxExists(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("MailboxExists(%q) = %t, want %t", name, got, want)
		}
	}
}

func Test_ListMailboxes_When_ListHoldsInvalidName_Then_ResultLeavesItOutAndRecordsIt(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{folders: []scriptFolder{
		{name: "Q&A", sizes: []uint32{5}},
		{name: "INBOX", sizes: []uint32{100, 250}},
		{name: "Entw\xfcrfe", sizes: []uint32{7}},
		{name: "Work"},
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
		"Work":  {Name: "Work"},
	}
	if !maps.Equal(got, want) {
		t.Errorf("ListMailboxes = %+v, want %+v", got, want)
	}
	for _, verb := range []string{"STATUS", "EXAMINE"} {
		for _, name := range srv.capturedNames(verb) {
			if name != "INBOX" && name != "Work" {
				t.Errorf("%s was sent for %q, a name the client left out", verb, name)
			}
		}
	}
	if got, want := c.InvalidMailboxNames(), []string{"Entw\ufffdrfe", "Q&A"}; !slices.Equal(got, want) {
		t.Errorf("InvalidMailboxNames = %q, want %q", got, want)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1", n)
	}
}

func Test_ListMailboxes_When_ListChangesAfterConnect_Then_InvalidNamesFollowTheLatestList(t *testing.T) {
	t.Parallel()

	var lists atomic.Int32
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "LIST" {
			return false, true
		}
		bad := "A&B"
		if lists.Add(1) > 1 {
			bad = "Q&A"
		}
		writeLine(conn, `* LIST () "/" "INBOX"`)
		writeLine(conn, `* LIST () "/" "%s"`, bad)
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
	if got := c.InvalidMailboxNames(); !slices.Equal(got, []string{"A&B"}) {
		t.Fatalf("InvalidMailboxNames after New = %q, want [A&B]", got)
	}

	if _, err := c.ListMailboxes(t.Context()); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}

	if got := c.InvalidMailboxNames(); !slices.Equal(got, []string{"Q&A"}) {
		t.Errorf("InvalidMailboxNames after ListMailboxes = %q, want [Q&A]", got)
	}
}

func Test_ListMailboxes_When_SessionEndedByBye_Then_ReconnectsAndLists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		farewell string
	}{
		{name: "the server sent a BYE", farewell: "* BYE server shutting down"},
		{name: "the server hung up without a BYE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			srv := newFakeServer(t)
			srv.addConnHandler(byeAfterLoginHandler(srv, release, tt.farewell))
			srv.addConnHandler(interceptHandler(srv, serveNormally))
			c := newClientWithFake(t, srv)
			loggedOut := c.c.Load().LoggedOut()
			releaseOnce()
			awaitSignal(t, loggedOut, "the client to notice the server ending the session")

			var infos []*MailboxInfo
			err := awaitCall(t, "ListMailboxes", func() error {
				var err error
				infos, err = c.ListMailboxes(t.Context())
				return err
			})

			if err != nil {
				t.Fatalf("ListMailboxes: %v, want the listing on a new session", err)
			}
			if len(infos) != 1 || infos[0].Name != "INBOX" {
				t.Errorf("ListMailboxes = %+v, want only INBOX", infos)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2", got)
			}
			if got := srv.callCount("LIST"); got != 1 {
				t.Errorf("LIST count = %d, want 1: the ended session sent none", got)
			}
		})
	}
}

func Test_CacheReads_When_ContextAlreadyEnded_Then_ReturnContextError(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{folders: []scriptFolder{{name: "INBOX"}, {name: "INBOX/Work"}}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	exists, existsErr := c.MailboxExists(ctx, "INBOX")
	children, listErr := c.ListSubfolders(ctx, "INBOX", "")

	if !errors.Is(existsErr, context.Canceled) || exists {
		t.Errorf("MailboxExists = %t, %v, want false and context.Canceled", exists, existsErr)
	}
	if !errors.Is(listErr, context.Canceled) || children != nil {
		t.Errorf("ListSubfolders = %q, %v, want nil and context.Canceled", children, listErr)
	}
	if n := srv.callCount("LIST"); n != 1 {
		t.Errorf("LIST count = %d, want 1: the cache serves both reads", n)
	}
}
