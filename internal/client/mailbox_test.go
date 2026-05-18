package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
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
