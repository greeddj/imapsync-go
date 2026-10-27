package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"github.com/urfave/cli/v3"
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
	// A non-nil limiter signals "user already configured throttle" - the
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

func makePlanTrackers() (srcTr, dstTr *progress.Tracker) {
	return progress.NewTracker("src", 10), progress.NewTracker("dst", 10)
}

// Test_buildSyncPlan_emptySrc_returnsNoPlans asserts the "already in sync"
// short-circuit: when src has zero messages, the plan list is empty.
func Test_buildSyncPlan_emptySrc_returnsNoPlans(t *testing.T) {
	t.Parallel()

	srcSrv := newFakeServer(t)
	dstSrv := newFakeServer(t)

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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
// has a@x, b@x, c@x at UIDs 1-3; dst has only b@x → plan must contain UIDs
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	_, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "scan destination folder") {
		t.Errorf("error %q does not contain %q", err.Error(), "scan destination folder")
	}
}

func Test_buildSyncPlan_When_SourceScanFails_Then_RecordsFailureAndKeepsOtherPlans(t *testing.T) {
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{
		{Source: "Missing", Destination: "Missing"},
		{Source: "INBOX", Destination: "INBOX"},
	}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
	if err != nil {
		t.Fatalf("buildSyncPlan returned error: %v", err)
	}
	if len(summary.Plans) != 1 || summary.Plans[0].SourceFolder != "INBOX" {
		t.Fatalf("Plans = %+v, want only INBOX", summary.Plans)
	}
	if summary.TotalNew != 1 {
		t.Errorf("TotalNew = %d, want 1", summary.TotalNew)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly one", summary.Failures)
	}
	f := summary.Failures[0]
	if f.SourceFolder != "Missing" || f.DestinationFolder != "Missing" {
		t.Errorf("failure names = %q -> %q, want Missing -> Missing", f.SourceFolder, f.DestinationFolder)
	}
	if !errors.Is(f.Err, errSourceScan) {
		t.Errorf("failure error = %v, want errSourceScan", f.Err)
	}
	if !strings.Contains(f.Err.Error(), "cannot select folder Missing") {
		t.Errorf("failure error = %q, want the select error", f.Err)
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
	got1, err := expandMappingsWithSubfolders(context.Background(), io.Discard, srcC, mappings1, delimiter, delimiter, false, true)
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
	got2, err := expandMappingsWithSubfolders(context.Background(), io.Discard, srcC, mappings2, delimiter, delimiter, false, true)
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
// path in expandMappingsWithSubfolders produces the correct deduplication
// result and writes the found-subfolder and skipped-mapping lines.
func Test_expandMappingsWithSubfolders_verboseLogging(t *testing.T) {
	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"INBOX", "INBOX/Sub"}))

	srcC := newAppClient(t, srcSrv, "src")
	delimiter := srcC.GetDelimiter()

	// Explicit INBOX/Sub mapping will be deduped (expansion wins). The verbose +
	// !quiet path writes the found-subfolder and dropped-mapping lines.
	mappings := []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
		{Source: "INBOX/Sub", Destination: "INBOX/Sub"},
	}
	var out strings.Builder
	got, err := expandMappingsWithSubfolders(context.Background(), &out, srcC, mappings, delimiter, delimiter, true, false)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (INBOX + INBOX/Sub), got %v", len(got), got)
	}
	if want := "Found subfolder: INBOX/Sub"; !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not contain %q", out.String(), want)
	}
	if want := "Mapping \"INBOX/Sub\" skipped - already covered by a parent mapping's expansion"; !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not contain %q", out.String(), want)
	}
}

// Test_expandMappingsWithSubfolders_canceledContext asserts that a pre-canceled
// context causes expandMappingsWithSubfolders to return an error immediately.
func Test_expandMappingsWithSubfolders_canceledContext(t *testing.T) {
	srcSrv := newFakeServer(t)
	srcC := newAppClient(t, srcSrv, "src")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := expandMappingsWithSubfolders(ctx, io.Discard, srcC, []config.DirectoryMapping{
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

	srcTr, dstTr := makePlanTrackers()
	_, err := buildSyncPlan(ctx, srcC, dstC, []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
	}, srcTr, dstTr, "src", "dst")
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
}

func Test_expandMappingsWithSubfolders_When_DelimiterIsEmpty_Then_FlatSideHasNoHierarchy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		srcDelimiter string
		dstDelimiter string
		want         []config.DirectoryMapping
	}{
		{
			name: "flat source gets no subfolders",
			want: []config.DirectoryMapping{mapping("INBOX", "TARGET")},
		},
		{
			name:         "flat destination keeps the source relative name",
			srcDelimiter: "/",
			want:         []config.DirectoryMapping{mapping("INBOX", "TARGET"), mapping("INBOX/Sub", "TARGET/Sub")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcSrv := newFakeServer(t)
			srcSrv.addConnHandler(customListHandler(srcSrv, []string{"INBOX", "INBOX/Sub"}))
			srcC := newAppClient(t, srcSrv, "src")

			got, err := expandMappingsWithSubfolders(t.Context(), io.Discard, srcC, []config.DirectoryMapping{mapping("INBOX", "TARGET")}, tt.srcDelimiter, tt.dstDelimiter, false, true)

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("mappings = %v, want %v", got, tt.want)
			}
		})
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
	got, err := expandMappingsWithSubfolders(context.Background(), io.Discard, srcC, mappings, "/", ".", false, true)
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
		var totalNewSize atomic.Uint64

		s := &folderScan{
			srcMap:         map[string]uint32{"a@x": 1, "b@x": 2},
			dstIDs:         map[string]struct{}{"b@x": {}},
			srcFolderSize:  2000, // 2 messages summed to 2000 bytes
			srcFolderCount: 2,
		}

		// First call: only one side arrived - must not diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}
		if s.srcMap == nil || s.dstIDs == nil {
			t.Fatal("maps freed prematurely after first call")
		}

		// Second call: both sides arrived - diff fires.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
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
		// 1 new of 2 total at 2000 bytes → 1000 bytes estimated.
		if got := totalNewSize.Load(); got != 1000 {
			t.Errorf("totalNewSize=%d, want 1000 (2000 × 1/2)", got)
		}
		if plans[0].NewSize != 1000 {
			t.Errorf("plans[0].NewSize=%d, want 1000", plans[0].NewSize)
		}
	})

	t.Run("srcErr_freesMaps", func(t *testing.T) {
		t.Parallel()
		mappings := []config.DirectoryMapping{
			{Source: "INBOX", Destination: "INBOX"},
		}
		plans := make([]FolderSyncPlan, 1)
		var totalNew atomic.Int64
		var totalNewSize atomic.Uint64

		s := &folderScan{
			srcErr: fmt.Errorf("src scan failed"),
			srcMap: map[string]uint32{"a@x": 1},
			dstIDs: map[string]struct{}{"b@x": {}},
		}

		// First call: one side arrived - no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived - error branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
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
		var totalNewSize atomic.Uint64

		s := &folderScan{
			dstErr: fmt.Errorf("dst scan failed"),
			srcMap: map[string]uint32{"a@x": 1},
			dstIDs: map[string]struct{}{"b@x": {}},
		}

		// First call: one side arrived - no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived - dstErr branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{
		{Source: "INBOX", Destination: "INBOX"},
		{Source: "Drafts", Destination: "Drafts"},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{
		{Source: "A", Destination: "A"},
		{Source: "B", Destination: "B"},
		{Source: "C", Destination: "C"},
	}

	start := time.Now()
	_, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

	// Both sides sleep 300ms per EXAMINE - long enough that we can cancel mid-flight.
	srcSrv.addConnHandler(slowExamineHandler(srcSrv, mailboxes, msgs, 300*time.Millisecond))
	dstSrv.addConnHandler(slowExamineHandler(dstSrv, mailboxes, msgs, 300*time.Millisecond))

	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{{Source: "INBOX", Destination: "INBOX"}}

	ctx, cancel := context.WithCancel(context.Background())

	baseline := runtime.NumGoroutine()

	done := make(chan error, 1)
	go func() {
		done <- func() error {
			_, err := buildSyncPlan(ctx, srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{
		{Source: "A", Destination: "A"},
		{Source: "B", Destination: "B"},
		{Source: "C", Destination: "C"},
	}

	summary, err := buildSyncPlan(context.Background(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
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

// --- runSync and ActionSync end to end, against store-backed servers ---

const (
	e2eTimeout  = 10 * time.Second
	e2ePassword = "hunter2-secret"
)

// e2eContext bounds a test run so a protocol bug fails fast instead of hanging.
func e2eContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	t.Cleanup(cancel)
	return ctx
}

func e2eConfig(src, dst *fakeServer, workers int, mappings ...config.DirectoryMapping) *config.Config {
	return &config.Config{
		Src:     config.Credentials{Label: "src", Server: src.ln.Addr().String(), User: "alice", Pass: e2ePassword},
		Dst:     config.Credentials{Label: "dst", Server: dst.ln.Addr().String(), User: "bob", Pass: e2ePassword},
		Map:     mappings,
		Workers: workers,
	}
}

// e2eOptions aims runSync at plain-TCP servers. input feeds the prompts; an
// empty input makes any unexpected prompt fail with EOF instead of hanging.
func e2eOptions(out io.Writer, input string) syncOptions {
	return syncOptions{
		out:        out,
		in:         strings.NewReader(input),
		clientOpts: client.Options{UseTLS: false, DialTimeout: 5 * time.Second},
	}
}

func mapping(src, dst string) config.DirectoryMapping {
	return config.DirectoryMapping{Source: src, Destination: dst}
}

func fakeMsgs(ids ...string) []fakeMsg {
	msgs := make([]fakeMsg, len(ids))
	for i, id := range ids {
		msgs[i] = fakeMsg{msgID: id}
	}
	return msgs
}

func assertContainsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q\noutput: %q", want, got)
		}
	}
}

func assertContainsNone(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q\noutput: %q", bad, got)
		}
	}
}

func assertIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// assertReadOnly checks that the server never saw a command that changes mail
// or flags, and never opened a mailbox read-write.
func assertReadOnly(t *testing.T, name string, srv *fakeServer) {
	t.Helper()
	for _, verb := range []string{"APPEND", "CREATE", "STORE", "UID STORE", "EXPUNGE", "DELETE", "RENAME", "SELECT"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("%s saw %d %s commands, want 0", name, n, verb)
		}
	}
	if n := srv.seenMutatingFetches(); n != 0 {
		t.Errorf("%s saw %d fetches that set \\Seen, want 0", name, n)
	}
}

func assertNoLogin(t *testing.T, servers ...*fakeServer) {
	t.Helper()
	for i, srv := range servers {
		if n := srv.callCount("LOGIN"); n != 0 {
			t.Errorf("server %d saw %d LOGIN commands, want 0", i, n)
		}
	}
}

func TestRunSync_When_AutoConfirmed_Then_CopiesMissingMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rateLimit config.RateLimit
	}{
		{name: "without rate limits"},
		{name: "with shared rate limiters", rateLimit: config.RateLimit{DownBPS: 50_000_000, UpBPS: 50_000_000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": fakeMsgs("m1@x", "m2@x"),
				"Work":  fakeMsgs("m3@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			cfg := e2eConfig(src, dst, 2, mapping("INBOX", "INBOX"), mapping("Work", "Work"))
			cfg.RateLimit = tt.rateLimit
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m3@x")
			assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), "m2@x")
			assertIDs(t, "dst APPENDs to Work", dst.appended("Work"), "m3@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work")
			assertContainsAll(t, out.String(),
				"Messages to be copied to destination",
				"Folders to be created on destination",
				"Total new messages to sync: 2",
				"Sync completed successfully",
			)
			assertContainsNone(t, out.String(), e2ePassword)
			assertReadOnly(t, "source", src)
			if n := dst.callCount("SELECT"); n != 0 {
				t.Errorf("destination saw %d read-write SELECTs, want 0", n)
			}
			if n := dst.seenMutatingFetches(); n != 0 {
				t.Errorf("destination saw %d fetches that set Seen, want 0", n)
			}
		})
	}
}

func TestRunSync_When_SourceDropsOnFolderNamedLikeThrottle_Then_FolderIsStillCopied(t *testing.T) {
	t.Parallel()

	const folder = "Alerts/Account Lockout"
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, folder: fakeMsgs("lock1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping(folder, folder))
	src.dropOnceOnOpen(folder)
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	if n := src.callCount("DROP"); n != 1 {
		t.Errorf("source dropped %d connections, want 1", n)
	}
	assertIDs(t, "dst APPENDs to "+folder, dst.appended(folder), "lock1@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_RunTwice_Then_SecondRunWritesNothing(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x", "m2@x"),
		"Work":  fakeMsgs("m3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	cfg := e2eConfig(src, dst, 2, mapping("INBOX", "INBOX"), mapping("Work", "Work"))

	first := e2eOptions(&syncBuffer{}, "")
	first.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, first); err != nil {
		t.Fatalf("first runSync: %v", err)
	}
	appends, creates := dst.callCount("APPEND"), dst.callCount("CREATE")
	if appends != 2 || creates != 1 {
		t.Fatalf("first run did %d APPEND and %d CREATE, want 2 and 1", appends, creates)
	}

	out := &syncBuffer{}
	second := e2eOptions(out, "")
	second.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, second); err != nil {
		t.Fatalf("second runSync: %v", err)
	}

	assertContainsAll(t, out.String(), "All folders already synced!")
	if n := dst.callCount("APPEND"); n != appends {
		t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
	}
	if n := dst.callCount("CREATE"); n != creates {
		t.Errorf("CREATE count went from %d to %d on the second run", creates, n)
	}
}

func TestRunSync_When_ConfirmationAnswered_Then_WritesOnlyAfterYes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantErrIs  error
		wantOutput []string
		wantCopied bool
		wantErr    bool
	}{
		{
			name:       "declined",
			input:      "n\n",
			wantOutput: []string{"Proceed with synchronization? [y/N]", "Sync canceled by user"},
		},
		{
			name:       "accepted",
			input:      "y\n",
			wantOutput: []string{"Proceed with synchronization? [y/N]", "Sync completed successfully"},
			wantCopied: true,
		},
		{
			name:       "input ends before an answer",
			input:      "",
			wantOutput: []string{"Proceed with synchronization? [y/N]"},
			wantErr:    true,
			wantErrIs:  io.EOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
			out := &syncBuffer{}

			err := runSync(e2eContext(t), cfg, e2eOptions(out, tt.input))

			switch {
			case tt.wantErr && err == nil:
				t.Fatal("runSync returned nil, want an error")
			case !tt.wantErr && err != nil:
				t.Fatalf("runSync: %v", err)
			case tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs):
				t.Errorf("error %v does not wrap %v", err, tt.wantErrIs)
			}
			assertContainsAll(t, out.String(), tt.wantOutput...)

			if tt.wantCopied {
				assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
				return
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands before a yes, want 0", n)
			}
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("dst saw %d CREATE commands before a yes, want 0", n)
			}
		})
	}
}

func TestRunSync_When_DelimitersDiffer_Then_PathIsRewrittenAfterConfirmation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantOutput  []string
		notOutput   []string
		wantFolder  string
		wantCreates []string
		autoConfirm bool
	}{
		{
			name:        "auto confirm rewrites the destination path",
			autoConfirm: true,
			wantOutput:  []string{"Folder path delimiter mismatch detected", "Auto-confirming delimiter fix", "Fixed destination"},
			wantFolder:  "Archive.2024",
			wantCreates: []string{"Archive", "Archive.2024"},
		},
		{
			name:  "both prompts are answered from one input stream",
			input: "y\ny\n",
			wantOutput: []string{
				"Fix folder delimiters to match server configuration? [y/N]",
				"Proceed with synchronization? [y/N]",
				"Fixed destination",
			},
			wantFolder:  "Archive.2024",
			wantCreates: []string{"Archive", "Archive.2024"},
		},
		{
			name:        "declined fix keeps the path and warns",
			input:       "n\ny\n",
			wantOutput:  []string{"Please note if delimiter", "Proceed with synchronization? [y/N]"},
			notOutput:   []string{"Fixed destination"},
			wantFolder:  "Archive/2024",
			wantCreates: []string{"Archive/2024"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": fakeMsgs("m1@x")})
			// The destination needs a folder to report its "." delimiter.
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("Archive/2024", "Archive/2024"))
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.autoConfirm = tt.autoConfirm

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertContainsAll(t, out.String(), tt.wantOutput...)
			assertContainsNone(t, out.String(), tt.notOutput...)
			assertIDs(t, "dst "+tt.wantFolder, dst.folderMsgIDs(tt.wantFolder), "m1@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
		})
	}
}

func TestRunSync_When_InputEndsAtDelimiterPrompt_Then_ReturnsReadErrorBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": fakeMsgs("m1@x")})
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Archive/2024", "Archive/2024"))
	out := &syncBuffer{}

	err := runSync(e2eContext(t), cfg, e2eOptions(out, ""))

	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want it to wrap io.EOF", err)
	}
	assertContainsAll(t, out.String(), "Fix folder delimiters to match server configuration? [y/N]")
	if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d CREATE or APPEND commands, want 0", n)
	}
}

func TestRunSync_When_OnlyOneFolderFlagGiven_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		srcFolder string
		dstFolder string
	}{
		{name: "source folder without destination folder", srcFolder: "INBOX"},
		{name: "destination folder without source folder", dstFolder: "INBOX"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
			opts := e2eOptions(&syncBuffer{}, "")
			opts.autoConfirm = true
			opts.srcFolder, opts.dstFolder = tt.srcFolder, tt.dstFolder

			err := runSync(e2eContext(t), cfg, opts)

			if err == nil || !strings.Contains(err.Error(), "both --src-folder and --dest-folder must be specified") {
				t.Fatalf("error = %v, want the folder-pairing refusal", err)
			}
			assertNoLogin(t, src, dst)
		})
	}
}

func TestRunSync_When_BothFolderFlagsGiven_Then_TheyReplaceConfiguredMappings(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Work":  fakeMsgs("m2@x"),
		"Extra": fakeMsgs("m3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"), mapping("Extra", "Extra"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true
	opts.srcFolder, opts.dstFolder = "Work", "Archive"

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "m2@x")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"))
	if dst.hasFolder("Extra") {
		t.Error("dst has Extra, a folder only the configured mappings named")
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Archive")
}

func TestRunSync_When_NoMappingsConfigured_Then_EverySourceFolderIsSynced(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Sent":  fakeMsgs("m2@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 2)
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	assertIDs(t, "dst Sent", dst.folderMsgIDs("Sent"), "m2@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Sent")
	assertReadOnly(t, "source", src)

	// Folder names come from the planning session's cache: the source opens no
	// session the destination does not, sends no STATUS, and closes them all.
	srcLogins, dstLogins := src.callCount("LOGIN"), dst.callCount("LOGIN")
	if srcLogins != dstLogins {
		t.Errorf("source LOGIN count = %d, destination = %d, want equal", srcLogins, dstLogins)
	}
	if got := src.callCount("LOGOUT"); got != srcLogins {
		t.Errorf("source LOGOUT count = %d, want %d (one per session)", got, srcLogins)
	}
	if got := src.callCount("STATUS"); got != 0 {
		t.Errorf("source STATUS count = %d, want 0", got)
	}
}

func TestRunSync_When_SourceHasSubfolders_Then_TheyAreCopiedToMatchingPaths(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Work":       fakeMsgs("m1@x"),
		"Work/Sub":   fakeMsgs("m2@x"),
		"Other":      fakeMsgs("m3@x"),
		"Work/Sub/A": fakeMsgs("m4@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 2, mapping("Work", "Mail"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst Mail", dst.folderMsgIDs("Mail"), "m1@x")
	assertIDs(t, "dst Mail/Sub", dst.folderMsgIDs("Mail/Sub"), "m2@x")
	assertIDs(t, "dst Mail/Sub/A", dst.folderMsgIDs("Mail/Sub/A"), "m4@x")
	if dst.hasFolder("Other") {
		t.Error("dst has Other, which no mapping covers")
	}
}

func TestRunSync_When_FolderCreationRefused_Then_FailsBeforeAnyAppend(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreate()
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if err == nil || !strings.Contains(err.Error(), "failed to create 1 folders") {
		t.Fatalf("error = %v, want %q", err, "failed to create 1 folders")
	}
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
}

func TestRunSync_When_CreateAnswersDuplicateFolderName_Then_SyncCompletes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	// Another client creates Work after the planning LIST, so Gmail refuses our CREATE.
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb != "CREATE" {
			return "", false
		}
		dst.addFolder("Work")
		return "NO [ALREADYEXISTS] Duplicate folder name Work (Failure)", true
	}))
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true
	opts.verbose = true

	err := runSync(e2eContext(t), cfg, opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work")
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
	assertContainsAll(t, out.String(), "Folder Work already exists")
	assertContainsNone(t, out.String(), "Failed to create folder", "failed to create")
	// One LIST per connected client: the planning client and the single worker.
	// The duplicate-folder reply must not make either of them list again.
	if n := dst.callCount("LIST"); n != 2 {
		t.Errorf("dst LIST count = %d, want 2", n)
	}
}

func TestRunSync_When_AppendRefused_Then_ReportsErrorsAndExitsSilently(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseAppend()
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), "Sync completed with errors", "0 messages uploaded, 1 errors occurred")
}

func TestRunSync_When_DestinationRefusesLoginMidRun_Then_WorkerSendsOneRefusedLogin(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x", "m3@x", "m4@x", "m5@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	// The first dst connection plans; the second is the only worker's.
	dst.addConnHandler(dst.serveStore)
	dst.addConnHandler(refuseLoginAfterAppendHandler(dst))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Errorf("error = %v, want ErrSilentExit", err)
	}
	if n := dst.callCount("LOGIN"); n != 3 {
		t.Errorf("dst saw %d LOGIN commands, want 3 (planning, worker and one refused repair)", n)
	}
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("dst saw %d APPEND commands, want 1", n)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"))
}

func TestRunSync_When_LoginRefused_Then_NamesTheFailingSide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		want      string
		mappings  []config.DirectoryMapping
		refuseSrc bool
		refuseDst bool
	}{
		{
			name:      "source login refused",
			refuseSrc: true,
			mappings:  []config.DirectoryMapping{mapping("INBOX", "INBOX")},
			want:      "source connection failed",
		},
		{
			name:      "destination login refused",
			refuseDst: true,
			mappings:  []config.DirectoryMapping{mapping("INBOX", "INBOX")},
			want:      "destination connection failed",
		},
		{
			name:      "source login refused while mappings come from the source",
			refuseSrc: true,
			want:      "source connection failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			if tt.refuseSrc {
				src.refuseLogin()
			}
			if tt.refuseDst {
				dst.refuseLogin()
			}
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			opts := e2eOptions(&syncBuffer{}, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), cfg, opts)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), e2ePassword) {
				t.Errorf("error %q leaks the password", err)
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
		})
	}
}

func TestRunSync_When_QuietWithoutConfirm_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "with configured mappings", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
		{name: "with mappings built from the source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.quiet = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if !errors.Is(err, errQuietWithoutConfirm) {
				t.Fatalf("error = %v, want errQuietWithoutConfirm", err)
			}
			assertContainsAll(t, err.Error(), "--quiet", "--confirm")
			if got := out.String(); got != "" {
				t.Errorf("output = %q, want nothing written", got)
			}
			assertNoLogin(t, src, dst)
		})
	}
}

func TestRunSync_When_QuietAndConfirmed_Then_CopiesWithoutPreview(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
	assertContainsNone(t, out.String(), "Messages to be copied to destination", "Total new messages to sync")
}

func TestRunSync_When_Verbose_Then_PreviewSamplesUIDsAndShowsDelimiters(t *testing.T) {
	t.Parallel()

	const total = verboseUIDLimit + 5
	ids := make([]string, total)
	for i := range ids {
		ids[i] = "m" + strconv.Itoa(i+1) + "@x"
	}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Big": fakeMsgs(ids...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Big": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Big", "Big"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.verbose, opts.autoConfirm = true, true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	got := out.String()
	if n := strings.Count(got, "  \u2022 UID "); n != verboseUIDLimit {
		t.Errorf("preview lists %d UIDs, want %d", n, verboseUIDLimit)
	}
	assertContainsAll(t, got,
		"and 5 more",
		"Starting sync with 1 workers",
		"Server delimiters",
		`Source [src]: "/"`,
		`Destination [dst]: "/"`,
	)
	if n := len(dst.folderMsgIDs("Big")); n != total {
		t.Errorf("dst Big holds %d messages, want %d", n, total)
	}
}

func TestRunSync_When_ContextAlreadyCanceled_Then_ReturnsBeforeConnecting(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertNoLogin(t, src, dst)
}

// syncFlags mirrors the flag names ActionSync reads; the aliases are the ones
// the tests pass.
func syncFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "src-folder", Aliases: []string{"s"}},
		&cli.StringFlag{Name: "dest-folder", Aliases: []string{"d"}},
		&cli.IntFlag{Name: "workers", Aliases: []string{"w"}, Value: 4},
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"V"}},
		&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}},
		&cli.BoolFlag{Name: "confirm", Aliases: []string{"y", "yes"}},
		&cli.IntFlag{Name: "bps-down"},
		&cli.IntFlag{Name: "bps-up"},
		&cli.IntFlag{Name: "max-connections"},
	}
}

// runCLIAction runs action as subcommand name under a root command that owns
// the config flag, the same shape as cmd/imapsync-go/main.go, and returns the
// action's own error.
func runCLIAction(t *testing.T, action func(context.Context, *cli.Command) error, name string, flags []cli.Flag, args ...string) error {
	t.Helper()

	var got error
	root := &cli.Command{
		Name:            "imapsync-go",
		HideHelpCommand: true,
		Flags:           []cli.Flag{&cli.StringFlag{Name: "config", Aliases: []string{"c"}}},
		Commands: []*cli.Command{{
			Name:   name,
			Flags:  flags,
			Action: func(ctx context.Context, c *cli.Command) error { got = action(ctx, c); return nil },
		}},
	}
	if err := root.Run(e2eContext(t), append([]string{"imapsync-go"}, args...)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got
}

// writeJSONConfig stores a config for the given servers in a temp dir and
// returns its path.
func writeJSONConfig(t *testing.T, src, dst *fakeServer, mappings ...config.DirectoryMapping) string {
	t.Helper()

	cfg := e2eConfig(src, dst, 1, mappings...)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestActionSync_When_ServerIsNotTLS_Then_FailsWithoutSendingCredentials(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	cfgPath := writeJSONConfig(t, src, dst, mapping("INBOX", "INBOX"))

	err := runCLIAction(t, ActionSync, "sync", syncFlags(), "-c", cfgPath, "sync", "-y")

	if err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("error = %v, want a TLS handshake failure", err)
	}
	assertNoLogin(t, src, dst)
}

func TestActionSync_When_ConfigIsMissing_Then_ReportsLoadFailure(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.json")

	err := runCLIAction(t, ActionSync, "sync", syncFlags(), "-c", missing, "sync", "-y")

	if err == nil || !strings.Contains(err.Error(), "failed to load config") {
		t.Fatalf("error = %v, want %q", err, "failed to load config")
	}
}

// Unlike writeJSONConfig, the config that writeRawConfig writes can hold a key that config.Config does not define.
func writeRawConfig(t *testing.T, ext string, src, dst *fakeServer, extra string) string {
	t.Helper()

	var content string
	switch ext {
	case ".json":
		content = fmt.Sprintf(
			`{"src":{"server":%q,"user":"alice","pass":%q},"dst":{"server":%q,"user":"bob","pass":%q}%s}`,
			src.ln.Addr().String(), e2ePassword, dst.ln.Addr().String(), e2ePassword, extra)
	default:
		content = fmt.Sprintf(
			"src: {server: %q, user: alice, pass: %q}\ndst: {server: %q, user: bob, pass: %q}\n%s",
			src.ln.Addr().String(), e2ePassword, dst.ln.Addr().String(), e2ePassword, extra)
	}
	path := filepath.Join(t.TempDir(), "config"+ext)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func watchDials(srv *fakeServer) *atomic.Bool {
	var dialed atomic.Bool
	srv.addConnHandler(func(conn net.Conn) {
		dialed.Store(true)
		_ = conn.Close()
	})
	return &dialed
}

func TestActionSync_When_ConfigHasUnknownKey_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ext   string
		extra string
	}{
		{
			name: "JSON misspelled map and rate_limit", ext: ".json",
			extra: `,"maps":[{"src":"INBOX","dst":"INBOX"}],"rate-limit":{"down_bps":300000}`,
		},
		{
			name: "YAML misspelled map and rate_limit", ext: ".yaml",
			extra: "maps:\n  - src: INBOX\n    dst: INBOX\nratelimit:\n  down_bps: 300000\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := newFakeServer(t), newFakeServer(t)
			srcDialed, dstDialed := watchDials(src), watchDials(dst)
			cfgPath := writeRawConfig(t, tt.ext, src, dst, tt.extra)

			err := runCLIAction(t, ActionSync, "sync", syncFlags(), "-c", cfgPath, "sync", "-y")

			if err == nil {
				t.Fatal("error = nil, want a config load failure")
			}
			if !strings.Contains(err.Error(), "failed to load config") || !strings.Contains(err.Error(), cfgPath) {
				t.Errorf("error = %v, want %q and the config path", err, "failed to load config")
			}
			if msg := strings.ReplaceAll(err.Error(), cfgPath, ""); !strings.Contains(msg, "maps") {
				t.Errorf("error = %q, want the key %q", msg, "maps")
			}
			if srcDialed.Load() || dstDialed.Load() {
				t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
			}
			assertNoLogin(t, src, dst)
		})
	}
}

func writeAuthConfig(t *testing.T, ext string, src, dst *fakeServer, side, value string) string {
	t.Helper()

	authFor := func(forSide string) string {
		switch {
		case forSide != side:
			return ""
		case ext == ".json":
			return fmt.Sprintf(`,"auth":%q`, value)
		default:
			return fmt.Sprintf("  auth: %q\n", value)
		}
	}

	var content string
	if ext == ".json" {
		content = fmt.Sprintf(
			`{"src":{"server":%q,"user":"alice","pass":%q%s},"dst":{"server":%q,"user":"bob","pass":%q%s}}`,
			src.ln.Addr().String(), e2ePassword, authFor("src"), dst.ln.Addr().String(), e2ePassword, authFor("dst"))
	} else {
		content = fmt.Sprintf(
			"src:\n  server: %q\n  user: alice\n  pass: %q\n%sdst:\n  server: %q\n  user: bob\n  pass: %q\n%s",
			src.ln.Addr().String(), e2ePassword, authFor("src"), dst.ln.Addr().String(), e2ePassword, authFor("dst"))
	}
	path := filepath.Join(t.TempDir(), "config"+ext)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestActionSync_When_AuthIsUnsupported_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ext   string
		side  string
		value string
	}{
		{name: "JSON destination", ext: ".json", side: "dst", value: "xoauth2"},
		{name: "YAML source", ext: ".yaml", side: "src", value: "cram_md5"},
		{name: "YML destination with a value shaped like an app password", ext: ".yml", side: "dst", value: "abcdefghijklmnop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := newFakeServer(t), newFakeServer(t)
			srcDialed, dstDialed := watchDials(src), watchDials(dst)
			cfgPath := writeAuthConfig(t, tt.ext, src, dst, tt.side, tt.value)

			err := runCLIAction(t, ActionSync, "sync", syncFlags(), "-c", cfgPath, "sync", "-y")

			if !errors.Is(err, config.ErrAuthUnsupported) {
				t.Fatalf("error = %v, want %v", err, config.ErrAuthUnsupported)
			}
			for _, want := range []string{"failed to load config", tt.side + ": "} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
			for _, unwanted := range []string{strconv.Quote(tt.value), tt.value} {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("error = %q, want no value %s", err, unwanted)
				}
			}
			if strings.Contains(err.Error(), e2ePassword) {
				t.Errorf("error = %q, want no password", err)
			}
			if srcDialed.Load() || dstDialed.Load() {
				t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
			}
			assertNoLogin(t, src, dst)
		})
	}
}

func TestActionSync_When_MapEntryHasNoFolderName_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr   error
		name      string
		ext       string
		extra     string
		args      []string
		wantEntry int
	}{
		{
			name: "JSON absent dst", ext: ".json", args: []string{"-y"},
			extra: `,"map":[{"src":"INBOX"}]`, wantEntry: 1, wantErr: config.ErrMapDstRequired,
		},
		{
			name: "YAML empty src in second entry", ext: ".yaml", args: []string{"-y"},
			extra:     "map:\n  - src: INBOX\n    dst: INBOX\n  - src: \"\"\n    dst: Archive\n",
			wantEntry: 2, wantErr: config.ErrMapSrcRequired,
		},
		{
			name: "YAML null entry", ext: ".yaml", args: []string{"-y"},
			extra: "map:\n  - ~\n", wantEntry: 1, wantErr: config.ErrMapSrcRequired,
		},
		{
			name: "folder flags do not replace the file check", ext: ".json",
			args:  []string{"-y", "-s", "INBOX", "-d", "INBOX"},
			extra: `,"map":[{"src":"","dst":""}]`, wantEntry: 1, wantErr: config.ErrMapSrcRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := newFakeServer(t), newFakeServer(t)
			srcDialed, dstDialed := watchDials(src), watchDials(dst)
			cfgPath := writeRawConfig(t, tt.ext, src, dst, tt.extra)

			err := runCLIAction(t, ActionSync, "sync", syncFlags(), append([]string{"-c", cfgPath, "sync"}, tt.args...)...)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if want := fmt.Sprintf("map entry %d: ", tt.wantEntry); !strings.Contains(err.Error(), "failed to load config") ||
				!strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want %q and %q", err, "failed to load config", want)
			}
			if srcDialed.Load() || dstDialed.Load() {
				t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
			}
			assertNoLogin(t, src, dst)
		})
	}
}

func TestActionSync_When_QuietWithoutConfirm_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	cfgPath := writeJSONConfig(t, src, dst, mapping("INBOX", "INBOX"))

	err := runCLIAction(t, ActionSync, "sync", syncFlags(), "-c", cfgPath, "sync", "-q")

	if !errors.Is(err, errQuietWithoutConfirm) {
		t.Fatalf("error = %v, want errQuietWithoutConfirm", err)
	}
	assertNoLogin(t, src, dst)
}

func TestRunSync_When_SourceDelimiterDiffers_Then_SourcePathIsRewritten(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, ".", map[string][]fakeMsg{"Work.Sub": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Work/Sub", "Work/Sub"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertContainsAll(t, out.String(),
		`Source folder "Work/Sub" uses delimiter "/", server expects "."`,
		"Fixed source: \"Work/Sub\" \u2192 \"Work.Sub\"",
	)
	assertContainsNone(t, out.String(), "Fixed destination")
	assertIDs(t, "dst Work/Sub", dst.folderMsgIDs("Work/Sub"), "m1@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work", "Work/Sub")
	assertReadOnly(t, "source", src)
}

// cancelOnWrite cancels a context once marker is written, so a test can stop
// runSync at an exact point instead of racing it.
type cancelOnWrite struct {
	*syncBuffer
	cancel context.CancelFunc
	marker string
	once   sync.Once
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(string(p), w.marker) {
		w.once.Do(w.cancel)
	}
	return n, err
}

func TestRunSync_When_CanceledAtPhaseBoundary_Then_NothingMoreIsSent(t *testing.T) {
	t.Parallel()

	oneMessage := map[string][]fakeMsg{"Work": fakeMsgs("m1@x")}
	tests := []struct {
		src          map[string][]fakeMsg
		dst          map[string][]fakeMsg
		mapping      config.DirectoryMapping
		name         string
		marker       string
		input        string
		srcDelimiter string
		dstDelimiter string
		notOutput    []string
		autoConfirm  bool
		verbose      bool
		wantNoLogin  bool
	}{
		{
			name:         "before connecting",
			marker:       "Connecting to servers",
			verbose:      true,
			autoConfirm:  true,
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
			wantNoLogin:  true,
		},
		{
			name:         "before validating the mappings",
			marker:       "Server delimiters",
			verbose:      true,
			autoConfirm:  true,
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
		},
		{
			name:         "before expanding subfolders",
			marker:       "Checking for subfolders",
			verbose:      true,
			autoConfirm:  true,
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
		},
		{
			name:         "before the delimiter prompt",
			marker:       "delimiter mismatch detected",
			input:        "y\ny\n",
			src:          map[string][]fakeMsg{"Archive/2024": fakeMsgs("m1@x")},
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Archive/2024", "Archive/2024"),
			srcDelimiter: "/",
			dstDelimiter: ".",
			notOutput:    []string{"Fix folder delimiters"},
		},
		{
			name:         "before the confirmation prompt",
			marker:       "Total new messages to sync",
			input:        "y\n",
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
			notOutput:    []string{"Proceed with synchronization?"},
		},
		{
			name:         "before creating folders",
			marker:       "Creating destination folders",
			autoConfirm:  true,
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"INBOX": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
		},
		{
			name:         "before starting the workers",
			marker:       "Syncing messages",
			autoConfirm:  true,
			src:          oneMessage,
			dst:          map[string][]fakeMsg{"Work": nil},
			mapping:      mapping("Work", "Work"),
			srcDelimiter: "/",
			dstDelimiter: "/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.srcDelimiter, tt.src)
			dst := newStoreServer(t, tt.dstDelimiter, tt.dst)
			ctx, cancel := context.WithCancel(e2eContext(t))
			t.Cleanup(cancel)
			out := &cancelOnWrite{syncBuffer: &syncBuffer{}, cancel: cancel, marker: tt.marker}
			opts := e2eOptions(out, tt.input)
			opts.autoConfirm, opts.verbose = tt.autoConfirm, tt.verbose

			err := runSync(ctx, e2eConfig(src, dst, 1, tt.mapping), opts)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			assertContainsAll(t, out.String(), tt.marker)
			assertContainsNone(t, out.String(), tt.notOutput...)
			if tt.wantNoLogin {
				assertNoLogin(t, src, dst)
			}
			if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d CREATE or APPEND commands after the cancel, want 0", n)
			}
		})
	}
}

func TestRunSync_When_NestedFoldersAreNew_Then_EachPathIsCreatedOnceParentFirst(t *testing.T) {
	t.Parallel()

	const children = 12
	folders := map[string][]fakeMsg{"Work": fakeMsgs("w@x")}
	for i := range children {
		folders[fmt.Sprintf("Work/S%02d", i)] = fakeMsgs(fmt.Sprintf("s%02d@x", i))
	}
	src := newStoreServer(t, "/", folders)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 2, mapping("Work", "Mail"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	// Folders are created in sorted order: each parent before its children, and
	// no path twice.
	want := []string{"Mail"}
	for i := range children {
		want = append(want, fmt.Sprintf("Mail/S%02d", i))
	}
	if creates := dst.capturedNames("CREATE"); !slices.Equal(creates, want) {
		t.Errorf("CREATE names = %v, want %v", creates, want)
	}
	assertIDs(t, "dst Mail", dst.folderMsgIDs("Mail"), "w@x")
	for i := range children {
		assertIDs(t, "dst child", dst.folderMsgIDs(fmt.Sprintf("Mail/S%02d", i)), fmt.Sprintf("s%02d@x", i))
	}
	assertContainsAll(t, out.String(), "Found 12 subfolders, total folders to sync: 13")
}

func TestRunSync_When_ProviderIsKnown_Then_WarnsBeforeConnecting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		quiet       bool
		wantWarning bool
	}{
		{name: "normal output", wantWarning: true},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Src:     config.Credentials{Label: "src", Server: "imap.gmail.com:993", User: "alice", Pass: e2ePassword},
				Dst:     config.Credentials{Label: "dst", Server: "127.0.0.1:1", User: "bob", Pass: e2ePassword},
				Workers: 1,
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.clientOpts.DialTimeout = time.Nanosecond
			opts.autoConfirm, opts.quiet = true, tt.quiet
			// A lone --src-folder is refused before any dial. The nanosecond
			// timeout keeps the test offline even if that check moves after dialing.
			opts.srcFolder = "INBOX"

			err := runSync(e2eContext(t), cfg, opts)

			if err == nil || !strings.Contains(err.Error(), "both --src-folder and --dest-folder must be specified") {
				t.Fatalf("error = %v, want the folder-pairing refusal", err)
			}
			if !tt.wantWarning {
				if got := out.String(); got != "" {
					t.Errorf("output = %q, want nothing written", got)
				}
				return
			}
			assertContainsAll(t, out.String(), "Provider with known IMAP limits detected", "imap.gmail.com:993", "Gmail")
			assertContainsNone(t, out.String(), e2ePassword)
		})
	}
}

func TestActionSync_When_ContextAlreadyCanceled_Then_ReturnsBeforeReadingConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ActionSync(ctx, &cli.Command{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRunSync_When_DestinationScanFails_Then_NothingIsWritten(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "EXAMINE" {
			return "NO [CANNOT] examine refused", true
		}
		return "", false
	}))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if err == nil || !strings.Contains(err.Error(), `scan destination folder "INBOX"`) {
		t.Fatalf("error = %v, want the destination scan failure", err)
	}
	if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d CREATE or APPEND commands, want 0", n)
	}
}

func TestRunSync_When_CanceledDuringFolderCreation_Then_ReportsCancelNotCreateFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "CREATE" {
			cancel()
			return "", true
		}
		return "", false
	}))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("Work", "Work")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "failed to create") {
		t.Errorf("error %q reports a creation failure for a Ctrl+C", err)
	}
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
}

func TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
	idle := func(string, int) (string, bool) { return "", false }
	stallOnAppend := func(verb string, _ int) (string, bool) {
		if verb == "APPEND" {
			cancel()
			return "", true
		}
		return "", false
	}
	// The first dst connection plans; the second is the only worker's.
	dst.addConnHandler(scriptedHandler(dst, []string{"A", "B"}, idle))
	dst.addConnHandler(scriptedHandler(dst, []string{"A", "B"}, stallOnAppend))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertContainsNone(t, out.String(), "Sync completed")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("dst saw %d APPEND commands, want 1 (no plan starts after the cancel)", n)
	}
}

func TestRunSync_When_TransferIsLimited_Then_WorkersShareOneBudgetPerDirection(t *testing.T) {
	t.Parallel()

	const (
		bps       = 200_000
		perFolder = 3
		bodyLines = 640
	)
	tests := []struct {
		name  string
		limit config.RateLimit
	}{
		{name: "download", limit: config.RateLimit{DownBPS: bps}},
		{name: "upload", limit: config.RateLimit{UpBPS: bps}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A bucket moves at most burst + rate x elapsed bytes: one shared bucket
			// cannot finish before minElapsed, per-worker buckets would stay in burst.
			filler := strings.Repeat(strings.Repeat("x", 78)+"\r\n", bodyLines)
			msgs := func(prefix string) []fakeMsg {
				out := make([]fakeMsg, perFolder)
				for i := range out {
					id := fmt.Sprintf("%s%d@x", prefix, i)
					out[i] = fakeMsg{msgID: id, body: imapFullBody(id) + filler}
				}
				return out
			}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": msgs("a"), "B": msgs("b")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
			cfg := e2eConfig(src, dst, 2, mapping("A", "A"), mapping("B", "B"))
			cfg.RateLimit = tt.limit
			opts := e2eOptions(&syncBuffer{}, "")
			opts.autoConfirm = true
			total := 2 * perFolder * (len(imapFullBody("a0@x")) + len(filler))
			minElapsed := time.Duration(float64(total-bps) / bps * 0.8 * float64(time.Second))

			start := time.Now()
			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}
			elapsed := time.Since(start)

			if elapsed < minElapsed {
				t.Errorf("sync took %v, want at least %v for %d bytes through one %d B/s budget", elapsed, minElapsed, total, bps)
			}
			if n := len(dst.folderMsgIDs("A")) + len(dst.folderMsgIDs("B")); n != 2*perFolder {
				t.Errorf("dst holds %d messages, want %d", n, 2*perFolder)
			}
		})
	}
}

const (
	// hostileWireName is the modified UTF-7 form of hostileName; go-imap decodes
	// it and encodes it back byte for byte.
	hostileWireName   = "Evil&ABs-[2J&AA0-x&IC4-gpj"
	hostileName       = "Evil\x1b[2J\rx\u202egpj"
	hostileReply      = "\x1b]0;pwned\x07 refused\r now \u202e"
	hostileShownName  = `Evil\x1b[2J\rx\u202egpj`
	hostileReplyShown = `\x1b]0;pwned\a refused\r now \u202e`
)

func TestRunSync_When_SourceFolderNameCarriesControlCharacters_Then_OutputShowsEscapesAndServerGetsExactName(t *testing.T) {
	t.Parallel()

	const (
		childWireName  = hostileWireName + "/Sub&ABs-"
		childShownName = hostileShownName + `/Sub\x1b`
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":         nil,
		hostileWireName: fakeMsgs("e1@x"),
		childWireName:   fakeMsgs("e2@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1)
	out := &syncBuffer{}
	opts := e2eOptions(out, "y\n")
	opts.verbose = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), hostileWireName, childWireName)
	assertIDs(t, "dst APPENDs to the folder", dst.appended(hostileWireName), "e1@x")
	assertIDs(t, "dst APPENDs to the subfolder", dst.appended(childWireName), "e2@x")
	got := out.String()
	assertContainsAll(t, got,
		"• "+hostileShownName+" → "+hostileShownName+" will copy 1 messages",
		"• "+childShownName+" → "+childShownName+" will copy 1 messages",
		"• "+hostileShownName+"\n",
		"• "+childShownName+"\n",
		"Found subfolder: "+childShownName+" → "+childShownName,
	)
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J")
}

func Test_buildSyncPlan_When_FolderNameCarriesControlCharacters_Then_TrackersShowEscapesAndPlanKeepsExactName(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: fakeMsgs("a@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: nil})
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{mapping(hostileName, hostileName)}

	summary, err := buildSyncPlan(t.Context(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")
	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}

	if want := "[src] Scanned " + hostileShownName + " (1/1)"; srcTr.Message != want {
		t.Errorf("source tracker message = %q, want %q", srcTr.Message, want)
	}
	if want := "[dst] Scanned " + hostileShownName + " (1/1)"; dstTr.Message != want {
		t.Errorf("destination tracker message = %q, want %q", dstTr.Message, want)
	}
	if len(summary.Plans) != 1 {
		t.Fatalf("Plans = %d, want 1", len(summary.Plans))
	}
	if p := summary.Plans[0]; p.SourceFolder != hostileName || p.DestinationFolder != hostileName {
		t.Errorf("plan names = %q -> %q, want the exact name %q on both sides", p.SourceFolder, p.DestinationFolder, hostileName)
	}
}

func Test_buildSyncPlan_When_SourceFolderRefusedWithHostileReply_Then_FailureKeepsExactNameAndReply(t *testing.T) {
	t.Parallel()

	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(scriptedHandler(srcSrv, []string{hostileWireName}, func(verb string, _ int) (string, bool) {
		if verb == "EXAMINE" {
			return "NO " + hostileReply, true
		}
		return "", false
	}))
	dstSrv := newFakeServer(t)
	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")
	srcTr, dstTr := makePlanTrackers()

	summary, err := buildSyncPlan(t.Context(), srcC, dstC, []config.DirectoryMapping{mapping(hostileName, hostileName)}, srcTr, dstTr, "src", "dst")

	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 0 {
		t.Errorf("Plans = %d, want 0: the refused folder has no plan", len(summary.Plans))
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly one", summary.Failures)
	}
	f := summary.Failures[0]
	if f.SourceFolder != hostileName || f.DestinationFolder != hostileName {
		t.Errorf("failure names = %q -> %q, want the exact name %q on both sides", f.SourceFolder, f.DestinationFolder, hostileName)
	}
	if !strings.Contains(f.Err.Error(), hostileReply) {
		t.Errorf("failure error = %q, want the raw reply %q", f.Err, hostileReply)
	}
	if !errors.Is(f.Err, errSourceScan) {
		t.Errorf("failure error = %v, want errSourceScan", f.Err)
	}
}

func Test_buildSyncPlan_When_SourceScanIsRefused_Then_OnlyAnUnlistedParentIsSkipped(t *testing.T) {
	t.Parallel()

	archiveMappings := []config.DirectoryMapping{mapping("Archive", "Archive"), mapping("Archive/2024", "Archive/2024")}
	tests := []struct {
		folders     map[string][]fakeMsg
		refuse      map[string]string
		name        string
		mappings    []config.DirectoryMapping
		wantSkipped []string
		wantFailed  []string
		wantPlans   []string
	}{
		{
			name:        "unlisted parent with listed children",
			folders:     map[string][]fakeMsg{"Archive/2024": fakeMsgs("a1@x")},
			mappings:    archiveMappings,
			wantSkipped: []string{"Archive"},
			wantPlans:   []string{"Archive/2024"},
		},
		{
			name:       "unlisted parent that answers with a throttle",
			folders:    map[string][]fakeMsg{"Archive/2024": fakeMsgs("a1@x")},
			refuse:     map[string]string{"Archive": "NO Too many simultaneous connections"},
			mappings:   archiveMappings,
			wantFailed: []string{"Archive"},
			wantPlans:  []string{"Archive/2024"},
		},
		{
			name:       "listed parent that refuses to open",
			folders:    map[string][]fakeMsg{"Archive": nil, "Archive/2024": fakeMsgs("a1@x")},
			refuse:     map[string]string{"Archive": "NO Permission denied"},
			mappings:   archiveMappings,
			wantFailed: []string{"Archive"},
			wantPlans:  []string{"Archive/2024"},
		},
		{
			name:       "unlisted name without children",
			folders:    map[string][]fakeMsg{"Archive/2024": fakeMsgs("a1@x")},
			mappings:   []config.DirectoryMapping{mapping("Archiv", "Archiv")},
			wantFailed: []string{"Archiv"},
		},
		{
			name:       "unlisted INBOX with children",
			folders:    map[string][]fakeMsg{"INBOX/Sub": fakeMsgs("s1@x")},
			mappings:   []config.DirectoryMapping{mapping("INBOX", "INBOX"), mapping("INBOX/Sub", "INBOX/Sub")},
			wantFailed: []string{"INBOX"},
			wantPlans:  []string{"INBOX/Sub"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.folders)
			for folder, reply := range tt.refuse {
				src.refuseOpenWith(folder, reply)
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			srcC := newAppClient(t, src, "src")
			dstC := newAppClient(t, dst, "dst")
			srcTr, dstTr := makePlanTrackers()

			summary, err := buildSyncPlan(t.Context(), srcC, dstC, tt.mappings, srcTr, dstTr, "src", "dst")

			if err != nil {
				t.Fatalf("buildSyncPlan: %v", err)
			}
			var skipped, failed, planned []string
			for _, s := range summary.Skipped {
				skipped = append(skipped, s.SourceFolder)
				if !errors.Is(s.Err, client.ErrSelectRefused) {
					t.Errorf("skip error = %v, want client.ErrSelectRefused", s.Err)
				}
				if errors.Is(s.Err, errSourceScan) {
					t.Errorf("skip error = %v, want the raw scan error without errSourceScan", s.Err)
				}
			}
			for _, f := range summary.Failures {
				failed = append(failed, f.SourceFolder)
				if !errors.Is(f.Err, errSourceScan) {
					t.Errorf("failure error = %v, want errSourceScan", f.Err)
				}
			}
			for _, p := range summary.Plans {
				planned = append(planned, p.SourceFolder)
			}
			assertIDs(t, "Skipped", skipped, tt.wantSkipped...)
			assertIDs(t, "Failures", failed, tt.wantFailed...)
			assertIDs(t, "Plans", planned, tt.wantPlans...)
		})
	}
}

func waitForTrackerValue(t *testing.T, tr *progress.Tracker, want int64) {
	t.Helper()

	deadline := time.Now().Add(e2eTimeout)
	for tr.Value() < want {
		if time.Now().After(deadline) {
			t.Errorf("tracker value = %d after %v, want %d", tr.Value(), e2eTimeout, want)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func Test_buildSyncPlan_When_ContextEndsBeforeHierarchyCheck_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srcTr, dstTr := makePlanTrackers()
	srcSrv := newFakeServer(t)
	srcSrv.addConnHandler(scriptedHandler(srcSrv, []string{"Parent/Child", "Last"}, func(verb string, nth int) (string, bool) {
		if verb != "EXAMINE" {
			return "", false
		}
		if nth == 1 {
			// The destination scan returns nil after its last Increment, so the cancel below
			// can only reach the summary phase.
			waitForTrackerValue(t, dstTr, 2)
		} else {
			cancel()
		}
		return "NO Permission denied", true
	}))
	dstSrv := newFakeServer(t)
	srcC := newAppClient(t, srcSrv, "src")
	dstC := newAppClient(t, dstSrv, "dst")
	mappings := []config.DirectoryMapping{mapping("Parent", "Parent"), mapping("Last", "Last")}

	summary, err := buildSyncPlan(ctx, srcC, dstC, mappings, srcTr, dstTr, "src", "dst")

	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if srcTr.Value() != 2 || dstTr.Value() != 2 {
		t.Errorf("tracker values = %d src, %d dst, want 2 on both: each scan must end before the error", srcTr.Value(), dstTr.Value())
	}
	assertIDs(t, "src EXAMINE names", srcSrv.capturedNames("EXAMINE"), "Parent", "Last")
	if n := dstSrv.callCount("EXAMINE"); n != 0 {
		t.Errorf("dst saw %d EXAMINE commands, want 0: neither destination exists", n)
	}
}

// Everything derived from the context, such as the errgroup context, is already done after the first Err call.
type endsAfterFirstErr struct {
	context.Context
	end   context.CancelFunc
	calls atomic.Int32
}

func (c *endsAfterFirstErr) Err() error {
	if c.calls.Add(1) == 1 {
		c.end()
		return nil
	}
	return c.Context.Err()
}

func Test_buildSyncPlan_When_ContextEndsAfterEntryCheck_Then_NeitherScanStarts(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil})
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	srcTr, dstTr := makePlanTrackers()
	base, end := context.WithCancel(t.Context())
	t.Cleanup(end)
	ctx := &endsAfterFirstErr{Context: base, end: end}

	summary, err := buildSyncPlan(ctx, srcC, dstC, []config.DirectoryMapping{mapping("A", "A")}, srcTr, dstTr, "src", "dst")

	if summary != nil {
		t.Errorf("summary = %+v, want nil", summary)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if srcTr.Message != "src" || dstTr.Message != "dst" {
		t.Errorf("tracker messages = %q src, %q dst, want both untouched: no scan may start", srcTr.Message, dstTr.Message)
	}
	if srcTr.Value() != 0 || dstTr.Value() != 0 {
		t.Errorf("tracker values = %d src, %d dst, want 0 on both", srcTr.Value(), dstTr.Value())
	}
	for _, srv := range []*fakeServer{src, dst} {
		if n := srv.callCount("EXAMINE"); n != 0 {
			t.Errorf("a server saw %d EXAMINE commands, want 0", n)
		}
	}
}

// The name is short so go-pretty does not snip the tracker line that holds it.
func hostileFolder(letter string) (wire, name, shown string) {
	return letter + "&ABs-[2J", letter + "\x1b[2J", letter + `\x1b[2J`
}

func TestRunSync_When_FolderCreationFails_Then_LogShowsEscapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr     func(error) bool
		name        string
		createReply string
		wantLog     string
		verbose     bool
	}{
		{
			name:        "refused with a hostile reply",
			createReply: "NO " + hostileReply,
			wantLog:     `Failed to create folder "` + hostileShownName + `": [dst] failed to create mailbox ` + hostileShownName + ": ",
			wantErr:     func(err error) bool { return err != nil && strings.Contains(err.Error(), "failed to create 1 folders") },
		},
		{
			name:        "already exists",
			createReply: "NO [ALREADYEXISTS] Mailbox already exists",
			verbose:     true,
			wantLog:     "Folder " + hostileShownName + " already exists",
			// The fake CREATE creates nothing, so the APPEND that follows is refused.
			wantErr: func(err error) bool { return errors.Is(err, ErrSilentExit) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: fakeMsgs("a@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseCreateWith(tt.createReply)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.verbose = tt.verbose

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(hostileName, hostileName)), opts)

			if !tt.wantErr(err) {
				t.Errorf("error = %v", err)
			}
			got := out.String()
			assertContainsAll(t, got, tt.wantLog)
			assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned")
		})
	}
}

func TestRunSync_When_CanceledDuringFolderCreation_Then_CreatingLineShowsEscapes(t *testing.T) {
	t.Parallel()

	wire, name, shown := hostileFolder("A")
	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{wire: fakeMsgs("a@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "CREATE" {
			cancel()
			return "", true
		}
		return "", false
	}))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping(name, name)), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	got := out.String()
	assertContainsAll(t, got, "(1/1) Creating "+shown)
	assertContainsNone(t, got, name)
}

func TestRunSync_When_CanceledBeforeSecondPlanStarts_Then_WaitingLineShowsEscapes(t *testing.T) {
	t.Parallel()

	srcWireA, srcA, _ := hostileFolder("A")
	dstWireB, dstB, _ := hostileFolder("B")
	srcWireC, srcC, shownC := hostileFolder("C")
	dstWireD, dstD, shownD := hostileFolder("D")
	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{srcWireA: fakeMsgs("a@x"), srcWireC: fakeMsgs("c@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{dstWireB: nil, dstWireD: nil})
	idle := func(string, int) (string, bool) { return "", false }
	stallOnAppend := func(verb string, _ int) (string, bool) {
		if verb == "APPEND" {
			cancel()
			return "", true
		}
		return "", false
	}
	// The first dst connection plans; the second is the only worker's.
	dst.addConnHandler(scriptedHandler(dst, []string{dstWireB, dstWireD}, idle))
	dst.addConnHandler(scriptedHandler(dst, []string{dstWireB, dstWireD}, stallOnAppend))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping(srcA, dstB), mapping(srcC, dstD)), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	got := out.String()
	assertContainsAll(t, got, "2/2 Waiting: "+shownC+" → "+shownD)
	assertContainsNone(t, got, srcC, dstD, srcA, dstB)
}

func Test_buildSyncPlan_When_ScanIsInFlight_Then_TrackerShowsEscapedFolderName(t *testing.T) {
	t.Parallel()

	for _, held := range []string{"src", "dst"} {
		t.Run(held+" scan", func(t *testing.T) {
			t.Parallel()

			arrived, release := make(chan struct{}, 1), make(chan struct{})
			react := func(label string) func(string, int) (string, bool) {
				return func(verb string, _ int) (string, bool) {
					if verb == "EXAMINE" && label == held {
						arrived <- struct{}{}
						<-release
					}
					return "", false
				}
			}
			srcSrv, dstSrv := newFakeServer(t), newFakeServer(t)
			srcSrv.addConnHandler(scriptedHandler(srcSrv, []string{hostileWireName}, react("src")))
			dstSrv.addConnHandler(scriptedHandler(dstSrv, []string{hostileWireName}, react("dst")))
			srcC, dstC := newAppClient(t, srcSrv, "src"), newAppClient(t, dstSrv, "dst")
			// Registered after the clients, so it runs first and frees a scan parked in a failed test.
			t.Cleanup(func() { close(release) })
			srcTr, dstTr := makePlanTrackers()
			done := make(chan error, 1)
			go func() {
				_, err := buildSyncPlan(t.Context(), srcC, dstC, []config.DirectoryMapping{mapping(hostileName, hostileName)}, srcTr, dstTr, "src", "dst")
				done <- err
			}()

			select {
			case <-arrived:
			case <-time.After(e2eTimeout):
				t.Fatal("the held EXAMINE never arrived")
			}
			// The held scan waits for the server, so nothing else writes its tracker.
			tracker := srcTr
			if held == "dst" {
				tracker = dstTr
			}
			got := tracker.Message
			release <- struct{}{}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("buildSyncPlan: %v", err)
				}
			case <-time.After(e2eTimeout):
				t.Fatal("buildSyncPlan did not return after the EXAMINE was released")
			}

			if want := "[" + held + "] Scanning " + hostileShownName + " (1/1)"; got != want {
				t.Errorf("tracker message during the scan = %q, want %q", got, want)
			}
		})
	}
}

func TestBuildUploadLimitWarning(t *testing.T) {
	t.Parallel()

	floor := client.MinUploadBPS(4)
	tests := []struct {
		name  string
		upBPS int
		warn  bool
	}{
		{name: "no upload limit", upBPS: 0},
		{name: "limit at the floor", upBPS: floor},
		{name: "limit one below the floor", upBPS: floor - 1, warn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildUploadLimitWarning(tt.upBPS, 4)
			if !tt.warn {
				if got != "" {
					t.Errorf("warning = %q, want none", got)
				}
				return
			}
			assertContainsAll(t, got,
				fmt.Sprintf("Upload limit %d bytes/s is below %d bytes/s for 4 workers", tt.upBPS, floor),
				"Raise --bps-up or lower --workers")
		})
	}
}

func TestRunSync_When_UploadLimitIsTooLow_Then_WarnsBeforeTheSync(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	cfg := e2eConfig(src, dst, 2, mapping("INBOX", "INBOX"))
	cfg.RateLimit.UpBPS = 50
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertContainsAll(t, out.String(), "Upload limit 50 bytes/s is below", "All folders already synced!")
}

func TestRunSync_When_BothSidesListGmailContainer_Then_MirrorCopiesEveryOtherFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":        fakeMsgs("m1@x", "m2@x"),
		"[Gmail]/Sent": fakeMsgs("s1@x"),
	})
	src.markNoSelect("[Gmail]")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "[Gmail]/Sent": nil})
	dst.markNoSelect("[Gmail]")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
	assertIDs(t, "dst [Gmail]/Sent", dst.folderMsgIDs("[Gmail]/Sent"), "s1@x")
	for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
		if slices.Contains(srv.capturedNames("EXAMINE"), "[Gmail]") {
			t.Errorf("%s opened [Gmail], a folder its LIST marks \\Noselect", name)
		}
	}
	if n := dst.callCount("CREATE"); n != 0 {
		t.Errorf("dst saw %d CREATE commands, want 0", n)
	}
	assertReadOnly(t, "source", src)
	assertContainsAll(t, out.String(), "Sync completed successfully")
	assertContainsNone(t, out.String(), "cannot be synced")
}

func TestRunSync_When_MappedSourceIsNoSelect_Then_OnlyItsChildrenAreCopied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		byFlags bool
	}{
		{name: "config map"},
		{name: "source and destination flags", byFlags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"[Gmail]/Sent":   fakeMsgs("s1@x"),
				"[Gmail]/Drafts": fakeMsgs("d1@x"),
			})
			src.markNoSelect("[Gmail]")
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			if tt.byFlags {
				opts.srcFolder, opts.dstFolder = "[Gmail]", "Archive"
			} else {
				cfg.Map = []config.DirectoryMapping{mapping("[Gmail]", "Archive")}
			}

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertIDs(t, "dst Archive/Sent", dst.folderMsgIDs("Archive/Sent"), "s1@x")
			assertIDs(t, "dst Archive/Drafts", dst.folderMsgIDs("Archive/Drafts"), "d1@x")
			if slices.Contains(src.capturedNames("EXAMINE"), "[Gmail]") {
				t.Error("source opened [Gmail], a folder its LIST marks \\Noselect")
			}
			assertContainsNone(t, out.String(), "cannot be synced")
			assertReadOnly(t, "source", src)
		})
	}
}

func assertInOrder(t *testing.T, got string, parts ...string) {
	t.Helper()
	offset := 0
	for _, part := range parts {
		i := strings.Index(got[offset:], part)
		if i < 0 {
			t.Errorf("output does not contain %q after offset %d\noutput: %q", part, offset, got)
			return
		}
		offset += i + len(part)
	}
}

func TestRunSync_When_DestinationIsNoSelect_Then_ReportsFolderFailureAndContinues(t *testing.T) {
	t.Parallel()

	const failureLine = `• Projects → Projects: 2 messages not copied: destination folder cannot hold messages (\Noselect)`
	tests := []struct {
		name      string
		input     string
		wantOut   []string
		wantNone  []string
		wantOrder []string
		withInbox bool
		quiet     bool
		confirm   bool
	}{
		{
			name:      "other folders still copy",
			withInbox: true,
			confirm:   true,
			wantOut: []string{
				"Folders that cannot be synced", failureLine,
				"1 messages uploaded, 0 errors occurred, 1 folders failed",
			},
			wantNone: []string{"Sync completed successfully"},
		},
		{
			name:    "nothing else to copy",
			confirm: true,
			wantOut: []string{
				"Folders that cannot be synced", failureLine,
				"0 messages uploaded, 0 errors occurred, 1 folders failed",
			},
			wantNone: []string{"All folders already synced!", "Sync completed successfully"},
		},
		{
			name:    "quiet",
			quiet:   true,
			confirm: true,
			wantOut: []string{
				"Folders that cannot be synced", failureLine,
				"0 messages uploaded, 0 errors occurred, 1 folders failed",
			},
			wantNone: []string{"All folders already synced!", "Sync completed successfully"},
		},
		{
			name:      "interactive",
			input:     "y\n",
			withInbox: true,
			wantOut: []string{
				"Folders that cannot be synced", failureLine,
				"1 messages uploaded, 0 errors occurred, 1 folders failed",
			},
			wantNone:  []string{"Sync completed successfully"},
			wantOrder: []string{"Folders that cannot be synced", "Proceed with synchronization?"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcFolders := map[string][]fakeMsg{"Projects": fakeMsgs("p1@x", "p2@x")}
			dstFolders := map[string][]fakeMsg{}
			mappings := []config.DirectoryMapping{mapping("Projects", "Projects")}
			if tt.withInbox {
				srcFolders["INBOX"] = fakeMsgs("m1@x")
				dstFolders["INBOX"] = nil
				mappings = []config.DirectoryMapping{mapping("INBOX", "INBOX"), mapping("Projects", "Projects")}
			}
			src := newStoreServer(t, "/", srcFolders)
			dst := newStoreServer(t, "/", dstFolders)
			dst.markNoSelect("Projects")
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.autoConfirm, opts.quiet = tt.confirm, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mappings...), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit", err)
			}
			if tt.withInbox {
				assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			}
			if slices.Contains(dst.capturedNames("EXAMINE"), "Projects") {
				t.Error("destination opened Projects, a folder its LIST marks \\Noselect")
			}
			if slices.Contains(dst.capturedNames("CREATE"), "Projects") {
				t.Error("destination received CREATE for Projects, which its LIST already shows")
			}
			assertIDs(t, "dst APPENDs to Projects", dst.appended("Projects"))
			if !tt.withInbox {
				if n := dst.callCount("APPEND"); n != 0 {
					t.Errorf("dst saw %d APPEND commands, want 0", n)
				}
			}
			assertContainsAll(t, out.String(), tt.wantOut...)
			assertContainsNone(t, out.String(), tt.wantNone...)
			assertInOrder(t, out.String(), tt.wantOrder...)
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_NoSelectDestinationHasNothingToReceive_Then_SyncSucceeds(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Projects": nil})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	dst.markNoSelect("Projects")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Projects", "Projects")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertContainsAll(t, out.String(), "All folders already synced!")
	assertContainsNone(t, out.String(), "cannot be synced", "folders failed")
	if slices.Contains(dst.capturedNames("EXAMINE"), "Projects") {
		t.Error("destination opened Projects, a folder its LIST marks \\Noselect")
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_NoSelectDestinationNameCarriesControlCharacters_Then_FailureLineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: fakeMsgs("a@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	dst.markNoSelect(hostileWireName)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(hostileName, hostileName)), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got, "• "+hostileShownName+" → "+hostileShownName+": 1 messages not copied")
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J")
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), hostileWireName)
	assertReadOnly(t, "source", src)
}

func Test_buildSyncPlan_When_DestinationIsNoSelect_Then_RecordsFailureNotPlan(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":    fakeMsgs("m1@x"),
		"Projects": fakeMsgs("p1@x", "p2@x"),
		"Empty":    nil,
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.markNoSelect("Projects", "Empty")
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{mapping("INBOX", "INBOX"), mapping("Projects", "Projects"), mapping("Empty", "Empty")}

	summary, err := buildSyncPlan(t.Context(), srcC, dstC, mappings, srcTr, dstTr, "src", "dst")

	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Plans) != 1 || summary.Plans[0].SourceFolder != "INBOX" {
		t.Errorf("Plans = %+v, want only INBOX", summary.Plans)
	}
	if summary.TotalNew != 1 {
		t.Errorf("TotalNew = %d, want 1", summary.TotalNew)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly one", summary.Failures)
	}
	f := summary.Failures[0]
	if f.SourceFolder != "Projects" || f.DestinationFolder != "Projects" {
		t.Errorf("failure names = %q -> %q, want Projects -> Projects", f.SourceFolder, f.DestinationFolder)
	}
	if !errors.Is(f.Err, errDestinationNoSelect) {
		t.Errorf("failure error = %v, want errDestinationNoSelect", f.Err)
	}
	if !strings.Contains(f.Err.Error(), "2 messages not copied") {
		t.Errorf("failure error = %q, want the message count", f.Err)
	}
	assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "INBOX")
}

func Test_buildSyncPlan_When_SourceScanFailsAndDestinationIsNoSelect_Then_RecordsOneSourceFailure(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Projects": fakeMsgs("p1@x", "p2@x")})
	src.refuseOpenWith("Projects", "NO [UNAVAILABLE] Temporary server failure")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	dst.markNoSelect("Projects")
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	srcTr, dstTr := makePlanTrackers()

	summary, err := buildSyncPlan(t.Context(), srcC, dstC, []config.DirectoryMapping{mapping("Projects", "Projects")}, srcTr, dstTr, "src", "dst")

	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly one", summary.Failures)
	}
	f := summary.Failures[0]
	if !errors.Is(f.Err, errSourceScan) {
		t.Errorf("failure error = %v, want errSourceScan", f.Err)
	}
	if errors.Is(f.Err, errDestinationNoSelect) {
		t.Errorf("failure error = %v, want no errDestinationNoSelect: the source failure comes first", f.Err)
	}
}

func Test_maybeDiff_When_DestinationIsNoSelect_Then_NoPlanAndMapsFreed(t *testing.T) {
	t.Parallel()

	mappings := []config.DirectoryMapping{mapping("Projects", "Projects")}
	plans := make([]FolderSyncPlan, 1)
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64
	s := &folderScan{
		srcMap:         map[string]uint32{"a@x": 1, "b@x": 2},
		dstIDs:         map[string]struct{}{},
		srcFolderSize:  2000,
		srcFolderCount: 2,
		dstNoSelect:    true,
	}

	if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
		t.Fatal("maybeDiff returned true on the first call, want false")
	}
	if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize) {
		t.Fatal("maybeDiff returned false on the second call, want true")
	}

	if plans[0].SourceFolder != "" {
		t.Errorf("plan populated for a \\Noselect destination: %+v", plans[0])
	}
	if totalNew.Load() != 0 || totalNewSize.Load() != 0 {
		t.Errorf("totals = %d messages, %d bytes, want 0 and 0", totalNew.Load(), totalNewSize.Load())
	}
	if s.srcMap != nil || s.dstIDs != nil {
		t.Errorf("maps not freed: srcMap=%v dstIDs=%v", s.srcMap, s.dstIDs)
	}
}

func Test_expandMappingsWithSubfolders_When_SourceIsNoSelect_Then_SkipsItAndKeepsChildren(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "verbose", verbose: true},
		{name: "not verbose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"a": nil, "a/b": nil, "a/b/c": fakeMsgs("c@x"), "a/d": fakeMsgs("d@x"), "x": nil,
			})
			src.markNoSelect("a", "a/b")
			srcC := newAppClient(t, src, "src")
			mappings := []config.DirectoryMapping{mapping("a", "A"), mapping("Missing", "Missing")}
			var out strings.Builder

			got, err := expandMappingsWithSubfolders(t.Context(), &out, srcC, mappings, "/", "/", tt.verbose, false)

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			// The cache iteration order is random, so compare as a set.
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if want := []string{"Missing->Missing", "a/b/c->A/b/c", "a/d->A/d"}; !slices.Equal(pairs, want) {
				t.Errorf("expanded = %v, want %v", pairs, want)
			}
			skipA := "Folder a skipped - the source server marks it \\Noselect\n"
			skipAB := "Folder a/b skipped - the source server marks it \\Noselect\n"
			if !tt.verbose {
				assertContainsNone(t, out.String(), "skipped", "Found subfolder")
				return
			}
			for _, line := range []string{skipA, skipAB} {
				if n := strings.Count(out.String(), line); n != 1 {
					t.Errorf("output holds the line %q %d times, want 1\noutput: %q", line, n, out.String())
				}
			}
			assertContainsAll(t, out.String(), "Found subfolder: a/b/c → A/b/c", "Found subfolder: a/d → A/d")
			assertContainsNone(t, out.String(), "Found subfolder: a/b →")
		})
	}
}

func Test_expandMappingsWithSubfolders_When_SourceIsUnlistedParent_Then_KeepsItWithChildren(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "verbose", verbose: true},
		{name: "not verbose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": nil, "Archive/2025": nil})
			srcC := newAppClient(t, src, "src")
			var out strings.Builder

			got, err := expandMappingsWithSubfolders(t.Context(), &out, srcC, []config.DirectoryMapping{mapping("Archive", "Old")}, "/", "/", tt.verbose, false)

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if want := []string{"Archive->Old", "Archive/2024->Old/2024", "Archive/2025->Old/2025"}; !slices.Equal(pairs, want) {
				t.Errorf("expanded = %v, want %v", pairs, want)
			}
			assertContainsNone(t, out.String(), "skipped")
		})
	}
}

func Test_isHierarchyLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		folders map[string][]fakeMsg
		name    string
		query   string
		hidden  []string
		want    bool
		flat    bool
	}{
		{name: "unlisted with listed children", folders: map[string][]fakeMsg{"Archive/2024": nil}, query: "Archive", want: true},
		{
			name: "readable but hidden with listed children", folders: map[string][]fakeMsg{"Archive": fakeMsgs("p1@x"), "Archive/2024": nil},
			hidden: []string{"Archive"}, query: "Archive", want: true,
		},
		{name: "listed name", folders: map[string][]fakeMsg{"Archive": nil}, query: "Archive"},
		{name: "listed name that also has children", folders: map[string][]fakeMsg{"Archive": nil, "Archive/2024": nil}, query: "Archive"},
		{name: "unlisted without children", folders: map[string][]fakeMsg{"Archive/2024": nil}, query: "Archiv"},
		{name: "upper case INBOX unlisted with children", folders: map[string][]fakeMsg{"INBOX/Sub": nil}, query: "INBOX"},
		{name: "mixed case Inbox unlisted with children", folders: map[string][]fakeMsg{"Inbox/Sub": nil}, query: "Inbox"},
		{name: "flat server whose names share the prefix", folders: map[string][]fakeMsg{"Archive/2024": nil, "Archiveold": nil}, query: "Archive", flat: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			delimiter := "/"
			if tt.flat {
				delimiter = ""
			}
			src := newStoreServer(t, delimiter, tt.folders)
			src.hideFromList(tt.hidden...)
			srcC := newAppClient(t, src, "src")

			got, err := isHierarchyLevel(t.Context(), srcC, tt.query)

			if err != nil {
				t.Fatalf("isHierarchyLevel: %v", err)
			}
			if got != tt.want {
				t.Errorf("isHierarchyLevel(%q) = %t, want %t", tt.query, got, tt.want)
			}
		})
	}

	t.Run("context already canceled", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": nil})
		srcC := newAppClient(t, src, "src")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		got, err := isHierarchyLevel(ctx, srcC, "Archive")

		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if got {
			t.Error("isHierarchyLevel = true, want false with the error")
		}
	})
}

func Test_expandMappingsWithSubfolders_When_InboxIsUnlistedButHasSubfolders_Then_InboxIsKept(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		inbox string
	}{
		{name: "upper case", inbox: "INBOX"},
		{name: "mixed case", inbox: "Inbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{tt.inbox + "/Sub": fakeMsgs("s1@x")})
			srcC := newAppClient(t, src, "src")

			got, err := expandMappingsWithSubfolders(t.Context(), io.Discard, srcC, []config.DirectoryMapping{mapping(tt.inbox, tt.inbox)}, "/", "/", false, false)

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if want := []string{tt.inbox + "->" + tt.inbox, tt.inbox + "/Sub->" + tt.inbox + "/Sub"}; !slices.Equal(pairs, want) {
				t.Errorf("expanded = %v, want %v", pairs, want)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_SourceIsUnlistedWithoutSubfolders_Then_MappingIsKept(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Sent": nil})
	srcC := newAppClient(t, src, "src")
	var out strings.Builder

	got, err := expandMappingsWithSubfolders(t.Context(), &out, srcC, []config.DirectoryMapping{mapping("Sentt", "Sent")}, "/", "/", true, false)

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if want := []config.DirectoryMapping{mapping("Sentt", "Sent")}; !slices.Equal(got, want) {
		t.Errorf("expanded = %v, want %v: the scan must report the missing folder", got, want)
	}
	assertContainsNone(t, out.String(), "skipped")
}

const folderFailuresHeader = "Folders that cannot be synced"

func TestRunSync_When_SourceFolderFlagHasTypo_Then_ReportsFailureAndExitsNonZero(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Sent":  fakeMsgs("s1@x", "s2@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true
	opts.srcFolder, opts.dstFolder = "Sentt", "Sent"

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		folderFailuresHeader,
		"• Sentt → Sent: cannot scan source folder: ",
		"0 messages uploaded, 0 errors occurred, 1 folders failed",
	)
	assertContainsNone(t, got, "All folders already synced!", "Sync completed successfully")
	if n := strings.Count(got, folderFailuresHeader); n != 1 {
		t.Errorf("output holds the failure header %d times, want 1\noutput: %q", n, got)
	}
	if n := dst.callCount("CREATE"); n != 0 {
		t.Errorf("dst saw %d CREATE commands, want 0", n)
	}
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_MappedSourceIsMissing_Then_OtherFoldersCopyAndRunFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		quiet      bool
		wantHeader int
	}{
		{name: "normal", wantHeader: 2},
		{name: "quiet", quiet: true, wantHeader: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"), mapping("Sent", "Sent Items"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit", err)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			if dst.hasFolder("Sent Items") {
				t.Error("dst holds Sent Items, want none: the source folder cannot be scanned")
			}
			if slices.Contains(dst.capturedNames("CREATE"), "Sent Items") {
				t.Error("dst received CREATE for Sent Items")
			}
			got := out.String()
			assertContainsAll(t, got,
				"• Sent → Sent Items: cannot scan source folder:",
				"1 messages uploaded, 0 errors occurred, 1 folders failed",
			)
			assertContainsNone(t, got, "Sync completed successfully")
			if n := strings.Count(got, folderFailuresHeader); n != tt.wantHeader {
				t.Errorf("output holds the failure header %d times, want %d\noutput: %q", n, tt.wantHeader, got)
			}
			if !tt.quiet {
				assertInOrder(t, got, folderFailuresHeader, "Messages to be copied to destination", "Syncing messages", folderFailuresHeader, "1 folders failed")
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_SourceRefusesExamineWithoutMapping_Then_FolderIsReported(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":   fakeMsgs("m1@x"),
		"Unavail": fakeMsgs("u1@x", "u2@x", "u3@x"),
	})
	src.refuseOpenWith("Unavail", "NO [UNAVAILABLE] Temporary server failure")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	if dst.hasFolder("Unavail") {
		t.Error("dst holds Unavail, want none")
	}
	assertContainsAll(t, out.String(),
		"• Unavail → Unavail: cannot scan source folder:",
		"Temporary server failure",
		"1 folders failed",
	)
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_PlanningSourceClientGivesUp_Then_EveryLaterFolderIsReported(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": fakeMsgs("a1@x"),
		"B": fakeMsgs("b1@x"),
		"C": fakeMsgs("c1@x"),
	})
	// LOGIN 1 is the planning client, LOGIN 2 its reconnect after the drop,
	// LOGIN 3 the worker.
	src.dropOnceOnOpen("B")
	src.refuseLoginAt(2)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"), mapping("C", "C")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x")
	if dst.hasFolder("B") || dst.hasFolder("C") {
		t.Errorf("dst holds B=%v C=%v, want neither", dst.hasFolder("B"), dst.hasFolder("C"))
	}
	got := out.String()
	assertContainsAll(t, got,
		"• B → B: cannot scan source folder:",
		"• C → C: cannot scan source folder:",
		"1 messages uploaded, 0 errors occurred, 2 folders failed",
	)
	assertContainsNone(t, got, "Sync completed successfully")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_OnlyFailuresAndNothingToCopy_Then_NoPromptAndExitsNonZero(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Sent": fakeMsgs("s1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.srcFolder, opts.dstFolder = "Sentt", "Sent"

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want no read error: nothing is left to confirm", err)
	}
	assertContainsAll(t, out.String(), folderFailuresHeader, "1 folders failed")
	assertContainsNone(t, out.String(), "Proceed with synchronization?", "All folders already synced!")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceRefusalCarriesControlCharacters_Then_FailureLineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newFakeServer(t)
	src.addConnHandler(scriptedHandler(src, []string{hostileWireName}, func(verb string, _ int) (string, bool) {
		if verb == "EXAMINE" {
			return "NO " + hostileReply, true
		}
		return "", false
	}))
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(hostileName, hostileName)), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		"• "+hostileShownName+" → "+hostileShownName+": cannot scan source folder: ",
		hostileReplyShown,
	)
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned")
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), hostileWireName)
}

func TestRunSync_When_ContextEndsDuringSourceScan_Then_ReturnsCanceledWithoutFailureList(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	src := newFakeServer(t)
	src.addConnHandler(scriptedHandler(src, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "EXAMINE" {
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-release
		}
		return "", false
	}))
	// Registered after the server, so it runs first and frees a handler parked in a failed test.
	t.Cleanup(func() { close(release) })
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	go func() {
		select {
		case <-arrived:
			cancel()
		case <-ctx.Done():
		}
	}()
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	// An absent destination lets the dst scan finish without I/O, so only the ctx check after the scan stops the failure list.
	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("INBOX", "Copy")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertContainsNone(t, out.String(), "cannot be synced", "folders failed")
}

func TestRunSync_When_MappedSourceIsReadableButUnlisted_Then_ItAndItsChildrenAreCopied(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Archive":      fakeMsgs("p1@x", "p2@x"),
		"Archive/2024": fakeMsgs("a1@x"),
	})
	src.hideFromList("Archive")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Archive", "Archive")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "p1@x", "p2@x")
	assertIDs(t, "dst Archive/2024", dst.folderMsgIDs("Archive/2024"), "a1@x")
	if !slices.Contains(src.capturedNames("EXAMINE"), "Archive") {
		t.Errorf("source EXAMINE names = %v, want Archive among them", src.capturedNames("EXAMINE"))
	}
	got := out.String()
	assertContainsAll(t, got, "Sync completed successfully")
	assertContainsNone(t, got, "skipped", "cannot be synced")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_UnlistedParentRefusesOpen_Then_SkipsItAndCopiesChildren(t *testing.T) {
	t.Parallel()

	const skipLine = "Folder Archive skipped - the source server lists only its subfolders and refuses to open it: "
	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "normal"},
		{name: "quiet", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"Archive/2024": fakeMsgs("a1@x"),
				"Archive/2025": fakeMsgs("a2@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Archive", "Archive")), opts)

			if err != nil {
				t.Fatalf("runSync: %v", err)
			}
			assertIDs(t, "dst Archive/2024", dst.folderMsgIDs("Archive/2024"), "a1@x")
			assertIDs(t, "dst Archive/2025", dst.folderMsgIDs("Archive/2025"), "a2@x")
			assertIDs(t, "dst APPENDs to Archive", dst.appended("Archive"))
			got := out.String()
			assertContainsNone(t, got, "cannot be synced")
			assertReadOnly(t, "source", src)
			if tt.quiet {
				assertContainsNone(t, got, "skipped")
				return
			}
			if !slices.Contains(src.capturedNames("EXAMINE"), "Archive") {
				t.Errorf("source EXAMINE names = %v, want Archive among them", src.capturedNames("EXAMINE"))
			}
			if n := strings.Count(got, skipLine); n != 1 {
				t.Errorf("output holds the skip line %d times, want 1\noutput: %q", n, got)
			}
			assertContainsAll(t, got, "Unknown Mailbox", "Sync completed successfully")
			assertInOrder(t, got, skipLine, "Messages to be copied to destination")
		})
	}
}

func TestRunSync_When_UnlistedParentDropsSessionAndReconnectIsRefused_Then_ReportsFailure(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": fakeMsgs("a1@x")})
	// LOGIN 1 is the planning client, LOGIN 2 its reconnect after the drop.
	src.dropOnceOnOpen("Archive")
	src.refuseLoginAt(2)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Archive", "Archive")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		folderFailuresHeader,
		"• Archive → Archive: cannot scan source folder:",
		"2 folders failed",
	)
	assertContainsNone(t, got, "skipped")
}

func TestRunSync_When_UnlistedParentRefusalCarriesControlCharacters_Then_SkipLineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName + "/2024": fakeMsgs("e1@x")})
	src.refuseOpenWith(hostileWireName, "NO "+hostileReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(hostileName, hostileName)), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		"Folder "+hostileShownName+" skipped - ",
		hostileReplyShown,
	)
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned")
	if names := src.capturedNames("EXAMINE"); len(names) == 0 || names[0] != hostileWireName {
		t.Errorf("source EXAMINE names = %q, want the first to be the exact name %q", names, hostileWireName)
	}
	assertIDs(t, "dst child folder", dst.folderMsgIDs(hostileWireName+"/2024"), "e1@x")
}

func assertNoCommandFor(t *testing.T, what string, srv *fakeServer, folder string) {
	t.Helper()
	for _, verb := range []string{"EXAMINE", "SELECT", "STATUS", "CREATE"} {
		if slices.Contains(srv.capturedNames(verb), folder) {
			t.Errorf("%s received %s for %q, a name its LIST marks \\Noselect", what, verb, folder)
		}
	}
	assertIDs(t, what+" APPENDs to "+folder, srv.appended(folder))
}

func TestRunSync_When_PromptIsDeclinedWithFailures_Then_ListsFailuresFirstAndWritesNothing(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	out := &syncBuffer{}
	opts := e2eOptions(out, "n\n")

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "Copy"), mapping("Gone", "Gone")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	got := out.String()
	assertInOrder(t, got,
		folderFailuresHeader,
		"• Gone → Gone: cannot scan source folder:",
		"• INBOX → Copy will copy 2 messages",
		"Proceed with synchronization?",
		"Sync canceled by user",
	)
	assertContainsNone(t, got, "Creating destination folders", "Syncing messages", "Sync completed", "All folders already synced!")
	if n := strings.Count(got, folderFailuresHeader); n != 1 {
		t.Errorf("output holds the failure header %d times, want 1\noutput: %q", n, got)
	}
	for _, verb := range []string{"CREATE", "APPEND"} {
		if n := dst.callCount(verb); n != 0 {
			t.Errorf("dst saw %d %s commands after the prompt was declined, want 0", n, verb)
		}
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_AppendsFailAndFoldersFail_Then_FinalLineCountsBoth(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseAppend()
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"), mapping("Gone1", "Gone1"), mapping("Gone2", "Gone2"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		"• Gone1 → Gone1: cannot scan source folder:",
		"• Gone2 → Gone2: cannot scan source folder:",
		"0 messages uploaded, 1 errors occurred, 2 folders failed",
	)
	assertContainsNone(t, got, "Sync completed successfully", "All folders already synced!")
	if n := strings.Count(got, folderFailuresHeader); n != 2 {
		t.Errorf("output holds the failure header %d times, want 2 (before the plan and before the final line)\noutput: %q", n, got)
	}
	assertInOrder(t, got, folderFailuresHeader, "Messages to be copied to destination", "Syncing messages", folderFailuresHeader, "2 folders failed")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceExamineIsThrottled_Then_FolderIsReportedWithoutReconnect(t *testing.T) {
	t.Parallel()

	const (
		reply = "Account exceeded command or bandwidth limits"
		wire  = "NO [THROTTLED] " + reply
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x")})
	src.refuseOpenWith("A", wire)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst B", dst.folderMsgIDs("B"), "b1@x")
	if dst.hasFolder("A") {
		t.Error("dst holds A, want none: its source scan was throttled")
	}
	assertContainsAll(t, out.String(),
		"• A → A: cannot scan source folder: ",
		reply,
		"1 messages uploaded, 0 errors occurred, 1 folders failed",
	)
	assertContainsNone(t, out.String(), "Sync completed successfully")
	examinesOfA := 0
	for _, name := range src.capturedNames("EXAMINE") {
		if name == "A" {
			examinesOfA++
		}
	}
	if examinesOfA != 1 {
		t.Errorf("source saw %d EXAMINE commands for A, want 1: a throttled reply is not retried", examinesOfA)
	}
	if n := src.callCount("LOGIN"); n != 2 {
		t.Errorf("source saw %d LOGIN commands, want 2 (the planning client and one worker): no reconnect", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_RunTwiceWithAFailureThatStays_Then_SecondRunWritesNothingAndFailsAgain(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"), mapping("Gone", "Gone"))

	firstOut := &syncBuffer{}
	first := e2eOptions(firstOut, "")
	first.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, first); !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first runSync error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, firstOut.String(), "2 messages uploaded, 0 errors occurred, 1 folders failed")
	appends, creates := dst.callCount("APPEND"), dst.callCount("CREATE")
	if appends != 2 {
		t.Fatalf("first run did %d APPEND, want 2", appends)
	}

	secondOut := &syncBuffer{}
	err := runSync(e2eContext(t), cfg, e2eOptions(secondOut, ""))

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("second runSync error = %v, want ErrSilentExit", err)
	}
	got := secondOut.String()
	assertContainsAll(t, got,
		folderFailuresHeader,
		"• Gone → Gone: cannot scan source folder:",
		"0 messages uploaded, 0 errors occurred, 1 folders failed",
	)
	assertContainsNone(t, got, "All folders already synced!", "Sync completed successfully", "Proceed with synchronization?")
	if n := dst.callCount("APPEND"); n != appends {
		t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
	}
	if n := dst.callCount("CREATE"); n != creates {
		t.Errorf("CREATE count went from %d to %d on the second run", creates, n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_NoSelectSourceNameCarriesControlCharacters_Then_SkipLineShowsEscapesAndChildrenCopy(t *testing.T) {
	t.Parallel()

	const (
		wire       = "Zed&ABs-[2J&IC4-x&MWQ-"
		name       = "Zed\x1b[2J\u202ex\u3164"
		shown      = `Zed\x1b[2J\u202ex\u3164`
		childWire  = wire + "/Kid"
		childShown = shown + "/Kid"
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{childWire: fakeMsgs("k1@x")})
	src.markNoSelect(wire)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(name, name)), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		"Folder "+shown+" skipped - the source server marks it \\Noselect\n",
		"Found subfolder: "+childShown+" → "+childShown,
		"Sync completed successfully",
	)
	assertContainsNone(t, got, name, "\u202e", "\x1b[2J", "cannot be synced")
	// The "Created folder" log is a documented %q site, which keeps U+3164, so only the skip line is checked for it.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "skipped") && strings.Contains(line, "\u3164") {
			t.Errorf("skip line holds the raw rune U+3164: %q", line)
		}
	}
	assertNoCommandFor(t, "source", src, wire)
	assertIDs(t, "dst APPENDs to the child", dst.appended(childWire), "k1@x")
	if creates := dst.capturedNames("CREATE"); !slices.Contains(creates, childWire) {
		t.Errorf("dst CREATE names = %q, want the exact wire name %q among them", creates, childWire)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_MappedSourceIsNoSelectWithoutSubfolders_Then_NothingIsScannedAndSyncSucceeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		byFlags bool
	}{
		{name: "config map"},
		{name: "source and destination flags", byFlags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			src.markNoSelect("Container")
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, true
			if tt.byFlags {
				opts.srcFolder, opts.dstFolder = "Container", "Archive"
			} else {
				cfg.Map = []config.DirectoryMapping{mapping("Container", "Archive")}
			}

			err := runSync(e2eContext(t), cfg, opts)

			if err != nil {
				t.Fatalf("runSync: %v", err)
			}
			assertContainsAll(t, out.String(),
				"Folder Container skipped - the source server marks it \\Noselect",
				"All folders already synced!",
			)
			assertContainsNone(t, out.String(), "cannot be synced", "folders failed", "Sync completed successfully")
			assertNoCommandFor(t, "source", src, "Container")
			for _, verb := range []string{"CREATE", "APPEND"} {
				if n := dst.callCount(verb); n != 0 {
					t.Errorf("dst saw %d %s commands, want 0", n, verb)
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_NoSelectDestinationReceivesDuplicateMessageIds_Then_FailureCountsDistinctIds(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Projects": fakeMsgs("p1@x", "p1@x", "p2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	dst.markNoSelect("Projects")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Projects", "Projects")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), "• Projects → Projects: 2 messages not copied: ")
	assertContainsNone(t, out.String(), "3 messages not copied")
	assertNoCommandFor(t, "destination", dst, "Projects")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_FolderFailsAndCreateIsRefused_Then_FailuresPrecedeTheCreateError(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreate()
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "Copy"), mapping("Gone", "Gone")), opts)

	if err == nil || !strings.Contains(err.Error(), "failed to create 1 folders") {
		t.Fatalf("error = %v, want %q", err, "failed to create 1 folders")
	}
	if errors.Is(err, ErrSilentExit) {
		t.Errorf("error = %v, want a create error, not ErrSilentExit", err)
	}
	got := out.String()
	assertInOrder(t, got, folderFailuresHeader, "• Gone → Gone: cannot scan source folder:", "Messages to be copied to destination", "Creating destination folders")
	assertContainsNone(t, got, "Syncing messages", "Sync completed")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationParentIsNoSelect_Then_ChildIsCreatedWithoutCreatingTheParent(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"[Gmail]/Sent": fakeMsgs("s1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.markNoSelect("[Gmail]")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("[Gmail]/Sent", "[Gmail]/Sent")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "[Gmail]/Sent")
	assertIDs(t, "dst APPENDs to [Gmail]/Sent", dst.appended("[Gmail]/Sent"), "s1@x")
	assertNoCommandFor(t, "destination", dst, "[Gmail]")
	assertContainsAll(t, out.String(), "Sync completed successfully")
	assertContainsNone(t, out.String(), "cannot be synced")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_BothServersAreFlat_Then_DottedFolderIsCopiedUnchanged(t *testing.T) {
	t.Parallel()

	const folder = "Mr. Smith"
	src := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil, folder: fakeMsgs("a@x", "b@x")})
	dst := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), "a@x", "b@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), folder)
	if slices.Contains(src.capturedNames("EXAMINE"), "Mr/ Smith") {
		t.Errorf("source EXAMINE names = %q, want no rewritten name", src.capturedNames("EXAMINE"))
	}
	assertContainsAll(t, out.String(), `Source [src]: ""`, `Destination [dst]: ""`)
	assertContainsNone(t, out.String(), "server expects", "delimiter mismatch", "Fixed source", "Fixed destination")
}

func TestRunSync_When_DestinationIsFlat_Then_CreatesNoParentFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work/2023": fakeMsgs("w@x")})
	dst := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil})
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work/2023", "Work/2023")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work/2023")
	if dst.hasFolder("Work") {
		t.Error("destination holds a folder Work that the source does not have")
	}
	assertIDs(t, "dst Work/2023", dst.folderMsgIDs("Work/2023"), "w@x")
}

func TestRunSync_When_SourceIsFlat_Then_MappingGetsNoSubfolders(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "", map[string][]fakeMsg{
		"Work":      fakeMsgs("w1@x"),
		"Work/2023": fakeMsgs("w2@x"),
		"Workshop":  fakeMsgs("s1@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Archive")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "w1@x")
	assertIDs(t, "dst folders", newAppClient(t, dst, "dst").MailboxNames(), "Archive", "INBOX")
	for _, name := range []string{"Work/2023", "Workshop"} {
		if slices.Contains(src.capturedNames("EXAMINE"), name) {
			t.Errorf("source EXAMINE names = %q, want no %q: a flat server has no subfolders", src.capturedNames("EXAMINE"), name)
		}
	}
}

func TestRunSync_When_DestinationIsFlat_Then_SubfoldersKeepTheirSourceNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		src         map[string][]fakeMsg
		mappings    []config.DirectoryMapping
		wantDst     map[string][]string
		wantCreates []string
		wantAppends int
	}{
		{
			name: "explicit mapping",
			src: map[string][]fakeMsg{
				"Work":         fakeMsgs("w1@x"),
				"Work/2023":    fakeMsgs("w2@x"),
				"Work/2023/Q1": fakeMsgs("w3@x"),
			},
			mappings:    []config.DirectoryMapping{mapping("Work", "Archive")},
			wantDst:     map[string][]string{"Archive": {"w1@x"}, "Archive/2023": {"w2@x"}, "Archive/2023/Q1": {"w3@x"}},
			wantCreates: []string{"Archive", "Archive/2023", "Archive/2023/Q1"},
			wantAppends: 3,
		},
		{
			name: "mirror mode",
			src: map[string][]fakeMsg{
				"INBOX":     nil,
				"Work":      fakeMsgs("w1@x"),
				"Work/2023": fakeMsgs("w2@x"),
			},
			wantDst:     map[string][]string{"Work": {"w1@x"}, "Work/2023": {"w2@x"}},
			wantCreates: []string{"Work", "Work/2023"},
			wantAppends: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil})
			opts := e2eOptions(&syncBuffer{}, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if err != nil {
				t.Fatalf("runSync: %v", err)
			}
			for folder, want := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), want...)
			}
			assertIDs(t, "dst CREATE names", slices.Sorted(slices.Values(dst.capturedNames("CREATE"))), tt.wantCreates...)
			if n := dst.callCount("APPEND"); n != tt.wantAppends {
				t.Errorf("dst APPEND count = %d, want %d: no message may land twice", n, tt.wantAppends)
			}
		})
	}
}
