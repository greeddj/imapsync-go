package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/client"
)

// fakeServer is a minimal IMAP wire-level server for unit tests. It records
// how many times each command verb was called. Per-connection handlers
// registered via addConnHandler drive all test scenarios; the fallback
// handle() path serves vanilla responses when no handler is queued.
type fakeServer struct {
	ln             net.Listener
	counts         map[string]int
	names          map[string][]string
	connHandlers   []func(net.Conn)
	connHandlerIdx int
	mu             sync.Mutex
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeServer listen: %v", err)
	}
	s := &fakeServer{
		ln:     ln,
		counts: make(map[string]int),
		names:  make(map[string][]string),
	}
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

func (s *fakeServer) close() {
	_ = s.ln.Close()
}

// addConnHandler appends a per-connection handler. The first Accept uses
// connHandlers[0], the second uses connHandlers[1], etc. When exhausted,
// connections fall back to handle().
func (s *fakeServer) addConnHandler(fn func(net.Conn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connHandlers = append(s.connHandlers, fn)
}

func (s *fakeServer) callCount(verb string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[verb]
}

func (s *fakeServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		var handler func(net.Conn)
		if s.connHandlerIdx < len(s.connHandlers) {
			handler = s.connHandlers[s.connHandlerIdx]
			s.connHandlerIdx++
		}
		s.mu.Unlock()
		if handler != nil {
			go handler(conn)
		} else {
			go s.handle(conn)
		}
	}
}

// handle serves one IMAP connection with default scripted replies.
func (s *fakeServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] fake ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			_, _ = fmt.Fprintf(conn, "* BAD parse error\r\n")
			continue
		}
		tag := parts[0]
		verb := strings.ToUpper(parts[1])
		arg := ""
		if len(parts) == 3 {
			arg = parts[2]
		}
		s.mu.Lock()
		s.counts[verb]++
		switch verb {
		case "SELECT", "CREATE":
			name := strings.Trim(arg, `"`)
			s.names[verb] = append(s.names[verb], name)
		}
		s.mu.Unlock()
		if err := s.dispatch(conn, tag, verb, arg, ""); err != nil {
			return
		}
	}
}

// dispatch writes the appropriate IMAP response for one command. reply, when
// non-empty, overrides the default success reply.
func (s *fakeServer) dispatch(conn net.Conn, tag, verb, arg, reply string) error {
	write := func(lines ...string) error {
		for _, l := range lines {
			if _, err := fmt.Fprintf(conn, "%s\r\n", l); err != nil {
				return err
			}
		}
		return nil
	}
	taggedOK := tag + " OK " + verb + " completed"

	switch verb {
	case "LOGIN":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	case "LOGOUT":
		return write("* BYE Logging out", taggedOK)
	case "NOOP":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	case "CAPABILITY":
		return write("* CAPABILITY IMAP4rev1", taggedOK)
	case "SELECT":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(
			"* 0 EXISTS", "* 0 RECENT",
			"* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)",
			"* OK [PERMANENTFLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft \\*)] Flags permitted.",
			"* OK [UIDVALIDITY 1] UIDs valid",
			"* OK [UIDNEXT 1] Predicted next UID",
			tag+" OK [READ-WRITE] SELECT completed",
		)
	case "EXAMINE":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(
			"* 0 EXISTS", "* 0 RECENT",
			"* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)",
			tag+" OK [READ-ONLY] EXAMINE completed",
		)
	case "STATUS":
		if reply != "" {
			return write(tag + " " + reply)
		}
		mboxArg := strings.SplitN(arg, " ", 2)
		mboxName := strings.Trim(mboxArg[0], `"`)
		return write(fmt.Sprintf("* STATUS %s (MESSAGES 0)", mboxName), taggedOK)
	case "LIST":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(`* LIST (\HasNoChildren) "/" INBOX`, taggedOK)
	case "FETCH":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	case "UID":
		upperArg := strings.ToUpper(arg)
		if strings.HasPrefix(upperArg, "FETCH") {
			s.mu.Lock()
			s.counts["UID FETCH"]++
			s.mu.Unlock()
			if reply != "" {
				return write(tag + " " + reply)
			}
			return write(taggedOK)
		}
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	case "CREATE":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	default:
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(taggedOK)
	}
}

// newAppClient constructs a *client.Client connected to srv via plain TCP.
// It registers a Logout cleanup when the test ends.
func newAppClient(t *testing.T, srv *fakeServer, label string) *client.Client {
	t.Helper()
	c, err := client.New(
		context.Background(),
		srv.ln.Addr().String(),
		"user", "pass",
		client.Options{UseTLS: false},
	)
	if err != nil {
		t.Fatalf("newAppClient(%s): %v", label, err)
	}
	c.SetPrefix(label)
	t.Cleanup(func() { _ = c.Logout() })
	return c
}

// imapMsgIDHeader builds a minimal Message-Id header section, including the
// blank-line separator, as sent in a FETCH BODY[HEADER.FIELDS (MESSAGE-ID)]
// response.
func imapMsgIDHeader(msgID string) string {
	return fmt.Sprintf("Message-Id: <%s>\r\n\r\n", msgID)
}

// nilEnvelope is the IMAP wire representation of an empty envelope. All 10
// fields are NIL; go-imap's Envelope.Parse leaves every field at its zero
// value when the field is nil, so AppendMessage's msg.Envelope.Date access
// is safe (zero time.Time).
const nilEnvelope = "(NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL)"

// imapFullBody builds a minimal RFC822 message body for use in UID FETCH
// BODY[] responses.
func imapFullBody(msgID string) string {
	return fmt.Sprintf("From: sender@test\r\nMessage-Id: <%s>\r\nSubject: test\r\n\r\nBody.\r\n", msgID)
}

// parseLiteralSize extracts n from a `{n}` or `{n+}` literal marker at the
// end of an IMAP command arg. Returns 0 if none found.
func parseLiteralSize(arg string) int {
	arg = strings.TrimSpace(arg)
	if !strings.HasSuffix(arg, "}") {
		return 0
	}
	start := strings.LastIndex(arg, "{")
	if start < 0 {
		return 0
	}
	inner := arg[start+1 : len(arg)-1]
	inner = strings.TrimSuffix(inner, "+")
	n, err := strconv.Atoi(inner)
	if err != nil {
		return 0
	}
	return n
}

// msgIDFetchHandler returns a per-connection IMAP handler that responds to the
// initial LOGIN + LIST (for mailbox-cache loading in client.New), then EXAMINE
// with N EXISTS, then FETCH with Message-Id header responses. mailboxes is the
// LIST reply; msgs maps folder name → []{ uid, msgID }.
//
// The handler uses bufio.Scanner and is NOT suitable for connections that need
// to handle APPEND (which uses synchronizing literals).
func msgIDFetchHandler(srv *fakeServer, mailboxes []string, msgs map[string][]struct {
	msgID string
	uid   uint32
}) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		var selectedFolder string
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
			arg := ""
			if len(parts) == 3 {
				arg = parts[2]
			}
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()

			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				for _, mb := range mailboxes {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "EXAMINE", "SELECT":
				selectedFolder = strings.Trim(arg, `"`)
				n := len(msgs[selectedFolder])
				_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* 0 RECENT\r\n", n)
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "FETCH":
				for i, m := range msgs[selectedFolder] {
					hdr := imapMsgIDHeader(m.msgID)
					_, _ = fmt.Fprintf(conn,
						"* %d FETCH (UID %d BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
						i+1, m.uid, len(hdr), hdr,
					)
				}
				_, _ = fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 0)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "UID":
				srv.mu.Lock()
				srv.counts["UID FETCH"]++
				srv.mu.Unlock()
				_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
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

// slowExamineHandler wraps msgIDFetchHandler but sleeps delay before each
// EXAMINE/SELECT response. The delay simulates asymmetric server latency so
// tests can observe that the other side's tracker advances independently.
func slowExamineHandler(srv *fakeServer, mailboxes []string, msgs map[string][]struct {
	msgID string
	uid   uint32
}, delay time.Duration) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		var selectedFolder string
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
			arg := ""
			if len(parts) == 3 {
				arg = parts[2]
			}
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()

			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				for _, mb := range mailboxes {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "EXAMINE", "SELECT":
				// Inject latency before responding so the fast side can advance ahead.
				time.Sleep(delay)
				selectedFolder = strings.Trim(arg, `"`)
				n := len(msgs[selectedFolder])
				_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* 0 RECENT\r\n", n)
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "FETCH":
				for i, m := range msgs[selectedFolder] {
					hdr := imapMsgIDHeader(m.msgID)
					_, _ = fmt.Fprintf(conn,
						"* %d FETCH (UID %d BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s)\r\n",
						i+1, m.uid, len(hdr), hdr,
					)
				}
				_, _ = fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 0)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "UID":
				srv.mu.Lock()
				srv.counts["UID FETCH"]++
				srv.mu.Unlock()
				_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
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

// uidFetchBodyHandler returns a per-connection IMAP handler that handles
// EXAMINE + UID FETCH by returning full message bodies (ENVELOPE + BODY[]).
// It also handles APPEND (for dst), draining the literal body correctly.
//
// mailboxes is the LIST response; uidBodies maps folder → []{ uid, body }.
// appendReply, when non-empty, is used as the tagged reply for APPEND (e.g.
// "NO Quota exceeded"). When empty, APPEND succeeds.
func uidFetchBodyHandler(srv *fakeServer, mailboxes []string, uidBodies map[string][]struct {
	body string
	uid  uint32
}, appendReply string) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		reader := bufio.NewReader(conn)
		var selectedFolder string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 2 {
				continue
			}
			tag, verb := parts[0], strings.ToUpper(parts[1])
			arg := ""
			if len(parts) == 3 {
				arg = parts[2]
			}
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()

			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				for _, mb := range mailboxes {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "EXAMINE", "SELECT":
				selectedFolder = strings.Trim(arg, `"`)
				n := len(uidBodies[selectedFolder])
				_, _ = fmt.Fprintf(conn, "* %d EXISTS\r\n* 0 RECENT\r\n", n)
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "UID":
				srv.mu.Lock()
				srv.counts["UID FETCH"]++
				srv.mu.Unlock()
				for i, m := range uidBodies[selectedFolder] {
					_, _ = fmt.Fprintf(conn,
						"* %d FETCH (UID %d ENVELOPE %s BODY[] {%d}\r\n%s)\r\n",
						i+1, m.uid, nilEnvelope, len(m.body), m.body,
					)
				}
				_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
			case "APPEND":
				if appendReply != "" {
					_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, appendReply)
					continue
				}
				// Synchronizing literal: send continuation, drain exactly n bytes.
				_, _ = fmt.Fprintf(conn, "+ Ready for literal data\r\n")
				n := parseLiteralSize(arg)
				if n > 0 {
					_, _ = io.ReadFull(reader, make([]byte, n))
				}
				_, _ = fmt.Fprintf(conn, "%s OK APPEND completed\r\n", tag)
			case "STATUS":
				mboxName := strings.Trim(strings.SplitN(arg, " ", 2)[0], `"`)
				_, _ = fmt.Fprintf(conn, "* STATUS %s (MESSAGES 0)\r\n", mboxName)
				_, _ = fmt.Fprintf(conn, "%s OK STATUS completed\r\n", tag)
			case "FETCH":
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
