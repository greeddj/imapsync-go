package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
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

	err := c.StreamMessagesByUIDs(context.Background(), "INBOX", uids, func(_ Message) error {
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
			err := c.StreamMessagesByUIDs(ctx, "INBOX", tt.uids, func(Message) error {
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
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(msg Message) error {
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
// callback error in the first batch is returned, the batch is drained without
// more callbacks, and no second batch is requested.
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
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(Message) error {
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
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", []uint32{1}, func(Message) error {
				calls++
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
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
	genBefore := c.connGen.Load()

	uids := make([]uint32, uidFetchBatchSize+1)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}

	ctx := newFiredContext(t.Context())
	calls := 0
	errc := make(chan error, 1)
	go func() {
		errc <- c.StreamMessagesByUIDs(ctx, "INBOX", uids, func(Message) error {
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
	if got := c.connGen.Load(); got != genBefore {
		t.Errorf("connGen = %d, want %d (a canceled client must not reconnect)", got, genBefore)
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

	result, totalSize, err := c.FetchMessageMap(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("FetchMessageMap: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("returned map has %d entries, want 1", len(result))
	}
	if _, ok := result["ok@host"]; !ok {
		t.Errorf("expected key ok@host in map, got %v", result)
	}
	if totalSize == 0 {
		t.Error("totalSize = 0, want > 0 - RFC822.SIZE not aggregated from fake server response")
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
		// More messages than the channel and go-imap's reader can hold, so the
		// reader stops consuming while the first callback runs.
		{name: "in the middle of a batch", messages: messageChanBuffer + 10},
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
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(Message) error {
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
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", []uint32{1}, func(Message) error {
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
			err := c.StreamMessagesByUIDs(t.Context(), "INBOX", []uint32{1, 2, 3}, func(Message) error {
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
	body := "Subject: uid " + strconv.FormatUint(uint64(uid), 10) + "\r\n\r\nbody\r\n"
	writeLine(conn, "* %d FETCH (UID %d BODY[] {%d}\r\n%s)", uid, uid, len(body), body)
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

func requestedAsSent(requested []uint32) []uint32 { return requested }

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
				return c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(msg Message) error {
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
		return c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(msg Message) error {
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
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", []uint32{1, 2, 3}, func(msg Message) error {
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

// Test_StreamMessagesByUIDs_When_BodylessResponseComesFirst_Then_FullMessageStillDelivered
// asserts that a FETCH response without a body does not hide the later response
// that carries it.
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

	withBody := make(map[uint32]int)
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", []uint32{1, 2, 3}, func(msg Message) error {
		if msg.m.GetBody(fullBodyPeekSection) != nil {
			withBody[msg.m.Uid]++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMessagesByUIDs: %v", err)
	}

	for _, uid := range []uint32{1, 2, 3} {
		if got := withBody[uid]; got != 1 {
			t.Errorf("UID %d reached the callback with its body %d times, want 1", uid, got)
		}
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
	err := c.StreamMessagesByUIDs(t.Context(), "INBOX", uids, func(msg Message) error {
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
