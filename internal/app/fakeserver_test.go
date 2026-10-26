package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/mail"
	"slices"
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
	store          *mailStore
	counts         map[string]int
	names          map[string][]string
	connHandlers   []func(net.Conn)
	connHandlerIdx int
	mu             sync.Mutex
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	return listenFakeServer(t, nil)
}

// listenFakeServer starts the accept loop. store is set before the loop
// starts so the loop reads it without locking.
func listenFakeServer(t *testing.T, store *mailStore) *fakeServer {
	t.Helper()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeServer listen: %v", err)
	}
	s := &fakeServer{
		ln:     ln,
		store:  store,
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

// addConnHandler queues a per-connection handler, used in Accept order. Once
// the queue is empty, connections use the store-backed handler if the server
// has a store, else handle().
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
		switch {
		case handler != nil:
			go handler(conn)
		case s.store != nil:
			go s.serveStore(conn)
		default:
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

var imapQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func envelopeFor(m storedMsg) string {
	if m.envelopeID == "" {
		return nilEnvelope
	}
	return "(NIL NIL NIL NIL NIL NIL NIL NIL NIL \"" + imapQuoteEscaper.Replace(m.envelopeID) + "\")"
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

// fakeMsg is one message a store-backed server holds. An empty body means
// imapFullBody(msgID). A non-empty envelopeID is the Message-Id its ENVELOPE
// reports; empty leaves it NIL.
type fakeMsg struct {
	msgID      string
	body       string
	envelopeID string
}

type storedMsg struct {
	msgID      string
	body       string
	envelopeID string
	uid        uint32
}

type appendRecord struct {
	folder string
	msgID  string
}

// mailStore is the mailbox state behind a store-backed fakeServer; the server's
// mu guards every field. A non-empty reply knob replaces the tagged OK status
// of its command.
type mailStore struct {
	folders      map[string][]storedMsg
	nextUID      map[string]uint32
	noSelect     map[string]bool
	openReply    map[string]string
	statusReply  map[string]string
	delimiter    string
	loginReply   string
	createReply  string
	appendReply  string
	dropOnOpen   string
	appends      []appendRecord
	loginRefusal int
	seenMutating int
}

// newStoreServer starts a fake IMAP server with in-memory mailboxes. Queued
// connHandlers take precedence. A server with no folders reports no delimiter,
// so the client assumes "/".
func newStoreServer(t *testing.T, delimiter string, folders map[string][]fakeMsg) *fakeServer {
	t.Helper()
	st := &mailStore{
		folders:   make(map[string][]storedMsg, len(folders)),
		nextUID:   make(map[string]uint32, len(folders)),
		delimiter: delimiter,
	}
	for name, msgs := range folders {
		st.folders[name] = nil
		st.nextUID[name] = 1
		for _, m := range msgs {
			body := m.body
			if body == "" {
				body = imapFullBody(m.msgID)
			}
			st.addStored(name, storedMsg{msgID: m.msgID, body: body, envelopeID: m.envelopeID})
		}
	}
	return listenFakeServer(t, st)
}

func (st *mailStore) add(folder, msgID, body string) {
	st.addStored(folder, storedMsg{msgID: msgID, body: body})
}

func (st *mailStore) addStored(folder string, m storedMsg) {
	m.uid = st.nextUID[folder]
	st.nextUID[folder] = m.uid + 1
	st.folders[folder] = append(st.folders[folder], m)
}

func (s *fakeServer) addFolder(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store.folders[name]; ok {
		return
	}
	s.store.folders[name] = nil
	s.store.nextUID[name] = 1
}

// markNoSelect adds each folder that is missing and lists it as \Noselect, the
// way a server lists a container such as Gmail's "[Gmail]".
func (s *fakeServer) markNoSelect(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.noSelect == nil {
		s.store.noSelect = make(map[string]bool, len(names))
	}
	for _, name := range names {
		if _, ok := s.store.folders[name]; !ok {
			s.store.folders[name] = nil
			s.store.nextUID[name] = 1
		}
		s.store.noSelect[name] = true
	}
}

func (s *fakeServer) refuseLogin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.loginReply = "NO [AUTHENTICATIONFAILED] Invalid credentials"
}

// refuseLoginAt makes only the n-th LOGIN (1-based, counted over the life of the
// server) end with a refusal. Every other LOGIN succeeds.
func (s *fakeServer) refuseLoginAt(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.loginRefusal = n
}

func (s *fakeServer) refuseCreate() {
	s.refuseCreateWith("NO CREATE refused by policy")
}

// refuseCreateWith makes every CREATE end with reply, which carries its own
// status word, as the tagged status line.
func (s *fakeServer) refuseCreateWith(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.createReply = reply
}

// refuseOpenWith makes every EXAMINE and SELECT of folder end with reply, which
// carries its own status word, as the tagged status line. The folder stays in LIST.
func (s *fakeServer) refuseOpenWith(folder, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.openReply == nil {
		s.store.openReply = make(map[string]string)
	}
	s.store.openReply[folder] = reply
}

// refuseStatusWith makes every STATUS of folder end with reply, which carries its
// own status word, as the tagged status line. The key is the name on the wire.
func (s *fakeServer) refuseStatusWith(folder, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.statusReply == nil {
		s.store.statusReply = make(map[string]string)
	}
	s.store.statusReply[folder] = reply
}

func (s *fakeServer) dropOnceOnOpen(folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.dropOnOpen = folder
}

// takeDropOnOpen reports whether the open of name must drop. Only on a match does
// it clear the knob and count the synthetic event "DROP".
func (s *fakeServer) takeDropOnOpen(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.dropOnOpen == "" || s.store.dropOnOpen != name {
		return false
	}
	s.store.dropOnOpen = ""
	s.counts["DROP"]++
	return true
}

func (s *fakeServer) refuseAppend() {
	s.refuseAppendWith("NO Quota exceeded")
}

// refuseAppendWith makes every APPEND end with reply, which carries its own
// status word, as the tagged status line.
func (s *fakeServer) refuseAppendWith(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.appendReply = reply
}

// capturedNames returns the mailbox names the clients passed to verb (CREATE
// for store servers), in arrival order.
func (s *fakeServer) capturedNames(verb string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names[verb])
}

// appended returns the Message-Ids APPENDed to folder, in arrival order.
func (s *fakeServer) appended(folder string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, rec := range s.store.appends {
		if rec.folder == folder {
			ids = append(ids, rec.msgID)
		}
	}
	return ids
}

// folderMsgIDs returns the Message-Ids stored in folder in UID order, or nil
// when the folder does not exist.
func (s *fakeServer) folderMsgIDs(folder string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, m := range s.store.folders[folder] {
		ids = append(ids, m.msgID)
	}
	return ids
}

func (s *fakeServer) hasFolder(folder string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.store.folders[folder]
	return ok
}

// seenMutatingFetches counts FETCH requests that would set \Seen on the
// source: non-PEEK BODY[...] and bare RFC822, RFC822.TEXT or RFC822.HEADER.
func (s *fakeServer) seenMutatingFetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.seenMutating
}

// replyWriter writes tagged and untagged response lines to one connection.
// Write errors are dropped: a broken connection ends the read loop anyway.
type replyWriter struct{ conn net.Conn }

func (w replyWriter) line(format string, args ...any) {
	_, _ = fmt.Fprintf(w.conn, format+"\r\n", args...)
}

// serveStore serves one connection from the store. It uses synchronising
// literals: the greeting does not advertise LITERAL+.
func (s *fakeServer) serveStore(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	rw := replyWriter{conn: conn}
	rw.line("* OK [CAPABILITY IMAP4rev1] fake ready")

	rd := bufio.NewReader(conn)
	var selected string
	for {
		raw, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		raw = strings.TrimRight(raw, "\r\n")
		if raw == "" {
			continue
		}
		tag, verb, arg := splitCommand(raw)
		s.recordCommand(verb, arg)

		switch verb {
		case "LOGIN":
			s.storeLogin(rw, tag)
		case "LOGOUT":
			rw.line("* BYE Logging out")
			rw.line("%s OK LOGOUT completed", tag)
			return
		case "CAPABILITY":
			rw.line("* CAPABILITY IMAP4rev1")
			rw.line("%s OK CAPABILITY completed", tag)
		case "LIST":
			s.storeList(rw, tag)
		case "EXAMINE", "SELECT":
			name, _ := parseMailboxArg(arg)
			if s.takeDropOnOpen(name) {
				rw.line("* BYE Autologout")
				return
			}
			if s.storeSelect(rw, tag, verb, name) {
				selected = name
			}
		case "STATUS":
			s.storeStatus(rw, tag, arg)
		case "FETCH":
			s.storeFetch(rw, tag, selected, arg, false)
		case "UID":
			sub, rest, _ := strings.Cut(arg, " ")
			if strings.EqualFold(sub, "FETCH") {
				s.storeFetch(rw, tag, selected, rest, true)
			} else {
				rw.line("%s OK UID %s completed", tag, strings.ToUpper(sub))
			}
		case "CREATE":
			s.storeCreate(rw, tag, arg)
		case "APPEND":
			if !s.storeAppend(rw, rd, tag, arg) {
				return
			}
		default:
			rw.line("%s OK %s completed", tag, verb)
		}
	}
}

// splitCommand splits "tag VERB args" and upper-cases the verb.
func splitCommand(line string) (tag, verb, arg string) {
	tag, rest, _ := strings.Cut(line, " ")
	verb, arg, _ = strings.Cut(rest, " ")
	return tag, strings.ToUpper(verb), arg
}

// recordCommand counts verb, plus "UID <SUB>" for UID commands. CREATE, EXAMINE,
// SELECT and STATUS names are recorded on arrival, whether or not the server accepts them.
func (s *fakeServer) recordCommand(verb, arg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[verb]++
	switch verb {
	case "UID":
		sub, _, _ := strings.Cut(arg, " ")
		s.counts["UID "+strings.ToUpper(sub)]++
	case "CREATE", "EXAMINE", "SELECT", "STATUS":
		name, _ := parseMailboxArg(arg)
		s.names[verb] = append(s.names[verb], name)
	}
}

func (s *fakeServer) storeLogin(rw replyWriter, tag string) {
	s.mu.Lock()
	reply := s.store.loginReply
	if reply == "" && s.store.loginRefusal > 0 && s.counts["LOGIN"] == s.store.loginRefusal {
		reply = "NO [AUTHENTICATIONFAILED] Invalid credentials"
	}
	s.mu.Unlock()
	if reply != "" {
		rw.line("%s %s", tag, reply)
		return
	}
	rw.line("%s OK LOGIN completed", tag)
}

func (s *fakeServer) storeList(rw replyWriter, tag string) {
	s.mu.Lock()
	delim := s.store.delimiter
	names := slices.Sorted(maps.Keys(s.store.folders))
	noSelect := maps.Clone(s.store.noSelect)
	s.mu.Unlock()
	for _, name := range names {
		attrs := ""
		if noSelect[name] {
			attrs = `\HasChildren \Noselect`
		}
		rw.line(`* LIST (%s) "%s" "%s"`, attrs, delim, name)
	}
	rw.line("%s OK LIST completed", tag)
}

// storeSelect reports whether the folder exists and is now selected.
func (s *fakeServer) storeSelect(rw replyWriter, tag, verb, name string) bool {
	s.mu.Lock()
	msgs, ok := s.store.folders[name]
	n := len(msgs)
	next := s.store.nextUID[name]
	noSelect := s.store.noSelect[name]
	refusal := s.store.openReply[name]
	s.mu.Unlock()
	if refusal != "" {
		rw.line("%s %s", tag, refusal)
		return false
	}
	if !ok || noSelect {
		rw.line("%s NO [NONEXISTENT] Unknown Mailbox", tag)
		return false
	}
	access := "READ-ONLY"
	if verb == "SELECT" {
		access = "READ-WRITE"
	}
	rw.line("* %d EXISTS", n)
	rw.line("* 0 RECENT")
	rw.line(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`)
	rw.line("* OK [UIDVALIDITY 1] UIDs valid")
	rw.line("* OK [UIDNEXT %d] Predicted next UID", next)
	rw.line("%s OK [%s] %s completed", tag, access, verb)
	return true
}

func (s *fakeServer) storeStatus(rw replyWriter, tag, arg string) {
	name, _ := parseMailboxArg(arg)
	s.mu.Lock()
	msgs, ok := s.store.folders[name]
	n := len(msgs)
	noSelect := s.store.noSelect[name]
	refusal := s.store.statusReply[name]
	s.mu.Unlock()
	if refusal != "" {
		rw.line("%s %s", tag, refusal)
		return
	}
	if !ok || noSelect {
		rw.line("%s NO [NONEXISTENT] Unknown Mailbox", tag)
		return
	}
	rw.line(`* STATUS "%s" (MESSAGES %d)`, name, n)
	rw.line("%s OK STATUS completed", tag)
}

// storeFetch answers FETCH and UID FETCH. Only the requested items are sent,
// except UID, which a UID FETCH response always carries.
func (s *fakeServer) storeFetch(rw replyWriter, tag, selected, arg string, byUID bool) {
	set, items, _ := strings.Cut(arg, " ")
	upper := strings.ToUpper(items)

	s.mu.Lock()
	if mutatesSeen(upper) {
		s.store.seenMutating++
	}
	msgs := slices.Clone(s.store.folders[selected])
	s.mu.Unlock()

	if selected == "" {
		rw.line("%s BAD No mailbox selected", tag)
		return
	}

	ranges := parseIDSet(set)
	wantUID := byUID || hasToken(upper, "UID")
	wantSize := hasToken(upper, "RFC822.SIZE")
	wantHeader := strings.Contains(upper, "HEADER.FIELDS")
	wantEnvelope := strings.Contains(upper, "ENVELOPE")
	wantBody := strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "BODY[]")

	for i, m := range msgs {
		id := uint32(i + 1)
		if byUID {
			id = m.uid
		}
		if !inIDSet(ranges, id) {
			continue
		}
		var parts []string
		if wantUID {
			parts = append(parts, fmt.Sprintf("UID %d", m.uid))
		}
		if wantSize {
			parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(m.body)))
		}
		if wantEnvelope {
			parts = append(parts, "ENVELOPE "+envelopeFor(m))
		}
		if wantHeader {
			hdr := imapMsgIDHeader(m.msgID)
			parts = append(parts, fmt.Sprintf("BODY[HEADER.FIELDS (\"MESSAGE-ID\")] {%d}\r\n%s", len(hdr), hdr))
		}
		if wantBody {
			parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(m.body), m.body))
		}
		rw.line("* %d FETCH (%s)", i+1, strings.Join(parts, " "))
	}
	rw.line("%s OK FETCH completed", tag)
}

func (s *fakeServer) storeCreate(rw replyWriter, tag, arg string) {
	name, _ := parseMailboxArg(arg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.createReply != "" {
		rw.line("%s %s", tag, s.store.createReply)
		return
	}
	if _, ok := s.store.folders[name]; ok {
		rw.line("%s NO [ALREADYEXISTS] Mailbox already exists", tag)
		return
	}
	s.store.folders[name] = nil
	s.store.nextUID[name] = 1
	rw.line("%s OK CREATE completed", tag)
}

// storeAppend drains the literal before any tagged reply, refusals included:
// replying NO before the continuation races go-imap's literal writer. It
// reports false when the connection died mid-literal.
func (s *fakeServer) storeAppend(rw replyWriter, rd *bufio.Reader, tag, arg string) bool {
	name, _ := parseMailboxArg(arg)
	size := parseLiteralSize(arg)

	rw.line("+ Ready for literal data")
	lit := make([]byte, size)
	if _, err := io.ReadFull(rd, lit); err != nil {
		return false
	}
	// The command line ends with CRLF after the literal.
	if _, err := rd.ReadString('\n'); err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.appendReply != "" {
		rw.line("%s %s", tag, s.store.appendReply)
		return true
	}
	if _, ok := s.store.folders[name]; !ok {
		rw.line("%s NO [TRYCREATE] Mailbox does not exist", tag)
		return true
	}
	if s.store.noSelect[name] {
		rw.line("%s NO [NONEXISTENT] Unknown Mailbox", tag)
		return true
	}
	msgID := ""
	if m, err := mail.ReadMessage(bytes.NewReader(lit)); err == nil {
		msgID = strings.Trim(m.Header.Get("Message-Id"), "<>")
	}
	s.store.add(name, msgID, string(lit))
	s.store.appends = append(s.store.appends, appendRecord{folder: name, msgID: msgID})
	rw.line("%s OK APPEND completed", tag)
	return true
}

// parseMailboxArg reads a quoted or atom mailbox name from the start of arg
// and returns it with the unparsed remainder.
func parseMailboxArg(arg string) (name, rest string) {
	arg = strings.TrimLeft(arg, " ")
	if !strings.HasPrefix(arg, `"`) {
		name, rest, _ = strings.Cut(arg, " ")
		return name, rest
	}
	var b strings.Builder
	for i := 1; i < len(arg); i++ {
		switch c := arg[i]; {
		case c == '\\' && i+1 < len(arg):
			i++
			b.WriteByte(arg[i])
		case c == '"':
			return b.String(), arg[i+1:]
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), ""
}

type idRange struct{ lo, hi uint32 }

// parseIDSet parses an IMAP sequence or UID set: "5", "2:4", "1,3,7:*".
func parseIDSet(set string) []idRange {
	var out []idRange
	for part := range strings.SplitSeq(set, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		if !isRange {
			hi = lo
		}
		a, b := parseSetBound(lo), parseSetBound(hi)
		if a > b {
			a, b = b, a
		}
		out = append(out, idRange{lo: a, hi: b})
	}
	return out
}

func parseSetBound(s string) uint32 {
	if s == "*" {
		return math.MaxUint32
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func inIDSet(ranges []idRange, id uint32) bool {
	return slices.ContainsFunc(ranges, func(r idRange) bool { return id >= r.lo && id <= r.hi })
}

// hasToken reports whether tok appears in the upper-cased item list as a whole
// word, so "RFC822" does not match "RFC822.SIZE".
func hasToken(upperItems, tok string) bool {
	words := strings.FieldsFunc(upperItems, func(r rune) bool { return r == ' ' || r == '(' || r == ')' })
	return slices.Contains(words, tok)
}

// mutatesSeen reports whether a FETCH item list would set \Seen: a BODY[...]
// without PEEK, or a bare RFC822, RFC822.TEXT or RFC822.HEADER.
func mutatesSeen(upperItems string) bool {
	if strings.Contains(upperItems, "BODY[") {
		return true
	}
	return hasToken(upperItems, "RFC822") || hasToken(upperItems, "RFC822.TEXT") || hasToken(upperItems, "RFC822.HEADER")
}

// scriptedHandler serves folders. react sees each command first, after any
// APPEND literal: a reply replaces the tagged status, an empty reply goes
// silent, handled=false keeps the default. nth is 1-based.
func scriptedHandler(srv *fakeServer, folders []string, react func(verb string, nth int) (reply string, handled bool)) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		rw := replyWriter{conn: conn}
		rw.line("* OK [CAPABILITY IMAP4rev1] fake ready")

		rd := bufio.NewReader(conn)
		seen := make(map[string]int)
		for {
			raw, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			raw = strings.TrimRight(raw, "\r\n")
			if raw == "" {
				continue
			}
			tag, verb, arg := splitCommand(raw)
			srv.recordCommand(verb, arg)
			seen[verb]++

			if verb == "APPEND" {
				rw.line("+ Ready for literal data")
				if _, err := io.CopyN(io.Discard, rd, int64(parseLiteralSize(arg))); err != nil {
					return
				}
				if _, err := rd.ReadString('\n'); err != nil {
					return
				}
			}
			if reply, handled := react(verb, seen[verb]); handled {
				if reply == "" {
					_, _ = rd.ReadByte()
					return
				}
				rw.line("%s %s", tag, reply)
				continue
			}

			switch verb {
			case "LIST":
				for _, name := range folders {
					rw.line(`* LIST () "/" "%s"`, name)
				}
				rw.line("%s OK LIST completed", tag)
			case "EXAMINE", "SELECT":
				rw.line("* 0 EXISTS")
				rw.line("* 0 RECENT")
				rw.line("%s OK [READ-ONLY] %s completed", tag, verb)
			case "LOGOUT":
				rw.line("* BYE Logging out")
				rw.line("%s OK LOGOUT completed", tag)
				return
			default:
				rw.line("%s OK %s completed", tag, verb)
			}
		}
	}
}

// listFailsAfterConnectHandler lets client.New spend the first LIST on its cache
// and refuses every later one, so ListMailboxes fails.
func listFailsAfterConnectHandler(srv *fakeServer, folders ...string) func(net.Conn) {
	return scriptedHandler(srv, folders, func(verb string, nth int) (string, bool) {
		if verb == "LIST" && nth > 1 {
			return "NO [CANNOT] list refused", true
		}
		return "", false
	})
}

func refuseLoginAfterAppendHandler(srv *fakeServer) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		rw := replyWriter{conn: conn}
		rw.line("* OK [CAPABILITY IMAP4rev1] fake ready")

		rd := bufio.NewReader(conn)
		for {
			raw, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			raw = strings.TrimRight(raw, "\r\n")
			if raw == "" {
				continue
			}
			tag, verb, arg := splitCommand(raw)
			srv.recordCommand(verb, arg)

			switch verb {
			case "LOGIN":
				srv.storeLogin(rw, tag)
			case "LIST":
				srv.storeList(rw, tag)
			case "APPEND":
				rw.line("+ Ready for literal data")
				if _, err := io.CopyN(io.Discard, rd, int64(parseLiteralSize(arg))); err != nil {
					return
				}
				if _, err := rd.ReadString('\n'); err != nil {
					return
				}
				srv.refuseLogin()
				return
			case "LOGOUT":
				rw.line("* BYE Logging out")
				rw.line("%s OK LOGOUT completed", tag)
				return
			default:
				rw.line("%s OK %s completed", tag, verb)
			}
		}
	}
}
