package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

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
		tc := tc
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
			// the LengthActive()==0 short-circuit — that short-circuit would drop
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
