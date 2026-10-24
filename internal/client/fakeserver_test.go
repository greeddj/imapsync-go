package client

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
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

// stallingLoginHandler returns a per-connection handler that sends the IMAP
// greeting but never replies to LOGIN, causing go-imap to block until the
// connection is terminated by Cancel().
func stallingLoginHandler(srv *fakeServer) func(net.Conn) {
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
			verb := strings.ToUpper(parts[1])
			srv.mu.Lock()
			srv.counts[verb]++
			srv.mu.Unlock()
			if verb == "LOGIN" {
				// Never reply — block until the connection is closed.
				b := make([]byte, 1)
				_, _ = conn.Read(b)
				return
			}
		}
	}
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

// newDisconnectedClient creates a *Client wired to srv but does not connect, so
// tests can drive connectAndLogin themselves. The client is cancelled when the
// test ends.
func newDisconnectedClient(t *testing.T, srv *fakeServer) *Client {
	t.Helper()

	c := &Client{
		serverAddr:  srv.ln.Addr().String(),
		username:    "user",
		password:    "pass",
		backoff:     initialBackoff,
		dialTimeout: 5 * time.Second,
		folderLocks: make(map[string]*sync.Mutex),
		cancelCh:    make(chan struct{}),
	}
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	t.Cleanup(func() {
		c.Cancel()
		_ = c.Logout()
	})
	return c
}

// command is one parsed IMAP command line as seen by a scripted handler. arg
// is everything after the verb.
type command struct {
	tag  string
	verb string
	arg  string
}

// record counts cmd under its verb and captures the mailbox argument of the
// verbs that carry one. A UID command is also counted as "UID <SUBVERB>".
func (s *fakeServer) record(cmd command) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counts[cmd.verb]++
	switch cmd.verb {
	case "SELECT", "EXAMINE", "CREATE", "STATUS", "APPEND":
		s.names[cmd.verb] = append(s.names[cmd.verb], firstArg(cmd.arg))
	case "UID":
		sub, _, _ := strings.Cut(cmd.arg, " ")
		s.counts["UID "+strings.ToUpper(sub)]++
	}
}

// bump counts a synthetic event that has no IMAP verb of its own.
func (s *fakeServer) bump(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[event]++
}

// firstArg returns the first argument of arg, unquoting it when quoted.
func firstArg(arg string) string {
	if rest, ok := strings.CutPrefix(arg, `"`); ok {
		if name, _, found := strings.Cut(rest, `"`); found {
			return name
		}
	}
	name, _, _ := strings.Cut(arg, " ")
	return name
}

// writeLine sends one CRLF-terminated line to conn.
func writeLine(conn net.Conn, format string, args ...any) {
	_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
}

// serveCommands sends greeting, then feeds each parsed command to fn until fn
// returns false or the connection ends. fn gets the reader to consume literals.
func serveCommands(srv *fakeServer, conn net.Conn, greeting string, fn func(rd *bufio.Reader, cmd command) bool) {
	defer func() { _ = conn.Close() }()

	writeLine(conn, "%s", greeting)

	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
		if len(parts) < 2 {
			continue
		}
		cmd := command{tag: parts[0], verb: strings.ToUpper(parts[1])}
		if len(parts) == 3 {
			cmd.arg = parts[2]
		}
		srv.record(cmd)
		if !fn(rd, cmd) {
			return
		}
	}
}

const (

	// cramMD5Challenge is the example challenge from RFC 2195.
	cramMD5Challenge = "<1896.697170952@postoffice.reston.mci.net>"
)

// cramMD5Handler accepts only a CRAM-MD5 exchange whose digest matches the one it
// computes for user and pass, and counts it as "CRAM-MD5 VERIFIED". It serves
// LIST afterwards.
func cramMD5Handler(srv *fakeServer, user, pass string) func(net.Conn) {
	return func(conn net.Conn) {
		greeting := "* OK [CAPABILITY IMAP4rev1 AUTH=CRAM-MD5] fake ready"
		serveCommands(srv, conn, greeting, func(rd *bufio.Reader, cmd command) bool {
			switch cmd.verb {
			case "AUTHENTICATE":
				writeLine(conn, "+ %s", base64.StdEncoding.EncodeToString([]byte(cramMD5Challenge)))
				line, err := rd.ReadString('\n')
				if err != nil {
					return false
				}
				got, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(line))

				mac := hmac.New(md5.New, []byte(pass))
				mac.Write([]byte(cramMD5Challenge))
				want := user + " " + hex.EncodeToString(mac.Sum(nil))

				if decodeErr != nil || string(got) != want {
					writeLine(conn, "%s NO [AUTHENTICATIONFAILED] Authentication failed", cmd.tag)
					return true
				}
				srv.bump("CRAM-MD5 VERIFIED")
				writeLine(conn, "%s OK AUTHENTICATE completed", cmd.tag)
			case "LIST":
				writeLine(conn, `* LIST (\HasNoChildren) "/" INBOX`)
				writeLine(conn, "%s OK LIST completed", cmd.tag)
			case "LOGOUT":
				writeLine(conn, "* BYE Logging out")
				writeLine(conn, "%s OK LOGOUT completed", cmd.tag)
				return false
			default:
				writeLine(conn, "%s OK %s completed", cmd.tag, cmd.verb)
			}
			return true
		})
	}
}
