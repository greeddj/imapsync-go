package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
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

	synced, errors := runFolderSync(context.Background(), w, plan, tr, 0, 1, pw, false)

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
	dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, "NO Quota exceeded"))

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

	synced, errors := runFolderSync(context.Background(), w, plan, tr, 0, 1, pw, false)

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

	synced, errors := runFolderSync(ctx, w, plan, tr, 0, 1, pw, false)

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
	synced, errors := runFolderSync(context.Background(), w, plan, tr, 0, 1, pw, true)
	if synced != 1 {
		t.Errorf("synced=%d, want 1", synced)
	}
	if errors != 0 {
		t.Errorf("errors=%d, want 0", errors)
	}
}

// Test_runFolderSync_streamError asserts that a non-transient error from
// StreamMessagesByUIDs (src server returns BAD for UID FETCH) is surfaced as
// a stream-error: errors is incremented and the tracker is marked as errored.
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

	synced, errors := runFolderSync(context.Background(), w, plan, tr, 0, 1, pw, false)
	if synced != 0 {
		t.Errorf("synced=%d, want 0", synced)
	}
	// The stream error increments errors by 1 in the post-loop check.
	if errors != 1 {
		t.Errorf("errors=%d, want 1 (from stream error)", errors)
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
			dstSrv.addConnHandler(uidFetchBodyHandler(dstSrv, []string{"INBOX"}, nil, "NO Quota exceeded"))

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

			runFolderSync(context.Background(), w, plan, tr, 0, 1, pw, tc.verbose)

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

			synced, _ := runFolderSync(t.Context(), w, plan, tr, 0, 1, pw, true)
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
		runFolderSync(t.Context(), w, plan, tr, 0, 1, progress.NewWriter(1, true), false)
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

	synced, errs := runFolderSync(ctx, w, plan, tr, 0, 1, progress.NewWriter(1, true), false)

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

	synced, errs := runFolderSync(t.Context(), w, plan, tr, 0, 1, pw, false)
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

func Test_runFolderSync_When_SourceUIDValidityChanged_Then_CountsOneErrorAndAppendsNothing(t *testing.T) {
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

	synced, errs := runFolderSync(t.Context(), w, plan, tr, 0, 1, pw, false)
	pw.Stop()
	pw.WaitForRenderDone()

	if synced != 0 || errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 0, 1", synced, errs)
	}
	if !tr.IsErrored() {
		t.Error("tracker not marked as errored after the mismatch")
	}
	if got := dst.callCount("APPEND"); got != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", got)
	}
	if got := src.callCount("UID FETCH"); got != 0 {
		t.Errorf("source saw %d UID FETCH commands, want 0", got)
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
}

// runStandbyPlan runs the plan on one worker and returns what the worker counted and logged.
func runStandbyPlan(ctx context.Context, t *testing.T, src, dst *fakeServer, plan FolderSyncPlan, verbose bool) standbyRun {
	t.Helper()

	w := &syncWorker{src: newAppClient(t, src, "src"), dst: newAppClient(t, dst, "dst")}
	sb := &syncBuffer{}
	// The sentinel stays active so the render pass Stop triggers still prints the
	// queued log lines.
	pw := progress.NewWriter(2, true)
	pw.SetOutputWriter(sb)
	pw.Start()
	pw.AppendTracker(progress.NewTracker("sentinel", 100))
	tr := progress.NewTracker("test", 10)
	pw.AppendTracker(tr)

	synced, errs := runFolderSync(ctx, w, plan, tr, 0, 1, pw, verbose)
	pw.Stop()
	pw.WaitForRenderDone()
	return standbyRun{tr: tr, log: sb.String(), synced: synced, errs: errs}
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

	if got.synced != 1 || got.errs != 1 {
		t.Errorf("synced, errors = %d, %d, want 1, 1: the stream error is the only error", got.synced, got.errs)
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
