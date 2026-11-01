package client

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/responses"
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
	}

	uids := make([]uint32, 1001)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}

	err := c.StreamMessagesByUIDs(context.Background(), "INBOX", 0, uids, nil, func(_ Message) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if got := srv.callCount("UID FETCH"); got != 3 {
		t.Errorf("UID FETCH count = %d, want 3 (batches of 500+500+1)", got)
	}
}

// Test_readMessageIDHeader asserts which of the fetched body sections the
// Message-Id is read from: the one matching messageIDHeaderSection first, then
// any other non-nil section as a fallback.
func Test_readMessageIDHeader(t *testing.T) {
	t.Parallel()

	// go-imap stores a server reply under the section name without PEEK.
	matching := func() *imap.BodySectionName {
		return &imap.BodySectionName{BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    []string{"MESSAGE-ID"},
		}}
	}
	fullBody := func() *imap.BodySectionName { return &imap.BodySectionName{} }
	textPart := func() *imap.BodySectionName {
		return &imap.BodySectionName{BodyPartName: imap.BodyPartName{Specifier: imap.TextSpecifier}}
	}
	firstPart := func() *imap.BodySectionName {
		return &imap.BodySectionName{BodyPartName: imap.BodyPartName{Path: []int{1}}}
	}
	header := func(id string) imap.Literal {
		return strings.NewReader("Message-Id: <" + id + ">\r\n\r\n")
	}
	noID := func() imap.Literal { return strings.NewReader("Subject: hi\r\n\r\n") }

	tests := []struct {
		sections func() map[*imap.BodySectionName]imap.Literal
		name     string
		want     string
	}{
		{
			name: "matching section",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{matching(): header("match@host")}
			},
			want: "match@host",
		},
		{
			name: "matching section wins over other sections",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{
					matching(): header("match@host"),
					fullBody(): header("other@host"),
				}
			},
			want: "match@host",
		},
		{
			name: "differently shaped section found by the fallback scan",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{fullBody(): header("scan@host")}
			},
			want: "scan@host",
		},
		{
			name: "fallback scan skips nil literals",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{
					fullBody():  nil,
					textPart():  nil,
					firstPart(): header("scan@host"),
				}
			},
			want: "scan@host",
		},
		{
			name: "only nil literals",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{fullBody(): nil, textPart(): nil}
			},
			want: "",
		},
		{
			name: "matching section without a Message-Id",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{matching(): noID()}
			},
			want: "",
		},
		{
			name: "scanned section without a Message-Id",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{fullBody(): noID()}
			},
			want: "",
		},
		{
			name: "no body sections",
			sections: func() map[*imap.BodySectionName]imap.Literal {
				return map[*imap.BodySectionName]imap.Literal{}
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := imap.NewMessage(1, nil)
			msg.Body = tt.sections()
			if got := readMessageIDHeader(msg); got != tt.want {
				t.Errorf("readMessageIDHeader = %q, want %q", got, tt.want)
			}
		})
	}
}

// Test_StreamMessagesByUIDs_returnsWithoutFetching asserts that an empty UID
// list and an already canceled context both return before any EXAMINE or
// UID FETCH is sent.
func Test_StreamMessagesByUIDs_returnsWithoutFetching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr   error
		name      string
		uids      []uint32
		cancelCtx bool
	}{
		{name: "empty uid slice"},
		{name: "context already canceled", uids: []uint32{1, 2}, cancelCtx: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newScriptedClient(t, &mailboxScript{
				folders:     []scriptFolder{{name: "INBOX", sizes: []uint32{1, 2}}},
				uidMessages: 2,
			})
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.cancelCtx {
				cancel()
			}

			calls := 0
			err := c.StreamMessagesByUIDs(ctx, "INBOX", 0, tt.uids, nil, func(Message) error {
				calls++
				return nil
			})
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if calls != 0 {
				t.Errorf("callback ran %d times, want 0", calls)
			}
			if got := srv.callCount("EXAMINE"); got != 0 {
				t.Errorf("EXAMINE count = %d, want 0", got)
			}
			if got := srv.callCount("UID FETCH"); got != 0 {
				t.Errorf("UID FETCH count = %d, want 0", got)
			}
		})
	}
}

// Test_StreamMessagesByUIDs_deliversMessages asserts that the callback receives
// every fetched message with its body, in UID order, and that the caller's UID
// slice is left untouched.
func Test_StreamMessagesByUIDs_deliversMessages(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{
		folders:     []scriptFolder{{name: "INBOX", sizes: []uint32{1, 1, 1}}},
		uidMessages: 3,
	})

	uids := []uint32{12, 10, 11}
	var (
		gotUIDs []uint32
		bodies  []string
	)
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
		lit := msg.m.GetBody(fullBodyPeekSection)
		if lit == nil {
			return errors.New("message has no body section")
		}
		body, err := io.ReadAll(lit)
		if err != nil {
			return err
		}
		gotUIDs = append(gotUIDs, msg.m.Uid)
		bodies = append(bodies, string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if want := []uint32{10, 11, 12}; !slices.Equal(gotUIDs, want) {
		t.Errorf("delivered UIDs = %v, want %v", gotUIDs, want)
	}
	wantBodies := []string{
		"Subject: uid 10\r\n\r\nbody\r\n",
		"Subject: uid 11\r\n\r\nbody\r\n",
		"Subject: uid 12\r\n\r\nbody\r\n",
	}
	if !slices.Equal(bodies, wantBodies) {
		t.Errorf("delivered bodies = %q, want %q", bodies, wantBodies)
	}
	if want := []uint32{12, 10, 11}; !slices.Equal(uids, want) {
		t.Errorf("caller UID slice = %v, want it unchanged %v", uids, want)
	}
	if got := srv.capturedNames("EXAMINE"); !slices.Equal(got, []string{"INBOX"}) {
		t.Errorf("EXAMINE names = %v, want [INBOX]", got)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1", got)
	}
}

// Test_StreamMessagesByUIDs_callbackErrorStopsFurtherBatches asserts that a
// callback error in the first batch is returned, no more callbacks run for the
// batch, and no second batch is requested.
func Test_StreamMessagesByUIDs_callbackErrorStopsFurtherBatches(t *testing.T) {
	t.Parallel()

	srv, c := newScriptedClient(t, &mailboxScript{
		folders:     []scriptFolder{{name: "INBOX", sizes: []uint32{1}}},
		uidMessages: 3,
	})

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}

	errStop := errors.New("callback refused the message")
	calls := 0
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(Message) error {
		calls++
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Errorf("error = %v, want %v", err, errStop)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1 (the rest of the batch is drained)", calls)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1 (no second batch)", got)
	}
}

// Test_StreamMessagesByUIDs_whenServerRefuses asserts that a refused EXAMINE or
// UID FETCH is returned with the step that failed, and is not retried.
func Test_StreamMessagesByUIDs_whenServerRefuses(t *testing.T) {
	t.Parallel()

	inbox := []scriptFolder{{name: "INBOX", sizes: []uint32{1}}}
	tests := []struct {
		script       *mailboxScript
		name         string
		wantErr      string
		wantExamines int
		wantFetches  int
	}{
		{
			name:         "examine refused",
			script:       &mailboxScript{examineRefused: map[string]bool{"INBOX": true}, folders: inbox},
			wantErr:      "select folder INBOX: examine refused",
			wantExamines: 1,
			wantFetches:  0,
		},
		{
			name:         "uid fetch refused",
			script:       &mailboxScript{uidFetchRefused: true, folders: inbox},
			wantErr:      "body fetch: fetch refused",
			wantExamines: 1,
			wantFetches:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, c := newScriptedClient(t, tt.script)

			calls := 0
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(Message) error {
				calls++
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if se, ok := errors.AsType[*StreamError](err); !ok || se.FailedBatches != 1 {
				t.Errorf("error = %v, want a *StreamError with one failed batch", err)
			}
			if calls != 0 {
				t.Errorf("callback ran %d times, want 0", calls)
			}
			if got := srv.callCount("EXAMINE"); got != tt.wantExamines {
				t.Errorf("EXAMINE count = %d, want %d", got, tt.wantExamines)
			}
			if got := srv.callCount("UID FETCH"); got != tt.wantFetches {
				t.Errorf("UID FETCH count = %d, want %d", got, tt.wantFetches)
			}
		})
	}
}

// Test_StreamMessagesByUIDs_whenContextExpiresMidBatch asserts that a context ending
// in the callback returns its error and requests no further batch.
func Test_StreamMessagesByUIDs_whenContextExpiresMidBatch(t *testing.T) {
	t.Parallel()

	hold := newStall(func(cmd command) bool {
		return cmd.verb == "UID" && strings.HasPrefix(strings.ToUpper(cmd.arg), "FETCH")
	})
	hold.before = func(conn net.Conn, _ *bufio.Reader, _ command) bool {
		const body = "Subject: uid 1\r\n\r\nbody\r\n"
		writeLine(conn, "* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)", len(body), body)
		return true
	}
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, hold.intercept))
	c := newClientWithFake(t, srv)

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}

	ctx := newFiredContext(t.Context())
	calls := 0
	errc := make(chan error, 1)
	go func() {
		errc <- c.StreamMessagesByUIDs(ctx, "INBOX", 0, uids, nil, func(Message) error {
			calls++
			ctx.fire()
			return nil
		})
	}()

	err := awaitError(t, errc, "StreamMessagesByUIDs")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want %v", err, context.DeadlineExceeded)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1", calls)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1 (no second batch after the cancel)", got)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (a canceled client must not reconnect)", got)
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
	}

	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})
	c.verbose = true

	result, _, err := c.FetchMessageMap(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}
	if want := map[string][]MessageRef{"ok@host": {{UID: 1, Size: 1024}}}; !equalMessageMaps(result, want) {
		t.Errorf("map = %v, want %v", result, want)
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

// scanRow is one scripted scan response. An empty msgID sends a header section with no Message-Id; noUID leaves out the UID item.
type scanRow struct {
	msgID string
	uid   uint32
	size  uint32
	noUID bool
}

// scanIntercept answers EXAMINE with one EXISTS per row and each FETCH with the rows in the given order.
func scanIntercept(rows []scanRow) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return scanSession(rows, len(rows), 0)
}

// writeScanRow sends the Message-Id scan response for row number n, counted from 1.
func writeScanRow(conn net.Conn, n int, r scanRow) {
	hdr := "\r\n"
	if r.msgID != "" {
		hdr = "Message-Id: <" + r.msgID + ">\r\n\r\n"
	}
	uidItem := fmt.Sprintf("UID %d ", r.uid)
	if r.noUID {
		uidItem = ""
	}
	writeLine(conn, "* %d FETCH (%sRFC822.SIZE %d BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)",
		n, uidItem, r.size, len(hdr), hdr)
}

// scanSession serves one scan session. FETCH sends the first sent rows, and with fewer than len(rows) it hangs up before the tagged reply. Zero uidValidity sends none.
func scanSession(rows []scanRow, sent int, uidValidity uint32) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (bool, bool) {
		switch cmd.verb {
		case "SELECT", "EXAMINE":
			writeLine(conn, "* %d EXISTS", len(rows))
			writeLine(conn, "* 0 RECENT")
			if uidValidity != 0 {
				writeLine(conn, "* OK [UIDVALIDITY %d] UIDs valid", uidValidity)
			}
			writeLine(conn, "%s OK [READ-ONLY] %s completed", cmd.tag, cmd.verb)
			return true, true
		case "FETCH":
			for i, r := range rows[:sent] {
				writeScanRow(conn, i+1, r)
			}
			if sent < len(rows) {
				return true, false
			}
			writeLine(conn, "%s OK FETCH completed", cmd.tag)
			return true, true
		}
		return false, true
	}
}

func equalMessageMaps(a, b map[string][]MessageRef) bool {
	return maps.EqualFunc(a, b, slices.Equal[[]MessageRef])
}

func Test_FetchMessageMap_When_MessagesShareMessageId_Then_KeepsEveryUIDInAscendingOrder(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanIntercept([]scanRow{
		{msgID: "dup@x", uid: 9, size: 10},
		{msgID: "dup@x", uid: 4, size: 20},
		{msgID: "one@x", uid: 6, size: 30},
		{msgID: "dup@x", uid: 7, size: 40},
		{uid: 8, size: 50},
	})))
	c := newClientWithFake(t, srv)
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

	got, _, err := c.FetchMessageMap(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}
	want := map[string][]MessageRef{
		"dup@x": {{UID: 4, Size: 20}, {UID: 7, Size: 40}, {UID: 9, Size: 10}},
		"one@x": {{UID: 6, Size: 30}},
	}
	if !equalMessageMaps(got, want) {
		t.Errorf("map = %v, want %v", got, want)
	}
	var warnings int
	for _, msg := range logged {
		if strings.Contains(msg, "1 message(s) without Message-Id") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("got %d warnings about a missing Message-Id, want 1; log: %v", warnings, logged)
	}
}

func Test_FetchMessageMap_When_ServerRepeatsAResponse_Then_KeepsTheUIDOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want map[string][]MessageRef
		name string
		rows []scanRow
	}{
		{
			name: "the repeated response is alone for its id",
			rows: []scanRow{{msgID: "a@x", uid: 5, size: 10}, {msgID: "a@x", uid: 5, size: 10}},
			want: map[string][]MessageRef{"a@x": {{UID: 5, Size: 10}}},
		},
		{
			name: "the repeated responses sit inside a group of repeated ids",
			rows: []scanRow{
				{msgID: "dup@x", uid: 9, size: 10},
				{msgID: "dup@x", uid: 4, size: 20},
				{msgID: "dup@x", uid: 9, size: 10},
				{msgID: "dup@x", uid: 6, size: 30},
				{msgID: "dup@x", uid: 4, size: 20},
			},
			want: map[string][]MessageRef{"dup@x": {{UID: 4, Size: 20}, {UID: 6, Size: 30}, {UID: 9, Size: 10}}},
		},
		{
			name: "a repeated response does not disturb an id that has one message",
			rows: []scanRow{
				{msgID: "a@x", uid: 1, size: 10},
				{msgID: "b@x", uid: 2, size: 20},
				{msgID: "b@x", uid: 2, size: 20},
			},
			want: map[string][]MessageRef{"a@x": {{UID: 1, Size: 10}}, "b@x": {{UID: 2, Size: 20}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, scanIntercept(tt.rows)))
			c := newClientWithFake(t, srv)

			got, _, err := c.FetchMessageMap(t.Context(), "INBOX")

			if err != nil {
				t.Fatalf("FetchMessageMap: %v", err)
			}
			if !equalMessageMaps(got, tt.want) {
				t.Errorf("map = %v, want %v", got, tt.want)
			}
		})
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
//   - message 1 with Message-Id: <ok@host> and RFC822.SIZE 1024
//   - message 2 with an empty Message-Id header and RFC822.SIZE 512
func writeFetchResponses(conn net.Conn) {
	hdr1 := "Message-Id: <ok@host>\r\n\r\n"
	_, _ = fmt.Fprintf(conn,
		"* 1 FETCH (UID 1 RFC822.SIZE 1024 BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
		len(hdr1), hdr1,
	)

	hdr2 := "\r\n"
	_, _ = fmt.Fprintf(conn,
		"* 2 FETCH (UID 2 RFC822.SIZE 512 BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
		len(hdr2), hdr2,
	)
}

// countIntercept reports examineCount messages for SELECT and EXAMINE and
// statusCount for STATUS. It sends the sequence set of each FETCH on fetchSets
// and answers it with writeFetchResponses.
func countIntercept(examineCount, statusCount uint32, fetchSets chan<- string) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (bool, bool) {
		switch cmd.verb {
		case "SELECT", "EXAMINE":
			writeLine(conn, "* %d EXISTS", examineCount)
			writeLine(conn, "* 0 RECENT")
			writeLine(conn, "%s OK [READ-ONLY] %s completed", cmd.tag, cmd.verb)
		case "STATUS":
			writeLine(conn, `* STATUS "%s" (MESSAGES %d)`, firstArg(cmd.arg), statusCount)
			writeLine(conn, "%s OK STATUS completed", cmd.tag)
		case "FETCH":
			fetchSets <- firstArg(cmd.arg)
			writeFetchResponses(conn)
			writeLine(conn, "%s OK FETCH completed", cmd.tag)
		default:
			return false, true
		}
		return true, true
	}
}

func Test_FetchMessageMap_When_ServerReportsHugeMessageCount_Then_AllocationStaysSmall(t *testing.T) {
	// Not parallel: TotalAlloc counts every goroutine, so a parallel test would add its own allocations.
	const (
		hugeMessageCount uint32 = 1 << 22
		// The capped hint costs about 6.1 MiB. An uncapped hint for hugeMessageCount costs about 384 MiB.
		maxScanAlloc uint64 = 16 << 20
	)
	wantSeqSet := fmt.Sprintf("1:%d", hugeMessageCount)

	tests := []struct {
		name         string
		examineCount uint32
		statusCount  uint32
		wantExamine  int
		wantStatus   int
		warmUp       bool
	}{
		{
			name:         "count from EXAMINE",
			examineCount: hugeMessageCount,
			wantExamine:  1,
		},
		{
			name:         "count from STATUS of the selected folder",
			examineCount: 2,
			statusCount:  hugeMessageCount,
			wantExamine:  1,
			wantStatus:   1,
			warmUp:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sets := make(chan string, 2)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, countIntercept(tt.examineCount, tt.statusCount, sets)))
			c := newClientWithFake(t, srv)

			if tt.warmUp {
				if _, _, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil {
					t.Fatalf("FetchMessageMap while selecting the folder: %v", err)
				}
				if got := <-sets; got != "1:2" {
					t.Fatalf("FETCH sequence set while selecting the folder = %q, want %q", got, "1:2")
				}
			}

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			ids, _, err := c.FetchMessageMap(t.Context(), "INBOX")
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("FetchMessageMap: %v", err)
			}

			if want := map[string][]MessageRef{"ok@host": {{UID: 1, Size: 1024}}}; !equalMessageMaps(ids, want) {
				t.Errorf("ids = %v, want %v", ids, want)
			}
			delta := after.TotalAlloc - before.TotalAlloc
			t.Logf("FetchMessageMap allocated %.1f MiB", float64(delta)/(1<<20))
			if delta >= maxScanAlloc {
				t.Errorf("FetchMessageMap allocated %.1f MiB, want less than %d MiB",
					float64(delta)/(1<<20), maxScanAlloc>>20)
			}
			if got := <-sets; got != wantSeqSet {
				t.Errorf("FETCH sequence set = %q, want %q (the range must not be capped)", got, wantSeqSet)
			}
			if got := srv.callCount("EXAMINE"); got != tt.wantExamine {
				t.Errorf("EXAMINE count = %d, want %d", got, tt.wantExamine)
			}
			if got := srv.callCount("STATUS"); got != tt.wantStatus {
				t.Errorf("STATUS count = %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestMessage_MessageID(t *testing.T) {
	t.Parallel()

	withEnvelope := imap.NewMessage(1, nil)
	withEnvelope.Envelope = &imap.Envelope{MessageId: "<a@x>"}
	tests := []struct {
		msg  Message
		name string
		want string
	}{
		{name: "zero value", msg: Message{}},
		{name: "server sent no envelope", msg: Message{m: imap.NewMessage(1, nil)}},
		{name: "envelope present", msg: Message{m: withEnvelope}, want: "<a@x>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.msg.MessageID(); got != tt.want {
				t.Errorf("MessageID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessage_UID(t *testing.T) {
	t.Parallel()

	withUID := imap.NewMessage(1, nil)
	withUID.Uid = 42
	tests := []struct {
		msg  Message
		name string
		want uint32
	}{
		{name: "zero value", msg: Message{}},
		{name: "server sent no UID", msg: Message{m: imap.NewMessage(1, nil)}},
		{name: "UID present", msg: Message{m: withUID}, want: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.msg.UID(); got != tt.want {
				t.Errorf("UID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_MessagesAreDelivered_Then_UIDIsTheRequestedOne(t *testing.T) {
	t.Parallel()

	_, c := newScriptedClient(t, &mailboxScript{
		folders:     []scriptFolder{{name: "INBOX", sizes: []uint32{1, 1, 1}}},
		uidMessages: 3,
	})
	var got []uint32

	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{12, 10, 11}, nil, func(msg Message) error {
		got = append(got, msg.UID())
		return nil
	})

	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}
	if want := []uint32{10, 11, 12}; !slices.Equal(got, want) {
		t.Errorf("Message.UID() of the delivered messages = %v, want %v", got, want)
	}
}

// A worker holds two bodies at most: the one onMessage holds and the one go-imap reads while it waits.
func Test_StreamMessagesByUIDs_When_CallbackBlocks_Then_AtMostOneBodyIsReadAhead(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const messages = 16

		var read atomic.Int32
		srv := newPipeServer()
		c := newPipeClient(t, interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
			if !isUIDFetch(cmd) {
				return false, true
			}
			for _, uid := range requestedUIDs(cmd) {
				writeUIDMessage(conn, uid)
				// net.Pipe has no buffer, so a returned write means the client read the whole response.
				read.Add(1)
			}
			writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
			return true, true
		}))

		uids := make([]uint32, messages)
		for i := range uids {
			uids[i] = uint32(i + 1)
		}
		var seen []int32
		err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(Message) error {
			synctest.Wait()
			seen = append(seen, read.Load())
			return nil
		})
		if err != nil {
			t.Fatalf("StreamMessagesByUIDs: %v", err)
		}

		if len(seen) != messages {
			t.Fatalf("callback ran %d times, want %d", len(seen), messages)
		}
		for i, got := range seen {
			k := i + 1
			if want := int32(min(k+1, messages)); got != want {
				t.Errorf("callback %d saw %d responses read, want %d: the body in onMessage plus at most one read ahead", k, got, want)
			}
		}
	})
}

// Test_StreamMessagesByUIDs_When_CallbackIsSlow_Then_WatchdogDoesNotTrip asserts
// that time spent in onMessage, our own work, is never counted as server silence.
func Test_StreamMessagesByUIDs_When_CallbackIsSlow_Then_WatchdogDoesNotTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages int
	}{
		// The server has sent the tagged OK and is silent while the callback runs.
		{name: "at the batch tail", messages: 1},
		// More messages than the body stream holds, so the reader stops
		// consuming while the first callback runs.
		{name: "in the middle of a batch", messages: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := &mailboxScript{
				folders:     []scriptFolder{{name: "INBOX", sizes: make([]uint32, tt.messages)}},
				uidMessages: tt.messages,
			}
			srv := newFakeServer(t)
			srv.addConnHandler(script.handler(srv))
			c := newWatchedClient(t, srv, holdLimit)

			uids := make([]uint32, tt.messages)
			for i := range uids {
				uids[i] = uint32(i + 1)
			}
			delivered := 0
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(Message) error {
				delivered++
				if delivered == 1 {
					time.Sleep(3*holdLimit + holdLimit/2)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v", err)
			}
			if delivered != tt.messages {
				t.Errorf("callback ran %d times, want %d", delivered, tt.messages)
			}
			if c.idle.Load().tripped() {
				t.Error("the watchdog tripped while the callback ran")
			}

			if err := c.safeCall(func(cli *imapclient.Client) error { return cli.Noop() }); err != nil {
				t.Fatalf("safeCall after the stream: %v", err)
			}
			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1 (the source session survived the slow callback)", got)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_ServerSilentBeforeFirstMessage_Then_WatchdogRepairsAndStreamCompletes(t *testing.T) {
	t.Parallel()

	fetch := newStall(verbIs("UID"))
	hungUp := make(chan struct{})
	script := &mailboxScript{uidMessages: 1}
	srv := newFakeServer(t)
	srv.addConnHandler(endedSignal(interceptHandler(srv, fetch.intercept), hungUp))
	srv.addConnHandler(script.handler(srv))
	c := newWatchedClient(t, srv, tripLimit)
	first := c.idle.Load()
	// The reconnect dials the session whose callback must stay outside any span.
	c.idleDur = holdLimit

	delivered := 0
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(Message) error {
			delivered++
			time.Sleep(3*holdLimit + holdLimit/2)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after the watchdog reconnect", err)
	}
	if delivered != 1 {
		t.Errorf("callback ran %d times, want 1", delivered)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the trip)", got)
	}
	if got := srv.callCount("UID FETCH"); got != 2 {
		t.Errorf("UID FETCH count = %d, want 2 (the stalled batch is retried)", got)
	}
	if !first.tripped() {
		t.Error("the first session's watchdog did not trip")
	}
	if second := c.idle.Load(); second == first || second.tripped() {
		t.Error("the slow callback ran on a stale or tripped watchdog")
	}
	if c.isCancelled() {
		t.Error("a watchdog trip cancelled the client")
	}
	awaitSignal(t, hungUp, "the client to close the stalled connection")
}

// onMessage runs outside any span and every span must close again; an unbalanced
// count leaves the watchdog dead or armed on an idle session.
func Test_StreamMessagesByUIDs_When_CallbackRuns_Then_WatchdogSpanIsClosedInsideAndBalancedAfter(t *testing.T) {
	t.Parallel()

	errCallback := errors.New("callback refused the message")
	tests := []struct {
		err  error
		name string
	}{
		{name: "callback succeeds"},
		{name: "callback fails", err: errCallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const messages = 3
			script := &mailboxScript{uidMessages: messages}
			srv := newFakeServer(t)
			srv.addConnHandler(script.handler(srv))
			c := newWatchedClient(t, srv, holdLimit)
			w := c.idle.Load()

			calls := 0
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, nil, func(Message) error {
				calls++
				if got := w.active.Load(); got != 0 {
					t.Errorf("open spans inside callback %d = %d, want 0", calls, got)
				}
				return tt.err
			})
			if !errors.Is(err, tt.err) {
				t.Fatalf("StreamMessagesByUIDs error = %v, want %v", err, tt.err)
			}
			wantCalls := messages
			if tt.err != nil {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Errorf("callback ran %d times, want %d", calls, wantCalls)
			}
			if got := w.active.Load(); got != 0 {
				t.Errorf("open spans after the stream = %d, want 0", got)
			}
			if w.tripped() {
				t.Error("the watchdog tripped on a healthy stream")
			}
		})
	}
}

// uidFetchLog records the UIDs each UID FETCH asked for, one entry per command.
type uidFetchLog struct {
	requests [][]uint32
	mu       sync.Mutex
}

func (l *uidFetchLog) record(uids []uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, uids)
}

func (l *uidFetchLog) all() [][]uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

func isUIDFetch(cmd command) bool {
	return cmd.verb == "UID" && strings.HasPrefix(strings.ToUpper(cmd.arg), "FETCH")
}

// requestedUIDs expands the UID set of a UID FETCH command.
func requestedUIDs(cmd command) []uint32 {
	fields := strings.Fields(cmd.arg)
	if len(fields) < 2 {
		return nil
	}
	set, err := imap.ParseSeqSet(fields[1])
	if err != nil {
		return nil
	}
	var uids []uint32
	for _, r := range set.Set {
		for uid := r.Start; uid <= r.Stop; uid++ {
			uids = append(uids, uid)
		}
	}
	return uids
}

// writeUIDMessage sends one FETCH response that carries the full body of uid.
func writeUIDMessage(conn net.Conn, uid uint32) {
	writeUIDMessageWithItems(conn, uid, "")
}

// writeUIDMessageWithItems is writeUIDMessage with extra fetch items, such as " FLAGS (\Seen)" with a leading space, between the UID and the body.
func writeUIDMessageWithItems(conn net.Conn, uid uint32, extra string) {
	body := "Subject: uid " + strconv.FormatUint(uint64(uid), 10) + "\r\n\r\nbody\r\n"
	writeLine(conn, "* %d FETCH (UID %d%s BODY[] {%d}\r\n%s)", uid, uid, extra, len(body), body)
}

// answerUIDFetch is an interceptHandler hook that answers each UID FETCH with the
// messages send picks for the requested UIDs, then the tagged OK.
func answerUIDFetch(log *uidFetchLog, send func(requested []uint32) []uint32) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		for _, uid := range send(requested) {
			writeUIDMessage(conn, uid)
		}
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}
}

// cutUIDFetch sends the first sent requested messages and never the tagged OK, then hangs up if hangUp is set, else waits for the client to close.
func cutUIDFetch(log *uidFetchLog, sent int, hangUp bool) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		for _, uid := range requested[:min(sent, len(requested))] {
			writeUIDMessage(conn, uid)
		}
		if !hangUp {
			_, _ = rd.ReadByte()
		}
		return true, false
	}
}

// byeAfterUIDFetch answers each UID FETCH in full; after release it sends a BYE
// and hangs up, so the BYE lands while the caller still drains the batch.
func byeAfterUIDFetch(log *uidFetchLog, release <-chan struct{}) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		for _, uid := range requested {
			writeUIDMessage(conn, uid)
		}
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		<-release
		writeLine(conn, "* BYE Autologout; idle for too long")
		return true, false
	}
}

func requestedAsSent(requested []uint32) []uint32 { return requested }

func Test_StreamMessagesByUIDs_When_ServerSendsByeBetweenBatches_Then_ReconnectsAndDeliversTheRest(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeAfterUIDFetch(fetches, release)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	first := c.c.Load()

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		if msg.m.Uid == uidFetchBatchSize {
			releaseOnce()
			awaitSignal(t, first.LoggedOut(), "the first session to end after the BYE")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after the reconnect", err)
	}

	if !slices.Equal(got, uids) {
		t.Errorf("callback got %d UIDs, want each of the %d exactly once, in order", len(got), len(uids))
	}
	want := [][]uint32{uids[:uidFetchBatchSize], uids[uidFetchBatchSize:]}
	if gotSets := fetches.all(); !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH batches = %d, want %d (500 + 1, no batch fetched twice)", len(gotSets), len(want))
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the BYE)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the new session selects the folder)", got)
	}
}

func Test_StreamMessagesByUIDs_When_ExamineOfAnotherFolderFailed_Then_SelectsAgain(t *testing.T) {
	t.Parallel()

	// RFC 3501 section 6.3.1: a failed EXAMINE leaves no mailbox selected.
	selected := ""
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		switch {
		case cmd.verb == "EXAMINE" && firstArg(cmd.arg) == "Missing":
			selected = ""
			writeLine(conn, "%s NO [NONEXISTENT] Mailbox doesn't exist: Missing", cmd.tag)
			return true, true
		case cmd.verb == "EXAMINE":
			selected = firstArg(cmd.arg)
			return false, true
		case isUIDFetch(cmd) && selected == "":
			writeLine(conn, "%s BAD No mailbox selected.", cmd.tag)
			return true, true
		case isUIDFetch(cmd):
			writeUIDMessage(conn, 1)
			writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
			return true, true
		}
		return false, true
	}))
	c := newWatchedClient(t, srv, 0)

	if _, _, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil {
		t.Fatalf("FetchMessageMap INBOX: %v", err)
	}
	if _, _, err := c.FetchMessageMap(t.Context(), "Missing"); err == nil {
		t.Fatal("FetchMessageMap Missing returned nil, want the refusal")
	}

	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after a new EXAMINE", err)
	}

	if want := []uint32{1}; !slices.Equal(got, want) {
		t.Errorf("callback got UIDs %v, want %v", got, want)
	}
	if got, want := srv.capturedNames("EXAMINE"), []string{"INBOX", "Missing", "INBOX"}; !slices.Equal(got, want) {
		t.Errorf("EXAMINE names = %v, want %v", got, want)
	}
	if got := srv.callCount("LOGIN"); got != 1 {
		t.Errorf("LOGIN count = %d, want 1 (the session is healthy, so no reconnect)", got)
	}
}

// Test_StreamMessagesByUIDs_When_TransientErrorAfterPartialBatch_Then_RetryFetchesOnlyUndeliveredUIDs
// asserts that a retried batch asks for the UIDs not yet handed to the callback,
// so no message reaches it twice.
func Test_StreamMessagesByUIDs_When_TransientErrorAfterPartialBatch_Then_RetryFetchesOnlyUndeliveredUIDs(t *testing.T) {
	t.Parallel()

	const (
		total = 5
		sent  = 2
	)
	tests := []struct {
		name   string
		idle   time.Duration
		hangUp bool
	}{
		{name: "server goes silent and the watchdog trips", idle: tripLimit},
		{name: "server drops the socket", idle: holdLimit, hangUp: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, cutUIDFetch(fetches, sent, tt.hangUp)))
			srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
			c := newWatchedClient(t, srv, tt.idle)
			first := c.idle.Load()
			// The session after the reconnect must not trip on a slow runner.
			c.idleDur = holdLimit

			uids := make([]uint32, total)
			for i := range uids {
				uids[i] = uint32(i + 1)
			}
			var got []uint32
			err := awaitCall(t, "StreamMessagesByUIDs", func() error {
				return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
					got = append(got, msg.m.Uid)
					return nil
				})
			})
			if err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v, want success after the retry", err)
			}

			if !slices.Equal(got, uids) {
				t.Errorf("callback got UIDs %v, want each of %v exactly once", got, uids)
			}
			want := [][]uint32{uids, uids[sent:]}
			if gotSets := fetches.all(); !slices.EqualFunc(gotSets, want, slices.Equal) {
				t.Errorf("UID FETCH asked for %v, want %v (the retry asks only for the rest)", gotSets, want)
			}
			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2 (one fresh session for the retry)", got)
			}
			if first.tripped() == tt.hangUp {
				t.Errorf("first session tripped = %v, want %v", first.tripped(), !tt.hangUp)
			}
		})
	}
}

// Test_StreamMessagesByUIDs_When_TransientErrorAfterEveryMessage_Then_RetrySendsNoCommand
// asserts that a batch fully handed over before the error is not selected or
// fetched again on the new session.
func Test_StreamMessagesByUIDs_When_TransientErrorAfterEveryMessage_Then_RetrySendsNoCommand(t *testing.T) {
	t.Parallel()

	const total = 5
	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, cutUIDFetch(fetches, total, false)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, tripLimit)
	first := c.idle.Load()
	c.idleDur = holdLimit

	uids := make([]uint32, total)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
			got = append(got, msg.m.Uid)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after the retry", err)
	}

	if !slices.Equal(got, uids) {
		t.Errorf("callback got UIDs %v, want each of %v exactly once", got, uids)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1 (nothing left to fetch on the retry)", got)
	}
	if got := srv.callCount("EXAMINE") + srv.callCount("SELECT"); got != 1 {
		t.Errorf("EXAMINE+SELECT count = %d, want 1 (the new session never selects)", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the retry still repairs the session)", got)
	}
	if !first.tripped() {
		t.Error("the first session's watchdog did not trip")
	}
}

// Test_StreamMessagesByUIDs_When_ServerRepeatsAUID_Then_CallbackRunsOnce asserts
// that a second FETCH response for a UID already handed over is dropped.
func Test_StreamMessagesByUIDs_When_ServerRepeatsAUID_Then_CallbackRunsOnce(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, func([]uint32) []uint32 {
		return []uint32{1, 2, 2, 3}
	})))
	c := newClientWithFake(t, srv)

	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if want := []uint32{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("callback got UIDs %v, want %v (UID 2 once)", got, want)
	}
}

func Test_StreamMessagesByUIDs_When_BodylessResponseComesFirst_Then_FullMessageStillDelivered(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		writeUIDMessage(conn, 1)
		writeLine(conn, `* 2 FETCH (UID 2 FLAGS (\Seen))`)
		writeUIDMessage(conn, 2)
		writeUIDMessage(conn, 3)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	calls := make(map[uint32]int)
	withoutBody := 0
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, nil, func(msg Message) error {
		calls[msg.m.Uid]++
		if msg.m.GetBody(fullBodyPeekSection) == nil {
			withoutBody++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	for _, uid := range []uint32{1, 2, 3} {
		if got := calls[uid]; got != 1 {
			t.Errorf("UID %d reached the callback %d times, want 1", uid, got)
		}
	}
	if withoutBody != 0 {
		t.Errorf("callback got %d messages without a body, want 0", withoutBody)
	}
}

func Test_StreamMessagesByUIDs_When_UIDOnlyGetsBodylessResponses_Then_CallbackSkipsItAndStreamSucceeds(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		writeUIDMessage(conn, 1)
		writeLine(conn, `* 2 FETCH (UID 2 FLAGS (\Seen))`)
		writeUIDMessage(conn, 3)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success", err)
	}

	if want := []uint32{1, 3}; !slices.Equal(got, want) {
		t.Errorf("callback got UIDs %v, want %v", got, want)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1: a missing body is no reason to fetch again", got)
	}
}

// Test_StreamMessagesByUIDs_When_StreamSpansBatches_Then_EveryUIDIsDelivered
// asserts that the delivered state of one batch does not leak into the next.
func Test_StreamMessagesByUIDs_When_StreamSpansBatches_Then_EveryUIDIsDelivered(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newClientWithFake(t, srv)

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if !slices.Equal(got, uids) {
		t.Errorf("callback ran %d times, want %d, once per UID in order", len(got), len(uids))
	}
	want := [][]uint32{uids[:uidFetchBatchSize], uids[uidFetchBatchSize:]}
	if gotSets := fetches.all(); !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH batches = %d, want %d (500 + 1)", len(gotSets), len(want))
	}
}

func Test_StreamMessagesByUIDs_When_StreamSpansBatches_Then_SelectsFolderOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		folder   string
		wireName string
	}{
		{name: "ASCII name", folder: "INBOX", wireName: "INBOX"},
		{name: "non-ASCII name", folder: "Entw\u00fcrfe", wireName: "Entw&APw-rfe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
			c := newClientWithFake(t, srv)

			uids := make([]uint32, 2*uidFetchBatchSize+1)
			for i := range uids {
				uids[i] = uint32(i + 1)
			}
			var got []uint32
			err := c.StreamMessagesByUIDs(t.Context(), tt.folder, 0, uids, nil, func(msg Message) error {
				got = append(got, msg.m.Uid)
				return nil
			})
			if err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v", err)
			}

			if !slices.Equal(got, uids) {
				t.Errorf("callback ran %d times, want %d, once per UID in order", len(got), len(uids))
			}
			if got, want := srv.capturedNames("EXAMINE"), []string{tt.wireName}; !slices.Equal(got, want) {
				t.Errorf("EXAMINE names = %v, want %v (one EXAMINE for all batches)", got, want)
			}
			if got := srv.callCount("UID FETCH"); got != 3 {
				t.Errorf("UID FETCH count = %d, want 3 (500 + 500 + 1)", got)
			}
			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1", got)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_SessionEndedAfterEarlierSelect_Then_ReselectsOnANewSession(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeAfterExamine(release)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	first := c.c.Load()
	if _, err := c.selectIfNeeded(first, "INBOX"); err != nil {
		t.Fatalf("selectIfNeeded: %v", err)
	}
	releaseOnce()
	awaitSignal(t, first.LoggedOut(), "the first session to end after the BYE")

	uids := []uint32{1, 2, 3}
	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after the reconnect", err)
	}

	if !slices.Equal(got, uids) {
		t.Errorf("callback got UIDs %v, want each of %v exactly once", got, uids)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the BYE)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the new session selects the folder)", got)
	}
	if gotSets, want := fetches.all(), [][]uint32{uids}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH asked for %v, want %v (only the new session fetches)", gotSets, want)
	}
}

func Test_FetchMessageMap_When_RefusalEchoesTransientPhraseOfFolderName_Then_ReconnectsOnce(t *testing.T) {
	t.Parallel()

	const folder = "Not logged in"
	refuseExamine := func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "EXAMINE" {
			return false, true
		}
		writeLine(conn, "%s NO Mailbox doesn't exist: %s", cmd.tag, firstArg(cmd.arg))
		return true, true
	}
	srv := newFakeServer(t)
	// More connections than the single reconnect may use, all serving the same refusal.
	for range 4 {
		srv.addConnHandler(interceptHandler(srv, refuseExamine))
	}
	c := newClientWithFake(t, srv)

	_, _, err := c.FetchMessageMap(t.Context(), folder)

	if err == nil {
		t.Fatal("FetchMessageMap returned nil, want the refusal")
	}
	if want := "Mailbox doesn't exist: " + folder; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one reconnect and no more)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the first attempt and its single retry)", got)
	}
}

func Test_FetchMessageMap_When_ExamineIsRefused_Then_OnlyServerRefusalsAreMarked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		folder       string
		status       string
		info         string
		wantClass    ErrClass
		wantLogins   int
		wantExamines int
		dropFirst    bool
		wantMarked   bool
	}{
		{
			name: "refusal of an unknown mailbox", folder: "Archive", status: "NO", info: "Mailbox doesn't exist: Archive",
			wantMarked: true, wantClass: ClassUnknown, wantLogins: 1, wantExamines: 1,
		},
		{
			name: "refusal with a permanent phrase", folder: "Archive", status: "NO", info: "No such mailbox",
			wantMarked: true, wantClass: ClassPermanent, wantLogins: 1, wantExamines: 1,
		},
		{
			name: "bad command reply", folder: "Archive", status: "BAD", info: "Invalid mailbox name",
			wantMarked: true, wantClass: ClassUnknown, wantLogins: 1, wantExamines: 1,
		},
		{
			name: "throttle reply", folder: "Archive", status: "NO", info: "Too many simultaneous connections",
			wantMarked: false, wantClass: ClassThrottled, wantLogins: 1, wantExamines: 1,
		},
		{
			name: "transient reply on both sessions", folder: "Not logged in", status: "NO", info: "Mailbox doesn't exist: Not logged in",
			wantMarked: false, wantClass: ClassTransient, wantLogins: 2, wantExamines: 2,
		},
		{
			name: "first session drops and the second refuses", folder: "Archive", status: "NO", info: "Mailbox doesn't exist: Archive",
			dropFirst: true, wantMarked: true, wantClass: ClassUnknown, wantLogins: 2, wantExamines: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refuseExamine := func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
				if cmd.verb != "EXAMINE" {
					return false, true
				}
				writeLine(conn, "%s %s %s", cmd.tag, tt.status, tt.info)
				return true, true
			}
			srv := newFakeServer(t)
			if tt.dropFirst {
				srv.addConnHandler(interceptHandler(srv, byeOn(verbIs("EXAMINE"))))
			}
			// A second refusing session serves the retry of the transient row.
			srv.addConnHandler(interceptHandler(srv, refuseExamine))
			srv.addConnHandler(interceptHandler(srv, refuseExamine))
			c := newClientWithFake(t, srv)

			_, _, err := c.FetchMessageMap(t.Context(), tt.folder)

			if err == nil {
				t.Fatal("FetchMessageMap returned nil, want the refusal")
			}
			if got := errors.Is(err, ErrSelectRefused); got != tt.wantMarked {
				t.Errorf("errors.Is(err, ErrSelectRefused) = %t, want %t (error: %v)", got, tt.wantMarked, err)
			}
			if want := "cannot select folder " + tt.folder + ": " + tt.info; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
			if got := classifyError(err); got != tt.wantClass {
				t.Errorf("classifyError(err) = %s, want %s", got, tt.wantClass)
			}
			if got, bare := classifyError(err), classifyError(errors.New(tt.info)); got != bare {
				t.Errorf("classifyError(err) = %s, want the class of the bare reply, %s", got, bare)
			}
			if got := srv.callCount("LOGIN"); got != tt.wantLogins {
				t.Errorf("LOGIN count = %d, want %d", got, tt.wantLogins)
			}
			if got := srv.callCount("EXAMINE"); got != tt.wantExamines {
				t.Errorf("EXAMINE count = %d, want %d", got, tt.wantExamines)
			}
		})
	}
}

func Test_FetchMessageMap_When_ExamineDropsAndReconnectIsRefused_Then_ErrorIsNotSelectRefusal(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeOn(verbIs("EXAMINE"))))
	srv.addConnHandler(connHandlerWithLoginReply(srv, "NO [AUTHENTICATIONFAILED] Invalid credentials"))
	c := newClientWithFake(t, srv)

	_, _, err := c.FetchMessageMap(t.Context(), "Archive")

	if err == nil {
		t.Fatal("FetchMessageMap returned nil, want the reconnect failure")
	}
	if errors.Is(err, ErrSelectRefused) {
		t.Errorf("error = %v, want no ErrSelectRefused: the session broke, the server did not refuse the folder", err)
	}
	if got := classifyError(err); got != ClassPermanent {
		t.Errorf("classifyError(err) = %s, want %s", got, ClassPermanent)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the first session and the refused reconnect)", got)
	}
}

// The error class comes from go-imap, not from our wrapper text.
func Test_FetchMessageMap_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		folder       string
		prefix       string
		dropVerb     string
		selected     bool
		wantExamines int
		wantStatuses int
	}{
		{name: "plain folder name", folder: "Work", dropVerb: "EXAMINE", wantExamines: 2},
		{name: "folder name with lockout", folder: "Alerts/Account Lockout", dropVerb: "EXAMINE", wantExamines: 2},
		{name: "folder name with quota exceeded", folder: "Quota exceeded", dropVerb: "EXAMINE", wantExamines: 2},
		{name: "folder name with session expired", folder: "Session expired", dropVerb: "EXAMINE", wantExamines: 2},
		{name: "folder name with too many simultaneous", folder: "Too many simultaneous", dropVerb: "EXAMINE", wantExamines: 2},
		{name: "label with bandwidth limit exceeded", folder: "Work", prefix: "bandwidth limit exceeded", dropVerb: "EXAMINE", wantExamines: 2},
		{
			name:         "status of the already selected folder",
			folder:       "Alerts/Account Lockout",
			dropVerb:     "STATUS",
			selected:     true,
			wantExamines: 2,
			wantStatuses: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, byeOn(verbIs(tt.dropVerb))))
			srv.addConnHandler(interceptHandler(srv, serveNormally))
			c := newClientWithFake(t, srv)
			c.prefix = tt.prefix
			if tt.selected {
				if _, err := c.selectIfNeeded(c.c.Load(), tt.folder); err != nil {
					t.Fatalf("selectIfNeeded: %v", err)
				}
			}

			if _, _, err := c.FetchMessageMap(t.Context(), tt.folder); err != nil {
				t.Fatalf("FetchMessageMap: %v, want success after the reconnect", err)
			}

			if got := srv.callCount("LOGIN"); got != 2 {
				t.Errorf("LOGIN count = %d, want 2 (one fresh session for the retry)", got)
			}
			if got := srv.callCount("EXAMINE"); got != tt.wantExamines {
				t.Errorf("EXAMINE count = %d, want %d", got, tt.wantExamines)
			}
			if got := srv.callCount("STATUS"); got != tt.wantStatuses {
				t.Errorf("STATUS count = %d, want %d", got, tt.wantStatuses)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndStreams(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeOn(verbIs("EXAMINE"))))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newClientWithFake(t, srv)

	uids := []uint32{1, 2, 3}
	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "Quota exceeded", 0, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want success after the reconnect", err)
	}

	if !slices.Equal(got, uids) {
		t.Errorf("callback got UIDs %v, want each of %v exactly once", got, uids)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session for the retry)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2 (the retry selects again)", got)
	}
	if gotSets, want := fetches.all(), [][]uint32{uids}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH asked for %v, want %v (only the new session fetches)", gotSets, want)
	}
}

func Test_FetchMessageMap_When_FolderNameCarriesControlCharacters_Then_WarningShowsEscapes(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(fetchTwoMessagesHandler(srv))
	c := newClientWithFake(t, srv)
	c.verbose = true
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

	if _, _, err := c.FetchMessageMap(t.Context(), hostileName); err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}

	for _, msg := range logged {
		assertGraphic(t, "log line", msg)
	}
	var warning string
	for _, msg := range logged {
		if strings.Contains(msg, "1 message(s) without Message-Id") {
			warning = msg
		}
	}
	if !strings.Contains(warning, hostileShownName) {
		t.Errorf("warning = %q, want it to hold %q", warning, hostileShownName)
	}
}

func Test_FetchMessageMap_When_ListGivesInboxChildInAnotherCase_Then_ExaminesTheListedName(t *testing.T) {
	t.Parallel()

	srv, c := newLoadedClient(t, &mailboxScript{
		folders:        []scriptFolder{{name: "INBOX"}, {name: "Inbox/Work"}},
		examineRefused: map[string]bool{"INBOX/Work": true},
	})

	subfolders, err := c.ListSubfolders(t.Context(), "INBOX", "")
	if err != nil {
		t.Fatalf("ListSubfolders: %v", err)
	}
	for _, name := range subfolders {
		if _, _, err := c.FetchMessageMap(t.Context(), name); err != nil {
			t.Errorf("FetchMessageMap(%q): %v", name, err)
		}
	}

	if got := srv.capturedNames("EXAMINE"); !slices.Equal(got, []string{"Inbox/Work"}) {
		t.Errorf("EXAMINE names = %q, want only the listed Inbox/Work: the server refuses INBOX/Work", got)
	}
}

func TestCheckUIDValidity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantText string
		planned  uint32
		current  uint32
	}{
		{name: "server sent none on either side"},
		{name: "plan holds none", current: 5},
		{name: "equal values", planned: 5, current: 5},
		{name: "session sends none now", planned: 5, wantText: "(UIDVALIDITY 5, now none)"},
		{name: "different values", planned: 5, current: 6, wantText: "(UIDVALIDITY 5, now 6)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := CheckUIDValidity(tt.planned, tt.current)

			if tt.wantText == "" {
				if err != nil {
					t.Fatalf("CheckUIDValidity(%d, %d) = %v, want nil", tt.planned, tt.current, err)
				}
				return
			}
			if !errors.Is(err, ErrUIDValidityChanged) {
				t.Fatalf("CheckUIDValidity(%d, %d) = %v, want ErrUIDValidityChanged", tt.planned, tt.current, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
			if tt.current == 0 && strings.Contains(err.Error(), "now 0") {
				t.Errorf("error = %q, want it to say none, not 0", err)
			}
		})
	}
}

func Test_FetchMessageMap_When_ExamineReportsUIDValidity_Then_ReturnsIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		uidValidity uint32
	}{
		{name: "server sends a value", uidValidity: 7},
		{name: "server sends none", uidValidity: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, examineAs(tt.uidValidity)))
			c := newClientWithFake(t, srv)

			_, got, err := c.FetchMessageMap(t.Context(), "INBOX")

			if err != nil {
				t.Fatalf("FetchMessageMap: %v", err)
			}
			if got != tt.uidValidity {
				t.Errorf("UIDVALIDITY = %d, want %d", got, tt.uidValidity)
			}
		})
	}
}

func Test_FetchMessageMap_When_FolderStaysSelected_Then_StatusReportsUIDValidity(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(7), statusAs(srv, 9))))
	c := newClientWithFake(t, srv)

	_, first, err := c.FetchMessageMap(t.Context(), "INBOX")
	if err != nil {
		t.Fatalf("first FetchMessageMap: %v", err)
	}
	_, second, err := c.FetchMessageMap(t.Context(), "INBOX")
	if err != nil {
		t.Fatalf("second FetchMessageMap: %v", err)
	}

	if first != 7 {
		t.Errorf("UIDVALIDITY of the first call = %d, want 7 (from EXAMINE)", first)
	}
	if second != 9 {
		t.Errorf("UIDVALIDITY of the second call = %d, want 9 (from STATUS)", second)
	}
	if got := srv.callCount("EXAMINE"); got != 1 {
		t.Errorf("EXAMINE count = %d, want 1 (the second call keeps the selection)", got)
	}
	if got := srv.callCount("STATUS"); got != 1 {
		t.Errorf("STATUS count = %d, want 1", got)
	}
	if got := srv.callCount(statusMessagesAndUIDValidity); got != 1 {
		t.Errorf("STATUS requests with MESSAGES and UIDVALIDITY = %d, want 1", got)
	}
}

func Test_FetchMessageMap_When_RetriedOnANewSession_Then_ReturnsTheNewSessionValue(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(3), byeOn(verbIs("FETCH")))))
	srv.addConnHandler(interceptHandler(srv, examineAs(4)))
	c := newClientWithFake(t, srv)

	_, got, err := c.FetchMessageMap(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after the reconnect", err)
	}
	if got != 4 {
		t.Errorf("UIDVALIDITY = %d, want 4 (the value of the session that served the result)", got)
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2", got)
	}
}

func Test_StreamMessagesByUIDs_When_UIDValidityDiffers_Then_FetchesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		folder string
	}{
		{name: "plain folder name", folder: "INBOX"},
		{name: "folder name holds a transient phrase", folder: "Not logged in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, examineAs(6)))
			c := newClientWithFake(t, srv)

			var got []uint32
			err := c.StreamMessagesByUIDs(t.Context(), tt.folder, 5, []uint32{1, 2, 3}, nil, func(msg Message) error {
				got = append(got, msg.m.Uid)
				return nil
			})

			if !errors.Is(err, ErrUIDValidityChanged) {
				t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged", err)
			}
			for _, want := range []string{"folder " + tt.folder + ": ", "UIDVALIDITY 5, now 6"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if got := classifyError(err); got != ClassUnknown {
				t.Errorf("classifyError(err) = %s, want %s", got, ClassUnknown)
			}
			if isRetryable(err) || canRevive(err) {
				t.Errorf("isRetryable, canRevive = %t, %t, want false, false", isRetryable(err), canRevive(err))
			}
			if len(got) != 0 {
				t.Errorf("callback got UIDs %v, want none", got)
			}
			if got := srv.callCount("UID FETCH"); got != 0 {
				t.Errorf("UID FETCH count = %d, want 0", got)
			}
			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1 (a mismatch is no reason to reconnect)", got)
			}
			if got := srv.callCount("EXAMINE"); got != 1 {
				t.Errorf("EXAMINE count = %d, want 1", got)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_UIDValidityChangesAcrossReconnect_Then_StopsWithoutFetching(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(5), byeAfterUIDFetch(fetches, release))))
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(6), answerUIDFetch(fetches, requestedAsSent))))
	// A third session must not appear: the mismatch ends the stream.
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(6), answerUIDFetch(fetches, requestedAsSent))))
	c := newWatchedClient(t, srv, 0)
	first := c.c.Load()

	uids := make([]uint32, 2*uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	var got []uint32
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 5, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		if msg.m.Uid == uidFetchBatchSize {
			releaseOnce()
			awaitSignal(t, first.LoggedOut(), "the first session to end after the BYE")
		}
		return nil
	})

	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged", err)
	}
	if !slices.Equal(got, uids[:uidFetchBatchSize]) {
		t.Errorf("callback got %d UIDs, want exactly the %d of the first batch", len(got), uidFetchBatchSize)
	}
	if gotSets, want := fetches.all(), [][]uint32{uids[:uidFetchBatchSize]}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH batches = %d, want 1 (the first batch; the new session fetches nothing)", len(gotSets))
	}
	if got := srv.callCount("LOGIN"); got != 2 {
		t.Errorf("LOGIN count = %d, want 2 (one fresh session after the BYE, no third)", got)
	}
	if got := srv.callCount("EXAMINE"); got != 2 {
		t.Errorf("EXAMINE count = %d, want 2", got)
	}
}

func Test_StreamMessagesByUIDs_When_SessionReportsNoUIDValidity_Then_FetchesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		keepSelected bool
	}{
		{name: "fresh EXAMINE"},
		{name: "kept selection", keepSelected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(0), answerUIDFetch(fetches, requestedAsSent))))
			c := newClientWithFake(t, srv)
			uids := []uint32{1, 2, 3}
			if tt.keepSelected {
				if _, uidValidity, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil || uidValidity != 0 {
					t.Fatalf("FetchMessageMap = %d, %v, want 0, nil", uidValidity, err)
				}
				uids = uids[:1]
			}

			var got []uint32
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 5, uids, nil, func(msg Message) error {
				got = append(got, msg.m.Uid)
				return nil
			})

			if !errors.Is(err, ErrUIDValidityChanged) {
				t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged", err)
			}
			for _, want := range []string{"folder INBOX: ", "(UIDVALIDITY 5, now none)"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if got := classifyError(err); got != ClassUnknown {
				t.Errorf("classifyError(err) = %s, want %s", got, ClassUnknown)
			}
			if isRetryable(err) || canRevive(err) {
				t.Errorf("isRetryable, canRevive = %t, %t, want false, false", isRetryable(err), canRevive(err))
			}
			if len(got) != 0 {
				t.Errorf("callback got UIDs %v, want none", got)
			}
			if got := srv.callCount("UID FETCH"); got != 0 {
				t.Errorf("UID FETCH count = %d, want 0", got)
			}
			if got := srv.callCount("LOGIN"); got != 1 {
				t.Errorf("LOGIN count = %d, want 1 (a mismatch is no reason to reconnect)", got)
			}
			if got := srv.callCount("EXAMINE"); got != 1 {
				t.Errorf("EXAMINE count = %d, want 1", got)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_PlanHasNoUIDValidityOrValuesMatch_Then_Streams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		planned uint32
		server  uint32
	}{
		{name: "plan holds none", planned: 0, server: 7},
		{name: "values match", planned: 7, server: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(tt.server), answerUIDFetch(fetches, requestedAsSent))))
			c := newClientWithFake(t, srv)

			uids := []uint32{1, 2, 3}
			var got []uint32
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", tt.planned, uids, nil, func(msg Message) error {
				got = append(got, msg.m.Uid)
				return nil
			})

			if err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v", err)
			}
			if !slices.Equal(got, uids) {
				t.Errorf("callback got UIDs %v, want %v", got, uids)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_KeptSelectionHasOtherUIDValidity_Then_FetchesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantFetch int
		planned   uint32
		wantErr   bool
	}{
		{name: "plan holds another value", planned: 4, wantErr: true},
		{name: "plan holds the value of the selection", planned: 5, wantFetch: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(5), answerUIDFetch(fetches, requestedAsSent))))
			c := newClientWithFake(t, srv)
			if _, uidValidity, err := c.FetchMessageMap(t.Context(), "INBOX"); err != nil || uidValidity != 5 {
				t.Fatalf("FetchMessageMap = %d, %v, want 5, nil", uidValidity, err)
			}

			var got []uint32
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", tt.planned, []uint32{1}, nil, func(msg Message) error {
				got = append(got, msg.m.Uid)
				return nil
			})

			if gotErr := errors.Is(err, ErrUIDValidityChanged); gotErr != tt.wantErr {
				t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged = %t", err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("StreamMessagesByUIDs: %v", err)
			}
			if len(got) != tt.wantFetch {
				t.Errorf("callback ran %d times, want %d", len(got), tt.wantFetch)
			}
			if got := srv.callCount("UID FETCH"); got != tt.wantFetch {
				t.Errorf("UID FETCH count = %d, want %d", got, tt.wantFetch)
			}
			if got := srv.callCount("EXAMINE"); got != 1 {
				t.Errorf("EXAMINE count = %d, want 1 (the stream reads the kept selection)", got)
			}
		})
	}
}

func Test_FetchMessageMap_When_ConnectionDropsMidScan_Then_RetryKeepsEachMessageOnce(t *testing.T) {
	t.Parallel()

	rows := []scanRow{
		{msgID: "dup@x", uid: 9, size: 10},
		{uid: 8, size: 50},
		{msgID: "dup@x", uid: 4, size: 20},
		{msgID: "one@x", uid: 6, size: 30},
		{msgID: "dup@x", uid: 7, size: 40},
	}
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanSession(rows, 3, 3)))
	srv.addConnHandler(interceptHandler(srv, scanSession(rows, len(rows), 4)))
	c := newClientWithFake(t, srv)
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

	got, uidValidity, err := c.FetchMessageMap(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after the reconnect", err)
	}
	want := map[string][]MessageRef{
		"dup@x": {{UID: 4, Size: 20}, {UID: 7, Size: 40}, {UID: 9, Size: 10}},
		"one@x": {{UID: 6, Size: 30}},
	}
	if !equalMessageMaps(got, want) {
		t.Errorf("map = %v, want %v (each message once, none from the first attempt twice)", got, want)
	}
	if uidValidity != 4 {
		t.Errorf("UIDVALIDITY = %d, want 4 (the session that served the result)", uidValidity)
	}
	if n := srv.callCount("LOGIN"); n != 2 {
		t.Errorf("LOGIN count = %d, want 2", n)
	}
	var warnings int
	for _, msg := range logged {
		if strings.Contains(msg, "1 message(s) without Message-Id") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("got %d warnings about a missing Message-Id, want 1 (the first attempt must not add to it); log: %v", warnings, logged)
	}
}

func Test_FetchMessageMap_When_ResponseHasMessageIdButNoUID_Then_ScanFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rows []scanRow
	}{
		{
			name: "the response has no UID item",
			rows: []scanRow{{msgID: "a@x", noUID: true, size: 10}, {msgID: "b@x", uid: 2, size: 20}},
		},
		{
			name: "the response sends UID 0",
			rows: []scanRow{{msgID: "a@x", uid: 0, size: 10}, {msgID: "b@x", uid: 2, size: 20}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, scanIntercept(tt.rows)))
			c := newClientWithFake(t, srv)
			var logged []string
			c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

			got, _, err := c.FetchMessageMap(t.Context(), "INBOX")

			if !errors.Is(err, errMissingUID) {
				t.Fatalf("FetchMessageMap error = %v, want errMissingUID", err)
			}
			if !strings.Contains(err.Error(), "(1 of 2 messages)") {
				t.Errorf("error = %q, want it to count 1 of 2 messages", err)
			}
			if got != nil {
				t.Errorf("map = %v, want nil", got)
			}
			if errors.Is(err, ErrSelectRefused) {
				t.Errorf("error = %v, must not be a select refusal (it would skip a hierarchy level)", err)
			}
			if n := srv.callCount("LOGIN"); n != 1 {
				t.Errorf("LOGIN count = %d, want 1 (no reconnect)", n)
			}
			if n := srv.callCount("FETCH"); n != 1 {
				t.Errorf("FETCH count = %d, want 1 (no retry)", n)
			}
			for _, msg := range logged {
				if strings.Contains(msg, "without Message-Id") {
					t.Errorf("logged %q, want no missing Message-Id warning", msg)
				}
			}
		})
	}
}

func Test_FetchMessageMap_When_ResponseHasNeitherMessageIdNorUID_Then_ScanSucceeds(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanIntercept([]scanRow{
		{noUID: true, size: 5},
		{msgID: "b@x", uid: 2, size: 20},
	})))
	c := newClientWithFake(t, srv)

	got, _, err := c.FetchMessageMap(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}
	if want := map[string][]MessageRef{"b@x": {{UID: 2, Size: 20}}}; !equalMessageMaps(got, want) {
		t.Errorf("map = %v, want %v", got, want)
	}
}

func Test_FetchMessageMap_When_UIDlessScanIsCutAndRetryIsClean_Then_Succeeds(t *testing.T) {
	t.Parallel()

	uidless := []scanRow{{msgID: "a@x", noUID: true, size: 10}, {msgID: "b@x", uid: 2, size: 20}}
	clean := []scanRow{{msgID: "a@x", uid: 1, size: 10}, {msgID: "b@x", uid: 2, size: 20}}
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanSession(uidless, 1, 0)))
	srv.addConnHandler(interceptHandler(srv, scanSession(clean, len(clean), 0)))
	c := newClientWithFake(t, srv)

	got, _, err := c.FetchMessageMap(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageMap: %v, want success after the reconnect", err)
	}
	want := map[string][]MessageRef{"a@x": {{UID: 1, Size: 10}}, "b@x": {{UID: 2, Size: 20}}}
	if !equalMessageMaps(got, want) {
		t.Errorf("map = %v, want %v", got, want)
	}
	if n := srv.callCount("LOGIN"); n != 2 {
		t.Errorf("LOGIN count = %d, want 2", n)
	}
}

func Test_FetchMessageCopies_When_ResponseHasMessageIdButNoUID_Then_KeepsTheCopy(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanIntercept([]scanRow{
		{msgID: "a@x", noUID: true, size: 10},
		{msgID: "b@x", uid: 0, size: 20},
		{msgID: "c@x", uid: 3, size: 30},
	})))
	c := newClientWithFake(t, srv)
	var logged []string
	c.SetProgressWriter(&logCapture{fn: func(msg string) { logged = append(logged, msg) }})

	got, err := c.FetchMessageCopies(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageCopies: %v", err)
	}
	want := map[string][]MessageRef{
		"a@x": {{UID: 0, Size: 10}},
		"b@x": {{UID: 0, Size: 20}},
		"c@x": {{UID: 3, Size: 30}},
	}
	if !equalMessageMaps(got, want) {
		t.Errorf("map = %v, want %v", got, want)
	}
	if n := srv.callCount("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1 (no reconnect)", n)
	}
	if n := srv.callCount("FETCH"); n != 1 {
		t.Errorf("FETCH count = %d, want 1 (no retry)", n)
	}
	for _, msg := range logged {
		if strings.Contains(msg, "without Message-Id") {
			t.Errorf("logged %q, want no missing Message-Id warning", msg)
		}
	}
}

func Test_FetchMessageCopies_When_UIDlessResponsesShareMessageId_Then_CountsEachCopy(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, scanIntercept([]scanRow{
		{msgID: "d@x", noUID: true, size: 10},
		{msgID: "d@x", uid: 0, size: 20},
		{msgID: "d@x", uid: 5, size: 30},
		{msgID: "d@x", uid: 5, size: 30},
	})))
	c := newClientWithFake(t, srv)

	got, err := c.FetchMessageCopies(t.Context(), "INBOX")

	if err != nil {
		t.Fatalf("FetchMessageCopies: %v", err)
	}
	// The order of refs with UID 0 is not defined, so the test fixes one.
	refs := got["d@x"]
	slices.SortFunc(refs, func(a, b MessageRef) int {
		return cmp.Or(cmp.Compare(a.UID, b.UID), cmp.Compare(a.Size, b.Size))
	})
	want := []MessageRef{{UID: 0, Size: 10}, {UID: 0, Size: 20}, {UID: 5, Size: 30}}
	if len(got) != 1 || !slices.Equal(refs, want) {
		t.Errorf("map = %v, want d@x with %v (UID 0 never merges, a repeated UID 5 merges)", got, want)
	}
}

func Test_StreamMessagesByUIDs_When_UIDValidityChangesOnRetryInsideFirstBatch_Then_FetchesNoUndeliveredUID(t *testing.T) {
	t.Parallel()

	const delivered = 2
	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(5), cutUIDFetch(fetches, delivered, true))))
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(6), answerUIDFetch(fetches, requestedAsSent))))
	c := newClientWithFake(t, srv)

	uids := []uint32{1, 2, 3, 4, 5}
	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", 5, uids, nil, func(msg Message) error {
			got = append(got, msg.m.Uid)
			return nil
		})
	})

	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged", err)
	}
	if !slices.Equal(got, uids[:delivered]) {
		t.Errorf("callback got UIDs %v, want only the %v delivered before the drop", got, uids[:delivered])
	}
	if gotSets, want := fetches.all(), [][]uint32{uids}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH asked for %v, want %v: the session with the new UIDVALIDITY must fetch nothing", gotSets, want)
	}
	if n := srv.callCount("LOGIN"); n != 2 {
		t.Errorf("LOGIN count = %d, want 2 (the drop reconnects once, the mismatch does not)", n)
	}
	if n := srv.callCount("EXAMINE"); n != 2 {
		t.Errorf("EXAMINE count = %d, want 2", n)
	}
}

func Test_StreamMessagesByUIDs_When_ResponseCarriesFlags_Then_MessageKeepsTheServerSpelling(t *testing.T) {
	t.Parallel()

	args := make(chan string, 1)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		args <- cmd.arg
		writeUIDMessageWithItems(conn, 1, ` FLAGS (\Seen \FLAGGED $Forwarded NonJunk $MailFlagBit0)`)
		writeUIDMessageWithItems(conn, 2, "")
		writeUIDMessageWithItems(conn, 3, ` FLAGS ()`)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	got := make(map[uint32][]string)
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, nil, func(msg Message) error {
		got[msg.UID()] = msg.m.Flags
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	arg := <-args
	if !strings.Contains(arg, "FLAGS") || !strings.Contains(arg, "BODY.PEEK[]") {
		t.Errorf("UID FETCH arguments = %q, want FLAGS and BODY.PEEK[]", arg)
	}
	if strings.Contains(strings.ReplaceAll(arg, "BODY.PEEK[]", ""), "BODY[") {
		t.Errorf("UID FETCH arguments = %q, want no BODY[] without PEEK", arg)
	}
	if want := []string{`\Seen`, `\FLAGGED`, "$Forwarded", "NonJunk", "$MailFlagBit0"}; !slices.Equal(got[1], want) {
		t.Errorf("flags of UID 1 = %q, want the spelling of the server %q", got[1], want)
	}
	if flags, ok := got[2]; !ok || flags != nil {
		t.Errorf("flags of UID 2 = %#v, want nil for a response without FLAGS", flags)
	}
	if flags := got[3]; flags == nil || len(flags) != 0 {
		t.Errorf("flags of UID 3 = %#v, want an empty slice for FLAGS ()", flags)
	}
}

func Test_StreamMessagesByUIDs_When_ResponseCarriesInternalDate_Then_MessageKeepsIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want  time.Time
		name  string
		extra string
		uid   uint32
	}{
		{
			name:  "a date with a positive offset",
			uid:   1,
			extra: ` INTERNALDATE "17-Jul-2012 09:30:00 +0200"`,
			want:  time.Date(2012, 7, 17, 9, 30, 0, 0, time.FixedZone("", 2*3600)),
		},
		{
			name:  "a day padded with a space, as Cyrus sends it",
			uid:   2,
			extra: ` INTERNALDATE " 2-Jan-2006 15:04:05 -0700"`,
			want:  time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*3600)),
		},
		{
			name:  "a day with no padding",
			uid:   3,
			extra: ` INTERNALDATE "2-Jan-2006 15:04:05 -0700"`,
			want:  time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*3600)),
		},
		{name: "a response with no INTERNALDATE item", uid: 4},
		{name: "text that is not a date", uid: 5, extra: ` INTERNALDATE "not a date"`},
		{name: "a NIL date", uid: 6, extra: ` INTERNALDATE NIL`},
	}
	uids := make([]uint32, len(tests))
	extras := make(map[uint32]string, len(tests))
	for i, tt := range tests {
		uids[i] = tt.uid
		extras[tt.uid] = tt.extra
	}

	args := make(chan string, 1)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		args <- cmd.arg
		for _, uid := range uids {
			writeUIDMessageWithItems(conn, uid, extras[uid])
		}
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	got := make(map[uint32]time.Time)
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
		got[msg.UID()] = msg.m.InternalDate
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	arg := <-args
	for _, item := range []string{"ENVELOPE", "FLAGS", "INTERNALDATE", "BODY.PEEK[]"} {
		if !strings.Contains(arg, item) {
			t.Errorf("UID FETCH arguments = %q, want %s", arg, item)
		}
	}
	if strings.Contains(strings.ReplaceAll(arg, "BODY.PEEK[]", ""), "BODY[") {
		t.Errorf("UID FETCH arguments = %q, want no BODY[] without PEEK", arg)
	}
	for _, tt := range tests {
		date, ok := got[tt.uid]
		if !ok {
			t.Errorf("%s: the callback got no message for UID %d", tt.name, tt.uid)
			continue
		}
		if !date.Equal(tt.want) {
			t.Errorf("%s: InternalDate = %v, want %v", tt.name, date, tt.want)
		}
		_, gotOffset := date.Zone()
		_, wantOffset := tt.want.Zone()
		if gotOffset != wantOffset {
			t.Errorf("%s: offset = %d, want %d", tt.name, gotOffset, wantOffset)
		}
	}
}

func Test_StreamMessagesByUIDs_When_UnilateralOrUnrequestedFetchArrives_Then_CallbackSkipsIt(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		writeLine(conn, `* 7 FETCH (FLAGS (\Seen))`)
		writeUIDMessageWithItems(conn, 1, ` FLAGS (\Flagged)`)
		writeUIDMessageWithItems(conn, 99, ` FLAGS (\Draft)`)
		writeLine(conn, `* 8 FETCH (UID 0 FLAGS (\Draft))`)
		writeUIDMessageWithItems(conn, 2, ` FLAGS (\Answered)`)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	flags := make(map[uint32][]string)
	calls := 0
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2}, nil, func(msg Message) error {
		calls++
		flags[msg.UID()] = msg.m.Flags
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v, want nil", err)
	}

	if calls != 2 || !slices.Equal(flags[1], []string{`\Flagged`}) || !slices.Equal(flags[2], []string{`\Answered`}) {
		t.Errorf("callback ran %d times with flags %v, want UID 1 and UID 2 once each", calls, flags)
	}
}

func Test_uidFetchBodies_When_SessionHasNoSelection_Then_ReturnsErrNoMailboxSelectedAndClosesTheChannel(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newClientWithFake(t, srv)
	messages := make(chan *imap.Message)
	uids := new(imap.SeqSet)
	uids.AddNum(1)

	err := uidFetchBodies(c.c.Load(), uids, []imap.FetchItem{imap.FetchFlags}, messages)

	if !errors.Is(err, imapclient.ErrNoMailboxSelected) {
		t.Fatalf("error = %v, want imapclient.ErrNoMailboxSelected", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError = %v, want %v", got, ClassTransient)
	}
	select {
	case msg, ok := <-messages:
		if ok {
			t.Errorf("received %v, want a closed channel", msg)
		}
	default:
		t.Error("the message channel is still open")
	}
	for range messages {
		t.Error("the closed channel delivered a message")
	}
	if got := srv.callCount("UID FETCH"); got != 0 {
		t.Errorf("UID FETCH count = %d, want 0", got)
	}
}

func Test_bodyFetchHandler_Handle(t *testing.T) {
	t.Parallel()

	type outcome int
	const (
		delivered outcome = iota
		unhandled
		failed
	)
	fetch := func(items ...any) *imap.DataResp {
		return &imap.DataResp{Fields: []any{"1", "FETCH", items}}
	}
	tests := []struct {
		resp      imap.Resp
		name      string
		wantFlags []string
		want      outcome
	}{
		{name: "a response that is not FETCH", resp: &imap.DataResp{Fields: []any{"1", "EXISTS"}}, want: unhandled},
		{name: "a status response", resp: &imap.StatusResp{Type: imap.StatusRespOk}, want: unhandled},
		{name: "FETCH whose sequence number is not a number", resp: &imap.DataResp{Fields: []any{"FETCH", "x", []any{"UID", "5"}}}, want: failed},
		{name: "FETCH with no item list", resp: &imap.DataResp{Fields: []any{"1", "FETCH"}}, want: failed},
		{name: "FETCH whose item list is not a list", resp: &imap.DataResp{Fields: []any{"1", "FETCH", "UID"}}, want: failed},
		{name: "an item list that go-imap cannot parse", resp: fetch(uint32(1), "x"), want: failed},
		{name: "a response with no UID", resp: fetch("FLAGS", []any{`\Seen`}), want: unhandled},
		{name: "a response with UID 0", resp: fetch("UID", "0", "FLAGS", []any{`\Seen`}), want: unhandled},
		{name: "a UID that was not requested", resp: fetch("UID", "6", "FLAGS", []any{`\Seen`}), want: unhandled},
		{
			name:      "flags keep the spelling of the server",
			resp:      fetch("UID", "5", "FLAGS", []any{`\Seen`, `\FLAGGED`, "$Forwarded", "NonJunk"}),
			want:      delivered,
			wantFlags: []string{`\Seen`, `\FLAGGED`, "$Forwarded", "NonJunk"},
		},
		{
			name:      "the item key is lower case and the values are raw strings",
			resp:      fetch(imap.RawString("uid"), "5", imap.RawString("flags"), []any{imap.RawString("$A"), "$B"}),
			want:      delivered,
			wantFlags: []string{"$A", "$B"},
		},
		{
			name:      "the last of two FLAGS items decides, and the first list is longer",
			resp:      fetch("FLAGS", []any{`\A`, `\B`, "$Long"}, "UID", "5", "FLAGS", []any{"$Last"}),
			want:      delivered,
			wantFlags: []string{"$Last"},
		},
		{
			name:      "the last of two FLAGS items decides, and the first list is shorter",
			resp:      fetch("FLAGS", []any{"$X"}, "UID", "5", "FLAGS", []any{"$Y", "$Z"}),
			want:      delivered,
			wantFlags: []string{"$Y", "$Z"},
		},
		{
			name:      "a literal item keeps the text that go-imap parsed",
			resp:      fetch("UID", "5", "FLAGS", []any{"$Atom", bytes.NewBufferString("LitFlag"), "$Next"}),
			want:      delivered,
			wantFlags: []string{"$Atom", "litflag", "$Next"},
		},
		{
			name:      "a NIL item keeps the text that go-imap parsed",
			resp:      fetch("UID", "5", "FLAGS", []any{"$Atom", nil}),
			want:      delivered,
			wantFlags: []string{"$Atom", ""},
		},
		{name: "an empty flag list", resp: fetch("UID", "5", "FLAGS", []any{}), want: delivered, wantFlags: []string{}},
		{name: "a response with no FLAGS item", resp: fetch("UID", "5"), want: delivered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uids := new(imap.SeqSet)
			uids.AddNum(5)
			messages := make(chan *imap.Message, 1)
			h := &bodyFetchHandler{messages: messages, uids: uids}

			err := h.Handle(tt.resp)

			switch tt.want {
			case unhandled:
				if !errors.Is(err, responses.ErrUnhandled) {
					t.Errorf("error = %v, want responses.ErrUnhandled", err)
				}
			case failed:
				if err == nil || errors.Is(err, responses.ErrUnhandled) {
					t.Errorf("error = %v, want a failure that is not ErrUnhandled", err)
				}
			case delivered:
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				msg := <-messages
				if msg.Uid != 5 {
					t.Errorf("UID = %d, want 5", msg.Uid)
				}
				if !slices.Equal(msg.Flags, tt.wantFlags) || (msg.Flags == nil) != (tt.wantFlags == nil) {
					t.Errorf("flags = %#v, want %#v", msg.Flags, tt.wantFlags)
				}
			}
			if tt.want != delivered && len(messages) != 0 {
				t.Errorf("the handler sent %d messages, want none", len(messages))
			}
		})
	}
}

// uidSearchLog records the UIDs and the argument of each UID SEARCH, one entry per command.
type uidSearchLog struct {
	args     []string
	requests [][]uint32
	mu       sync.Mutex
}

func (l *uidSearchLog) record(arg string, uids []uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.args = append(l.args, arg)
	l.requests = append(l.requests, uids)
}

func (l *uidSearchLog) allArgs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.args)
}

func (l *uidSearchLog) allRequests() [][]uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

func isUIDSearch(cmd command) bool {
	return cmd.verb == "UID" && strings.HasPrefix(strings.ToUpper(cmd.arg), "SEARCH")
}

// searchedUIDs expands the UID set of a "SEARCH UID <set>" command.
func searchedUIDs(cmd command) []uint32 {
	fields := strings.Fields(cmd.arg)
	if len(fields) != 3 || !strings.EqualFold(fields[1], "UID") {
		return nil
	}
	set, err := imap.ParseSeqSet(fields[2])
	if err != nil {
		return nil
	}
	var uids []uint32
	for _, r := range set.Set {
		for uid := r.Start; uid <= r.Stop; uid++ {
			uids = append(uids, uid)
		}
	}
	return uids
}

// holding picks, from the requested UIDs, those that the server holds.
func holding(held ...uint32) func(requested []uint32) []uint32 {
	return func(requested []uint32) []uint32 {
		var hits []uint32
		for _, uid := range requested {
			if slices.Contains(held, uid) {
				hits = append(hits, uid)
			}
		}
		return hits
	}
}

// answerUIDSearch is an interceptHandler hook that logs each UID SEARCH, answers it with the UIDs that reply picks for the requested set, then sends the tagged OK.
func answerUIDSearch(log *uidSearchLog, reply func(requested []uint32) []uint32) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDSearch(cmd) {
			return false, true
		}
		requested := searchedUIDs(cmd)
		log.record(cmd.arg, requested)
		line := "* SEARCH"
		for _, uid := range reply(requested) {
			line += " " + strconv.FormatUint(uint64(uid), 10)
		}
		writeLine(conn, "%s", line)
		writeLine(conn, "%s OK UID SEARCH completed", cmd.tag)
		return true, true
	}
}

func Test_ExistingUIDs_When_ServerAnswers_Then_ResultIsTheHeldSubsetOfTheInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reply   func(requested []uint32) []uint32
		name    string
		wantArg string
		asked   []uint32
		want    []uint32
	}{
		{name: "the server holds some UIDs", asked: []uint32{5, 3, 1, 4}, reply: holding(1, 4, 5, 9), wantArg: "SEARCH UID 1,3:5", want: []uint32{1, 4, 5}},
		{name: "the server holds none", asked: []uint32{7, 8}, reply: holding(), wantArg: "SEARCH UID 7:8"},
		{name: "the server holds all", asked: []uint32{2, 1, 3}, reply: holding(1, 2, 3), wantArg: "SEARCH UID 1:3", want: []uint32{1, 2, 3}},
		{name: "the input repeats a UID", asked: []uint32{2, 2, 1, 1}, reply: holding(1, 2), wantArg: "SEARCH UID 1:2", want: []uint32{1, 2}},
		{name: "the input holds UID 0", asked: []uint32{0, 3, 0}, reply: holding(3), wantArg: "SEARCH UID 3", want: []uint32{3}},
		{
			name:  "the reply names UIDs nobody asked for and repeats others",
			asked: []uint32{1, 2, 3},
			reply: func([]uint32) []uint32 {
				return []uint32{3, 3, 9, 1, 1, 0, math.MaxUint32, 3}
			},
			wantArg: "SEARCH UID 1:3",
			want:    []uint32{1, 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			searches := new(uidSearchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, tt.reply)))
			c := newClientWithFake(t, srv)
			input := slices.Clone(tt.asked)

			got, err := c.ExistingUIDs(t.Context(), "INBOX", 0, tt.asked)
			if err != nil {
				t.Fatalf("ExistingUIDs: %v", err)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("ExistingUIDs = %v, want %v", got, tt.want)
			}
			if len(got) > len(tt.asked) {
				t.Errorf("ExistingUIDs returned %d UIDs for %d asked", len(got), len(tt.asked))
			}
			if !slices.Equal(tt.asked, input) {
				t.Errorf("ExistingUIDs changed its input to %v, was %v", tt.asked, input)
			}
			assertStrings(t, "UID SEARCH arguments", searches.allArgs(), tt.wantArg)
			for _, verb := range []string{"FETCH", "UID FETCH", "STORE", "UID STORE"} {
				if n := srv.callCount(verb); n != 0 {
					t.Errorf("server saw %d %s commands, want 0: the check reads no message", n, verb)
				}
			}
		})
	}
}

func assertStrings(t *testing.T, what string, got []string, want ...string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func Test_ExistingUIDs_When_NothingIsAsked_Then_SendsNoCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		asked []uint32
	}{
		{name: "nil input", asked: nil},
		{name: "empty input", asked: []uint32{}},
		{name: "only UID 0", asked: []uint32{0}},
		{name: "UID 0 twice", asked: []uint32{0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			searches := new(uidSearchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, holding(0, 1))))
			c := newClientWithFake(t, srv)

			got, err := c.ExistingUIDs(t.Context(), "INBOX", 5, tt.asked)

			if err != nil || got != nil {
				t.Errorf("ExistingUIDs = %v, %v, want nil, nil", got, err)
			}
			for _, verb := range []string{"EXAMINE", "UID SEARCH", "LOGIN"} {
				want := 0
				if verb == "LOGIN" {
					want = 1
				}
				if n := srv.callCount(verb); n != want {
					t.Errorf("server saw %d %s commands, want %d", n, verb, want)
				}
			}
		})
	}
}

func Test_ExistingUIDs_When_MoreThanOneBatchIsAsked_Then_EachCommandCarriesAtMost500UIDs(t *testing.T) {
	t.Parallel()

	searches := new(uidSearchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, requestedAsSent)))
	c := newClientWithFake(t, srv)
	const total = 2*uidFetchBatchSize + 1
	asked := make([]uint32, total)
	for i := range asked {
		asked[i] = uint32(total - i)
	}

	got, err := c.ExistingUIDs(t.Context(), "INBOX", 0, asked)
	if err != nil {
		t.Fatalf("ExistingUIDs: %v", err)
	}

	want := make([]uint32, total)
	for i := range want {
		want[i] = uint32(i + 1)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ExistingUIDs returned %d UIDs, want the %d asked ones in ascending order", len(got), total)
	}
	assertStrings(t, "UID SEARCH arguments", searches.allArgs(), "SEARCH UID 1:500", "SEARCH UID 501:1000", "SEARCH UID 1001")
	for i, request := range searches.allRequests() {
		if len(request) > uidFetchBatchSize {
			t.Errorf("UID SEARCH #%d carried %d UIDs, want at most %d", i+1, len(request), uidFetchBatchSize)
		}
	}
	if n := srv.callCount("EXAMINE"); n != 1 {
		t.Errorf("EXAMINE count = %d, want 1 for all batches", n)
	}
}

func Test_ExistingUIDs_When_SecondBatchFails_Then_ReturnsNoUIDsAndTheError(t *testing.T) {
	t.Parallel()

	searches := new(uidSearchLog)
	srv := newFakeServer(t)
	var seen atomic.Int32
	refuseSecond := func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDSearch(cmd) || seen.Add(1) != 2 {
			return false, true
		}
		writeLine(conn, "%s NO [CANNOT] search refused", cmd.tag)
		return true, true
	}
	srv.addConnHandler(interceptHandler(srv, chainHooks(refuseSecond, answerUIDSearch(searches, requestedAsSent))))
	c := newClientWithFake(t, srv)
	asked := make([]uint32, 2*uidFetchBatchSize+1)
	for i := range asked {
		asked[i] = uint32(i + 1)
	}

	got, err := c.ExistingUIDs(t.Context(), "INBOX", 0, asked)

	if err == nil || !strings.Contains(err.Error(), "search refused") {
		t.Fatalf("ExistingUIDs error = %v, want the refusal", err)
	}
	if got != nil {
		t.Errorf("ExistingUIDs = %d UIDs, want none: the first batch alone proves nothing about the rest", len(got))
	}
	if n := srv.callCount("UID SEARCH"); n != 2 {
		t.Errorf("UID SEARCH count = %d, want 2: the call stops at the first failed batch", n)
	}
}

func Test_ExistingUIDs_When_FolderIsAlreadySelected_Then_SendsNoSecondExamine(t *testing.T) {
	t.Parallel()

	searches := new(uidSearchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, holding(1, 2))))
	c := newClientWithFake(t, srv)

	for range 2 {
		if _, err := c.ExistingUIDs(t.Context(), "INBOX", 0, []uint32{1, 2}); err != nil {
			t.Fatalf("ExistingUIDs: %v", err)
		}
	}

	assertStrings(t, "EXAMINE names", srv.capturedNames("EXAMINE"), "INBOX")
	if n := srv.callCount("UID SEARCH"); n != 2 {
		t.Errorf("UID SEARCH count = %d, want 2", n)
	}
}

func Test_ExistingUIDs_When_UIDValidityDiffers_Then_SendsNoSearch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		wantErr      string
		server       uint32
		planned      uint32
		keepSelected bool
	}{
		{name: "another value after a fresh EXAMINE", server: 6, planned: 5, wantErr: "UIDVALIDITY 5, now 6"},
		{name: "another value on a kept selection", server: 6, planned: 5, keepSelected: true, wantErr: "UIDVALIDITY 5, now 6"},
		{name: "no value after a fresh EXAMINE", server: 0, planned: 5, wantErr: "(UIDVALIDITY 5, now none)"},
		{name: "no value on a kept selection", server: 0, planned: 5, keepSelected: true, wantErr: "(UIDVALIDITY 5, now none)"},
		{name: "plan holds none", server: 7, planned: 0},
		{name: "values match", server: 7, planned: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			searches := new(uidSearchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(tt.server), answerUIDSearch(searches, holding(1, 2)))))
			c := newClientWithFake(t, srv)
			priming := 0
			if tt.keepSelected {
				if _, err := c.ExistingUIDs(t.Context(), "INBOX", 0, []uint32{1}); err != nil {
					t.Fatalf("priming ExistingUIDs: %v", err)
				}
				priming = 1
			}

			got, err := c.ExistingUIDs(t.Context(), "INBOX", tt.planned, []uint32{1, 2})

			if tt.wantErr == "" {
				if err != nil || !slices.Equal(got, []uint32{1, 2}) {
					t.Fatalf("ExistingUIDs = %v, %v, want [1 2], nil", got, err)
				}
				return
			}
			if !errors.Is(err, ErrUIDValidityChanged) {
				t.Fatalf("ExistingUIDs error = %v, want ErrUIDValidityChanged", err)
			}
			for _, want := range []string{"folder INBOX: ", tt.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if got := classifyError(err); got != ClassUnknown {
				t.Errorf("classifyError(err) = %s, want %s", got, ClassUnknown)
			}
			if got != nil {
				t.Errorf("ExistingUIDs = %v, want none", got)
			}
			if n := srv.callCount("UID SEARCH"); n != priming {
				t.Errorf("UID SEARCH count = %d, want %d: the mismatch sends no search", n, priming)
			}
			if n := srv.callCount("EXAMINE"); n != 1 {
				t.Errorf("EXAMINE count = %d, want 1", n)
			}
			if n := srv.callCount("LOGIN"); n != 1 {
				t.Errorf("LOGIN count = %d, want 1: a mismatch is no reason to reconnect", n)
			}
		})
	}
}

func Test_ExistingUIDs_When_ServerRefusesTheSearch_Then_ReturnsTheLabeledErrorWithoutReconnect(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDSearch(cmd) {
			return false, true
		}
		writeLine(conn, "%s NO [CANNOT] search refused", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)
	c.prefix = "src"

	got, err := c.ExistingUIDs(t.Context(), "Work", 0, []uint32{1})

	if err == nil {
		t.Fatal("ExistingUIDs returned nil, want the refusal")
	}
	for _, want := range []string{"[src] check UIDs in Work: ", "search refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if got != nil {
		t.Errorf("ExistingUIDs = %v, want none", got)
	}
	if n := srv.callCount("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1: a refusal is no reason to reconnect", n)
	}
	if n := srv.callCount("UID SEARCH"); n != 1 {
		t.Errorf("UID SEARCH count = %d, want 1", n)
	}
}

func Test_ExistingUIDs_When_SessionEndsDuringTheSearch_Then_ReconnectsSelectsAgainAndSearchesAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hook func(net.Conn, *bufio.Reader, command) (handled, keep bool)
		name string
	}{
		{name: "the connection drops", hook: dropThenHold(isUIDSearch)},
		{name: "the server sends BYE", hook: byeOn(isUIDSearch)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			searches := new(uidSearchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.hook))
			srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, holding(1, 3))))
			c := newWatchedClient(t, srv, 0)

			got, err := awaitCallUIDs(t, func() ([]uint32, error) {
				return c.ExistingUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3})
			})

			if err != nil {
				t.Fatalf("ExistingUIDs: %v, want success after the reconnect", err)
			}
			if want := []uint32{1, 3}; !slices.Equal(got, want) {
				t.Errorf("ExistingUIDs = %v, want %v", got, want)
			}
			for verb, want := range map[string]int{"LOGIN": 2, "EXAMINE": 2, "UID SEARCH": 2} {
				if n := srv.callCount(verb); n != want {
					t.Errorf("%s count = %d, want %d", verb, n, want)
				}
			}
		})
	}
}

// awaitCallUIDs is awaitCall for a call that returns UIDs.
func awaitCallUIDs(t *testing.T, call func() ([]uint32, error)) ([]uint32, error) {
	t.Helper()

	var got []uint32
	err := awaitCall(t, "ExistingUIDs", func() error {
		var err error
		got, err = call()
		return err
	})
	return got, err
}

func Test_ExistingUIDs_When_ContextEnded_Then_ReturnsItsErrorAndSendsNoCommand(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDSearch(new(uidSearchLog), holding(1))))
	c := newClientWithFake(t, srv)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := c.ExistingUIDs(ctx, "INBOX", 0, []uint32{1})

	if !errors.Is(err, context.Canceled) || got != nil {
		t.Errorf("ExistingUIDs = %v, %v, want nil, context.Canceled", got, err)
	}
	for _, verb := range []string{"EXAMINE", "UID SEARCH"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("server saw %d %s commands, want 0", n, verb)
		}
	}
}

func Test_ExistingUIDs_When_ContextEndsDuringTheSearch_Then_ReturnsItsError(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	st := newStall(isUIDSearch)
	srv.addConnHandler(interceptHandler(srv, st.intercept))
	c := newClientWithFake(t, srv)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type result struct {
		err error
		got []uint32
	}
	done := make(chan result, 1)
	go func() {
		got, err := c.ExistingUIDs(ctx, "INBOX", 0, []uint32{1})
		done <- result{got: got, err: err}
	}()

	awaitSignal(t, st.reached, "the server to receive UID SEARCH")
	cancel()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) || r.got != nil {
			t.Errorf("ExistingUIDs = %v, %v, want nil, context.Canceled", r.got, r.err)
		}
	case <-timer.C:
		t.Fatal("ExistingUIDs still blocked 5s after the cancel")
	}
	if n := srv.callCount("LOGIN"); n != 1 {
		t.Errorf("LOGIN count = %d, want 1: a canceled client must not reconnect", n)
	}
}

func Test_uidSearchIDs_When_SessionHasNoSelection_Then_ReturnsErrNoMailboxSelectedAndSendsNothing(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newClientWithFake(t, srv)
	uids := new(imap.SeqSet)
	uids.AddNum(1)

	got, err := uidSearchIDs(c.c.Load(), uids)

	if !errors.Is(err, imapclient.ErrNoMailboxSelected) {
		t.Fatalf("error = %v, want imapclient.ErrNoMailboxSelected", err)
	}
	if got := classifyError(err); got != ClassTransient {
		t.Errorf("classifyError = %v, want %v", got, ClassTransient)
	}
	if got != nil {
		t.Errorf("uidSearchIDs = %v, want none", got)
	}
	if n := srv.callCount("UID SEARCH"); n != 0 {
		t.Errorf("UID SEARCH count = %d, want 0", n)
	}
}

// countedContext reports cancellation through Err once the server has seen n commands of the verb, such as "UID SEARCH". Its Done channel stays nil, so nothing cancels the client.
type countedContext struct {
	context.Context
	srv  *fakeServer
	verb string
	n    int
}

func (c countedContext) Done() <-chan struct{} { return nil }

func (c countedContext) Err() error {
	if c.srv.callCount(c.verb) >= c.n {
		return context.Canceled
	}
	return nil
}

func Test_ExistingUIDs_When_ContextEndsAfterAReply_Then_ReturnsItsErrorAndSendsNoMoreSearch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		asked        int
		wantSearches int
	}{
		{name: "after the only batch", asked: 3, wantSearches: 1},
		{name: "between two batches", asked: uidFetchBatchSize + 1, wantSearches: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, answerUIDSearch(new(uidSearchLog), requestedAsSent)))
			c := newClientWithFake(t, srv)
			asked := make([]uint32, tt.asked)
			for i := range asked {
				asked[i] = uint32(i + 1)
			}

			got, err := c.ExistingUIDs(countedContext{Context: t.Context(), srv: srv, verb: "UID SEARCH", n: 1}, "INBOX", 0, asked)

			if !errors.Is(err, context.Canceled) || got != nil {
				t.Errorf("ExistingUIDs = %v, %v, want nil, context.Canceled", got, err)
			}
			if n := srv.callCount("UID SEARCH"); n != tt.wantSearches {
				t.Errorf("UID SEARCH count = %d, want %d", n, tt.wantSearches)
			}
		})
	}
}

// sequentialUIDs returns the UIDs 1 to n.
func sequentialUIDs(n int) []uint32 {
	uids := make([]uint32, n)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	return uids
}

// without returns a send function for answerUIDFetchEnding that sends every requested UID except skip.
func without(skip ...uint32) func(requested []uint32) []uint32 {
	return func(requested []uint32) []uint32 {
		return slices.DeleteFunc(slices.Clone(requested), func(uid uint32) bool { return slices.Contains(skip, uid) })
	}
}

func sendNothing([]uint32) []uint32 { return nil }

// answerUIDFetchEnding is answerUIDFetch with its own tagged status. status gives the text for the n-th UID FETCH of the server, counted from 1; empty means OK.
func answerUIDFetchEnding(log *uidFetchLog, send func(requested []uint32) []uint32, status func(n int) string) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	var fetches atomic.Int32
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		for _, uid := range send(requested) {
			writeUIDMessage(conn, uid)
		}
		if reply := status(int(fetches.Add(1))); reply != "" {
			writeLine(conn, "%s %s", cmd.tag, reply)
		} else {
			writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		}
		return true, true
	}
}

// refuseBatches gives answerUIDFetchEnding the status text reply for each UID FETCH whose number is in batches.
func refuseBatches(reply string, batches ...int) func(n int) string {
	return func(n int) string {
		if slices.Contains(batches, n) {
			return reply
		}
		return ""
	}
}

// streamedUIDs collects the UIDs that StreamMessagesByUIDs hands to the callback.
func streamedUIDs(ctx context.Context, c *Client, folder string, uidValidity uint32, uids []uint32) ([]uint32, error) {
	var got []uint32
	err := c.StreamMessagesByUIDs(ctx, folder, uidValidity, uids, nil, func(msg Message) error {
		got = append(got, msg.m.Uid)
		return nil
	})
	return got, err
}

// streamErrorOf returns the *StreamError in err and fails the test when err holds none.
func streamErrorOf(t *testing.T, err error) *StreamError {
	t.Helper()

	se, ok := errors.AsType[*StreamError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want a *StreamError", err, err)
	}
	return se
}

func Test_StreamMessagesByUIDs_When_ABatchIsRefused_Then_LaterBatchesAreStillFetched(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDFetchEnding(fetches, without(3), refuseBatches("NO Some messages could not be FETCHed (Failure)", 1))))
	c := newClientWithFake(t, srv)
	uids := sequentialUIDs(2*uidFetchBatchSize + 1)

	got, err := streamedUIDs(t.Context(), c, "INBOX", 0, uids)

	want := [][]uint32{uids[:uidFetchBatchSize], uids[uidFetchBatchSize : 2*uidFetchBatchSize], uids[2*uidFetchBatchSize:]}
	if gotSets := fetches.all(); !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH sets = %d, want the 3 batches [1..500], [501..1000], [1001] and no retry of UID 3", len(gotSets))
	}
	if wantUIDs := slices.DeleteFunc(slices.Clone(uids), func(uid uint32) bool { return uid == 3 }); !slices.Equal(got, wantUIDs) {
		t.Errorf("callback got %d UIDs, want every UID except 3, once, in ascending order (%d)", len(got), len(wantUIDs))
	}
	se := streamErrorOf(t, err)
	if se.FailedBatches != 1 || se.Stop != nil {
		t.Errorf("FailedBatches, Stop = %d, %v, want 1, nil", se.FailedBatches, se.Stop)
	}
	if se.First == nil || !strings.Contains(se.First.Error(), "body fetch: Some messages could not be FETCHed (Failure)") {
		t.Errorf("First = %v, want the refusal of the first batch", se.First)
	}
	if strings.Contains(err.Error(), "batches failed") {
		t.Errorf("error = %q, want no batch count for one failed batch", err)
	}
	if got := classifyError(err); got != ClassUnknown {
		t.Errorf("classifyError(err) = %s, want %s", got, ClassUnknown)
	}
	for verb, wantCount := range map[string]int{"LOGIN": 1, "EXAMINE": 1} {
		if n := srv.callCount(verb); n != wantCount {
			t.Errorf("%s count = %d, want %d", verb, n, wantCount)
		}
	}
}

func Test_StreamMessagesByUIDs_When_FetchResponseHasNoItemList_Then_OnlyItsBatchFails(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	var fetchCount atomic.Int32
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		fetches.record(requested)
		if fetchCount.Add(1) == 1 {
			writeUIDMessage(conn, 1)
			writeLine(conn, "* 7 FETCH")
			writeUIDMessage(conn, 2)
		} else {
			for _, uid := range requested {
				writeUIDMessage(conn, uid)
			}
		}
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)
	uids := sequentialUIDs(uidFetchBatchSize + 1)

	got, err := streamedUIDs(t.Context(), c, "INBOX", 0, uids)

	want := [][]uint32{uids[:uidFetchBatchSize], uids[uidFetchBatchSize:]}
	if gotSets := fetches.all(); !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH sets = %d, want the 2 batches [1..500], [501] and no retry of the first", len(gotSets))
	}
	if wantUIDs := []uint32{1, uint32(uidFetchBatchSize + 1)}; !slices.Equal(got, wantUIDs) {
		t.Errorf("callback got %v, want %v: the message after the refused response is not delivered", got, wantUIDs)
	}
	se := streamErrorOf(t, err)
	if se.FailedBatches != 1 || se.Stop != nil {
		t.Errorf("FailedBatches, Stop = %d, %v, want 1, nil", se.FailedBatches, se.Stop)
	}
	if !errors.Is(se.First, errFetchNotEnoughFields) {
		t.Errorf("First = %v, want errFetchNotEnoughFields", se.First)
	}
	if got := classifyError(err); got != ClassUnknown {
		t.Errorf("classifyError(err) = %s, want %s", got, ClassUnknown)
	}
	for verb, wantCount := range map[string]int{"LOGIN": 1, "EXAMINE": 1} {
		if n := srv.callCount(verb); n != wantCount {
			t.Errorf("%s count = %d, want %d", verb, n, wantCount)
		}
	}
}

// throttleReply is how Gmail ends a command once the account is over its limits.
const throttleReply = "NO [OVERQUOTA] Account exceeded command or bandwidth limits"

func Test_StreamMessagesByUIDs_When_SeveralBatchesFail_Then_ErrorKeepsTheFirstAndCountsThem(t *testing.T) {
	t.Parallel()

	uids := sequentialUIDs(2*uidFetchBatchSize + 1)
	numbered := func(n int) string { return fmt.Sprintf("NO refusal %d", n) }
	onlyTheMiddleBatch := func(requested []uint32) []uint32 {
		if requested[0] == uids[uidFetchBatchSize] {
			return requested
		}
		return nil
	}
	tests := []struct {
		hook         func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool)
		name         string
		wantFirst    string
		wantUIDs     []uint32
		wantFailed   int
		wantFetches  int
		wantExamines int
	}{
		{
			name: "every batch is refused",
			hook: func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
				return answerUIDFetchEnding(fetches, sendNothing, numbered)
			},
			wantFirst:    "body fetch: refusal 1",
			wantFailed:   3,
			wantFetches:  3,
			wantExamines: 1,
		},
		{
			name: "the first and the last batch are refused",
			hook: func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
				return answerUIDFetchEnding(fetches, onlyTheMiddleBatch, func(n int) string {
					if n == 2 {
						return ""
					}
					return numbered(n)
				})
			},
			wantFirst:    "body fetch: refusal 1",
			wantUIDs:     uids[uidFetchBatchSize : 2*uidFetchBatchSize],
			wantFailed:   2,
			wantFetches:  3,
			wantExamines: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.hook(fetches)))
			c := newClientWithFake(t, srv)

			got, err := streamedUIDs(t.Context(), c, "INBOX", 0, uids)

			se := streamErrorOf(t, err)
			if se.FailedBatches != tt.wantFailed || se.Stop != nil {
				t.Errorf("FailedBatches, Stop = %d, %v, want %d, nil", se.FailedBatches, se.Stop, tt.wantFailed)
			}
			if se.First == nil || !strings.Contains(se.First.Error(), tt.wantFirst) {
				t.Errorf("First = %v, want the failure of the first batch (%q)", se.First, tt.wantFirst)
			}
			if want := fmt.Sprintf(" (%d batches failed)", tt.wantFailed); !strings.HasSuffix(err.Error(), want) {
				t.Errorf("error = %q, want it to end with %q", err, want)
			}
			if !slices.Equal(got, tt.wantUIDs) {
				t.Errorf("callback got %d UIDs, want %d", len(got), len(tt.wantUIDs))
			}
			for verb, want := range map[string]int{"UID FETCH": tt.wantFetches, "EXAMINE": tt.wantExamines, "LOGIN": 1} {
				if n := srv.callCount(verb); n != want {
					t.Errorf("%s count = %d, want %d", verb, n, want)
				}
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_ExamineIsRefused_Then_StreamEndsAfterOneExamine(t *testing.T) {
	t.Parallel()

	uids := sequentialUIDs(2*uidFetchBatchSize + 1)
	tests := []struct {
		name      string
		folder    string
		reply     string
		wantReply string
		wantClass ErrClass
	}{
		{name: "plain refusal", folder: "INBOX", reply: "NO [CANNOT] examine refused", wantReply: "examine refused", wantClass: ClassUnknown},
		{name: "missing mailbox", folder: "Archive", reply: "NO No such mailbox", wantReply: "No such mailbox", wantClass: ClassPermanent},
		{name: "bad reply", folder: "Archive", reply: "BAD Invalid mailbox name", wantReply: "Invalid mailbox name", wantClass: ClassUnknown},
		{name: "folder name holds a throttle phrase", folder: "Account exceeded bandwidth limits", reply: "NO Mailbox doesn't exist", wantReply: "Mailbox doesn't exist", wantClass: ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
				if cmd.verb != "EXAMINE" {
					return false, true
				}
				writeLine(conn, "%s %s", cmd.tag, tt.reply)
				return true, true
			}))
			c := newClientWithFake(t, srv)

			got, err := streamedUIDs(t.Context(), c, tt.folder, 0, uids)

			se := streamErrorOf(t, err)
			if se.FailedBatches != 1 {
				t.Errorf("FailedBatches = %d, want 1", se.FailedBatches)
			}
			if !errors.Is(se.Stop, ErrSelectRefused) || !errors.Is(err, ErrSelectRefused) {
				t.Errorf("Stop = %v, err = %v, want both marked ErrSelectRefused", se.Stop, err)
			}
			if se.First == nil || !errors.Is(se.First, se.Stop) {
				t.Errorf("First = %v, want the same error as Stop (%v)", se.First, se.Stop)
			}
			if want := "cannot select folder " + tt.folder + ": " + tt.wantReply; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
			if strings.Contains(err.Error(), "batches failed") {
				t.Errorf("error = %q, want no batch count: one batch failed", err)
			}
			if got := classifyError(err); got != tt.wantClass || got != classifyError(errors.New(tt.wantReply)) {
				t.Errorf("classifyError(err) = %s, want %s: the folder name must not set the class", got, tt.wantClass)
			}
			if IsThrottled(se.Stop) {
				t.Errorf("IsThrottled(Stop) = true, want false")
			}
			if len(got) != 0 {
				t.Errorf("callback got %d UIDs, want none", len(got))
			}
			for verb, want := range map[string]int{"EXAMINE": 1, "UID FETCH": 0, "LOGIN": 1} {
				if n := srv.callCount(verb); n != want {
					t.Errorf("%s count = %d, want %d: no batch follows a refused EXAMINE", verb, n, want)
				}
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_FolderIsRefusedAfterTheFirstBatch_Then_LaterBatchesAreNotRequested(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, byeAfterUIDFetch(fetches, release)))
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "EXAMINE" {
			return false, true
		}
		writeLine(conn, "%s NO [NONEXISTENT] Unknown Mailbox", cmd.tag)
		return true, true
	}))
	// A third session must not appear: the refusal ends the stream.
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	first := c.c.Load()
	uids := sequentialUIDs(2*uidFetchBatchSize + 1)

	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
			got = append(got, msg.m.Uid)
			if msg.m.Uid == uidFetchBatchSize {
				releaseOnce()
				awaitSignal(t, first.LoggedOut(), "the first session to end after the BYE")
			}
			return nil
		})
	})

	se := streamErrorOf(t, err)
	if !errors.Is(err, ErrSelectRefused) || se.FailedBatches != 1 || !errors.Is(se.First, se.Stop) {
		t.Errorf("err = %v, FailedBatches = %d, want ErrSelectRefused after 1 failed batch, First the same as Stop", err, se.FailedBatches)
	}
	if strings.Contains(err.Error(), "batches failed") {
		t.Errorf("error = %q, want no batch count: one batch failed", err)
	}
	if !slices.Equal(got, uids[:uidFetchBatchSize]) {
		t.Errorf("callback got %d UIDs, want exactly the %d of the first batch", len(got), uidFetchBatchSize)
	}
	if gotSets, want := fetches.all(), [][]uint32{uids[:uidFetchBatchSize]}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("UID FETCH batches = %d, want 1 (the first batch; the refused session fetches nothing)", len(gotSets))
	}
	for verb, want := range map[string]int{"LOGIN": 2, "EXAMINE": 2} {
		if n := srv.callCount(verb); n != want {
			t.Errorf("%s count = %d, want %d", verb, n, want)
		}
	}
}

func Test_StreamMessagesByUIDs_When_ABatchFailsAfterItsRetry_Then_TheNextBatchRunsOnANewSession(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, dropThenHold(isUIDFetch)))
	srv.addConnHandler(interceptHandler(srv, dropThenHold(isUIDFetch)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	uids := sequentialUIDs(2*uidFetchBatchSize + 1)

	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		var err error
		got, err = streamedUIDs(t.Context(), c, "INBOX", 0, uids)
		return err
	})

	se := streamErrorOf(t, err)
	if se.FailedBatches != 1 || se.Stop != nil {
		t.Errorf("FailedBatches, Stop = %d, %v, want 1, nil", se.FailedBatches, se.Stop)
	}
	if got := classifyError(se.First); got != ClassTransient {
		t.Errorf("classifyError(First) = %s, want %s: both sessions dropped", got, ClassTransient)
	}
	if want := uids[uidFetchBatchSize:]; !slices.Equal(got, want) {
		t.Errorf("callback got %d UIDs, want the %d of the second and third batch, each once", len(got), len(want))
	}
	if gotSets, want := fetches.all(), [][]uint32{uids[uidFetchBatchSize : 2*uidFetchBatchSize], uids[2*uidFetchBatchSize:]}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("answered UID FETCH sets = %d, want the second and third batch", len(gotSets))
	}
	if n := srv.callCount("LOGIN"); n != 3 {
		t.Errorf("LOGIN count = %d, want 3: the first batch used two sessions and the next batch a third", n)
	}
}

func Test_StreamMessagesByUIDs_When_ExamineIsCutOnBothSessions_Then_ItIsNoRefusalAndTheNextBatchRuns(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, dropThenHold(verbIs("EXAMINE"))))
	srv.addConnHandler(interceptHandler(srv, dropThenHold(verbIs("EXAMINE"))))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	uids := sequentialUIDs(2*uidFetchBatchSize + 1)

	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		var err error
		got, err = streamedUIDs(t.Context(), c, "INBOX", 0, uids)
		return err
	})

	se := streamErrorOf(t, err)
	if se.FailedBatches != 1 || se.Stop != nil {
		t.Errorf("FailedBatches, Stop = %d, %v, want 1, nil", se.FailedBatches, se.Stop)
	}
	if errors.Is(err, ErrSelectRefused) {
		t.Errorf("err = %v, want no ErrSelectRefused: a dropped session is no server answer", err)
	}
	if got := classifyError(se.First); got != ClassTransient {
		t.Errorf("classifyError(First) = %s, want %s: both sessions dropped", got, ClassTransient)
	}
	if want := uids[uidFetchBatchSize:]; !slices.Equal(got, want) {
		t.Errorf("callback got %d UIDs, want the %d of the second and third batch, each once", len(got), len(want))
	}
	if gotSets, want := fetches.all(), [][]uint32{uids[uidFetchBatchSize : 2*uidFetchBatchSize], uids[2*uidFetchBatchSize:]}; !slices.EqualFunc(gotSets, want, slices.Equal) {
		t.Errorf("answered UID FETCH sets = %d, want the second and third batch", len(gotSets))
	}
	if n := srv.callCount("LOGIN"); n != 3 {
		t.Errorf("LOGIN count = %d, want 3: the first batch used two sessions and the next batch a third", n)
	}
}

func Test_StreamMessagesByUIDs_When_AStopCauseArrives_Then_TheStreamEndsThere(t *testing.T) {
	t.Parallel()

	uids := sequentialUIDs(2*uidFetchBatchSize + 1)
	tests := []struct {
		hook        func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool)
		wantStopIs  error
		name        string
		wantFirst   string
		wantStop    string
		wantSuffix  string
		uidValidity uint32
		wantFailed  int
		wantFetches int
		wantClass   ErrClass
	}{
		{
			name: "the first batch gets a throttled reply",
			hook: func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
				return answerUIDFetchEnding(fetches, sendNothing, refuseBatches(throttleReply, 1))
			},
			wantFirst:   "exceeded command or bandwidth limits",
			wantStop:    "exceeded command or bandwidth limits",
			wantFailed:  1,
			wantFetches: 1,
			wantClass:   ClassThrottled,
		},
		{
			name: "a throttled reply follows an unknown refusal",
			hook: func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
				return answerUIDFetchEnding(fetches, sendNothing, func(n int) string {
					if n == 1 {
						return "NO [CANNOT] fetch refused"
					}
					return throttleReply
				})
			},
			wantFirst:   "fetch refused",
			wantStop:    "exceeded command or bandwidth limits",
			wantSuffix:  " (2 batches failed)",
			wantFailed:  2,
			wantFetches: 2,
			wantClass:   ClassThrottled,
		},
		{
			name: "the UIDVALIDITY differs",
			hook: func(fetches *uidFetchLog) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
				return chainHooks(examineAs(6), answerUIDFetch(fetches, requestedAsSent))
			},
			uidValidity: 5,
			wantStopIs:  ErrUIDValidityChanged,
			wantFirst:   "UIDVALIDITY 5, now 6",
			wantStop:    "UIDVALIDITY 5, now 6",
			wantFailed:  1,
			wantFetches: 0,
			wantClass:   ClassUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetches := new(uidFetchLog)
			srv := newFakeServer(t)
			srv.addConnHandler(interceptHandler(srv, tt.hook(fetches)))
			c := newClientWithFake(t, srv)

			_, err := streamedUIDs(t.Context(), c, "INBOX", tt.uidValidity, uids)

			se := streamErrorOf(t, err)
			if se.FailedBatches != tt.wantFailed {
				t.Errorf("FailedBatches = %d, want %d", se.FailedBatches, tt.wantFailed)
			}
			if se.First == nil || !strings.Contains(se.First.Error(), tt.wantFirst) {
				t.Errorf("First = %v, want it to contain %q", se.First, tt.wantFirst)
			}
			if se.Stop == nil || !strings.Contains(se.Stop.Error(), tt.wantStop) {
				t.Errorf("Stop = %v, want it to contain %q", se.Stop, tt.wantStop)
			}
			if tt.wantStopIs != nil && !errors.Is(se.Stop, tt.wantStopIs) {
				t.Errorf("Stop = %v, want it to wrap %v", se.Stop, tt.wantStopIs)
			}
			if tt.wantSuffix != "" && !strings.HasSuffix(err.Error(), tt.wantSuffix) {
				t.Errorf("error = %q, want it to end with %q", err, tt.wantSuffix)
			}
			if got := classifyError(err); got != tt.wantClass {
				t.Errorf("classifyError(err) = %s, want %s", got, tt.wantClass)
			}
			if n := srv.callCount("UID FETCH"); n != tt.wantFetches {
				t.Errorf("UID FETCH count = %d, want %d: no batch follows a stop cause", n, tt.wantFetches)
			}
			if n := srv.callCount("LOGIN"); n != 1 {
				t.Errorf("LOGIN count = %d, want 1", n)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_UIDValidityChangesAfterARefusedBatch_Then_TheStreamStops(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(5), func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		fetches.record(requestedUIDs(cmd))
		writeLine(conn, "%s NO [CANNOT] fetch refused", cmd.tag)
		writeLine(conn, "* BYE Autologout; idle for too long")
		return true, false
	})))
	srv.addConnHandler(interceptHandler(srv, chainHooks(examineAs(6), answerUIDFetch(fetches, requestedAsSent))))
	c := newWatchedClient(t, srv, 0)
	uids := sequentialUIDs(2*uidFetchBatchSize + 1)

	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		var err error
		got, err = streamedUIDs(t.Context(), c, "INBOX", 5, uids)
		return err
	})

	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("StreamMessagesByUIDs = %v, want ErrUIDValidityChanged", err)
	}
	se := streamErrorOf(t, err)
	if se.First == nil || !strings.Contains(se.First.Error(), "fetch refused") || errors.Is(se.First, ErrUIDValidityChanged) {
		t.Errorf("First = %v, want the refusal of the first batch", se.First)
	}
	if se.Stop == nil || !errors.Is(se.Stop, ErrUIDValidityChanged) || !strings.Contains(se.Stop.Error(), "UIDVALIDITY 5, now 6") {
		t.Errorf("Stop = %v, want the mismatch of the new session", se.Stop)
	}
	if se.FailedBatches != 2 {
		t.Errorf("FailedBatches = %d, want 2", se.FailedBatches)
	}
	if len(got) != 0 {
		t.Errorf("callback got %d UIDs, want none", len(got))
	}
	if n := srv.callCount("UID FETCH"); n != 1 {
		t.Errorf("UID FETCH count = %d, want 1: the third batch is never requested", n)
	}
	if n := srv.callCount("LOGIN"); n != 2 {
		t.Errorf("LOGIN count = %d, want 2: one new session after the BYE", n)
	}
}

func Test_StreamMessagesByUIDs_When_ClientHasGivenUp_Then_EachBatchFailsWithoutALogin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		giveUp func(t *testing.T, srv *fakeServer, c *Client) error
		name   string
	}{
		{
			name:   "permanent refusal",
			giveUp: loseSession,
		},
		{
			name: "transient refusals within the revive interval",
			giveUp: func(t *testing.T, srv *fakeServer, c *Client) error {
				t.Helper()
				err := giveUpOn(t, srv, c, "NO [UNAVAILABLE] Temporary authentication failure", ClassTransient)
				c.reviveDur = time.Hour
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			c := newClientWithFake(t, srv)
			dials := countDials(c)
			stored := tt.giveUp(t, srv, c)
			dialsBefore, loginsBefore := dials.Load(), srv.callCount("LOGIN")

			got, err := streamedUIDs(t.Context(), c, "INBOX", 0, sequentialUIDs(2*uidFetchBatchSize+1))

			se := streamErrorOf(t, err)
			if se.FailedBatches != 3 || se.Stop != nil {
				t.Errorf("FailedBatches, Stop = %d, %v, want 3, nil", se.FailedBatches, se.Stop)
			}
			if !errors.Is(err, stored) {
				t.Errorf("error = %v, want the stored give-up %v", err, stored)
			}
			if len(got) != 0 {
				t.Errorf("callback got %d UIDs, want none", len(got))
			}
			if n := dials.Load(); n != dialsBefore {
				t.Errorf("dial count went from %d to %d, want no dial", dialsBefore, n)
			}
			if n := srv.callCount("LOGIN"); n != loginsBefore {
				t.Errorf("LOGIN count went from %d to %d, want no LOGIN", loginsBefore, n)
			}
			if n := srv.callCount("UID FETCH"); n != 0 {
				t.Errorf("UID FETCH count = %d, want 0", n)
			}
		})
	}
}

func Test_StreamMessagesByUIDs_When_CancelledAfterARefusedBatch_Then_ReturnsCanceledAndFetchesNoMore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup func(t *testing.T, srv *fakeServer, target *atomic.Pointer[Client]) context.Context
		name  string
	}{
		{
			name: "the context ends",
			setup: func(t *testing.T, srv *fakeServer, _ *atomic.Pointer[Client]) context.Context {
				srv.addConnHandler(interceptHandler(srv, answerUIDFetchEnding(new(uidFetchLog), sendNothing, refuseBatches("NO [CANNOT] fetch refused", 1))))
				return countedContext{Context: t.Context(), srv: srv, verb: "UID FETCH", n: 1}
			},
		},
		{
			name: "the client is cancelled",
			setup: func(t *testing.T, srv *fakeServer, target *atomic.Pointer[Client]) context.Context {
				// The hook runs after the server counted the command and before it answers.
				srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
					if !isUIDFetch(cmd) {
						return false, true
					}
					target.Load().Cancel()
					writeLine(conn, "%s NO [CANNOT] fetch refused", cmd.tag)
					return true, false
				}))
				return t.Context()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newFakeServer(t)
			var target atomic.Pointer[Client]
			ctx := tt.setup(t, srv, &target)
			c := newClientWithFake(t, srv)
			target.Store(c)

			_, err := streamedUIDs(ctx, c, "INBOX", 0, sequentialUIDs(2*uidFetchBatchSize+1))

			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
			if _, ok := errors.AsType[*StreamError](err); ok {
				t.Errorf("error = %v, want the bare cancellation and no *StreamError", err)
			}
			if n := srv.callCount("UID FETCH"); n != 1 {
				t.Errorf("UID FETCH count = %d, want 1: no batch follows the cancellation", n)
			}
		})
	}
}

// holdAfterOneMessage answers the first UID FETCH with its first message and no tagged reply. It waits for the client to close the connection, then closes closed.
func holdAfterOneMessage(log *uidFetchLog, closed chan<- struct{}) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		writeUIDMessage(conn, requested[0])
		_, _ = io.Copy(io.Discard, rd)
		close(closed)
		return true, false
	}
}

// hangUpAfterOneMessage is holdAfterOneMessage for a server that closes the connection after the message and then closes hungUp.
func hangUpAfterOneMessage(log *uidFetchLog, hungUp chan<- struct{}) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		requested := requestedUIDs(cmd)
		log.record(requested)
		writeUIDMessage(conn, requested[0])
		_ = conn.Close()
		close(hungUp)
		return true, false
	}
}

func Test_StreamMessagesByUIDs_When_HaltBeforeTheFirstBatch_Then_SendsNoCommand(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	errHalt := errors.New("the run stopped")
	calls := 0

	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2, 3}, func() error { return errHalt }, func(Message) error {
		calls++
		return nil
	})

	if !errors.Is(err, errHalt) {
		t.Errorf("error = %v, want the halt error", err)
	}
	if calls != 0 {
		t.Errorf("callback ran %d times, want 0", calls)
	}
	if got := srv.callCount("EXAMINE") + srv.callCount("SELECT"); got != 0 {
		t.Errorf("EXAMINE+SELECT count = %d, want 0", got)
	}
	if got := srv.callCount("UID FETCH"); got != 0 {
		t.Errorf("UID FETCH count = %d, want 0", got)
	}
}

func Test_StreamMessagesByUIDs_When_HaltTurnsOnAfterABatch_Then_NoNextBatch(t *testing.T) {
	t.Parallel()

	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	errHalt := errors.New("the run stopped")
	var halted atomic.Bool
	halt := func() error {
		if halted.Load() {
			return errHalt
		}
		return nil
	}

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	calls := 0
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, halt, func(msg Message) error {
		calls++
		if msg.UID() == uidFetchBatchSize {
			halted.Store(true)
		}
		return nil
	})

	if !errors.Is(err, errHalt) {
		t.Errorf("error = %v, want the halt error", err)
	}
	if calls != uidFetchBatchSize {
		t.Errorf("callback ran %d times, want %d: the whole first batch", calls, uidFetchBatchSize)
	}
	if got := srv.callCount("UID FETCH"); got != 1 {
		t.Errorf("UID FETCH count = %d, want 1: no second batch", got)
	}
}

func Test_StreamMessagesByUIDs_When_HaltTurnsOnDuringARetry_Then_NewSessionGetsNoFetch(t *testing.T) {
	t.Parallel()

	const total, sent = 5, 2
	fetches := new(uidFetchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, cutUIDFetch(fetches, sent, true)))
	srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
	c := newWatchedClient(t, srv, 0)
	errHalt := errors.New("the run stopped")
	var halted atomic.Bool
	halt := func() error {
		if halted.Load() {
			return errHalt
		}
		return nil
	}

	uids := make([]uint32, total)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	var got []uint32
	err := awaitCall(t, "StreamMessagesByUIDs", func() error {
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, halt, func(msg Message) error {
			got = append(got, msg.UID())
			if len(got) == sent {
				halted.Store(true)
			}
			return nil
		})
	})

	if !errors.Is(err, errHalt) {
		t.Errorf("error = %v, want the halt error", err)
	}
	if !slices.Equal(got, uids[:sent]) {
		t.Errorf("callback got UIDs %v, want %v", got, uids[:sent])
	}
	if gotSets := fetches.all(); !slices.EqualFunc(gotSets, [][]uint32{uids}, slices.Equal) {
		t.Errorf("UID FETCH asked for %v, want one request: the new session gets none", gotSets)
	}
	if got := srv.callCount("EXAMINE") + srv.callCount("SELECT"); got != 1 {
		t.Errorf("EXAMINE+SELECT count = %d, want 1: the new session never selects", got)
	}
}

func Test_StreamMessagesByUIDs_When_CallbackStopsTheStream_Then_SessionClosesAndNothingIsRetried(t *testing.T) {
	t.Parallel()

	errStop := errors.New("the run stopped")
	uids := []uint32{1, 2, 3}

	t.Run("the server keeps waiting", func(t *testing.T) {
		t.Parallel()

		fetches := new(uidFetchLog)
		closed := make(chan struct{})
		srv := newFakeServer(t)
		srv.addConnHandler(interceptHandler(srv, holdAfterOneMessage(fetches, closed)))
		srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
		c := newWatchedClient(t, srv, 0)

		calls := 0
		err := awaitCall(t, "StreamMessagesByUIDs", func() error {
			return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(Message) error {
				calls++
				return errStop
			})
		})

		if !errors.Is(err, errStop) {
			t.Errorf("error = %v, want the callback error", err)
		}
		awaitSignal(t, closed, "the client to close the connection with a batch in flight")
		if calls != 1 {
			t.Errorf("callback ran %d times, want 1", calls)
		}
		if got := srv.callCount("LOGIN"); got != 1 {
			t.Errorf("LOGIN count = %d, want 1: a stop starts no reconnect", got)
		}
		if got := srv.callCount("UID FETCH"); got != 1 {
			t.Errorf("UID FETCH count = %d, want 1: a stop starts no retry", got)
		}

		var got []uint32
		err = awaitCall(t, "later StreamMessagesByUIDs", func() error {
			return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(msg Message) error {
				got = append(got, msg.UID())
				return nil
			})
		})
		if err != nil {
			t.Fatalf("later stream error = %v, want success on a new session", err)
		}
		if !slices.Equal(got, uids) {
			t.Errorf("later stream delivered %v, want %v", got, uids)
		}
		if got := srv.callCount("LOGIN"); got != 2 {
			t.Errorf("LOGIN count = %d after the later stream, want 2: one reconnect", got)
		}
	})

	t.Run("the server drops the connection after the callback error", func(t *testing.T) {
		t.Parallel()

		fetches := new(uidFetchLog)
		hungUp := make(chan struct{})
		srv := newFakeServer(t)
		srv.addConnHandler(interceptHandler(srv, hangUpAfterOneMessage(fetches, hungUp)))
		srv.addConnHandler(interceptHandler(srv, answerUIDFetch(fetches, requestedAsSent)))
		c := newWatchedClient(t, srv, 0)

		err := awaitCall(t, "StreamMessagesByUIDs", func() error {
			return c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, uids, nil, func(Message) error {
				awaitSignal(t, hungUp, "the server to drop the connection")
				return errStop
			})
		})

		if !errors.Is(err, errStop) {
			t.Errorf("error = %v, want the callback error", err)
		}
		if got := srv.callCount("LOGIN"); got != 1 {
			t.Errorf("LOGIN count = %d, want 1: the drop after a stop starts no reconnect", got)
		}
		if got := srv.callCount("UID FETCH"); got != 1 {
			t.Errorf("UID FETCH count = %d, want 1: the drop after a stop starts no retry", got)
		}
	})
}

func Test_ExistingUIDs_When_ReplyNamesUIDsBetweenTheAskedOnes_Then_OnlyTheAskedUIDsCount(t *testing.T) {
	t.Parallel()

	searches := new(uidSearchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, func([]uint32) []uint32 {
		return []uint32{3, 7, 5, 4, 10, 0}
	})))
	c := newClientWithFake(t, srv)

	got, err := c.ExistingUIDs(t.Context(), "INBOX", 0, []uint32{1, 5, 9})
	if err != nil {
		t.Fatalf("ExistingUIDs: %v", err)
	}

	if want := []uint32{5}; !slices.Equal(got, want) {
		t.Errorf("ExistingUIDs = %v, want %v: a UID that nobody asked for must not stand for a neighbour", got, want)
	}
}

func Test_StreamMessagesByUIDs_When_HaltIsOnAndTheClientHasNoSession_Then_SendsNoLogin(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, serveNormally))
	c := newDisconnectedClient(t, srv)
	errHalt := errors.New("the run stopped")

	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1, 2}, func() error { return errHalt }, func(Message) error {
		t.Error("callback ran, want no message")
		return nil
	})

	if !errors.Is(err, errHalt) {
		t.Errorf("error = %v, want the halt error", err)
	}
	for _, verb := range []string{"LOGIN", "EXAMINE", "UID FETCH"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("%s count = %d, want 0: a stopped run must not log in a Client for a stream it will not start", verb, n)
		}
	}
}

func Test_StreamMessagesByUIDs_When_ExamineGetsAThrottledReply_Then_TheStreamEndsThere(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if cmd.verb != "EXAMINE" {
			return false, true
		}
		writeLine(conn, "%s %s", cmd.tag, throttleReply)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	got, err := streamedUIDs(t.Context(), c, "INBOX", 0, sequentialUIDs(2*uidFetchBatchSize+1))

	se := streamErrorOf(t, err)
	if se.FailedBatches != 1 {
		t.Errorf("FailedBatches = %d, want 1", se.FailedBatches)
	}
	if se.Stop == nil || !strings.Contains(se.Stop.Error(), "select folder INBOX: ") || !IsThrottled(se.Stop) {
		t.Errorf("Stop = %v, want the throttled reply of EXAMINE", se.Stop)
	}
	if errors.Is(se.Stop, ErrSelectRefused) || errors.Is(err, ErrSelectRefused) {
		t.Errorf("Stop = %v, err = %v, want no ErrSelectRefused: a throttle can clear", se.Stop, err)
	}
	if got := classifyError(err); got != ClassThrottled {
		t.Errorf("classifyError(err) = %s, want %s", got, ClassThrottled)
	}
	if len(got) != 0 {
		t.Errorf("callback got %d UIDs, want none", len(got))
	}
	for verb, want := range map[string]int{"EXAMINE": 1, "UID FETCH": 0, "LOGIN": 1} {
		if n := srv.callCount(verb); n != want {
			t.Errorf("%s count = %d, want %d: no batch follows a throttled reply", verb, n, want)
		}
	}
}

func Test_ExistingUIDs_When_InputRepeatsEachUID_Then_OneCommandCarriesTheDistinctUIDs(t *testing.T) {
	t.Parallel()

	searches := new(uidSearchLog)
	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, answerUIDSearch(searches, requestedAsSent)))
	c := newClientWithFake(t, srv)
	asked := slices.Concat(sequentialUIDs(uidFetchBatchSize), sequentialUIDs(uidFetchBatchSize))

	got, err := c.ExistingUIDs(t.Context(), "INBOX", 0, asked)
	if err != nil {
		t.Fatalf("ExistingUIDs: %v", err)
	}

	if want := sequentialUIDs(uidFetchBatchSize); !slices.Equal(got, want) {
		t.Errorf("ExistingUIDs returned %d UIDs, want the %d distinct ones", len(got), len(want))
	}
	if n := srv.callCount("UID SEARCH"); n != 1 {
		t.Errorf("UID SEARCH count = %d, want 1: the repeats must not use up the room of a command", n)
	}
}

func Test_StreamMessagesByUIDs_When_ItemNamesAreLowerCase_Then_FlagsAndDateAreKept(t *testing.T) {
	t.Parallel()

	srv := newFakeServer(t)
	srv.addConnHandler(interceptHandler(srv, func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !isUIDFetch(cmd) {
			return false, true
		}
		writeUIDMessageWithItems(conn, 1, ` flags (\Seen $MixedCase NonJunk) internaldate "17-Jul-2012 09:30:00 +0200"`)
		writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
		return true, true
	}))
	c := newClientWithFake(t, srv)

	var flags []string
	var date time.Time
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", 0, []uint32{1}, nil, func(msg Message) error {
		flags, date = msg.m.Flags, msg.m.InternalDate
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	if want := []string{`\Seen`, "$MixedCase", "NonJunk"}; !slices.Equal(flags, want) {
		t.Errorf("flags = %q, want %q: the keywords keep the spelling of the server", flags, want)
	}
	if want := time.Date(2012, 7, 17, 9, 30, 0, 0, time.FixedZone("", 2*3600)); !date.Equal(want) {
		t.Errorf("InternalDate = %v, want %v", date, want)
	}
}
