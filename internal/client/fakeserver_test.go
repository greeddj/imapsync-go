package client

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
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

	// IMAP greeting - go-imap's imapclient.New blocks until it sees this.
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

// buildTestIMAPMessage constructs a minimal Message suitable for
// passing to AppendMessage. The body section key must be the resp() form of
// fullBodyPeekSection (Peek=false, empty BodyPartName) because go-imap always
// stores server-returned body sections without the PEEK flag.
func buildTestIMAPMessage() Message {
	section := &imap.BodySectionName{} // Peek=false matches fullBodyPeekSection.resp()
	msg := imap.NewMessage(1, []imap.FetchItem{fullBodyPeekSection.FetchItem()})
	msg.Body[section] = strings.NewReader("From: test@example.com\r\nSubject: hi\r\n\r\nHello\r\n")
	msg.Envelope = &imap.Envelope{MessageId: "test-id@example.com"}
	return Message{m: msg}
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
	scriptedGreeting = "* OK [CAPABILITY IMAP4rev1] fake ready"
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

// scriptFolder is one mailbox of a mailboxScript; each entry of sizes is the
// RFC822.SIZE of one message.
type scriptFolder struct {
	name  string
	sizes []uint32
}

// mailboxScript describes the answers of a handler built by handler. A refusal
// knob turns the command into a tagged NO that classifyError does not retry.
type mailboxScript struct {
	statusRefused   map[string]bool   // by folder
	examineRefused  map[string]bool   // by folder
	createReplies   map[string]string // folder -> reply after the tag; empty means OK
	folders         []scriptFolder
	uidMessages     int // messages returned for each UID FETCH
	listRefused     bool
	uidFetchRefused bool
}

func (m *mailboxScript) folder(name string) *scriptFolder {
	for i := range m.folders {
		if m.folders[i].name == name {
			return &m.folders[i]
		}
	}
	return nil
}

// handler serves LIST, STATUS, SELECT/EXAMINE, FETCH, UID FETCH and CREATE from
// the script. UID FETCH returns up to uidMessages messages from the lowest
// requested UID.
func (m *mailboxScript) handler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		var selected string
		serveCommands(srv, conn, scriptedGreeting, func(_ *bufio.Reader, cmd command) bool {
			first := firstArg(cmd.arg)
			switch cmd.verb {
			case "LIST":
				if m.listRefused {
					writeLine(conn, "%s NO [UNAVAILABLE] list refused", cmd.tag)
					break
				}
				for _, f := range m.folders {
					writeLine(conn, `* LIST (\HasNoChildren) "/" "%s"`, f.name)
				}
				writeLine(conn, "%s OK LIST completed", cmd.tag)
			case "STATUS":
				if m.statusRefused[first] {
					writeLine(conn, "%s NO [CANNOT] status refused", cmd.tag)
					break
				}
				count := 0
				if f := m.folder(first); f != nil {
					count = len(f.sizes)
				}
				writeLine(conn, `* STATUS "%s" (MESSAGES %d)`, first, count)
				writeLine(conn, "%s OK STATUS completed", cmd.tag)
			case "SELECT", "EXAMINE":
				if m.examineRefused[first] {
					writeLine(conn, "%s NO [CANNOT] examine refused", cmd.tag)
					break
				}
				selected = first
				count := 0
				if f := m.folder(first); f != nil {
					count = len(f.sizes)
				}
				writeLine(conn, "* %d EXISTS", count)
				writeLine(conn, "* 0 RECENT")
				writeLine(conn, "%s OK [READ-ONLY] %s completed", cmd.tag, cmd.verb)
			case "FETCH":
				set, err := imap.ParseSeqSet(first)
				if err != nil {
					writeLine(conn, "%s BAD %v", cmd.tag, err)
					break
				}
				if f := m.folder(selected); f != nil {
					for i, size := range f.sizes {
						if seq := uint32(i + 1); set.Contains(seq) {
							writeLine(conn, "* %d FETCH (RFC822.SIZE %d)", seq, size)
						}
					}
				}
				writeLine(conn, "%s OK FETCH completed", cmd.tag)
			case "UID":
				m.serveUID(conn, cmd)
			case "CREATE":
				if reply := m.createReplies[first]; reply != "" {
					writeLine(conn, "%s %s", cmd.tag, reply)
					break
				}
				writeLine(conn, "%s OK CREATE completed", cmd.tag)
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

func (m *mailboxScript) serveUID(conn net.Conn, cmd command) {
	fields := strings.Fields(cmd.arg)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "FETCH") {
		writeLine(conn, "%s OK UID completed", cmd.tag)
		return
	}
	if m.uidFetchRefused {
		writeLine(conn, "%s NO [UNAVAILABLE] fetch refused", cmd.tag)
		return
	}
	set, err := imap.ParseSeqSet(fields[1])
	if err != nil || len(set.Set) == 0 {
		writeLine(conn, "%s BAD bad uid set", cmd.tag)
		return
	}
	first := set.Set[0].Start
	for i := range m.uidMessages {
		uid := first + uint32(i)
		if !set.Contains(uid) {
			break
		}
		body := "Subject: uid " + strconv.FormatUint(uint64(uid), 10) + "\r\n\r\nbody\r\n"
		writeLine(conn, "* %d FETCH (UID %d BODY[] {%d}\r\n%s)", uid, uid, len(body), body)
	}
	writeLine(conn, "%s OK UID FETCH completed", cmd.tag)
}

// newScriptedClient starts a fake server whose first connection follows script
// and returns it with a connected client.
func newScriptedClient(t *testing.T, script *mailboxScript) (*fakeServer, *Client) {
	t.Helper()

	srv := newFakeServer(t)
	srv.addConnHandler(script.handler(srv))
	return srv, newClientWithFake(t, srv)
}

// literalSize extracts N from the trailing "{N}" of an APPEND line.
func literalSize(arg string) (int64, bool) {
	open := strings.LastIndexByte(arg, '{')
	end := strings.LastIndexByte(arg, '}')
	if open < 0 || end < open {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(arg[open+1:end], "+"), 10, 64)
	return n, err == nil
}

// appendOutcomeHandler drains the whole APPEND literal before sending appendReply.
// Replying earlier would race go-imap's literal writer.
func appendOutcomeHandler(srv *fakeServer, appendReply string) func(net.Conn) {
	return func(conn net.Conn) {
		serveCommands(srv, conn, scriptedGreeting, func(rd *bufio.Reader, cmd command) bool {
			switch cmd.verb {
			case "APPEND":
				size, ok := literalSize(cmd.arg)
				if !ok {
					writeLine(conn, "%s BAD no literal", cmd.tag)
					return true
				}
				writeLine(conn, "+ Ready for literal data")
				if _, err := io.CopyN(io.Discard, rd, size); err != nil {
					return false
				}
				if _, err := rd.ReadString('\n'); err != nil {
					return false
				}
				writeLine(conn, "%s %s", cmd.tag, appendReply)
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

// byeAfterLoginHandler accepts LOGIN, then sends BYE and closes once release is
// closed. The gate keeps the BYE from racing the client reading the LOGIN reply.
func byeAfterLoginHandler(srv *fakeServer, release <-chan struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		serveCommands(srv, conn, scriptedGreeting, func(_ *bufio.Reader, cmd command) bool {
			if cmd.verb != "LOGIN" {
				writeLine(conn, "%s OK %s completed", cmd.tag, cmd.verb)
				return true
			}
			writeLine(conn, "%s OK LOGIN completed", cmd.tag)
			<-release
			writeLine(conn, "* BYE server shutting down")
			return false
		})
	}
}

// interceptHandler answers common commands like a one-message server. intercept
// sees each command first: handled=true skips the default answer, keep=false
// ends the connection.
func interceptHandler(srv *fakeServer, intercept func(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool)) func(net.Conn) {
	return func(conn net.Conn) {
		serveCommands(srv, conn, scriptedGreeting, func(rd *bufio.Reader, cmd command) bool {
			if handled, keep := intercept(conn, rd, cmd); handled {
				return keep
			}
			switch cmd.verb {
			case "LIST":
				writeLine(conn, `* LIST (\HasNoChildren) "/" "INBOX"`)
				writeLine(conn, "%s OK LIST completed", cmd.tag)
			case "STATUS":
				writeLine(conn, `* STATUS "%s" (MESSAGES 1)`, firstArg(cmd.arg))
				writeLine(conn, "%s OK STATUS completed", cmd.tag)
			case "SELECT", "EXAMINE":
				writeLine(conn, "* 1 EXISTS")
				writeLine(conn, "* 0 RECENT")
				writeLine(conn, "%s OK [READ-ONLY] %s completed", cmd.tag, cmd.verb)
			case "FETCH":
				writeLine(conn, "* 1 FETCH (RFC822.SIZE 10)")
				writeLine(conn, "%s OK FETCH completed", cmd.tag)
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

// silentHandler accepts a connection and never writes, so the client waits for a
// greeting. accepted closes on arrival, closed closes when the client hangs up.
func silentHandler(accepted, closed chan<- struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()

		close(accepted)
		_, _ = conn.Read(make([]byte, 1))
		close(closed)
	}
}

// holdHandler writes greeting, then keeps the connection open until the client
// hangs up. closed closes at that moment.
func holdHandler(greeting string, closed chan<- struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()

		writeLine(conn, "%s", greeting)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}
}

// retainDialed keeps every connection c dials reachable until the test ends. Without
// it the finalizer of an unreachable connection closes a socket the code under test leaked.
func retainDialed(t *testing.T, c *Client) {
	t.Helper()

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	dial := c.dialFn
	c.dialFn = func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := dial(ctx, addr)
		if err == nil {
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
		return conn, err
	}
	t.Cleanup(func() { runtime.KeepAlive(conns) })
}

// stall goes silent on the first command match accepts, so the client blocks
// until Cancel or a canceled context closes its connection.
type stall struct {
	match   func(cmd command) bool
	before  func(conn net.Conn, rd *bufio.Reader, cmd command) bool
	reached chan struct{}
	once    sync.Once
}

func newStall(match func(cmd command) bool) *stall {
	return &stall{match: match, reached: make(chan struct{})}
}

func (s *stall) intercept(conn net.Conn, rd *bufio.Reader, cmd command) (handled, keep bool) {
	if !s.match(cmd) {
		return false, true
	}
	if s.before != nil && !s.before(conn, rd, cmd) {
		return true, false
	}
	s.once.Do(func() { close(s.reached) })
	_, _ = rd.ReadByte()
	return true, false
}

// firedContext expires with context.DeadlineExceeded when fire is called. That
// differs from the context.Canceled safeCall returns after Cancel, so a test can
// tell which guard answered.
type firedContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newFiredContext(parent context.Context) *firedContext {
	return &firedContext{Context: parent, done: make(chan struct{})}
}

func (c *firedContext) Done() <-chan struct{} { return c.done }

func (c *firedContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *firedContext) fire() { c.once.Do(func() { close(c.done) }) }

// verbIs matches commands with the given verb.
func verbIs(verb string) func(command) bool {
	return func(cmd command) bool { return cmd.verb == verb }
}

// verbOn matches commands with the given verb whose first argument is name.
func verbOn(verb, name string) func(command) bool {
	return func(cmd command) bool { return cmd.verb == verb && firstArg(cmd.arg) == name }
}

// byeOn is an interceptHandler hook that answers each command match accepts with
// an untagged BYE and ends the connection, as a server does on autologout.
func byeOn(match func(command) bool) func(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return func(conn net.Conn, _ *bufio.Reader, cmd command) (handled, keep bool) {
		if !match(cmd) {
			return false, true
		}
		writeLine(conn, "* BYE Autologout; idle for too long")
		return true, false
	}
}

// awaitSignal fails the test when ch stays open for too long.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
	}
}

// awaitError returns what errc delivers, failing the test when the call is
// still blocked after a few seconds.
func awaitError(t *testing.T, errc <-chan error, what string) error {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-errc:
		return err
	case <-timer.C:
		t.Fatalf("%s did not return within 5s", what)
		return nil
	}
}

const (
	// tripLimit is an idle limit that a test expects to see expire.
	tripLimit = 100 * time.Millisecond
	// holdLimit is an idle limit that must not expire during a test: stimulus
	// gaps stay within a tenth of it and observation windows span several.
	holdLimit = 250 * time.Millisecond
)

// newWatchedClient connects a client whose watchdog trips after idle. Backoff is
// zero and reconnectDur stays zero, so a reconnect never sleeps.
func newWatchedClient(t *testing.T, srv *fakeServer, idle time.Duration) *Client {
	t.Helper()

	c := newDisconnectedClient(t, srv)
	c.idleDur = idle
	c.backoff = 0
	if err := c.connectAndLogin(t.Context()); err != nil {
		t.Fatalf("newWatchedClient connectAndLogin: %v", err)
	}
	return c
}

// serveNormally is an interceptHandler hook that leaves every command to the
// default answers.
func serveNormally(net.Conn, *bufio.Reader, command) (handled, keep bool) {
	return false, true
}

// endedSignal runs handler and closes ended once it returns. With a stall or an
// idle handler that happens when the client hangs up on the connection.
func endedSignal(handler func(net.Conn), ended chan<- struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		defer close(ended)
		handler(conn)
	}
}

// readAppendLiteral sends the continuation request and drains the literal with
// its CRLF. It reports false when the client hangs up first; fits stall.before.
func readAppendLiteral(conn net.Conn, rd *bufio.Reader, cmd command) bool {
	size, ok := literalSize(cmd.arg)
	if !ok {
		return false
	}
	writeLine(conn, "+ Ready for literal data")
	if _, err := io.CopyN(io.Discard, rd, size); err != nil {
		return false
	}
	_, err := rd.ReadString('\n')
	return err == nil
}

// trickle writes s one byte at a time, gap apart. It reports false when a write
// fails.
func trickle(conn net.Conn, s string, gap time.Duration) bool {
	for i := range len(s) {
		if _, err := conn.Write([]byte{s[i]}); err != nil {
			return false
		}
		time.Sleep(gap)
	}
	return true
}

// stayOpen fails the test when ch closes within d.
func stayOpen(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		t.Fatalf("%s within %s", what, d)
	case <-timer.C:
	}
}

// awaitCall runs call in a goroutine and returns its error. A server stall then
// fails the test after a few seconds instead of hanging it.
func awaitCall(t *testing.T, what string, call func() error) error {
	t.Helper()

	errc := make(chan error, 1)
	go func() { errc <- call() }()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-errc:
		return err
	case <-timer.C:
		t.Fatalf("%s still blocked 5s into a server stall", what)
		return nil
	}
}
