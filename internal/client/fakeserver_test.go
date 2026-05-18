package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

// fakeServer is a minimal IMAP wire-level server for unit tests. It records
// how many times each command verb was called. Per-connection handlers
// registered via addConnHandler drive all test scenarios; the fallback
// handle() path serves vanilla responses when no handler is queued.
type fakeServer struct {
	ln             net.Listener
	counts         map[string]int      // per-verb call count
	names          map[string][]string // per-verb ordered argument captures
	connHandlers   []func(net.Conn)    // consumed one-per-connection; falls back to handle() when exhausted
	connHandlerIdx int
	mu             sync.Mutex
}

// newFakeServer starts a fake IMAP listener on a random local port.
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

// addConnHandler appends a per-connection handler function. The first call to
// Accept uses connHandlers[0], the second uses connHandlers[1], etc. When the
// queue is exhausted, connections fall back to handle().
func (s *fakeServer) addConnHandler(fn func(net.Conn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connHandlers = append(s.connHandlers, fn)
}

// callCount returns how many times the verb was seen.
func (s *fakeServer) callCount(verb string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[verb]
}

// capturedNames returns the argument strings captured for the given verb.
func (s *fakeServer) capturedNames(verb string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names[verb]...)
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

// handle serves one IMAP connection: writes a greeting, then loops reading
// commands and dispatching scripted replies.
func (s *fakeServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// IMAP greeting — go-imap's imapclient.New blocks until it sees this.
	_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] fake ready\r\n")

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Every client line starts with a tag, then the command, then args.
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

// dispatch writes the appropriate IMAP response for one command.
// reply, when non-empty, overrides the default success reply.
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
			"* 0 EXISTS",
			"* 0 RECENT",
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
			"* 0 EXISTS",
			"* 0 RECENT",
			"* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)",
			tag+" OK [READ-ONLY] EXAMINE completed",
		)

	case "STATUS":
		if reply != "" {
			return write(tag + " " + reply)
		}
		mboxArg := strings.SplitN(arg, " ", 2)
		mboxName := strings.Trim(mboxArg[0], `"`)
		return write(
			fmt.Sprintf("* STATUS %s (MESSAGES 0)", mboxName),
			taggedOK,
		)

	case "LIST":
		if reply != "" {
			return write(tag + " " + reply)
		}
		return write(
			`* LIST (\HasNoChildren) "/" INBOX`,
			taggedOK,
		)

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

// newClientWithFake creates a *Client whose dialFn connects to srv. It calls
// connectAndLogin but NOT loadMailboxCache, so tests that need a populated
// cache must set it directly.
//
// The client is cleaned up (Cancel + Logout) when the test ends.
func newClientWithFake(t *testing.T, srv *fakeServer) *Client {
	t.Helper()

	c := &Client{
		serverAddr:   srv.ln.Addr().String(),
		username:     "user",
		password:     "pass",
		backoff:      initialBackoff,
		reconnectDur: 0, // no minimum interval in tests
		dialTimeout:  5 * time.Second,
		folderLocks:  make(map[string]*sync.Mutex),
		cancelCh:     make(chan struct{}),
	}

	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}

	if err := c.connectAndLogin(context.Background()); err != nil {
		t.Fatalf("newClientWithFake connectAndLogin: %v", err)
	}

	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})
	return c
}

// buildTestIMAPMessage constructs a minimal *imap.Message suitable for
// passing to AppendMessage. The body section key must be the resp() form of
// fullBodyPeekSection (Peek=false, empty BodyPartName) because go-imap always
// stores server-returned body sections without the PEEK flag.
func buildTestIMAPMessage() *imap.Message {
	section := &imap.BodySectionName{} // Peek=false matches fullBodyPeekSection.resp()
	msg := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	msg.Body[section] = strings.NewReader("From: test@example.com\r\nSubject: hi\r\n\r\nHello\r\n")
	msg.Envelope = &imap.Envelope{MessageId: "test-id@example.com"}
	return msg
}
