package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

func TestParseMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "angle brackets",
			in:   "Message-Id: <abc@example.com>\r\n\r\n",
			want: "abc@example.com",
		},
		{
			name: "no brackets",
			in:   "Message-Id: bare-id@example.com\r\n\r\n",
			want: "bare-id@example.com",
		},
		{
			name: "case-insensitive header",
			in:   "MESSAGE-ID: <id@host>\r\n\r\n",
			want: "id@host",
		},
		{
			name: "lowercase header",
			in:   "message-id: <id@host>\r\n\r\n",
			want: "id@host",
		},
		{
			name: "missing terminator (defensive append)",
			in:   "Message-Id: <id@host>\r\n",
			want: "id@host",
		},
		{
			name: "no Message-Id present",
			in:   "Subject: hi\r\n\r\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "garbage",
			in:   "not a valid header\r\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseMessageID(strings.NewReader(tt.in))
			if got != tt.want {
				t.Errorf("parseMessageID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimAngleBrackets(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"<abc>":    "abc",
		"abc":      "abc",
		"<abc":     "<abc",
		"abc>":     "abc>",
		"":         "",
		"<>":       "",
		"<a@b.c>":  "a@b.c",
		"<<nest>>": "<nest>",
	}
	for in, want := range cases {
		if got := trimAngleBrackets(in); got != want {
			t.Errorf("trimAngleBrackets(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test_StreamMessagesByUIDs_batchesAt500 asserts that StreamMessagesByUIDs
// splits a 1001-UID slice into exactly 3 UID FETCH commands (500+500+1).
func Test_StreamMessagesByUIDs_batchesAt500(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
		loaded:    true,
	}

	uids := make([]uint32, 1001)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}

	err := c.StreamMessagesByUIDs(context.Background(), "INBOX", uids, func(_ *imap.Message) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if got := srv.callCount("UID FETCH"); got != 3 {
		t.Errorf("UID FETCH count = %d, want 3 (batches of 500+500+1)", got)
	}
}

// Test_FetchMessageMap_reportsMissingMessageId asserts that FetchMessageMap
// counts messages without a Message-Id header, logs the count via the
// ProgressWriter, and returns only the messages that have a valid Message-Id.
func Test_FetchMessageMap_reportsMissingMessageId(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(fetchTwoMessagesHandler(srv))

	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
		loaded:    true,
	}

	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})
	c.verbose = true

	result, err := c.FetchMessageMap(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("returned map has %d entries, want 1", len(result))
	}
	if _, ok := result["ok@host"]; !ok {
		t.Errorf("expected key ok@host in map, got %v", result)
	}

	var foundLog bool
	for _, msg := range logged {
		if strings.Contains(msg, "1 message(s) without Message-Id") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Errorf("expected log containing '1 message(s) without Message-Id'; got: %v", logged)
	}
}

// Test_FetchMessageIDSet_returnsIDsWithoutUIDs asserts that FetchMessageIDSet
// wraps FetchMessageMap correctly: it returns the same Message-Id keys but
// drops the UID values, and the returned set contains exactly those IDs.
func Test_FetchMessageIDSet_returnsIDsWithoutUIDs(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(fetchTwoMessagesHandler(srv))

	c := newClientWithFake(t, srv)
	c.mailboxCache = mailboxCache{
		folders:   map[string]struct{}{"INBOX": {}},
		delimiter: "/",
		loaded:    true,
	}

	result, err := c.FetchMessageIDSet(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("FetchMessageIDSet: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("FetchMessageIDSet returned %d entries, want 1", len(result))
	}
	if _, ok := result["ok@host"]; !ok {
		t.Errorf("expected key ok@host in set, got %v", result)
	}
}

// logCapture implements ProgressWriter for tests.
type logCapture struct {
	fn func(string)
}

func (l *logCapture) Log(msg string, a ...any) {
	l.fn(fmt.Sprintf(msg, a...))
}

// fetchTwoMessagesHandler returns a per-connection handler that serves LOGIN
// and EXAMINE normally, then responds to FETCH with 2 messages: one with a
// Message-Id and one without.
func fetchTwoMessagesHandler(srv *fakeServer) func(net.Conn) {
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
			case "SELECT", "EXAMINE":
				_, _ = fmt.Fprintf(conn, "* 2 EXISTS\r\n")
				_, _ = fmt.Fprintf(conn, "* 0 RECENT\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 2)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "FETCH":
				writeFetchResponses(conn)
				_, _ = fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
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

// writeFetchResponses emits two IMAP FETCH responses:
//   - message 1 with Message-Id: <ok@host>
//   - message 2 with an empty Message-Id header
func writeFetchResponses(conn net.Conn) {
	hdr1 := "Message-Id: <ok@host>\r\n\r\n"
	_, _ = fmt.Fprintf(conn,
		"* 1 FETCH (UID 1 BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
		len(hdr1), hdr1,
	)

	hdr2 := "\r\n"
	_, _ = fmt.Fprintf(conn,
		"* 2 FETCH (UID 2 BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
		len(hdr2), hdr2,
	)
}
