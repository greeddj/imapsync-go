package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
)

// Test_runFolderSync_successCounter asserts that runFolderSync returns
// (synced=2, errors=0) when src delivers 2 messages and dst accepts both
// APPENDs. It also verifies that APPEND was issued exactly twice.
func Test_runFolderSync_successCounter(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	body1 := imapFullBody("msg1@x")
	body2 := imapFullBody("msg2@x")

	srcBodies := map[string][]struct {
		body string
		uid  uint32
	}{
		"INBOX": {
			{uid: 1, body: body1},
			{uid: 2, body: body2},
		},
	}
	srcSrv.addConnHandler(uidFetchBodyHandler(srcSrv, []string{"INBOX"}, srcBodies, ""))
	dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, ""))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	w := &syncWorker{src: srcC, dst: dstC}

	plan := FolderSyncPlan{
		SourceFolder:            "INBOX",
		DestinationFolder:       "INBOX",
		SrcUIDs:                 []uint32{1, 2},
		NewMessages:             2,
		DestinationFolderExists: true,
	}

	pw := progress.NewWriter(1, true)
	tr := progress.NewTracker("test", 10)

	synced, errors, _ := runFolderSync(context.Background(), w, plan, new(throttleStop), tr, 0, 1, pw, false)

	if synced != 2 {
		t.Errorf("synced=%d, want 2", synced)
	}
	if errors != 0 {
		t.Errorf("errors=%d, want 0", errors)
	}
	if got := dstSrv.callCount("APPEND"); got != 2 {
		t.Errorf("APPEND count=%d, want 2", got)
	}
}

// Test_runFolderSync_appendErrorIncrementsErrorsCounter asserts that when dst
// rejects APPEND with NO, the errors counter is incremented, synced stays 0,
// and the tracker is marked as errored.
func Test_runFolderSync_appendErrorIncrementsErrorsCounter(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	body1 := imapFullBody("single@x")
	srcBodies := map[string][]struct {
		body string
		uid  uint32
	}{
		"INBOX": {{uid: 1, body: body1}},
	}

	srcSrv.addConnHandler(uidFetchBodyHandler(srcSrv, []string{"INBOX"}, srcBodies, ""))
	dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, "NO [CANNOT] APPEND refused by policy"))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	w := &syncWorker{src: srcC, dst: dstC}

	plan := FolderSyncPlan{
		SourceFolder:            "INBOX",
		DestinationFolder:       "INBOX",
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}

	pw := progress.NewWriter(1, true)
	tr := progress.NewTracker("test", 10)

	synced, errors, _ := runFolderSync(context.Background(), w, plan, new(throttleStop), tr, 0, 1, pw, false)

	if synced != 0 {
		t.Errorf("synced=%d, want 0", synced)
	}
	if errors != 1 {
		t.Errorf("errors=%d, want 1", errors)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
}

// Test_runFolderSync_canceledContext_returnsEarly asserts that runFolderSync
// short-circuits and returns (0, 0) when the context is already canceled.
func Test_runFolderSync_canceledContext_returnsEarly(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	w := &syncWorker{src: srcC, dst: dstC}

	plan := FolderSyncPlan{
		SourceFolder:      "INBOX",
		DestinationFolder: "INBOX",
		SrcUIDs:           []uint32{1},
		NewMessages:       1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling runFolderSync

	pw := progress.NewWriter(1, true)
	tr := progress.NewTracker("test", 10)

	synced, errors, _ := runFolderSync(ctx, w, plan, new(throttleStop), tr, 0, 1, pw, false)

	if synced != 0 || errors != 0 {
		t.Errorf("canceled: synced=%d, errors=%d, want (0, 0)", synced, errors)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored after cancel")
	}
}

// Test_newSyncWorkerPool_success asserts that newSyncWorkerPool creates the
// requested number of workers, each with a src and dst connection, and that
// close() logs out of all of them.
func Test_newSyncWorkerPool_success(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	cfg := &config.Config{
		Src: config.Credentials{
			Server: srcSrv.ln.Addr().String(),
			User:   "user",
			Pass:   "pass",
		},
		Dst: config.Credentials{
			Server: dstSrv.ln.Addr().String(),
			User:   "user",
			Pass:   "pass",
		},
	}

	srcOpts := client.Options{UseTLS: false}
	dstOpts := client.Options{UseTLS: false}

	pool, err := newSyncWorkerPool(context.Background(), cfg, srcOpts, dstOpts, 2)
	if err != nil {
		t.Fatalf("newSyncWorkerPool: %v", err)
	}
	if len(pool.all) != 2 {
		t.Errorf("pool size=%d, want 2", len(pool.all))
	}
	pool.close()
}

// Test_newSyncWorkerPool_canceledContext asserts that a pre-canceled context
// causes newSyncWorkerPool to return an error before connecting any workers.
func Test_newSyncWorkerPool_canceledContext(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	cfg := &config.Config{
		Src: config.Credentials{Server: srcSrv.ln.Addr().String(), User: "user", Pass: "pass"},
		Dst: config.Credentials{Server: dstSrv.ln.Addr().String(), User: "user", Pass: "pass"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newSyncWorkerPool(ctx, cfg, client.Options{UseTLS: false}, client.Options{UseTLS: false}, 1)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
}

// Test_runFolderSync_verboseSuccess asserts that verbose=true causes pw.Log to
// be called for each successfully synced message without altering the (synced,
// errors) result.
func Test_runFolderSync_verboseSuccess(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	body1 := imapFullBody("vb1@x")
	srcBodies := map[string][]struct {
		body string
		uid  uint32
	}{
		"INBOX": {{uid: 1, body: body1}},
	}

	srcSrv.addConnHandler(uidFetchBodyHandler(srcSrv, []string{"INBOX"}, srcBodies, ""))
	dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, ""))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	w := &syncWorker{src: srcC, dst: dstC}

	plan := FolderSyncPlan{
		SourceFolder:            "INBOX",
		DestinationFolder:       "INBOX",
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}

	pw := progress.NewWriter(1, true)
	tr := progress.NewTracker("test", 10)

	// verbose=true exercises pw.Log("Synced %d/%d...") on success path.
	synced, errors, _ := runFolderSync(context.Background(), w, plan, new(throttleStop), tr, 0, 1, pw, true)
	if synced != 1 {
		t.Errorf("synced=%d, want 1", synced)
	}
	if errors != 0 {
		t.Errorf("errors=%d, want 0", errors)
	}
}

// Test_runFolderSync_streamError asserts that a BAD reply to UID FETCH leaves
// the planned UID undelivered, so it counts as one error.
func Test_runFolderSync_streamError(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	// src: EXAMINE returns 1 EXISTS so the plan has 1 UID; UID FETCH returns BAD.
	srcSrv.addConnHandler(badUIDFetchHandler(srcSrv))
	dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, ""))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	w := &syncWorker{src: srcC, dst: dstC}

	plan := FolderSyncPlan{
		SourceFolder:            "INBOX",
		DestinationFolder:       "INBOX",
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}

	pw := progress.NewWriter(1, true)
	tr := progress.NewTracker("test", 10)

	synced, errors, _ := runFolderSync(context.Background(), w, plan, new(throttleStop), tr, 0, 1, pw, false)
	if synced != 0 {
		t.Errorf("synced=%d, want 0", synced)
	}
	// The stream error itself counts nothing; the planned UID that it left undelivered counts one.
	if errors != 1 {
		t.Errorf("errors=%d, want 1 (the undelivered UID)", errors)
	}
	if got := srcSrv.callCount("UID SEARCH"); got != 1 {
		t.Errorf("UID SEARCH count=%d, want 1", got)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored after stream error")
	}
}

// Test_newSyncWorkerPool_srcConnectFails asserts that when the source address
// is unreachable, newSyncWorkerPool returns an error and cleans up any
// partially created workers.
func Test_newSyncWorkerPool_srcConnectFails(t *testing.T) {
	// Use a closed listener's address so the connection is immediately refused.
	tmp := newFakeServer(t)
	deadAddr := tmp.ln.Addr().String()
	tmp.close()

	dstSrv := newFakeServer(t)

	cfg := &config.Config{
		Src: config.Credentials{Server: deadAddr, User: "user", Pass: "pass"},
		Dst: config.Credentials{Server: dstSrv.ln.Addr().String(), User: "user", Pass: "pass"},
	}

	_, err := newSyncWorkerPool(context.Background(), cfg, client.Options{UseTLS: false}, client.Options{UseTLS: false}, 1)
	if err == nil {
		t.Fatal("expected error from unreachable source, got nil")
	}
}

// Test_newSyncWorkerPool_dstConnectFails asserts that when the destination
// address is unreachable, newSyncWorkerPool returns an error after cleaning up
// the already-connected source client.
func Test_newSyncWorkerPool_dstConnectFails(t *testing.T) {
	srcSrv := newFakeServer(t)

	tmp := newFakeServer(t)
	deadAddr := tmp.ln.Addr().String()
	tmp.close()

	cfg := &config.Config{
		Src: config.Credentials{Server: srcSrv.ln.Addr().String(), User: "user", Pass: "pass"},
		Dst: config.Credentials{Server: deadAddr, User: "user", Pass: "pass"},
	}

	_, err := newSyncWorkerPool(context.Background(), cfg, client.Options{UseTLS: false}, client.Options{UseTLS: false}, 1)
	if err == nil {
		t.Fatal("expected error from unreachable destination, got nil")
	}
}

// syncBuffer is a mutex-guarded io.Writer adapter used to capture go-pretty's
// render output safely: go-pretty's render goroutine and the test assertion
// goroutine both access the underlying bytes concurrently.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Test_runFolderSync_persistentLogOnlyUnderVerbose asserts that the per-message
// "Failed to append message" log line is gated on verbose=true. When verbose is
// false the line must not appear in the writer output; when true it must.
func Test_runFolderSync_persistentLogOnlyUnderVerbose(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		verbose bool
		want    bool // whether "Failed to append message" must appear
	}{
		{verbose: false, want: false},
		{verbose: true, want: true},
	} {
		t.Run(fmt.Sprintf("verbose=%v", tc.verbose), func(t *testing.T) {
			t.Parallel()

			srcSrv := newFakeServer(t)
			dstSrv := newFakeServer(t)

			body1 := imapFullBody("quota@x")
			srcBodies := map[string][]struct {
				body string
				uid  uint32
			}{
				"INBOX": {{uid: 1, body: body1}},
			}

			srcSrv.addConnHandler(uidFetchBodyHandler(srcSrv, []string{"INBOX"}, srcBodies, ""))
			// dst rejects APPEND so the per-message error path is exercised.
			dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, "NO [CANNOT] APPEND refused by policy"))

			srcC := newAppClient(t, srcSrv, "src")
			dstC := newAppClient(t, dstSrv, "dst")

			w := &syncWorker{src: srcC, dst: dstC}

			plan := FolderSyncPlan{
				SourceFolder:            "INBOX",
				DestinationFolder:       "INBOX",
				SrcUIDs:                 []uint32{1},
				NewMessages:             1,
				DestinationFolderExists: true,
			}

			sb := &syncBuffer{}
			// numTrackers=2: one for the plan, one sentinel that stays active so
			// the final renderTrackers pass (triggered by Stop) is not skipped by
			// the LengthActive()==0 short-circuit - that short-circuit would drop
			// any queued pw.Log lines before they reach the output writer.
			pw := progress.NewWriter(2, true)
			pw.SetOutputWriter(sb)
			pw.Start()

			sentinel := progress.NewTracker("sentinel", 100)
			pw.AppendTracker(sentinel)
			tr := progress.NewTracker("test", 10)
			pw.AppendTracker(tr)

			runFolderSync(context.Background(), w, plan, new(throttleStop), tr, 0, 1, pw, tc.verbose)

			// Stop signals the render goroutine; it performs one final render pass
			// (flushing any queued Log lines) then sets renderInProgress=false.
			// Poll until render exits before reading sb so the assertion sees the
			// complete output.
			pw.Stop()
			pw.WaitForRenderDone()

			got := strings.Contains(sb.String(), "Failed to append message")
			if got != tc.want {
				t.Errorf("verbose=%v: buffer contains 'Failed to append message' = %v, want %v\nbuffer: %q",
					tc.verbose, got, tc.want, sb.String())
			}
		})
	}
}

// badUIDFetchHandler returns a per-connection handler that responds to EXAMINE
// with 1 EXISTS (so StreamMessagesByUIDs attempts a UID FETCH) but returns
// BAD for the UID FETCH command, causing a non-retryable stream error.
func badUIDFetchHandler(srv *fakeServer) func(net.Conn) {
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
			case "EXAMINE", "SELECT":
				// Return 1 EXISTS so StreamMessagesByUIDs proceeds to UID FETCH.
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* 0 RECENT\r\n")
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, verb)
			case "UID":
				if strings.HasPrefix(strings.ToUpper(parts[2]), "SEARCH") {
					// The message is still on the source, so the check calls it not delivered.
					srv.mu.Lock()
					srv.counts["UID SEARCH"]++
					srv.mu.Unlock()
					_, _ = fmt.Fprintf(conn, "* SEARCH 1\r\n%s OK UID SEARCH completed\r\n", tag)
					continue
				}
				srv.mu.Lock()
				srv.counts["UID FETCH"]++
				srv.mu.Unlock()
				// BAD triggers ClassUnknown → not retryable → stream error.
				_, _ = fmt.Fprintf(conn, "%s BAD UID FETCH not permitted\r\n", tag)
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

// holdLogoutHandler answers LOGOUT only after every one of n connections has
// sent it, so logouts that run one at a time never get a reply.
func holdLogoutHandler(srv *fakeServer, n int, mu *sync.Mutex, arrived *int, release chan struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			tag, verb, arg := splitCommand(sc.Text())
			srv.recordCommand(verb, arg)
			switch verb {
			case "LIST":
				_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" INBOX\r\n%s OK LIST completed\r\n", tag)
			case "LOGOUT":
				mu.Lock()
				*arrived++
				if *arrived == n {
					close(release)
				}
				mu.Unlock()
				<-release
				_, _ = fmt.Fprintf(conn, "* BYE Logging out\r\n%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s OK %s completed\r\n", tag, verb)
			}
		}
	}
}

func TestLogoutAll_When_ServersHoldLogout_Then_ClientsLogOutTogether(t *testing.T) {
	t.Parallel()

	const n = 4
	srv := newFakeServer(t)
	var (
		mu      sync.Mutex
		arrived int
	)
	release := make(chan struct{})
	for range n {
		srv.addConnHandler(holdLogoutHandler(srv, n, &mu, &arrived, release))
	}
	clients := make([]*client.Client, 0, n+1)
	for i := range n {
		clients = append(clients, newAppClient(t, srv, fmt.Sprintf("c%d", i)))
	}
	clients = append(clients, nil)

	done := make(chan struct{})
	go func() {
		logoutAll(clients...)
		close(done)
	}()

	// One logout limit is 2s. Logouts in turn would hold the first LOGOUT for
	// that long; together they finish as soon as the server releases them.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("logoutAll still running after one logout limit: the logouts ran one at a time")
	}
	if got := srv.callCount("LOGOUT"); got != n {
		t.Errorf("LOGOUT count = %d, want %d", got, n)
	}
}

// assertAllGraphic fails the test when s holds invalid UTF-8 or a rune that a
// terminal could act on.
func assertAllGraphic(t *testing.T, what, s string) {
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

func Test_runFolderSync_When_NamesCarryControlCharacters_Then_LogsAndTrackerShowEscapes(t *testing.T) {
	t.Parallel()

	const hostileID = "<m\x1b[2J@x\u202e>"
	plan := FolderSyncPlan{
		SourceFolder:            hostileName,
		DestinationFolder:       hostileName,
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}
	tests := []struct {
		setup       func(dst *fakeServer)
		name        string
		wantTracker string
		wantLog     []string
		wantSynced  int
	}{
		{
			name:  "append refused",
			setup: func(dst *fakeServer) { dst.refuseAppendWith("NO " + hostileReply) },
			wantLog: []string{
				"Failed to append message to " + hostileShownName + ": [dst] append: " + hostileReplyShown,
			},
			wantTracker: "1/1 Synced messages 0 (errors: 1) " + hostileShownName + " → " + hostileShownName,
		},
		{
			name:  "append accepted",
			setup: func(*fakeServer) {},
			wantLog: []string{
				"Synced 1/1 to " + hostileShownName + ", processed msg id " + `<m\x1b[2J@x\u202e>`,
			},
			wantTracker: "1/1 Synced messages 1 " + hostileShownName + " → " + hostileShownName,
			wantSynced:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				hostileWireName: {{msgID: "e1@x", envelopeID: hostileID}},
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: nil})
			tt.setup(dst)
			w := &syncWorker{src: newAppClient(t, src, "src"), dst: newAppClient(t, dst, "dst")}

			sb := &syncBuffer{}
			// The sentinel stays active so the render pass Stop triggers still prints
			// the queued log lines.
			pw := progress.NewWriter(2, true)
			pw.SetOutputWriter(sb)
			pw.Start()
			pw.AppendTracker(progress.NewTracker("sentinel", 100))
			tr := progress.NewTracker("test", 10)
			pw.AppendTracker(tr)

			synced, _, _ := runFolderSync(t.Context(), w, plan, new(throttleStop), tr, 0, 1, pw, true)
			pw.Stop()
			pw.WaitForRenderDone()

			if synced != tt.wantSynced {
				t.Errorf("synced = %d, want %d", synced, tt.wantSynced)
			}
			got := sb.String()
			assertContainsAll(t, got, tt.wantLog...)
			assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned")
			if tr.Message != tt.wantTracker {
				t.Errorf("final tracker message = %q, want %q", tr.Message, tt.wantTracker)
			}
			if tt.wantSynced == 1 {
				assertIDs(t, "dst APPENDs", dst.appended(hostileWireName), "e1@x")
			}
		})
	}
}

func TestTrackerReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short ASCII reason is unchanged", in: "Quota exceeded", want: "Quota exceeded"},
		{name: "empty reason stays empty", in: "", want: ""},
		{name: "control and bidi characters are escaped", in: "\x1b[2J\r\u202e", want: `\x1b[2J\r\u202e`},
		{name: "40 bytes are kept whole", in: strings.Repeat("a", 40), want: strings.Repeat("a", 40)},
		{name: "41 bytes are cut to 37 plus an ellipsis", in: strings.Repeat("a", 41), want: strings.Repeat("a", 37) + "..."},
		{name: "long ASCII reason is cut to 37 plus an ellipsis", in: strings.Repeat("a", 90), want: strings.Repeat("a", 37) + "..."},
		{
			name: "long Cyrillic reason is cut on a rune boundary",
			in:   strings.Repeat("ж", 30),
			want: strings.Repeat("ж", 18) + "...",
		},
		{
			name: "escapes that push the length over 40 cause the cut",
			in:   strings.Repeat("\x1b", 11),
			want: strings.Repeat(`\x1b`, 9) + `\` + "...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := trackerReason(tt.in)

			if got != tt.want {
				t.Errorf("trackerReason(%q) = %q, want %q", tt.in, got, tt.want)
			}
			assertAllGraphic(t, "reason", got)
			if len(got) > maxTrackerReason {
				t.Errorf("reason is %d bytes, want at most %d", len(got), maxTrackerReason)
			}
		})
	}
}

// holdAppends parks each APPEND until the test sends on release. It announces the
// APPEND number on arrived first, so the test reads worker state mid-command.
func holdAppends(srv *fakeServer, reply string, arrived chan<- int, release <-chan struct{}) func(net.Conn) {
	return scriptedHandler(srv, []string{hostileWireName}, func(verb string, nth int) (string, bool) {
		if verb != "APPEND" {
			return "", false
		}
		arrived <- nth
		<-release
		return reply, true
	})
}

func Test_runFolderSync_When_PlanIsInFlight_Then_TrackerLinesShowEscapes(t *testing.T) {
	t.Parallel()

	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(uidFetchBodyHandler(srcSrv, []string{hostileWireName}, map[string][]struct {
		body string
		uid  uint32
	}{hostileWireName: {{uid: 1, body: imapFullBody("m1@x")}, {uid: 2, body: imapFullBody("m2@x")}}}, ""))
	dstSrv := newFakeServer(t)
	arrived, release := make(chan int, 2), make(chan struct{})
	dstSrv.addConnHandler(holdAppends(dstSrv, "NO "+hostileReply, arrived, release))
	w := &syncWorker{src: newAppClient(t, srcSrv, "src"), dst: newAppClient(t, dstSrv, "dst")}
	// Registered after the clients, so it runs first and frees a worker parked in a failed test.
	t.Cleanup(func() { close(release) })
	plan := FolderSyncPlan{
		SourceFolder:            hostileName,
		DestinationFolder:       hostileName,
		SrcUIDs:                 []uint32{1, 2},
		NewMessages:             2,
		DestinationFolderExists: true,
	}
	tr := progress.NewTracker("test", 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFolderSync(t.Context(), w, plan, new(throttleStop), tr, 0, 1, progress.NewWriter(1, true), false)
	}()
	awaitAppend := func(nth int) {
		t.Helper()
		select {
		case got := <-arrived:
			if got != nth {
				t.Fatalf("APPEND #%d arrived, want #%d", got, nth)
			}
		case <-time.After(e2eTimeout):
			t.Fatalf("APPEND #%d never arrived", nth)
		}
	}

	// The worker waits for the server in both reads, so nothing else writes the tracker.
	awaitAppend(1)
	started := tr.Message
	release <- struct{}{}
	awaitAppend(2)
	afterFailure := stripANSI(tr.Message)
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(e2eTimeout):
		t.Fatal("runFolderSync did not return after the last APPEND")
	}

	wantStarted := "1/1 " + hostileShownName + " → " + hostileShownName
	if started != wantStarted {
		t.Errorf("tracker message while the first append runs = %q, want %q", started, wantStarted)
	}
	wantFailure := "1/1 (0↑ Σ2) " + hostileShownName + " → " + hostileShownName + " (1✗ " + `[dst] append: \x1b]0;pwned\a refused\...` + ")"
	if afterFailure != wantFailure {
		t.Errorf("tracker message after the first append failed = %q, want %q", afterFailure, wantFailure)
	}
	assertAllGraphic(t, "tracker message", started)
	assertAllGraphic(t, "tracker message", afterFailure)
}

func Test_runFolderSync_When_CanceledMidStream_Then_CancelLineShowsEscapes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(scriptedHandler(srcSrv, []string{hostileWireName}, func(verb string, _ int) (string, bool) {
		if verb == "UID" {
			cancel()
			return "", true
		}
		return "", false
	}))
	w := &syncWorker{src: newAppClient(t, srcSrv, "src"), dst: newAppClient(t, newFakeServer(t), "dst")}
	plan := FolderSyncPlan{
		SourceFolder:            hostileName,
		DestinationFolder:       hostileName,
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}
	tr := progress.NewTracker("test", 10)

	synced, errs, _ := runFolderSync(ctx, w, plan, new(throttleStop), tr, 0, 1, progress.NewWriter(1, true), false)

	if synced != 0 || errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 0, 0", synced, errs)
	}
	if want := "1/1 Canceled " + hostileShownName + " → " + hostileShownName; tr.Message != want {
		t.Errorf("tracker message = %q, want %q", tr.Message, want)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored after the cancel")
	}
}

func Test_runFolderSync_When_StreamFailsWithHostileReply_Then_LogAndTrackerShowEscapes(t *testing.T) {
	t.Parallel()

	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(scriptedHandler(srcSrv, []string{hostileWireName}, func(verb string, _ int) (string, bool) {
		if verb == "UID" {
			return "BAD " + hostileReply, true
		}
		return "", false
	}))
	w := &syncWorker{src: newAppClient(t, srcSrv, "src"), dst: newAppClient(t, newFakeServer(t), "dst")}
	plan := FolderSyncPlan{
		SourceFolder:            hostileName,
		DestinationFolder:       hostileName,
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		DestinationFolderExists: true,
	}
	sb := &syncBuffer{}
	// The sentinel stays active so the render pass Stop triggers still prints the
	// queued log lines.
	pw := progress.NewWriter(2, true)
	pw.SetOutputWriter(sb)
	pw.Start()
	pw.AppendTracker(progress.NewTracker("sentinel", 100))
	tr := progress.NewTracker("test", 10)
	pw.AppendTracker(tr)

	synced, errs, _ := runFolderSync(t.Context(), w, plan, new(throttleStop), tr, 0, 1, pw, false)
	pw.Stop()
	pw.WaitForRenderDone()

	if synced != 0 || errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", synced, errs)
	}
	got := sb.String()
	assertContainsAll(t, got, "Stream error for folder "+hostileShownName+": ", hostileReplyShown)
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned")
	if want := "1/1 Synced messages 0 (errors: 1) " + hostileShownName + " → " + hostileShownName; tr.Message != want {
		t.Errorf("final tracker message = %q, want %q", tr.Message, want)
	}
}

func Test_runFolderSync_When_SourceUIDValidityChanged_Then_CountsAnErrorForEachPlannedMessageAndAppendsNothing(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x")})
	src.setUIDValidity("INBOX", 2)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	w := &syncWorker{src: newAppClient(t, src, "src"), dst: newAppClient(t, dst, "dst")}
	plan := FolderSyncPlan{
		SourceFolder:            "INBOX",
		DestinationFolder:       "INBOX",
		SrcUIDs:                 []uint32{1, 2},
		NewMessages:             2,
		SrcUIDValidity:          1,
		DestinationFolderExists: true,
	}
	sb := &syncBuffer{}
	// The sentinel stays active so the render pass Stop triggers still prints the
	// queued log lines.
	pw := progress.NewWriter(2, true)
	pw.SetOutputWriter(sb)
	pw.Start()
	pw.AppendTracker(progress.NewTracker("sentinel", 100))
	tr := progress.NewTracker("test", 10)
	pw.AppendTracker(tr)

	synced, errs, _ := runFolderSync(t.Context(), w, plan, new(throttleStop), tr, 0, 1, pw, false)
	pw.Stop()
	pw.WaitForRenderDone()

	if synced != 0 || errs != 2 {
		t.Errorf("synced, errors = %d, %d, want 0, 2: one error for each planned message", synced, errs)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored after the mismatch")
	}
	if got := dst.callCount("APPEND"); got != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", got)
	}
	for _, verb := range []string{"UID FETCH", "UID SEARCH"} {
		if got := src.callCount(verb); got != 0 {
			t.Errorf("source saw %d %s commands, want 0", got, verb)
		}
	}
	assertContainsAll(t, sb.String(),
		"Stream error for folder INBOX: ", "source folder changed after the sync plan, run sync again (UIDVALIDITY 1, now 2)")
}

func Test_newSyncWorkerPool_When_WorkerListFails_Then_ErrorNamesTheWorkerLabel(t *testing.T) {
	t.Parallel()

	serves := func(srv *fakeServer) {
		srv.addConnHandler(scriptedHandler(srv, []string{"INBOX"}, func(string, int) (string, bool) { return "", false }))
	}
	refuses := func(srv *fakeServer) {
		srv.addConnHandler(scriptedHandler(srv, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
			if verb == "LIST" {
				return "NO [CANNOT] list refused", true
			}
			return "", false
		}))
	}
	tests := []struct {
		name     string
		want     string
		srcConns []func(*fakeServer)
		dstConns []func(*fakeServer)
	}{
		{name: "first source worker", srcConns: []func(*fakeServer){refuses}, want: "worker 1 source connect: failed to load mailbox list: [alpha-w1] list mailboxes"},
		{name: "first destination worker", srcConns: []func(*fakeServer){serves}, dstConns: []func(*fakeServer){refuses}, want: "worker 1 destination connect: failed to load mailbox list: [beta-w1] list mailboxes"},
		{name: "second source worker", srcConns: []func(*fakeServer){serves, refuses}, dstConns: []func(*fakeServer){serves}, want: "worker 2 source connect: failed to load mailbox list: [alpha-w2] list mailboxes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := newFakeServer(t), newFakeServer(t)
			for _, add := range tt.srcConns {
				add(src)
			}
			for _, add := range tt.dstConns {
				add(dst)
			}
			cfg := &config.Config{
				Src: config.Credentials{Label: "alpha", Server: src.ln.Addr().String(), User: "user", Pass: "pass"},
				Dst: config.Credentials{Label: "beta", Server: dst.ln.Addr().String(), User: "user", Pass: "pass"},
			}

			_, err := newSyncWorkerPool(t.Context(), cfg, client.Options{DialTimeout: 5 * time.Second}, client.Options{DialTimeout: 5 * time.Second}, 2)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// standbyFolder holds one other message and then m1@x, so the standby UID is 2 and a fetch of the whole folder would show.
func standbyFolder(body string) []fakeMsg {
	return []fakeMsg{{msgID: "other@x"}, {msgID: "m1@x", body: body}}
}

// standbyPlan is the plan of folder A to X for the UID 1. The UID 2 of each folder stands by for it, in the order given.
func standbyPlan(folders ...string) FolderSyncPlan {
	plan := FolderSyncPlan{
		SourceFolder:            "A",
		DestinationFolder:       "X",
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		SrcUIDValidity:          1,
		DestinationFolderExists: true,
	}
	for i, folder := range folders {
		plan.standbySources = append(plan.standbySources, standbySource{folder: folder, uidValidity: 1})
		plan.standbys = append(plan.standbys, standbyCopy{planned: 1, uid: 2, source: uint32(i)})
	}
	return plan
}

type standbyRun struct {
	tr     *progress.Tracker
	log    string
	synced int
	errs   int
	gone   int
}

// runStandbyPlan runs the plan on one worker and returns what the worker counted and logged.
func runStandbyPlan(ctx context.Context, t *testing.T, src, dst *fakeServer, plan FolderSyncPlan, verbose bool) standbyRun {
	t.Helper()

	return runStopPlan(ctx, t, newStopWorker(t, src, dst), plan, new(throttleStop), verbose)
}

// runStopPlan is runStandbyPlan on a worker that the caller built, with the stop of the run.
func runStopPlan(ctx context.Context, t *testing.T, w *syncWorker, plan FolderSyncPlan, stop *throttleStop, verbose bool) standbyRun {
	t.Helper()

	sb := &syncBuffer{}
	// The sentinel stays active so the render pass Stop triggers still prints the
	// queued log lines.
	pw := progress.NewWriter(2, true)
	pw.SetOutputWriter(sb)
	pw.Start()
	pw.AppendTracker(progress.NewTracker("sentinel", 100))
	tr := progress.NewTracker("test", 10)
	pw.AppendTracker(tr)

	synced, errs, gone := runFolderSync(ctx, w, plan, stop, tr, 0, 1, pw, verbose)
	pw.Stop()
	pw.WaitForRenderDone()
	return standbyRun{tr: tr, log: sb.String(), synced: synced, errs: errs, gone: gone}
}

func Test_runFolderSync_When_PlannedMessageHasEmptyBodyAndAStandby_Then_CopiesTheStandbyOnce(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	if !got.tr.IsDone() || got.tr.IsErrored() {
		t.Errorf("tracker done = %t, errored = %t, want done and not errored", got.tr.IsDone(), got.tr.IsErrored())
	}
	if want := "1/1 Synced messages 1 A → X"; got.tr.Message != want {
		t.Errorf("final tracker message = %q, want %q", got.tr.Message, want)
	}
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1", "2")
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	assertReadOnly(t, "source", src)
}

func Test_runFolderSync_When_PlannedBodyIsMissingAndAStandby_Then_CopiesTheStandby(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
	src.omitBodyIn("A", "m1@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "1")
}

func Test_runFolderSync_When_BodylessResponseComesBeforeTheFullOne_Then_CopiesThePlannedMessageAndNoStandby(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(subjectBody("m1@x", "from B"))})
	src.omitBodyFirstIn("A", "m1@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	assertIDs(t, "dst APPENDs", dst.appended("X"), "m1@x")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{imapFullBody("m1@x")}) {
		t.Errorf("dst X bodies = %q, want the copy of A", bodies)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: the full response delivered the planned message", n)
	}
}

func Test_runFolderSync_When_FirstStandbyIsAlsoEmpty_Then_TheNextSourceIsTried(t *testing.T) {
	t.Parallel()

	bodyOfC := subjectBody("m1@x", "from C")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder(bodyOfC)})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.answerBodyIn("B", "m1@x", "{0}\r\n")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B", "C"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfC}) {
		t.Errorf("dst X bodies = %q, want only the copy of C %q", bodies, bodyOfC)
	}
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B", "C")
}

func Test_runFolderSync_When_FirstStandbyCopiesTheMessage_Then_LaterSourcesAreNotFetched(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B", "C"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
}

func Test_runFolderSync_When_EveryStandbyIsEmpty_Then_CountsOneErrorAndAppendsNothing(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBody("m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B", "C"), true)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1: one error for the planned message, not one for each try", got.synced, got.errs)
	}
	if !got.tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
	if want := "1/1 Synced messages 0 (errors: 1) A → X"; got.tr.Message != want {
		t.Errorf("final tracker message = %q, want %q", got.tr.Message, want)
	}
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
	assertContainsAll(t, got.log, "Failed to append message to X: [dst] source returned an empty body")
	assertContainsNone(t, got.log, "Copied message")
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B", "C")
}

func Test_runFolderSync_When_DestinationRefusesThePlannedMessage_Then_NoStandbyIsFetched(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder("")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppend()

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", got.synced, got.errs)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
}

func Test_runFolderSync_When_DestinationRefusesTheStandby_Then_CountsOneErrorAndTriesNoOtherSource(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppend()

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B", "C"), false)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", got.synced, got.errs)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
}

func Test_runFolderSync_When_PlanHasNoStandbys_Then_EmptyBodyCountsOneError(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan(), false)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", got.synced, got.errs)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
}

func Test_runFolderSync_When_MainStreamFailsAfterTheRefusal_Then_TheStandbyStillCopies(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 0: the stream error counts nothing, because the source delivered the planned message", got.synced, got.errs, got.gone)
	}
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: every planned message was delivered", n)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "fetch cut short")
	assertContainsNone(t, got.log, "Stream error for folder B")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
}

func Test_runFolderSync_When_StandbySourceUIDValidityChanged_Then_CountsOneErrorAndAppendsNothing(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(subjectBody("m1@x", "from B"))})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.setUIDValidity("B", 2)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", got.synced, got.errs)
	}
	assertContainsAll(t, got.log, "Stream error for folder B: ", "source folder changed after the sync plan, run sync again (UIDVALIDITY 1, now 2)")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
	if n := src.callCount("UID FETCH"); n != 1 {
		t.Errorf("source saw %d UID FETCH commands, want 1: only the main stream fetches", n)
	}
}

func Test_runFolderSync_When_ContextEndsDuringTheMainStream_Then_NoStandbyIsFetched(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(subjectBody("m1@x", "from B"))})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.onCommand("UID", cancel)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(ctx, t, src, dst, standbyPlan("B"), false)

	if got.synced != 0 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 0, 0", got.synced, got.errs)
	}
	if want := "1/1 Canceled A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
}

func Test_runFolderSync_When_ContextEndsDuringTheStandbyStream_Then_ReturnsCanceledWithoutCounting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(subjectBody("m1@x", "from B")), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	var fetches atomic.Int32
	src.onCommand("UID", func() {
		if fetches.Add(1) == 2 {
			cancel()
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(ctx, t, src, dst, standbyPlan("B", "C"), false)

	if got.synced != 0 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 0, 0", got.synced, got.errs)
	}
	if want := "1/1 Canceled A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
}

func Test_runFolderSync_When_NamesCarryControlCharacters_Then_TheStandbyLineShowsEscapes(t *testing.T) {
	t.Parallel()

	const (
		hostileID        = "<m\x1b[2J@x\u202e>"
		standbyName      = "Std\x1b[2J\u202ez"
		standbyWireName  = "Std&ABs-[2J&IC4-z"
		standbyShownName = `Std\x1b[2J\u202ez`
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		hostileWireName: {{msgID: "e1@x", envelopeID: hostileID}},
		standbyWireName: {{msgID: "other@x"}, {msgID: "e1@x", envelopeID: hostileID}},
	})
	src.answerBodyIn(hostileWireName, "e1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: nil})
	plan := FolderSyncPlan{
		SourceFolder:            hostileName,
		DestinationFolder:       hostileName,
		SrcUIDs:                 []uint32{1},
		NewMessages:             1,
		SrcUIDValidity:          1,
		DestinationFolderExists: true,
		standbySources:          []standbySource{{folder: standbyName, uidValidity: 1}},
		standbys:                []standbyCopy{{planned: 1, uid: 2, source: 0}},
	}

	got := runStandbyPlan(t.Context(), t, src, dst, plan, true)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	want := "Copied message " + `<m\x1b[2J@x\u202e>` + " to " + hostileShownName + " from " + standbyShownName + ": the copy in " + hostileShownName + " has no usable body"
	assertContainsAll(t, got.log, want)
	assertContainsNone(t, got.log, hostileName, standbyName, "\u202e", "\x1b[2J")
	for _, line := range strings.Split(got.log, "\n") {
		// The writer puts its own erase-line sequence before each log line.
		if _, text, found := strings.Cut(line, "Copied message"); found {
			assertAllGraphic(t, "log line", "Copied message"+text)
		}
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), hostileWireName, standbyWireName)
	assertIDs(t, "dst APPEND names", dst.appended(hostileWireName), "e1@x")
}

func Test_runFolderSync_When_AStandbyReplacesAPlannedUID_Then_TheVerboseLineGivesTheCause(t *testing.T) {
	t.Parallel()

	const (
		refusedEnding      = "the copy in A has no usable body"
		notDeliveredEnding = "A did not deliver the planned copy"
		goneEnding         = "A no longer holds the planned copy"
	)
	bodyOfB := subjectBody("m1@x", "from B")
	tests := []struct {
		arrange      func(src *fakeServer)
		name         string
		wantEnding   string
		wantSearches []string
	}{
		{
			name:       "the planned body is refused",
			arrange:    func(src *fakeServer) { src.answerBodyIn("A", "m1@x", "NIL") },
			wantEnding: refusedEnding,
		},
		{
			name:         "the source holds the planned message but leaves it out of the UID FETCH",
			arrange:      func(src *fakeServer) { src.omitInUIDFetch("A", "m1@x") },
			wantEnding:   notDeliveredEnding,
			wantSearches: []string{"1"},
		},
		{
			name:         "the planned message left the source after the plan",
			arrange:      func(src *fakeServer) { src.removeMessage("A", "m1@x") },
			wantEnding:   goneEnding,
			wantSearches: []string{"1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("m1@x"),
				"B": {{msgID: "other@x"}, {msgID: "m1@x", envelopeID: "<m1@x>", body: bodyOfB}},
			})
			tt.arrange(src)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

			got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), true)

			if got.synced != 1 || got.errs != 0 || got.gone != 0 {
				t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 0", got.synced, got.errs, got.gone)
			}
			if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
				t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
			}
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands, want 1", n)
			}
			assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), tt.wantSearches...)
			want := "Copied message <m1@x> to X from B: " + tt.wantEnding
			var lines []string
			for _, line := range strings.Split(got.log, "\n") {
				// The writer puts its own erase-line sequence before each log line.
				if _, text, found := strings.Cut(line, "Copied message"); found {
					lines = append(lines, strings.TrimRight("Copied message"+text, " \r"))
				}
			}
			if !slices.Equal(lines, []string{want}) {
				t.Errorf("standby lines = %q, want only %q", lines, want)
			}
			for _, ending := range []string{refusedEnding, notDeliveredEnding, goneEnding} {
				if ending != tt.wantEnding {
					assertContainsNone(t, got.log, ending)
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func Test_runFolderSync_When_PlannedCopyIsEmptyAndItsOwnFolderHoldsAnotherCopy_Then_CopiesItWithoutASecondExamine(t *testing.T) {
	t.Parallel()

	const id = "m1@x"
	bodyOfB := subjectBody(id, "from B")
	copiesOfA := []string{subjectBody(id, "a1"), subjectBody(id, "a2")}
	tests := []struct {
		name         string
		wantBody     string
		wantFrom     string
		folders      []string
		wantExamines []string
		wantFetches  []string
		emptyAtTwo   bool
	}{
		{
			name:         "the own folder holds the usable copy",
			folders:      []string{"A"},
			wantBody:     copiesOfA[1],
			wantExamines: []string{"A"},
			wantFetches:  []string{"1", "2"},
			wantFrom:     "from A: the copy in A has no usable body",
		},
		{
			name:         "the own folder comes before a later source",
			folders:      []string{"A", "B"},
			wantBody:     copiesOfA[1],
			wantExamines: []string{"A"},
			wantFetches:  []string{"1", "2"},
			wantFrom:     "from A: the copy in A has no usable body",
		},
		{
			name:         "the own copy is empty too and a later source holds a usable copy",
			folders:      []string{"A", "B"},
			emptyAtTwo:   true,
			wantBody:     bodyOfB,
			wantExamines: []string{"A", "B"},
			wantFetches:  []string{"1", "2", "2"},
			wantFrom:     "from B: the copy in A has no usable body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": sharedIDMsgs(id, copiesOfA...), "B": standbyFolder(bodyOfB)})
			src.answerBodyAt("A", 1, "NIL")
			if tt.emptyAtTwo {
				src.answerBodyAt("A", 2, "NIL")
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

			got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan(tt.folders...), true)

			if got.synced != 1 || got.errs != 0 {
				t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
			}
			if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{tt.wantBody}) {
				t.Errorf("dst X bodies = %q, want only %q", bodies, tt.wantBody)
			}
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands, want 1", n)
			}
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), tt.wantExamines...)
			assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), tt.wantFetches...)
			assertContainsAll(t, got.log, "Copied message", tt.wantFrom)
			assertReadOnly(t, "source", src)
		})
	}
}

func Test_runFolderSync_When_OneStandbySourceHoldsSeveralCopies_Then_OnlyTheFirstUsableCopyIsAppended(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		emptyUIDs  []uint32
		wantBodies []string
		wantSynced int
		wantErrs   int
	}{
		{name: "the first copy is usable", wantBodies: []string{"two"}, wantSynced: 1},
		{name: "the first copy is empty", emptyUIDs: []uint32{2}, wantBodies: []string{"three"}, wantSynced: 1},
		{name: "every copy is empty", emptyUIDs: []uint32{2, 3}, wantErrs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bodies := map[string]string{"two": subjectBody("m1@x", "two"), "three": subjectBody("m1@x", "three")}
			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("m1@x"),
				"B": {{msgID: "other@x"}, {msgID: "m1@x", body: bodies["two"]}, {msgID: "m1@x", body: bodies["three"]}},
			})
			src.answerBodyIn("A", "m1@x", "NIL")
			for _, uid := range tt.emptyUIDs {
				src.answerBodyAt("B", uid, "NIL")
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			plan := standbyPlan("B")
			plan.standbys = append(plan.standbys, standbyCopy{planned: 1, uid: 3, source: 0})

			got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

			if got.synced != tt.wantSynced || got.errs != tt.wantErrs {
				t.Errorf("synced, errors = %d, %d, want %d, %d", got.synced, got.errs, tt.wantSynced, tt.wantErrs)
			}
			var want []string
			for _, key := range tt.wantBodies {
				want = append(want, bodies[key])
			}
			if got := dst.folderBodies("X"); !slices.Equal(got, want) {
				t.Errorf("dst X bodies = %q, want %q: the message arrives once, from its first usable copy", got, want)
			}
			if n := dst.callCount("APPEND"); n != len(want) {
				t.Errorf("destination saw %d APPEND commands, want %d", n, len(want))
			}
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
		})
	}
}

func Test_runFolderSync_When_DestinationRefusesTheStandbyInVerboseMode_Then_LogsTheEscapedReason(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppendWith("NO Quota \x1b[2J exceeded")

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B", "C"), true)

	if got.synced != 0 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", got.synced, got.errs)
	}
	assertContainsAll(t, got.log, "Failed to append message to X: [dst] source returned an empty body", `Quota \x1b[2J exceeded`)
	assertContainsNone(t, got.log, "\x1b[2J")
	if n := strings.Count(got.log, "Failed to append message to X: "); n != 2 {
		t.Errorf("log holds %d failed-append lines, want 2: the refused body and the refused standby\nlog: %s", n, got.log)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
}

func Test_runFolderSync_When_ContextEndsDuringTheStandbyAppend_Then_ReturnsCanceledWithoutCounting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(subjectBody("m1@x", "from B")), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	// The handler reacts after it read the whole literal, so the cancel never lands before the continuation.
	dst := newFakeServer(t)
	dst.addConnHandler(scriptedHandler(dst, []string{"X"}, func(verb string, _ int) (string, bool) {
		if verb == "APPEND" {
			cancel()
			return "", true
		}
		return "", false
	}))

	got := runStandbyPlan(ctx, t, src, dst, standbyPlan("B", "C"), false)

	if got.synced != 0 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 0, 0: a failure after the cancel is not a message error", got.synced, got.errs)
	}
	if want := "1/1 Canceled A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1: the standby of B", n)
	}
}

func Test_runFolderSync_When_StandbyCopyHasOtherFlags_Then_TheAppendCarriesTheStandbyFlags(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	inA := fakeMsgs("m1@x")
	inA[0].flags = `\Flagged`
	inB := standbyFolder(bodyOfB)
	inB[1].flags = `\Seen $Forwarded`
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": inA, "B": inB})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("X"), `(\Seen $Forwarded)`)
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
}

func Test_runFolderSync_When_StandbyCopyHasOtherInternalDate_Then_TheAppendCarriesTheStandbyDate(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	inA := fakeMsgs("m1@x")
	inA[0].internalDate = "17-Jul-2012 09:30:00 +0200"
	inB := standbyFolder(bodyOfB)
	inB[1].internalDate = "03-Mar-2015 10:11:12 -0500"
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": inA, "B": inB})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 {
		t.Errorf("synced, errors = %d, %d, want 1, 0", got.synced, got.errs)
	}
	assertIDs(t, "dst APPEND dates", dst.appendedDates("X"), " 3-Mar-2015 10:11:12 -0500")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
}

// copyPlan is the plan of folder A to X for the UIDs 1 to n.
func copyPlan(n int) FolderSyncPlan {
	plan := FolderSyncPlan{
		SourceFolder:            "A",
		DestinationFolder:       "X",
		NewMessages:             n,
		SrcUIDValidity:          1,
		DestinationFolderExists: true,
	}
	for uid := 1; uid <= n; uid++ {
		plan.SrcUIDs = append(plan.SrcUIDs, uint32(uid))
	}
	return plan
}

func Test_runFolderSync_When_PlannedUIDsEndDifferently_Then_EachIsSyncedFailedOrGoneOnce(t *testing.T) {
	t.Parallel()

	const (
		goneLine     = "Source folder A no longer holds "
		searchRefuse = "NO [CANNOT] search refused"
	)
	bodyOfB := subjectBody("m1@x", "from B")
	manyIDs := make([]string, clientBatchSize+1)
	firstBatchBodies := make([]string, clientBatchSize)
	for i := range manyIDs {
		manyIDs[i] = fmt.Sprintf("n%d@x", i+1)
		if i < clientBatchSize {
			firstBatchBodies[i] = imapFullBody(manyIDs[i])
		}
	}
	tests := []struct {
		arrange      func(src *fakeServer)
		folders      map[string][]fakeMsg
		name         string
		wantBodies   []string
		wantSearches []string
		wantLog      []string
		plan         FolderSyncPlan
		wantSynced   int
		wantErrs     int
		wantGone     int
		wantGoneLine bool
	}{
		{
			name:       "every message is delivered",
			folders:    map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")},
			plan:       copyPlan(2),
			wantSynced: 2,
			wantBodies: []string{imapFullBody("m1@x"), imapFullBody("m2@x")},
		},
		{
			name:       "a response without a body comes before the full one",
			folders:    map[string][]fakeMsg{"A": fakeMsgs("m1@x")},
			arrange:    func(src *fakeServer) { src.omitBodyFirstIn("A", "m1@x") },
			plan:       copyPlan(1),
			wantSynced: 1,
			wantBodies: []string{imapFullBody("m1@x")},
		},
		{
			name:         "the source leaves a held message out of the UID FETCH",
			folders:      map[string][]fakeMsg{"A": fakeMsgs("m1@x")},
			arrange:      func(src *fakeServer) { src.omitInUIDFetch("A", "m1@x") },
			plan:         copyPlan(1),
			wantErrs:     1,
			wantSearches: []string{"1"},
		},
		{
			name:         "the response of a held message has no BODY[]",
			folders:      map[string][]fakeMsg{"A": fakeMsgs("m1@x")},
			arrange:      func(src *fakeServer) { src.omitBodyIn("A", "m1@x") },
			plan:         copyPlan(1),
			wantErrs:     1,
			wantSearches: []string{"1"},
		},
		{
			name:         "the message left the source after the plan",
			folders:      map[string][]fakeMsg{"A": fakeMsgs("m1@x")},
			arrange:      func(src *fakeServer) { src.removeMessage("A", "m1@x") },
			plan:         copyPlan(1),
			wantGone:     1,
			wantSearches: []string{"1"},
			wantGoneLine: true,
		},
		{
			name:         "a held message that the source leaves out has a usable standby",
			folders:      map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)},
			arrange:      func(src *fakeServer) { src.omitInUIDFetch("A", "m1@x") },
			plan:         standbyPlan("B"),
			wantSynced:   1,
			wantBodies:   []string{bodyOfB},
			wantSearches: []string{"1"},
		},
		{
			name:         "a gone message has a usable standby",
			folders:      map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)},
			arrange:      func(src *fakeServer) { src.removeMessage("A", "m1@x") },
			plan:         standbyPlan("B"),
			wantSynced:   1,
			wantBodies:   []string{bodyOfB},
			wantSearches: []string{"1"},
		},
		{
			name:    "a gone message has an empty standby",
			folders: map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder("")},
			arrange: func(src *fakeServer) {
				src.removeMessage("A", "m1@x")
				src.answerBodyIn("B", "m1@x", "NIL")
			},
			plan:         standbyPlan("B"),
			wantGone:     1,
			wantSearches: []string{"1"},
			wantGoneLine: true,
		},
		{
			name:    "a held message that the source leaves out has an empty standby",
			folders: map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder("")},
			arrange: func(src *fakeServer) {
				src.omitInUIDFetch("A", "m1@x")
				src.answerBodyIn("B", "m1@x", "NIL")
			},
			plan:         standbyPlan("B"),
			wantErrs:     1,
			wantSearches: []string{"1"},
		},
		{
			name:    "the source refuses the check",
			folders: map[string][]fakeMsg{"A": fakeMsgs("m1@x")},
			arrange: func(src *fakeServer) {
				src.omitInUIDFetch("A", "m1@x")
				src.refuseUIDSearchWith("A", searchRefuse)
			},
			plan:         copyPlan(1),
			wantErrs:     1,
			wantSearches: []string{"1"},
		},
		{
			name:     "the UIDVALIDITY changed before the copy",
			folders:  map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")},
			arrange:  func(src *fakeServer) { src.setUIDValidity("A", 2) },
			plan:     copyPlan(2),
			wantErrs: 2,
		},
		{
			name:       "the source ends the UID FETCH with a tagged NO after every body",
			folders:    map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")},
			arrange:    func(src *fakeServer) { src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short") },
			plan:       copyPlan(2),
			wantSynced: 2,
			wantBodies: []string{imapFullBody("m1@x"), imapFullBody("m2@x")},
			wantLog:    []string{"Stream error for folder A: ", "fetch cut short"},
		},
		{
			name:    "one message is delivered, one is gone and one is held",
			folders: map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x", "m3@x")},
			arrange: func(src *fakeServer) {
				src.removeMessage("A", "m2@x")
				src.omitInUIDFetch("A", "m3@x")
			},
			plan:         copyPlan(3),
			wantSynced:   1,
			wantErrs:     1,
			wantGone:     1,
			wantBodies:   []string{imapFullBody("m1@x")},
			wantSearches: []string{"2:3"},
			wantGoneLine: true,
		},
		{
			name:    "the second batch fails before it delivers anything",
			folders: map[string][]fakeMsg{"A": fakeMsgs(manyIDs...)},
			arrange: func(src *fakeServer) {
				var fetches atomic.Int32
				src.onCommand("UID", func() {
					if fetches.Add(1) == 2 {
						src.omitInUIDFetch("A", manyIDs[clientBatchSize])
						src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
					}
				})
			},
			plan:         copyPlan(clientBatchSize + 1),
			wantSynced:   clientBatchSize,
			wantErrs:     1,
			wantBodies:   firstBatchBodies,
			wantSearches: []string{strconv.Itoa(clientBatchSize + 1)},
			wantLog:      []string{"Stream error for folder A: ", "fetch cut short"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.folders)
			if tt.arrange != nil {
				tt.arrange(src)
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

			got := runStandbyPlan(t.Context(), t, src, dst, tt.plan, false)

			if got.synced != tt.wantSynced || got.errs != tt.wantErrs || got.gone != tt.wantGone {
				t.Errorf("synced, errors, gone = %d, %d, %d, want %d, %d, %d", got.synced, got.errs, got.gone, tt.wantSynced, tt.wantErrs, tt.wantGone)
			}
			if sum := got.synced + got.errs + got.gone; sum != tt.plan.NewMessages {
				t.Errorf("synced + errors + gone = %d, want the %d planned messages", sum, tt.plan.NewMessages)
			}
			if !got.tr.IsDone() || got.tr.IsErrored() != (tt.wantErrs > 0) {
				t.Errorf("tracker done = %t, errored = %t, want done and errored = %t", got.tr.IsDone(), got.tr.IsErrored(), tt.wantErrs > 0)
			}
			if bodies := dst.folderBodies("X"); !slices.Equal(bodies, tt.wantBodies) {
				t.Errorf("dst X holds %d bodies, want %d, and the same ones in the same order", len(bodies), len(tt.wantBodies))
			}
			if n := dst.callCount("APPEND"); n != tt.wantSynced {
				t.Errorf("destination saw %d APPEND commands, want %d", n, tt.wantSynced)
			}
			assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), tt.wantSearches...)
			assertContainsAll(t, got.log, tt.wantLog...)
			wantLines := 0
			if tt.wantGoneLine {
				wantLines = 1
			}
			if n := strings.Count(got.log, goneLine); n != wantLines {
				t.Errorf("log holds the gone line %d times, want %d\nlog: %s", n, wantLines, got.log)
			}
			if tt.wantGoneLine {
				assertContainsAll(t, got.log, fmt.Sprintf("Source folder A no longer holds %d of the %d planned messages for X, so they are not copied", tt.wantGone, tt.plan.NewMessages))
			}
			assertReadOnly(t, "source", src)
		})
	}
}

// clientBatchSize is the largest UID set that the client sends in one command.
const clientBatchSize = 500

func Test_runFolderSync_When_ContextEndsDuringTheCheck_Then_ReturnsCanceledWithoutGoneLine(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x")})
	src.omitInUIDFetch("A", "m1@x")
	var commands atomic.Int32
	// The first UID command is the FETCH that leaves the message out, the second is the check.
	src.onCommand("UID", func() {
		if commands.Add(1) == 2 {
			cancel()
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(ctx, t, src, dst, copyPlan(1), false)

	if got.synced != 0 || got.errs != 0 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 0, 0", got.synced, got.errs, got.gone)
	}
	if want := "1/1 Canceled A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
	assertContainsNone(t, got.log, "no longer holds")
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "1")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
}

func Test_runFolderSync_When_GoneNamesCarryControlCharacters_Then_TheGoneLineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: fakeMsgs("m1@x", "m2@x")})
	src.removeMessage(hostileWireName, "m2@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: nil})
	plan := copyPlan(2)
	plan.SourceFolder, plan.DestinationFolder = hostileName, hostileName

	got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

	if got.synced != 1 || got.errs != 0 || got.gone != 1 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 1", got.synced, got.errs, got.gone)
	}
	want := "Source folder " + hostileShownName + " no longer holds 1 of the 2 planned messages for " + hostileShownName + ", so they are not copied"
	assertContainsAll(t, got.log, want)
	assertContainsNone(t, got.log, hostileName, "\u202e", "\x1b[2J")
	for _, line := range strings.Split(got.log, "\n") {
		// The writer puts its own erase-line sequence before each log line.
		if _, text, found := strings.Cut(line, "Source folder"); found {
			assertAllGraphic(t, "log line", "Source folder"+text)
		}
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), hostileWireName)
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2")
	assertIDs(t, "dst APPENDs", dst.appended(hostileWireName), "m1@x")
}

// gmailUnreadableReply is how Gmail ends a UID FETCH that holds a message it cannot read, after it sent the others.
const gmailUnreadableReply = "NO Some messages could not be FETCHed (Failure)"

// throttleReply is how Gmail ends a command once the account is over its limits.
const throttleReply = "NO [OVERQUOTA] Account exceeded command or bandwidth limits"

// numberedIDs returns the Message-Ids n1@x to nN@x.
func numberedIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d@x", i+1)
	}
	return ids
}

// bodiesOf returns the full body of each Message-Id, in order, leaving out the ones in skip.
func bodiesOf(ids []string, skip ...string) []string {
	bodies := make([]string, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(skip, id) {
			bodies = append(bodies, imapFullBody(id))
		}
	}
	return bodies
}

func Test_runFolderSync_When_ABatchHoldsAnUnreadableMessage_Then_LaterBatchesAreCopied(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...)})
	src.omitInUIDFetch("A", ids[2])
	src.endUIDFetchThatOmitsWith("A", gmailUnreadableReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := copyPlan(len(ids))

	got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

	if got.synced != 1000 || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1000, 1, 0", got.synced, got.errs, got.gone)
	}
	if sum := got.synced + got.errs + got.gone; sum != plan.NewMessages {
		t.Errorf("synced + errors + gone = %d, want the %d planned messages", sum, plan.NewMessages)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500", "501:1000", "1001")
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "3")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, bodiesOf(ids, ids[2])) {
		t.Errorf("dst X holds %d bodies, want the 1000 readable ones, in order", len(bodies))
	}
	if n := strings.Count(got.log, "Stream error for folder A: "); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Some messages could not be FETCHed (Failure)")
	assertContainsNone(t, got.log, "batches failed")
	if !got.tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
	assertReadOnly(t, "source", src)
}

func Test_runFolderSync_When_SeveralBatchesFail_Then_OneLineCountsThem(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...)})
	src.omitInUIDFetch("A", ids[1])
	src.omitInUIDFetch("A", ids[599])
	src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := copyPlan(len(ids))

	got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

	if got.synced != 999 || got.errs != 2 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 999, 2, 0", got.synced, got.errs, got.gone)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500", "501:1000", "1001")
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2,600")
	if n := strings.Count(got.log, "Stream error for folder"); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "fetch cut short (3 batches failed)")
	if !got.tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
}

func Test_runFolderSync_When_SourceFolderCannotBeOpened_Then_EveryPlannedMessageIsAnError(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...)})
	src.refuseOpenWith("A", "NO [NONEXISTENT] Unknown Mailbox")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := copyPlan(len(ids))
	stop := new(throttleStop)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 1001 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1001, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	if stop.get() != nil {
		t.Errorf("stop = %v, want none: a refused folder is no throttle", stop.get())
	}
	if n := src.callCount("EXAMINE"); n != 2 {
		t.Errorf("source saw %d EXAMINE commands, want 2: one for the stream and one for the check", n)
	}
	for _, verb := range []string{"UID FETCH", "UID SEARCH"} {
		if n := src.callCount(verb); n != 0 {
			t.Errorf("source saw %d %s commands, want 0", n, verb)
		}
	}
	if n := strings.Count(got.log, "Stream error for folder"); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "cannot select folder A", "Unknown Mailbox")
	assertContainsNone(t, got.log, "batches failed")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
	if !got.tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
}

func Test_runFolderSync_When_SourceFolderCannotBeOpenedAndAStandbyExists_Then_TheStandbyOfAnotherFolderCopiesTheMessage(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
	src.refuseOpenWith("A", "NO [NONEXISTENT] Unknown Mailbox")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 0", got.synced, got.errs, got.gone)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "A", "B")
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "2")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
	if n := strings.Count(got.log, "Stream error for folder A: "); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsNone(t, got.log, "batches failed")
	if !got.tr.IsDone() || got.tr.IsErrored() {
		t.Errorf("tracker done = %t, errored = %t, want done and not errored", got.tr.IsDone(), got.tr.IsErrored())
	}
}

func Test_runFolderSync_When_SourceUIDValidityChangedAndAStandbyExists_Then_TheStandbyOfAnotherFolderCopiesTheMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		wantStreamEnd  string
		plan           FolderSyncPlan
		wantBatchCount bool
	}{
		{
			name:          "the standby is in another folder",
			plan:          standbyPlan("B"),
			wantStreamEnd: "(UIDVALIDITY 1, now 2)",
		},
		{
			name:           "the plan's own folder is the first standby source",
			plan:           standbyPlan("A", "B"),
			wantStreamEnd:  "(UIDVALIDITY 1, now 2) (2 batches failed)",
			wantBatchCount: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bodyOfB := subjectBody("m1@x", "from B")
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
			src.setUIDValidity("A", 2)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

			got := runStandbyPlan(t.Context(), t, src, dst, tt.plan, false)

			if got.synced != 1 || got.errs != 0 || got.gone != 0 {
				t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 0", got.synced, got.errs, got.gone)
			}
			if sum := got.synced + got.errs + got.gone; sum != tt.plan.NewMessages {
				t.Errorf("synced + errors + gone = %d, want the %d planned messages", sum, tt.plan.NewMessages)
			}
			if n := src.callCount("UID SEARCH"); n != 0 {
				t.Errorf("source saw %d UID SEARCH commands, want 0", n)
			}
			if n := src.callCount("UID FETCH"); n != 1 {
				t.Errorf("source saw %d UID FETCH commands, want 1: only the standby in B", n)
			}
			assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "2")
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
			if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
				t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
			}
			if n := strings.Count(got.log, "Stream error for folder A: "); n != 1 {
				t.Errorf("log holds the stream line of A %d times, want 1\nlog: %s", n, got.log)
			}
			assertContainsAll(t, got.log, tt.wantStreamEnd)
			assertContainsNone(t, got.log, "Stream error for folder B")
			if !tt.wantBatchCount {
				assertContainsNone(t, got.log, "batches failed")
			}
			if !got.tr.IsDone() || got.tr.IsErrored() {
				t.Errorf("tracker done = %t, errored = %t, want done and not errored", got.tr.IsDone(), got.tr.IsErrored())
			}
		})
	}
}

func Test_runFolderSync_When_SourceThrottlesTheFirstBatch_Then_EveryPlannedMessageIsAnErrorWithoutACheck(t *testing.T) {
	t.Parallel()

	src := newFakeServer(t)
	src.addConnHandler(scriptedHandler(src, []string{"A"}, func(verb string, _ int) (string, bool) {
		if verb == "UID" {
			return throttleReply, true
		}
		return "", false
	}))
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := copyPlan(2*clientBatchSize + 1)
	plan.SrcUIDValidity = 0

	got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

	if got.synced != 0 || got.errs != 1001 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1001, 0", got.synced, got.errs, got.gone)
	}
	if n := src.callCount("UID FETCH"); n != 1 {
		t.Errorf("source saw %d UID FETCH commands, want 1: a throttled reply ends the stream", n)
	}
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: a throttled source gets no check", n)
	}
	if n := strings.Count(got.log, "Stream error for folder"); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "exceeded command or bandwidth limits")
	assertContainsNone(t, got.log, "batches failed")
}

func Test_runFolderSync_When_AThrottledStopFollowsARefusedBody_Then_NoStandbyIsFetchedAndBothAreErrors(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(clientBatchSize + 1)
	ids[0] = "m1@x"
	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...), "B": standbyFolder(bodyOfB)})
	src.answerBodyIn("A", "m1@x", "NIL")
	var fetches atomic.Int32
	src.onCommand("UID", func() {
		if fetches.Add(1) == 2 {
			src.omitInUIDFetch("A", ids[clientBatchSize])
			src.endUIDFetchThatOmitsWith("A", throttleReply)
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := copyPlan(len(ids))
	plan.standbySources = []standbySource{{folder: "B", uidValidity: 1}}
	plan.standbys = []standbyCopy{{planned: 1, uid: 2, source: 0}}
	stop := new(throttleStop)

	got := runStopPlan(t.Context(), t, &syncWorker{src: newAppClient(t, src, "src"), dst: newAppClient(t, dst, "dst")}, plan, stop, false)

	if got.synced != clientBatchSize-1 || got.errs != 2 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want %d, 2, 0: the refused body and the undelivered UID are errors", got.synced, got.errs, got.gone, clientBatchSize-1)
	}
	assertAccounted(t, got, plan)
	bodies := dst.folderBodies("X")
	if len(bodies) != clientBatchSize-1 || slices.Contains(bodies, bodyOfB) {
		t.Errorf("dst X holds %d bodies, want %d, none from B", len(bodies), clientBatchSize-1)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0", n)
	}
	cause := stop.get()
	if cause == nil || cause.side != sideSource || !client.IsThrottled(cause.err) {
		t.Errorf("stop cause = %+v, want a throttled reply from the source", cause)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "exceeded command or bandwidth limits")
}

func Test_runFolderSync_When_MainAndStandbyStreamsFail_Then_OneLineNamesTheFirst(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
	src.endUIDFetchWith("B", "NO [CANNOT] standby cut short")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(t.Context(), t, src, dst, standbyPlan("B"), false)

	if got.synced != 1 || got.errs != 0 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 0, 0", got.synced, got.errs, got.gone)
	}
	if n := strings.Count(got.log, "Stream error for folder"); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "fetch cut short (2 batches failed)")
	assertContainsNone(t, got.log, "standby cut short", "Stream error for folder B")
	if bodies := dst.folderBodies("X"); !slices.Equal(bodies, []string{bodyOfB}) {
		t.Errorf("dst X bodies = %q, want only the copy of B %q", bodies, bodyOfB)
	}
}

func Test_runFolderSync_When_ContextEndsAfterARefusedBatch_Then_PrintsNoStreamLine(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...)})
	src.endUIDFetchWith("A", "NO [CANNOT] fetch refused")
	var fetches atomic.Int32
	src.onCommand("UID", func() {
		if fetches.Add(1) == 2 {
			cancel()
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})

	got := runStandbyPlan(ctx, t, src, dst, copyPlan(len(ids)), false)

	if got.errs != 0 || got.gone != 0 {
		t.Errorf("errors, gone = %d, %d, want 0, 0", got.errs, got.gone)
	}
	if want := "1/1 Canceled A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
	assertContainsNone(t, got.log, "Stream error", "no longer holds")
	if n := src.callCount("UID FETCH"); n != 2 {
		t.Errorf("source saw %d UID FETCH commands, want 2: no batch follows the cancel", n)
	}
}

// totalCommands counts every command the server has seen, whatever its verb.
func totalCommands(srv *fakeServer) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	total := 0
	for _, n := range srv.counts {
		total += n
	}
	return total
}

// assertAccounted checks that each planned message ended as synced, an error or gone.
func assertAccounted(t *testing.T, got standbyRun, plan FolderSyncPlan) {
	t.Helper()

	if sum := got.synced + got.errs + got.gone; sum != plan.NewMessages {
		t.Errorf("synced + errors + gone = %d + %d + %d = %d, want the %d planned messages", got.synced, got.errs, got.gone, sum, plan.NewMessages)
	}
}

// newStopWorker also checks, when the test ends, that the worker only read from the source.
func newStopWorker(t *testing.T, src, dst *fakeServer) *syncWorker {
	t.Helper()

	t.Cleanup(func() {
		if src.store != nil {
			assertReadOnly(t, "source", src)
		} else {
			assertNoWriteCommands(t, "source", src)
		}
	})
	return &syncWorker{src: newAppClient(t, src, "src"), dst: newAppClient(t, dst, "dst")}
}

func Test_runFolderSync_When_RunIsAlreadyStopped_Then_SendsNoCommand(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	w := newStopWorker(t, src, dst)
	stop := new(throttleStop)
	stop.record(sideDestination, errors.New("Quota exceeded"))
	srcBefore, dstBefore := totalCommands(src), totalCommands(dst)
	plan := copyPlan(2)

	got := runStopPlan(t.Context(), t, w, plan, stop, false)

	if got.synced != 0 || got.errs != 2 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 2, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	if n := totalCommands(src) - srcBefore; n != 0 {
		t.Errorf("source saw %d new commands, want none", n)
	}
	if n := totalCommands(dst) - dstBefore; n != 0 {
		t.Errorf("destination saw %d new commands, want none", n)
	}
	if want := "1/1 Stopped A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
}

func Test_runFolderSync_When_AnotherWorkerStopsTheRunDuringAnAppend_Then_NoFurtherAppend(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(5)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.onCommand("APPEND", func() { stop.record(sideSource, errors.New("another worker was throttled")) })
	plan := copyPlan(5)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 1 || got.errs != 4 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 1, 4, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	if n := src.callCount("UID FETCH"); n != 1 {
		t.Errorf("source saw %d UID FETCH commands, want 1", n)
	}
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: a stopped plan gets no check", n)
	}
	if cause := stop.get(); cause == nil || cause.side != sideSource {
		t.Errorf("stop cause = %+v, want the first recorded one, from the source", cause)
	}
	assertContainsNone(t, got.log, "Stream error")
}

func Test_runFolderSync_When_TheRunStopsAtTheLastMessageOfABatch_Then_NoNextBatch(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(clientBatchSize + 1)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.onCommand("APPEND", func() {
		if dst.callCount("APPEND") == clientBatchSize {
			stop.record(sideSource, errors.New("another worker was throttled"))
		}
	})
	plan := copyPlan(clientBatchSize + 1)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != clientBatchSize || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want %d, 1, 0", got.synced, got.errs, got.gone, clientBatchSize)
	}
	assertAccounted(t, got, plan)
	if n := dst.callCount("APPEND"); n != clientBatchSize {
		t.Errorf("destination saw %d APPEND commands, want %d", n, clientBatchSize)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500")
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0", n)
	}
}

func Test_runFolderSync_When_DestinationThrottles_Then_RecordsTheDestinationAndStops(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(5)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppendWith(gmailThrottleReply)
	plan := copyPlan(5)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, true)

	if got.synced != 0 || got.errs != 5 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 5, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	cause := stop.get()
	if cause == nil || cause.side != sideDestination || !strings.Contains(cause.err.Error(), "Account exceeded command or bandwidth limits. (Failure)") {
		t.Fatalf("stop cause = %+v, want the destination and the reply", cause)
	}
	if !client.IsThrottled(cause.err) {
		t.Errorf("IsThrottled(%v) = false, want true", cause.err)
	}
	assertContainsAll(t, got.log, "Failed to append message to X: ")
	assertContainsNone(t, got.log, "Stream error")
	if !got.tr.IsErrored() {
		t.Error("tracker not marked as errored")
	}
}

func Test_runFolderSync_When_ExistenceCheckIsThrottled_Then_RecordsTheSourceAndSkipsStandbys(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder("")})
	src.omitInUIDFetch("A", "m1@x")
	src.refuseUIDSearchWith("A", throttleReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := standbyPlan("B")

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	cause := stop.get()
	if cause == nil || cause.side != sideSource || !client.IsThrottled(cause.err) {
		t.Errorf("stop cause = %+v, want the source and a throttled reply", cause)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
}

func Test_runFolderSync_When_StandbyAppendFindsTheDestinationGone_Then_NoOtherSourceIsTried(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.dropAtAppendAndRefuseLogin(1)
	plan := standbyPlan("B", "C")

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	if stop.stopped() {
		t.Errorf("stop cause = %+v, want none: a permanent give-up is no throttle", stop.get())
	}
	assertContainsNone(t, got.log, "Stream error")
}

func Test_runFolderSync_When_StandbySourceThrottles_Then_RecordsTheSourceAndTriesNoOtherSource(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
	src.answerBodyIn("A", "m1@x", "NIL")
	src.endUIDFetchWith("B", throttleReply)
	src.omitInUIDFetch("B", "m1@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	plan := standbyPlan("B", "C")

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	cause := stop.get()
	if cause == nil || cause.side != sideSource || !client.IsThrottled(cause.err) {
		t.Errorf("stop cause = %+v, want the source and a throttled reply", cause)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
	assertContainsAll(t, got.log, "Stream error for folder B: ", "exceeded command or bandwidth limits")
}

func Test_runFolderSync_When_TheRunStopsDuringAStandbyStream_Then_NoLaterSourceIsTried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		omit bool
	}{
		{name: "the standby stream delivers no message", omit: true},
		{name: "the standby stream delivers its message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stop := new(throttleStop)
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(""), "C": standbyFolder("")})
			src.answerBodyIn("A", "m1@x", "NIL")
			if tt.omit {
				src.omitInUIDFetch("B", "m1@x")
			}
			// The first UID command is the FETCH of A, the second is the standby FETCH of B.
			var commands atomic.Int32
			src.onCommand("UID", func() {
				if commands.Add(1) == 2 {
					stop.record(sideDestination, errors.New("another worker was throttled"))
				}
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			plan := standbyPlan("B", "C")

			got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

			if got.synced != 0 || got.errs != 1 || got.gone != 0 {
				t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 1, 0", got.synced, got.errs, got.gone)
			}
			assertAccounted(t, got, plan)
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("destination saw %d APPEND commands, want 0", n)
			}
		})
	}
}

func Test_runFolderSync_When_AppendFailsAfterAnotherWorkerStoppedTheRun_Then_ThePlanEndsAtOnce(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(5)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppendWith("NO [CANNOT] APPEND refused by policy")
	dst.onCommand("APPEND", func() { stop.record(sideSource, errors.New("another worker was throttled")) })
	plan := copyPlan(5)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 5 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 5, 0", got.synced, got.errs, got.gone)
	}
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1: the stop ends the plan after the failed one", n)
	}
	assertAccounted(t, got, plan)
}

func Test_runFolderSync_When_DestinationGaveUpBeforeThePlan_Then_NoSourceCommand(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(3)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	w := newStopWorker(t, src, dst)
	// A BYE in reply to EXAMINE ends the session, and the refused LOGIN is a permanent give-up.
	dst.dropOnceOnOpen("X")
	dst.refuseLogin()
	if _, err := w.dst.FetchMessageCopies(t.Context(), "X"); err == nil {
		t.Fatal("FetchMessageCopies error = nil, want the give-up of the destination")
	}
	srcBefore, dstBefore := totalCommands(src), totalCommands(dst)
	plan := copyPlan(3)

	got := runStopPlan(t.Context(), t, w, plan, stop, false)

	if got.synced != 0 || got.errs != 3 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 3, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	if n := totalCommands(src) - srcBefore; n != 0 {
		t.Errorf("source saw %d new commands, want none", n)
	}
	if n := totalCommands(dst) - dstBefore; n != 0 {
		t.Errorf("destination saw %d new commands, want none: the gate refuses without a dial or a LOGIN", n)
	}
	if stop.stopped() {
		t.Errorf("stop cause = %+v, want none: a permanent give-up is no throttle", stop.get())
	}
	if want := "1/1 Synced messages 0 (errors: 3) A → X"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored", got.tr.Message, got.tr.IsErrored(), want)
	}
}

func Test_runFolderSync_When_AppendIsRefusedOnALiveSession_Then_ThePlanGoesOn(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(5)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.refuseAppendWith("NO [CANNOT] APPEND refused by policy")
	plan := copyPlan(5)

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 5 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 5, 0", got.synced, got.errs, got.gone)
	}
	if n := dst.callCount("APPEND"); n != 5 {
		t.Errorf("destination saw %d APPEND commands, want 5: every message is tried", n)
	}
	if stop.stopped() {
		t.Errorf("stop cause = %+v, want none: a refusal on a live session is no stop", stop.get())
	}
	if n := dst.callCount("LOGIN"); n != 1 {
		t.Errorf("destination saw %d LOGIN commands, want 1", n)
	}
}

func Test_streamFailure(t *testing.T) {
	t.Parallel()

	throttled := fmt.Errorf("[src] body fetch: %w", errors.New("Account exceeded command or bandwidth limits"))
	tests := []struct {
		err          error
		wantFirst    error
		wantThrottle error
		name         string
		wantBatches  int
	}{
		{
			name:         "throttled stop with a network failure first",
			err:          &client.StreamError{First: io.EOF, Stop: throttled, FailedBatches: 2},
			wantBatches:  2,
			wantThrottle: throttled,
			wantFirst:    io.EOF,
		},
		{
			name:         "throttled stop under a wrapper",
			err:          fmt.Errorf("stream: %w", &client.StreamError{First: throttled, Stop: throttled, FailedBatches: 1}),
			wantBatches:  1,
			wantThrottle: throttled,
			wantFirst:    throttled,
		},
		{
			name:        "UIDVALIDITY stop",
			err:         &client.StreamError{First: client.ErrUIDValidityChanged, Stop: client.ErrUIDValidityChanged, FailedBatches: 1},
			wantBatches: 1,
			wantFirst:   client.ErrUIDValidityChanged,
		},
		{
			name:        "failed batches without a stop",
			err:         &client.StreamError{First: io.EOF, FailedBatches: 3},
			wantBatches: 3,
			wantFirst:   io.EOF,
		},
		{
			name:        "other error counts as one batch",
			err:         io.ErrUnexpectedEOF,
			wantBatches: 1,
			wantFirst:   io.ErrUnexpectedEOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			batches, throttle, first := streamFailure(tt.err)

			if batches != tt.wantBatches || !errors.Is(throttle, tt.wantThrottle) || !errors.Is(first, tt.wantFirst) {
				t.Errorf("streamFailure = %d, %v, %v, want %d, %v, %v", batches, throttle, first, tt.wantBatches, tt.wantThrottle, tt.wantFirst)
			}
		})
	}
}

func Test_streamFailure_When_StopIsThrottledAndFirstIsANetworkError_Then_ReportsTheThrottle(t *testing.T) {
	t.Parallel()

	throttled := errors.New("Account exceeded command or bandwidth limits")
	err := &client.StreamError{First: io.EOF, Stop: throttled, FailedBatches: 2}

	if client.IsThrottled(err) {
		t.Fatal("IsThrottled(whole error) = true, want false: the network failure hides the stop")
	}
	if _, throttle, _ := streamFailure(err); !errors.Is(throttle, throttled) {
		t.Errorf("streamFailure throttle = %v, want %v", throttle, throttled)
	}
}

func Test_throttleStop_When_SeveralRepliesAreRecorded_Then_TheFirstOneWins(t *testing.T) {
	t.Parallel()

	var stop throttleStop
	if stop.stopped() || stop.get() != nil || stop.halt() != nil {
		t.Fatalf("a new stop: stopped = %t, cause = %+v, halt = %v, want a run that goes on", stop.stopped(), stop.get(), stop.halt())
	}
	if stop.recordThrottled(sideSource, errors.New("[CANNOT] refused by policy")) || stop.stopped() {
		t.Fatalf("a reply that is no throttle stopped the run: %+v", stop.get())
	}

	first := errors.New("Account exceeded command or bandwidth limits")
	if !stop.recordThrottled(sideSource, first) {
		t.Fatal("recordThrottled = false for a throttled reply, want true")
	}
	stop.record(sideDestination, errors.New("later destination reply"))
	if !stop.recordThrottled(sideDestination, errors.New("Quota exceeded (mailbox for user is full)")) {
		t.Error("recordThrottled = false for a later throttled reply, want true: it is a throttle, even if it does not win")
	}

	cause := stop.get()
	if cause == nil || cause.side != sideSource || !errors.Is(cause.err, first) {
		t.Errorf("cause = %+v, want the source and the first reply", cause)
	}
	if err := stop.halt(); !errors.Is(err, errPlanStopped) {
		t.Errorf("halt = %v, want errPlanStopped", err)
	}
}

func Test_throttleStop_When_WorkersRecordAtTheSameTime_Then_OneCauseStaysForEveryReader(t *testing.T) {
	t.Parallel()

	var stop throttleStop
	replies := make([]error, 16)
	seen := make([]*throttleCause, len(replies))
	var wg sync.WaitGroup
	for i := range replies {
		replies[i] = fmt.Errorf("reply %d", i)
		wg.Go(func() {
			stop.record(sideDestination, replies[i])
			seen[i] = stop.get()
		})
	}
	wg.Wait()

	cause := stop.get()
	if cause == nil || !slices.Contains(replies, cause.err) {
		t.Fatalf("cause = %+v, want one of the recorded replies", cause)
	}
	for i, got := range seen {
		if got != cause {
			t.Errorf("worker %d read cause %+v after it recorded, want the one cause %+v", i, got, cause)
		}
	}
}

func Test_runFolderSync_When_TheRunStopsAtTheLastMessageOfAStandbyBatch_Then_NoNextStandbyBatch(t *testing.T) {
	t.Parallel()

	stop := new(throttleStop)
	ids := numberedIDs(clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...), "B": fakeMsgs(ids...)})
	for _, id := range ids {
		src.answerBodyIn("A", id, "NIL")
	}
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	dst.onCommand("APPEND", func() {
		if dst.callCount("APPEND") == clientBatchSize {
			stop.record(sideSource, errors.New("another worker was throttled"))
		}
	})
	plan := copyPlan(len(ids))
	plan.standbySources = []standbySource{{folder: "B", uidValidity: 1}}
	for uid := uint32(1); uid <= uint32(len(ids)); uid++ {
		plan.standbys = append(plan.standbys, standbyCopy{planned: uid, uid: uid, source: 0})
	}

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != clientBatchSize || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want %d, 1, 0", got.synced, got.errs, got.gone, clientBatchSize)
	}
	assertAccounted(t, got, plan)
	// Two batches of A, then one batch of B. The stop falls on the last message of that batch.
	if n := src.callCount("UID FETCH"); n != 3 {
		t.Errorf("source saw %d UID FETCH commands, want 3: no standby batch follows the stop", n)
	}
	if n := dst.callCount("APPEND"); n != clientBatchSize {
		t.Errorf("destination saw %d APPEND commands, want %d", n, clientBatchSize)
	}
	assertReadOnly(t, "source", src)
}

func Test_runFolderSync_When_StandbySourceHasItsOwnUIDValidity_Then_TheStandbyStreamUsesThatValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		wantLog      []string
		standbyValue uint32
		wantSynced   int
		wantErrs     int
	}{
		{name: "the standby source still reports its value", standbyValue: 7, wantSynced: 1},
		{name: "the standby source changed", standbyValue: 8, wantErrs: 1, wantLog: []string{"Stream error for folder B: ", "(UIDVALIDITY 8, now 7)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bodyOfB := subjectBody("m1@x", "from B")
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x"), "B": standbyFolder(bodyOfB)})
			src.answerBodyIn("A", "m1@x", "NIL")
			// The plan holds 1 for folder A, so a stream that used it for B would fail the check.
			src.setUIDValidity("B", 7)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			plan := standbyPlan("B")
			plan.standbySources[0].uidValidity = tt.standbyValue

			got := runStandbyPlan(t.Context(), t, src, dst, plan, false)

			if got.synced != tt.wantSynced || got.errs != tt.wantErrs || got.gone != 0 {
				t.Errorf("synced, errors, gone = %d, %d, %d, want %d, %d, 0", got.synced, got.errs, got.gone, tt.wantSynced, tt.wantErrs)
			}
			assertAccounted(t, got, plan)
			assertContainsAll(t, got.log, tt.wantLog...)
			wantFetches := 1 + tt.wantSynced
			if n := src.callCount("UID FETCH"); n != wantFetches {
				t.Errorf("source saw %d UID FETCH commands, want %d", n, wantFetches)
			}
		})
	}
}

func Test_runFolderSync_When_ExamineGetsAThrottledReply_Then_TheStopNamesTheSourceAndNoCommandFollows(t *testing.T) {
	t.Parallel()

	src := newFakeServer(t)
	src.addConnHandler(scriptedHandler(src, []string{"A"}, func(verb string, _ int) (string, bool) {
		if verb == "EXAMINE" {
			return throttleReply, true
		}
		return "", false
	}))
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	stop := new(throttleStop)
	plan := copyPlan(3)
	plan.SrcUIDValidity = 0

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 0 || got.errs != 3 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 3, 0", got.synced, got.errs, got.gone)
	}
	assertAccounted(t, got, plan)
	cause := stop.get()
	if cause == nil || cause.side != sideSource || !client.IsThrottled(cause.err) {
		t.Errorf("stop cause = %+v, want the source and a throttled reply", cause)
	}
	for verb, want := range map[string]int{"EXAMINE": 1, "UID FETCH": 0, "UID SEARCH": 0} {
		if n := src.callCount(verb); n != want {
			t.Errorf("source saw %d %s commands, want %d", n, verb, want)
		}
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "select folder A", "exceeded command or bandwidth limits")
	assertContainsNone(t, got.log, "batches failed")
}

func Test_runFolderSync_When_ARefusedBatchIsFollowedByAThrottledBatch_Then_OneLineCountsBothAndTheStopNamesTheSource(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...)})
	var fetches atomic.Int32
	src.onCommand("UID", func() {
		switch fetches.Add(1) {
		case 1:
			src.endUIDFetchWith("A", "NO [CANNOT] fetch refused")
		case 2:
			src.endUIDFetchWith("A", throttleReply)
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	stop := new(throttleStop)
	plan := copyPlan(len(ids))

	got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

	if got.synced != 2*clientBatchSize || got.errs != 1 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want %d, 1, 0: both batches delivered their bodies before the status", got.synced, got.errs, got.gone, 2*clientBatchSize)
	}
	assertAccounted(t, got, plan)
	cause := stop.get()
	if cause == nil || cause.side != sideSource || !client.IsThrottled(cause.err) {
		t.Errorf("stop cause = %+v, want the source and a throttled reply", cause)
	}
	if n := strings.Count(got.log, "Stream error for folder"); n != 1 {
		t.Errorf("log holds the stream line %d times, want 1\nlog: %s", n, got.log)
	}
	assertContainsAll(t, got.log, "Stream error for folder A: ", "fetch refused", "(2 batches failed)")
	if n := src.callCount("UID FETCH"); n != 2 {
		t.Errorf("source saw %d UID FETCH commands, want 2: the throttled reply ends the stream", n)
	}
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: a throttled source gets no check", n)
	}
}

func Test_runFolderSync_When_RunIsStoppedAndContextEnds_Then_ReturnsCanceledWithoutCommands(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
	w := newStopWorker(t, src, dst)
	stop := new(throttleStop)
	stop.record(sideDestination, errors.New("Quota exceeded"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	srcBefore, dstBefore := totalCommands(src), totalCommands(dst)

	got := runStopPlan(ctx, t, w, copyPlan(2), stop, false)

	if got.synced != 0 || got.errs != 0 || got.gone != 0 {
		t.Errorf("synced, errors, gone = %d, %d, %d, want 0, 0, 0: a cancel counts nothing", got.synced, got.errs, got.gone)
	}
	if want := "1/1 Canceled"; got.tr.Message != want || !got.tr.IsErrored() {
		t.Errorf("tracker message = %q, errored = %t, want %q and errored: the cancel wins over the stop", got.tr.Message, got.tr.IsErrored(), want)
	}
	if n := totalCommands(src) - srcBefore; n != 0 {
		t.Errorf("source saw %d new commands, want none", n)
	}
	if n := totalCommands(dst) - dstBefore; n != 0 {
		t.Errorf("destination saw %d new commands, want none", n)
	}
	if cause := stop.get(); cause == nil || cause.side != sideDestination {
		t.Errorf("stop cause = %+v, want the one recorded before the cancel", cause)
	}
}

// messageEvent is what the source does to one planned message in the random run.
type messageEvent int

const (
	eventNone          messageEvent = iota
	eventBodylessFirst              // a response without BODY[] comes before the full one
	eventEmptyBody                  // BODY[] is NIL
	eventNoBodyItem                 // no response carries BODY[]
	eventOmitted                    // the FETCH leaves the message out, and the source still holds it
	eventExpunged                   // the source no longer holds the message
	eventKinds
)

// standbyEvent is what the source does to the standby copy of one planned message.
type standbyEvent int

const (
	standbyNone standbyEvent = iota
	standbyOK
	standbyEmpty
	standbyOmitted
	standbyExpunged
)

// throttleKind is the one throttled reply of a random run.
type throttleKind int

const (
	throttleNone throttleKind = iota
	throttleDestinationAppend
	throttleSourceFetch
	throttleSourceExamine
	throttleSourceSearch
	throttleKinds
)

// randomRun is one scripted run of runFolderSync. Index k is the planned message with UID k+1.
type randomRun struct {
	events          []messageEvent
	standbys        []standbyEvent
	dstRefuses      []bool
	throttleAt      int
	throttle        throttleKind
	batchRefused    bool
	validityChanged bool
	searchRefused   bool
}

func (r randomRun) id(k int) string { return fmt.Sprintf("r%d@x", k+1) }

func (r randomRun) bodyOfStandby(k int) string { return subjectBody(r.id(k), "from B") }

// randomOutcome is what runFolderSync must report and send when no reply is a throttle.
type randomOutcome struct {
	fromA, fromB []string
	errs, gone   int
	attempts     int
	searched     int
	standbyAsked int
}

func newRandomRun(rng *rand.Rand) randomRun {
	n := 1 + rng.IntN(12)
	dense := rng.IntN(20) > 0
	if rng.IntN(25) == 0 {
		n, dense = clientBatchSize+2, false
	}
	r := randomRun{
		events:     make([]messageEvent, n),
		standbys:   make([]standbyEvent, n),
		dstRefuses: make([]bool, n),
	}
	for k := range n {
		if dense || rng.IntN(100) == 0 {
			r.events[k] = messageEvent(rng.IntN(int(eventKinds)))
		}
		if dense && rng.IntN(2) == 0 {
			r.standbys[k] = standbyEvent(1 + rng.IntN(4))
		}
		r.dstRefuses[k] = dense && rng.IntN(12) == 0
	}
	r.batchRefused = rng.IntN(8) == 0
	r.validityChanged = rng.IntN(12) == 0
	r.searchRefused = rng.IntN(10) == 0
	if rng.IntN(7) == 0 {
		r.throttle = throttleKind(1 + rng.IntN(int(throttleKinds)-1))
		r.throttleAt = rng.IntN(n)
	}
	return r
}

// expect follows the rules of the copy: a message ends as synced, an error or gone, and a standby copy can replace a planned one that waits.
func (r randomRun) expect() randomOutcome {
	var out randomOutcome
	pending := make(map[int]pendingCause)
	var undelivered []int
	for k, event := range r.events {
		delivered := !r.validityChanged && (event == eventNone || event == eventBodylessFirst || event == eventEmptyBody)
		switch {
		case !delivered:
			undelivered = append(undelivered, k)
		case event == eventEmptyBody:
			if r.standbys[k] != standbyNone {
				pending[k] = causeRefusedBody
			} else {
				out.errs++
			}
		case r.dstRefuses[k]:
			out.attempts++
			out.errs++
		default:
			out.attempts++
			out.fromA = append(out.fromA, r.id(k))
		}
	}
	switch {
	case len(undelivered) == 0 || r.validityChanged:
	case r.searchRefused:
		out.searched = 1
	default:
		out.searched = (len(undelivered) + clientBatchSize - 1) / clientBatchSize
	}
	for _, k := range undelivered {
		cause := causeNotDelivered
		if r.events[k] == eventExpunged && !r.validityChanged && !r.searchRefused {
			cause = causeGone
		}
		switch {
		case r.standbys[k] != standbyNone:
			pending[k] = cause
		case cause == causeGone:
			out.gone++
		default:
			out.errs++
		}
	}
	out.standbyAsked = len(pending)
	for k := range r.events {
		if _, waits := pending[k]; !waits || r.standbys[k] != standbyOK {
			continue
		}
		out.attempts++
		if r.dstRefuses[k] {
			out.errs++
		} else {
			out.fromB = append(out.fromB, r.id(k))
		}
		delete(pending, k)
	}
	for _, cause := range pending {
		if cause == causeGone {
			out.gone++
		} else {
			out.errs++
		}
	}
	return out
}

func Test_runFolderSync_When_EventsAreRandom_Then_EveryPlannedMessageEndsOnceAndNothingIsSentTwice(t *testing.T) {
	t.Parallel()

	for seed := uint64(1); seed <= 150; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			run := newRandomRun(rand.New(rand.NewPCG(seed, seed*7919)))
			n := len(run.events)
			folderA := make([]fakeMsg, n)
			var folderB []fakeMsg
			plan := copyPlan(n)
			plan.standbySources = []standbySource{{folder: "B", uidValidity: 1}}
			for k := range n {
				folderA[k] = fakeMsg{msgID: run.id(k)}
				if run.standbys[k] != standbyNone {
					folderB = append(folderB, fakeMsg{msgID: run.id(k), body: run.bodyOfStandby(k)})
					plan.standbys = append(plan.standbys, standbyCopy{planned: uint32(k + 1), uid: uint32(len(folderB)), source: 0})
				}
			}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": folderA, "B": folderB})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			for k, event := range run.events {
				id := run.id(k)
				switch event {
				case eventBodylessFirst:
					src.omitBodyFirstIn("A", id)
				case eventEmptyBody:
					src.answerBodyIn("A", id, "NIL")
				case eventNoBodyItem:
					src.omitBodyIn("A", id)
				case eventOmitted:
					src.omitInUIDFetch("A", id)
				case eventExpunged:
					src.removeMessage("A", id)
				}
				switch run.standbys[k] {
				case standbyEmpty:
					src.answerBodyIn("B", id, "NIL")
				case standbyOmitted:
					src.omitInUIDFetch("B", id)
				case standbyExpunged:
					src.removeMessage("B", id)
				}
				if run.dstRefuses[k] {
					dst.refuseAppendOf(id, "NO [CANNOT] message refused by policy")
				}
			}
			if run.batchRefused {
				src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
			}
			if run.validityChanged {
				src.setUIDValidity("A", 2)
			}
			if run.searchRefused {
				src.refuseUIDSearchWith("A", "NO [CANNOT] search refused")
			}
			switch run.throttle {
			case throttleDestinationAppend:
				dst.refuseAppendOf(run.id(run.throttleAt), gmailThrottleReply)
			case throttleSourceFetch:
				src.endUIDFetchWith("A", throttleReply)
			case throttleSourceExamine:
				src.refuseOpenWith("A", throttleReply)
			case throttleSourceSearch:
				src.refuseUIDSearchWith("A", throttleReply)
			}
			stop := new(throttleStop)

			got := runStopPlan(t.Context(), t, newStopWorker(t, src, dst), plan, stop, false)

			assertAccounted(t, got, plan)
			appended := dst.appended("X")
			sortedIDs := slices.Sorted(slices.Values(appended))
			if compacted := slices.Compact(slices.Clone(sortedIDs)); len(compacted) != len(sortedIDs) {
				t.Errorf("destination stored a Message-Id twice: %v", appended)
			}
			if len(appended) != got.synced {
				t.Errorf("destination stored %d messages, runFolderSync reported %d synced", len(appended), got.synced)
			}
			for _, body := range dst.folderBodies("X") {
				if body == "" {
					t.Error("destination stored an empty message")
				}
			}
			assertReadOnly(t, "source", src)

			if run.throttle != throttleNone {
				if stop.stopped() && run.throttle != throttleDestinationAppend && slices.Contains(src.capturedNames("EXAMINE"), "B") {
					t.Errorf("source examined the standby folder after its own throttled reply: %v", src.capturedNames("EXAMINE"))
				}
				if stop.stopped() && run.throttle == throttleSourceFetch && src.callCount("UID SEARCH") != 0 {
					t.Errorf("source saw %d UID SEARCH commands after a throttled FETCH, want 0", src.callCount("UID SEARCH"))
				}
				return
			}

			want := run.expect()
			if got.synced != len(want.fromA)+len(want.fromB) || got.errs != want.errs || got.gone != want.gone {
				t.Errorf("synced, errors, gone = %d, %d, %d, want %d, %d, %d\nrun: %+v", got.synced, got.errs, got.gone, len(want.fromA)+len(want.fromB), want.errs, want.gone, run)
			}
			if wantIDs := slices.Sorted(slices.Values(slices.Concat(want.fromA, want.fromB))); !slices.Equal(sortedIDs, wantIDs) {
				t.Errorf("destination holds %v, want %v", sortedIDs, wantIDs)
			}
			wantBodies := make([]string, 0, len(want.fromA)+len(want.fromB))
			for _, id := range want.fromA {
				wantBodies = append(wantBodies, imapFullBody(id))
			}
			for k := range n {
				if slices.Contains(want.fromB, run.id(k)) {
					wantBodies = append(wantBodies, run.bodyOfStandby(k))
				}
			}
			if bodies := dst.folderBodies("X"); !slices.Equal(slices.Sorted(slices.Values(bodies)), slices.Sorted(slices.Values(wantBodies))) {
				t.Errorf("destination bodies differ from the copies the rules choose (%d stored, %d wanted)", len(bodies), len(wantBodies))
			}
			if stop.stopped() {
				t.Errorf("stop cause = %+v, want none: no reply was a throttle", stop.get())
			}
			if appends := dst.callCount("APPEND"); appends != want.attempts {
				t.Errorf("destination saw %d APPEND commands, want %d", appends, want.attempts)
			}
			if searches := src.callCount("UID SEARCH"); searches != want.searched {
				t.Errorf("source saw %d UID SEARCH commands, want %d", searches, want.searched)
			}
			wantFetches := 0
			if !run.validityChanged {
				wantFetches = (n + clientBatchSize - 1) / clientBatchSize
			}
			wantFetches += (want.standbyAsked + clientBatchSize - 1) / clientBatchSize
			if fetches := src.callCount("UID FETCH"); fetches != wantFetches {
				t.Errorf("source saw %d UID FETCH commands, want %d", fetches, wantFetches)
			}
		})
	}
}
