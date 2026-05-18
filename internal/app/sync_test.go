package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
)

func TestBuildProviderWarning_noKnownProvider_returnsEmpty(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Src:     config.Credentials{Server: "mail.privatehost.example:993"},
		Dst:     config.Credentials{Server: "imap.otherhost.example:993"},
		Workers: 4,
	}
	got := buildProviderWarning(cfg, nil, nil)
	if got != "" {
		t.Errorf("buildProviderWarning for unknown servers = %q, want empty", got)
	}
}

func TestBuildProviderWarning_gmailSrc_recommendsBPS(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Src:     config.Credentials{Server: "imap.gmail.com:993"},
		Dst:     config.Credentials{Server: "mail.privatehost.example:993"},
		Workers: 4,
	}
	got := buildProviderWarning(cfg, nil, nil)
	if !strings.Contains(got, "Gmail") {
		t.Errorf("warning missing provider name: %q", got)
	}
	if !strings.Contains(got, "max simultaneous connections: 15") {
		t.Errorf("warning missing connection cap: %q", got)
	}
	// Limiter is nil → recommend a concrete --bps-down value (300000 from
	// the Gmail provider profile).
	if !strings.Contains(got, "--bps-down 300000") {
		t.Errorf("warning missing bps-down recommendation: %q", got)
	}
}

func TestBuildProviderWarning_gmailDst_withLimiter_omitsRecommendation(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Src:     config.Credentials{Server: "mail.privatehost.example:993"},
		Dst:     config.Credentials{Server: "imap.gmail.com:993"},
		Workers: 4,
	}
	// A non-nil limiter signals "user already configured throttle" — the
	// warning must not nag with another recommendation.
	dstLim := ratelimit.NewLimiter(300_000)
	got := buildProviderWarning(cfg, nil, dstLim)
	if strings.Contains(got, "no rate limit set") {
		t.Errorf("warning suggests rate limit even though dstLim is configured: %q", got)
	}
}

func TestBuildProviderWarning_workersOverProviderCap_addsHint(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Src:     config.Credentials{Server: "imap.gmail.com:993"},
		Dst:     config.Credentials{Server: "mail.privatehost.example:993"},
		Workers: 20,
	}
	got := buildProviderWarning(cfg, nil, nil)
	if !strings.Contains(got, "may exceed") {
		t.Errorf("warning missing workers-exceed hint: %q", got)
	}
}

func TestBuildProviderWarning_gmailDst_nilLimiter_recommendsBPSUp(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Src:     config.Credentials{Server: "mail.privatehost.example:993"},
		Dst:     config.Credentials{Server: "imap.gmail.com:993"},
		Workers: 4,
	}
	// nil limiter for dst → the isUpload=true branch sets flag to "--bps-up".
	got := buildProviderWarning(cfg, nil, nil)
	if !strings.Contains(got, "--bps-up") {
		t.Errorf("warning missing --bps-up recommendation: %q", got)
	}
}

func TestFolderDelimiter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		server    string
		wantDelim string
		wantOK    bool
	}{
		{name: "flatPathNoServer", path: "INBOX", server: "", wantDelim: "none", wantOK: true},
		{name: "flatPathWithServer", path: "INBOX", server: "/", wantDelim: "none", wantOK: true},
		{name: "slashMatches", path: "Archive/2023", server: "/", wantDelim: "/", wantOK: true},
		{name: "slashMismatchOnDot", path: "Archive/2023", server: ".", wantDelim: "/", wantOK: false},
		{name: "dotMatches", path: "Archive.2023", server: ".", wantDelim: ".", wantOK: true},
		{name: "dotMismatchOnSlash", path: "Archive.2023", server: "/", wantDelim: ".", wantOK: false},
		{name: "backslashMatches", path: `Archive\2023`, server: `\`, wantDelim: `\`, wantOK: true},
		{name: "backslashMismatchOnSlash", path: `Archive\2023`, server: "/", wantDelim: `\`, wantOK: false},
		{name: "noServerMeansAnyOK", path: "Archive/2023", server: "", wantDelim: "/", wantOK: true},
		{name: "firstDelimWins", path: "a/b.c", server: "/", wantDelim: "/", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotDelim, gotOK := folderDelimiter(tt.path, tt.server)
			if gotDelim != tt.wantDelim || gotOK != tt.wantOK {
				t.Errorf("folderDelimiter(%q, %q) = (%q, %v), want (%q, %v)",
					tt.path, tt.server, gotDelim, gotOK, tt.wantDelim, tt.wantOK)
			}
		})
	}
}

func TestComputeEffectiveWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		workers int
		maxConn int
		planCnt int
		want    int
	}{
		{name: "underBudget", workers: 4, maxConn: 15, planCnt: 20, want: 4},
		{name: "atBudgetReservesOneForPlanner", workers: 15, maxConn: 15, planCnt: 20, want: 14},
		{name: "overBudgetClampedToMaxConnMinusOne", workers: 20, maxConn: 15, planCnt: 20, want: 14},
		{name: "maxConnOneFloorsToOne", workers: 1, maxConn: 1, planCnt: 20, want: 1},
		{name: "unlimitedMaxConn", workers: 4, maxConn: 0, planCnt: 20, want: 4},
		{name: "fewerPlansThanWorkers", workers: 4, maxConn: 15, planCnt: 2, want: 2},
		{name: "zeroPlansFloorsToOne", workers: 4, maxConn: 15, planCnt: 0, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := computeEffectiveWorkers(tt.workers, tt.maxConn, tt.planCnt); got != tt.want {
				t.Errorf("computeEffectiveWorkers(%d, %d, %d) = %d, want %d",
					tt.workers, tt.maxConn, tt.planCnt, got, tt.want)
			}
		})
	}
}

func TestDedupeMappings_keepsFirstOccurrence(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "A", Destination: "Y"},
		{Source: "B", Destination: "Z"},
	}
	out, dropped := dedupeMappings(in)

	wantOut := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "B", Destination: "Z"},
	}
	if !slices.Equal(out, wantOut) {
		t.Errorf("out=%+v, want %+v", out, wantOut)
	}
	wantDropped := []config.DirectoryMapping{{Source: "A", Destination: "Y"}}
	if !slices.Equal(dropped, wantDropped) {
		t.Errorf("dropped=%+v, want %+v", dropped, wantDropped)
	}
}

func TestDedupeMappings_emptyInput(t *testing.T) {
	t.Parallel()

	out, dropped := dedupeMappings(nil)
	if len(out) != 0 {
		t.Errorf("out=%+v, want empty", out)
	}
	if dropped != nil {
		t.Errorf("dropped=%+v, want nil", dropped)
	}
}

func TestDedupeMappings_noDuplicates_passthrough(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "B", Destination: "Y"},
		{Source: "C", Destination: "Z"},
	}
	out, dropped := dedupeMappings(in)

	if !slices.Equal(out, in) {
		t.Errorf("out=%+v, want %+v", out, in)
	}
	if dropped != nil {
		t.Errorf("dropped=%+v, want nil", dropped)
	}
}

// TestDedupeMappings_parentExpandsBeforeExplicitChild mirrors the real bug:
// a parent mapping's subfolder expansion produces an entry that an explicit
// later mapping would re-add. The first occurrence (the expansion) wins.
func TestDedupeMappings_parentExpandsBeforeExplicitChild(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "jira", Destination: "jira"},
		{Source: "jira/DEVOPS", Destination: "jira/DEVOPS"},
		{Source: "jira/INTERNAL", Destination: "jira/INTERNAL"},
		{Source: "jira/DEVOPS", Destination: "DEVOPS"},
		{Source: "jira/INTERNAL", Destination: "INTERNAL"},
	}
	out, dropped := dedupeMappings(in)
	if len(out) != 3 {
		t.Errorf("len(out)=%d, want 3, got %+v", len(out), out)
	}
	if len(dropped) != 2 {
		t.Errorf("len(dropped)=%d, want 2, got %+v", len(dropped), dropped)
	}
}

// makePlanPW creates the progress objects required by buildSyncPlan. quiet=true
// suppresses all terminal output during tests.
func makePlanPW() (*progress.Writer, *progress.Tracker, *progress.Tracker) {
	pw := progress.NewWriter(1, true)
	srcTr := progress.NewTracker("src", 10)
	dstTr := progress.NewTracker("dst", 10)
	return pw, srcTr, dstTr
}

// Test_buildSyncPlan_emptySrc_returnsNoPlans asserts the "already in sync"
// short-circuit: when src has zero messages, the plan list is empty.
func Test_buildSyncPlan_emptySrc_returnsNoPlans(t *testing.T) {
	// Not t.Parallel() — per-connection handler state is shared with the
	// fakeServer acceptLoop, and parallel tests would race on connHandlerIdx.

	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 0 {
		t.Errorf("Plans=%d, want 0", len(summary.Plans))
	}
	if summary.TotalNew != 0 {
		t.Errorf("TotalNew=%d, want 0", summary.TotalNew)
	}
}

// Test_buildSyncPlan_messageIdDiff_correct asserts the Message-Id diff: src
// has a@x, b@x, c@x at UIDs 1–3; dst has only b@x → plan must contain UIDs
// 1 and 3 (sorted), NewMessages==2, DestinationFolderExists==true.
func Test_buildSyncPlan_messageIdDiff_correct(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {
			{uid: 1, msgID: "a@x"},
			{uid: 2, msgID: "b@x"},
			{uid: 3, msgID: "c@x"},
		},
	}
	dstMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {
			{uid: 1, msgID: "b@x"},
		},
	}

	srcSrv.addConnHandler(msgIDFetchHandler(srcSrv, []string{"INBOX"}, srcMsgs))
	dstSrv.addConnHandler(msgIDFetchHandler(dstSrv, []string{"INBOX"}, dstMsgs))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 1 {
		t.Fatalf("Plans=%d, want 1", len(summary.Plans))
	}
	plan := summary.Plans[0]
	if plan.NewMessages != 2 {
		t.Errorf("NewMessages=%d, want 2", plan.NewMessages)
	}
	if !plan.DestinationFolderExists {
		t.Error("DestinationFolderExists=false, want true")
	}
	want := []uint32{1, 3}
	if !slices.Equal(plan.SrcUIDs, want) {
		t.Errorf("SrcUIDs=%v, want %v", plan.SrcUIDs, want)
	}
	if summary.TotalNew != 2 {
		t.Errorf("TotalNew=%d, want 2", summary.TotalNew)
	}
}

// Test_buildSyncPlan_dstMissing_setsDestinationFolderExistsFalse asserts that
// when the destination folder is absent from the mailbox cache, the plan sets
// DestinationFolderExists=false and no FETCH is issued on the dst side.
func Test_buildSyncPlan_dstMissing_setsDestinationFolderExistsFalse(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {{uid: 1, msgID: "only@x"}},
	}
	srcSrv.addConnHandler(msgIDFetchHandler(srcSrv, []string{"INBOX"}, srcMsgs))

	// dst LIST returns no mailboxes → INBOX not in cache → MailboxExists=false.
	dstSrv.addConnHandler(emptyListHandler(dstSrv))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 1 {
		t.Fatalf("Plans=%d, want 1", len(summary.Plans))
	}
	plan := summary.Plans[0]
	if plan.DestinationFolderExists {
		t.Error("DestinationFolderExists=true, want false")
	}
	if plan.NewMessages != 1 {
		t.Errorf("NewMessages=%d, want 1", plan.NewMessages)
	}
	if dstSrv.callCount("FETCH") != 0 {
		t.Errorf("dst FETCH count=%d, want 0", dstSrv.callCount("FETCH"))
	}
	if dstSrv.callCount("UID FETCH") != 0 {
		t.Errorf("dst UID FETCH count=%d, want 0", dstSrv.callCount("UID FETCH"))
	}
}

// Test_buildSyncPlan_dstErrorIsFatal asserts that a non-transient error from
// the destination EXAMINE causes buildSyncPlan to return an error wrapping
// "scan destination folder".
func Test_buildSyncPlan_dstErrorIsFatal(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {{uid: 1, msgID: "msg@x"}},
	}
	srcSrv.addConnHandler(msgIDFetchHandler(srcSrv, []string{"INBOX"}, srcMsgs))

	// dst has INBOX in cache but EXAMINE returns BAD → FetchMessageMap fails.
	dstSrv.addConnHandler(badExamineHandler(dstSrv))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	_, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "scan destination folder") {
		t.Errorf("error %q does not contain %q", err.Error(), "scan destination folder")
	}
}

// Test_buildSyncPlan_srcErrorContinues_dstFineSkipsMapping asserts that a
// source-side EXAMINE error causes that mapping to be skipped (not fatal),
// while subsequent mappings are still planned.
func Test_buildSyncPlan_srcErrorContinues_dstFineSkipsMapping(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {{uid: 1, msgID: "ok@x"}},
	}
	srcSrv.addConnHandler(noOnMissingFetchHandler(srcSrv, []string{"INBOX", "Missing"}, srcMsgs))
	dstC := newAppClient(t, dstSrv, "dst")
	srcC := newAppClient(t, srcSrv, "src")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{
		{Source: "Missing", Destination: "Missing"},
		{Source: "INBOX", Destination: "INBOX"},
	}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err != nil {
		t.Fatalf("buildSyncPlan returned error: %v", err)
	}
	if len(summary.Plans) != 1 {
		t.Fatalf("Plans=%d, want 1 (only INBOX plan)", len(summary.Plans))
	}
	if summary.Plans[0].SourceFolder != "INBOX" {
		t.Errorf("plan source=%q, want INBOX", summary.Plans[0].SourceFolder)
	}
}

// Test_buildSyncPlan_srcErrorVerboseLogging asserts that the verbose pw.Log
// call for a source-side scan error is reached when verbose=true.
func Test_buildSyncPlan_srcErrorVerboseLogging(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcMsgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {{uid: 1, msgID: "ok@x"}},
	}
	srcSrv.addConnHandler(noOnMissingFetchHandler(srcSrv, []string{"INBOX", "Missing"}, srcMsgs))
	dstC := newAppClient(t, dstSrv, "dst")
	srcC := newAppClient(t, srcSrv, "src")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{
		{Source: "Missing", Destination: "Missing"},
		{Source: "INBOX", Destination: "INBOX"},
	}

	// verbose=true exercises the pw.Log("⚠️ Failed to fetch source folder...")
	// branch for the "Missing" folder error.
	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", true)
	if err != nil {
		t.Fatalf("buildSyncPlan returned error: %v", err)
	}
	if len(summary.Plans) != 1 {
		t.Fatalf("Plans=%d, want 1", len(summary.Plans))
	}
}

// Test_expandMappingsWithSubfolders_addsChildrenAndDedups asserts that
// subfolders in the mailbox cache are appended and that a later explicit
// mapping for the same source is deduplicated in favour of the expansion.
func Test_expandMappingsWithSubfolders_addsChildrenAndDedups(t *testing.T) {
	srcSrv := newFakeServer(t)

	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"jira", "jira/DEVOPS", "jira/SCD"}))

	srcC := newAppClient(t, srcSrv, "src")

	delimiter := srcC.GetDelimiter() // "/"

	mappings1 := []config.DirectoryMapping{{Source: "jira", Destination: "jira"}}
	got1, err := expandMappingsWithSubfolders(context.Background(), srcC, mappings1, delimiter, delimiter, false, true)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders run1: %v", err)
	}
	if len(got1) != 3 {
		t.Fatalf("run1 len=%d, want 3, got %v", len(got1), got1)
	}
	srcs1 := make([]string, len(got1))
	for i, m := range got1 {
		srcs1[i] = m.Source
	}
	slices.Sort(srcs1)
	if !slices.Equal(srcs1, []string{"jira", "jira/DEVOPS", "jira/SCD"}) {
		t.Errorf("run1 sources=%v, want [jira jira/DEVOPS jira/SCD]", srcs1)
	}

	mappings2 := []config.DirectoryMapping{
		{Source: "jira", Destination: "jira"},
		{Source: "jira/DEVOPS", Destination: "DEVOPS"},
	}
	got2, err := expandMappingsWithSubfolders(context.Background(), srcC, mappings2, delimiter, delimiter, false, true)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders run2: %v", err)
	}
	if len(got2) != 3 {
		t.Fatalf("run2 len=%d, want 3, got %v", len(got2), got2)
	}
	// The first occurrence of jira/DEVOPS is from the parent expansion, whose
	// destination is jira/DEVOPS (not the explicit DEVOPS).
	var devopsMapping config.DirectoryMapping
	for _, m := range got2 {
		if m.Source == "jira/DEVOPS" {
			devopsMapping = m
			break
		}
	}
	if devopsMapping.Destination != "jira/DEVOPS" {
		t.Errorf("jira/DEVOPS destination=%q, want jira/DEVOPS (first occurrence wins)", devopsMapping.Destination)
	}
}

// --- per-test connection handler helpers ---

// emptyListHandler serves a LIST that returns no mailboxes. Used to simulate
// a dst where the target folder does not yet exist.
func emptyListHandler(srv *fakeServer) func(net.Conn) {
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
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
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

// badExamineHandler serves LIST with INBOX (so the cache has it) but responds
// to EXAMINE with a BAD error. Used to simulate a fatal dst scan error.
func badExamineHandler(srv *fakeServer) func(net.Conn) {
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
				_, _ = fmt.Fprintf(conn, "%s BAD internal error\r\n", tag)
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

// noOnMissingFetchHandler returns a handler that sends NO for EXAMINE on the
// "Missing" folder and normal responses for every other folder.
func noOnMissingFetchHandler(srv *fakeServer, mailboxes []string, msgs map[string][]struct {
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
				folder := strings.Trim(arg, `"`)
				if folder == "Missing" {
					_, _ = fmt.Fprintf(conn, "%s NO Mailbox does not exist\r\n", tag)
					continue
				}
				selectedFolder = folder
				n := len(msgs[folder])
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

// customListHandler returns a handler that serves the given mailboxes in the
// LIST response and normal responses for everything else. Used to prime the
// mailbox cache with specific folder names (e.g. for subfolder expansion tests).
func customListHandler(srv *fakeServer, mailboxes []string) func(net.Conn) {
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
				for _, mb := range mailboxes {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
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

// Test_expandMappingsWithSubfolders_verboseLogging asserts that the verbose
// path in expandMappingsWithSubfolders runs without error and produces the
// correct deduplication result.
func Test_expandMappingsWithSubfolders_verboseLogging(t *testing.T) {
	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"INBOX", "INBOX/Sub"}))

	srcC := newAppClient(t, srcSrv, "src")
	delimiter := srcC.GetDelimiter()

	// Explicit INBOX/Sub mapping will be deduped (expansion wins). The verbose +
	// !quiet path exercises the fmt.Printf calls for found-subfolder and dropped.
	mappings := []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
		{Source: "INBOX/Sub", Destination: "INBOX/Sub"},
	}
	got, err := expandMappingsWithSubfolders(context.Background(), srcC, mappings, delimiter, delimiter, true, false)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (INBOX + INBOX/Sub), got %v", len(got), got)
	}
}

// Test_expandMappingsWithSubfolders_canceledContext asserts that a pre-canceled
// context causes expandMappingsWithSubfolders to return an error immediately.
func Test_expandMappingsWithSubfolders_canceledContext(t *testing.T) {
	srcSrv := newFakeServer(t)
	srcC := newAppClient(t, srcSrv, "src")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := expandMappingsWithSubfolders(ctx, srcC, []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
	}, "/", "/", false, true)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
}

// Test_buildSyncPlan_canceledContext asserts that a pre-canceled context causes
// buildSyncPlan to return an error immediately without scanning any folders.
func Test_buildSyncPlan_canceledContext(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)
	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pw, srcTr, dstTr := makePlanPW()
	_, err := buildSyncPlan(ctx, srcC, dstC, []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
	}, srcTr, dstTr, pw, "src", "dst", false)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
}

// Test_expandMappingsWithSubfolders_emptyDelimiters covers the else-branches
// for relativePath and dstPath when srcDelimiter and dstDelimiter are "".
// listSubfoldersFromCache falls back to the client's cached "/" delimiter, so
// INBOX/Sub is found. With empty dstDelimiter the destination is the parent
// mapping's destination (not expanded).
func Test_expandMappingsWithSubfolders_emptyDelimiters(t *testing.T) {
	srcSrv := newFakeServer(t)
	// Include a real subfolder so listSubfoldersFromCache (using "/") finds it.
	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"INBOX", "INBOX/Sub"}))
	srcC := newAppClient(t, srcSrv, "src")

	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "TARGET"}}
	// srcDelimiter="" → else branch: relativePath = TrimPrefix(subfolder, mapping.Source)
	// dstDelimiter="" → else branch: dstPath = mapping.Destination
	got, err := expandMappingsWithSubfolders(context.Background(), srcC, mappings, "", "", false, true)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (INBOX + INBOX/Sub), got %v", len(got), got)
	}
	if got[1].Destination != "TARGET" {
		t.Errorf("INBOX/Sub destination=%q, want TARGET", got[1].Destination)
	}
}

// Test_expandMappingsWithSubfolders_delimiterConversion covers the
// srcDelimiter != dstDelimiter path that rewrites the subfolder relative path
// before building the destination folder name.
func Test_expandMappingsWithSubfolders_delimiterConversion(t *testing.T) {
	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"jira", "jira/DEVOPS"}))
	srcC := newAppClient(t, srcSrv, "src")

	mappings := []config.DirectoryMapping{{Source: "jira", Destination: "jira"}}
	// srcDelimiter="/", dstDelimiter="." → ReplaceAll("/", ".") in relative path.
	got, err := expandMappingsWithSubfolders(context.Background(), srcC, mappings, "/", ".", false, true)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2, got %v", len(got), got)
	}
	// The subfolder jira/DEVOPS has relative path "DEVOPS"; after conversion
	// (no "/" in "DEVOPS" to replace) the dst is "jira.DEVOPS".
	if got[1].Destination != "jira.DEVOPS" {
		t.Errorf("jira/DEVOPS destination=%q, want jira.DEVOPS", got[1].Destination)
	}
}

// Test_maybeDiff_freesMaps asserts that maybeDiff releases srcMap and dstIDs
// after the diff, and that the resulting plan contains the correct SrcUIDs.
func Test_maybeDiff_freesMaps(t *testing.T) {
	t.Parallel()

	t.Run("happyPath_diffProducesUIDs", func(t *testing.T) {
		t.Parallel()
		mappings := []config.DirectoryMapping{
			{Source: "INBOX", Destination: "INBOX"},
		}
		plans := make([]FolderSyncPlan, 1)
		var totalNew atomic.Int64

		s := &folderScan{
			srcMap: map[string]uint32{"a@x": 1, "b@x": 2},
			dstIDs: map[string]struct{}{"b@x": {}},
		}

		// First call: only one side arrived — must not diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}
		if s.srcMap == nil || s.dstIDs == nil {
			t.Fatal("maps freed prematurely after first call")
		}

		// Second call: both sides arrived — diff fires.
		if !maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after diff")
		}
		if s.dstIDs != nil {
			t.Error("dstIDs not nil after diff")
		}
		want := []uint32{1}
		if !slices.Equal(plans[0].SrcUIDs, want) {
			t.Errorf("SrcUIDs=%v, want %v", plans[0].SrcUIDs, want)
		}
		if totalNew.Load() != 1 {
			t.Errorf("totalNew=%d, want 1", totalNew.Load())
		}
	})

	t.Run("srcErr_freesMaps", func(t *testing.T) {
		t.Parallel()
		mappings := []config.DirectoryMapping{
			{Source: "INBOX", Destination: "INBOX"},
		}
		plans := make([]FolderSyncPlan, 1)
		var totalNew atomic.Int64

		s := &folderScan{
			srcErr: fmt.Errorf("src scan failed"),
			srcMap: map[string]uint32{"a@x": 1},
			dstIDs: map[string]struct{}{"b@x": {}},
		}

		// First call: one side arrived — no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived — error branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after srcErr branch")
		}
		if s.dstIDs != nil {
			t.Error("dstIDs not nil after srcErr branch")
		}
		if plans[0].SourceFolder != "" {
			t.Errorf("plan populated despite srcErr: %+v", plans[0])
		}
		if totalNew.Load() != 0 {
			t.Errorf("totalNew=%d, want 0", totalNew.Load())
		}
	})

	t.Run("dstErr_freesMaps", func(t *testing.T) {
		t.Parallel()
		mappings := []config.DirectoryMapping{
			{Source: "INBOX", Destination: "INBOX"},
		}
		plans := make([]FolderSyncPlan, 1)
		var totalNew atomic.Int64

		s := &folderScan{
			dstErr: fmt.Errorf("dst scan failed"),
			srcMap: map[string]uint32{"a@x": 1},
			dstIDs: map[string]struct{}{"b@x": {}},
		}

		// First call: one side arrived — no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived — dstErr branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after dstErr branch")
		}
		if s.dstIDs != nil {
			t.Error("dstIDs not nil after dstErr branch")
		}
		if plans[0].SourceFolder != "" {
			t.Errorf("plan populated despite dstErr: %+v", plans[0])
		}
		if totalNew.Load() != 0 {
			t.Errorf("totalNew=%d, want 0", totalNew.Load())
		}
	})
}

// Test_buildSyncPlan_srcAdvancesAheadOfDst asserts that the src tracker
// advances independently when dst is slow: after 120ms src must be ahead.
func Test_buildSyncPlan_srcAdvancesAheadOfDst(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	msgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX":  {{uid: 1, msgID: "a@x"}},
		"Drafts": {{uid: 1, msgID: "b@x"}},
	}

	// src responds instantly; dst sleeps 250ms per EXAMINE.
	srcSrv.addConnHandler(msgIDFetchHandler(srcSrv, []string{"INBOX", "Drafts"}, msgs))
	dstSrv.addConnHandler(slowExamineHandler(dstSrv, []string{"INBOX", "Drafts"}, msgs, 250*time.Millisecond))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
		{Source: "Drafts", Destination: "Drafts"},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	}()

	// Sample after src should have finished but dst is still sleeping.
	time.Sleep(120 * time.Millisecond)
	srcVal := srcTr.Value()
	dstVal := dstTr.Value()

	<-done

	if srcVal <= dstVal {
		t.Errorf("expected src tracker (%d) ahead of dst tracker (%d) at 120ms", srcVal, dstVal)
	}
}

// Test_buildSyncPlan_dstErrorMidStream asserts that a dst BAD error on mapping
// 2 (with slow src) causes buildSyncPlan to return quickly with a wrapped error.
func Test_buildSyncPlan_dstErrorMidStream(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	msgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"A": {{uid: 1, msgID: "a@x"}},
		"B": {{uid: 1, msgID: "b@x"}},
		"C": {{uid: 1, msgID: "c@x"}},
	}
	mailboxes := []string{"A", "B", "C"}

	// src is slow (250ms per folder) so it would take >750ms to finish all 3.
	srcSrv.addConnHandler(slowExamineHandler(srcSrv, mailboxes, msgs, 250*time.Millisecond))
	// dst: A succeeds, B returns BAD, C would succeed but we never reach it.
	dstSrv.addConnHandler(func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] fake ready\r\n")
		sc := bufio.NewScanner(conn)
		callCount := 0
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
			dstSrv.mu.Lock()
			dstSrv.counts[verb]++
			dstSrv.mu.Unlock()
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				for _, mb := range mailboxes {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "EXAMINE", "SELECT":
				callCount++
				if callCount == 2 {
					_, _ = fmt.Fprintf(conn, "%s BAD internal error\r\n", tag)
					continue
				}
				folder := strings.Trim(arg, `"`)
				_, _ = fmt.Fprintf(conn, "* 0 EXISTS\r\n* 0 RECENT\r\n")
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, folder)
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
	})

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{
		{Source: "A", Destination: "A"},
		{Source: "B", Destination: "B"},
		{Source: "C", Destination: "C"},
	}

	start := time.Now()
	_, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from dst BAD, got nil")
	}
	if !strings.Contains(err.Error(), "scan destination folder") {
		t.Errorf("error %q does not contain %q", err.Error(), "scan destination folder")
	}
	// Src would need ~750ms for all 3 folders; dst error on folder 2 must
	// cancel src and return well before src finishes folder 3.
	if elapsed > 600*time.Millisecond {
		t.Errorf("buildSyncPlan took %v, want <600ms (dst error must cancel src promptly)", elapsed)
	}
}

// Test_buildSyncPlan_cancelHaltsBothSides asserts that canceling the context
// while both goroutines are blocked causes buildSyncPlan to return promptly
// with context.Canceled, and that no goroutines are leaked.
func Test_buildSyncPlan_cancelHaltsBothSides(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	msgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"INBOX": {{uid: 1, msgID: "a@x"}},
	}
	mailboxes := []string{"INBOX"}

	// Both sides sleep 300ms per EXAMINE — long enough that we can cancel mid-flight.
	srcSrv.addConnHandler(slowExamineHandler(srcSrv, mailboxes, msgs, 300*time.Millisecond))
	dstSrv.addConnHandler(slowExamineHandler(dstSrv, mailboxes, msgs, 300*time.Millisecond))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	ctx, cancel := context.WithCancel(context.Background())

	baseline := runtime.NumGoroutine()

	done := make(chan error, 1)
	go func() {
		done <- func() error {
			_, err := buildSyncPlan(ctx, srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
			return err
		}()
	}()

	// Cancel while both goroutines are sleeping inside EXAMINE.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error after cancel, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("buildSyncPlan did not return within 500ms after cancel")
	}

	// Goroutines spawned by buildSyncPlan must have exited.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine count %d exceeds baseline %d by more than 2 after cancel", got, baseline)
	}
}

// Test_buildSyncPlan_orderPreserved asserts that summary.Plans preserves the
// input mappings order even when dst folders complete in reverse order.
func Test_buildSyncPlan_orderPreserved(t *testing.T) {
	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	msgs := map[string][]struct {
		msgID string
		uid   uint32
	}{
		"A": {{uid: 1, msgID: "a@x"}},
		"B": {{uid: 2, msgID: "b@x"}},
		"C": {{uid: 3, msgID: "c@x"}},
	}

	// src is fast; dst uses a counter-based delay: first call 300ms, second 200ms, third 100ms.
	// Completions arrive in order [C, B, A] but plans must be returned as [A, B, C].
	srcSrv.addConnHandler(msgIDFetchHandler(srcSrv, []string{"A", "B", "C"}, msgs))

	var dstExamineCall atomic.Int32
	dstSrv.addConnHandler(func(conn net.Conn) {
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
			dstSrv.mu.Lock()
			dstSrv.counts[verb]++
			dstSrv.mu.Unlock()
			switch verb {
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case "LIST":
				for _, mb := range []string{"A", "B", "C"} {
					_, _ = fmt.Fprintf(conn, "* LIST (\\HasNoChildren) \"/\" %s\r\n", mb)
				}
				_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "EXAMINE", "SELECT":
				// First call sleeps 300ms, second 200ms, third 100ms → completes C, B, A.
				call := dstExamineCall.Add(1)
				delay := time.Duration(4-call) * 100 * time.Millisecond
				time.Sleep(delay)
				selectedFolder = strings.Trim(arg, `"`)
				_, _ = fmt.Fprintf(conn, "* 0 EXISTS\r\n* 0 RECENT\r\n")
				_, _ = fmt.Fprintf(conn, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-ONLY] %s completed\r\n", tag, selectedFolder)
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
	})

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	pw, srcTr, dstTr := makePlanPW()
	mappings := []config.DirectoryMapping{
		{Source: "A", Destination: "A"},
		{Source: "B", Destination: "B"},
		{Source: "C", Destination: "C"},
	}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, pw, "src", "dst", false)
	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 3 {
		t.Fatalf("Plans=%d, want 3", len(summary.Plans))
	}
	for i, m := range mappings {
		if summary.Plans[i].SourceFolder != m.Source {
			t.Errorf("Plans[%d].SourceFolder=%q, want %q", i, summary.Plans[i].SourceFolder, m.Source)
		}
	}
}
