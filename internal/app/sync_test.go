package app

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
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
	"unicode/utf8"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"github.com/greeddj/imapsync-go/internal/termsafe"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/urfave/cli/v3"
	"golang.org/x/time/rate"
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
	// Limiter is nil → recommend a concrete --bps-down value (27000 from
	// the Gmail provider profile).
	if !strings.Contains(got, "--bps-down 27000") {
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
	if !strings.Contains(got, "--bps-up 5000") {
		t.Errorf("warning missing --bps-up recommendation: %q", got)
	}
}

func Test_buildProviderWarning_When_SideHasADailyQuota_Then_NamesTheDailyCap(t *testing.T) {
	t.Parallel()

	const (
		header   = "\n⚠️  Provider with known IMAP limits detected:\n"
		conns    = "     • max simultaneous connections: 15\n"
		quota    = "     • daily quota: 2500 MB down, 500 MB up\n"
		notes    = "     • notes: Workspace IMAP bandwidth limits & 15 simultaneous connections per account\n"
		dstBlock = "   destination [imap.gmail.com:993] - Gmail\n"
		srcBlock = "   source [imap.gmail.com:993] - Gmail\n"
		dstCap   = "     • a run moves at most 500 MB a day to this account; a larger mailbox takes more than one day\n"
		srcCap   = "     • a run moves at most 2500 MB a day from this account; a larger mailbox takes more than one day\n"
		dstRec   = "     • no rate limit set - recommended: --bps-up 5000\n"
		srcRec   = "     • no rate limit set - recommended: --bps-down 27000\n"
	)
	tests := []struct {
		srcLim, dstLim *rate.Limiter
		name           string
		srcServer      string
		dstServer      string
		want           string
	}{
		{
			name: "Gmail destination without a limiter", srcServer: "mail.privatehost.example:993", dstServer: "imap.gmail.com:993",
			want: header + dstBlock + conns + quota + dstCap + dstRec + notes + "\n",
		},
		{
			name: "Gmail source without a limiter", srcServer: "imap.gmail.com:993", dstServer: "mail.privatehost.example:993",
			want: header + srcBlock + conns + quota + srcCap + srcRec + notes + "\n",
		},
		{
			name: "Gmail destination with a limiter", srcServer: "mail.privatehost.example:993", dstServer: "imap.gmail.com:993",
			dstLim: ratelimit.NewLimiter(5_000),
			want:   header + dstBlock + conns + quota + dstCap + notes + "\n",
		},
		{
			name: "Gmail source with a limiter", srcServer: "imap.gmail.com:993", dstServer: "mail.privatehost.example:993",
			srcLim: ratelimit.NewLimiter(27_000),
			want:   header + srcBlock + conns + quota + srcCap + notes + "\n",
		},
		{
			name: "Gmail on both sides without limiters", srcServer: "imap.gmail.com:993", dstServer: "imap.gmail.com:993",
			want: header + srcBlock + conns + quota + srcCap + srcRec + notes +
				dstBlock + conns + quota + dstCap + dstRec + notes + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Src:     config.Credentials{Server: tt.srcServer},
				Dst:     config.Credentials{Server: tt.dstServer},
				Workers: 4,
			}
			if got := buildProviderWarning(cfg, tt.srcLim, tt.dstLim); got != tt.want {
				t.Errorf("warning = %q\nwant      %q", got, tt.want)
			}
		})
	}
}

func Test_buildUploadLimitWarning_When_GmailRecommendationRunsWithMostWorkers_Then_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	p, ok := client.DetectProvider("imap.gmail.com:993")
	if !ok {
		t.Fatal("imap.gmail.com is not a known provider")
	}
	// 10 is the upper bound of the worker clamp in config.New.
	if got := buildUploadLimitWarning(p.UpBPS, 10); got != "" {
		t.Errorf("warning for the recommended upload rate = %q, want empty", got)
	}
}

func Test_delimiterRewrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		serverDelim   string
		typed         string
		wantRewritten string
		wantUsed      string
		listed        []string
		noSelect      []string
	}{
		{name: "listed name with a dot is kept", serverDelim: "/", listed: []string{"example.com"}, typed: "example.com"},
		{name: "listed name with a dot and a space is kept", serverDelim: "/", listed: []string{"Mr. Smith"}, typed: "Mr. Smith"},
		{name: "unlisted name with no listed rewrite is kept", serverDelim: "/", listed: []string{"INBOX"}, typed: "v1.2"},
		{
			name: "dot path is rewritten to a listed slash path", serverDelim: "/", listed: []string{"Archive/2024"},
			typed: "Archive.2024", wantRewritten: "Archive/2024", wantUsed: ".",
		},
		{
			name: "backslash path is rewritten to a listed slash path", serverDelim: "/", listed: []string{"Archive/2024"},
			typed: `Archive\2024`, wantRewritten: "Archive/2024", wantUsed: `\`,
		},
		{
			name: "slash path is rewritten to a listed dot path", serverDelim: ".", listed: []string{"Work.Sub"},
			typed: "Work/Sub", wantRewritten: "Work.Sub", wantUsed: "/",
		},
		{name: "listed name with a slash on a dot server is kept", serverDelim: ".", listed: []string{"INBOX.AC/DC"}, typed: "INBOX.AC/DC"},
		{
			name: "later delimiter is tried when the first rewrite is not listed", serverDelim: "/", listed: []string{"a.b/c"},
			typed: `a.b\c`, wantRewritten: "a.b/c", wantUsed: `\`,
		},
		{
			name: "listed noselect rewrite counts as existing", serverDelim: ".", noSelect: []string{"Work.Sub"},
			typed: "Work/Sub", wantRewritten: "Work.Sub", wantUsed: "/",
		},
		{name: "flat server offers no rewrite", serverDelim: "", listed: []string{"Archive/2024"}, typed: "Archive.2024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			folders := make(map[string][]fakeMsg, len(tt.listed))
			for _, name := range tt.listed {
				folders[name] = nil
			}
			srv := newStoreServer(t, tt.serverDelim, folders)
			srv.markNoSelect(tt.noSelect...)
			c := newAppClient(t, srv, "srv")

			rewritten, used, err := delimiterRewrite(e2eContext(t), c, tt.typed)

			if err != nil {
				t.Fatalf("delimiterRewrite(%q): %v", tt.typed, err)
			}
			if rewritten != tt.wantRewritten || used != tt.wantUsed {
				t.Errorf("delimiterRewrite(%q) = (%q, %q), want (%q, %q)",
					tt.typed, rewritten, used, tt.wantRewritten, tt.wantUsed)
			}
		})
	}
}

func Test_delimiterRewrite_When_ContextEnded_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": nil})
	c := newAppClient(t, srv, "srv")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rewritten, used, err := delimiterRewrite(ctx, c, "Archive.2024")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if rewritten != "" || used != "" {
		t.Errorf("delimiterRewrite = (%q, %q), want no rewrite", rewritten, used)
	}
}

func Test_convertFolderPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		srcDelimiter string
		dstDelimiter string
		want         string
		wantRenamed  bool
	}{
		{name: "same delimiter keeps a dot", path: "example.com", srcDelimiter: "/", dstDelimiter: "/", want: "example.com"},
		{name: "slash path takes the destination delimiter", path: "Work/Sub", srcDelimiter: "/", dstDelimiter: ".", want: "Work.Sub"},
		{name: "dot in a name becomes an underscore", path: "Mr. Smith", srcDelimiter: "/", dstDelimiter: ".", want: "Mr_ Smith", wantRenamed: true},
		{name: "dot path takes the destination delimiter", path: "INBOX.Work", srcDelimiter: ".", dstDelimiter: "/", want: "INBOX/Work"},
		{name: "slash in a name becomes an underscore", path: "INBOX.AC/DC", srcDelimiter: ".", dstDelimiter: "/", want: "INBOX/AC_DC", wantRenamed: true},
		{name: "dot in a later segment keeps the levels", path: "Clients/acme.com", srcDelimiter: "/", dstDelimiter: ".", want: "Clients.acme_com", wantRenamed: true},
		{name: "dot in a single segment", path: "v1.2", srcDelimiter: "/", dstDelimiter: ".", want: "v1_2", wantRenamed: true},
		{name: "source delimiter becomes a level", path: "v1/2", srcDelimiter: "/", dstDelimiter: ".", want: "v1.2"},
		{name: "flat destination keeps the source delimiter as text", path: "Work/Sub", srcDelimiter: "/", dstDelimiter: "", want: "Work/Sub"},
		{name: "flat source keeps a name without the destination delimiter", path: "Mr. Smith", srcDelimiter: "", dstDelimiter: "/", want: "Mr. Smith"},
		{name: "flat source renames the destination delimiter", path: "Mr. Smith", srcDelimiter: "", dstDelimiter: ".", want: "Mr_ Smith", wantRenamed: true},
		{name: "flat source renames a slash for a slash destination", path: "Work/2023", srcDelimiter: "", dstDelimiter: "/", want: "Work_2023", wantRenamed: true},
		{name: "backslash source", path: `a\b.c`, srcDelimiter: `\`, dstDelimiter: ".", want: "a.b_c", wantRenamed: true},
		{name: "underscore destination uses a hyphen", path: "a_b/c", srcDelimiter: "/", dstDelimiter: "_", want: "a-b_c", wantRenamed: true},
		{name: "empty path", path: "", srcDelimiter: "/", dstDelimiter: ".", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, renamed := convertFolderPath(tt.path, tt.srcDelimiter, tt.dstDelimiter)
			if got != tt.want || renamed != tt.wantRenamed {
				t.Errorf("convertFolderPath(%q, %q, %q) = (%q, %t), want (%q, %t)",
					tt.path, tt.srcDelimiter, tt.dstDelimiter, got, renamed, tt.want, tt.wantRenamed)
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
		{name: "workersEqualMaxConn", workers: 10, maxConn: 10, planCnt: 20, want: 10},
		{name: "overBudgetClampedToMaxConn", workers: 10, maxConn: 3, planCnt: 20, want: 3},
		{name: "maxConnBelowWorkers", workers: 4, maxConn: 2, planCnt: 20, want: 2},
		{name: "maxConnOne", workers: 4, maxConn: 1, planCnt: 20, want: 1},
		{name: "unlimitedMaxConn", workers: 4, maxConn: 0, planCnt: 20, want: 4},
		{name: "negativeMaxConnIsUnlimited", workers: 4, maxConn: -1, planCnt: 20, want: 4},
		{name: "fewerPlansThanWorkers", workers: 4, maxConn: 15, planCnt: 2, want: 2},
		{name: "fewerPlansThanMaxConn", workers: 4, maxConn: 3, planCnt: 2, want: 2},
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

func Test_expandMappingsWithSubfolders_addsChildrenAndDedups(t *testing.T) {
	srcSrv := newFakeServer(t)

	srcSrv.addConnHandler(customListHandler(srcSrv, []string{"jira", "jira/DEVOPS", "jira/SCD"}))

	srcC := newAppClient(t, srcSrv, "src")

	delimiter := srcC.GetDelimiter() // "/"

	mappings1 := rootMappings(mapping("jira", "jira"))
	dstC := newDestinationClient(t, delimiter)
	got1, _, err := expandMappingsWithSubfolders(context.Background(), srcC, dstC, mappings1, delimiter, delimiter)
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

	mappings2 := rootMappings(mapping("jira", "jira"), mapping("jira/DEVOPS", "DEVOPS"))
	got2, _, err := expandMappingsWithSubfolders(context.Background(), srcC, dstC, mappings2, delimiter, delimiter)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders run2: %v", err)
	}
	if len(got2) != 3 {
		t.Fatalf("run2 len=%d, want 3, got %v", len(got2), got2)
	}
	// The explicit mapping names jira/DEVOPS itself, so its destination
	// DEVOPS wins over the derived jira/DEVOPS.
	var devopsMapping folderMapping
	for _, m := range got2 {
		if m.Source == "jira/DEVOPS" {
			devopsMapping = m
			break
		}
	}
	if devopsMapping.Destination != "DEVOPS" {
		t.Errorf("jira/DEVOPS destination=%q, want DEVOPS (the explicit mapping wins)", devopsMapping.Destination)
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

	// The explicit mapping and the derived one agree on the destination, so one is dropped. Verbose prints the found-subfolder and dropped-mapping lines.
	mappings := rootMappings(mapping("INBOX", "INBOX"), mapping("INBOX/Sub", "INBOX/Sub"))
	var out strings.Builder
	dstC := newDestinationClient(t, delimiter)
	got, report, err := expandMappingsWithSubfolders(context.Background(), srcC, dstC, mappings, delimiter, delimiter)
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	printExpansion(&out, report, got, true, false)
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

	dstC := newDestinationClient(t, "/")
	_, _, err := expandMappingsWithSubfolders(ctx, srcC, dstC, rootMappings(mapping("INBOX", "INBOX")), "/", "/")
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
		want         []folderMapping
	}{
		{
			name: "flat source gets no subfolders",
			want: rootMappings(mapping("INBOX", "TARGET")),
		},
		{
			name:         "flat destination keeps the source relative name",
			srcDelimiter: "/",
			want:         rootMappings(mapping("INBOX", "TARGET"), mapping("INBOX/Sub", "TARGET/Sub")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcSrv := newFakeServer(t)
			srcSrv.addConnHandler(customListHandler(srcSrv, []string{"INBOX", "INBOX/Sub"}))
			srcC := newAppClient(t, srcSrv, "src")

			dstC := newDestinationClient(t, tt.dstDelimiter)
			got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("INBOX", "TARGET")), tt.srcDelimiter, tt.dstDelimiter)

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

	mappings := rootMappings(mapping("jira", "jira"))
	dstC := newDestinationClient(t, ".")
	got, _, err := expandMappingsWithSubfolders(context.Background(), srcC, dstC, mappings, "/", ".")
	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2, got %v", len(got), got)
	}
	if got[1].Destination != "jira.DEVOPS" {
		t.Errorf("jira/DEVOPS destination=%q, want jira.DEVOPS", got[1].Destination)
	}
}

// Test_splitDestinationClashes checks that a destination shared with a renamed mapping fails every member of the group, and that a group without a rename stays.
func Test_splitDestinationClashes(t *testing.T) {
	t.Parallel()

	type clash struct{ source, destination, other string }
	plain := func(src, dst string) folderMapping { return folderMapping{DirectoryMapping: mapping(src, dst)} }
	renamed := func(src, dst string) folderMapping {
		return folderMapping{DirectoryMapping: mapping(src, dst), renamed: true}
	}
	tests := []struct {
		name        string
		in          []folderMapping
		wantKept    []string
		wantClashes []clash
	}{
		{
			name:     "no shared name",
			in:       []folderMapping{plain("A", "X"), renamed("B", "Y"), plain("C", "Z")},
			wantKept: []string{"A", "B", "C"},
		},
		{
			name:        "plain mapping then renamed mapping on one name",
			in:          []folderMapping{plain("a_b", "a_b"), renamed("a.b", "a_b")},
			wantClashes: []clash{{"a_b", "a_b", "a.b"}, {"a.b", "a_b", "a_b"}},
		},
		{
			name:        "renamed mapping then plain mapping on one name",
			in:          []folderMapping{renamed("a.b", "a_b"), plain("a_b", "a_b")},
			wantClashes: []clash{{"a.b", "a_b", "a_b"}, {"a_b", "a_b", "a.b"}},
		},
		{
			name:     "two plain mappings on one name",
			in:       []folderMapping{plain("A", "X"), plain("B", "X")},
			wantKept: []string{"A", "B"},
		},
		{
			name:        "plain mappings and a renamed one on one name",
			in:          []folderMapping{plain("A", "X"), plain("B", "X"), renamed("C", "X")},
			wantClashes: []clash{{"A", "X", "B"}, {"B", "X", "A"}, {"C", "X", "A"}},
		},
		{
			name:        "three renamed and plain mappings on one name",
			in:          []folderMapping{renamed("a.b", "a_b"), renamed("a:b", "a_b"), plain("a_b", "a_b")},
			wantClashes: []clash{{"a.b", "a_b", "a:b"}, {"a:b", "a_b", "a.b"}, {"a_b", "a_b", "a.b"}},
		},
		{
			name:        "source name with control characters stays raw in the error",
			in:          []folderMapping{renamed("v\x1b[2J.1", "v\x1b[2J_1"), plain("w", "v\x1b[2J_1")},
			wantClashes: []clash{{"v\x1b[2J.1", "v\x1b[2J_1", "w"}, {"w", "v\x1b[2J_1", "v\x1b[2J.1"}},
		},
		{
			name:        "unrelated mapping stays while the clashing group fails",
			in:          []folderMapping{renamed("a.b", "a_b"), plain("K", "K"), plain("a_b", "a_b")},
			wantKept:    []string{"K"},
			wantClashes: []clash{{"a.b", "a_b", "a_b"}, {"a_b", "a_b", "a.b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kept, clashes := splitDestinationClashes(tt.in)

			keptSources := make([]string, len(kept))
			for i, m := range kept {
				keptSources[i] = m.Source
			}
			if !slices.Equal(keptSources, tt.wantKept) {
				t.Errorf("kept sources = %q, want %q", keptSources, tt.wantKept)
			}
			if len(clashes) != len(tt.wantClashes) {
				t.Fatalf("clashes = %v, want %d", clashes, len(tt.wantClashes))
			}
			for i, want := range tt.wantClashes {
				got := clashes[i]
				if got.SourceFolder != want.source || got.DestinationFolder != want.destination {
					t.Errorf("clash %d = %q -> %q, want %q -> %q", i, got.SourceFolder, got.DestinationFolder, want.source, want.destination)
				}
				if !errors.Is(got.Err, errDestinationClash) {
					t.Errorf("clash %d error = %v, want errDestinationClash", i, got.Err)
				}
				if wantText := "same destination folder as source folder " + want.other; got.Err.Error() != wantText {
					t.Errorf("clash %d error text = %q, want %q", i, got.Err.Error(), wantText)
				}
			}
		})
	}
}

func Test_splitEmptyLevelDestinations(t *testing.T) {
	t.Parallel()

	plain := func(src, dst string) folderMapping { return folderMapping{DirectoryMapping: mapping(src, dst)} }
	type failure struct{ source, destination, text string }
	levelText := func(name, delimiter string) string {
		return fmt.Sprintf(`destination folder has an empty level: "%s", destination delimiter "%s"`, name, delimiter)
	}
	tests := []struct {
		name         string
		delimiter    string
		listed       []string
		noSelect     []string
		in           []folderMapping
		wantKept     []string
		wantFailures []failure
	}{
		{
			name:      "no empty level keeps every mapping",
			delimiter: "/",
			in:        []folderMapping{plain("A", "A"), plain("A/b", "A/b"), plain("c.d", "c.d")},
			wantKept:  []string{"A", "A/b", "c.d"},
		},
		{
			name:      "unlisted names with an empty level fail in order and the others stay",
			delimiter: "/",
			in:        []folderMapping{plain("A", "A"), plain("x/", "x/"), plain("B", "B"), plain("p", "a//b"), plain("q", "/X")},
			wantKept:  []string{"A", "B"},
			wantFailures: []failure{
				{"x/", "x/", levelText("x/", "/")},
				{"p", "a//b", levelText("a//b", "/")},
				{"q", "/X", levelText("/X", "/")},
			},
		},
		{
			name:      "listed name keeps its mapping",
			delimiter: "/",
			listed:    []string{"x/", "a//b"},
			in:        []folderMapping{plain("p", "x/"), plain("q", "a//b"), plain("r", "/X")},
			wantKept:  []string{"p", "q"},
			wantFailures: []failure{
				{"r", "/X", levelText("/X", "/")},
			},
		},
		{
			name:      "name that LIST marks as Noselect counts as listed",
			delimiter: "/",
			noSelect:  []string{"x/"},
			in:        []folderMapping{plain("p", "x/")},
			wantKept:  []string{"p"},
		},
		{
			name:      "dot server reads the dot as the delimiter",
			delimiter: ".",
			in:        []folderMapping{plain("p", "x."), plain("q", "a..b"), plain("r", "a/b//c")},
			wantKept:  []string{"r"},
			wantFailures: []failure{
				{"p", "x.", levelText("x.", ".")},
				{"q", "a..b", levelText("a..b", ".")},
			},
		},
		{
			name:      "flat server has no levels",
			delimiter: "",
			in:        []folderMapping{plain("p", "x/"), plain("q", "a//b"), plain("r", "/X"), plain("s", "x.")},
			wantKept:  []string{"p", "q", "r", "s"},
		},
		{
			name:      "names with control characters stay raw in the failure",
			delimiter: "/",
			in:        []folderMapping{plain("v\x1b[2J/", "v\x1b[2J/")},
			wantFailures: []failure{
				{"v\x1b[2J/", "v\x1b[2J/", levelText("v\x1b[2J/", "/")},
			},
		},
		{
			name:      "renamed flag does not change the check",
			delimiter: "/",
			in:        []folderMapping{{DirectoryMapping: mapping("p/q..r", "p_q//r"), renamed: true}},
			wantFailures: []failure{
				{"p/q..r", "p_q//r", levelText("p_q//r", "/")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			folders := map[string][]fakeMsg{"INBOX": nil}
			for _, name := range tt.listed {
				folders[name] = nil
			}
			dst := newStoreServer(t, tt.delimiter, folders)
			dst.markNoSelect(tt.noSelect...)
			dstC := newAppClient(t, dst, "dst")

			kept, failures, err := splitEmptyLevelDestinations(t.Context(), dstC, tt.in, tt.delimiter)

			if err != nil {
				t.Fatalf("splitEmptyLevelDestinations: %v", err)
			}
			keptSources := make([]string, len(kept))
			for i, m := range kept {
				keptSources[i] = m.Source
			}
			if !slices.Equal(keptSources, tt.wantKept) {
				t.Errorf("kept sources = %q, want %q", keptSources, tt.wantKept)
			}
			if len(failures) != len(tt.wantFailures) {
				t.Fatalf("failures = %v, want %d", failures, len(tt.wantFailures))
			}
			for i, want := range tt.wantFailures {
				got := failures[i]
				if got.SourceFolder != want.source || got.DestinationFolder != want.destination {
					t.Errorf("failure %d = %q -> %q, want %q -> %q", i, got.SourceFolder, got.DestinationFolder, want.source, want.destination)
				}
				if !errors.Is(got.Err, errEmptyLevel) {
					t.Errorf("failure %d error = %v, want errEmptyLevel", i, got.Err)
				}
				if got.Err.Error() != want.text {
					t.Errorf("failure %d error text = %q, want %q", i, got.Err.Error(), want.text)
				}
			}
		})
	}
}

func Test_splitEmptyLevelDestinations_When_ContextEnded_Then_ReturnsContextErrorOnlyForANameThatNeedsTheCheck(t *testing.T) {
	t.Parallel()

	dstC := newDestinationClient(t, "/")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	kept, failures, err := splitEmptyLevelDestinations(ctx, dstC, []folderMapping{{DirectoryMapping: mapping("A", "a/b")}}, "/")
	if err != nil || len(kept) != 1 || len(failures) != 0 {
		t.Errorf("name with no empty level = %v, %v, %v, want one kept mapping and no error", kept, failures, err)
	}
	kept, failures, err = splitEmptyLevelDestinations(ctx, dstC, []folderMapping{{DirectoryMapping: mapping("A", "a//b")}}, "/")
	if !errors.Is(err, context.Canceled) || kept != nil || failures != nil {
		t.Errorf("name with an empty level = %v, %v, %v, want context.Canceled and no result", kept, failures, err)
	}
}

func Test_splitRefusedDestinations(t *testing.T) {
	t.Parallel()

	errRefused := errors.New("CREATE refused by policy")
	plan := func(src, dst string) FolderSyncPlan {
		return FolderSyncPlan{SourceFolder: src, DestinationFolder: dst, NewMessages: 1, SrcUIDs: []uint32{1}}
	}
	type failure struct{ source, destination string }
	tests := []struct {
		refused      map[string]error
		name         string
		in           []FolderSyncPlan
		wantKept     []string
		wantFailures []failure
	}{
		{
			name:     "no refused name keeps every plan in order",
			refused:  map[string]error{},
			in:       []FolderSyncPlan{plan("A", "X"), plan("B", "Y"), plan("C", "Z")},
			wantKept: []string{"A>X", "B>Y", "C>Z"},
		},
		{
			name:     "nil map keeps every plan",
			in:       []FolderSyncPlan{plan("A", "X"), plan("B", "Y")},
			wantKept: []string{"A>X", "B>Y"},
		},
		{
			name:         "refused shared destination fails both plans in plan order",
			refused:      map[string]error{"S": errRefused},
			in:           []FolderSyncPlan{plan("A", "X"), plan("B", "S"), plan("C", "Y"), plan("D", "S"), plan("E", "Z")},
			wantKept:     []string{"A>X", "C>Y", "E>Z"},
			wantFailures: []failure{{"B", "S"}, {"D", "S"}},
		},
		{
			name:     "refused name that no plan uses changes nothing",
			refused:  map[string]error{"Unused": errRefused},
			in:       []FolderSyncPlan{plan("A", "X"), plan("B", "Y")},
			wantKept: []string{"A>X", "B>Y"},
		},
		{
			name:         "every plan refused leaves no plan",
			refused:      map[string]error{"X": errRefused, "Y": errRefused},
			in:           []FolderSyncPlan{plan("A", "X"), plan("B", "Y")},
			wantFailures: []failure{{"A", "X"}, {"B", "Y"}},
		},
		{
			name:         "destination bytes decide the match",
			refused:      map[string]error{"inbox/Sub": errRefused},
			in:           []FolderSyncPlan{plan("A", "INBOX/Sub"), plan("B", "inbox/Sub")},
			wantKept:     []string{"A>INBOX/Sub"},
			wantFailures: []failure{{"B", "inbox/Sub"}},
		},
		{
			name:     "no plan at all",
			refused:  map[string]error{"X": errRefused},
			wantKept: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pairs := func(plans []FolderSyncPlan) []string {
				out := make([]string, len(plans))
				for i, p := range plans {
					out[i] = p.SourceFolder + ">" + p.DestinationFolder
				}
				return out
			}
			before := pairs(tt.in)

			kept, failures := splitRefusedDestinations(tt.in, tt.refused)

			if got := pairs(kept); !slices.Equal(got, tt.wantKept) {
				t.Errorf("kept plans = %q, want %q", got, tt.wantKept)
			}
			if len(failures) != len(tt.wantFailures) {
				t.Fatalf("failures = %v, want %d", failures, len(tt.wantFailures))
			}
			for i, want := range tt.wantFailures {
				got := failures[i]
				if got.SourceFolder != want.source || got.DestinationFolder != want.destination {
					t.Errorf("failure %d = %q -> %q, want %q -> %q", i, got.SourceFolder, got.DestinationFolder, want.source, want.destination)
				}
				if !errors.Is(got.Err, errDestinationCreate) {
					t.Errorf("failure %d error = %v, want errDestinationCreate", i, got.Err)
				}
				if !errors.Is(got.Err, errRefused) {
					t.Errorf("failure %d error = %v, want the refusal behind it", i, got.Err)
				}
				if wantText := "cannot create destination folder: CREATE refused by policy"; got.Err.Error() != wantText {
					t.Errorf("failure %d error text = %q, want %q", i, got.Err.Error(), wantText)
				}
			}
			if len(kept) > 0 {
				kept[0].SourceFolder = "changed"
			}
			if after := pairs(tt.in); !slices.Equal(after, before) {
				t.Errorf("input plans = %q after the call, want %q: kept must not share the input slice", after, before)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_SegmentHoldsDestinationDelimiter_Then_ItBecomesUnderscore(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Projects": nil, "Projects/v1.2": nil, "Projects/v1/2": nil, "Projects/v1_2": nil,
	})
	srcC := newAppClient(t, src, "src")
	var out strings.Builder

	dstC := newDestinationClient(t, ".")
	got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("Projects", "Old.Projects")), "/", ".")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	printExpansion(&out, report, got, true, false)
	want := []folderMapping{
		{DirectoryMapping: mapping("Projects", "Old.Projects")},
		{DirectoryMapping: mapping("Projects/v1.2", "Old.Projects.v1_2"), renamed: true},
		{DirectoryMapping: mapping("Projects/v1/2", "Old.Projects.v1.2")},
		{DirectoryMapping: mapping("Projects/v1_2", "Old.Projects.v1_2")},
	}
	if !slices.Equal(got, want) {
		t.Errorf("mappings = %+v, want %+v", got, want)
	}
	assertContainsAll(t, out.String(), "Found subfolder: Projects/v1.2 → Old.Projects.v1_2\n")
}

func Test_expandMappingsWithSubfolders_When_RootIsRenamed_Then_SubfoldersAreRenamed(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"acme.com": nil, "acme.com/x": nil})
	srcC := newAppClient(t, src, "src")
	roots := []folderMapping{{DirectoryMapping: mapping("acme.com", "acme_com"), renamed: true}}

	dstC := newDestinationClient(t, ".")
	got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, roots, "/", ".")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	want := []folderMapping{
		{DirectoryMapping: mapping("acme.com", "acme_com"), renamed: true},
		{DirectoryMapping: mapping("acme.com/x", "acme_com.x"), renamed: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("mappings = %+v, want %+v", got, want)
	}
}

func Test_expandMappingsWithSubfolders_When_ListOrderVaries_Then_SubfoldersFollowByteOrder(t *testing.T) {
	t.Parallel()

	children := []string{"b", "a_b", "Z", "a", "10", "c", "a.b", "9", "B", "a b"}
	folders := map[string][]fakeMsg{"r": nil}
	for _, child := range children {
		folders["r/"+child] = nil
	}
	src := newStoreServer(t, "/", folders)
	srcC := newAppClient(t, src, "src")

	dstC := newDestinationClient(t, "/")
	got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("r", "r")), "/", "/")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	sources := make([]string, len(got))
	for i, m := range got {
		sources[i] = m.Source
	}
	want := []string{"r", "r/10", "r/9", "r/B", "r/Z", "r/a", "r/a b", "r/a.b", "r/a_b", "r/b", "r/c"}
	if !slices.Equal(sources, want) {
		t.Errorf("sources = %q, want %q", sources, want)
	}
}

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
			srcMap:      map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 700}}, "b@x": {{UID: 2, Size: 1300}}},
			dstMap:      map[string][]client.MessageRef{"b@x": {{UID: 40, Size: 1300}}},
			srcMessages: 2,
		}

		// First call: only one side arrived - must not diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}
		if s.srcMap == nil || s.dstMap == nil {
			t.Fatal("maps freed prematurely after first call")
		}

		// Second call: both sides arrived - diff fires.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after diff")
		}
		if s.dstMap != nil {
			t.Error("dstMap not nil after diff")
		}
		want := []uint32{1}
		if !slices.Equal(plans[0].SrcUIDs, want) {
			t.Errorf("SrcUIDs=%v, want %v", plans[0].SrcUIDs, want)
		}
		if totalNew.Load() != 1 {
			t.Errorf("totalNew=%d, want 1", totalNew.Load())
		}
		// The planned message is a@x, so its own size counts and nothing else does.
		if got := totalNewSize.Load(); got != 700 {
			t.Errorf("totalNewSize=%d, want 700", got)
		}
		if plans[0].NewSize != 700 {
			t.Errorf("plans[0].NewSize=%d, want 700", plans[0].NewSize)
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
			srcMap: map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 100}}},
			dstMap: map[string][]client.MessageRef{"b@x": {{UID: 1, Size: 100}}},
		}

		// First call: one side arrived - no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived - error branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after srcErr branch")
		}
		if s.dstMap != nil {
			t.Error("dstMap not nil after srcErr branch")
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
			srcMap: map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 100}}},
			dstMap: map[string][]client.MessageRef{"b@x": {{UID: 1, Size: 100}}},
		}

		// First call: one side arrived - no diff yet.
		if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned true on first call, want false")
		}

		// Second call: both sides arrived - dstErr branch must free maps and
		// leave the plan empty.
		if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
			t.Fatal("maybeDiff returned false on second call, want true")
		}
		if s.srcMap != nil {
			t.Error("srcMap not nil after dstErr branch")
		}
		if s.dstMap != nil {
			t.Error("dstMap not nil after dstErr branch")
		}
		if plans[0].SourceFolder != "" {
			t.Errorf("plan populated despite dstErr: %+v", plans[0])
		}
		if totalNew.Load() != 0 {
			t.Errorf("totalNew=%d, want 0", totalNew.Load())
		}
	})
}

func Test_maybeDiff_When_MessagesShareMessageId_Then_PlansByCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src       map[string][]client.MessageRef
		dst       map[string][]client.MessageRef
		name      string
		wantUIDs  []uint32
		wantSize  uint64
		wantExtra int
	}{
		{
			name:     "three messages and none on the destination",
			src:      map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:      map[string][]client.MessageRef{},
			wantUIDs: []uint32{4, 5, 6},
			wantSize: 60,
		},
		{
			name:      "three messages and one on the destination",
			src:       map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:       map[string][]client.MessageRef{"s@x": msgRefs(90)},
			wantUIDs:  []uint32{5, 6},
			wantSize:  50,
			wantExtra: 2,
		},
		{
			name: "three messages and three on the destination",
			src:  map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:  map[string][]client.MessageRef{"s@x": msgRefs(90, 91, 92)},
		},
		{
			name: "three messages and five on the destination",
			src:  map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:  map[string][]client.MessageRef{"s@x": msgRefs(90, 91, 92, 93, 94)},
		},
		{
			name: "unique ids, a repeated id and an id that only the destination holds",
			src: map[string][]client.MessageRef{
				"u1@x":   {{UID: 1, Size: 5}},
				"r@x":    {{UID: 2, Size: 10}, {UID: 3, Size: 20}, {UID: 7, Size: 40}},
				"u2@x":   {{UID: 4, Size: 1}},
				"same@x": {{UID: 5, Size: 100}},
			},
			dst: map[string][]client.MessageRef{
				"r@x":    msgRefs(50),
				"same@x": msgRefs(51),
				"only@x": msgRefs(52),
			},
			wantUIDs:  []uint32{1, 3, 4, 7},
			wantSize:  66,
			wantExtra: 2,
		},
		{
			name: "unique ids only, and the destination holds one of them",
			src: map[string][]client.MessageRef{
				"a@x": {{UID: 1, Size: 10}},
				"b@x": {{UID: 2, Size: 20}},
			},
			dst:      map[string][]client.MessageRef{"b@x": {{UID: 90, Size: 20}}},
			wantUIDs: []uint32{1},
			wantSize: 10,
		},
		{
			name: "a repeated id with one copy on the destination and a unique id that is not there",
			src: map[string][]client.MessageRef{
				"r@x": {{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}},
				"u@x": {{UID: 4, Size: 5}},
			},
			dst:       map[string][]client.MessageRef{"r@x": msgRefs(90)},
			wantUIDs:  []uint32{2, 3, 4},
			wantSize:  55,
			wantExtra: 2,
		},
		{
			name:      "the destination copy has the size of the last source copy",
			src:       map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:       map[string][]client.MessageRef{"s@x": {{UID: 90, Size: 30}}},
			wantUIDs:  []uint32{4, 5},
			wantSize:  30,
			wantExtra: 2,
		},
		{
			name:      "the destination copies have the sizes of the first and the last source copy",
			src:       map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:       map[string][]client.MessageRef{"s@x": {{UID: 90, Size: 10}, {UID: 91, Size: 30}}},
			wantUIDs:  []uint32{5},
			wantSize:  20,
			wantExtra: 1,
		},
		{
			name:      "one destination copy matches by size and the other falls back to the lowest UID",
			src:       map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}},
			dst:       map[string][]client.MessageRef{"s@x": {{UID: 90, Size: 30}, {UID: 91, Size: 99}}},
			wantUIDs:  []uint32{5},
			wantSize:  20,
			wantExtra: 1,
		},
		{
			name:      "all source copies have one size",
			src:       map[string][]client.MessageRef{"s@x": {{UID: 4, Size: 10}, {UID: 5, Size: 10}, {UID: 6, Size: 10}}},
			dst:       map[string][]client.MessageRef{"s@x": {{UID: 90, Size: 10}}},
			wantUIDs:  []uint32{5, 6},
			wantSize:  20,
			wantExtra: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{mapping("Src", "Dst")}
			plans := make([]FolderSyncPlan, 1)
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64
			s := &folderScan{srcMap: tt.src, dstMap: tt.dst, srcMessages: countRefs(tt.src), dstExists: true}

			maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount)
			if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
				t.Fatal("maybeDiff returned false on the second call, want true")
			}

			if s.srcMap != nil || s.dstMap != nil {
				t.Errorf("maps not freed: srcMap=%v dstMap=%v", s.srcMap, s.dstMap)
			}
			if len(tt.wantUIDs) == 0 {
				if plans[0].SourceFolder != "" {
					t.Errorf("plan = %+v, want no plan", plans[0])
				}
				if totalNew.Load() != 0 || totalNewSize.Load() != 0 {
					t.Errorf("totals = %d messages, %d bytes, want 0 and 0", totalNew.Load(), totalNewSize.Load())
				}
				return
			}
			if !slices.Equal(plans[0].SrcUIDs, tt.wantUIDs) {
				t.Errorf("SrcUIDs = %v, want %v", plans[0].SrcUIDs, tt.wantUIDs)
			}
			if !slices.IsSorted(plans[0].SrcUIDs) {
				t.Errorf("SrcUIDs = %v, want ascending order", plans[0].SrcUIDs)
			}
			if plans[0].NewMessages != len(tt.wantUIDs) {
				t.Errorf("NewMessages = %d, want %d", plans[0].NewMessages, len(tt.wantUIDs))
			}
			if plans[0].NewSize != tt.wantSize {
				t.Errorf("NewSize = %d, want %d", plans[0].NewSize, tt.wantSize)
			}
			if plans[0].ExtraCopies != tt.wantExtra {
				t.Errorf("ExtraCopies = %d, want %d", plans[0].ExtraCopies, tt.wantExtra)
			}
			if got := totalNew.Load(); got != int64(len(tt.wantUIDs)) {
				t.Errorf("totalNew = %d, want %d", got, len(tt.wantUIDs))
			}
			if got := totalNewSize.Load(); got != tt.wantSize {
				t.Errorf("totalNewSize = %d, want %d", got, tt.wantSize)
			}
			if !plans[0].DestinationFolderExists {
				t.Error("DestinationFolderExists = false, want true")
			}
		})
	}
}

// reservedCounts maps each Message-Id to the number of copies that earlier plans reserved for it.
func reservedCounts(byID map[string][]reservation) map[string]int {
	counts := make(map[string]int, len(byID))
	for id, list := range byID {
		counts[id] = len(list)
	}
	return counts
}

func Test_maybeDiff_When_PlansShareADestination_Then_LaterPlansCopyOnlyTheExcess(t *testing.T) {
	t.Parallel()

	// A message of UID n has size 10*n, so the expected size follows from the expected UIDs.
	refs := func(uids ...uint32) []client.MessageRef {
		out := make([]client.MessageRef, len(uids))
		for i, uid := range uids {
			out[i] = client.MessageRef{UID: uid, Size: 10 * uid}
		}
		return out
	}
	type slot struct {
		src       map[string][]client.MessageRef
		dst       map[string][]client.MessageRef
		wantHeld  map[string]int
		srcErr    error
		dstErr    error
		dest      string
		wantUIDs  []uint32
		wantExtra int
		noSelect  bool
	}
	tests := []struct {
		name     string
		slots    []slot
		noShared bool
	}{
		{
			name: "a message in both folders lands in the first plan",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"a": refs(1), "s": refs(2)}, wantUIDs: []uint32{1, 2}, wantHeld: map[string]int{"a": 1, "s": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"s": refs(7), "b": refs(8)}, wantUIDs: []uint32{8}},
			},
		},
		{
			name: "swapped order moves the shared message to the new first plan",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"s": refs(7), "b": refs(8)}, wantUIDs: []uint32{7, 8}, wantHeld: map[string]int{"s": 1, "b": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"a": refs(1), "s": refs(2)}, wantUIDs: []uint32{1}},
			},
		},
		{
			name: "the destination ends with the largest count",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1, 2)}, dst: map[string][]client.MessageRef{"x": refs(90)}, wantUIDs: []uint32{2}, wantExtra: 1, wantHeld: map[string]int{"x": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(5, 6, 7)}, dst: map[string][]client.MessageRef{"x": refs(90)}, wantUIDs: []uint32{7}, wantExtra: 1},
			},
		},
		{
			name: "a copy that only an earlier mapping reserved does not count",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1)}, wantUIDs: []uint32{1}, wantHeld: map[string]int{"x": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(5, 6)}, wantUIDs: []uint32{6}},
			},
		},
		{
			name: "three plans add up",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1, 2)}, dst: map[string][]client.MessageRef{"x": refs(90)}, wantUIDs: []uint32{2}, wantExtra: 1, wantHeld: map[string]int{"x": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(5, 6, 7, 8)}, dst: map[string][]client.MessageRef{"x": refs(90)}, wantUIDs: []uint32{7, 8}, wantExtra: 2, wantHeld: map[string]int{"x": 3}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(11, 12)}, dst: map[string][]client.MessageRef{"x": refs(90)}},
			},
		},
		{
			name: "a later plan that the earlier one covers has no plan",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"m1": refs(1), "m2": refs(2)}, wantUIDs: []uint32{1, 2}, wantHeld: map[string]int{"m1": 1, "m2": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"m1": refs(5)}},
			},
		},
		{
			name: "a failed first plan reserves nothing",
			slots: []slot{
				{dest: "D", srcErr: errors.New("scan failed"), dst: map[string][]client.MessageRef{"x": refs(90)}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(5, 6), "y": refs(7)}, dst: map[string][]client.MessageRef{"x": refs(90)}, wantUIDs: []uint32{6, 7}, wantExtra: 1},
			},
		},
		{
			name: "a failed last plan releases the counts",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1)}, wantUIDs: []uint32{1}, wantHeld: map[string]int{"x": 1}},
				{dest: "D", srcErr: errors.New("scan failed")},
			},
		},
		{
			name: "a last plan with a destination error releases the counts",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1)}, wantUIDs: []uint32{1}, wantHeld: map[string]int{"x": 1}},
				{dest: "D", dstErr: errors.New("scan failed")},
			},
		},
		{
			name: "a last plan for a Noselect destination releases the counts",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(1)}, wantUIDs: []uint32{1}, wantHeld: map[string]int{"x": 1}},
				{dest: "D", src: map[string][]client.MessageRef{"x": refs(5)}, noSelect: true},
			},
		},
		{
			name: "another destination in between is not changed",
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"a": refs(1)}, wantUIDs: []uint32{1}, wantHeld: map[string]int{"a": 1}},
				{dest: "E", src: map[string][]client.MessageRef{"a": refs(2)}, wantUIDs: []uint32{2}},
				{dest: "D", src: map[string][]client.MessageRef{"a": refs(3), "c": refs(4)}, wantUIDs: []uint32{4}},
			},
		},
		{
			name:     "no shared destination",
			noShared: true,
			slots: []slot{
				{dest: "D", src: map[string][]client.MessageRef{"a": refs(1)}, wantUIDs: []uint32{1}},
				{dest: "E", src: map[string][]client.MessageRef{"a": refs(2)}, wantUIDs: []uint32{2}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := make([]config.DirectoryMapping, len(tt.slots))
			for i, sl := range tt.slots {
				mappings[i] = mapping("S"+strconv.Itoa(i), sl.dest)
			}
			shared := newSharedDestinations(mappings)
			if (shared == nil) != tt.noShared {
				t.Fatalf("newSharedDestinations is nil = %t, want %t", shared == nil, tt.noShared)
			}
			plans := make([]FolderSyncPlan, len(tt.slots))
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64
			var wantTotal int
			var wantTotalSize uint64

			for i, sl := range tt.slots {
				s := &folderScan{
					srcMap: sl.src, dstMap: sl.dst, srcErr: sl.srcErr, dstErr: sl.dstErr, dstNoSelect: sl.noSelect,
					srcMessages: countRefs(sl.src), dstExists: true,
				}
				maybeDiff(s, i, mappings, plans, &totalNew, &totalNewSize, shared, copyByCount)
				if !maybeDiff(s, i, mappings, plans, &totalNew, &totalNewSize, shared, copyByCount) {
					t.Fatalf("slot %d: maybeDiff returned false on the second call, want true", i)
				}

				var wantSize uint64
				for _, uid := range sl.wantUIDs {
					wantSize += 10 * uint64(uid)
				}
				wantTotal += len(sl.wantUIDs)
				wantTotalSize += wantSize
				got := plans[i]
				if len(sl.wantUIDs) == 0 {
					if got.SourceFolder != "" {
						t.Errorf("slot %d: plan = %+v, want no plan", i, got)
					}
				} else {
					if !slices.Equal(got.SrcUIDs, sl.wantUIDs) {
						t.Errorf("slot %d: SrcUIDs = %v, want %v", i, got.SrcUIDs, sl.wantUIDs)
					}
					if got.NewMessages != len(sl.wantUIDs) || got.NewSize != wantSize {
						t.Errorf("slot %d: NewMessages = %d and NewSize = %d, want %d and %d", i, got.NewMessages, got.NewSize, len(sl.wantUIDs), wantSize)
					}
					if got.ExtraCopies != sl.wantExtra {
						t.Errorf("slot %d: ExtraCopies = %d, want %d", i, got.ExtraCopies, sl.wantExtra)
					}
				}
				if s.srcMap != nil || s.dstMap != nil {
					t.Errorf("slot %d: maps not freed: srcMap=%v dstMap=%v", i, s.srcMap, s.dstMap)
				}
				if shared == nil {
					continue
				}
				byID, has := shared.reserved[sl.dest]
				if held := reservedCounts(byID); has != (len(sl.wantHeld) > 0) || !maps.Equal(held, sl.wantHeld) {
					t.Errorf("slot %d: reserved[%s] counts = %v (present %t), want %v", i, sl.dest, held, has, sl.wantHeld)
				}
			}

			if got := totalNew.Load(); got != int64(wantTotal) {
				t.Errorf("totalNew = %d, want %d", got, wantTotal)
			}
			if got := totalNewSize.Load(); got != wantTotalSize {
				t.Errorf("totalNewSize = %d, want %d", got, wantTotalSize)
			}
			if shared != nil && len(shared.reserved) != 0 {
				t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
			}
		})
	}
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

func rootMappings(in ...config.DirectoryMapping) []folderMapping {
	return typedMappings(in)
}

// newDestinationClient dials a destination that lists only INBOX, so ResolveName changes no name except the letter case of an INBOX level.
func newDestinationClient(t *testing.T, delimiter string) *client.Client {
	t.Helper()
	return newAppClient(t, newStoreServer(t, delimiter, map[string][]fakeMsg{"INBOX": nil}), "dst")
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

// renderScreen models only the sequences that the progress writer emits. Any other escape is an error, so a test never passes on a misread screen.
func renderScreen(raw string) ([]string, error) {
	rows := [][]rune{nil}
	row, col := 0, 0
	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\n':
			row++
			col = 0
			if row == len(rows) {
				rows = append(rows, nil)
			}
		case '\r':
			col = 0
		case '\x1b':
			if i+1 >= len(runes) || runes[i+1] != '[' {
				return nil, fmt.Errorf("escape at rune %d does not start a CSI sequence", i)
			}
			start := i + 2
			end := start
			for end < len(runes) && (runes[end] == ';' || runes[end] >= '0' && runes[end] <= '9') {
				end++
			}
			if end == len(runes) {
				return nil, fmt.Errorf("CSI sequence at rune %d has no final byte", i)
			}
			params := string(runes[start:end])
			switch final := runes[end]; final {
			case 'm':
			case 'A':
				n := 1
				if params != "" {
					var err error
					if n, err = strconv.Atoi(params); err != nil {
						return nil, fmt.Errorf("cursor-up at rune %d has parameters %q: %w", i, params, err)
					}
				}
				row = max(row-n, 0)
			case 'K':
				if params != "" && params != "0" {
					return nil, fmt.Errorf("erase at rune %d has parameters %q", i, params)
				}
				if col < len(rows[row]) {
					rows[row] = rows[row][:col]
				}
			default:
				return nil, fmt.Errorf("unsupported escape %q at rune %d", "\x1b["+params+string(final), i)
			}
			i = end
		default:
			for len(rows[row]) < col {
				rows[row] = append(rows[row], ' ')
			}
			if col < len(rows[row]) {
				rows[row][col] = r
			} else {
				rows[row] = append(rows[row], r)
			}
			col++
		}
	}
	screen := make([]string, len(rows))
	for i, cells := range rows {
		screen[i] = strings.TrimRight(string(cells), " ")
	}
	// The cursor row is empty after a final newline and is not a line the user sees.
	if last := len(screen) - 1; screen[last] == "" {
		screen = screen[:last]
	}
	return screen, nil
}

func screenRows(t *testing.T, raw string) []string {
	t.Helper()
	rows, err := renderScreen(raw)
	if err != nil {
		t.Fatalf("render screen: %v\noutput: %q", err, raw)
	}
	return rows
}

func Test_renderScreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "plain rows", raw: "a\nb\n", want: []string{"a", "b"}},
		{name: "last row without newline", raw: "a\nb", want: []string{"a", "b"}},
		{name: "empty output", raw: "", want: []string{}},
		{name: "blank row between rows", raw: "a\n\nb\n", want: []string{"a", "", "b"}},
		{name: "cursor up and erase replace a row", raw: "a\nb\n\x1b[A\x1b[Kc\n", want: []string{"a", "c"}},
		{name: "cursor up by a count", raw: "a\nb\nc\n\x1b[2A\x1b[Kx\n", want: []string{"a", "x", "c"}},
		{name: "erase at column 0 empties the row", raw: "abcdef\r\x1b[K", want: []string{}},
		{name: "erase from the cursor to the end of the row", raw: "abcdef\rab\x1b[0K", want: []string{"ab"}},
		{name: "SGR is dropped", raw: "\x1b[92m\x1b[1;31mred\x1b[0m text\n", want: []string{"red text"}},
		{name: "carriage return overwrites from column 0", raw: "abcdef\rxy\n", want: []string{"xycdef"}},
		{name: "cursor up at row 0 stays at row 0", raw: "\x1b[A\x1b[5Aa\n", want: []string{"a"}},
		{name: "trailing spaces are removed", raw: "a   \nb\n", want: []string{"a", "b"}},
		{name: "wide runes advance by one column", raw: "⚠️ ✓ done\n", want: []string{"⚠️ ✓ done"}},
		{name: "erase screen is refused", raw: "\x1b[2J", wantErr: true},
		{name: "escape without a bracket is refused", raw: "a\x1bb", wantErr: true},
		{name: "escape at the end is refused", raw: "a\x1b", wantErr: true},
		{name: "unfinished CSI is refused", raw: "a\x1b[12", wantErr: true},
		{name: "erase with an unknown parameter is refused", raw: "a\x1b[2K", wantErr: true},
		{name: "cursor up with a list is refused", raw: "a\x1b[1;2A", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := renderScreen(tt.raw)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("renderScreen(%q) = %q, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("renderScreen(%q): %v", tt.raw, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("renderScreen(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func rowsHolding(rows []string, parts ...string) []int {
	var found []int
	for i, row := range rows {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(row, part) }) {
			found = append(found, i)
		}
	}
	return found
}

// assertOneRow checks that exactly one row holds every part and returns its index, or -1 when that is not so.
func assertOneRow(t *testing.T, rows []string, parts ...string) int {
	t.Helper()
	found := rowsHolding(rows, parts...)
	if len(found) != 1 {
		t.Errorf("%d rows hold all of %q, want 1\nscreen:\n%s", len(found), parts, strings.Join(rows, "\n"))
		return -1
	}
	return found[0]
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
	assertNoWriteCommands(t, name, srv)
	if n := srv.seenMutatingFetches(); n != 0 {
		t.Errorf("%s saw %d fetches that set \\Seen, want 0", name, n)
	}
}

// assertNoWriteCommands is assertReadOnly without the \Seen check, which needs a store-backed server.
func assertNoWriteCommands(t *testing.T, name string, srv *fakeServer) {
	t.Helper()
	for _, verb := range []string{"APPEND", "CREATE", "STORE", "UID STORE", "COPY", "UID COPY", "MOVE", "UID MOVE", "EXPUNGE", "UID EXPUNGE", "DELETE", "RENAME", "SELECT"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("%s saw %d %s commands, want 0", name, n, verb)
		}
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

func TestRunSync_When_DestinationListsTheRewrittenPath_Then_PathIsRewrittenAfterConfirmation(t *testing.T) {
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
		},
		{
			name:  "both prompts are answered from one input stream",
			input: "y\ny\n",
			wantOutput: []string{
				"Fix folder delimiters to match server configuration? [y/N]",
				"Proceed with synchronization? [y/N]",
				"Fixed destination",
			},
			wantFolder: "Archive.2024",
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
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil, "Archive.2024": nil})
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

func TestRunSync_When_DelimiterIsRewritten_Then_ConfigMapKeepsTheTypedNames(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, ".", map[string][]fakeMsg{"Work.Sub": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Work/Sub", "Work/Sub"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	if want := []config.DirectoryMapping{mapping("Work/Sub", "Work/Sub")}; !slices.Equal(cfg.Map, want) {
		t.Errorf("cfg.Map = %+v, want %+v", cfg.Map, want)
	}
}

func TestRunSync_When_InputEndsAtDelimiterPrompt_Then_ReturnsReadErrorBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": fakeMsgs("m1@x")})
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil, "Archive.2024": nil})
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

func TestRunSync_When_MirroredNameHoldsDelimiterText_Then_FolderIsCopiedUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		folder       string
		srcDelimiter string
		dstDelimiter string
	}{
		{name: "domain name on slash servers", folder: "example.com", srcDelimiter: "/", dstDelimiter: "/"},
		{name: "dot and space on slash servers", folder: "Mr. Smith", srcDelimiter: "/", dstDelimiter: "/"},
		{name: "slash on dot servers", folder: "AC/DC", srcDelimiter: ".", dstDelimiter: "."},
		{name: "dot and space on a flat source", folder: "Mr. Smith", srcDelimiter: "", dstDelimiter: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.srcDelimiter, map[string][]fakeMsg{
				"INBOX":   fakeMsgs("m1@x"),
				tt.folder: fakeMsgs("m2@x", "m3@x"),
			})
			dst := newStoreServer(t, tt.dstDelimiter, map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertIDs(t, "dst "+tt.folder, dst.folderMsgIDs(tt.folder), "m2@x", "m3@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.folder)
			for _, name := range src.capturedNames("EXAMINE") {
				if name != "INBOX" && name != tt.folder {
					t.Errorf("source EXAMINE of %q, want only INBOX or %q", name, tt.folder)
				}
			}
			assertContainsNone(t, out.String(), "delimiter mismatch", "Fixed source", "Fixed destination", "is renamed to")

			appends := dst.callCount("APPEND")
			if appends != 3 {
				t.Fatalf("first run did %d APPEND commands, want 3", appends)
			}
			second := &syncBuffer{}
			again := e2eOptions(second, "")
			again.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, again); err != nil {
				t.Fatalf("second runSync: %v", err)
			}
			assertContainsAll(t, second.String(), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
		})
	}
}

func TestRunSync_When_TypedNameIsListed_Then_NoDelimiterPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		folder    string
		delimiter string
		viaFlags  bool
	}{
		{name: "config map with a dot on slash servers", folder: "example.com", delimiter: "/"},
		{name: "folder flags with a dot on slash servers", folder: "example.com", delimiter: "/", viaFlags: true},
		{name: "config map with a slash on dot servers", folder: "AC/DC", delimiter: "."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.delimiter, map[string][]fakeMsg{
				"INBOX":   nil,
				tt.folder: fakeMsgs("m1@x", "m2@x"),
			})
			dst := newStoreServer(t, tt.delimiter, map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping(tt.folder, tt.folder))
			out := &syncBuffer{}
			// The only answer is for the "Proceed" prompt: a delimiter prompt would take it.
			opts := e2eOptions(out, "y\n")
			if tt.viaFlags {
				cfg.Map = nil
				opts.srcFolder, opts.dstFolder = tt.folder, tt.folder
			}

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertContainsAll(t, out.String(), "Proceed with synchronization? [y/N]")
			assertContainsNone(t, out.String(), "Fix folder delimiters", "delimiter mismatch")
			assertIDs(t, "dst "+tt.folder, dst.folderMsgIDs(tt.folder), "m1@x", "m2@x")
		})
	}
}

func TestRunSync_When_TypedNameAndItsRewriteAreMissing_Then_NameIsKeptWithoutPrompt(t *testing.T) {
	t.Parallel()

	t.Run("source", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
		cfg := e2eConfig(src, dst, 1, mapping("Archive.2024", "Archive.2024"))
		out := &syncBuffer{}

		err := runSync(e2eContext(t), cfg, e2eOptions(out, ""))

		if !errors.Is(err, ErrSilentExit) {
			t.Fatalf("error = %v, want ErrSilentExit", err)
		}
		assertContainsAll(t, out.String(), "Folders that cannot be synced", "Archive.2024", "cannot scan source folder")
		assertContainsNone(t, out.String(), "Fix folder delimiters")
		examined := src.capturedNames("EXAMINE")
		if !slices.Contains(examined, "Archive.2024") {
			t.Errorf("source EXAMINE names = %v, want them to contain Archive.2024", examined)
		}
		if slices.Contains(examined, "Archive/2024") {
			t.Errorf("source EXAMINE names = %v, want no rewritten name", examined)
		}
	})

	t.Run("destination", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": fakeMsgs("m1@x")})
		dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
		cfg := e2eConfig(src, dst, 1, mapping("Archive/2024", "Archive/2024"))
		out := &syncBuffer{}

		if err := runSync(e2eContext(t), cfg, e2eOptions(out, "y\n")); err != nil {
			t.Fatalf("runSync: %v", err)
		}

		assertContainsNone(t, out.String(), "Fix folder delimiters")
		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Archive/2024")
		assertIDs(t, "dst Archive/2024", dst.folderMsgIDs("Archive/2024"), "m1@x")
	})
}

func TestRunSync_When_MirrorCrossesDelimiters_Then_DestinationUsesItsDelimiterWithoutPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src         map[string][]fakeMsg
		wantDst     map[string][]string
		name        string
		wantSrcOpen []string
		wantCreates []string
	}{
		{
			name:        "listed parent",
			src:         map[string][]fakeMsg{"Work": fakeMsgs("a@x"), "Work/Sub": fakeMsgs("b@x")},
			wantDst:     map[string][]string{"Work": {"a@x"}, "Work.Sub": {"b@x"}},
			wantSrcOpen: []string{"Work", "Work/Sub"},
			wantCreates: []string{"Work", "Work.Sub"},
		},
		{
			name:        "unlisted parent",
			src:         map[string][]fakeMsg{"Work/Sub": fakeMsgs("b@x")},
			wantDst:     map[string][]string{"Work.Sub": {"b@x"}},
			wantSrcOpen: []string{"Work/Sub"},
			wantCreates: []string{"Work", "Work.Sub"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), e2eOptions(out, "y\n")); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertContainsNone(t, out.String(), "Fix folder delimiters", "delimiter mismatch", "is renamed to")
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
			for _, name := range src.capturedNames("EXAMINE") {
				if !slices.Contains(tt.wantSrcOpen, name) {
					t.Errorf("source EXAMINE of %q, want only %v", name, tt.wantSrcOpen)
				}
			}
		})
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

func TestRunSync_When_FolderFlagsNameAFolderWithSubfolders_Then_EachSubfolderIsCopiedBelowTheDestinationFolder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src          map[string][]fakeMsg
		wantDst      map[string][]string
		name         string
		srcDelimiter string
		wantSrcOpen  []string
		wantCreates  []string
		absentDst    []string
		wantOut      []string
		wantNoOut    []string
		wantAppends  int
	}{
		{
			name:         "both servers use slash",
			srcDelimiter: "/",
			src: map[string][]fakeMsg{
				"A":          fakeMsgs("a1@x"),
				"A/Sub":      fakeMsgs("s1@x"),
				"A/Sub/Deep": fakeMsgs("d1@x"),
				"Ab":         fakeMsgs("x1@x"),
				"Other":      fakeMsgs("o1@x"),
			},
			wantDst:     map[string][]string{"B": {"a1@x"}, "B/Sub": {"s1@x"}, "B/Sub/Deep": {"d1@x"}},
			wantCreates: []string{"B", "B/Sub", "B/Sub/Deep"},
			wantSrcOpen: []string{"A", "A/Sub", "A/Sub/Deep"},
			absentDst:   []string{"A", "Ab", "Other"},
			wantNoOut:   []string{"delimiter mismatch", "Fix folder delimiters", "is renamed to"},
			wantAppends: 3,
		},
		{
			name:         "source uses dot",
			srcDelimiter: ".",
			src: map[string][]fakeMsg{
				"A":          fakeMsgs("a1@x"),
				"A.Sub":      fakeMsgs("s1@x"),
				"A.Sub.Deep": fakeMsgs("d1@x"),
				"Ab":         fakeMsgs("x1@x"),
			},
			wantDst:     map[string][]string{"B": {"a1@x"}, "B/Sub": {"s1@x"}, "B/Sub/Deep": {"d1@x"}},
			wantCreates: []string{"B", "B/Sub", "B/Sub/Deep"},
			wantSrcOpen: []string{"A", "A.Sub", "A.Sub.Deep"},
			absentDst:   []string{"A", "Ab", "A.Sub", "A.Sub.Deep"},
			wantNoOut:   []string{"delimiter mismatch", "Fix folder delimiters", "is renamed to"},
			wantAppends: 3,
		},
		{
			name:         "a source level holds the destination delimiter",
			srcDelimiter: ".",
			src: map[string][]fakeMsg{
				"A":     fakeMsgs("a1@x"),
				"A.x/y": fakeMsgs("s1@x"),
			},
			wantDst:     map[string][]string{"B": {"a1@x"}, "B/x_y": {"s1@x"}},
			wantCreates: []string{"B", "B/x_y"},
			wantSrcOpen: []string{"A", "A.x/y"},
			absentDst:   []string{"A", "A.x/y", "B/x/y"},
			wantOut:     []string{"Folder A.x/y is renamed to B/x_y"},
			wantAppends: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.srcDelimiter, tt.src)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1)

			first := runSyncConfirmed(t, cfg, "A", "B")
			appends, creates := dst.callCount("APPEND"), dst.callCount("CREATE")
			second := runSyncConfirmed(t, cfg, "A", "B")

			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			for _, folder := range tt.absentDst {
				if dst.hasFolder(folder) {
					t.Errorf("dst has %s, a folder the options do not copy", folder)
				}
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
			if appends != tt.wantAppends {
				t.Errorf("dst APPEND count after the first run = %d, want %d", appends, tt.wantAppends)
			}
			examined := src.capturedNames("EXAMINE")
			for _, name := range examined {
				if !slices.Contains(tt.wantSrcOpen, name) {
					t.Errorf("source EXAMINE of %q, want only %v", name, tt.wantSrcOpen)
				}
			}
			for _, name := range tt.wantSrcOpen {
				if !slices.Contains(examined, name) {
					t.Errorf("source EXAMINE names = %v, want them to contain %s", examined, name)
				}
			}
			assertContainsAll(t, first, tt.wantOut...)
			assertContainsNone(t, first, tt.wantNoOut...)
			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("dst APPEND count went from %d to %d on the second run", appends, n)
			}
			if n := dst.callCount("CREATE"); n != creates {
				t.Errorf("dst CREATE count went from %d to %d on the second run", creates, n)
			}
			assertReadOnly(t, "source", src)
		})
	}
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

func TestRunSync_When_OneCreateIsRefused_Then_OtherFoldersStillCopy(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Good":  fakeMsgs("m2@x"),
		"Bad":   fakeMsgs("m3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreateOf("Bad", "NO [CANNOT] Invalid folder name")
	cfg := e2eConfig(src, dst, 1)
	const badLine = "• Bad → Bad: cannot create destination folder: [dst] failed to create mailbox Bad: Invalid folder name"

	first := &syncBuffer{}
	opts := e2eOptions(first, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first run error = %v, want ErrSilentExit\noutput: %s", err, first.String())
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	assertIDs(t, "dst Good", dst.folderMsgIDs("Good"), "m2@x")
	assertIDs(t, "dst APPENDs to Bad", dst.appended("Bad"))
	if dst.hasFolder("Bad") {
		t.Error("dst has Bad, which it refused to create")
	}
	got := first.String()
	assertContainsAll(t, got, `Failed to create folder "Bad"`, badLine)
	assertInOrder(t, got, "Creating destination folders", folderFailuresHeader, badLine, "Syncing messages")
	assertContainsAll(t, got, "2 messages uploaded, 0 errors occurred, 1 folders failed")

	second := &syncBuffer{}
	opts = e2eOptions(second, "")
	opts.autoConfirm = true

	err = runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("second run error = %v, want ErrSilentExit\noutput: %s", err, second.String())
	}
	if n := dst.callCount("APPEND"); n != 2 {
		t.Errorf("dst saw %d APPEND commands over both runs, want 2", n)
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Bad", "Good", "Bad")
	// Run one logs in the planning client and one worker. Run two plans only, so it opens no worker.
	if n := dst.callCount("LOGIN"); n != 3 {
		t.Errorf("dst LOGIN count = %d, want 3", n)
	}
	assertContainsAll(t, second.String(), badLine, "0 messages uploaded, 0 errors occurred, 1 folders failed")
	assertContainsNone(t, second.String(), "Syncing messages")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_EveryCreateIsRefused_Then_FolderFailsAndNoWorkerConnects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "default output"},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseCreate()
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit", err)
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LOGIN"); n != 1 {
					t.Errorf("%s LOGIN count = %d, want 1: no worker connection after the last plan failed", name, n)
				}
			}
			got := out.String()
			assertContainsAll(t, got,
				"• Work → Work: cannot create destination folder: [dst] failed to create mailbox Work: CREATE refused by policy",
				"0 messages uploaded, 0 errors occurred, 1 folders failed",
			)
			assertContainsNone(t, got, "Syncing messages")
			if tt.quiet {
				assertContainsNone(t, got, "Messages to be copied", "Failed to create folder")
			} else {
				assertContainsAll(t, got, `Failed to create folder "Work"`)
			}
		})
	}
}

func TestRunSync_When_FolderCreationLogs_Then_EveryLogAndTheFinalTrackerLineStayOnScreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src      map[string][]fakeMsg
		dst      map[string][]fakeMsg
		setup    func(dst *fakeServer)
		check    func(t *testing.T, dst *fakeServer)
		name     string
		mappings []config.DirectoryMapping
		wantRows [][]string
		noRows   [][]string
		verbose  bool
		wantExit bool
	}{
		{
			name:     "every CREATE refused",
			src:      map[string][]fakeMsg{"Work": fakeMsgs("m1@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			setup:    func(dst *fakeServer) { dst.refuseCreate() },
			mappings: []config.DirectoryMapping{mapping("Work", "Work")},
			wantExit: true,
			wantRows: [][]string{
				{`Failed to create folder "Work"`, "CREATE refused by policy"},
				{"Created 0 of 1 folders, 1 refused", "✗ error"},
			},
			noRows: [][]string{{"Created", "✓ done"}},
		},
		{
			name:     "one of two refused",
			src:      map[string][]fakeMsg{"Good": fakeMsgs("m1@x"), "Bad": fakeMsgs("m2@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			setup:    func(dst *fakeServer) { dst.refuseCreateOf("Bad", "NO [CANNOT] Invalid folder name") },
			mappings: []config.DirectoryMapping{mapping("Good", "Good"), mapping("Bad", "Bad")},
			wantExit: true,
			wantRows: [][]string{
				{`Failed to create folder "Bad"`, "Invalid folder name"},
				{"Created 1 of 2 folders, 1 refused", "✗ error"},
			},
			noRows: [][]string{{"Created", "✓ done"}},
		},
		{
			name:     "throttled CREATE",
			src:      map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x"), "B": fakeMsgs("b1@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			setup:    func(dst *fakeServer) { dst.refuseCreateOf("A", gmailThrottleReply) },
			mappings: []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B")},
			wantExit: true,
			wantRows: [][]string{
				{`Failed to create folder "A"`},
				{"Created 0 of 2 folders, 1 refused", "✗ error"},
				{"⛔ Sync stopped"},
			},
			noRows: [][]string{{"Created", "✓ done"}},
		},
		{
			name:     "created, verbose",
			src:      map[string][]fakeMsg{"Good": fakeMsgs("m1@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			mappings: []config.DirectoryMapping{mapping("Good", "Good")},
			verbose:  true,
			wantRows: [][]string{
				{`Created folder "Good"`},
				{"Created 1 folders", "✓ done"},
			},
			noRows: [][]string{{"✗ error"}},
		},
		{
			name:     "already exists, verbose",
			src:      map[string][]fakeMsg{"A": fakeMsgs("a1@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil, "A": nil},
			setup:    func(dst *fakeServer) { dst.hideFromList("A") },
			mappings: []config.DirectoryMapping{mapping("A", "A")},
			verbose:  true,
			wantRows: [][]string{
				{"Folder A already exists"},
				{"Created 0 folders", "✓ done"},
			},
			noRows: [][]string{{"✗ error"}},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x")
			},
		},
		{
			name:     "parent refused below a created parent",
			src:      map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			setup:    func(dst *fakeServer) { dst.refuseCreateOf("Old/2020", "NO [CANNOT] Invalid folder name") },
			mappings: []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantExit: true,
			wantRows: [][]string{
				{`Failed to create folder "Old/2020/Inbox"`, "failed to create parent folder Old/2020"},
				{"Created 1 of 3 folders, 1 refused", "✗ error"},
			},
			noRows: [][]string{{"Created", "✓ done"}},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Old", "Old/2020")
				if !dst.hasFolder("Old") {
					t.Error("dst has no Old, which it accepted as the parent")
				}
				if dst.hasFolder("Old/2020") {
					t.Error("dst has Old/2020, which it refused to create")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "/", tt.dst)
			if tt.setup != nil {
				tt.setup(dst)
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, tt.verbose

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			switch {
			case tt.wantExit && !errors.Is(err, ErrSilentExit):
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			case !tt.wantExit && err != nil:
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}
			rows := screenRows(t, out.String())
			for _, parts := range tt.wantRows {
				assertOneRow(t, rows, parts...)
			}
			for _, parts := range tt.noRows {
				if found := rowsHolding(rows, parts...); len(found) != 0 {
					t.Errorf("rows %v hold all of %q, want none\nscreen:\n%s", found, parts, strings.Join(rows, "\n"))
				}
			}
			if tt.check != nil {
				tt.check(t, dst)
			}
		})
	}
}

func TestRunSync_When_CreateOfASharedDestinationIsRefused_Then_EveryMappingOfItFails(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x"), "C": fakeMsgs("c1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreateOf("Shared", "NO [CANNOT] Invalid folder name")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("A", "Shared"), mapping("B", "Shared"), mapping("C", "Other")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertInOrder(t, got,
		folderFailuresHeader,
		"• A → Shared: cannot create destination folder: [dst] failed to create mailbox Shared: Invalid folder name",
		"• B → Shared: cannot create destination folder: [dst] failed to create mailbox Shared: Invalid folder name",
		"Syncing messages",
	)
	assertContainsAll(t, got, "1 messages uploaded, 0 errors occurred, 2 folders failed")
	assertIDs(t, "dst Other", dst.folderMsgIDs("Other"), "c1@x")
	assertIDs(t, "dst APPENDs to Shared", dst.appended("Shared"))
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Other", "Shared")
	if slices.Contains(dst.capturedNames("EXAMINE"), "Shared") {
		t.Error("dst received EXAMINE for Shared, which it refused to create")
	}
	// One plan is left, so the pool holds one worker: the dropped plans must not size it.
	for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
		if n := srv.callCount("LOGIN"); n != 2 {
			t.Errorf("%s LOGIN count = %d, want 2", name, n)
		}
	}
}

func TestRunSync_When_ParentCreateIsRefused_Then_FoldersBelowItFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src         map[string][]fakeMsg
		copied      map[string]string
		name        string
		refused     string
		mappings    []config.DirectoryMapping
		wantLines   []string
		wantCreates []string
		wantAbsent  []string
	}{
		{
			name: "parent and child are both planned",
			src: map[string][]fakeMsg{
				"Bad":     fakeMsgs("m1@x"),
				"Bad/Sub": fakeMsgs("m2@x"),
				"Good":    fakeMsgs("m3@x"),
			},
			refused: "Bad",
			wantLines: []string{
				"• Bad → Bad: cannot create destination folder: [dst] failed to create mailbox Bad: ",
				"• Bad/Sub → Bad/Sub: cannot create destination folder: [dst] failed to create parent folder Bad: ",
			},
			wantCreates: []string{"Bad", "Bad", "Good"},
			copied:      map[string]string{"Good": "m3@x"},
			wantAbsent:  []string{"Bad", "Bad/Sub"},
		},
		{
			name: "parent has no plan",
			src: map[string][]fakeMsg{
				"Top/Child": fakeMsgs("m1@x"),
				"Good":      fakeMsgs("m2@x"),
			},
			refused:  "Top",
			mappings: []config.DirectoryMapping{mapping("Top/Child", "Top/Child"), mapping("Good", "Good")},
			wantLines: []string{
				"• Top/Child → Top/Child: cannot create destination folder: [dst] failed to create parent folder Top: ",
			},
			wantCreates: []string{"Good", "Top"},
			copied:      map[string]string{"Good": "m2@x"},
			wantAbsent:  []string{"Top", "Top/Child"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseCreateOf(tt.refused, "NO [CANNOT] Invalid folder name")
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertInOrder(t, out.String(), append([]string{folderFailuresHeader}, tt.wantLines...)...)
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
			for folder, id := range tt.copied {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), id)
			}
			for _, folder := range tt.wantAbsent {
				if dst.hasFolder(folder) {
					t.Errorf("dst has %s, which it refused to create", folder)
				}
				assertIDs(t, "dst APPENDs to "+folder, dst.appended(folder))
			}
		})
	}
}

func TestRunSync_When_ContextEndsDuringCreateAfterARefusal_Then_ReturnsCanceledWithoutFailureList(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, nth int) (string, bool) {
		if verb != "CREATE" {
			return "", false
		}
		if nth == 1 {
			return "NO [CANNOT] refused", true
		}
		cancel()
		return "", true
	}))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertContainsNone(t, out.String(), folderFailuresHeader, "cannot create destination folder")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
}

func TestRunSync_When_CreateIsRefusedAndAnotherDestinationIsHidden_Then_OnlyTheHiddenOneIsRechecked(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Hidden": fakeMsgs("m1@x", "m2@x"), "Bad": fakeMsgs("m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Hidden": fakeMsgs("m1@x")})
	dst.hideFromList("Hidden")
	dst.refuseCreateOf("Bad", "NO [CANNOT] Invalid folder name")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Hidden", "Hidden"), mapping("Bad", "Bad")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	assertInOrder(t, out.String(),
		"• Bad → Bad: cannot create destination folder: [dst] failed to create mailbox Bad: Invalid folder name",
		"already exists and holds 1 of the 2 planned messages",
		"Syncing messages",
	)
	if slices.Contains(dst.capturedNames("EXAMINE"), "Bad") {
		t.Error("dst received EXAMINE for Bad, which it refused to create")
	}
	assertIDs(t, "dst Hidden", dst.folderMsgIDs("Hidden"), "m1@x", "m2@x")
	assertIDs(t, "dst APPENDs to Bad", dst.appended("Bad"))
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

func TestRunSync_When_SourceReturnsEmptyBody_Then_MessageFailsOnEachRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{name: "NIL", value: "NIL"},
		{name: "a zero-length literal", value: "{0}\r\n"},
		{name: "an empty quoted string", value: `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("good@x", "empty@x")})
			src.answerBody("empty@x", tt.value)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))

			first := &syncBuffer{}
			opts := e2eOptions(first, "")
			opts.autoConfirm = true
			opts.verbose = true
			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("first run error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, first.String(), "1 messages uploaded, 1 errors occurred", "source returned an empty body")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "good@x")
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("dst saw %d APPEND commands after the first run, want 1", n)
			}

			second := &syncBuffer{}
			opts = e2eOptions(second, "")
			opts.autoConfirm = true
			err = runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("second run error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, second.String(), "0 messages uploaded, 1 errors occurred")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "good@x")
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("dst saw %d APPEND commands after the second run, want 1 (the empty message gets none)", n)
			}
		})
	}
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
			dst:          map[string][]fakeMsg{"INBOX": nil, "Archive.2024": nil},
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

func TestRunSync_When_DestinationParentsAreMissing_Then_PreviewCounterAndWireAgree(t *testing.T) {
	t.Parallel()

	const header = "Folders to be created on destination:\n"
	oneMessage := map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")}
	tests := []struct {
		src         map[string][]fakeMsg
		dst         map[string][]fakeMsg
		setup       func(dst *fakeServer)
		arrived     map[string]string
		name        string
		dstDelim    string
		mappings    []config.DirectoryMapping
		wantFolders []string
		quiet       bool
	}{
		{
			name:        "two parents are missing",
			src:         oneMessage,
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantFolders: []string{"Old", "Old/2020", "Old/2020/Inbox"},
			arrived:     map[string]string{"Old/2020/Inbox": "m1@x"},
		},
		{
			name:        "the first parent is listed",
			src:         oneMessage,
			dst:         map[string][]fakeMsg{"INBOX": nil, "Old": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantFolders: []string{"Old/2020", "Old/2020/Inbox"},
			arrived:     map[string]string{"Old/2020/Inbox": "m1@x"},
		},
		{
			name:        "the first parent is a container that cannot hold messages",
			src:         oneMessage,
			dst:         map[string][]fakeMsg{"INBOX": nil},
			setup:       func(dst *fakeServer) { dst.markNoSelect("Old") },
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantFolders: []string{"Old/2020", "Old/2020/Inbox"},
			arrived:     map[string]string{"Old/2020/Inbox": "m1@x"},
		},
		{
			name:        "the destination server is flat",
			src:         oneMessage,
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantFolders: []string{"Old/2020/Inbox"},
			arrived:     map[string]string{"Old/2020/Inbox": "m1@x"},
		},
		{
			name:        "two folders share a parent",
			src:         map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("A", "Old/A"), mapping("B", "Old/B")},
			wantFolders: []string{"Old", "Old/A", "Old/B"},
			arrived:     map[string]string{"Old/A": "a@x", "Old/B": "b@x"},
		},
		{
			name:        "the parent has its own plan after the child",
			src:         map[string][]fakeMsg{"X": fakeMsgs("x@x"), "Y": fakeMsgs("y@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("X", "Top/Sub"), mapping("Y", "Top")},
			wantFolders: []string{"Top", "Top/Sub"},
			arrived:     map[string]string{"Top/Sub": "x@x", "Top": "y@x"},
		},
		{
			name:        "quiet run shows no preview",
			src:         oneMessage,
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020/Inbox")},
			wantFolders: []string{"Old", "Old/2020", "Old/2020/Inbox"},
			arrived:     map[string]string{"Old/2020/Inbox": "m1@x"},
			quiet:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, tt.dstDelim, tt.dst)
			if tt.setup != nil {
				tt.setup(dst)
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "y\n")
			if tt.quiet {
				opts = e2eOptions(out, "")
				opts.quiet, opts.autoConfirm = true, true
			}

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			got := out.String()
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantFolders...)
			for folder, id := range tt.arrived {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), id)
			}
			if tt.quiet {
				assertContainsNone(t, got, "Folders to be created", "Created")
				return
			}
			block := header
			for _, folder := range tt.wantFolders {
				block += "• " + folder + "\n"
			}
			block += "\n📨 Total new messages to sync: " + strconv.Itoa(len(tt.arrived))
			assertInOrder(t, got, block, "✍️ Proceed with synchronization?", "Creating destination folders")
			start := strings.Index(got, header)
			end := strings.Index(got, "\n📨 Total new messages")
			if start < 0 || end < start {
				t.Fatalf("output has no list of folders to create before the total\noutput: %s", got)
			}
			if n := strings.Count(got[start+len(header):end], "• "); n != len(tt.wantFolders) {
				t.Errorf("preview lists %d folders, want %d", n, len(tt.wantFolders))
			}
			rows := screenRows(t, got)
			assertOneRow(t, rows, fmt.Sprintf("Created %d folders", len(tt.wantFolders)), "✓ done")
		})
	}
}

func TestRunSync_When_NewDestinationHasMoreMissingLevelsThanThePreviewShows_Then_PreviewIsCappedAndEveryLevelIsCreated(t *testing.T) {
	t.Parallel()

	const levels = previewFolderLimit + 3
	parts := numberedFolders(levels)
	missing := make([]string, levels)
	for i := range missing {
		missing[i] = strings.Join(parts[:i+1], "/")
	}
	deepest := missing[levels-1]
	wantBlock := foldersToCreateHeader + folderBullets(missing[:previewFolderLimit]...) +
		fmt.Sprintf("• … and %d more\n", levels-previewFolderLimit) + "\n📨 Total new messages to sync"
	newServers := func(t *testing.T) (src, dst *fakeServer) {
		t.Helper()
		src = newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
		dst = newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
		return src, dst
	}

	t.Run("declined", func(t *testing.T) {
		t.Parallel()

		src, dst := newServers(t)
		out := &syncBuffer{}

		err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", deepest)), e2eOptions(out, "n\n"))

		if err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		assertContainsAll(t, out.String(), wantBlock, "Sync canceled by user")
		if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
			t.Errorf("dst saw %d CREATE or APPEND commands after the answer n, want 0", n)
		}
	})

	t.Run("confirmed", func(t *testing.T) {
		t.Parallel()

		src, dst := newServers(t)
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true

		if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", deepest)), opts); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}

		assertContainsAll(t, out.String(), wantBlock)
		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), missing...)
		assertOneRow(t, screenRows(t, out.String()), fmt.Sprintf("Created %d folders", levels), "✓ done")
		assertIDs(t, "dst "+deepest, dst.folderMsgIDs(deepest), "m1@x")
	})
}

func TestRunSync_When_TypedDestinationHasAnEmptyLevel_Then_RunIsRefusedBeforeAnyCommandThatChangesTheDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		delimiter   string
		destination string
	}{
		{name: "empty first level", delimiter: "/", destination: "/X"},
		{name: "empty level in the middle", delimiter: "/", destination: "a//b"},
		{name: "empty last level", delimiter: "/", destination: "x/"},
		{name: "only the delimiter", delimiter: "/", destination: "/"},
		{name: "empty first level on a dot server", delimiter: ".", destination: ".X"},
		{name: "empty level in the middle on a dot server", delimiter: ".", destination: "a..b"},
		{name: "empty last level on a dot server", delimiter: ".", destination: "x."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Work": fakeMsgs("w1@x")})
			dst := newStoreServer(t, tt.delimiter, map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Fine"), mapping("INBOX", tt.destination)), e2eOptions(out, "y\n"))

			if !errors.Is(err, errEmptyLevel) {
				t.Fatalf("error = %v, want errEmptyLevel\noutput: %s", err, out.String())
			}
			want := fmt.Sprintf(`map entry 2: destination folder has an empty level: "%s", destination delimiter "%s"`, tt.destination, tt.delimiter)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if got := out.String(); got != "" {
				t.Errorf("output = %q, want none: no preview and no question", got)
			}
			assertNoCommandsAfterSetup(t, "src", src)
			assertNoCommandsAfterSetup(t, "dst", dst)
		})
	}
}

func TestRunSync_When_DestinationFlagHasAnEmptyLevel_Then_ErrorNamesTheFlag(t *testing.T) {
	t.Parallel()

	for _, destination := range []string{"/X", "a//b", "x/"} {
		t.Run(destination, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "y\n")
			opts.srcFolder, opts.dstFolder = "INBOX", destination

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "Ignored")), opts)

			if !errors.Is(err, errEmptyLevel) {
				t.Fatalf("error = %v, want errEmptyLevel\noutput: %s", err, out.String())
			}
			want := fmt.Sprintf(`--dest-folder: destination folder has an empty level: "%s", destination delimiter "/"`, destination)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if got := out.String(); got != "" {
				t.Errorf("output = %q, want none: no preview and no question", got)
			}
			assertNoCommandsAfterSetup(t, "src", src)
			assertNoCommandsAfterSetup(t, "dst", dst)
		})
	}
}

func TestRunSync_When_DestinationNameWithEmptyLevelCarriesControlCharacters_Then_ErrorHoldsTheRawNameAndEscapeMakesItSafe(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", hostileName+"//b")), e2eOptions(&syncBuffer{}, "y\n"))

	if !errors.Is(err, errEmptyLevel) {
		t.Fatalf("error = %v, want errEmptyLevel", err)
	}
	if !strings.Contains(err.Error(), hostileName+"//b") {
		t.Errorf("error = %q, want the raw name %q", err.Error(), hostileName+"//b")
	}
	shown := termsafe.Escape(err.Error())
	assertContainsAll(t, shown, hostileShownName+"//b")
	assertContainsNone(t, shown, hostileName, "\u202e", "\x1b[2J", "\rx")
	assertNoCommandsAfterSetup(t, "dst", dst)
}

func TestRunSync_When_DestinationListsTheNameWithAnEmptyLevel_Then_ItIsNotRefusedAndSyncs(t *testing.T) {
	t.Parallel()

	for _, destination := range []string{"/X", "a//b", "x/"} {
		t.Run(destination, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, destination: nil})
			out := &syncBuffer{}

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", destination)), e2eOptions(out, "y\n")); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertIDs(t, "dst "+destination, dst.folderMsgIDs(destination), "m1@x")
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("dst saw %d CREATE commands, want 0 for a listed folder", n)
			}
		})
	}
}

func TestRunSync_When_DestinationListsTheNameWithAnEmptyLevelAsNoSelect_Then_ItIsNotRefusedAsAnEmptyLevel(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.markNoSelect("a//b")
	out := &syncBuffer{}

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "a//b")), e2eOptions(out, "y\n"))

	if errors.Is(err, errEmptyLevel) {
		t.Fatalf("error = %v, want no errEmptyLevel for a listed name", err)
	}
	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit for the \\Noselect destination\noutput: %s", err, out.String())
	}
	assertContainsAll(t, out.String(), `destination folder cannot hold messages (\Noselect)`)
}

func TestRunSync_When_DestinationDelimiterIsNotTheRepeatedCharacter_Then_TheNameIsNotRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		delimiter   string
		destination string
	}{
		{name: "slash names on a dot server, first level", delimiter: ".", destination: "/X"},
		{name: "slash names on a dot server, middle", delimiter: ".", destination: "a//b"},
		{name: "slash names on a dot server, last level", delimiter: ".", destination: "x/"},
		{name: "dot names on a slash server", delimiter: "/", destination: "a..b"},
		{name: "slash names on a flat server, first level", delimiter: "", destination: "/X"},
		{name: "slash names on a flat server, middle", delimiter: "", destination: "a//b"},
		{name: "slash names on a flat server, last level", delimiter: "", destination: "x/"},
		{name: "dot names on a flat server", delimiter: "", destination: ".X..y."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, tt.delimiter, map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", tt.destination)), e2eOptions(out, "y\n")); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertIDs(t, "dst "+tt.destination, dst.folderMsgIDs(tt.destination), "m1@x")
			if !slices.Contains(dst.capturedNames("CREATE"), tt.destination) {
				t.Errorf("dst CREATE names = %q, want %q among them", dst.capturedNames("CREATE"), tt.destination)
			}
		})
	}
}

func Test_checkDestinationLevels_When_ContextEnded_Then_ReturnsContextErrorOnlyForANameWithAnEmptyLevel(t *testing.T) {
	t.Parallel()

	dst := newDestinationClient(t, "/")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := checkDestinationLevels(ctx, dst, []config.DirectoryMapping{mapping("INBOX", "a/b")}, ""); err != nil {
		t.Errorf("checkDestinationLevels for a name with no empty level = %v, want nil", err)
	}
	if err := checkDestinationLevels(ctx, dst, []config.DirectoryMapping{mapping("INBOX", "a//b")}, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("checkDestinationLevels for a name with an empty level = %v, want context.Canceled", err)
	}
}

func Test_hasEmptyLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		delimiter string
		want      bool
	}{
		{name: "plain name", input: "Work", delimiter: "/", want: false},
		{name: "nested name", input: "Work/2023/Q1", delimiter: "/", want: false},
		{name: "leading delimiter", input: "/X", delimiter: "/", want: true},
		{name: "trailing delimiter", input: "x/", delimiter: "/", want: true},
		{name: "repeated delimiter", input: "a//b", delimiter: "/", want: true},
		{name: "three delimiters", input: "a///b", delimiter: "/", want: true},
		{name: "only the delimiter", input: "/", delimiter: "/", want: true},
		{name: "other character repeated", input: "a..b", delimiter: "/", want: false},
		{name: "dot repeated on a dot server", input: "a..b", delimiter: ".", want: true},
		{name: "backslash and slash on a dot server", input: `\X/`, delimiter: ".", want: false},
		{name: "multi-byte delimiter", input: "a::b", delimiter: "::", want: false},
		{name: "multi-byte delimiter repeated", input: "a::::b", delimiter: "::", want: true},
		{name: "plain name on a flat server", input: "Work", delimiter: "", want: false},
		{name: "leading slash on a flat server", input: "/X", delimiter: "", want: false},
		{name: "repeated slash on a flat server", input: "a//b", delimiter: "", want: false},
		{name: "empty name on a flat server", input: "", delimiter: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hasEmptyLevel(tt.input, tt.delimiter); got != tt.want {
				t.Errorf("hasEmptyLevel(%q, %q) = %t, want %t", tt.input, tt.delimiter, got, tt.want)
			}
		})
	}
}

// assertNoCommandsAfterSetup checks that the server saw only the commands of the connect phase.
func assertNoCommandsAfterSetup(t *testing.T, side string, srv *fakeServer) {
	t.Helper()

	for _, verb := range []string{"SELECT", "EXAMINE", "STATUS", "FETCH", "UID", "UID FETCH", "UID SEARCH", "CREATE", "APPEND"} {
		if n := srv.callCount(verb); n != 0 {
			t.Errorf("%s saw %d %s commands, want 0", side, n, verb)
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for verb, n := range srv.counts {
		switch verb {
		case "LOGIN", "CAPABILITY", "LIST", "LOGOUT":
		default:
			t.Errorf("%s saw %d %s commands, want only LOGIN, CAPABILITY, LIST and LOGOUT", side, n, verb)
		}
	}
}

func assertNoCommandNamesFolder(t *testing.T, side string, srv *fakeServer, folder string) {
	t.Helper()

	for _, verb := range []string{"SELECT", "EXAMINE", "STATUS", "CREATE"} {
		if slices.Contains(srv.capturedNames(verb), folder) {
			t.Errorf("%s saw %s %q, want no command for it", side, verb, folder)
		}
	}
	if ids := srv.appended(folder); len(ids) != 0 {
		t.Errorf("%s APPENDs to %q = %v, want none", side, folder, ids)
	}
}

func TestRunSync_When_MirroredNameHasAnEmptyLevel_Then_OnlyThatFolderFailsAndNoCommandNamesIt(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"x/", "a//b", "/X"} {
		for _, quiet := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s quiet=%t", name, quiet), func(t *testing.T) {
				t.Parallel()

				src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "A": fakeMsgs("a1@x", "a2@x"), name: fakeMsgs("f1@x")})
				dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
				out := &syncBuffer{}
				opts := e2eOptions(out, "y\n")
				if quiet {
					opts.quiet, opts.autoConfirm = true, true
				}

				err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

				if !errors.Is(err, ErrSilentExit) {
					t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
				}
				got := out.String()
				failureLine := fmt.Sprintf("• %s → %s: destination folder has an empty level: \"%s\", destination delimiter \"/\"\n", name, name, name)
				assertContainsAll(t, got, "Folders that cannot be synced", failureLine, "3 messages uploaded, 0 errors occurred, 1 folders failed")
				if quiet {
					if n := strings.Count(got, failureLine); n != 1 {
						t.Errorf("failure line shown %d times with --quiet, want 1\noutput: %s", n, got)
					}
				}
				assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i1@x")
				assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x", "a2@x")
				assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A")
				if n := dst.callCount("APPEND"); n != 3 {
					t.Errorf("dst saw %d APPEND commands, want 3", n)
				}
				if got := slices.Sorted(slices.Values(src.capturedNames("EXAMINE"))); !slices.Contains(got, "A") || !slices.Contains(got, "INBOX") {
					t.Errorf("src EXAMINE names = %q, want INBOX and A among them", got)
				}
				assertNoCommandNamesFolder(t, "src", src, name)
				assertNoCommandNamesFolder(t, "dst", dst, name)
			})
		}
	}
}

func TestRunSync_When_MirroredNameHasAnEmptyLevelAndNothingElseIsNew_Then_NoQuestionAndTheRunFails(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "x/": fakeMsgs("f1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x")})
	out := &syncBuffer{}

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), e2eOptions(out, ""))

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got,
		"• x/ → x/: destination folder has an empty level",
		"Sync completed with errors. 0 messages uploaded, 0 errors occurred, 1 folders failed",
	)
	assertContainsNone(t, got, "Proceed with synchronization?", "All folders already synced!")
	if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d CREATE or APPEND commands, want 0", n)
	}
	assertNoCommandNamesFolder(t, "src", src, "x/")
	assertNoCommandNamesFolder(t, "dst", dst, "x/")
}

func TestRunSync_When_ExpandedSubfolderHasAnEmptyLevel_Then_OnlyThatSubfolderFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		source      string
		destination string
	}{
		{name: "empty level in the middle", source: "Work//bad", destination: "Fine//bad"},
		{name: "empty last level", source: "Work/sub/", destination: "Fine/sub/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX":   fakeMsgs("i1@x"),
				"Work":    fakeMsgs("w1@x"),
				"Work/ok": fakeMsgs("o1@x"),
				tt.source: fakeMsgs("f1@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Fine")), e2eOptions(out, "y\n"))

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertContainsAll(t, out.String(),
				fmt.Sprintf("• %s → %s: destination folder has an empty level: \"%s\", destination delimiter \"/\"\n", tt.source, tt.destination, tt.destination),
				"2 messages uploaded, 0 errors occurred, 1 folders failed",
			)
			assertIDs(t, "dst Fine", dst.folderMsgIDs("Fine"), "w1@x")
			assertIDs(t, "dst Fine/ok", dst.folderMsgIDs("Fine/ok"), "o1@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Fine", "Fine/ok")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"))
			assertNoCommandNamesFolder(t, "src", src, tt.source)
			assertNoCommandNamesFolder(t, "dst", dst, tt.destination)
		})
	}
}

func TestRunSync_When_SubfoldersFailBeforeThePlan_Then_VerboseExpansionNamesOnlyThePlannedFolders(t *testing.T) {
	t.Parallel()

	const (
		clashDots   = "• Projects/v1.2 → Old.Projects.v1_2: same destination folder as source folder Projects/v1_2\n"
		clashPlain  = "• Projects/v1_2 → Old.Projects.v1_2: same destination folder as source folder Projects/v1.2\n"
		emptyLevel  = "• Projects//bad → Old.Projects..bad: destination folder has an empty level: \"Old.Projects..bad\", destination delimiter \".\"\n"
		foundInside = "  📁 Found subfolder: Projects/Box/in → Old.Projects.Box.in\n"
		foundOK     = "  📁 Found subfolder: Projects/ok → Old.Projects.ok\n"
		skipBox     = "  ℹ️  Folder Projects/Box skipped - the source server marks it \\Noselect\n"
		override    = "  ℹ️  Folder Projects/ok goes to Special, not to Old.Projects.ok: the mapping of Projects/ok wins over the mapping of Projects\n"
		countLine   = "📂 Found 2 subfolders, total folders to sync: 3\n"
	)
	failingTree := map[string][]fakeMsg{
		"INBOX":         nil,
		"Projects":      fakeMsgs("p1@x"),
		"Projects/v1.2": fakeMsgs("a1@x"),
		"Projects/v1_2": fakeMsgs("b1@x"),
		"Projects//bad": fakeMsgs("f1@x"),
	}
	withKeptChildren := maps.Clone(failingTree)
	withKeptChildren["Projects/ok"] = fakeMsgs("o1@x")
	withKeptChildren["Projects/Box/in"] = fakeMsgs("i1@x")
	withOverride := maps.Clone(failingTree)
	withOverride["Projects/ok"] = fakeMsgs("o1@x")
	keptDst := map[string][]string{"Old.Projects": {"p1@x"}, "Old.Projects.ok": {"o1@x"}, "Old.Projects.Box.in": {"i1@x"}}
	allFound := []string{"Found subfolder: Projects/v1.2", "Found subfolder: Projects/v1_2", "Found subfolder: Projects//bad", "Found 5 subfolders", "Found 4 subfolders"}
	tests := []struct {
		src          map[string][]fakeMsg
		wantDst      map[string][]string
		name         string
		wantSummary  string
		noSelect     []string
		mappings     []config.DirectoryMapping
		wantInOrder  []string
		wantFailures []string
		wantNone     []string
		wantFound    int
		verbose      bool
		quiet        bool
	}{
		{
			name:         "some subfolders remain after the splits",
			src:          withKeptChildren,
			noSelect:     []string{"Projects/Box"},
			mappings:     []config.DirectoryMapping{mapping("Projects", "Old.Projects")},
			verbose:      true,
			wantDst:      keptDst,
			wantInOrder:  []string{"Checking for subfolders...\n", foundInside, foundOK, skipBox, countLine, folderFailuresHeader},
			wantFailures: []string{clashDots, clashPlain, emptyLevel},
			wantFound:    2,
			wantNone:     allFound,
			wantSummary:  "3 messages uploaded, 0 errors occurred, 3 folders failed",
		},
		{
			name:         "no subfolder remains after the splits",
			src:          failingTree,
			mappings:     []config.DirectoryMapping{mapping("Projects", "Old.Projects")},
			verbose:      true,
			wantDst:      map[string][]string{"Old.Projects": {"p1@x"}},
			wantInOrder:  []string{"Checking for subfolders...\n", folderFailuresHeader},
			wantFailures: []string{clashDots, clashPlain, emptyLevel},
			wantNone:     append([]string{"subfolders, total folders to sync", "skipped"}, allFound...),
			wantSummary:  "1 messages uploaded, 0 errors occurred, 3 folders failed",
		},
		{
			name:         "a mapping that wins over its parent stays named",
			src:          withOverride,
			mappings:     []config.DirectoryMapping{mapping("Projects", "Old.Projects"), mapping("Projects/ok", "Special")},
			verbose:      true,
			wantDst:      map[string][]string{"Old.Projects": {"p1@x"}, "Special": {"o1@x"}},
			wantInOrder:  []string{override, folderFailuresHeader},
			wantFailures: []string{clashDots, clashPlain, emptyLevel},
			wantNone:     append([]string{"subfolders, total folders to sync"}, allFound...),
			wantSummary:  "2 messages uploaded, 0 errors occurred, 3 folders failed",
		},
		{
			name:         "without verbose",
			src:          withKeptChildren,
			noSelect:     []string{"Projects/Box"},
			mappings:     []config.DirectoryMapping{mapping("Projects", "Old.Projects")},
			wantDst:      keptDst,
			wantInOrder:  []string{folderFailuresHeader},
			wantFailures: []string{clashDots, clashPlain, emptyLevel},
			wantNone:     append([]string{"Checking for subfolders", "Found subfolder", "subfolders, total folders to sync", "skipped"}, allFound...),
			wantSummary:  "3 messages uploaded, 0 errors occurred, 3 folders failed",
		},
		{
			name:         "verbose and quiet",
			src:          withKeptChildren,
			noSelect:     []string{"Projects/Box"},
			mappings:     []config.DirectoryMapping{mapping("Projects", "Old.Projects")},
			verbose:      true,
			quiet:        true,
			wantDst:      keptDst,
			wantInOrder:  []string{folderFailuresHeader},
			wantFailures: []string{clashDots, clashPlain, emptyLevel},
			wantNone:     append([]string{"Checking for subfolders", "Found subfolder", "subfolders, total folders to sync", "skipped"}, allFound...),
			wantSummary:  "3 messages uploaded, 0 errors occurred, 3 folders failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			src.markNoSelect(tt.noSelect...)
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose, opts.quiet = true, tt.verbose, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			got := out.String()
			assertInOrder(t, got, tt.wantInOrder...)
			_, failures, ok := strings.Cut(got, folderFailuresHeader)
			if !ok {
				t.Fatalf("output does not contain %q\noutput: %s", folderFailuresHeader, got)
			}
			assertInOrder(t, failures, tt.wantFailures...)
			assertContainsAll(t, got, tt.wantSummary)
			assertContainsNone(t, got, tt.wantNone...)
			if n := strings.Count(got, "Found subfolder"); n != tt.wantFound {
				t.Errorf("output holds %d found-subfolder lines, want %d\noutput: %s", n, tt.wantFound, got)
			}
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			for _, folder := range []string{"Old.Projects.v1_2", "Old.Projects..bad"} {
				if dst.hasFolder(folder) {
					t.Errorf("destination holds folder %q, want none", folder)
				}
			}
			examined := src.capturedNames("EXAMINE")
			for _, folder := range []string{"Projects/v1.2", "Projects/v1_2", "Projects//bad"} {
				if slices.Contains(examined, folder) {
					t.Errorf("source EXAMINE names = %q, want no %q: a failed folder must not be scanned", examined, folder)
				}
			}
		})
	}
}

func TestRunSync_When_RootHasNoEntryOrIsRepeated_Then_VerboseCountsThePlannedSubfolders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src       map[string][]fakeMsg
		name      string
		wantLine  string
		noSelect  []string
		mappings  []config.DirectoryMapping
		wantDst   map[string][]string
		wantFound []string
	}{
		{
			name:      "root is \\Noselect",
			src:       map[string][]fakeMsg{"INBOX": nil, "Box/a": fakeMsgs("a1@x"), "Box/b": fakeMsgs("b1@x")},
			noSelect:  []string{"Box"},
			mappings:  []config.DirectoryMapping{mapping("Box", "Moved")},
			wantLine:  "📂 Found 2 subfolders, total folders to sync: 2\n",
			wantFound: []string{"Found subfolder: Box/a → Moved/a\n", "Found subfolder: Box/b → Moved/b\n"},
			wantDst:   map[string][]string{"Moved/a": {"a1@x"}, "Moved/b": {"b1@x"}},
		},
		{
			name:      "map item is repeated",
			src:       map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("w1@x"), "Work/a": fakeMsgs("a1@x")},
			mappings:  []config.DirectoryMapping{mapping("Work", "Moved"), mapping("Work", "Moved")},
			wantLine:  "📂 Found 1 subfolders, total folders to sync: 2\n",
			wantFound: []string{"Found subfolder: Work/a → Moved/a\n"},
			wantDst:   map[string][]string{"Moved": {"w1@x"}, "Moved/a": {"a1@x"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			src.markNoSelect(tt.noSelect...)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			got := out.String()
			assertContainsAll(t, got, tt.wantFound...)
			if n := strings.Count(got, "subfolders, total folders to sync"); n != 1 {
				t.Errorf("output holds %d count lines, want 1\noutput: %s", n, got)
			}
			assertContainsAll(t, got, tt.wantLine)
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
		})
	}
}

func TestRunSync_When_DerivedNameWithAnEmptyLevelIsListedOnTheDestination_Then_ItIsNotRefused(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "x/": fakeMsgs("f1@x"), "a//b": fakeMsgs("f2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "x/": nil, "a//b": nil})
	out := &syncBuffer{}

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), e2eOptions(out, "y\n")); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsNone(t, out.String(), "empty level", "Folders that cannot be synced")
	assertIDs(t, "dst x/", dst.folderMsgIDs("x/"), "f1@x")
	assertIDs(t, "dst a//b", dst.folderMsgIDs("a//b"), "f2@x")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i1@x")
	if names := dst.capturedNames("CREATE"); len(names) != 0 {
		t.Errorf("dst CREATE names = %q, want none for listed folders", names)
	}
}

func TestRunSync_When_DerivedNameWithAnEmptyLevelIsListedAsNoSelect_Then_ItFailsAsNoSelectNotAsAnEmptyLevel(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "x/": fakeMsgs("f1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.markNoSelect("x/")
	out := &syncBuffer{}

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), e2eOptions(out, ""))

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got, `• x/ → x/: 1 messages not copied: destination folder cannot hold messages (\Noselect)`)
	assertContainsNone(t, got, "empty level")
}

func TestRunSync_When_DestinationIsFlat_Then_NamesWithDelimiterTextAreNotRefusedAsEmptyLevels(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "x/": fakeMsgs("f1@x"), "a//b": fakeMsgs("f2@x")})
	dst := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), e2eOptions(out, "y\n")); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsNone(t, out.String(), "empty level", "Folders that cannot be synced")
	assertIDs(t, "dst x/", dst.folderMsgIDs("x/"), "f1@x")
	assertIDs(t, "dst a//b", dst.folderMsgIDs("a//b"), "f2@x")
	assertIDs(t, "dst CREATE names", slices.Sorted(slices.Values(dst.capturedNames("CREATE"))), "a//b", "x/")
}

func TestRunSync_When_RenamedMirrorNameHasAnEmptyLevel_Then_NoRenameNoticeShows(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "p/q..r": fakeMsgs("f1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got, `• p/q..r → p_q//r: destination folder has an empty level: "p_q//r", destination delimiter "/"`+"\n")
	assertContainsNone(t, got, "is renamed to")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i1@x")
	assertNoCommandNamesFolder(t, "src", src, "p/q..r")
	assertNoCommandNamesFolder(t, "dst", dst, "p_q//r")
}

func TestRunSync_When_FailedDestinationsDifferOnlyInLetterCase_Then_NoCaseWarningShows(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), "x/": fakeMsgs("f1@x"), "X/": fakeMsgs("f2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got, "• x/ → x/: destination folder has an empty level", "• X/ → X/: destination folder has an empty level")
	assertContainsNone(t, got, "differ only in letter case")
}

func TestRunSync_When_MirroredNameWithAnEmptyLevelCarriesControlCharacters_Then_FailureLineShowsEscapesAndNoCommandNamesIt(t *testing.T) {
	t.Parallel()

	const wireName = hostileWireName + "/"
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("i1@x"), wireName: fakeMsgs("f1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	// Quiet mode silences the progress writer, so the whole output can be checked for raw control characters.
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got, fmt.Sprintf("• %s/ → %s/: destination folder has an empty level: \"%s/\", destination delimiter \"/\"\n", hostileShownName, hostileShownName, hostileShownName))
	assertContainsNone(t, got, "\x1b", hostileName, "\u202e")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i1@x")
	assertNoCommandNamesFolder(t, "src", src, wireName)
	assertNoCommandNamesFolder(t, "dst", dst, wireName)
}

func TestRunSync_When_ContextEndsBeforeTheFoldersToCreateAreListed_Then_ReturnsCanceledWithoutPreview(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	out := &cancelOnWrite{syncBuffer: &syncBuffer{}, cancel: cancel, marker: "Folders that cannot be synced"}
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Old/2020/Work"), mapping("Missing", "Missing"))

	err := runSync(ctx, cfg, e2eOptions(out, "y\n"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertContainsAll(t, out.String(), "Folders that cannot be synced")
	assertContainsNone(t, out.String(), "Messages to be copied to destination", "Folders to be created on destination", "Proceed with synchronization?")
	if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d CREATE or APPEND commands after the cancel, want 0", n)
	}
}

func TestRunSync_When_ParentLevelNameCarriesControlCharacters_Then_PreviewShowsEscapesAndServerGetsExactNames(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", hostileName+"/Sub")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got, "Folders to be created on destination:\n• "+hostileShownName+"\n• "+hostileShownName+"/Sub\n\n")
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\rx")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), hostileWireName, hostileWireName+"/Sub")
	assertIDs(t, "dst subfolder", dst.folderMsgIDs(hostileWireName+"/Sub"), "m1@x")
}

func TestRunSync_When_HiddenParentAnswersAlreadyExists_Then_CounterLeavesItOut(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Old": nil})
	dst.hideFromList("Old")
	out := &syncBuffer{}
	opts := e2eOptions(out, "y\n")

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "Old/2020")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertInOrder(t, got, "Folders to be created on destination:\n• Old\n• Old/2020\n\n", "Proceed with synchronization?", "Creating destination folders")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Old", "Old/2020")
	assertOneRow(t, screenRows(t, got), "Created 1 folders", "✓ done")
	assertIDs(t, "dst Old/2020", dst.folderMsgIDs("Old/2020"), "m1@x")
}

func TestRunSync_When_HiddenFolderAnswersAlreadyExistsAfterItsParentIsMade_Then_OnlyTheMissingMessageIsCopied(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Old/2020": fakeMsgs("m1@x")})
	dst.hideFromList("Old/2020")
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "Old/2020"))
	runOnce := func() string {
		out := &syncBuffer{}
		if err := runSync(e2eContext(t), cfg, e2eOptions(out, "y\n")); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		return out.String()
	}

	first := runOnce()

	assertInOrder(t, first,
		"Folders to be created on destination:\n• Old\n• Old/2020\n\n",
		"Creating destination folders",
		"Destination folder Old/2020 already exists and holds 1 of the 2 planned messages from INBOX, so they are not copied",
		"Syncing messages",
	)
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Old", "Old/2020")
	assertIDs(t, "dst APPENDs to Old/2020", dst.appended("Old/2020"), "m2@x")
	assertOneRow(t, screenRows(t, first), "Created 1 folders", "✓ done")

	second := runOnce()

	assertIDs(t, "dst APPENDs to Old/2020 over both runs", dst.appended("Old/2020"), "m2@x")
	assertContainsAll(t, second, "holds 2 of the 2 planned messages")
	assertContainsNone(t, second, "Syncing messages")
}

func TestRunSync_When_SourceFolderHasNoMessageToCopy_Then_NoDestinationFolderIsCreated(t *testing.T) {
	t.Parallel()

	mirror := func(t *testing.T, src, dst *fakeServer, autoConfirm bool) string {
		t.Helper()
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = autoConfirm
		if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		return out.String()
	}

	t.Run("empty folder next to a folder with a message", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Empty": nil, "A": fakeMsgs("a1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})

		first := mirror(t, src, dst, true)

		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A")
		if dst.hasFolder("Empty") {
			t.Error("dst has the folder Empty, want none")
		}
		assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x")
		assertContainsAll(t, first, foldersToCreateHeader+folderBullets("A")+"\n📨 Total new messages to sync: 1")
		assertContainsNone(t, first, "Empty →", "• Empty")
		assertReadOnly(t, "source", src)

		src.addMessage("Empty", "e1@x")

		second := mirror(t, src, dst, true)

		assertIDs(t, "dst CREATE names over both runs", dst.capturedNames("CREATE"), "A", "Empty")
		assertIDs(t, "dst Empty", dst.folderMsgIDs("Empty"), "e1@x")
		assertContainsAll(t, second, foldersToCreateHeader+folderBullets("Empty")+"\n📨 Total new messages to sync: 1")
	})

	t.Run("folder whose messages have no Message-Id", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{
			"INBOX": nil,
			"NoIds": {{body: bodyOfSize("", 300)}},
			"A":     fakeMsgs("a1@x"),
		})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})

		got := mirror(t, src, dst, true)

		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A")
		if dst.hasFolder("NoIds") {
			t.Error("dst has the folder NoIds, want none")
		}
		assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x")
		assertContainsAll(t, got, foldersToCreateHeader+folderBullets("A")+"\n📨 Total new messages to sync: 1")
		assertContainsNone(t, got, "NoIds →", "• NoIds")
	})

	t.Run("no folder has a message to copy", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Empty": nil})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})

		got := mirror(t, src, dst, false)

		assertContainsAll(t, got, "All folders already synced!")
		assertContainsNone(t, got, "Folders to be created on destination", "Proceed with synchronization?")
		if n := dst.callCount("CREATE"); n != 0 {
			t.Errorf("dst saw %d CREATE commands, want 0", n)
		}
		if n := dst.callCount("APPEND"); n != 0 {
			t.Errorf("dst saw %d APPEND commands, want 0", n)
		}
	})

	t.Run("empty parent of a folder with a message", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Projects": nil, "Projects/2024": fakeMsgs("p1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})

		got := mirror(t, src, dst, true)

		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Projects", "Projects/2024")
		assertContainsAll(t, got, foldersToCreateHeader+folderBullets("Projects", "Projects/2024")+"\n📨 Total new messages to sync: 1")
		assertIDs(t, "dst Projects", dst.folderMsgIDs("Projects"))
		assertIDs(t, "dst Projects/2024", dst.folderMsgIDs("Projects/2024"), "p1@x")
	})
}

func Test_foldersToCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup      func(dst *fakeServer)
		dstFolders map[string][]fakeMsg
		name       string
		delimiter  string
		plans      []FolderSyncPlan
		want       []string
	}{
		{
			name:       "plans that exist or copy nothing are skipped",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			plans: []FolderSyncPlan{
				{DestinationFolder: "A/B", NewMessages: 1, DestinationFolderExists: true},
				{DestinationFolder: "C/D"},
				{DestinationFolder: "E", NewMessages: 2},
			},
			want: []string{"E"},
		},
		{
			name:       "no plan gives no folder",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
		},
		{
			name:       "a shared level shows one time",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			plans: []FolderSyncPlan{
				{DestinationFolder: "Old/X", NewMessages: 1},
				{DestinationFolder: "Old/Y", NewMessages: 1},
				{DestinationFolder: "Old/X", NewMessages: 1},
			},
			want: []string{"Old", "Old/X", "Old/Y"},
		},
		{
			name:       "a parent with its own later plan comes first",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			plans: []FolderSyncPlan{
				{DestinationFolder: "Top/Sub", NewMessages: 1},
				{DestinationFolder: "Top", NewMessages: 1},
			},
			want: []string{"Top", "Top/Sub"},
		},
		{
			name:       "plans in another order than the byte order of their destinations",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			plans: []FolderSyncPlan{
				{DestinationFolder: "Old/Z", NewMessages: 1},
				{DestinationFolder: "New/A", NewMessages: 1},
			},
			want: []string{"New", "New/A", "Old", "Old/Z"},
		},
		{
			name:       "a listed parent is left out",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil, "Old": nil},
			plans:      []FolderSyncPlan{{DestinationFolder: "Old/2020/Inbox", NewMessages: 1}},
			want:       []string{"Old/2020", "Old/2020/Inbox"},
		},
		{
			name:       "a flat destination gives only the plan folders",
			delimiter:  "",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			plans: []FolderSyncPlan{
				{DestinationFolder: "Old/2020/Inbox", NewMessages: 1},
				{DestinationFolder: "Old/2020/Other", NewMessages: 1},
			},
			want: []string{"Old/2020/Inbox", "Old/2020/Other"},
		},
		{
			name:       "a parent that cannot hold messages is not listed",
			delimiter:  "/",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			setup:      func(dst *fakeServer) { dst.markNoSelect("[Gmail]") },
			plans:      []FolderSyncPlan{{DestinationFolder: "[Gmail]/New", NewMessages: 1}},
			want:       []string{"[Gmail]/New"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dst := newStoreServer(t, tt.delimiter, tt.dstFolders)
			if tt.setup != nil {
				tt.setup(dst)
			}
			dstClient := newAppClient(t, dst, "dst")

			got, err := foldersToCreate(t.Context(), destinationsToCreate(tt.plans), dstClient)

			if err != nil {
				t.Fatalf("foldersToCreate: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("foldersToCreate = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_foldersToCreate_When_ContextEnded_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		plan FolderSyncPlan
	}{
		{name: "nested destination", plan: FolderSyncPlan{DestinationFolder: "Old/2020", NewMessages: 1}},
		{name: "destination without a delimiter", plan: FolderSyncPlan{DestinationFolder: "Archive", NewMessages: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dstClient := newDestinationClient(t, "/")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			got, err := foldersToCreate(ctx, destinationsToCreate([]FolderSyncPlan{tt.plan}), dstClient)

			if !errors.Is(err, context.Canceled) {
				t.Errorf("foldersToCreate error = %v, want context.Canceled", err)
			}
			if got != nil {
				t.Errorf("foldersToCreate = %q, want no names", got)
			}
		})
	}
}

func Test_destinationsToCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		plans []FolderSyncPlan
		want  []string
	}{
		{name: "no plan", want: []string{}},
		{
			name: "plans that exist or copy nothing are left out",
			plans: []FolderSyncPlan{
				{DestinationFolder: "A", NewMessages: 1, DestinationFolderExists: true},
				{DestinationFolder: "B"},
				{DestinationFolder: "C", NewMessages: 2},
			},
			want: []string{"C"},
		},
		{
			name: "byte order, not plan order",
			plans: []FolderSyncPlan{
				{DestinationFolder: "Old/Z", NewMessages: 1},
				{DestinationFolder: "New/A", NewMessages: 1},
				{DestinationFolder: "New", NewMessages: 1},
			},
			want: []string{"New", "New/A", "Old/Z"},
		},
		{
			name: "a shared destination shows one time",
			plans: []FolderSyncPlan{
				{DestinationFolder: "B", NewMessages: 1},
				{DestinationFolder: "A", NewMessages: 1},
				{DestinationFolder: "B", NewMessages: 3},
			},
			want: []string{"A", "B"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := destinationsToCreate(tt.plans)

			if !slices.Equal(got, tt.want) {
				t.Errorf("destinationsToCreate = %q, want %q", got, tt.want)
			}
		})
	}
}

const foldersToCreateHeader = "\n\U0001F5C2\uFE0F Folders to be created on destination:\n"

func numberedFolders(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("F%02d", i)
	}
	return names
}

func folderBullets(names ...string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString("• " + name + "\n")
	}
	return b.String()
}

func Test_printFoldersToCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		want  string
		names []string
	}{
		{name: "no names", names: nil, want: ""},
		{name: "one name", names: []string{"Old"}, want: foldersToCreateHeader + folderBullets("Old")},
		{
			name:  "exactly the limit",
			names: numberedFolders(previewFolderLimit),
			want:  foldersToCreateHeader + folderBullets(numberedFolders(previewFolderLimit)...),
		},
		{
			name:  "one name over the limit",
			names: numberedFolders(previewFolderLimit + 1),
			want:  foldersToCreateHeader + folderBullets(numberedFolders(previewFolderLimit)...) + "• … and 1 more\n",
		},
		{
			name:  "names with control and bidi characters",
			names: []string{hostileName, hostileName + "/Sub"},
			want:  foldersToCreateHeader + folderBullets(hostileShownName, hostileShownName+"/Sub"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder

			printFoldersToCreate(&out, tt.names)

			if got := out.String(); got != tt.want {
				t.Errorf("printFoldersToCreate output = %q, want %q", got, tt.want)
			}
			assertContainsNone(t, out.String(), hostileName, "\u202e", "\x1b[2J", "\rx")
		})
	}
}

func Test_printFoldersToCreate_When_OneNameHasThousandsOfLevels_Then_OutputStaysBounded(t *testing.T) {
	t.Parallel()

	const levels = 5000
	deep := strings.Repeat("a/", levels-1) + "a"
	names := make([]string, 0, levels)
	for i := range levels - 1 {
		names = append(names, deep[:2*i+1])
	}
	names = append(names, deep)
	var out strings.Builder

	printFoldersToCreate(&out, names)

	got := out.String()
	if n := strings.Count(got, "\n• "); n != previewFolderLimit+1 {
		t.Errorf("output has %d bullet lines, want %d", n, previewFolderLimit+1)
	}
	if want := fmt.Sprintf("• … and %d more\n", levels-previewFolderLimit); !strings.HasSuffix(got, want) {
		t.Errorf("output does not end with %q\noutput: %q", want, got)
	}
	if limit := previewFolderLimit*len(deep) + 128; len(got) > limit {
		t.Errorf("output is %d bytes, want at most %d", len(got), limit)
	}
	assertContainsNone(t, got, deep)
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
			assertContainsAll(t, out.String(), "Provider with known IMAP limits detected", "imap.gmail.com:993", "Gmail",
				"a run moves at most 2500 MB a day from this account", "--bps-down 27000")
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

func TestRunSync_When_DestinationScanSendsNoUID_Then_CopiesOnlyTheMissingMessage(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst.omitScanUIDIn("INBOX")
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))

	first := runSyncOnce(t, cfg)

	assertIDs(t, "dst INBOX after the first run", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
	assertIDs(t, "dst APPENDs after the first run", dst.appended("INBOX"), "m2@x")
	assertContainsAll(t, first, "Sync completed successfully")
	assertContainsNone(t, first, "server sent no UID")

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	assertContainsNone(t, second, "server sent no UID", "Sync completed successfully")
	assertIDs(t, "dst APPENDs over both runs", dst.appended("INBOX"), "m2@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_CanceledDuringFolderCreation_Then_ReportsCancelNotCreateFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		destination string
		wantCounter string
		wantCreates []string
	}{
		{name: "a destination with no missing parent", destination: "Work", wantCounter: "(1/1) Creating Work", wantCreates: []string{"Work"}},
		// The counter counts the parents too: Old, Old/2020 and Old/2020/Work.
		{name: "a destination with missing parents", destination: "Old/2020/Work", wantCounter: "(1/3) Creating Old/2020/Work", wantCreates: []string{"Old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(ctx, e2eConfig(src, dst, 1, mapping("Work", tt.destination)), opts)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if strings.Contains(err.Error(), "failed to create") {
				t.Errorf("error %q reports a creation failure for a Ctrl+C", err)
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
			assertContainsAll(t, out.String(), tt.wantCounter)
			assertContainsNone(t, out.String(), folderFailuresHeader, "cannot create destination folder", "Failed to create folder", "Sync completed", "Sync stopped")
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
		})
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
	atReturn := out.String()
	// Four render ticks: a progress writer that outlived runSync would draw again.
	time.Sleep(400 * time.Millisecond)
	if later := out.String(); later != atReturn {
		t.Errorf("runSync printed after it returned\nat return: %q\nlater: %q", atReturn, later)
	}
	assertContainsNone(t, atReturn, "errors occurred", "Sync stopped", folderFailuresHeader)
	rows := screenRows(t, atReturn)
	assertOneRow(t, rows, "1/2 Canceled A → A", "✗ error")
	if started := rowsHolding(rows, "2/2 "); len(started) != 0 {
		t.Errorf("rows %v belong to the plan that never started, want none\nscreen:\n%s", started, strings.Join(rows, "\n"))
	}
	if stale := rowsHolding(rows, "→", "%"); len(stale) != 0 {
		t.Errorf("rows %v hold a tracker that is still in progress\nscreen:\n%s", stale, strings.Join(rows, "\n"))
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
		wantListed  string
		verbose     bool
	}{
		{
			name:        "refused with a hostile reply",
			createReply: "NO " + hostileReply,
			wantLog:     `Failed to create folder "` + hostileShownName + `": [dst] failed to create mailbox ` + hostileShownName + ": ",
			wantListed:  "• " + hostileShownName + " → " + hostileShownName + ": cannot create destination folder: [dst] failed to create mailbox " + hostileShownName + ": " + hostileReplyShown,
			wantErr:     func(err error) bool { return errors.Is(err, ErrSilentExit) },
		},
		{
			name:        "already exists",
			createReply: "NO [ALREADYEXISTS] Mailbox already exists",
			verbose:     true,
			wantLog:     "Folder " + hostileShownName + " already exists",
			// The fake CREATE creates nothing, so the scan of the folder that follows fails.
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
			if tt.wantListed != "" {
				assertInOrder(t, got, folderFailuresHeader, tt.wantListed)
			}
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

func TestRunSync_When_CanceledBeforeSecondPlanStarts_Then_OnlyTheStartedPlanHasATracker(t *testing.T) {
	t.Parallel()

	srcWireA, srcA, shownA := hostileFolder("A")
	dstWireB, dstB, shownB := hostileFolder("B")
	srcWireC, srcC, _ := hostileFolder("C")
	dstWireD, dstD, _ := hostileFolder("D")
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
	_, afterMarker, found := strings.Cut(got, "📥 Syncing messages...")
	if !found {
		t.Fatalf("output does not hold the copy marker\noutput: %q", got)
	}
	assertContainsAll(t, afterMarker, "1/2 Canceled "+shownA+" → "+shownB)
	assertContainsNone(t, afterMarker, "2/2")
	assertContainsNone(t, got, "Waiting", srcC, dstD, srcA, dstB)
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
		srcMap:      map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 1000}}, "b@x": {{UID: 2, Size: 1000}}},
		dstMap:      map[string][]client.MessageRef{},
		srcMessages: 2,
		dstNoSelect: true,
	}

	if maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
		t.Fatal("maybeDiff returned true on the first call, want false")
	}
	if !maybeDiff(s, 0, mappings, plans, &totalNew, &totalNewSize, nil, copyByCount) {
		t.Fatal("maybeDiff returned false on the second call, want true")
	}

	if plans[0].SourceFolder != "" {
		t.Errorf("plan populated for a \\Noselect destination: %+v", plans[0])
	}
	if totalNew.Load() != 0 || totalNewSize.Load() != 0 {
		t.Errorf("totals = %d messages, %d bytes, want 0 and 0", totalNew.Load(), totalNewSize.Load())
	}
	if s.srcMap != nil || s.dstMap != nil {
		t.Errorf("maps not freed: srcMap=%v dstMap=%v", s.srcMap, s.dstMap)
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
			mappings := rootMappings(mapping("a", "A"), mapping("Missing", "Missing"))
			var out strings.Builder

			dstC := newDestinationClient(t, "/")
			got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, mappings, "/", "/")

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			printExpansion(&out, report, got, tt.verbose, false)
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

			dstC := newDestinationClient(t, "/")
			got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("Archive", "Old")), "/", "/")

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			printExpansion(&out, report, got, tt.verbose, false)
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

			typed := []config.DirectoryMapping{mapping(tt.inbox, tt.inbox)}
			resolveMappings(typed, srcC, srcC)
			dstC := newDestinationClient(t, "/")
			got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, typedMappings(typed), "/", "/")

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if want := []string{"INBOX->INBOX", tt.inbox + "/Sub->INBOX/Sub"}; !slices.Equal(pairs, want) {
				t.Errorf("expanded = %v, want %v", pairs, want)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_ListedChildSpellsInboxInAnotherCase_Then_RelativePathIsCutByLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		root         config.DirectoryMapping
		dstDelimiter string
		want         []string
	}{
		{
			name: "dot destination", root: mapping("INBOX", "INBOX"), dstDelimiter: ".",
			want: []string{"INBOX->INBOX", "Inbox/Sub->INBOX.Sub", "Inbox/Sub/Deep->INBOX.Sub.Deep"},
		},
		{
			name: "other destination folder", root: mapping("INBOX", "Archive"), dstDelimiter: "/",
			want: []string{"INBOX->Archive", "Inbox/Sub->Archive/Sub", "Inbox/Sub/Deep->Archive/Sub/Deep"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Inbox/Sub": nil, "Inbox/Sub/Deep": nil})
			srcC := newAppClient(t, src, "src")

			dstC := newDestinationClient(t, tt.dstDelimiter)
			got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(tt.root), "/", tt.dstDelimiter)

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if !slices.Equal(pairs, tt.want) {
				t.Errorf("expanded = %v, want %v", pairs, tt.want)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_SourceIsUnlistedWithoutSubfolders_Then_MappingIsKept(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Sent": nil})
	srcC := newAppClient(t, src, "src")
	var out strings.Builder

	dstC := newDestinationClient(t, "/")
	got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("Sentt", "Sent")), "/", "/")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	printExpansion(&out, report, got, true, false)
	if want := rootMappings(mapping("Sentt", "Sent")); !slices.Equal(got, want) {
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

func TestRunSync_When_SourceScanSendsNoUID_Then_FolderFailsAndOtherFoldersCopy(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Work":  fakeMsgs("w1@x", "w2@x"),
	})
	src.omitScanUIDIn("Work")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	if dst.hasFolder("Work") {
		t.Error("dst holds Work, want none: the source folder cannot be scanned")
	}
	assertContainsAll(t, out.String(),
		"• Work → Work: cannot scan source folder:",
		"server sent no UID for a message",
		"1 folders failed",
	)
	for _, set := range src.capturedNames("UID FETCH") {
		if strings.Contains(set, "*") {
			t.Errorf("source received UID FETCH %q, want no set that holds *", set)
		}
	}
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

func TestRunSync_When_SourceFolderIsRefusedAfterThePlan_Then_OneExamineEndsItsStreamAndOtherFoldersCopy(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(2*clientBatchSize + 1)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(ids...), "B": fakeMsgs("b1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	var once sync.Once
	dst.onCommand("CREATE", func() {
		once.Do(func() { src.refuseOpenWith("A", "NO [NONEXISTENT] Unknown Mailbox") })
	})
	cfg := e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst B", dst.folderMsgIDs("B"), "b1@x")
	assertIDs(t, "dst A", dst.folderMsgIDs("A"))
	examinesOfA := 0
	for _, name := range src.capturedNames("EXAMINE") {
		if name == "A" {
			examinesOfA++
		}
	}
	if examinesOfA != 3 {
		t.Errorf("source saw %d EXAMINE commands for A, want 3: the plan scan, the stream and the check", examinesOfA)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1")
	got := out.String()
	if n := strings.Count(got, "Stream error for folder A"); n != 1 {
		t.Errorf("output holds the stream line %d times, want 1\noutput: %q", n, got)
	}
	assertContainsAll(t, got, "1 messages uploaded, 1001 errors occurred, 0 folders failed")
	assertContainsNone(t, got, "batches failed", "Sync stopped")
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

func TestRunSync_When_NoSelectDestinationReceivesDuplicateMessageIds_Then_FailureCountsEveryMessage(t *testing.T) {
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
	assertContainsAll(t, out.String(), "• Projects → Projects: 3 messages not copied: ")
	assertContainsNone(t, out.String(), "2 messages not copied")
	assertNoCommandFor(t, "destination", dst, "Projects")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_FolderFailsAndCreateIsRefused_Then_BothFailuresAreListedInOrder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreate()
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "Copy"), mapping("Gone", "Gone")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertInOrder(t, got,
		folderFailuresHeader,
		"• Gone → Gone: cannot scan source folder:",
		"Messages to be copied to destination",
		"Creating destination folders",
		folderFailuresHeader,
		"• INBOX → Copy: cannot create destination folder:",
	)
	assertContainsAll(t, got, "2 folders failed")
	assertContainsNone(t, got, "Syncing messages", "Sync completed successfully")
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

func renameLine(src, dst, delimiter string) string {
	return "⚠️  Folder " + src + " is renamed to " + dst + `: the destination server uses "` + delimiter + `" as its hierarchy delimiter` + "\n"
}

func TestRunSync_When_SourceSegmentHoldsDestinationDelimiter_Then_FolderIsRenamedNotNested(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src          map[string][]fakeMsg
		wantDst      map[string][]string
		name         string
		srcDelimiter string
		mappings     []config.DirectoryMapping
		wantCreates  []string
		wantRenames  []string
		wantSrcOpen  []string
		wantNoFolder []string
	}{
		{
			name:         "typed mapping with a dotted version folder",
			srcDelimiter: "/",
			src: map[string][]fakeMsg{
				"Projects":      fakeMsgs("m1@x"),
				"Projects/v1.2": fakeMsgs("m2@x"),
				"Projects/v1/2": fakeMsgs("m3@x", "m2@x"),
			},
			mappings: []config.DirectoryMapping{mapping("Projects", "Projects")},
			wantDst: map[string][]string{
				"Projects":      {"m1@x"},
				"Projects.v1_2": {"m2@x"},
				"Projects.v1.2": {"m3@x", "m2@x"},
			},
			wantCreates: []string{"Projects", "Projects.v1", "Projects.v1.2", "Projects.v1_2"},
			wantRenames: []string{renameLine("Projects/v1.2", "Projects.v1_2", ".")},
			wantSrcOpen: []string{"Projects", "Projects/v1.2", "Projects/v1/2"},
		},
		{
			name:         "typed mapping with a domain label",
			srcDelimiter: "/",
			src: map[string][]fakeMsg{
				"Clients":          fakeMsgs("c1@x"),
				"Clients/acme.com": fakeMsgs("c2@x"),
			},
			mappings:     []config.DirectoryMapping{mapping("Clients", "Clients")},
			wantDst:      map[string][]string{"Clients": {"c1@x"}, "Clients.acme_com": {"c2@x"}},
			wantCreates:  []string{"Clients", "Clients.acme_com"},
			wantRenames:  []string{renameLine("Clients/acme.com", "Clients.acme_com", ".")},
			wantSrcOpen:  []string{"Clients", "Clients/acme.com"},
			wantNoFolder: []string{"Clients.acme"},
		},
		{
			name:         "mirror with a domain label",
			srcDelimiter: "/",
			src: map[string][]fakeMsg{
				"INBOX":            nil,
				"Clients":          fakeMsgs("c1@x"),
				"Clients/acme.com": fakeMsgs("c2@x"),
			},
			wantDst:      map[string][]string{"Clients": {"c1@x"}, "Clients.acme_com": {"c2@x"}},
			wantCreates:  []string{"Clients", "Clients.acme_com"},
			wantRenames:  []string{renameLine("Clients/acme.com", "Clients.acme_com", ".")},
			wantSrcOpen:  []string{"INBOX", "Clients", "Clients/acme.com"},
			wantNoFolder: []string{"Clients.acme"},
		},
		{
			name:         "mirror with a dotted top-level name",
			srcDelimiter: "/",
			src:          map[string][]fakeMsg{"INBOX": nil, "acme.com": fakeMsgs("a1@x")},
			wantDst:      map[string][]string{"acme_com": {"a1@x"}},
			wantCreates:  []string{"acme_com"},
			wantRenames:  []string{renameLine("acme.com", "acme_com", ".")},
			wantSrcOpen:  []string{"INBOX", "acme.com"},
			wantNoFolder: []string{"acme"},
		},
		{
			name:         "mirror from a flat source",
			srcDelimiter: "",
			src:          map[string][]fakeMsg{"INBOX": nil, "Mr. Smith": fakeMsgs("a1@x")},
			wantDst:      map[string][]string{"Mr_ Smith": {"a1@x"}},
			wantCreates:  []string{"Mr_ Smith"},
			wantRenames:  []string{renameLine("Mr. Smith", "Mr_ Smith", ".")},
			wantSrcOpen:  []string{"INBOX", "Mr. Smith"},
			wantNoFolder: []string{"Mr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.srcDelimiter, tt.src)
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			wantAppends := 0
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
				assertIDs(t, "dst APPENDs to "+folder, dst.appended(folder), ids...)
				wantAppends += len(ids)
				if n := strings.Count(out.String(), "• "+folder+"\n"); n != 1 {
					t.Errorf("preview lists folder %q %d times, want 1", folder, n)
				}
			}
			if n := dst.callCount("APPEND"); n != wantAppends {
				t.Errorf("dst APPEND count = %d, want %d", n, wantAppends)
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), tt.wantCreates...)
			for _, folder := range tt.wantNoFolder {
				if dst.hasFolder(folder) {
					t.Errorf("destination holds folder %q that the source does not have", folder)
				}
			}
			for _, name := range src.capturedNames("EXAMINE") {
				if !slices.Contains(tt.wantSrcOpen, name) {
					t.Errorf("source EXAMINE of %q, want only %v", name, tt.wantSrcOpen)
				}
			}
			assertContainsAll(t, out.String(), append([]string{"Sync completed successfully"}, tt.wantRenames...)...)
			if n := strings.Count(out.String(), "is renamed to"); n != len(tt.wantRenames) {
				t.Errorf("output holds %d rename lines, want %d\noutput: %q", n, len(tt.wantRenames), out.String())
			}
			assertContainsNone(t, out.String(), "Folder Projects/v1/2 is renamed")

			appends := dst.callCount("APPEND")
			second := &syncBuffer{}
			again := e2eOptions(second, "")
			again.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, again); err != nil {
				t.Fatalf("second runSync: %v", err)
			}
			assertContainsAll(t, second.String(), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
		})
	}
}

func TestRunSync_When_FolderIsRenamed_Then_WarningShowsWithoutVerboseAndBeforePrompt(t *testing.T) {
	t.Parallel()

	rename := renameLine("acme.com", "acme_com", ".")
	tests := []struct {
		name         string
		input        string
		wantOrdered  []string
		wantNoOutput []string
		quiet        bool
	}{
		{
			name:        "interactive run",
			input:       "y\n",
			wantOrdered: []string{rename, "Messages to be copied to destination", "Proceed with synchronization?", "Sync completed successfully"},
		},
		{
			name:         "quiet run",
			quiet:        true,
			wantOrdered:  []string{rename, "Sync completed successfully"},
			wantNoOutput: []string{"Messages to be copied to destination", "Proceed with synchronization?"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "acme.com": fakeMsgs("a1@x")})
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.quiet, opts.autoConfirm = tt.quiet, tt.quiet

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertInOrder(t, out.String(), tt.wantOrdered...)
			assertContainsNone(t, out.String(), tt.wantNoOutput...)
			assertIDs(t, "dst acme_com", dst.folderMsgIDs("acme_com"), "a1@x")
		})
	}
}

func TestRunSync_When_RenamedNameCarriesControlCharacters_Then_WarningShowsEscapesAndServerGetsExactName(t *testing.T) {
	t.Parallel()

	const (
		srcWire  = "Clients/" + hostileWireName + ".com"
		dstWire  = "Clients." + hostileWireName + "_com"
		srcShown = "Clients/" + hostileShownName + ".com"
		dstShown = "Clients." + hostileShownName + "_com"
	)
	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "typed mapping", mappings: []config.DirectoryMapping{mapping("Clients", "Clients")}},
		{name: "mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Clients": fakeMsgs("c1@x"), srcWire: fakeMsgs("c2@x")})
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			// Quiet mode silences the progress writer, so the whole output can be checked for raw control characters.
			opts := e2eOptions(out, "")
			opts.quiet, opts.autoConfirm = true, true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			assertContainsAll(t, out.String(), renameLine(srcShown, dstShown, "."))
			assertContainsNone(t, out.String(), "\x1b", "\r", "\u202e")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Clients", dstWire)
			assertIDs(t, "dst APPENDs to the renamed folder", dst.appended(dstWire), "c2@x")
			examined := src.capturedNames("EXAMINE")
			if !slices.Contains(examined, srcWire) {
				t.Errorf("source EXAMINE names = %q, want the exact name %q", examined, srcWire)
			}
			for _, name := range examined {
				if name != "Clients" && name != srcWire {
					t.Errorf("source EXAMINE of %q, want only Clients or %q", name, srcWire)
				}
			}
		})
	}
}

func caseVariantLine(names ...string) string {
	return "⚠️  Destination folders " + strings.Join(names, ", ") +
		" differ only in letter case. If the destination server ignores letter case, a message that their source folders share is copied more than once.\n"
}

func Test_printCaseVariantDestinations(t *testing.T) {
	t.Parallel()

	const (
		upperHostileName  = "EVIL\x1b[2J\rX\u202eGPJ"
		upperHostileShown = `EVIL\x1b[2J\rX\u202eGPJ`
	)
	tests := []struct {
		name     string
		want     string
		listed   []string
		mappings []config.DirectoryMapping
	}{
		{
			name:     "two spellings of one name",
			mappings: []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			listed:   []string{"INBOX"},
			want:     caseVariantLine("Archive", "archive") + "\n",
		},
		{
			name:     "two mappings with the same destination",
			mappings: []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "Archive")},
			listed:   []string{"INBOX"},
		},
		{
			name:     "both spellings are listed",
			mappings: []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			listed:   []string{"Archive", "archive"},
		},
		{
			name:     "only one spelling is listed",
			mappings: []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			listed:   []string{"Archive"},
			want:     caseVariantLine("Archive", "archive") + "\n",
		},
		{
			name: "three spellings and a repeat in unsorted order",
			mappings: []config.DirectoryMapping{
				mapping("A", "archive"), mapping("B", "ARCHIVE"), mapping("C", "Archive"), mapping("D", "archive"),
			},
			want: caseVariantLine("ARCHIVE", "Archive", "archive") + "\n",
		},
		{
			name:     "a later spelling repeats exactly",
			mappings: []config.DirectoryMapping{mapping("A", "archive"), mapping("B", "ARCHIVE"), mapping("C", "ARCHIVE")},
			want:     caseVariantLine("ARCHIVE", "archive") + "\n",
		},
		{
			name: "every distinct spelling is listed despite a repeat",
			mappings: []config.DirectoryMapping{
				mapping("A", "archive"), mapping("B", "ARCHIVE"), mapping("C", "ARCHIVE"), mapping("D", "Archive"),
			},
			listed: []string{"ARCHIVE", "Archive", "archive"},
		},
		{
			name: "two groups in reverse key order",
			mappings: []config.DirectoryMapping{
				mapping("B", "beta"), mapping("C", "Beta"), mapping("D", "alpha"), mapping("E", "Alpha"),
			},
			want: caseVariantLine("Alpha", "alpha") + caseVariantLine("Beta", "beta") + "\n",
		},
		{
			name: "one group is listed and the other is not",
			mappings: []config.DirectoryMapping{
				mapping("A", "Archive"), mapping("B", "archive"), mapping("C", "Beta"), mapping("D", "beta"),
			},
			listed: []string{"Archive", "archive"},
			want:   caseVariantLine("Beta", "beta") + "\n",
		},
		{
			name:     "a listed folder outside the plan does not count",
			mappings: []config.DirectoryMapping{mapping("Sent", "sent")},
			listed:   []string{"Sent"},
		},
		{
			name:     "names with control and bidi characters",
			mappings: []config.DirectoryMapping{mapping("A", hostileName), mapping("B", upperHostileName)},
			want:     caseVariantLine(upperHostileShown, hostileShownName) + "\n",
		},
		{
			name: "no mappings",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder
			printCaseVariantDestinations(&out, tt.mappings, tt.listed)

			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
			assertContainsNone(t, out.String(), "\x1b", "\r", "\u202e")
		})
	}
}

func firstOutputDifference(got, want string) string {
	i := 0
	for i < min(len(got), len(want)) && got[i] == want[i] {
		i++
	}
	window := func(s string) string { return s[max(i-20, 0):min(i+40, len(s))] }
	return fmt.Sprintf("length %d, want %d; first difference at byte %d: got %q, want %q",
		len(got), len(want), i, window(got), window(want))
}

func Test_printCaseVariantDestinations_When_GroupHoldsEveryLetterCaseVariant_Then_ListsEachNameOnce(t *testing.T) {
	t.Parallel()

	const base = "abcdefghijklmn"
	variants := make([]string, 0, 1<<len(base))
	for mask := range 1 << len(base) {
		name := []byte(base)
		for i := range name {
			if mask&(1<<i) != 0 {
				name[i] -= 'a' - 'A'
			}
		}
		variants = append(variants, string(name))
	}
	sorted := slices.Clone(variants)
	slices.Sort(sorted)

	mappings := make([]config.DirectoryMapping, 0, len(sorted)+3)
	for _, name := range slices.Backward(sorted) {
		mappings = append(mappings, mapping("src/"+strconv.Itoa(len(mappings)), name))
	}
	for _, name := range []string{mappings[0].Destination, sorted[len(sorted)/2], sorted[0]} {
		mappings = append(mappings, mapping("src/"+strconv.Itoa(len(mappings)), name))
	}

	unrelated := []string{"INBOX", "Sent", "zzz"}
	listedMissingOne := append(slices.DeleteFunc(slices.Clone(variants), func(v string) bool { return v == base }), unrelated...)
	listedAll := append(slices.Clone(variants), unrelated...)
	slices.Sort(listedMissingOne)
	slices.Sort(listedAll)
	tests := []struct {
		name   string
		want   string
		listed []string
	}{
		{
			name:   "one variant is not listed",
			listed: listedMissingOne,
			want:   caseVariantLine(sorted...) + "\n",
		},
		{
			name:   "every variant is listed",
			listed: listedAll,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wantMappings, wantListed := slices.Clone(mappings), slices.Clone(tt.listed)
			var out strings.Builder
			printCaseVariantDestinations(&out, mappings, tt.listed)

			if got := out.String(); got != tt.want {
				t.Errorf("output differs: %s", firstOutputDifference(got, tt.want))
			}
			if !slices.Equal(mappings, wantMappings) {
				t.Error("the call changed the mappings of the caller")
			}
			if !slices.Equal(tt.listed, wantListed) {
				t.Error("the call changed the listed names of the caller")
			}
		})
	}
}

func TestRunSync_When_DestinationsDifferOnlyInLetterCase_Then_WarningShowsAndEachFolderCopies(t *testing.T) {
	t.Parallel()

	line := caseVariantLine("Archive", "archive")
	tests := []struct {
		srcFolders  map[string][]fakeMsg
		dstFolders  map[string][]fakeMsg
		wantFolders map[string][]string
		name        string
		input       string
		mappings    []config.DirectoryMapping
		wantOrdered []string
		wantCreates []string
		wantLines   int
		quiet       bool
	}{
		{
			name: "typed and interactive",
			srcFolders: map[string][]fakeMsg{
				"INBOX": nil,
				"A":     fakeMsgs("a1@x", "shared@x"),
				"B":     fakeMsgs("b1@x", "shared@x"),
			},
			dstFolders:  map[string][]fakeMsg{"INBOX": nil},
			mappings:    []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			input:       "y\n",
			wantOrdered: []string{line, "Messages to be copied to destination", "Proceed with synchronization?", "Sync completed successfully"},
			wantCreates: []string{"Archive", "archive"},
			wantLines:   1,
			wantFolders: map[string][]string{"Archive": {"a1@x", "shared@x"}, "archive": {"b1@x", "shared@x"}},
		},
		{
			name: "typed and quiet",
			srcFolders: map[string][]fakeMsg{
				"INBOX": nil,
				"A":     fakeMsgs("a1@x", "shared@x"),
				"B":     fakeMsgs("b1@x", "shared@x"),
			},
			dstFolders:  map[string][]fakeMsg{"INBOX": nil},
			mappings:    []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			quiet:       true,
			wantOrdered: []string{line, "Sync completed successfully"},
			wantCreates: []string{"Archive", "archive"},
			wantLines:   1,
			wantFolders: map[string][]string{"Archive": {"a1@x", "shared@x"}, "archive": {"b1@x", "shared@x"}},
		},
		{
			name: "mirror and quiet",
			srcFolders: map[string][]fakeMsg{
				"INBOX":   nil,
				"Archive": fakeMsgs("c1@x", "shared@x"),
				"archive": fakeMsgs("c2@x", "shared@x"),
			},
			dstFolders:  map[string][]fakeMsg{"INBOX": nil},
			quiet:       true,
			wantOrdered: []string{line, "Sync completed successfully"},
			wantCreates: []string{"Archive", "archive"},
			wantLines:   1,
			wantFolders: map[string][]string{"Archive": {"c1@x", "shared@x"}, "archive": {"c2@x", "shared@x"}},
		},
		{
			name: "destination lists both spellings",
			srcFolders: map[string][]fakeMsg{
				"INBOX": nil,
				"A":     fakeMsgs("a1@x", "shared@x"),
				"B":     fakeMsgs("b1@x", "shared@x"),
			},
			dstFolders:  map[string][]fakeMsg{"INBOX": nil, "Archive": nil, "archive": nil},
			mappings:    []config.DirectoryMapping{mapping("A", "Archive"), mapping("B", "archive")},
			quiet:       true,
			wantOrdered: []string{"Sync completed successfully"},
			wantFolders: map[string][]string{"Archive": {"a1@x", "shared@x"}, "archive": {"b1@x", "shared@x"}},
		},
		{
			name: "destination names that resolve to one folder",
			srcFolders: map[string][]fakeMsg{
				"INBOX": nil,
				"A":     fakeMsgs("a1@x"),
				"B":     fakeMsgs("b1@x"),
			},
			dstFolders:  map[string][]fakeMsg{"INBOX": nil},
			mappings:    []config.DirectoryMapping{mapping("A", "INBOX/Sub"), mapping("B", "inbox/Sub")},
			quiet:       true,
			wantOrdered: []string{"Sync completed successfully"},
			wantCreates: []string{"INBOX/Sub"},
			wantFolders: map[string][]string{"INBOX/Sub": {"a1@x", "b1@x"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.srcFolders)
			dst := newStoreServer(t, "/", tt.dstFolders)
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.quiet, opts.autoConfirm = tt.quiet, tt.quiet

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v", err)
			}

			if n := strings.Count(out.String(), "differ only in letter case"); n != tt.wantLines {
				t.Errorf("warning lines = %d, want %d\noutput: %q", n, tt.wantLines, out.String())
			}
			assertInOrder(t, out.String(), tt.wantOrdered...)
			assertIDs(t, "dst CREATE names", slices.Sorted(slices.Values(dst.capturedNames("CREATE"))), tt.wantCreates...)
			for folder, ids := range tt.wantFolders {
				assertIDs(t, "dst "+folder, slices.Sorted(slices.Values(dst.folderMsgIDs(folder))), ids...)
			}
		})
	}
}

func TestRunSync_When_RenameMakesTwoSourcesShareADestination_Then_BothFailAndNothingMerges(t *testing.T) {
	t.Parallel()

	const (
		dottedClash = "• Projects/v1.2 → Projects.v1_2: same destination folder as source folder Projects/v1_2\n"
		plainClash  = "• Projects/v1_2 → Projects.v1_2: same destination folder as source folder Projects/v1.2\n"
		mirrorDots  = "• a.b → a_b: same destination folder as source folder a_b\n"
		mirrorPlain = "• a_b → a_b: same destination folder as source folder a.b\n"
		missingLine = "• Missing → Missing: cannot scan source folder"
	)
	projects := map[string][]fakeMsg{
		"Projects":      fakeMsgs("p1@x"),
		"Projects/v1.2": fakeMsgs("a1@x"),
		"Projects/v1_2": fakeMsgs("b1@x"),
	}
	tests := []struct {
		src           map[string][]fakeMsg
		wantDst       map[string][]string
		name          string
		wantNoFolder  string
		wantFailed    string
		wantClashes   []string
		wantNeverOpen []string
		mappings      []config.DirectoryMapping
		quiet         bool
		autoConfirm   bool
	}{
		{
			name:          "typed mapping",
			src:           projects,
			mappings:      []config.DirectoryMapping{mapping("Projects", "Projects")},
			autoConfirm:   true,
			wantDst:       map[string][]string{"Projects": {"p1@x"}},
			wantNoFolder:  "Projects.v1_2",
			wantClashes:   []string{dottedClash, plainClash},
			wantNeverOpen: []string{"Projects/v1.2", "Projects/v1_2"},
			wantFailed:    "2 folders failed",
		},
		{
			name:          "typed mapping in quiet mode",
			src:           projects,
			mappings:      []config.DirectoryMapping{mapping("Projects", "Projects")},
			quiet:         true,
			autoConfirm:   true,
			wantDst:       map[string][]string{"Projects": {"p1@x"}},
			wantNoFolder:  "Projects.v1_2",
			wantClashes:   []string{dottedClash, plainClash},
			wantNeverOpen: []string{"Projects/v1.2", "Projects/v1_2"},
			wantFailed:    "2 folders failed",
		},
		{
			name:          "mirror",
			src:           map[string][]fakeMsg{"INBOX": nil, "a.b": fakeMsgs("x1@x"), "a_b": fakeMsgs("y1@x")},
			wantNoFolder:  "a_b",
			wantClashes:   []string{mirrorDots, mirrorPlain},
			wantNeverOpen: []string{"a.b", "a_b"},
			wantFailed:    "2 folders failed",
		},
		{
			name:          "both clash lines come before a later failure",
			src:           projects,
			mappings:      []config.DirectoryMapping{mapping("Projects", "Projects"), mapping("Missing", "Missing")},
			autoConfirm:   true,
			wantDst:       map[string][]string{"Projects": {"p1@x"}},
			wantNoFolder:  "Projects.v1_2",
			wantClashes:   []string{dottedClash, plainClash, missingLine},
			wantNeverOpen: []string{"Projects/v1.2", "Projects/v1_2"},
			wantFailed:    "3 folders failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = tt.autoConfirm, tt.quiet

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, out.String(), folderFailuresHeader, tt.wantFailed)
			assertInOrder(t, out.String(), tt.wantClashes...)
			assertContainsNone(t, out.String(), "is renamed to")
			if !tt.autoConfirm {
				assertContainsNone(t, out.String(), "Proceed with synchronization?")
			}
			wantAppends := 0
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
				assertIDs(t, "dst APPENDs to "+folder, dst.appended(folder), ids...)
				wantAppends += len(ids)
			}
			if n := dst.callCount("APPEND"); n != wantAppends {
				t.Errorf("dst APPEND count = %d, want %d", n, wantAppends)
			}
			if dst.hasFolder(tt.wantNoFolder) {
				t.Errorf("destination holds folder %q, want none: the clashing sources must not merge", tt.wantNoFolder)
			}
			examined := src.capturedNames("EXAMINE")
			for _, name := range tt.wantNeverOpen {
				if slices.Contains(examined, name) {
					t.Errorf("source EXAMINE names = %q, want no %q: a clashing folder must not be scanned", examined, name)
				}
			}

			appends := dst.callCount("APPEND")
			again := e2eOptions(&syncBuffer{}, "")
			again.autoConfirm, again.quiet = tt.autoConfirm, tt.quiet
			if err := runSync(e2eContext(t), cfg, again); !errors.Is(err, ErrSilentExit) {
				t.Fatalf("second run error = %v, want ErrSilentExit", err)
			}
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
		})
	}
}

func TestRunSync_When_RenameClashAppearsAfterAnEarlierRun_Then_ExistingFolderGetsNoForeignMail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		first    map[string][]fakeMsg
		second   map[string][]fakeMsg
		name     string
		folder   string
		wantIDs  []string
		mappings []config.DirectoryMapping
	}{
		{
			name:    "mirror: a renamed folder appears that sorts before the filled one",
			first:   map[string][]fakeMsg{"INBOX": nil, "a_b": fakeMsgs("y1@x")},
			second:  map[string][]fakeMsg{"INBOX": nil, "a.b": fakeMsgs("x1@x"), "a_b": fakeMsgs("y1@x")},
			folder:  "a_b",
			wantIDs: []string{"y1@x"},
		},
		{
			name:    "mirror: a plain folder appears that has the name of a renamed one",
			first:   map[string][]fakeMsg{"INBOX": nil, "a.b": fakeMsgs("x1@x")},
			second:  map[string][]fakeMsg{"INBOX": nil, "a.b": fakeMsgs("x1@x"), "a_b": fakeMsgs("y1@x")},
			folder:  "a_b",
			wantIDs: []string{"x1@x"},
		},
		{
			name:     "typed mapping: a renamed subfolder appears that sorts before the filled one",
			first:    map[string][]fakeMsg{"Projects": fakeMsgs("p1@x"), "Projects/v1_2": fakeMsgs("b1@x")},
			second:   map[string][]fakeMsg{"Projects": fakeMsgs("p1@x"), "Projects/v1.2": fakeMsgs("a1@x"), "Projects/v1_2": fakeMsgs("b1@x")},
			folder:   "Projects.v1_2",
			wantIDs:  []string{"b1@x"},
			mappings: []config.DirectoryMapping{mapping("Projects", "Projects")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
			opts := e2eOptions(&syncBuffer{}, "")
			opts.autoConfirm = true
			if err := runSync(e2eContext(t), e2eConfig(newStoreServer(t, "/", tt.first), dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("first runSync: %v", err)
			}
			assertIDs(t, "dst "+tt.folder+" after the first run", dst.folderMsgIDs(tt.folder), tt.wantIDs...)
			appends := dst.callCount("APPEND")

			out := &syncBuffer{}
			again := e2eOptions(out, "")
			again.autoConfirm = true
			err := runSync(e2eContext(t), e2eConfig(newStoreServer(t, "/", tt.second), dst, 1, tt.mappings...), again)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("second runSync error = %v, want ErrSilentExit", err)
			}
			assertIDs(t, "dst "+tt.folder, dst.folderMsgIDs(tt.folder), tt.wantIDs...)
			assertIDs(t, "dst APPENDs to "+tt.folder, dst.appended(tt.folder), tt.wantIDs...)
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
			assertContainsAll(t, out.String(), folderFailuresHeader, "same destination folder as source folder")
		})
	}
}

func TestRunSync_When_TypedDestinationHoldsDestinationDelimiter_Then_NameIsNotRenamed(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Docs": fakeMsgs("d1@x"), "Docs/x": fakeMsgs("d2@x")})
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Docs", "Docs.v1.2")), opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst Docs.v1.2", dst.folderMsgIDs("Docs.v1.2"), "d1@x")
	assertIDs(t, "dst Docs.v1.2.x", dst.folderMsgIDs("Docs.v1.2.x"), "d2@x")
	if dst.hasFolder("Docs.v1_2") {
		t.Error("destination holds Docs.v1_2, but the operator typed the destination name")
	}
	assertContainsNone(t, out.String(), "is renamed to")
}

// runSyncConfirmed runs one sync with -y and returns what it printed. A non-empty srcFolder and dstFolder replace the configured mappings, as the folder flags do.
func runSyncConfirmed(t *testing.T, cfg *config.Config, srcFolder, dstFolder string) string {
	t.Helper()

	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true
	opts.srcFolder, opts.dstFolder = srcFolder, dstFolder
	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}
	return out.String()
}

func TestRunSync_When_DestinationNamesInboxInAnotherCase_Then_NothingIsCopiedTwice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		srcFolder    string
		dstFolder    string
		srcIDs       []string
		dstIDs       []string
		wantIDs      []string
		wantAppended []string
		viaFlags     bool
	}{
		{
			name: "map to Inbox", srcFolder: "INBOX", dstFolder: "Inbox",
			srcIDs: []string{"m1@x", "m2@x"}, dstIDs: []string{"m1@x", "m2@x"}, wantIDs: []string{"m1@x", "m2@x"},
		},
		{
			name: "map to inbox", srcFolder: "INBOX", dstFolder: "inbox",
			srcIDs: []string{"m1@x", "m2@x"}, dstIDs: []string{"m1@x", "m2@x"}, wantIDs: []string{"m1@x", "m2@x"},
		},
		{
			name: "folder flags with another case on each side", srcFolder: "inbox", dstFolder: "Inbox", viaFlags: true,
			srcIDs: []string{"m1@x", "m2@x"}, dstIDs: []string{"m1@x", "m2@x"}, wantIDs: []string{"m1@x", "m2@x"},
		},
		{
			name: "one message is missing", srcFolder: "INBOX", dstFolder: "Inbox",
			srcIDs: []string{"m1@x", "m2@x", "m3@x"}, dstIDs: []string{"m1@x", "m2@x"},
			wantIDs: []string{"m1@x", "m2@x", "m3@x"}, wantAppended: []string{"m3@x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs(tt.srcIDs...)})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs(tt.dstIDs...)})
			cfg := e2eConfig(src, dst, 1, mapping(tt.srcFolder, tt.dstFolder))
			srcFlag, dstFlag := "", ""
			if tt.viaFlags {
				cfg.Map = nil
				srcFlag, dstFlag = tt.srcFolder, tt.dstFolder
			}

			first := runSyncConfirmed(t, cfg, srcFlag, dstFlag)
			second := runSyncConfirmed(t, cfg, srcFlag, dstFlag)

			if len(tt.wantAppended) == 0 {
				assertContainsAll(t, first, "All folders already synced!")
			}
			assertContainsAll(t, second, "All folders already synced!")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), tt.wantIDs...)
			assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), tt.wantAppended...)
			if n := dst.callCount("APPEND"); n != len(tt.wantAppended) {
				t.Errorf("dst APPEND count = %d, want %d", n, len(tt.wantAppended))
			}
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("dst CREATE count = %d (%q), want 0: INBOX exists", n, dst.capturedNames("CREATE"))
			}
		})
	}
}

func TestRunSync_When_SourceNamesInboxInAnotherCase_Then_SubfoldersAreCopied(t *testing.T) {
	t.Parallel()

	for _, typed := range []string{"Inbox", "inbox"} {
		t.Run(typed, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "INBOX/Sub": fakeMsgs("s1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping(typed, "INBOX"))

			runSyncConfirmed(t, cfg, "", "")
			appends := dst.callCount("APPEND")
			second := runSyncConfirmed(t, cfg, "", "")

			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			assertIDs(t, "dst INBOX/Sub", dst.folderMsgIDs("INBOX/Sub"), "s1@x")
			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
		})
	}
}

func TestRunSync_When_SourceListsInboxChildInMixedCase_Then_ChildIsCopiedOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "typed mapping", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
		{name: "mirror"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Inbox/Sub": fakeMsgs("s1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "INBOX/Sub")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			assertIDs(t, "dst INBOX/Sub", dst.folderMsgIDs("INBOX/Sub"), "s1@x")
			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != 2 {
				t.Errorf("dst APPEND count = %d, want 2: one message for each folder, none on the second run", n)
			}
			examined := src.capturedNames("EXAMINE")
			if !slices.Contains(examined, "Inbox/Sub") {
				t.Errorf("source EXAMINE names = %q, want the listed Inbox/Sub among them", examined)
			}
			if slices.Contains(examined, "INBOX/Sub") {
				t.Errorf("source EXAMINE names = %q, want no INBOX/Sub: the server never listed it", examined)
			}
		})
	}
}

func TestRunSync_When_SourceListsTwoSpellingsOfAnInboxChild_Then_BothAreScannedAndCopied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "mirror"},
		{name: "typed INBOX to INBOX", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX":     fakeMsgs("m1@x"),
				"INBOX/Sub": fakeMsgs("s1@x"),
				"Inbox/Sub": fakeMsgs("s2@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			examined := src.capturedNames("EXAMINE")
			for _, want := range []string{"INBOX/Sub", "Inbox/Sub"} {
				if !slices.Contains(examined, want) {
					t.Errorf("source EXAMINE names = %q, want %q among them: the server lists both spellings", examined, want)
				}
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			got := dst.folderMsgIDs("INBOX/Sub")
			slices.Sort(got)
			assertIDs(t, "dst INBOX/Sub", got, "s1@x", "s2@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "INBOX/Sub")
			assertContainsAll(t, second, "All folders already synced!")
		})
	}
}

func TestRunSync_When_SourceListsInboxChildInAnotherCaseAndMatchesItExactly_Then_WireKeepsTheListedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "mirror"},
		{name: "typed INBOX to INBOX", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Inbox/Sub": fakeMsgs("s1@x")})
			src.foldOnlyBareInbox()
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			first := runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			assertContainsNone(t, first, folderFailuresHeader)
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			assertIDs(t, "dst INBOX/Sub", dst.folderMsgIDs("INBOX/Sub"), "s1@x")
			examined := src.capturedNames("EXAMINE")
			if !slices.Contains(examined, "Inbox/Sub") {
				t.Errorf("source EXAMINE names = %q, want the listed Inbox/Sub among them", examined)
			}
			if slices.Contains(examined, "INBOX/Sub") {
				t.Errorf("source EXAMINE names = %q, want no INBOX/Sub: the server never listed it", examined)
			}
			assertContainsAll(t, second, "All folders already synced!")
		})
	}
}

func TestRunSync_When_DestinationListsInboxChildInAnotherCase_Then_ItIsUsedUnderItsListedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "mirror"},
		{name: "typed INBOX to INBOX", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "INBOX/Sub": fakeMsgs("s1@x", "s2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Inbox/Sub": fakeMsgs("s1@x")})
			dst.foldOnlyBareInbox()
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			first := runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			assertContainsNone(t, first, folderFailuresHeader)
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("dst CREATE names = %q, want none: Inbox/Sub exists", dst.capturedNames("CREATE"))
			}
			for _, verb := range []string{"EXAMINE", "SELECT"} {
				if names := dst.capturedNames(verb); slices.Contains(names, "INBOX/Sub") {
					t.Errorf("dst %s names = %q, want no INBOX/Sub: the server lists Inbox/Sub", verb, names)
				}
			}
			assertIDs(t, "dst Inbox/Sub", dst.folderMsgIDs("Inbox/Sub"), "s1@x", "s2@x")
			assertIDs(t, "dst APPENDs to Inbox/Sub", dst.appended("Inbox/Sub"), "s2@x")
			assertContainsAll(t, second, "All folders already synced!")
		})
	}
}

func TestRunSync_When_DestinationListsInboxChildWithNilDelimiter_Then_ItIsUsedUnderItsListedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "mirror"},
		{name: "typed INBOX.Sub", mappings: []config.DirectoryMapping{mapping("INBOX.Sub", "INBOX.Sub")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil, "INBOX.Sub": fakeMsgs("s1@x", "s2@x")})
			dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil, "inbox.Sub": fakeMsgs("s1@x")})
			dst.foldOnlyBareInbox()
			dst.listWithNilDelimiter("inbox.Sub")
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			first := runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			assertContainsNone(t, first, folderFailuresHeader)
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("dst CREATE names = %q, want none: inbox.Sub exists", dst.capturedNames("CREATE"))
			}
			for _, verb := range []string{"EXAMINE", "SELECT"} {
				if names := dst.capturedNames(verb); slices.Contains(names, "INBOX.Sub") {
					t.Errorf("dst %s names = %q, want no INBOX.Sub: the server lists inbox.Sub", verb, names)
				}
			}
			if dst.hasFolder("INBOX.Sub") {
				t.Error("dst holds INBOX.Sub, want only the listed inbox.Sub")
			}
			assertIDs(t, "dst inbox.Sub", dst.folderMsgIDs("inbox.Sub"), "s1@x", "s2@x")
			assertIDs(t, "dst APPENDs to inbox.Sub", dst.appended("inbox.Sub"), "s2@x")
			assertContainsAll(t, second, "All folders already synced!")
		})
	}
}

func TestRunSync_When_SourceListsInboxChildWithNilDelimiter_Then_TypedNameScansTheListedFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil, "inbox.Sub": fakeMsgs("s1@x")})
	src.foldOnlyBareInbox()
	src.listWithNilDelimiter("inbox.Sub")
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX.Sub", "Archive"))

	out := runSyncConfirmed(t, cfg, "", "")

	examined := src.capturedNames("EXAMINE")
	if !slices.Contains(examined, "inbox.Sub") {
		t.Errorf("source EXAMINE names = %q, want the listed inbox.Sub among them", examined)
	}
	if slices.Contains(examined, "INBOX.Sub") {
		t.Errorf("source EXAMINE names = %q, want no INBOX.Sub: the server never listed it", examined)
	}
	assertContainsNone(t, out, folderFailuresHeader)
	assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "s1@x")
}

func TestRunSync_When_TwoSourcesDifferOnlyInInboxCase_Then_TheyAreOneSource(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("inbox", "A"), mapping("INBOX", "A"))

	runSyncConfirmed(t, cfg, "", "")

	assertIDs(t, "dst A", dst.folderMsgIDs("A"), "m1@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A")
}

func TestRunSync_When_DestinationNameDiffersInCaseFromAnotherFolder_Then_ItStaysASeparateFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Sent": fakeMsgs("s1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Sent": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Sent", "sent"))

	runSyncConfirmed(t, cfg, "", "")

	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "sent")
	assertIDs(t, "dst sent", dst.folderMsgIDs("sent"), "s1@x")
	assertIDs(t, "dst Sent", dst.folderMsgIDs("Sent"))
}

func Test_resolveMappings(t *testing.T) {
	t.Parallel()

	slash := newAppClient(t, newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil}), "slash")
	dot := newAppClient(t, newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil}), "dot")
	flat := newAppClient(t, newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil}), "flat")
	listedCase := newAppClient(t, newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Inbox/Work": nil}), "listed")

	tests := []struct {
		name string
		src  *client.Client
		dst  *client.Client
		in   config.DirectoryMapping
		want config.DirectoryMapping
	}{
		{name: "bare INBOX on both sides", src: slash, dst: dot, in: mapping("inbox", "Inbox"), want: mapping("INBOX", "INBOX")},
		{name: "INBOX level in front of a child", src: slash, dst: dot, in: mapping("Inbox/Work", "inbox.Work"), want: mapping("INBOX/Work", "INBOX.Work")},
		{name: "delimiter of the other server", src: slash, dst: dot, in: mapping("inbox.Work", "inbox/Work"), want: mapping("inbox.Work", "inbox/Work")},
		{name: "another folder keeps its letter case", src: slash, dst: dot, in: mapping("Sent", "sent"), want: mapping("Sent", "sent")},
		{name: "child of a flat destination", src: slash, dst: flat, in: mapping("inbox/Work", "inbox/Work"), want: mapping("INBOX/Work", "inbox/Work")},
		{name: "child that the source lists in another letter case", src: listedCase, dst: slash, in: mapping("INBOX/Work", "X"), want: mapping("Inbox/Work", "X")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{tt.in}

			resolveMappings(mappings, tt.src, tt.dst)

			if mappings[0] != tt.want {
				t.Errorf("mapping = %+v, want %+v", mappings[0], tt.want)
			}
		})
	}
}

func TestRunSync_When_CreateReportsExistingFolderWithMessages_Then_CopiesOnlyTheDifference(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
	dst.hideFromList("Work")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
	runOnce := func() string {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true
		if err := runSync(e2eContext(t), cfg, opts); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		return out.String()
	}

	first := runOnce()

	assertIDs(t, "dst Work after the first run", dst.folderMsgIDs("Work"), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Work"), "m3@x")
	assertContainsAll(t, first,
		"  ℹ️  Destination folder Work already exists and holds 2 of the 3 planned messages from Work, so they are not copied\n",
		"Syncing messages",
		"Sync completed successfully",
	)
	srcLogins, dstLogins, dstLists := src.callCount("LOGIN"), dst.callCount("LOGIN"), dst.callCount("LIST")

	second := runOnce()

	assertIDs(t, "dst Work after the second run", dst.folderMsgIDs("Work"), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs over both runs", dst.appended("Work"), "m3@x")
	assertContainsAll(t, second, "holds 3 of the 3 planned messages", "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages", "Sync completed successfully")
	if n := src.callCount("LOGIN") - srcLogins; n != 1 {
		t.Errorf("source saw %d LOGIN commands in the second run, want 1: no worker may connect", n)
	}
	if n := dst.callCount("LOGIN") - dstLogins; n != 1 {
		t.Errorf("destination saw %d LOGIN commands in the second run, want 1: no worker may connect", n)
	}
	if n := dst.callCount("LIST") - dstLists; n != 1 {
		t.Errorf("destination saw %d LIST commands in the second run, want 1", n)
	}
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work", "Work")
	assertReadOnly(t, "source", src)
}

// subjectBody builds a message whose body depends on subject, so two messages can share a Message-Id and still differ.
func subjectBody(msgID, subject string) string {
	return "From: printer@test\r\nMessage-Id: <" + msgID + ">\r\nSubject: " + subject + "\r\n\r\nBody " + subject + ".\r\n"
}

// bodyOfSize builds a message of exactly size bytes. An empty msgID leaves out the Message-Id header.
func bodyOfSize(msgID string, size int) string {
	head := "From: sizer@test\r\n"
	if msgID != "" {
		head += "Message-Id: <" + msgID + ">\r\n"
	}
	head += "Subject: sized\r\n\r\n"
	return head + strings.Repeat("x", size-len(head))
}

func totalSize(bodies ...string) int {
	var n int
	for _, b := range bodies {
		n += len(b)
	}
	return n
}

func sharedIDMsgs(msgID string, bodies ...string) []fakeMsg {
	msgs := make([]fakeMsg, len(bodies))
	for i, b := range bodies {
		msgs[i] = fakeMsg{msgID: msgID, body: b}
	}
	return msgs
}

// extraCopiesMarker is part of the preview line about planned copies that the destination already holds by Message-Id.
const extraCopiesMarker = "of these messages have a Message-Id"

// assertExtraCopiesNote checks that the "will copy" line of the folder is followed at once by the line that counts extra copies.
func assertExtraCopiesNote(t *testing.T, got, folder string, planned, extra int) {
	t.Helper()
	head := fmt.Sprintf("• %s → %s will copy %d messages (≈ ", folder, folder, planned)
	_, afterHead, found := strings.Cut(got, head)
	if !found {
		t.Errorf("output does not contain %q\noutput: %q", head, got)
		return
	}
	_, next, found := strings.Cut(afterHead, "\n")
	if !found {
		t.Errorf("the line that starts with %q does not end\noutput: %q", head, got)
		return
	}
	want := fmt.Sprintf("  ℹ️  %d of these messages have a Message-Id that the destination folder already holds\n", extra)
	if !strings.HasPrefix(next, want) {
		t.Errorf("the line after %q does not start with %q\noutput: %q", head, want, got)
	}
}

func runSyncOnce(t *testing.T, cfg *config.Config) string {
	t.Helper()
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}
	return out.String()
}

func TestRunSync_When_SourceMessagesShareMessageId_Then_EachIsCopiedOnce(t *testing.T) {
	t.Parallel()

	bodies := []string{
		subjectBody("scan@printer", "one"),
		subjectBody("scan@printer", "two"),
		subjectBody("scan@printer", "three"),
	}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs("scan@printer", bodies...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• Scans → Scans will copy 3 messages (≈ %d B)", totalSize(bodies...)),
		"Sync completed successfully",
	)
	assertContainsNone(t, first, extraCopiesMarker)
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Scans"), "scan@printer", "scan@printer", "scan@printer")
	if got := dst.folderBodies("Scans"); !slices.Equal(got, bodies) {
		t.Errorf("dst Scans bodies = %q, want the three source bodies %q", got, bodies)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages", "Sync completed successfully")
	if n := len(dst.appended("Scans")); n != 3 {
		t.Errorf("dst saw %d APPENDs over both runs, want 3", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationHoldsCopiesOfASharedMessageId_Then_CopiesOnlyTheRest(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	source := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	tests := []struct {
		name      string
		wantCopy  []string
		held      int
		wantExtra int
	}{
		{name: "destination holds one", held: 1, wantCopy: source[1:], wantExtra: 2},
		{name: "destination holds two", held: 2, wantCopy: source[2:], wantExtra: 1},
		{name: "destination holds as many as the source", held: 3},
		{name: "destination holds more than the source", held: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			heldBodies := make([]string, tt.held)
			for i := range heldBodies {
				heldBodies[i] = subjectBody(id, fmt.Sprintf("held %d", i+1))
			}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, source...)})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, heldBodies...)})
			cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))
			wantStored := append(slices.Clone(heldBodies), tt.wantCopy...)

			first := runSyncOnce(t, cfg)

			if got := dst.folderBodies("Scans"); !slices.Equal(got, wantStored) {
				t.Errorf("dst Scans bodies after the first run = %q, want %q", got, wantStored)
			}
			if n := len(dst.appended("Scans")); n != len(tt.wantCopy) {
				t.Errorf("dst saw %d APPENDs in the first run, want %d", n, len(tt.wantCopy))
			}
			if len(tt.wantCopy) == 0 {
				assertContainsAll(t, first, "All folders already synced!")
				assertContainsNone(t, first, extraCopiesMarker)
			} else {
				assertContainsAll(t, first, fmt.Sprintf("• Scans → Scans will copy %d messages", len(tt.wantCopy)))
				assertExtraCopiesNote(t, first, "Scans", len(tt.wantCopy), tt.wantExtra)
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			assertContainsNone(t, second, extraCopiesMarker)
			if n := len(dst.appended("Scans")); n != len(tt.wantCopy) {
				t.Errorf("dst saw %d APPENDs over both runs, want %d", n, len(tt.wantCopy))
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_DestinationKeepsOneCopyPerMessageId_Then_EveryRunNotesTheExtraCopies(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	dst.keepOnePerMessageID()
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "• Scans → Scans will copy 3 messages", "Sync completed successfully")
	assertContainsNone(t, first, extraCopiesMarker)
	assertIDs(t, "dst Scans messages after the first run", dst.folderMsgIDs("Scans"), id)
	if n := len(dst.appended("Scans")); n != 3 {
		t.Errorf("dst saw %d APPENDs in the first run, want 3", n)
	}

	for _, run := range []struct {
		name        string
		wantAppends int
	}{{"second", 5}, {"third", 7}} {
		out := runSyncOnce(t, cfg)

		assertContainsAll(t, out, "• Scans → Scans will copy 2 messages", "Sync completed successfully")
		assertExtraCopiesNote(t, out, "Scans", 2, 2)
		assertIDs(t, "dst Scans messages after the "+run.name+" run", dst.folderMsgIDs("Scans"), id)
		if n := len(dst.appended("Scans")); n != run.wantAppends {
			t.Errorf("dst saw %d APPENDs after the %s run, want %d", n, run.name, run.wantAppends)
		}
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationHoldsOneOfThreeCopies_Then_QuietRunPrintsNoNote(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	source := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, source...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, subjectBody(id, "held"))})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.quiet = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Scans", "Scans")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsNone(t, out.String(), extraCopiesMarker, "will copy")
	if n := len(dst.appended("Scans")); n != 2 {
		t.Errorf("dst saw %d APPENDs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_VerbosePreviewHasCopiesTheDestinationHolds_Then_NoteComesBeforeTheUIDList(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	source := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, source...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, subjectBody(id, "held"))})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Scans", "Scans")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertExtraCopiesNote(t, got, "Scans", 2, 2)
	willCopy := strings.Index(got, "will copy 2 messages")
	note := strings.Index(got, extraCopiesMarker)
	firstUID := strings.Index(got, "  • UID ")
	if willCopy < 0 || note < 0 || firstUID < 0 || willCopy >= note || note >= firstUID {
		t.Errorf("positions: will copy line %d, note %d, first UID line %d, want them in this order\noutput: %q", willCopy, note, firstUID, got)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_FolderHasSkippedAndExistingMessages_Then_PreviewSizeCountsOnlyPlannedMessages(t *testing.T) {
	t.Parallel()

	noID := bodyOfSize("", 900)
	existing := bodyOfSize("have@x", 950)
	plannedA := []string{bodyOfSize("a1@x", 120), bodyOfSize("a2@x", 150)}
	plannedB := []string{bodyOfSize("b1@x", 200)}
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": {{body: noID}, {msgID: "have@x", body: existing}, {msgID: "a1@x", body: plannedA[0]}, {msgID: "a2@x", body: plannedA[1]}},
		"B": {{msgID: "b1@x", body: plannedB[0]}},
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": {{msgID: "have@x", body: existing}}})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got,
		fmt.Sprintf("• A → A will copy 2 messages (≈ %d B)\n", totalSize(plannedA...)),
		fmt.Sprintf("• B → B will copy 1 messages (≈ %d B)\n", totalSize(plannedB...)),
		fmt.Sprintf("Total new messages to sync: 3 (≈ %d B)", totalSize(plannedA...)+totalSize(plannedB...)),
	)
	if n := strings.Count(got, "without Message-Id will be skipped"); n != 1 {
		t.Errorf("output holds %d warnings about a missing Message-Id, want 1\noutput: %q", n, got)
	}
	assertContainsAll(t, got, "A: 1 message(s) without Message-Id will be skipped")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationFoldersHoldMessagesWithoutMessageId_Then_OnlyTheSourceFolderIsWarned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hide string
		name string
	}{
		{name: "listed destination folders"},
		{name: "destination folder that LIST hides", hide: "A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": {{body: bodyOfSize("", 900)}, {msgID: "a1@x"}},
				"B": fakeMsgs("b1@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": nil,
				"A":     {{body: bodyOfSize("", 700)}},
				"B":     {{body: bodyOfSize("", 600)}, {body: bodyOfSize("", 650)}},
			})
			if tt.hide != "" {
				dst.hideFromList(tt.hide)
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			got := out.String()
			if n := strings.Count(got, "without Message-Id"); n != 1 {
				t.Errorf("output holds %d warnings about a missing Message-Id, want 1\noutput: %q", n, got)
			}
			assertContainsAll(t, got, "[src] ⚠️  A: 1 message(s) without Message-Id will be skipped - sync cannot track them")
			assertIDs(t, "dst A appends", dst.appended("A"), "a1@x")
			assertIDs(t, "dst B appends", dst.appended("B"), "b1@x")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_ExistingDestinationForcesASourceRescan_Then_TheMissingMessageIdWarningShowsOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "default output"},
		{name: "verbose output", verbose: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": {{body: bodyOfSize("", 900)}, {msgID: "a1@x"}, {msgID: "a2@x"}}})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "A": fakeMsgs("a1@x")})
			dst.hideFromList("A")
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, tt.verbose

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			raw := out.String()
			const warning = "[src] ⚠️  A: 1 message(s) without Message-Id will be skipped - sync cannot track them"
			if n := strings.Count(raw, "without Message-Id"); n != 1 {
				t.Errorf("raw output holds %d warnings about a missing Message-Id, want 1\noutput: %q", n, raw)
			}
			rows := screenRows(t, raw)
			assertOneRow(t, rows, warning)
			assertOneRow(t, rows, "Destination folder A already exists and holds 1 of the 2 planned messages from A")
			assertIDs(t, "dst appends", dst.appended("A"), "a2@x")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_ScanWarnsAboutMessagesWithoutMessageId_Then_TheWarningAndBothScanLinesStayOnScreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src      map[string][]fakeMsg
		dst      map[string][]fakeMsg
		name     string
		mappings []config.DirectoryMapping
		warnings []string
	}{
		{
			name:     "one folder",
			src:      map[string][]fakeMsg{"INBOX": {{body: bodyOfSize("", 900)}, {body: bodyOfSize("", 800)}}},
			dst:      map[string][]fakeMsg{"INBOX": nil},
			mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")},
			warnings: []string{"[src] ⚠️  INBOX: 2 message(s) without Message-Id will be skipped - sync cannot track them"},
		},
		{
			name: "two folders",
			src: map[string][]fakeMsg{
				"A": {{body: bodyOfSize("", 900)}},
				"B": {{body: bodyOfSize("", 800)}},
			},
			dst: map[string][]fakeMsg{
				"INBOX": nil,
				"A":     {{body: bodyOfSize("", 700)}},
				"B":     {{body: bodyOfSize("", 600)}},
			},
			mappings: []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B")},
			warnings: []string{
				"[src] ⚠️  A: 1 message(s) without Message-Id will be skipped - sync cannot track them",
				"[src] ⚠️  B: 1 message(s) without Message-Id will be skipped - sync cannot track them",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "/", tt.dst)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			rows := screenRows(t, out.String())
			synced := assertOneRow(t, rows, "✅ All folders already synced!")
			for _, warning := range tt.warnings {
				if i := assertOneRow(t, rows, warning); i > synced {
					t.Errorf("warning row %d comes after the summary row %d: %q", i, synced, warning)
				}
			}
			assertOneRow(t, rows, "[src] Scanned", "✓ done")
			assertOneRow(t, rows, "[dst] Scanned", "✓ done")
		})
	}
}

func TestRunSync_When_CreateReportsExistingFolderHoldingASharedMessageId_Then_CopiesTheRest(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	source := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	held := subjectBody(id, "held")
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": sharedIDMsgs(id, source...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": sharedIDMsgs(id, held)})
	dst.hideFromList("Work")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"  ℹ️  Destination folder Work already exists and holds 1 of the 3 planned messages from Work, so they are not copied\n",
		"Sync completed successfully",
	)
	if got, want := dst.folderBodies("Work"), []string{held, source[1], source[2]}; !slices.Equal(got, want) {
		t.Errorf("dst Work bodies after the first run = %q, want %q", got, want)
	}
	if n := len(dst.appended("Work")); n != 2 {
		t.Errorf("dst saw %d APPENDs in the first run, want 2", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "holds 3 of the 3 planned messages", "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages", "Sync completed successfully")
	if n := len(dst.appended("Work")); n != 2 {
		t.Errorf("dst saw %d APPENDs over both runs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func scanCopies(t *testing.T, id string) (one, two, three string) {
	t.Helper()

	one, two, three = subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")
	if len(three) == len(one) || len(three) == len(two) {
		t.Fatalf("body sizes = %d, %d, %d, want the third to differ from the others", len(one), len(two), len(three))
	}
	return one, two, three
}

func TestRunSync_When_DestinationHoldsOnlyTheHighestUIDCopy_Then_CopiesTheOtherCopiesOnce(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	one, two, three := scanCopies(t, id)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, two, three)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, three)})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, fmt.Sprintf("• Scans → Scans will copy 2 messages (≈ %d B)", len(one)+len(two)))
	if got, want := dst.folderBodies("Scans"), []string{three, one, two}; !slices.Equal(got, want) {
		t.Errorf("dst Scans bodies after the first run = %q, want %q", got, want)
	}
	if n := len(dst.appended("Scans")); n != 2 {
		t.Errorf("dst saw %d APPENDs in the first run, want 2", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	assertContainsNone(t, second, "Sync completed successfully")
	if n := len(dst.appended("Scans")); n != 2 {
		t.Errorf("dst saw %d APPENDs over both runs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationMissesAMiddleCopy_Then_CopiesOnlyThatCopy(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	one, two, three := scanCopies(t, id)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, two, three)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, three)})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)", len(two)))
	if got, want := dst.folderBodies("Scans"), []string{one, three, two}; !slices.Equal(got, want) {
		t.Errorf("dst Scans bodies after the first run = %q, want %q", got, want)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	if n := len(dst.appended("Scans")); n != 1 {
		t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationScanSendsNoUIDForSharedMessageIdCopies_Then_CopiesOnlyTheMissingCopy(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	one, two, three := scanCopies(t, id)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, two, three)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, three)})
	dst.omitScanUIDIn("Scans")
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)", len(two)))
	assertContainsNone(t, first, "server sent no UID")
	if got, want := dst.folderBodies("Scans"), []string{one, three, two}; !slices.Equal(got, want) {
		t.Errorf("dst Scans bodies after the first run = %q, want %q", got, want)
	}
	if n := len(dst.appended("Scans")); n != 1 {
		t.Errorf("dst saw %d APPENDs in the first run, want 1", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	assertContainsNone(t, second, "Sync completed successfully")
	if n := len(dst.appended("Scans")); n != 1 {
		t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_ExistingDestinationScanSendsNoUID_Then_TheRecheckDropsTheHeldMessage(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")})
	dst.hideFromList("Work")
	dst.omitScanUIDIn("Work")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"  ℹ️  Destination folder Work already exists and holds 1 of the 2 planned messages from Work, so they are not copied\n",
		"Sync completed successfully",
	)
	assertContainsNone(t, first, folderFailuresHeader, "server sent no UID")
	assertIDs(t, "dst Work after the first run", dst.folderMsgIDs("Work"), "m1@x", "m2@x")
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Work"), "m2@x")

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "holds 2 of the 2 planned messages", "All folders already synced!")
	assertContainsNone(t, second, folderFailuresHeader, "Sync completed successfully")
	assertIDs(t, "dst APPENDs over both runs", dst.appended("Work"), "m2@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_ExistingFolderHoldsOnlyTheHighestUIDCopy_Then_TheRecheckCopiesTheOtherCopies(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	one, two, three := scanCopies(t, id)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, one, two, three)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Scans": sharedIDMsgs(id, three)})
	dst.hideFromList("Scans")
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"  ℹ️  Destination folder Scans already exists and holds 1 of the 3 planned messages from Scans, so they are not copied\n",
		"Sync completed successfully",
	)
	if got, want := dst.folderBodies("Scans"), []string{three, one, two}; !slices.Equal(got, want) {
		t.Errorf("dst Scans bodies after the first run = %q, want %q", got, want)
	}
	if n := len(dst.appended("Scans")); n != 2 {
		t.Errorf("dst saw %d APPENDs in the first run, want 2", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "holds 3 of the 3 planned messages", "All folders already synced!")
	assertContainsNone(t, second, "Sync completed successfully")
	if n := len(dst.appended("Scans")); n != 2 {
		t.Errorf("dst saw %d APPENDs over both runs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SharedDestinationCopiesDifferInSize_Then_EachDistinctCopyArrivesOnce(t *testing.T) {
	t.Parallel()

	const id = "m@x"
	small, large := bodyOfSize(id, 300), bodyOfSize(id, 500)
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": sharedIDMsgs(id, small),
		"B": sharedIDMsgs(id, large, small),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("A", "X"), mapping("B", "X"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "• A → X will copy 1 messages", "• B → X will copy 1 messages", "Sync completed successfully")
	if got, want := slices.Sorted(slices.Values(dst.folderBodies("X"))), slices.Sorted(slices.Values([]string{small, large})); !slices.Equal(got, want) {
		t.Errorf("dst X holds %d bodies of sizes %v, want one of 300 and one of 500 bytes", len(got), bodySizes(got))
	}
	if n := dst.callCount("APPEND"); n != 2 {
		t.Errorf("destination saw %d APPEND commands, want 2", n)
	}

	assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
	if n := dst.callCount("APPEND"); n != 2 {
		t.Errorf("destination saw %d APPEND commands after the second run, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func bodySizes(bodies []string) []int {
	sizes := make([]int, len(bodies))
	for i, b := range bodies {
		sizes[i] = len(b)
	}
	return sizes
}

func TestRunSync_When_ExistingDestinationIsEmpty_Then_SourceIsNotScannedAgain(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": nil})
	dst.hideFromList("Work")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
	assertContainsNone(t, out.String(), "already exists and holds")
	// The copy reads bodies with UID FETCH, so every plain FETCH is a Message-Id scan.
	if n := src.callCount("FETCH"); n != 1 {
		t.Errorf("source saw %d Message-Id scans, want 1: the planning scan only", n)
	}
	assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "Work")
}

func TestRunSync_When_CreateSucceeds_Then_NewFolderIsNotScanned(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
	if slices.Contains(dst.capturedNames("EXAMINE"), "Work") {
		t.Error("dst received EXAMINE for Work, want none: CREATE made the folder, so it is empty")
	}
	assertContainsNone(t, out.String(), "already exists and holds")
}

func TestRunSync_When_DestinationListsInvalidNameAndCreateAnswersOKForFolderWithMessages_Then_CopiesOnlyTheDifference(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":    nil,
		"Входящие": nil,
		"Work":     fakeMsgs("m1@x", "m2@x"),
	})
	dst.hideFromList("Work")
	dst.refuseCreateWith("OK CREATE completed")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
	runOnce := func() string {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true
		if err := runSync(e2eContext(t), cfg, opts); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		return out.String()
	}

	first := runOnce()

	assertIDs(t, "dst Work after the first run", dst.folderMsgIDs("Work"), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Work"), "m3@x")
	assertContainsAll(t, first,
		leftOutLine("dst", "Входящие"),
		"  ℹ️  Destination folder Work already exists and holds 2 of the 3 planned messages from Work, so they are not copied\n",
		"Syncing messages",
		"Sync completed successfully",
	)

	second := runOnce()

	assertIDs(t, "dst Work after the second run", dst.folderMsgIDs("Work"), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs over both runs", dst.appended("Work"), "m3@x")
	assertContainsAll(t, second, "holds 3 of the 3 planned messages", "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work", "Work")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationListsInvalidNameAndCreateMakesFolder_Then_RecheckReadsOnlyTheDestination(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Входящие": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
	assertContainsAll(t, out.String(), leftOutLine("dst", "Входящие"))
	assertContainsNone(t, out.String(), "already exists and holds")
	// The copy reads bodies with UID FETCH, so every plain FETCH is a Message-Id scan.
	if n := src.callCount("FETCH"); n != 1 {
		t.Errorf("source saw %d Message-Id scans, want 1: the planning scan only", n)
	}
	assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "Work")
}

func TestRunSync_When_ExistingDestinationHoldsMessagesInQuietMode_Then_PrintsNoInfoLine(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
	dst.hideFromList("Work")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.quiet = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs to Work", dst.appended("Work"), "m3@x")
	assertContainsNone(t, out.String(), "already exists and holds")
}

func TestRunSync_When_ExistingDestinationCannotBeScanned_Then_ReportsFolderFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withOther bool
		quiet     bool
	}{
		{name: "another plan still copies", withOther: true},
		{name: "another plan still copies in quiet mode", withOther: true, quiet: true},
		{name: "no plan left"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcFolders := map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x")}
			mappings := []config.DirectoryMapping{mapping("Work", "Work")}
			if tt.withOther {
				srcFolders["Other"] = fakeMsgs("o1@x")
				mappings = append(mappings, mapping("Other", "Other"))
			}
			src := newStoreServer(t, "/", srcFolders)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")})
			dst.hideFromList("Work")
			dst.refuseOpenWith("Work", "NO "+hostileReply)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mappings...), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit", err)
			}
			got := out.String()
			assertContainsAll(t, got,
				"• Work → Work: cannot scan destination folder after CREATE: ",
				hostileReplyShown,
				"1 folders failed",
			)
			assertContainsNone(t, got, "already exists and holds", "\x1b]0;pwned", "\x07", "\u202e")
			assertIDs(t, "dst APPENDs to Work", dst.appended("Work"))
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
			if tt.withOther {
				assertIDs(t, "dst Other", dst.folderMsgIDs("Other"), "o1@x")
				wantHeader := 2
				if tt.quiet {
					wantHeader = 1
				} else {
					assertContainsAll(t, got, "Syncing messages")
				}
				if n := strings.Count(got, folderFailuresHeader); n != wantHeader {
					t.Errorf("output holds the failure header %d times, want %d\noutput: %q", n, wantHeader, got)
				}
				return
			}
			assertContainsNone(t, got, "Syncing messages", "All folders already synced!")
			if n := strings.Count(got, folderFailuresHeader); n != 1 {
				t.Errorf("output holds the failure header %d times, want 1\noutput: %q", n, got)
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
			if n := dst.callCount("LOGIN"); n != 1 {
				t.Errorf("dst saw %d LOGIN commands, want 1: no worker may connect", n)
			}
		})
	}
}

func TestRunSync_When_SourceRescanFails_Then_ReportsFolderFailure(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")})
	dst.hideFromList("Work")
	// The rescan can reuse the selected session and send STATUS, so both commands fail.
	dst.onCommand("CREATE", func() {
		src.refuseOpenWith("Work", "NO [CANNOT] folder gone")
		src.refuseStatusWith("Work", "NO [CANNOT] folder gone")
	})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Work", "Work")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), folderFailuresHeader, "• Work → Work: cannot scan source folder: ", "folder gone", "1 folders failed")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x")
}

func TestRunSync_When_ContextEndsDuringDestinationRecheck_Then_ReturnsCanceledWithoutAppend(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")})
	dst.hideFromList("Work")
	// Work is not cached, so the planning scan sends no EXAMINE and this one is the recheck's.
	dst.onCommand("EXAMINE", cancel)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(ctx, e2eConfig(src, dst, 1, mapping("Work", "Work")), opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0", n)
	}
	assertContainsNone(t, out.String(), folderFailuresHeader)
}

func Test_recheckExistingDestinations_When_ContextAlreadyEnded_Then_SendsNoCommand(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": fakeMsgs("b1@x")})
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	plans := []FolderSyncPlan{
		{SourceFolder: "A", DestinationFolder: "A", SrcUIDs: []uint32{1}, NewMessages: 1},
		{SourceFolder: "B", DestinationFolder: "B", SrcUIDs: []uint32{1}, NewMessages: 1},
	}
	existing := map[string]struct{}{"B": {}}
	out := &syncBuffer{}

	kept, failures, err := recheckExistingDestinations(ctx, out, false, srcC, dstC, plans, existing, &throttleStop{})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if kept != nil || failures != nil {
		t.Errorf("kept = %v, failures = %v, want both nil", kept, failures)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
	for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
		for _, verb := range []string{"EXAMINE", "STATUS", "FETCH"} {
			if n := srv.callCount(verb); n != 0 {
				t.Errorf("%s saw %d %s commands, want 0", name, n, verb)
			}
		}
	}
}

func Test_recheckExistingDestinations_When_PlannedMessagesAreDropped_Then_EachCauseHasItsOwnLine(t *testing.T) {
	t.Parallel()

	type folderNames struct {
		src, dst, srcWire, dstWire, srcShown, dstShown string
	}
	plain := folderNames{src: "Work", dst: "Work", srcWire: "Work", dstWire: "Work", srcShown: "Work", dstShown: "Work"}
	hostile := folderNames{
		src:      "S\x1b]0;p\xe2\x80\xae\xe3\x85\xa4",
		dst:      "D\x1b]0;p\xe2\x80\xae\xe3\x85\xa4",
		srcWire:  "S&ABs-]0;p&IC4xZA-",
		dstWire:  "D&ABs-]0;p&IC4xZA-",
		srcShown: `S\x1b]0;p\u202e\u3164`,
		dstShown: `D\x1b]0;p\u202e\u3164`,
	}
	heldLine := func(n folderNames, count int) string {
		return fmt.Sprintf("  ℹ️  Destination folder %s already exists and holds %d of the 3 planned messages from %s, so they are not copied\n",
			n.dstShown, count, n.srcShown)
	}
	goneLine := func(n folderNames, count int) string {
		return fmt.Sprintf("  ℹ️  Source folder %s no longer holds %d of the 3 planned messages for %s, so they are not copied\n",
			n.srcShown, count, n.dstShown)
	}
	tests := []struct {
		wantOut  func(n folderNames) string
		names    folderNames
		name     string
		srcIDs   []string
		dstIDs   []string
		wantKept []uint32
		quiet    bool
	}{
		{
			name:     "destination holds some",
			names:    plain,
			srcIDs:   []string{"m1@x", "m2@x", "m3@x"},
			dstIDs:   []string{"m1@x", "m2@x"},
			wantKept: []uint32{3},
			wantOut:  func(n folderNames) string { return heldLine(n, 2) },
		},
		{
			name:     "source lost one",
			names:    plain,
			srcIDs:   []string{"m1@x", "m2@x"},
			dstIDs:   []string{"other@x"},
			wantKept: []uint32{1, 2},
			wantOut:  func(n folderNames) string { return goneLine(n, 1) },
		},
		{
			name:     "both causes",
			names:    plain,
			srcIDs:   []string{"m1@x", "m2@x"},
			dstIDs:   []string{"m1@x"},
			wantKept: []uint32{2},
			wantOut:  func(n folderNames) string { return heldLine(n, 1) + goneLine(n, 1) },
		},
		{
			name:     "nothing left",
			names:    plain,
			srcIDs:   []string{"m1@x"},
			dstIDs:   []string{"m1@x"},
			wantKept: nil,
			wantOut:  func(n folderNames) string { return heldLine(n, 1) + goneLine(n, 2) },
		},
		{
			name:     "both causes in quiet mode",
			names:    plain,
			quiet:    true,
			srcIDs:   []string{"m1@x", "m2@x"},
			dstIDs:   []string{"m1@x"},
			wantKept: []uint32{2},
			wantOut:  func(folderNames) string { return "" },
		},
		{
			name:     "nothing dropped",
			names:    plain,
			srcIDs:   []string{"m1@x", "m2@x", "m3@x"},
			dstIDs:   []string{"other@x"},
			wantKept: []uint32{1, 2, 3},
			wantOut:  func(folderNames) string { return "" },
		},
		{
			name:     "names with control characters",
			names:    hostile,
			srcIDs:   []string{"m1@x", "m2@x"},
			dstIDs:   []string{"m1@x"},
			wantKept: []uint32{2},
			wantOut:  func(n folderNames) string { return heldLine(n, 1) + goneLine(n, 1) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{tt.names.srcWire: fakeMsgs(tt.srcIDs...)})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{tt.names.dstWire: fakeMsgs(tt.dstIDs...)})
			srcC := newAppClient(t, src, "src")
			dstC := newAppClient(t, dst, "dst")
			plans := []FolderSyncPlan{{
				SourceFolder:      tt.names.src,
				DestinationFolder: tt.names.dst,
				SrcUIDs:           []uint32{1, 2, 3},
				NewMessages:       3,
				NewSize:           300,
			}}
			existing := map[string]struct{}{tt.names.dst: {}}
			out := &syncBuffer{}

			kept, failures, err := recheckExistingDestinations(t.Context(), out, tt.quiet, srcC, dstC, plans, existing, &throttleStop{})

			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if failures != nil {
				t.Errorf("failures = %v, want nil", failures)
			}
			if want := tt.wantOut(tt.names); out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
			assertContainsNone(t, out.String(), "\x1b]0;", "\xe2\x80\xae", "\xe3\x85\xa4", "holds 0 of")
			if tt.wantKept == nil {
				if len(kept) != 0 {
					t.Fatalf("kept = %v, want no plan", kept)
				}
				return
			}
			if len(kept) != 1 {
				t.Fatalf("kept = %v, want one plan", kept)
			}
			if !slices.Equal(kept[0].SrcUIDs, tt.wantKept) || kept[0].NewMessages != len(tt.wantKept) {
				t.Errorf("kept SrcUIDs = %v, NewMessages = %d, want %v and %d", kept[0].SrcUIDs, kept[0].NewMessages, tt.wantKept, len(tt.wantKept))
			}
			if kept[0].SourceFolder != tt.names.src || kept[0].DestinationFolder != tt.names.dst {
				t.Errorf("kept folders = %q → %q, want the exact plan names %q → %q", kept[0].SourceFolder, kept[0].DestinationFolder, tt.names.src, tt.names.dst)
			}
		})
	}
}

func sharedPlan(src, dst string, size uint64, uids ...uint32) FolderSyncPlan {
	return FolderSyncPlan{SourceFolder: src, DestinationFolder: dst, SrcUIDs: uids, NewMessages: len(uids), NewSize: size}
}

func Test_recheckExistingDestinations_When_PlansShareADestination_Then_TheyAreComparedTogether(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	small, large := bodyOfSize(id, 200), bodyOfSize(id, 300)
	heldLine := func(dst string, count, planned int, src string) string {
		return fmt.Sprintf("  ℹ️  Destination folder %s already exists and holds %d of the %d planned messages from %s, so they are not copied\n", dst, count, planned, src)
	}
	goneLine := func(src string, count, planned int, dst string) string {
		return fmt.Sprintf("  ℹ️  Source folder %s no longer holds %d of the %d planned messages for %s, so they are not copied\n", src, count, planned, dst)
	}
	type keptPlan struct {
		src, dst string
		uids     []uint32
		size     uint64
	}
	reportedSrc := map[string][]fakeMsg{
		"A": {{msgID: id, body: small}},
		"B": {{msgID: id, body: small}, {msgID: id, body: large}},
	}
	reportedDst := map[string][]fakeMsg{"D": {{msgID: id, body: large}}}
	reportedPlans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1), sharedPlan("B", "D", 300, 2)}
	tests := []struct {
		srcFolders map[string][]fakeMsg
		dstFolders map[string][]fakeMsg
		name       string
		wantOut    string
		plans      []FolderSyncPlan
		existing   []string
		wantKept   []keptPlan
		quiet      bool
	}{
		{
			name:       "the destination holds the copy that matches the second plan",
			srcFolders: reportedSrc,
			dstFolders: reportedDst,
			plans:      reportedPlans,
			existing:   []string{"D"},
			wantOut:    heldLine("D", 1, 1, "B"),
			wantKept:   []keptPlan{{src: "A", dst: "D", uids: []uint32{1}, size: 200}},
		},
		{
			name:       "the same case in quiet mode",
			quiet:      true,
			srcFolders: reportedSrc,
			dstFolders: reportedDst,
			plans:      reportedPlans,
			existing:   []string{"D"},
			wantOut:    "",
			wantKept:   []keptPlan{{src: "A", dst: "D", uids: []uint32{1}, size: 200}},
		},
		{
			name: "the first planned message is gone and the second is held",
			srcFolders: map[string][]fakeMsg{
				"A": fakeMsgs("other@x"),
				"B": {{msgID: id, body: small}, {msgID: id, body: large}},
			},
			dstFolders: reportedDst,
			plans:      []FolderSyncPlan{sharedPlan("A", "D", 200, 3), sharedPlan("B", "D", 300, 2)},
			existing:   []string{"D"},
			wantOut:    goneLine("A", 1, 1, "D") + heldLine("D", 1, 1, "B"),
		},
		{
			name: "plans of two destinations are interleaved",
			srcFolders: map[string][]fakeMsg{
				"A": {{msgID: id, body: small}},
				"X": fakeMsgs("m1@x", "m2@x"),
				"B": {{msgID: id, body: small}, {msgID: id, body: large}},
			},
			dstFolders: map[string][]fakeMsg{"D": {{msgID: id, body: large}}, "E": fakeMsgs("m1@x")},
			plans:      []FolderSyncPlan{sharedPlan("A", "D", 500, 1, 2), sharedPlan("X", "E", 200, 1, 2), sharedPlan("B", "D", 300, 2)},
			existing:   []string{"D", "E"},
			wantOut:    goneLine("A", 1, 2, "D") + heldLine("E", 1, 2, "X") + heldLine("D", 1, 1, "B"),
			wantKept: []keptPlan{
				{src: "A", dst: "D", uids: []uint32{1}, size: 200},
				{src: "X", dst: "E", uids: []uint32{2}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcC := newAppClient(t, newStoreServer(t, "/", tt.srcFolders), "src")
			dstC := newAppClient(t, newStoreServer(t, "/", tt.dstFolders), "dst")
			existing := make(map[string]struct{}, len(tt.existing))
			for _, folder := range tt.existing {
				existing[folder] = struct{}{}
			}
			plannedUIDs := make([][]uint32, len(tt.plans))
			for i, plan := range tt.plans {
				plannedUIDs[i] = slices.Clone(plan.SrcUIDs)
			}
			out := &syncBuffer{}

			kept, failures, err := recheckExistingDestinations(t.Context(), out, tt.quiet, srcC, dstC, tt.plans, existing, &throttleStop{})

			if err != nil || failures != nil {
				t.Fatalf("error = %v, failures = %v, want both nil", err, failures)
			}
			if out.String() != tt.wantOut {
				t.Errorf("output = %q, want %q", out.String(), tt.wantOut)
			}
			if len(kept) != len(tt.wantKept) {
				t.Fatalf("kept %d plans %+v, want %d", len(kept), kept, len(tt.wantKept))
			}
			for i, want := range tt.wantKept {
				got := kept[i]
				if got.SourceFolder != want.src || got.DestinationFolder != want.dst || !slices.Equal(got.SrcUIDs, want.uids) || got.NewMessages != len(want.uids) {
					t.Errorf("kept[%d] = %s → %s %v (%d), want %s → %s %v", i, got.SourceFolder, got.DestinationFolder, got.SrcUIDs, got.NewMessages, want.src, want.dst, want.uids)
				}
				if want.size != 0 && got.NewSize != want.size {
					t.Errorf("kept[%d] NewSize = %d, want %d", i, got.NewSize, want.size)
				}
				if !got.DestinationFolderExists {
					t.Errorf("kept[%d] DestinationFolderExists = false, want true", i)
				}
			}
			for i, plan := range tt.plans {
				if !slices.Equal(plan.SrcUIDs, plannedUIDs[i]) {
					t.Errorf("the input SrcUIDs of plan %d changed to %v, was %v", i, plan.SrcUIDs, plannedUIDs[i])
				}
			}
		})
	}
}

func Test_recheckExistingDestinations_When_PlansShareADestination_Then_TheDestinationIsReadOnce(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": {{msgID: id, body: bodyOfSize(id, 200)}},
		"B": {{msgID: id, body: bodyOfSize(id, 200)}, {msgID: id, body: bodyOfSize(id, 300)}},
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": {{msgID: id, body: bodyOfSize(id, 300)}}})
	plans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1), sharedPlan("B", "D", 300, 2)}

	_, _, err := recheckExistingDestinations(t.Context(), &syncBuffer{}, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"D": {}}, &throttleStop{})

	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if n := dst.callCount("FETCH"); n != 1 {
		t.Errorf("destination saw %d Message-Id FETCH commands, want 1", n)
	}
	if n := dst.callCount("EXAMINE"); n != 1 {
		t.Errorf("destination saw %d EXAMINE commands, want 1", n)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "A", "B")
}

func Test_recheckExistingDestinations_When_OneSharedPlanCannotBeRescanned_Then_OnlyItFails(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	tests := []struct {
		breakSource func(src *fakeServer)
		wantErr     error
		name        string
		wantReason  string
	}{
		{
			name:        "the source refuses to open the folder",
			breakSource: func(src *fakeServer) { src.refuseOpenWith("A", "NO [CANNOT] folder gone") },
			wantErr:     errSourceScan,
			wantReason:  "folder gone",
		},
		{
			name:        "the source folder has a new UIDVALIDITY",
			breakSource: func(src *fakeServer) { src.setUIDValidity("A", 9) },
			wantErr:     client.ErrUIDValidityChanged,
			wantReason:  "source folder changed after the sync plan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": {{msgID: id, body: bodyOfSize(id, 200)}},
				"B": {{msgID: id, body: bodyOfSize(id, 200)}, {msgID: id, body: bodyOfSize(id, 300)}},
			})
			tt.breakSource(src)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": {{msgID: id, body: bodyOfSize(id, 300)}}})
			plans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1), sharedPlan("B", "D", 300, 2)}
			plans[0].SrcUIDValidity = 1
			plans[1].SrcUIDValidity = 1
			out := &syncBuffer{}

			kept, failures, err := recheckExistingDestinations(t.Context(), out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"D": {}}, &throttleStop{})

			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if len(failures) != 1 || failures[0].SourceFolder != "A" || failures[0].DestinationFolder != "D" {
				t.Fatalf("failures = %+v, want one failure for A → D", failures)
			}
			if !errors.Is(failures[0].Err, tt.wantErr) || !strings.Contains(failures[0].Err.Error(), tt.wantReason) {
				t.Errorf("failure error = %v, want %v and %q", failures[0].Err, tt.wantErr, tt.wantReason)
			}
			if errors.Is(failures[0].Err, errDestinationRecheck) {
				t.Errorf("failure error = %v, must not wrap errDestinationRecheck", failures[0].Err)
			}
			want := "  ℹ️  Destination folder D already exists and holds 1 of the 1 planned messages from B, so they are not copied\n"
			if out.String() != want {
				t.Errorf("output = %q, want only the line for B %q", out.String(), want)
			}
			if len(kept) != 0 {
				t.Errorf("kept = %+v, want none: B is covered by the destination", kept)
			}
		})
	}
}

func Test_recheckExistingDestinations_When_SharedDestinationCannotBeRead_Then_EveryPlanOfItFails(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": {{msgID: id, body: bodyOfSize(id, 200)}},
		"B": {{msgID: id, body: bodyOfSize(id, 200)}, {msgID: id, body: bodyOfSize(id, 300)}},
		"X": fakeMsgs("m1@x", "m2@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": nil, "E": fakeMsgs("m1@x")})
	dst.refuseOpenWith("D", "NO [CANNOT] folder gone")
	plans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1), sharedPlan("X", "E", 200, 1, 2), sharedPlan("B", "D", 300, 2)}
	out := &syncBuffer{}

	kept, failures, err := recheckExistingDestinations(t.Context(), out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"D": {}, "E": {}}, &throttleStop{})

	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if len(failures) != 2 || failures[0].SourceFolder != "A" || failures[1].SourceFolder != "B" {
		t.Fatalf("failures = %+v, want one failure each for A and B, in plan order", failures)
	}
	for _, f := range failures {
		if !errors.Is(f.Err, errDestinationRecheck) || errors.Is(f.Err, errSourceScan) || !strings.Contains(f.Err.Error(), "folder gone") {
			t.Errorf("failure error for %s = %v, want errDestinationRecheck with the reason", f.SourceFolder, f.Err)
		}
	}
	if len(kept) != 1 || kept[0].SourceFolder != "X" || !slices.Equal(kept[0].SrcUIDs, []uint32{2}) {
		t.Errorf("kept = %+v, want the plan of X with UID 2", kept)
	}
	if want := "  ℹ️  Destination folder E already exists and holds 1 of the 2 planned messages from X, so they are not copied\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "X")
}

func Test_recheckExistingDestinations_When_SharedDestinationIsEmpty_Then_NoSourceIsScanned(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": nil})
	plans := []FolderSyncPlan{sharedPlan("A", "D", 100, 1), sharedPlan("B", "D", 100, 1)}
	out := &syncBuffer{}

	kept, failures, err := recheckExistingDestinations(t.Context(), out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"D": {}}, &throttleStop{})

	if err != nil || failures != nil {
		t.Fatalf("error = %v, failures = %v, want both nil", err, failures)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %+v, want both plans", kept)
	}
	for i, plan := range kept {
		if !slices.Equal(plan.SrcUIDs, []uint32{1}) || plan.NewMessages != 1 || !plan.DestinationFolderExists {
			t.Errorf("kept[%d] = %+v, want UID 1, one message and DestinationFolderExists", i, plan)
		}
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
	for _, verb := range []string{"EXAMINE", "STATUS", "FETCH"} {
		if n := src.callCount(verb); n != 0 {
			t.Errorf("source saw %d %s commands, want 0", n, verb)
		}
	}
}

func Test_recheckExistingDestinations_When_ContextEndsBetweenSharedSourceScans_Then_ReturnsCanceledWithoutOutput(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": {{msgID: id, body: bodyOfSize(id, 200)}},
		"B": {{msgID: id, body: bodyOfSize(id, 200)}, {msgID: id, body: bodyOfSize(id, 300)}},
	})
	var examines atomic.Int32
	src.onCommand("EXAMINE", func() {
		if examines.Add(1) == 2 {
			cancel()
		}
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": {{msgID: id, body: bodyOfSize(id, 300)}}})
	plans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1), sharedPlan("B", "D", 300, 2)}
	out := &syncBuffer{}

	kept, failures, err := recheckExistingDestinations(ctx, out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"D": {}}, &throttleStop{})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if kept != nil || failures != nil {
		t.Errorf("kept = %v, failures = %v, want both nil", kept, failures)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
	if n := examines.Load(); n != 2 {
		t.Errorf("source saw %d EXAMINE commands, want 2: the cancel comes with the second one", n)
	}
}

func Test_destinationDiff_When_UIDsOfTwoSourcesCollide_Then_EachPlanKeepsOnlyItsOwnUIDs(t *testing.T) {
	t.Parallel()

	plans := []FolderSyncPlan{
		{SourceFolder: "A", DestinationFolder: "D", SrcUIDs: []uint32{5}, NewMessages: 1, NewSize: 200},
		{SourceFolder: "B", DestinationFolder: "D", SrcUIDs: []uint32{5}, NewMessages: 1, NewSize: 300},
	}
	dstMap := map[string][]client.MessageRef{"x@x": {{UID: 1, Size: 300}}}
	d := newDestinationDiff(plans, dstMap)

	d.addSource(0, map[string][]client.MessageRef{"x@x": {{UID: 5, Size: 200}}})
	d.addSource(1, map[string][]client.MessageRef{"x@x": {{UID: 4, Size: 200}, {UID: 5, Size: 300}}})
	got := d.results()

	if !slices.Equal(got[0].plan.SrcUIDs, []uint32{5}) || got[0].plan.NewSize != 200 || got[0].drops != (recheckDrops{}) {
		t.Errorf("plan A = %v (%d B), drops %+v, want UID 5 (200 B) and no drops", got[0].plan.SrcUIDs, got[0].plan.NewSize, got[0].drops)
	}
	if len(got[1].plan.SrcUIDs) != 0 || got[1].plan.NewMessages != 0 || got[1].drops != (recheckDrops{held: 1}) {
		t.Errorf("plan B = %v, drops %+v, want no UID and one held", got[1].plan.SrcUIDs, got[1].drops)
	}
}

// jointCase is one random destination: the mappings, the source maps and the destination copies.
type jointCase struct {
	srcMaps []map[string][]client.MessageRef
	dstMap  map[string][]client.MessageRef
	plans   []FolderSyncPlan
	slotOf  []int
}

// newJointCase plans random sources for one destination that LIST hides, as buildSyncPlan does, and adds the real destination copies.
func newJointCase(t *testing.T, rng *rand.Rand, rule copyRule) jointCase {
	t.Helper()

	ids := []string{"a@x", "b@x", "c@x"}
	slots := 2 + rng.IntN(3)
	mappings := make([]config.DirectoryMapping, slots)
	for i := range mappings {
		mappings[i] = mapping("S"+strconv.Itoa(i), "D")
	}
	jc := jointCase{srcMaps: make([]map[string][]client.MessageRef, slots), dstMap: make(map[string][]client.MessageRef)}
	for _, id := range ids {
		for k := range rng.IntN(4) {
			jc.dstMap[id] = append(jc.dstMap[id], client.MessageRef{UID: uint32(900 + k), Size: uint32(100 * (1 + rng.IntN(3)))})
		}
	}
	shared := newSharedDestinations(mappings)
	plans := make([]FolderSyncPlan, slots)
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64
	for i := range slots {
		jc.srcMaps[i] = make(map[string][]client.MessageRef)
		var uid uint32
		for _, id := range ids {
			for range rng.IntN(4) {
				uid += uint32(1 + rng.IntN(3))
				jc.srcMaps[i][id] = append(jc.srcMaps[i][id], client.MessageRef{UID: uid, Size: uint32(100 * (1 + rng.IntN(3)))})
			}
		}
		s := &folderScan{srcMap: maps.Clone(jc.srcMaps[i]), srcMessages: countRefs(jc.srcMaps[i]), srcUIDValidity: 1}
		diffBothSidesUnder(t, rule, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
	}
	for i, plan := range plans {
		if plan.SourceFolder != "" {
			jc.plans = append(jc.plans, plan)
			jc.slotOf = append(jc.slotOf, i)
		}
	}
	return jc
}

func Test_destinationDiff_When_PlansShareADestinationAndSourcesAreUnchanged_Then_TheDestinationEndsWithTheLargestCount(t *testing.T) {
	t.Parallel()

	for _, rule := range []copyRule{copyByCount, copyOnePerMessageID} {
		rng := rand.New(rand.NewPCG(41, 43+uint64(rule)))
		for round := range 500 {
			jc := newJointCase(t, rng, rule)
			if len(jc.plans) == 0 {
				continue
			}
			before := make([][]uint32, len(jc.plans))
			idOf := make([]map[uint32]string, len(jc.plans))
			sizeOf := make([]map[uint32]uint32, len(jc.plans))
			for k, plan := range jc.plans {
				before[k] = slices.Clone(plan.SrcUIDs)
				idOf[k] = make(map[uint32]string)
				sizeOf[k] = make(map[uint32]uint32)
				for id, refs := range jc.srcMaps[jc.slotOf[k]] {
					for _, ref := range refs {
						idOf[k][ref.UID] = id
						sizeOf[k][ref.UID] = ref.Size
					}
				}
			}

			d := newDestinationDiff(jc.plans, jc.dstMap)
			for k := range jc.plans {
				d.addSource(k, jc.srcMaps[jc.slotOf[k]])
			}
			got := d.results()

			keptPerID := make(map[string]int)
			for k, res := range got {
				if !slices.Equal(jc.plans[k].SrcUIDs, before[k]) {
					t.Fatalf("rule %d round %d plan %d: the input SrcUIDs changed to %v, was %v", rule, round, k, jc.plans[k].SrcUIDs, before[k])
				}
				var wantSize uint64
				var wantExtra, wantRepeated int
				for _, uid := range res.plan.SrcUIDs {
					if _, ok := slices.BinarySearch(before[k], uid); !ok {
						t.Fatalf("rule %d round %d plan %d: kept UID %d was not planned in %v", rule, round, k, uid, before[k])
					}
					id := idOf[k][uid]
					keptPerID[id]++
					wantSize += uint64(sizeOf[k][uid])
					if len(jc.dstMap[id]) > 0 {
						wantExtra++
					}
					all := jc.srcMaps[jc.slotOf[k]][id]
					wantRepeated += len(all) - len(rule.sourceCopies(all))
				}
				if res.drops.gone != 0 || res.drops.held+len(res.plan.SrcUIDs) != len(before[k]) {
					t.Fatalf("rule %d round %d plan %d: drops %+v with %d kept of %d planned, want gone 0 and held + kept = planned", rule, round, k, res.drops, len(res.plan.SrcUIDs), len(before[k]))
				}
				if res.plan.NewMessages != len(res.plan.SrcUIDs) || res.plan.NewSize != wantSize || res.plan.ExtraCopies != wantExtra || res.plan.RepeatedCopies != wantRepeated || !res.plan.DestinationFolderExists {
					t.Fatalf("rule %d round %d plan %d: NewMessages, NewSize, ExtraCopies, RepeatedCopies = %d, %d, %d, %d, want %d, %d, %d, %d",
						rule, round, k, res.plan.NewMessages, res.plan.NewSize, res.plan.ExtraCopies, res.plan.RepeatedCopies, len(res.plan.SrcUIDs), wantSize, wantExtra, wantRepeated)
				}
			}
			for _, id := range []string{"a@x", "b@x", "c@x"} {
				largest := len(jc.dstMap[id])
				for k := range jc.plans {
					all := jc.srcMaps[jc.slotOf[k]][id]
					largest = max(largest, len(rule.sourceCopies(all)))
				}
				if have := len(jc.dstMap[id]) + keptPerID[id]; have != largest {
					t.Fatalf("rule %d round %d: %s ends with %d copies, want %d (destination %v)", rule, round, id, have, largest, jc.dstMap[id])
				}
			}
		}
	}
}

func Test_destinationDiff_When_SourcesChangeAfterThePlan_Then_ResultNeverAddsAndNeverExceedsTheLargestFreshCount(t *testing.T) {
	t.Parallel()

	for _, rule := range []copyRule{copyByCount, copyOnePerMessageID} {
		rng := rand.New(rand.NewPCG(47, 53+uint64(rule)))
		for round := range 500 {
			jc := newJointCase(t, rng, rule)
			if len(jc.plans) == 0 {
				continue
			}
			fresh := make([]map[string][]client.MessageRef, len(jc.plans))
			for k := range jc.plans {
				fresh[k] = make(map[string][]client.MessageRef)
				old := jc.srcMaps[jc.slotOf[k]]
				next := uint32(100)
				for id, refs := range old {
					for _, ref := range refs {
						if rng.IntN(4) != 0 {
							fresh[k][id] = append(fresh[k][id], ref)
						}
					}
					for range rng.IntN(3) {
						fresh[k][id] = append(fresh[k][id], client.MessageRef{UID: next, Size: uint32(100 * (1 + rng.IntN(3)))})
						next++
					}
				}
			}

			d := newDestinationDiff(jc.plans, jc.dstMap)
			for k := range jc.plans {
				d.addSource(k, fresh[k])
			}
			got := d.results()

			keptPerID := make(map[string]int)
			for k, res := range got {
				listed := make(map[uint32]bool)
				idOf := make(map[uint32]string)
				for id, refs := range fresh[k] {
					for _, ref := range refs {
						listed[ref.UID] = true
						idOf[ref.UID] = id
					}
				}
				var wantGone int
				for _, uid := range jc.plans[k].SrcUIDs {
					if !listed[uid] {
						wantGone++
					}
				}
				for _, uid := range res.plan.SrcUIDs {
					if _, ok := slices.BinarySearch(jc.plans[k].SrcUIDs, uid); !ok {
						t.Fatalf("rule %d round %d plan %d: kept UID %d was not planned", rule, round, k, uid)
					}
					keptPerID[idOf[uid]]++
				}
				if res.drops.gone != wantGone || res.drops.held+res.drops.gone+len(res.plan.SrcUIDs) != len(jc.plans[k].SrcUIDs) {
					t.Fatalf("rule %d round %d plan %d: drops %+v with %d kept of %d planned, want gone %d and held + gone + kept = planned", rule, round, k, res.drops, len(res.plan.SrcUIDs), len(jc.plans[k].SrcUIDs), wantGone)
				}
			}
			for id, kept := range keptPerID {
				largest := len(jc.dstMap[id])
				for k := range jc.plans {
					largest = max(largest, len(rule.sourceCopies(fresh[k][id])))
				}
				if have := len(jc.dstMap[id]) + kept; have > largest {
					t.Fatalf("rule %d round %d: %s ends with %d copies, want at most %d", rule, round, id, have, largest)
				}
			}
		}
	}
}

func msgRefs(uids ...uint32) []client.MessageRef {
	refs := make([]client.MessageRef, len(uids))
	for i, uid := range uids {
		refs[i] = client.MessageRef{UID: uid, Size: 100}
	}
	return refs
}

func refsOfSizes(sizes ...uint32) []client.MessageRef {
	refs := make([]client.MessageRef, len(sizes))
	for i, size := range sizes {
		refs[i] = client.MessageRef{UID: uint32(i + 1), Size: size}
	}
	return refs
}

// referenceOwners is the quadratic oracle of copyMatcher.match: same-size copies take the lowest free ref, then the other copies do. The refs ascend by UID.
func referenceOwners(refs []client.MessageRef, sizes []uint32) []int32 {
	owner := make([]int32, len(refs))
	for i := range owner {
		owner[i] = -1
	}
	matched := make([]bool, len(sizes))
	for c, size := range sizes {
		for i, ref := range refs {
			if owner[i] < 0 && ref.Size == size {
				owner[i], matched[c] = int32(c), true
				break
			}
		}
	}
	for c := range sizes {
		if matched[c] {
			continue
		}
		for i := range refs {
			if owner[i] < 0 {
				owner[i] = int32(c)
				break
			}
		}
	}
	return owner
}

func Test_copyMatcher_match(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		refs  []client.MessageRef
		sizes []uint32
		want  []int32
	}{
		{
			name: "the last ref has the size of the only copy",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}, sizes: []uint32{30},
			want: []int32{-1, -1, 0},
		},
		{
			name: "two copies match the first and the last ref",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}, sizes: []uint32{10, 30},
			want: []int32{0, -1, 1},
		},
		{
			name: "a copy that no size matches takes the lowest UID",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}, sizes: []uint32{30, 99},
			want: []int32{1, -1, 0},
		},
		{
			name: "no size matches, so the first refs are taken",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}, sizes: []uint32{7, 8},
			want: []int32{0, 1, -1},
		},
		{
			name: "one copy of the size that every ref has",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 10}, {UID: 6, Size: 10}}, sizes: []uint32{10},
			want: []int32{0, -1, -1},
		},
		{
			name: "two copies of the size that every ref has",
			refs: []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 10}, {UID: 6, Size: 10}}, sizes: []uint32{10, 10},
			want: []int32{0, 1, -1},
		},
		{
			name: "the middle copy matches and the others fall back in copy order",
			refs: []client.MessageRef{{UID: 1, Size: 5}, {UID: 2, Size: 6}, {UID: 3, Size: 7}}, sizes: []uint32{9, 6, 8},
			want: []int32{0, 1, 2},
		},
		{
			name: "more copies than refs",
			refs: []client.MessageRef{{UID: 1, Size: 100}, {UID: 2, Size: 200}}, sizes: []uint32{200, 100, 100},
			want: []int32{1, 0},
		},
		{
			name: "the pairing follows the size",
			refs: []client.MessageRef{{UID: 1, Size: 200}, {UID: 2, Size: 100}}, sizes: []uint32{100, 200},
			want: []int32{1, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var m copyMatcher
			if got := m.match(tt.refs, tt.sizes); !slices.Equal(got, tt.want) {
				t.Errorf("match(%v, %v) = %v, want %v", tt.refs, tt.sizes, got, tt.want)
			}
		})
	}

	// One matcher serves every row in turn, so a buffer that the previous call left behind must not leak into the next result.
	var m copyMatcher
	for _, tt := range tests {
		if got := m.match(tt.refs, tt.sizes); !slices.Equal(got, tt.want) {
			t.Errorf("%s: reused match(%v, %v) = %v, want %v", tt.name, tt.refs, tt.sizes, got, tt.want)
		}
	}
}

func Test_copyMatcher_When_InputsAreRandom_Then_MatchesTheReference(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(21, 33))
	var m copyMatcher
	for round := range 3000 {
		refs := make([]client.MessageRef, 1+rng.IntN(8))
		var uid uint32
		for i := range refs {
			uid += uint32(1 + rng.IntN(3))
			refs[i] = client.MessageRef{UID: uid, Size: uint32(1 + rng.IntN(3))}
		}
		sizes := make([]uint32, 1+rng.IntN(8))
		held := make([]reservation, len(sizes))
		for i := range sizes {
			sizes[i] = uint32(1 + rng.IntN(3))
			held[i] = reservation{uid: uint32(i), size: sizes[i]}
		}
		want := referenceOwners(refs, sizes)

		got := slices.Clone(m.match(refs, sizes))

		if !slices.Equal(got, want) {
			t.Fatalf("round %d: match(%v, %v) = %v, want %v", round, refs, sizes, got, want)
		}
		owned := make(map[int32]bool, len(sizes))
		for _, c := range got {
			if c < 0 {
				continue
			}
			if owned[c] {
				t.Fatalf("round %d: copy %d owns two refs in %v", round, c, got)
			}
			owned[c] = true
		}
		if len(owned) != min(len(refs), len(sizes)) {
			t.Fatalf("round %d: %d refs are owned, want min(%d, %d)", round, len(owned), len(refs), len(sizes))
		}
		if owners := m.reservationOwners(refs, held); !slices.Equal(owners, want) {
			t.Fatalf("round %d: reservationOwners(%v, %v) = %v, want %v", round, refs, held, owners, want)
		}
		var wantLeft []client.MessageRef
		for i, c := range want {
			if c < 0 {
				wantLeft = append(wantLeft, refs[i])
			}
		}
		dst := refsOfSizes(sizes...)
		if left := m.destinationLeft(refs, dst); !slices.Equal(left, wantLeft) {
			t.Fatalf("round %d: destinationLeft(%v, %v) = %v, want %v", round, refs, dst, left, wantLeft)
		}
	}
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func Test_copyMatcher_When_GroupIsUncoveredOrFullyCovered_Then_AllocatesNothing(t *testing.T) {
	refs := []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}
	tests := []struct {
		name string
		dst  []client.MessageRef
	}{
		{name: "the destination holds no copy"},
		{name: "the destination holds as many copies as the source", dst: refsOfSizes(30, 20, 10)},
		{name: "the destination holds more copies than the source", dst: refsOfSizes(30, 20, 10, 10)},
		{name: "the destination holds some of the copies", dst: refsOfSizes(30, 99)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m copyMatcher
			m.destinationLeft(refs, tt.dst)

			if n := testing.AllocsPerRun(100, func() { m.destinationLeft(refs, tt.dst) }); n != 0 {
				t.Errorf("destinationLeft allocates %v times per call, want 0", n)
			}
		})
	}
}

// rediffOnePlan runs destinationDiff on a destination that has one plan.
func rediffOnePlan(plan FolderSyncPlan, srcMap, dstMap map[string][]client.MessageRef) (FolderSyncPlan, recheckDrops) {
	d := newDestinationDiff([]FolderSyncPlan{plan}, dstMap)
	d.addSource(0, srcMap)
	got := d.results()[0]
	return got.plan, got.drops
}

func Test_destinationDiff(t *testing.T) {
	t.Parallel()

	// The planned value is not zero, so each row shows that the recheck computes ExtraCopies again.
	const staleExtraCopies = 7

	planned := FolderSyncPlan{
		SourceFolder:      "Src",
		DestinationFolder: "Dst",
		SrcUIDs:           []uint32{1, 2, 3},
		NewMessages:       3,
		NewSize:           300,
		ExtraCopies:       staleExtraCopies,
	}
	allIDs := map[string][]client.MessageRef{"a@x": msgRefs(1), "b@x": msgRefs(2), "c@x": msgRefs(3)}
	tests := []struct {
		srcMap    map[string][]client.MessageRef
		dstMap    map[string][]client.MessageRef
		name      string
		wantUIDs  []uint32
		plan      FolderSyncPlan
		wantSize  uint64
		wantHeld  int
		wantGone  int
		wantExtra int
	}{
		{
			name:     "destination holds some IDs",
			plan:     planned,
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{"a@x": msgRefs(51), "c@x": msgRefs(52)},
			wantUIDs: []uint32{2},
			wantSize: 100,
			wantHeld: 2,
		},
		{
			name:     "destination holds all IDs",
			plan:     planned,
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{"a@x": msgRefs(51), "b@x": msgRefs(52), "c@x": msgRefs(53)},
			wantUIDs: nil,
			wantSize: 0,
			wantHeld: 3,
		},
		{
			name:     "empty destination keeps the plan",
			plan:     planned,
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{},
			wantUIDs: []uint32{1, 2, 3},
			wantSize: 300,
		},
		{
			name:     "fresh source UID outside the plan is never added",
			plan:     FolderSyncPlan{SourceFolder: "Src", DestinationFolder: "Dst", SrcUIDs: []uint32{1, 2}, NewMessages: 2, NewSize: 200, ExtraCopies: staleExtraCopies},
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{},
			wantUIDs: []uint32{1, 2},
			wantSize: 200,
		},
		{
			name:     "planned UID gone from the fresh source is dropped",
			plan:     planned,
			srcMap:   map[string][]client.MessageRef{"a@x": msgRefs(1), "c@x": msgRefs(3)},
			dstMap:   map[string][]client.MessageRef{},
			wantUIDs: []uint32{1, 3},
			wantSize: 200,
			wantGone: 1,
		},
		{
			name:     "destination holds one planned ID and the source lost another",
			plan:     planned,
			srcMap:   map[string][]client.MessageRef{"a@x": msgRefs(1), "c@x": msgRefs(3)},
			dstMap:   map[string][]client.MessageRef{"a@x": msgRefs(51)},
			wantUIDs: []uint32{3},
			wantSize: 100,
			wantHeld: 1,
			wantGone: 1,
		},
		{
			name:     "size follows the kept messages",
			plan:     planned,
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{"c@x": msgRefs(51)},
			wantUIDs: []uint32{1, 2},
			wantSize: 200,
			wantHeld: 1,
		},
		{
			name:     "plan without UIDs has no size",
			plan:     FolderSyncPlan{SourceFolder: "Src", DestinationFolder: "Dst", ExtraCopies: staleExtraCopies},
			srcMap:   allIDs,
			dstMap:   map[string][]client.MessageRef{},
			wantUIDs: nil,
			wantSize: 0,
		},
		{
			name:      "destination now holds one of three messages with one ID",
			plan:      planned,
			srcMap:    map[string][]client.MessageRef{"s@x": msgRefs(1, 2, 3)},
			dstMap:    map[string][]client.MessageRef{"s@x": msgRefs(51)},
			wantUIDs:  []uint32{2, 3},
			wantSize:  200,
			wantHeld:  1,
			wantExtra: 2,
		},
		{
			name:     "destination now holds three messages with one ID",
			plan:     planned,
			srcMap:   map[string][]client.MessageRef{"s@x": msgRefs(1, 2, 3)},
			dstMap:   map[string][]client.MessageRef{"s@x": msgRefs(51, 52, 53)},
			wantUIDs: nil,
			wantSize: 0,
			wantHeld: 3,
		},
		{
			name:      "source lost the first message of an ID that the destination holds once",
			plan:      planned,
			srcMap:    map[string][]client.MessageRef{"s@x": msgRefs(2, 3)},
			dstMap:    map[string][]client.MessageRef{"s@x": msgRefs(51)},
			wantUIDs:  []uint32{3},
			wantSize:  100,
			wantHeld:  1,
			wantGone:  1,
			wantExtra: 1,
		},
		{
			name: "size sums the fresh sizes of the kept messages",
			plan: planned,
			srcMap: map[string][]client.MessageRef{
				"a@x": {{UID: 1, Size: 10}},
				"b@x": {{UID: 2, Size: 20}, {UID: 3, Size: 40}},
			},
			dstMap:    map[string][]client.MessageRef{"b@x": msgRefs(51)},
			wantUIDs:  []uint32{1, 3},
			wantSize:  50,
			wantHeld:  1,
			wantExtra: 1,
		},
		{
			name:      "fresh source UID of an ID that the destination holds is outside the plan and never counts",
			plan:      FolderSyncPlan{SourceFolder: "Src", DestinationFolder: "Dst", SrcUIDs: []uint32{1, 2}, NewMessages: 2, NewSize: 200, ExtraCopies: staleExtraCopies},
			srcMap:    map[string][]client.MessageRef{"s@x": msgRefs(1, 2, 3)},
			dstMap:    map[string][]client.MessageRef{"s@x": msgRefs(51)},
			wantUIDs:  []uint32{2},
			wantSize:  100,
			wantHeld:  1,
			wantExtra: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := slices.Clone(tt.plan.SrcUIDs)

			got, drops := rediffOnePlan(tt.plan, tt.srcMap, tt.dstMap)

			if !slices.Equal(got.SrcUIDs, tt.wantUIDs) {
				t.Errorf("SrcUIDs = %v, want %v", got.SrcUIDs, tt.wantUIDs)
			}
			if got.NewMessages != len(tt.wantUIDs) {
				t.Errorf("NewMessages = %d, want %d", got.NewMessages, len(tt.wantUIDs))
			}
			if got.NewSize != tt.wantSize {
				t.Errorf("NewSize = %d, want %d", got.NewSize, tt.wantSize)
			}
			if got.ExtraCopies != tt.wantExtra {
				t.Errorf("ExtraCopies = %d, want %d", got.ExtraCopies, tt.wantExtra)
			}
			if drops.held != tt.wantHeld || drops.gone != tt.wantGone {
				t.Errorf("drops = held %d, gone %d, want held %d, gone %d", drops.held, drops.gone, tt.wantHeld, tt.wantGone)
			}
			if total := drops.held + drops.gone + len(got.SrcUIDs); total != len(tt.plan.SrcUIDs) {
				t.Errorf("held %d + gone %d + kept %d = %d, want %d planned UIDs", drops.held, drops.gone, len(got.SrcUIDs), total, len(tt.plan.SrcUIDs))
			}
			if !got.DestinationFolderExists {
				t.Error("DestinationFolderExists = false, want true")
			}
			if got.SourceFolder != tt.plan.SourceFolder || got.DestinationFolder != tt.plan.DestinationFolder {
				t.Errorf("folders = %q → %q, want %q → %q", got.SourceFolder, got.DestinationFolder, tt.plan.SourceFolder, tt.plan.DestinationFolder)
			}
			if !slices.Equal(tt.plan.SrcUIDs, before) {
				t.Errorf("input SrcUIDs = %v, want %v: the plan slice must not change", tt.plan.SrcUIDs, before)
			}
		})
	}
}

func Test_destinationDiff_When_ScanListsOneUIDUnderTwoMessageIds_Then_EachPlannedUIDCountsOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dstMap    map[string][]client.MessageRef
		wantUIDs  []uint32
		wantDrops recheckDrops
	}{
		{
			name:      "destination holds only one of the two Message-Ids",
			dstMap:    map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 10}}},
			wantUIDs:  []uint32{5},
			wantDrops: recheckDrops{},
		},
		{
			name: "destination holds both Message-Ids",
			dstMap: map[string][]client.MessageRef{
				"a@x": {{UID: 1, Size: 10}},
				"b@x": {{UID: 2, Size: 10}},
			},
			wantUIDs:  nil,
			wantDrops: recheckDrops{held: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan := FolderSyncPlan{SourceFolder: "A", DestinationFolder: "D", SrcUIDs: []uint32{5}, NewMessages: 1, NewSize: 10}
			srcMap := map[string][]client.MessageRef{
				"a@x": {{UID: 5, Size: 10}},
				"b@x": {{UID: 5, Size: 10}},
			}

			got, drops := rediffOnePlan(plan, srcMap, tt.dstMap)

			if !slices.Equal(got.SrcUIDs, tt.wantUIDs) {
				t.Errorf("SrcUIDs = %v, want %v", got.SrcUIDs, tt.wantUIDs)
			}
			if drops != tt.wantDrops {
				t.Errorf("drops = %+v, want %+v", drops, tt.wantDrops)
			}
			if total := drops.held + drops.gone + len(got.SrcUIDs); total != len(plan.SrcUIDs) {
				t.Errorf("held %d + gone %d + kept %d = %d, want %d planned UIDs", drops.held, drops.gone, len(got.SrcUIDs), total, len(plan.SrcUIDs))
			}
		})
	}
}

const notModifiedUTF7Reason = "the server sent a name that is not valid modified UTF-7"

// leftOutLine is the warning runSync and runShow print for a LIST name that is not valid modified UTF-7. shown is the name as the terminal shows it.
func leftOutLine(label, shown string) string {
	return "⚠️  [" + label + "] Folder " + shown + " is left out: " + notModifiedUTF7Reason + "\n"
}

func TestRunSync_When_UnmappedFolderNameIsNotModifiedUTF7_Then_RestSyncsAndWarns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		folder string
		shown  string
	}{
		{name: "raw UTF-8 name", folder: "Входящие", shown: "Входящие"},
		{name: "bare ampersand", folder: "Q&A", shown: "Q&A"},
		{name: "control and bidi characters", folder: "\x1b[2J\u202eevil", shown: "\\x1b[2J\\u202eevil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX":   fakeMsgs("m1@x"),
				tt.folder: fakeMsgs("m2@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

			if err != nil {
				t.Fatalf("runSync: %v", err)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			got := out.String()
			assertContainsAll(t, got, leftOutLine("src", tt.shown))
			if n := strings.Count(got, "is left out"); n != 1 {
				t.Errorf("output holds %d left-out lines, want 1:\n%s", n, got)
			}
			assertContainsNone(t, got, "\x1b[2J", "\u202e", "[dst] Folder")
			for _, verb := range []string{"EXAMINE", "STATUS"} {
				for _, name := range src.capturedNames(verb) {
					if name != "INBOX" {
						t.Errorf("source got %s for %q, want only INBOX", verb, name)
					}
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_MirrorSourceListsInvalidName_Then_OtherFoldersCopyAndWarningShowsWithQuiet(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Work":  fakeMsgs("w1@x"),
		"Q&A":   fakeMsgs("q1@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "w1@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work")
	assertContainsAll(t, out.String(), leftOutLine("src", "Q&A"))
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_MappingNamesInvalidFolder_Then_FolderFailsAndRestCopies(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX": fakeMsgs("m1@x"),
		"Q&A":   fakeMsgs("q1@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Q&A", "Q&A"), mapping("INBOX", "INBOX")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	got := out.String()
	assertContainsAll(t, got,
		leftOutLine("src", "Q&A"),
		folderFailuresHeader,
		"• Q&A → Q&A: cannot scan source folder:",
		"1 folders failed",
	)
	if dst.hasFolder("Q&A") || len(dst.capturedNames("CREATE")) != 0 {
		t.Errorf("dst holds Q&A=%v CREATE names=%q, want none", dst.hasFolder("Q&A"), dst.capturedNames("CREATE"))
	}
	assertIDs(t, "dst APPENDs to Q&A", dst.appended("Q&A"))
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationListsInvalidName_Then_SyncRunsAndWarnsWithDstLabel(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Входящие": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	got := out.String()
	assertContainsAll(t, got, leftOutLine("dst", "Входящие"))
	assertContainsNone(t, got, "[src] Folder")
}

func TestRunSync_When_BothSidesListInvalidNames_Then_SourceWarningsComeFirstInNameOrder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Q&A": nil, "AT&T": nil})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Входящие": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertInOrder(t, out.String(),
		leftOutLine("src", "AT&T"),
		leftOutLine("src", "Q&A"),
		leftOutLine("dst", "Входящие"),
		"Messages to be copied to destination",
	)
}

func TestRunSync_When_SourceListFailsAtConnect_Then_ErrorNamesTheLabel(t *testing.T) {
	t.Parallel()

	src := newFakeServer(t)
	src.addConnHandler(scriptedHandler(src, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "LIST" {
			return "NO [CANNOT] list refused", true
		}
		return "", false
	}))
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if err == nil || !strings.Contains(err.Error(), "[src] list mailboxes") {
		t.Fatalf("error = %v, want it to contain %q", err, "[src] list mailboxes")
	}
	if strings.Contains(err.Error(), "[]") {
		t.Errorf("error = %q, want no empty label", err)
	}
}

func TestRunSync_When_SourceChangesAfterTheCreateReply_Then_OnlyPlannedMessagesAreCopied(t *testing.T) {
	t.Parallel()

	const (
		heldLine = "  ℹ️  Destination folder Work already exists and holds 2 of the 3 planned messages from Work, so they are not copied\n"
		goneLine = "  ℹ️  Source folder Work no longer holds 1 of the 3 planned messages for Work, so they are not copied\n"
	)
	tests := []struct {
		change       func(src *fakeServer)
		name         string
		input        string
		wantDst      []string
		wantAppended []string
		wantNext     []string
		wantLines    []string
		noLines      []string
		autoConfirm  bool
	}{
		{
			name:         "unattended run with new mail",
			autoConfirm:  true,
			change:       func(src *fakeServer) { src.addMessage("Work", "m4@x") },
			wantDst:      []string{"m1@x", "m2@x", "m3@x"},
			wantAppended: []string{"m3@x"},
			wantNext:     []string{"m3@x", "m4@x"},
			wantLines:    []string{heldLine},
			noLines:      []string{"no longer holds"},
		},
		{
			name:         "interactive run with new mail",
			input:        "y\n",
			change:       func(src *fakeServer) { src.addMessage("Work", "m4@x") },
			wantDst:      []string{"m1@x", "m2@x", "m3@x"},
			wantAppended: []string{"m3@x"},
			wantNext:     []string{"m3@x", "m4@x"},
			wantLines:    []string{heldLine},
			noLines:      []string{"no longer holds"},
		},
		{
			name:         "planned message disappears",
			autoConfirm:  true,
			change:       func(src *fakeServer) { src.removeMessage("Work", "m3@x") },
			wantDst:      []string{"m1@x", "m2@x"},
			wantAppended: nil,
			wantNext:     nil,
			wantLines:    []string{heldLine, goneLine},
			noLines:      []string{"holds 3 of the 3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
			dst.hideFromList("Work")
			var once sync.Once
			dst.onCommand("CREATE", func() { once.Do(func() { tt.change(src) }) })
			cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.autoConfirm = tt.autoConfirm

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), tt.wantDst...)
			assertIDs(t, "dst APPENDs to Work", dst.appended("Work"), tt.wantAppended...)
			assertContainsNone(t, out.String(), folderFailuresHeader)
			assertContainsAll(t, out.String(), tt.wantLines...)
			assertContainsNone(t, out.String(), tt.noLines...)
			if !tt.autoConfirm {
				if n := strings.Count(out.String(), "Proceed with synchronization?"); n != 1 {
					t.Errorf("output asks to proceed %d times, want 1: the recheck must not ask again\noutput: %q", n, out.String())
				}
			}
			if len(tt.wantAppended) == 0 {
				assertContainsAll(t, out.String(), "All folders already synced!")
				if n := dst.callCount("LOGIN"); n != 1 {
					t.Errorf("destination saw %d LOGIN commands, want 1: no worker may connect", n)
				}
			}

			next := e2eOptions(&syncBuffer{}, "")
			next.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, next); err != nil {
				t.Fatalf("second runSync: %v", err)
			}
			assertIDs(t, "dst APPENDs to Work after the second run", dst.appended("Work"), tt.wantNext...)
		})
	}
}

// hookReader runs hook once, on the first Read. With matching delimiters, the first read of runSync is the confirmation prompt, so the hook runs after the preview.
type hookReader struct {
	r    io.Reader
	hook func()
	once sync.Once
}

func (h *hookReader) Read(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.r.Read(p)
}

func TestRunSync_When_SourceUIDValidityChangesBeforeCopy_Then_PlanFailsAndNothingIsAppended(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x", "c@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x")})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.rebuildFolder("INBOX", 2, 2, 0, 1) }}

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Errorf("runSync error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "a@x", "b@x")
	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"))
	if n := src.callCount("UID FETCH"); n != 0 {
		t.Errorf("source saw %d UID FETCH commands, want 0", n)
	}
	assertContainsAll(t, out.String(),
		"source folder changed after the sync plan, run sync again", "UIDVALIDITY 1, now 2", "Sync completed with errors")
	assertContainsNone(t, out.String(), "Sync completed successfully")
	assertReadOnly(t, "source", src)

	next := e2eOptions(&syncBuffer{}, "")
	next.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, next); err != nil {
		t.Fatalf("second runSync: %v", err)
	}
	assertIDs(t, "dst INBOX after the second run", dst.folderMsgIDs("INBOX"), "a@x", "b@x", "c@x")
	assertIDs(t, "dst APPENDs to INBOX after the second run", dst.appended("INBOX"), "c@x")
}

func TestRunSync_When_SourceStopsReportingUIDValidityBeforeCopy_Then_PlanFailsAndNothingIsAppended(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x", "c@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x")})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.setUIDValidity("INBOX", 0) }}

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Errorf("runSync error = %v, want ErrSilentExit", err)
	}
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "a@x", "b@x")
	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"))
	if n := src.callCount("UID FETCH"); n != 0 {
		t.Errorf("source saw %d UID FETCH commands, want 0", n)
	}
	assertContainsAll(t, out.String(),
		"source folder changed after the sync plan, run sync again", "UIDVALIDITY 1, now none", "Sync completed with errors")
	assertContainsNone(t, out.String(), "Sync completed successfully")
	assertReadOnly(t, "source", src)

	next := e2eOptions(&syncBuffer{}, "")
	next.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, next); err != nil {
		t.Fatalf("second runSync: %v", err)
	}
	assertIDs(t, "dst INBOX after the second run", dst.folderMsgIDs("INBOX"), "a@x", "b@x", "c@x")
	assertIDs(t, "dst APPENDs to INBOX after the second run", dst.appended("INBOX"), "c@x")
}

func TestRunSync_When_SourceUIDValidityChangesBeforeRecheck_Then_FolderFailsBeforeCopy(t *testing.T) {
	t.Parallel()

	const failureLine = "• Work → Work: source folder changed after the sync plan, run sync again (UIDVALIDITY 1, now 2)"
	tests := []struct {
		name        string
		mappings    []config.DirectoryMapping
		wantExamine []string
		wantStatus  []string
	}{
		{
			name:        "recheck keeps the planning selection",
			mappings:    []config.DirectoryMapping{mapping("Work", "Work")},
			wantExamine: []string{"Work"},
			wantStatus:  []string{"Work"},
		},
		{
			name:        "recheck selects the folder again",
			mappings:    []config.DirectoryMapping{mapping("Work", "Work"), mapping("Other", "Other")},
			wantExamine: []string{"Work", "Other", "Work", "Other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x"), "Other": fakeMsgs("o1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
			dst.hideFromList("Work")
			var once sync.Once
			// The new UIDs start above the planned ones, so no planned UID can match the missing message by chance.
			dst.onCommand("CREATE", func() { once.Do(func() { src.rebuildFolderFrom("Work", 2, 10, 2, 0, 1) }) })
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Errorf("runSync error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, out.String(), failureLine, "1 folders failed")
			assertContainsNone(t, out.String(), "already exists and holds", "no longer holds", "Sync completed successfully", "All folders already synced!")
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x", "m2@x")
			assertIDs(t, "dst APPENDs to Work", dst.appended("Work"))
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), tt.wantExamine...)
			assertIDs(t, "source STATUS names", src.capturedNames("STATUS"), tt.wantStatus...)
			if len(tt.mappings) == 1 {
				if n := src.callCount("LOGIN"); n != 1 {
					t.Errorf("source saw %d LOGIN commands, want 1: no worker may connect", n)
				}
			} else {
				assertIDs(t, "dst APPENDs to Other", dst.appended("Other"), "o1@x")
			}

			next := e2eOptions(&syncBuffer{}, "")
			next.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, next); err != nil {
				t.Fatalf("second runSync: %v", err)
			}
			assertIDs(t, "dst APPENDs to Work after the second run", dst.appended("Work"), "m3@x")
		})
	}
}

func TestRunSync_When_SourceStopsReportingUIDValidityBeforeRecheck_Then_FolderFailsBeforeCopy(t *testing.T) {
	t.Parallel()

	const failureLine = "• Work → Work: source folder changed after the sync plan, run sync again (UIDVALIDITY 1, now none)"
	tests := []struct {
		name        string
		mappings    []config.DirectoryMapping
		wantExamine []string
		wantStatus  []string
	}{
		{
			name:        "recheck keeps the planning selection",
			mappings:    []config.DirectoryMapping{mapping("Work", "Work")},
			wantExamine: []string{"Work"},
			wantStatus:  []string{"Work"},
		},
		{
			name:        "recheck selects the folder again",
			mappings:    []config.DirectoryMapping{mapping("Work", "Work"), mapping("Other", "Other")},
			wantExamine: []string{"Work", "Other", "Work", "Other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x"), "Other": fakeMsgs("o1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
			dst.hideFromList("Work")
			var once sync.Once
			dst.onCommand("CREATE", func() { once.Do(func() { src.setUIDValidity("Work", 0) }) })
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Errorf("runSync error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, out.String(), failureLine, "1 folders failed")
			assertContainsNone(t, out.String(), "already exists and holds", "no longer holds", "Sync completed successfully", "All folders already synced!")
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "m1@x", "m2@x")
			assertIDs(t, "dst APPENDs to Work", dst.appended("Work"))
			assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), tt.wantExamine...)
			assertIDs(t, "source STATUS names", src.capturedNames("STATUS"), tt.wantStatus...)
			if len(tt.mappings) == 1 {
				if n := src.callCount("LOGIN"); n != 1 {
					t.Errorf("source saw %d LOGIN commands, want 1: no worker may connect", n)
				}
			} else {
				assertIDs(t, "dst APPENDs to Other", dst.appended("Other"), "o1@x")
			}

			next := e2eOptions(&syncBuffer{}, "")
			next.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, next); err != nil {
				t.Fatalf("second runSync: %v", err)
			}
			assertIDs(t, "dst APPENDs to Work after the second run", dst.appended("Work"), "m3@x")
		})
	}
}

func Test_buildSyncPlan_When_SourceFoldersHaveOwnUIDValidity_Then_EachPlanCarriesIt(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("w1@x"), "Other": fakeMsgs("o1@x")})
	src.setUIDValidity("Work", 11)
	src.setUIDValidity("Other", 22)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Sent": nil})
	srcTr, dstTr := makePlanTrackers()
	mappings := []config.DirectoryMapping{mapping("Work", "Sent"), mapping("Other", "Sent")}

	summary, err := buildSyncPlan(t.Context(), newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), mappings, srcTr, dstTr, "src", "dst")

	if err != nil {
		t.Fatalf("buildSyncPlan: %v", err)
	}
	got := make(map[string]uint32, len(summary.Plans))
	for _, plan := range summary.Plans {
		got[plan.SourceFolder] = plan.SrcUIDValidity
	}
	if want := map[string]uint32{"Work": 11, "Other": 22}; !maps.Equal(got, want) {
		t.Errorf("plan UIDVALIDITY by source folder = %v, want %v", got, want)
	}
}

func TestRunSync_When_SourceSendsNoUIDValidity_Then_SyncCopiesNormally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		srcFolders map[string][]fakeMsg
		dstFolders map[string][]fakeMsg
		name       string
		folder     string
		wantDst    []string
		wantNew    []string
		hidden     bool
	}{
		{
			name:       "destination folder is listed",
			srcFolders: map[string][]fakeMsg{"INBOX": fakeMsgs("a@x", "b@x", "c@x")},
			dstFolders: map[string][]fakeMsg{"INBOX": fakeMsgs("a@x")},
			folder:     "INBOX",
			wantDst:    []string{"a@x", "b@x", "c@x"},
			wantNew:    []string{"b@x", "c@x"},
		},
		{
			name:       "destination folder is rechecked after CREATE",
			srcFolders: map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")},
			dstFolders: map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")},
			folder:     "Work",
			hidden:     true,
			wantDst:    []string{"m1@x", "m2@x", "m3@x"},
			wantNew:    []string{"m3@x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.srcFolders)
			src.setUIDValidity(tt.folder, 0)
			dst := newStoreServer(t, "/", tt.dstFolders)
			if tt.hidden {
				dst.hideFromList(tt.folder)
			}
			cfg := e2eConfig(src, dst, 1, mapping(tt.folder, tt.folder))

			out := runSyncOnce(t, cfg)

			assertIDs(t, "dst "+tt.folder, dst.folderMsgIDs(tt.folder), tt.wantDst...)
			assertIDs(t, "dst APPENDs to "+tt.folder, dst.appended(tt.folder), tt.wantNew...)
			assertContainsNone(t, out, "changed after the sync plan")
		})
	}
}

func TestRunSync_When_ExistingDestinationNamesCarryControlCharacters_Then_InfoLineShowsEscapesAndServersGetExactNames(t *testing.T) {
	t.Parallel()

	const (
		srcName  = "S\x1b]0;p\xe2\x80\xae\xe3\x85\xa4"
		dstName  = "D\x1b]0;p\xe2\x80\xae\xe3\x85\xa4"
		srcWire  = "S&ABs-]0;p&IC4xZA-"
		dstWire  = "D&ABs-]0;p&IC4xZA-"
		srcShown = `S\x1b]0;p\u202e\u3164`
		dstShown = `D\x1b]0;p\u202e\u3164`
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{srcWire: fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, dstWire: fakeMsgs("m1@x", "m2@x")})
	dst.hideFromList(dstWire)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(srcName, dstName)), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got,
		"  ℹ️  Destination folder "+dstShown+" already exists and holds 2 of the 3 planned messages from "+srcShown+", so they are not copied\n")
	assertContainsNone(t, got, "\x1b]0;", "\xe2\x80\xae", "\xe3\x85\xa4")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), dstWire)
	assertIDs(t, "dst APPENDs", dst.appended(dstWire), "m3@x")
	if examined := src.capturedNames("EXAMINE"); !slices.Contains(examined, srcWire) {
		t.Errorf("source EXAMINE names = %q, want the exact wire name %q among them", examined, srcWire)
	}
	if examined := dst.capturedNames("EXAMINE"); !slices.Contains(examined, dstWire) {
		t.Errorf("destination EXAMINE names = %q, want the exact wire name %q among them", examined, dstWire)
	}
	for _, name := range append(src.capturedNames("EXAMINE"), dst.capturedNames("EXAMINE")...) {
		if name != srcWire && name != dstWire {
			t.Errorf("EXAMINE of %q, want only %q or %q", name, srcWire, dstWire)
		}
	}
}

func TestRunSync_When_TypedSourceNamesInboxChildInAnotherCaseWithWrongDelimiter_Then_RewriteTargetsTheListedFolder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		viaFlags bool
	}{
		{name: "config map"},
		{name: "folder flags", viaFlags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "INBOX/Work": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("inbox.Work", "X"))
			srcFlag, dstFlag := "", ""
			if tt.viaFlags {
				cfg.Map = nil
				srcFlag, dstFlag = "inbox.Work", "X"
			}

			first := runSyncConfirmed(t, cfg, srcFlag, dstFlag)

			assertContainsAll(t, first,
				"Folder path delimiter mismatch detected",
				`Source folder "inbox.Work" uses delimiter ".", server expects "/"`,
				`Fixed source: "inbox.Work" → "inbox/Work"`,
			)
			assertIDs(t, "dst X", dst.folderMsgIDs("X"), "m1@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "X")
			examined := src.capturedNames("EXAMINE")
			if !slices.Contains(examined, "INBOX/Work") {
				t.Errorf("source EXAMINE names = %q, want the canonical INBOX/Work among them", examined)
			}
			for _, name := range examined {
				if name != "INBOX/Work" {
					t.Errorf("source EXAMINE of %q, want only INBOX/Work", name)
				}
			}
			if !tt.viaFlags {
				if want := []config.DirectoryMapping{mapping("inbox.Work", "X")}; !slices.Equal(cfg.Map, want) {
					t.Errorf("cfg.Map = %+v, want %+v", cfg.Map, want)
				}
			}

			second := runSyncConfirmed(t, cfg, srcFlag, dstFlag)

			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("dst APPEND count = %d, want 1: the second run copies nothing", n)
			}
		})
	}
}

type delimiterCase struct {
	name      string
	delimiter string
}

var delimiterCases = []delimiterCase{
	{name: "slash", delimiter: "/"},
	{name: "dot", delimiter: "."},
	{name: "backslash", delimiter: `\`},
	{name: "underscore", delimiter: "_"},
	{name: "flat", delimiter: ""},
}

func pathsOver(alphabet []string, maxLen int) []string {
	all := []string{""}
	level := []string{""}
	for range maxLen {
		var next []string
		for _, prefix := range level {
			for _, element := range alphabet {
				next = append(next, prefix+element)
			}
		}
		all = append(all, next...)
		level = next
	}
	return all
}

func checkConvertedLevels(t *testing.T, path, srcDelimiter, dstDelimiter string) {
	t.Helper()
	got, renamed := convertFolderPath(path, srcDelimiter, dstDelimiter)
	wantLevels := 0
	if srcDelimiter != "" {
		wantLevels = strings.Count(path, srcDelimiter)
	}
	if n := strings.Count(got, dstDelimiter); n != wantLevels {
		t.Errorf("convertFolderPath(%q, %q, %q) = %q with %d levels, want %d", path, srcDelimiter, dstDelimiter, got, n, wantLevels)
	}
	if want := strings.Contains(path, dstDelimiter); renamed != want {
		t.Errorf("convertFolderPath(%q, %q, %q) renamed = %t, want %t", path, srcDelimiter, dstDelimiter, renamed, want)
	}
}

func Test_convertFolderPath_When_AnyDelimiterPair_Then_LevelsMatchTheSource(t *testing.T) {
	t.Parallel()

	paths := pathsOver([]string{"a", "/", ".", `\`, "_", "-"}, 4)
	for _, src := range delimiterCases {
		for _, dst := range delimiterCases {
			if dst.delimiter == "" || src.delimiter == dst.delimiter {
				continue
			}
			t.Run(src.name+" to "+dst.name, func(t *testing.T) {
				t.Parallel()
				for _, path := range paths {
					checkConvertedLevels(t, path, src.delimiter, dst.delimiter)
				}
			})
		}
	}
}

func FuzzConvertFolderPath(f *testing.F) {
	for _, seed := range []string{"", "a/b.c", `a\b.c_d`, "Mr. Smith", "\xff.\xfe/"} {
		for srcIndex, src := range delimiterCases {
			for dstIndex, dst := range delimiterCases {
				if dst.delimiter != "" && src.delimiter != dst.delimiter {
					f.Add(seed, uint8(srcIndex), uint8(dstIndex))
				}
			}
		}
	}
	f.Fuzz(func(t *testing.T, path string, srcIndex, dstIndex uint8) {
		srcDelimiter := delimiterCases[int(srcIndex)%len(delimiterCases)].delimiter
		dstDelimiter := delimiterCases[int(dstIndex)%len(delimiterCases)].delimiter
		if dstDelimiter == "" || srcDelimiter == dstDelimiter {
			t.Skip("the conversion applies only to two different delimiters")
		}
		checkConvertedLevels(t, path, srcDelimiter, dstDelimiter)
	})
}

func Test_expandMappingsWithSubfolders_When_MirrorRootIsExpanded_Then_EachSubfolderAgreesWithMirrorMappings(t *testing.T) {
	t.Parallel()

	segments := []string{"A", "a.b", "a_b", "a/b", "x-y"}
	sources := []delimiterCase{
		{name: "source slash", delimiter: "/"},
		{name: "source dot", delimiter: "."},
		{name: "source underscore", delimiter: "_"},
	}
	for _, source := range sources {
		srcDelimiter := source.delimiter
		t.Run(source.name, func(t *testing.T) {
			t.Parallel()

			var usable []string
			for _, segment := range segments {
				if !strings.Contains(segment, srcDelimiter) {
					usable = append(usable, segment)
				}
			}
			folders := make(map[string][]fakeMsg)
			for _, root := range usable {
				folders[root] = nil
				for _, child := range usable {
					folders[root+srcDelimiter+child] = nil
					for _, grandchild := range usable[:min(len(usable), 3)] {
						folders[root+srcDelimiter+child+srcDelimiter+grandchild] = nil
					}
				}
			}
			srcC := newAppClient(t, newStoreServer(t, srcDelimiter, folders), "src")
			names := srcC.MailboxNames()

			for _, destination := range delimiterCases {
				dstDelimiter := destination.delimiter
				dstC := newDestinationClient(t, dstDelimiter)
				all, _, err := expandMappingsWithSubfolders(e2eContext(t), srcC, dstC, mirrorMappings(names, srcDelimiter, dstDelimiter), srcDelimiter, dstDelimiter)
				if err != nil {
					t.Fatalf("expandMappingsWithSubfolders(all names, dst %q): %v", dstDelimiter, err)
				}
				if len(all) != len(names) {
					t.Errorf("mirror with dst %q plans %d folders, want one for each of the %d listed names", dstDelimiter, len(all), len(names))
				}
				for _, got := range all {
					if want := mirrorMappings([]string{got.Source}, srcDelimiter, dstDelimiter)[0]; got != want {
						t.Errorf("mirror with dst %q keeps %+v for %q, but mirroring it alone gives %+v", dstDelimiter, got, got.Source, want)
					}
				}
				for _, name := range names {
					roots := mirrorMappings([]string{name}, srcDelimiter, dstDelimiter)
					expanded, _, err := expandMappingsWithSubfolders(e2eContext(t), srcC, dstC, roots, srcDelimiter, dstDelimiter)
					if err != nil {
						t.Fatalf("expandMappingsWithSubfolders(%q, dst %q): %v", name, dstDelimiter, err)
					}
					for _, got := range expanded {
						if want := mirrorMappings([]string{got.Source}, srcDelimiter, dstDelimiter)[0]; got != want {
							t.Errorf("expanding %q with dst %q gives %+v, but mirroring %q gives %+v", name, dstDelimiter, got, got.Source, want)
						}
					}
				}
			}
		})
	}
}

func TestRunSync_When_FlatSourceHoldsNameWithSlash_Then_UnlistedParentIsAFailureNotASkip(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), "Archive/2024": fakeMsgs("a1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Archive", "Archive"), mapping("INBOX", "INBOX")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), folderFailuresHeader, "• Archive → Archive: cannot scan source folder", "1 folders failed")
	assertContainsNone(t, out.String(), "skipped")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
	if dst.hasFolder("Archive") || dst.hasFolder("Archive/2024") {
		t.Error("destination holds Archive or Archive/2024, want neither: a flat server has no hierarchy")
	}
	for _, name := range src.capturedNames("EXAMINE") {
		if name == "Archive/2024" {
			t.Errorf("source EXAMINE of %q, want it never scanned as a subfolder", name)
		}
	}
}

func TestRunSync_When_BothServersAreFlat_Then_TypedNamesWithDelimiterTextAreCopiedUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantFolders map[string][]string
		name        string
		srcFlag     string
		dstFlag     string
		mappings    []config.DirectoryMapping
	}{
		{
			name:        "config map with both folders",
			mappings:    []config.DirectoryMapping{mapping("Mr. Smith", "Mr. Smith"), mapping("Work/2023", "Work/2023")},
			wantFolders: map[string][]string{"Mr. Smith": {"a@x"}, "Work/2023": {"b@x", "c@x"}},
		},
		{
			name:        "folder flags with a dot and a space",
			srcFlag:     "Mr. Smith",
			dstFlag:     "Mr. Smith",
			wantFolders: map[string][]string{"Mr. Smith": {"a@x"}},
		},
		{
			name:        "folder flags with a slash",
			srcFlag:     "Work/2023",
			dstFlag:     "Work/2023",
			wantFolders: map[string][]string{"Work/2023": {"b@x", "c@x"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "", map[string][]fakeMsg{
				"INBOX":     nil,
				"Mr. Smith": fakeMsgs("a@x"),
				"Work/2023": fakeMsgs("b@x", "c@x"),
			})
			dst := newStoreServer(t, "", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)
			out := &syncBuffer{}
			// The only answer is for the "Proceed" prompt: a delimiter prompt would take it.
			opts := e2eOptions(out, "y\n")
			opts.srcFolder, opts.dstFolder = tt.srcFlag, tt.dstFlag

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertContainsNone(t, out.String(), "server expects", "delimiter mismatch", "Fix folder delimiters", "Fixed source", "Fixed destination")
			wantCreates := slices.Sorted(maps.Keys(tt.wantFolders))
			assertIDs(t, "dst CREATE names", slices.Sorted(slices.Values(dst.capturedNames("CREATE"))), wantCreates...)
			for folder, ids := range tt.wantFolders {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			for _, name := range src.capturedNames("EXAMINE") {
				if !slices.Contains(wantCreates, name) {
					t.Errorf("source EXAMINE of %q, want only %q", name, wantCreates)
				}
			}
		})
	}
}

func TestRunSync_When_DestinationListsNothing_Then_NestedTypedNameIsCreatedWithoutParent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src      map[string][]fakeMsg
		mappings []config.DirectoryMapping
	}{
		{
			name:     "typed mapping",
			src:      map[string][]fakeMsg{"Work/2023": fakeMsgs("w@x")},
			mappings: []config.DirectoryMapping{mapping("Work/2023", "Work/2023")},
		},
		{
			name: "mirror",
			src:  map[string][]fakeMsg{"INBOX": nil, "Work/2023": fakeMsgs("w@x")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "/", nil)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertContainsAll(t, out.String(), `Destination [dst]: ""`, "Sync completed successfully")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work/2023")
			assertIDs(t, "dst Work/2023", dst.folderMsgIDs("Work/2023"), "w@x")
			if dst.hasFolder("Work") {
				t.Error("destination holds a parent folder Work: a server that lists no folder gives no hierarchy")
			}
		})
	}
}

func TestRunSync_When_ListHoldsNameWithLatin1Byte_Then_RunSucceedsAndShowsTheEscapedName(t *testing.T) {
	t.Parallel()

	const (
		latin1 = "Entw\xfcrfe"
		shown  = `Entw\xfcrfe`
	)
	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "mirror"},
		{name: "typed mapping for another folder", mappings: []config.DirectoryMapping{mapping("INBOX", "INBOX")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x"), latin1: fakeMsgs("m2@x")})
			src.listAsLiteral(latin1)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			got := out.String()
			assertContainsAll(t, got, leftOutLine("src", shown))
			if strings.Contains(got, "\xfc") || !utf8.ValidString(got) {
				t.Errorf("output holds the raw byte or invalid UTF-8: %q", got)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			for _, verb := range []string{"EXAMINE", "STATUS"} {
				for _, name := range src.capturedNames(verb) {
					if name != "INBOX" {
						t.Errorf("source got %s for %q, want only INBOX", verb, name)
					}
				}
			}
		})
	}
}

func TestRunSync_When_MirrorListsNamesThatAreNotModifiedUTF7_Then_NoCommandCarriesTheirBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		srcBad   string
		dstBad   string
		srcShown string
		dstShown string
		literal  bool
	}{
		{name: "bare ampersand", srcBad: "Q&A", dstBad: "R&D", srcShown: "Q&A", dstShown: "R&D"},
		{name: "raw UTF-8", srcBad: "Входящие", dstBad: "Отправленные", srcShown: "Входящие", dstShown: "Отправленные"},
		{
			name:   "Latin-1 byte as a literal",
			srcBad: "Entw\xfcrfe", dstBad: "Entw\xfcrfe2", srcShown: `Entw\xfcrfe`, dstShown: `Entw\xfcrfe2`,
			literal: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX":   fakeMsgs("m1@x"),
				"Work":    fakeMsgs("w1@x"),
				tt.srcBad: fakeMsgs("b1@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, tt.dstBad: fakeMsgs("d1@x")})
			if tt.literal {
				src.listAsLiteral(tt.srcBad)
				dst.listAsLiteral(tt.dstBad)
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x")
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "w1@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Work")
			allowed := []string{"INBOX", "Work"}
			for side, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
				for _, verb := range []string{"EXAMINE", "STATUS", "SELECT", "CREATE"} {
					for _, name := range srv.capturedNames(verb) {
						if !slices.Contains(allowed, name) {
							t.Errorf("%s got %s for %q, want only %q", side, verb, name, allowed)
						}
					}
				}
			}
			assertContainsAll(t, out.String(), leftOutLine("src", tt.srcShown), leftOutLine("dst", tt.dstShown))
		})
	}
}

func Test_delimiterRewrite_When_ContextEndsBetweenLookups_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := newStoreServer(t, "/", map[string][]fakeMsg{"Archive/2024": nil})
	c := newAppClient(t, srv, "srv")
	base, end := context.WithCancel(t.Context())
	ctx := &endsAfterFirstErr{Context: base, end: end}

	rewritten, used, err := delimiterRewrite(ctx, c, "Archive.2024")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if rewritten != "" || used != "" {
		t.Errorf("delimiterRewrite = (%q, %q), want no rewrite", rewritten, used)
	}
}

func Test_delimiterRewrite_When_TypedNameAndItsRewriteAreBothListed_Then_KeepsTheTypedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		noSelect bool
	}{
		{name: "selectable typed name"},
		{name: "typed name listed as noselect", noSelect: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newStoreServer(t, "/", map[string][]fakeMsg{"Work/Sub": nil})
			if tt.noSelect {
				srv.markNoSelect("Work.Sub")
			} else {
				srv.addFolder("Work.Sub")
			}
			c := newAppClient(t, srv, "srv")

			rewritten, used, err := delimiterRewrite(e2eContext(t), c, "Work.Sub")

			if err != nil {
				t.Fatalf("delimiterRewrite: %v", err)
			}
			if rewritten != "" || used != "" {
				t.Errorf("delimiterRewrite = (%q, %q), want no rewrite: the typed name is listed, so a delimiter character in it is name text", rewritten, used)
			}
		})
	}
}

func TestRunSync_When_TypedNameAndItsRewriteAreBothListed_Then_OnlyTheTypedFolderIsCopied(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":       nil,
		"example.com": fakeMsgs("dot@x"),
		"example/com": fakeMsgs("slash@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("example.com", "Target"))

	// The only answer is for the "Proceed" prompt: a delimiter prompt would take it.
	out := &syncBuffer{}
	if err := runSync(e2eContext(t), cfg, e2eOptions(out, "y\n")); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsNone(t, out.String(), "Fix folder delimiters", "delimiter mismatch")
	assertIDs(t, "dst Target", dst.folderMsgIDs("Target"), "dot@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Target")
	assertIDs(t, "src EXAMINE names", src.capturedNames("EXAMINE"), "example.com", "example.com")
}

func Test_collectDelimiterFixes_When_ContextEndsDuringLookup_Then_ReturnsContextError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		srcDelimiter string
	}{
		{name: "source lookup", srcDelimiter: "/"},
		{name: "destination lookup after a flat source", srcDelimiter: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcC := newAppClient(t, newStoreServer(t, tt.srcDelimiter, map[string][]fakeMsg{"INBOX": nil}), "src")
			dstC := newAppClient(t, newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil}), "dst")
			base, end := context.WithCancel(t.Context())
			ctx := &endsAfterFirstErr{Context: base, end: end}

			fixes, err := collectDelimiterFixes(ctx, srcC, dstC, []config.DirectoryMapping{mapping("Archive.2024", "Archive.2024")})

			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
			if fixes != nil {
				t.Errorf("fixes = %+v, want nil", fixes)
			}
		})
	}
}

func TestRunSync_When_RenameClashNamesCarryControlCharacters_Then_FailureLinesShowEscapesAndNothingIsSent(t *testing.T) {
	t.Parallel()

	const (
		dottedWire = "v&ABs-[2J.1"
		plainWire  = "v&ABs-[2J_1"
		dottedName = "v\x1b[2J.1"
		plainName  = "v\x1b[2J_1"
	)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, dottedWire: fakeMsgs("a@x"), plainWire: fakeMsgs("b@x")})
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	// Quiet mode silences the progress writer, so the whole output can be checked for raw control characters.
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	got := out.String()
	assertContainsAll(t, got,
		`• v\x1b[2J.1 → v\x1b[2J_1: same destination folder as source folder v\x1b[2J_1`+"\n",
		`• v\x1b[2J_1 → v\x1b[2J_1: same destination folder as source folder v\x1b[2J.1`+"\n",
	)
	assertContainsNone(t, got, "\x1b", dottedName, plainName)
	assertIDs(t, "source EXAMINE names", src.capturedNames("EXAMINE"), "INBOX")
	if n := dst.callCount("CREATE") + dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d CREATE or APPEND commands, want 0", n)
	}
}

func TestRunSync_When_DestinationInboxIsMissingFromList_Then_OnlyTheDifferenceIsCopied(t *testing.T) {
	t.Parallel()

	for _, typed := range []string{"INBOX", "inbox"} {
		t.Run(typed, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x", "m3@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
			dst.hideFromList("INBOX")
			cfg := e2eConfig(src, dst, 1, mapping(typed, typed))

			first := runSyncConfirmed(t, cfg, "", "")
			second := runSyncConfirmed(t, cfg, "", "")

			assertContainsAll(t, first, "holds 2 of the 3 planned messages")
			assertContainsAll(t, second, "All folders already synced!")
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x", "m3@x")
			assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), "m3@x")
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "INBOX", "INBOX")
		})
	}
}

func TestRunSync_When_OnlyOneDestinationIsMissingFromList_Then_OnlyThatFolderIsChecked(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Listed": fakeMsgs("a1@x", "a2@x", "a3@x"),
		"Hidden": fakeMsgs("b1@x", "b2@x", "b3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Listed": fakeMsgs("a1@x"), "Hidden": fakeMsgs("b1@x")})
	dst.hideFromList("Hidden")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Listed", "Listed"), mapping("Hidden", "Hidden")), opts)

	if err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}
	assertIDs(t, "dst APPENDs to Listed", dst.appended("Listed"), "a2@x", "a3@x")
	assertIDs(t, "dst APPENDs to Hidden", dst.appended("Hidden"), "b2@x", "b3@x")
	assertContainsAll(t, out.String(), "Destination folder Hidden already exists and holds 1 of the 3 planned messages from Hidden")
	assertContainsNone(t, out.String(), "from Listed")
	examined := dst.capturedNames("EXAMINE")
	for _, folder := range []string{"Listed", "Hidden"} {
		n := 0
		for _, name := range examined {
			if name == folder {
				n++
			}
		}
		if n != 1 {
			t.Errorf("destination got %d EXAMINE commands for %s, want 1: only the hidden folder needs a second look (%q)", n, folder, examined)
		}
	}
	// The copy reads bodies with UID FETCH, so every plain FETCH is a Message-Id scan.
	if n := src.callCount("FETCH"); n != 3 {
		t.Errorf("source saw %d Message-Id scans, want 3: two planning scans and one rescan of Hidden", n)
	}
}

func TestRunSync_When_DestinationListFailsAtConnect_Then_ErrorNamesTheLabel(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst := newFakeServer(t)
	dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
		if verb == "LIST" {
			return "NO [CANNOT] list refused", true
		}
		return "", false
	}))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if err == nil || !strings.Contains(err.Error(), "[dst] list mailboxes") {
		t.Fatalf("error = %v, want it to contain %q", err, "[dst] list mailboxes")
	}
	if strings.Contains(err.Error(), "[]") {
		t.Errorf("error = %q, want no empty label", err)
	}
}

func TestRunSync_When_ParentAndChildFoldersAreBothMapped_Then_ChildMappingWinsInEitherOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "parent first", mappings: []config.DirectoryMapping{mapping("INBOX", "Old"), mapping("INBOX/Work", "Work")}},
		{name: "child first", mappings: []config.DirectoryMapping{mapping("INBOX/Work", "Work"), mapping("INBOX", "Old")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": fakeMsgs("m1@x"), "INBOX/Work": fakeMsgs("w1@x"), "INBOX/Work/A": fakeMsgs("a1@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 2, tt.mappings...)

			runSyncConfirmed(t, cfg, "", "")
			appends := dst.callCount("APPEND")
			second := runSyncConfirmed(t, cfg, "", "")

			assertIDs(t, "dst Old", dst.folderMsgIDs("Old"), "m1@x")
			assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "w1@x")
			assertIDs(t, "dst Work/A", dst.folderMsgIDs("Work/A"), "a1@x")
			if dst.hasFolder("Old/Work") || dst.hasFolder("Old/Work/A") {
				t.Errorf("destination holds Old/Work or Old/Work/A: the mapping of INBOX/Work must decide where its folders go")
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Old", "Work", "Work/A")
			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("second run sent %d more APPEND commands, want 0", n-appends)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_ExplicitMappingsNest_Then_MostSpecificWinsInEveryOrder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "A/B": nil, "A/B/C": nil, "A/B/C/D": nil, "A/B/E": nil, "A/F": nil})
	srcC := newAppClient(t, src, "src")
	dstC := newDestinationClient(t, "/")
	explicit := []config.DirectoryMapping{mapping("A", "X"), mapping("A/B", "Y"), mapping("A/B/C", "Z")}
	want := []string{"A->X", "A/B->Y", "A/B/C->Z", "A/B/C/D->Z/D", "A/B/E->Y/E", "A/F->X/F"}
	slices.Sort(want)

	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			roots := rootMappings(explicit[order[0]], explicit[order[1]], explicit[order[2]])

			got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, roots, "/", "/")

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			pairs := make([]string, len(got))
			for i, m := range got {
				pairs[i] = m.Source + "->" + m.Destination
			}
			slices.Sort(pairs)
			if !slices.Equal(pairs, want) {
				t.Errorf("expanded = %v, want %v", pairs, want)
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_ChildMappingOverridesParent_Then_VerboseNamesBothDestinations(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "INBOX/Work": nil, "INBOX/Work/A": nil})
	srcC := newAppClient(t, src, "src")
	dstC := newDestinationClient(t, "/")
	orders := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "parent first", mappings: []config.DirectoryMapping{mapping("INBOX", "Old"), mapping("INBOX/Work", "Work")}},
		{name: "child first", mappings: []config.DirectoryMapping{mapping("INBOX/Work", "Work"), mapping("INBOX", "Old")}},
	}
	var outputs []string

	for _, tt := range orders {
		var out strings.Builder
		got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(tt.mappings...), "/", "/")
		if err != nil {
			t.Fatalf("%s: expandMappingsWithSubfolders: %v", tt.name, err)
		}
		printExpansion(&out, report, got, true, false)
		if len(got) != 3 {
			t.Errorf("%s: expanded = %v, want three folders", tt.name, got)
		}
		assertContainsAll(t, out.String(),
			"  ℹ️  Folder INBOX/Work goes to Work, not to Old/Work: the mapping of INBOX/Work wins over the mapping of INBOX\n",
			"  ℹ️  Folder INBOX/Work/A goes to Work/A, not to Old/Work/A: the mapping of INBOX/Work wins over the mapping of INBOX\n",
			"  📁 Found subfolder: INBOX/Work/A → Work/A\n",
		)
		if n := strings.Count(out.String(), "Found subfolder"); n != 1 {
			t.Errorf("%s: output holds %d found lines, want 1\noutput: %q", tt.name, n, out.String())
		}
		assertContainsNone(t, out.String(), "already covered", "→ Old/Work", "→ Old/Work/A")
		outputs = append(outputs, out.String())

		var quiet strings.Builder
		printExpansion(&quiet, report, got, false, false)
		if quiet.Len() != 0 {
			t.Errorf("%s: output without --verbose = %q, want none", tt.name, quiet.String())
		}
	}

	if outputs[0] != outputs[1] {
		t.Errorf("the order of the map items changes the output\nparent first: %q\nchild first:  %q", outputs[0], outputs[1])
	}
}

func Test_expandMappingsWithSubfolders_When_OverrideNamesCarryControlCharacters_Then_LineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"P": nil, "P/K": nil, "P/K/" + hostileWireName: nil})
	srcC := newAppClient(t, src, "src")
	dstC := newDestinationClient(t, "/")
	roots := rootMappings(mapping("P", "Old"), mapping("P/K", "New"))
	var out strings.Builder

	got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, roots, "/", "/")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	printExpansion(&out, report, got, true, false)
	assertContainsAll(t, out.String(),
		"  ℹ️  Folder P/K/"+hostileShownName+" goes to New/"+hostileShownName+", not to Old/K/"+hostileShownName+": the mapping of P/K wins over the mapping of P\n")
	assertContainsNone(t, out.String(), hostileName, "\u202e", "\x1b[2J")
	want := folderMapping{DirectoryMapping: mapping("P/K/"+hostileName, "New/"+hostileName)}
	if !slices.Contains(got, want) {
		t.Errorf("expanded = %+v, want it to keep the raw entry %+v", got, want)
	}
}

func Test_expandMappingsWithSubfolders_When_ExplicitChildIsNoSelectOrUnlisted_Then_ItsChildrenFollowIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		folders  map[string][]fakeMsg
		noSelect []string
		explicit []config.DirectoryMapping
		want     []string
	}{
		{
			name:     "child is a container",
			folders:  map[string][]fakeMsg{"INBOX": nil, "INBOX/Work": nil, "INBOX/Work/A": nil},
			noSelect: []string{"INBOX/Work"},
			explicit: []config.DirectoryMapping{mapping("INBOX", "Old"), mapping("INBOX/Work", "Work")},
			want:     []string{"INBOX->Old", "INBOX/Work/A->Work/A"},
		},
		{
			name:     "child is not listed",
			folders:  map[string][]fakeMsg{"Archive/2024/Q1": nil},
			explicit: []config.DirectoryMapping{mapping("Archive", "Old"), mapping("Archive/2024", "Y")},
			want:     []string{"Archive->Old", "Archive/2024->Y", "Archive/2024/Q1->Y/Q1"},
		},
	}
	for _, tt := range tests {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s reversed=%t", tt.name, reversed), func(t *testing.T) {
				t.Parallel()

				src := newStoreServer(t, "/", tt.folders)
				src.markNoSelect(tt.noSelect...)
				srcC := newAppClient(t, src, "src")
				dstC := newDestinationClient(t, "/")
				explicit := slices.Clone(tt.explicit)
				if reversed {
					slices.Reverse(explicit)
				}

				got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(explicit...), "/", "/")

				if err != nil {
					t.Fatalf("expandMappingsWithSubfolders: %v", err)
				}
				pairs := make([]string, len(got))
				for i, m := range got {
					pairs[i] = m.Source + "->" + m.Destination
				}
				slices.Sort(pairs)
				if !slices.Equal(pairs, tt.want) {
					t.Errorf("expanded = %v, want %v", pairs, tt.want)
				}
			})
		}
	}
}

func Test_expandMappingsWithSubfolders_When_ExplicitDestinationEqualsRenamedDerivedName_Then_ExplicitEntryIsKept(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Projects": nil, "Projects/v1.2": nil})
	srcC := newAppClient(t, src, "src")
	dstC := newDestinationClient(t, ".")
	explicit := []config.DirectoryMapping{mapping("Projects", "Old.Projects"), mapping("Projects/v1.2", "Old.Projects.v1_2")}

	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed=%t", reversed), func(t *testing.T) {
			roots := slices.Clone(explicit)
			if reversed {
				slices.Reverse(roots)
			}

			got, _, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(roots...), "/", ".")

			if err != nil {
				t.Fatalf("expandMappingsWithSubfolders: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("expanded = %+v, want two entries", got)
			}
			for _, m := range got {
				if m.renamed {
					t.Errorf("entry %+v is marked renamed, want only the typed destination and no rename", m)
				}
			}
			for _, want := range explicit {
				if !slices.ContainsFunc(got, func(m folderMapping) bool { return m.DirectoryMapping == want }) {
					t.Errorf("expanded = %+v, want it to hold %+v", got, want)
				}
			}
		})
	}
}

func Test_mostSpecific(t *testing.T) {
	t.Parallel()

	entry := func(root, source string) expansionEntry {
		return expansionEntry{root: root, folderMapping: folderMapping{DirectoryMapping: mapping(source, "D/"+root)}}
	}
	tests := []struct {
		name    string
		entries []expansionEntry
		want    []int
	}{
		{name: "empty input", entries: nil, want: []int{}},
		{name: "distinct sources pass through", entries: []expansionEntry{entry("A", "A"), entry("A", "A/B"), entry("A/B", "A/B/C")}, want: []int{0, 1, 2}},
		{name: "longer root wins when it comes last", entries: []expansionEntry{entry("A", "A/B/C"), entry("A/B", "A/B/C")}, want: []int{1, 1}},
		{name: "longer root wins when it comes first", entries: []expansionEntry{entry("A/B", "A/B/C"), entry("A", "A/B/C")}, want: []int{0, 0}},
		{
			name:    "longer root wins in the middle",
			entries: []expansionEntry{entry("A", "A/B/C/D"), entry("A/B/C", "A/B/C/D"), entry("A/B", "A/B/C/D")},
			want:    []int{1, 1, 1},
		},
		{name: "equal roots keep the first entry", entries: []expansionEntry{entry("A", "A/B"), entry("A", "A/B"), entry("A", "A/B")}, want: []int{0, 0, 0}},
		{
			name:    "equal length picks the smaller root, upper case first",
			entries: []expansionEntry{entry("INBOX/Work", "INBOX/Work/A"), entry("Inbox/Work", "INBOX/Work/A")},
			want:    []int{0, 0},
		},
		{
			name:    "equal length picks the smaller root, upper case last",
			entries: []expansionEntry{entry("Inbox/Work", "INBOX/Work/A"), entry("INBOX/Work", "INBOX/Work/A")},
			want:    []int{1, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := mostSpecific(tt.entries); !slices.Equal(got, tt.want) {
				t.Errorf("mostSpecific = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_checkSourceConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantText string
		in       []config.DirectoryMapping
	}{
		{name: "no items"},
		{name: "distinct sources", in: []config.DirectoryMapping{mapping("A", "X"), mapping("B", "X")}},
		{name: "equal items", in: []config.DirectoryMapping{mapping("A", "X"), mapping("A", "X")}},
		{
			name: "two destinations", in: []config.DirectoryMapping{mapping("A", "X"), mapping("A", "Y")},
			wantText: "map entries 1 and 2: different destination folders for source folder A",
		},
		{
			name: "the first item is the reference", in: []config.DirectoryMapping{mapping("A", "X"), mapping("B", "X"), mapping("A", "X"), mapping("A", "Y")},
			wantText: "map entries 1 and 4: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkSourceConflicts(tt.in)

			if tt.wantText == "" {
				if err != nil {
					t.Errorf("checkSourceConflicts = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errSourceConflict) {
				t.Fatalf("checkSourceConflicts = %v, want errSourceConflict", err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want the text %q", err, tt.wantText)
			}
		})
	}
}

func TestRunSync_When_TwoMappingsNameOneSourceWithDifferentDestinations_Then_RunStopsBeforeThePlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		srcFolders map[string][]fakeMsg
		delimiter  string
		wantSuffix string
		mappings   []config.DirectoryMapping
	}{
		{
			name:       "two spellings of INBOX",
			srcFolders: map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			delimiter:  "/",
			mappings:   []config.DirectoryMapping{mapping("inbox", "A"), mapping("INBOX", "B")},
			wantSuffix: "source folder INBOX",
		},
		{
			name:       "delimiter rewrite makes one source",
			srcFolders: map[string][]fakeMsg{"INBOX": nil, "A/B": fakeMsgs("b1@x")},
			delimiter:  "/",
			mappings:   []config.DirectoryMapping{mapping("A.B", "X"), mapping("A/B", "Y")},
			wantSuffix: "source folder A/B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, tt.delimiter, tt.srcFolders)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if !errors.Is(err, errSourceConflict) {
				t.Fatalf("error = %v, want errSourceConflict", err)
			}
			if !strings.Contains(err.Error(), "map entries 1 and 2: ") || !strings.HasSuffix(err.Error(), tt.wantSuffix) {
				t.Errorf("error = %q, want the entry numbers and the source name", err)
			}
			if n := src.callCount("EXAMINE"); n != 0 {
				t.Errorf("source saw %d EXAMINE commands (%q), want 0", n, src.capturedNames("EXAMINE"))
			}
			for _, verb := range []string{"CREATE", "APPEND"} {
				if n := dst.callCount(verb); n != 0 {
					t.Errorf("destination saw %d %s commands, want 0", n, verb)
				}
			}
			assertContainsNone(t, out.String(), "Messages to be copied")
		})
	}
}

func TestRunSync_When_TwoMappingsShareADestination_Then_ASharedMessageIdIsCopiedOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dstFolders  map[string][]fakeMsg
		name        string
		workers     int
		wantCreated bool
	}{
		{name: "one worker and an empty destination", workers: 1, dstFolders: map[string][]fakeMsg{"Sent": nil}},
		{name: "two workers and an empty destination", workers: 2, dstFolders: map[string][]fakeMsg{"Sent": nil}},
		{name: "one worker and a missing destination", workers: 1, dstFolders: map[string][]fakeMsg{"INBOX": nil}, wantCreated: true},
		{name: "two workers and a missing destination", workers: 2, dstFolders: map[string][]fakeMsg{"INBOX": nil}, wantCreated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"Sent":          fakeMsgs("m1@x", "m2@x"),
				"Sent Messages": fakeMsgs("m1@x", "m3@x"),
			})
			dst := newStoreServer(t, "/", tt.dstFolders)
			cfg := e2eConfig(src, dst, tt.workers, mapping("Sent", "Sent"), mapping("Sent Messages", "Sent"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				"• Sent → Sent will copy 2 messages",
				"• Sent Messages → Sent will copy 1 messages",
				"Total new messages to sync: 3",
			)
			got := slices.Sorted(slices.Values(dst.folderMsgIDs("Sent")))
			assertIDs(t, "dst Sent after the first run", got, "m1@x", "m2@x", "m3@x")
			if n := len(dst.appended("Sent")); n != 3 {
				t.Errorf("dst saw %d APPENDs in the first run, want 3", n)
			}
			if tt.wantCreated {
				assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Sent")
				assertContainsAll(t, first, "Folders to be created on destination:\n• Sent\n\n📨 Total new messages to sync: 3")
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := len(dst.appended("Sent")); n != 3 {
				t.Errorf("dst saw %d APPENDs over both runs, want 3", n)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_SharedDestinationHoldsCopies_Then_TheLargestCountWins(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	held := subjectBody(id, "held")
	fromA := []string{subjectBody(id, "a1"), subjectBody(id, "a2")}
	fromB := []string{subjectBody(id, "b1"), subjectBody(id, "b2"), subjectBody(id, "b3")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": append(sharedIDMsgs(id, fromA...), fakeMsg{msgID: "y@x"}),
		"B": append(sharedIDMsgs(id, fromB...), fakeMsg{msgID: "z@x"}),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Archive": sharedIDMsgs(id, held)})
	cfg := e2eConfig(src, dst, 1, mapping("A", "Archive"), mapping("B", "Archive"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"• A → Archive will copy 2 messages",
		"• B → Archive will copy 2 messages",
		"Total new messages to sync: 4",
	)
	assertIDs(t, "dst Archive after the first run", slices.Sorted(slices.Values(dst.folderMsgIDs("Archive"))), "x@x", "x@x", "x@x", "y@x", "z@x")
	if n := len(dst.appended("Archive")); n != 4 {
		t.Errorf("dst saw %d APPENDs in the first run, want 4", n)
	}
	// The earlier mapping carries the shared copies, and the lowest UIDs of an id count as present.
	wantBodies := []string{held, fromA[1], imapFullBody("y@x"), fromB[2], imapFullBody("z@x")}
	if got := dst.folderBodies("Archive"); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(wantBodies))) {
		t.Errorf("dst Archive bodies = %q, want %q in any order", got, wantBodies)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	if n := len(dst.appended("Archive")); n != 4 {
		t.Errorf("dst saw %d APPENDs over both runs, want 4", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SharedDestinationMapsInAnotherOrder_Then_TheFirstMappingCopiesTheSharedMessage(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Sent":          fakeMsgs("m1@x", "m2@x"),
		"Sent Messages": fakeMsgs("m1@x", "m3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Sent": nil})
	cfg := e2eConfig(src, dst, 1, mapping("Sent Messages", "Sent"), mapping("Sent", "Sent"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"• Sent Messages → Sent will copy 2 messages",
		"• Sent → Sent will copy 1 messages",
		"Total new messages to sync: 3",
	)
	assertIDs(t, "dst Sent", slices.Sorted(slices.Values(dst.folderMsgIDs("Sent"))), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs", dst.appended("Sent"), "m1@x", "m3@x", "m2@x")
}

func TestRunSync_When_AnEarlierMappingCoversALaterOne_Then_TheLaterOneHasNoPlan(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("A", "Archive"), mapping("B", "Archive")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got, "• A → Archive will copy 2 messages", "Total new messages to sync: 2", "Sync completed successfully")
	assertContainsNone(t, got, "B → Archive")
	assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "m1@x", "m2@x")
	if n := len(dst.appended("Archive")); n != 2 {
		t.Errorf("dst saw %d APPENDs, want 2", n)
	}
}

func TestRunSync_When_MappingsNameOneDestinationInTwoLetterCasesOfInbox_Then_TheyShareTheFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "INBOX/Archive"), mapping("B", "inbox/Archive")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsAll(t, out.String(),
		"• A → INBOX/Archive will copy 2 messages",
		"• B → INBOX/Archive will copy 1 messages",
		"Folders to be created on destination:\n• INBOX/Archive\n\n📨 Total new messages to sync: 3",
	)
	assertIDs(t, "dst INBOX/Archive", slices.Sorted(slices.Values(dst.folderMsgIDs("INBOX/Archive"))), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "INBOX/Archive")
}

func TestRunSync_When_SharedDestinationExistsButLISTHidesIt_Then_TheRecheckKeepsEachMessageOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		held         string
		wantInfo     string
		wantAppended []string
	}{
		{
			name:         "the folder holds a message of the first plan",
			held:         "m1@x",
			wantAppended: []string{"m2@x", "m3@x"},
			wantInfo:     "holds 1 of the 2 planned messages from A",
		},
		{
			name:         "the folder holds a message of the second plan",
			held:         "m3@x",
			wantAppended: []string{"m1@x", "m2@x"},
			wantInfo:     "holds 1 of the 1 planned messages from B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Archive": fakeMsgs(tt.held)})
			dst.hideFromList("Archive")
			cfg := e2eConfig(src, dst, 1, mapping("A", "Archive"), mapping("B", "Archive"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				"• A → Archive will copy 2 messages",
				"• B → Archive will copy 1 messages",
				"Total new messages to sync: 3",
				tt.wantInfo,
				"Sync completed successfully",
			)
			assertIDs(t, "dst Archive after the first run", slices.Sorted(slices.Values(dst.folderMsgIDs("Archive"))), "m1@x", "m2@x", "m3@x")
			assertIDs(t, "dst APPENDs", slices.Sorted(slices.Values(dst.appended("Archive"))), tt.wantAppended...)

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := len(dst.appended("Archive")); n != 2 {
				t.Errorf("dst saw %d APPENDs over both runs, want 2", n)
			}
		})
	}
}

func TestRunSync_When_HiddenSharedDestinationHoldsOneSizeOfAMessageId_Then_ItEndsWithEverySize(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	small, large := bodyOfSize(id, 200), bodyOfSize(id, 300)
	tests := []struct {
		name   string
		folder string
		hidden bool
	}{
		{name: "hidden folder", folder: "D", hidden: true},
		{name: "visible folder", folder: "D"},
		{name: "unlisted INBOX", folder: "INBOX", hidden: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": {{msgID: id, body: small}},
				"B": {{msgID: id, body: small}, {msgID: id, body: large}},
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, tt.folder: {{msgID: id, body: large}}})
			if tt.hidden {
				dst.hideFromList(tt.folder)
			}
			cfg := e2eConfig(src, dst, 1, mapping("A", tt.folder), mapping("B", tt.folder))

			first := runSyncOnce(t, cfg)

			if tt.hidden {
				assertContainsAll(t, first,
					fmt.Sprintf("• A → %s will copy 1 messages", tt.folder),
					fmt.Sprintf("• B → %s will copy 1 messages", tt.folder),
					fmt.Sprintf("Destination folder %s already exists and holds 1 of the 1 planned messages from B, so they are not copied", tt.folder),
				)
				assertContainsNone(t, first, "planned messages from A")
				if n := strings.Count(first, "already exists and holds"); n != 1 {
					t.Errorf("output holds %d lines about held messages, want 1\noutput: %q", n, first)
				}
			} else {
				assertContainsAll(t, first, fmt.Sprintf("• B → %s will copy 1 messages (≈ 200 B)", tt.folder))
				assertContainsNone(t, first, "already exists and holds")
			}
			assertContainsAll(t, first, "Sync completed successfully")
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands in the first run, want 1", n)
			}
			wantSizes := []int{200, 300}
			if got := slices.Sorted(slices.Values(bodySizes(dst.folderBodies(tt.folder)))); !slices.Equal(got, wantSizes) {
				t.Fatalf("dst %s holds copies of sizes %v after the first run, want %v", tt.folder, got, wantSizes)
			}

			for run := 2; run <= 3; run++ {
				again := runSyncOnce(t, cfg)

				assertContainsAll(t, again, "All folders already synced!")
				if n := dst.callCount("APPEND"); n != 1 {
					t.Errorf("destination saw %d APPEND commands after run %d, want 1", n, run)
				}
				if got := slices.Sorted(slices.Values(bodySizes(dst.folderBodies(tt.folder)))); !slices.Equal(got, wantSizes) {
					t.Errorf("dst %s holds copies of sizes %v after run %d, want %v", tt.folder, got, run, wantSizes)
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_ThreeMappingsShareAHiddenDestination_Then_ItEndsWithTheLargestCount(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	one, two, three := bodyOfSize(id, 100), bodyOfSize(id, 200), bodyOfSize(id, 300)
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d workers", workers), func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": {{msgID: id, body: one}},
				"B": {{msgID: id, body: one}, {msgID: id, body: two}},
				"C": {{msgID: id, body: one}, {msgID: id, body: two}, {msgID: id, body: three}},
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "D": {{msgID: id, body: two}}})
			dst.hideFromList("D")
			cfg := e2eConfig(src, dst, workers, mapping("A", "D"), mapping("B", "D"), mapping("C", "D"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				"• A → D will copy 1 messages",
				"• B → D will copy 1 messages",
				"• C → D will copy 1 messages",
				"Destination folder D already exists and holds 1 of the 1 planned messages from B, so they are not copied",
				"Sync completed successfully",
			)
			assertContainsNone(t, first, "planned messages from A", "planned messages from C")
			if n := dst.callCount("APPEND"); n != 2 {
				t.Errorf("destination saw %d APPEND commands in the first run, want 2", n)
			}
			wantSizes := []int{100, 200, 300}
			if got := slices.Sorted(slices.Values(bodySizes(dst.folderBodies("D")))); !slices.Equal(got, wantSizes) {
				t.Fatalf("dst D holds copies of sizes %v after the first run, want %v", got, wantSizes)
			}

			assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != 2 {
				t.Errorf("destination saw %d APPEND commands after the second run, want 2", n)
			}
		})
	}
}

func TestRunSync_When_HiddenSharedDestinationKeepsTheFirstMappingsCopyAndItsBodyIsEmpty_Then_ItsStandbyArrives(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	small, large := bodyOfSize(id, 200), bodyOfSize(id, 300)
	smallOfB := bodyOfSize(id, 199) + "y"
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": {{msgID: id, body: small}},
		"B": {{msgID: id, body: smallOfB}, {msgID: id, body: large}},
	})
	src.answerBodyIn("A", id, "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "D": {{msgID: id, body: large}}})
	dst.hideFromList("D")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "D"), mapping("B", "D")), opts)

	if err != nil {
		t.Fatalf("runSync error = %v, want nil\noutput: %s", err, out.String())
	}
	assertContainsAll(t, out.String(), "Sync completed successfully")
	assertContainsNone(t, out.String(), "errors occurred")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	if got, want := slices.Sorted(slices.Values(dst.folderBodies("D"))), slices.Sorted(slices.Values([]string{smallOfB, large})); !slices.Equal(got, want) {
		t.Errorf("dst D holds bodies of sizes %v, want the 200-byte copy of B and the 300-byte copy", bodySizes(got))
	}
}

func TestRunSync_When_TheFirstSharedMappingFails_Then_TheSecondCopiesTheSharedMessage(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Sent":          fakeMsgs("m1@x", "m2@x"),
		"Sent Messages": fakeMsgs("m1@x", "m3@x"),
	})
	src.refuseOpenWith("Sent", "NO [CANNOT] folder gone")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"Sent": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Sent", "Sent"), mapping("Sent Messages", "Sent")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(),
		folderFailuresHeader,
		"• Sent → Sent: cannot scan source folder: ",
		"• Sent Messages → Sent will copy 2 messages",
		"1 folders failed",
	)
	assertIDs(t, "dst Sent", dst.folderMsgIDs("Sent"), "m1@x", "m3@x")
}

func TestRunSync_When_ExpandedSubfolderAndMappingShareADestination_Then_EachMessageIsCopiedOnce(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Archive":      nil,
		"Archive/2020": fakeMsgs("m1@x", "m2@x"),
		"Old/2020":     fakeMsgs("m1@x", "m3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("Archive", "Archive"), mapping("Old/2020", "Archive/2020")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsAll(t, out.String(), "Total new messages to sync: 3")
	assertIDs(t, "dst Archive/2020", slices.Sorted(slices.Values(dst.folderMsgIDs("Archive/2020"))), "m1@x", "m2@x", "m3@x")
	if n := len(dst.appended("Archive/2020")); n != 3 {
		t.Errorf("dst saw %d APPENDs, want 3", n)
	}
}

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, rest := range permutations(n - 1) {
		for at := 0; at <= len(rest); at++ {
			p := slices.Insert(slices.Clone(rest), at, n-1)
			out = append(out, p)
		}
	}
	return out
}

func TestRunSync_When_ThreeNestedMappingsAreGiven_Then_MostSpecificWinsInEveryOrder(t *testing.T) {
	t.Parallel()

	explicit := []config.DirectoryMapping{mapping("A", "X"), mapping("A/B", "Y"), mapping("A/B/C", "Z")}
	wantDst := map[string][]string{"X": {"a1@x"}, "Y": {"b1@x"}, "Z": {"c1@x"}, "Z/D": {"d1@x"}, "Y/E": {"e1@x"}, "X/F": {"f1@x"}}
	// Each name would exist if a less specific mapping decided where its folder goes.
	wrongDst := []string{"X/B", "X/B/C", "X/B/C/D", "X/B/E", "Y/C", "Y/C/D"}

	for _, order := range permutations(len(explicit)) {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("a1@x"), "A/B": fakeMsgs("b1@x"), "A/B/C": fakeMsgs("c1@x"),
				"A/B/C/D": fakeMsgs("d1@x"), "A/B/E": fakeMsgs("e1@x"), "A/F": fakeMsgs("f1@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 2, explicit[order[0]], explicit[order[1]], explicit[order[2]])

			first := runSyncConfirmed(t, cfg, "", "")
			appends := dst.callCount("APPEND")
			second := runSyncConfirmed(t, cfg, "", "")

			for folder, ids := range wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			for _, folder := range wrongDst {
				if dst.hasFolder(folder) {
					t.Errorf("destination holds %q: the mapping with the longest source name must decide where a folder goes", folder)
				}
			}
			creates := dst.capturedNames("CREATE")
			slices.Sort(creates)
			assertIDs(t, "dst CREATE names", creates, "X", "X/F", "Y", "Y/E", "Z", "Z/D")
			if appends != len(wantDst) {
				t.Errorf("first run sent %d APPEND commands, want %d", appends, len(wantDst))
			}
			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("second run sent %d more APPEND commands, want 0", n-appends)
			}
			assertContainsNone(t, first, "goes to")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_MapRepeatsAnItemExactly_Then_RunMatchesASingleItem(t *testing.T) {
	t.Parallel()

	type outcome struct {
		appends  map[string][]string
		examine  []string
		create   []string
		fetch    int
		uidFetch int
		willCopy int
	}
	run := func(t *testing.T, mappings ...config.DirectoryMapping) outcome {
		t.Helper()
		src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x"), "A/B": fakeMsgs("b1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "X": nil})
		first := runSyncOnce(t, e2eConfig(src, dst, 2, mappings...))
		assertContainsAll(t, runSyncOnce(t, e2eConfig(src, dst, 2, mappings...)), "All folders already synced!")
		got := outcome{
			examine: src.capturedNames("EXAMINE"), create: dst.capturedNames("CREATE"),
			fetch: src.callCount("FETCH"), uidFetch: src.callCount("UID FETCH"),
			appends:  map[string][]string{"X": dst.appended("X"), "X/B": dst.appended("X/B")},
			willCopy: strings.Count(first, "will copy"),
		}
		slices.Sort(got.examine)
		slices.Sort(got.create)
		return got
	}

	single := run(t, mapping("A", "X"))
	repeated := run(t, mapping("A", "X"), mapping("A", "X"))

	if !slices.Equal(repeated.examine, single.examine) {
		t.Errorf("source EXAMINE names = %q, want %q: each folder is scanned once on each side", repeated.examine, single.examine)
	}
	if !slices.Equal(repeated.create, single.create) {
		t.Errorf("destination CREATE names = %q, want %q", repeated.create, single.create)
	}
	if repeated.fetch != single.fetch || repeated.uidFetch != single.uidFetch {
		t.Errorf("source FETCH, UID FETCH = %d, %d, want %d, %d", repeated.fetch, repeated.uidFetch, single.fetch, single.uidFetch)
	}
	if single.willCopy != 2 {
		t.Errorf("single-item preview lines = %d, want 2, for A and A/B", single.willCopy)
	}
	if repeated.willCopy != single.willCopy {
		t.Errorf("repeated-item preview lines = %d, want %d", repeated.willCopy, single.willCopy)
	}
	for folder, want := range map[string][]string{"X": {"a1@x", "a2@x"}, "X/B": {"b1@x"}} {
		if !slices.Equal(repeated.appends[folder], want) {
			t.Errorf("APPENDs to %s = %v, want %v: a repeated item must not copy a folder twice", folder, repeated.appends[folder], want)
		}
	}
}

func TestRunSync_When_MirrorHoldsNestedAndRenamedFolders_Then_NoMappingOverridesAnother(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		dstDelimiter  string
		wantDst       map[string][]string
		wantCreate    []string
		wantLeftEmpty []string
	}{
		{
			name:         "same delimiter",
			dstDelimiter: "/",
			wantDst: map[string][]string{
				"INBOX": {"i1@x"}, "INBOX/Work": {"w1@x"}, "INBOX/Work/A": {"a1@x"},
				"Archive": {"r1@x"}, "Archive/2020": {"r2@x"}, "Archive/a.b": {"r3@x"}, "Archive/a.b/c": {"r4@x"},
				"x.y": {"x1@x"}, "Q/1.2/3": {"q1@x"},
			},
			wantCreate:    []string{"Archive", "Archive/2020", "Archive/a.b", "Archive/a.b/c", "INBOX/Work", "INBOX/Work/A", "Q", "Q/1.2", "Q/1.2/3", "x.y"},
			wantLeftEmpty: []string{"Q", "Q/1.2"},
		},
		{
			name:         "dot delimiter renames segments that hold a dot",
			dstDelimiter: ".",
			wantDst: map[string][]string{
				"INBOX": {"i1@x"}, "INBOX.Work": {"w1@x"}, "INBOX.Work.A": {"a1@x"},
				"Archive": {"r1@x"}, "Archive.2020": {"r2@x"}, "Archive.a_b": {"r3@x"}, "Archive.a_b.c": {"r4@x"},
				"x_y": {"x1@x"}, "Q.1_2.3": {"q1@x"},
			},
			wantCreate:    []string{"Archive", "Archive.2020", "Archive.a_b", "Archive.a_b.c", "INBOX.Work", "INBOX.Work.A", "Q", "Q.1_2", "Q.1_2.3", "x_y"},
			wantLeftEmpty: []string{"Q", "Q.1_2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": fakeMsgs("i1@x"), "INBOX/Work": fakeMsgs("w1@x"), "INBOX/Work/A": fakeMsgs("a1@x"),
				"Archive": fakeMsgs("r1@x"), "Archive/2020": fakeMsgs("r2@x"), "Archive/a.b": fakeMsgs("r3@x"), "Archive/a.b/c": fakeMsgs("r4@x"),
				"x.y": fakeMsgs("x1@x"), "Q/1.2/3": fakeMsgs("q1@x"),
			})
			dst := newStoreServer(t, tt.dstDelimiter, map[string][]fakeMsg{"INBOX": nil})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 2), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertContainsNone(t, out.String(), "goes to", "wins over")
			for folder, ids := range tt.wantDst {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder), ids...)
			}
			for _, folder := range tt.wantLeftEmpty {
				assertIDs(t, "dst "+folder, dst.folderMsgIDs(folder))
			}
			creates := dst.capturedNames("CREATE")
			slices.Sort(creates)
			assertIDs(t, "dst CREATE names", creates, tt.wantCreate...)
			if n := dst.callCount("APPEND"); n != len(tt.wantDst) {
				t.Errorf("destination saw %d APPEND commands, want %d", n, len(tt.wantDst))
			}

			assertContainsAll(t, runSyncOnce(t, e2eConfig(src, dst, 2)), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != len(tt.wantDst) {
				t.Errorf("the second run left %d APPEND commands in total, want %d", n, len(tt.wantDst))
			}
		})
	}
}

func Test_expandMappingsWithSubfolders_When_QuietAndVerboseAreBothSet_Then_NothingIsPrinted(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "INBOX/Work": nil, "INBOX/Work/A": nil})
	srcC := newAppClient(t, src, "src")
	dstC := newDestinationClient(t, "/")
	var out strings.Builder

	got, report, err := expandMappingsWithSubfolders(t.Context(), srcC, dstC, rootMappings(mapping("INBOX", "Old"), mapping("INBOX/Work", "Work")), "/", "/")

	if err != nil {
		t.Fatalf("expandMappingsWithSubfolders: %v", err)
	}
	printExpansion(&out, report, got, true, true)
	if out.Len() != 0 {
		t.Errorf("output = %q, want none with --quiet", out.String())
	}
	if len(got) != 3 {
		t.Errorf("expanded = %v, want three folders: --quiet changes the output only", got)
	}
}

func TestRunSync_When_ExplicitChildDestinationEqualsRenamedSiblingDestination_Then_BothFailAsAClash(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Projects":      fakeMsgs("p1@x"),
		"Projects/v1.2": fakeMsgs("a1@x"),
		"Projects/v1_2": fakeMsgs("b1@x"),
	})
	dst := newStoreServer(t, ".", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Projects", "Old.Projects"), mapping("Projects/v1_2", "Old.Projects.v1_2")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(),
		folderFailuresHeader,
		"• Projects/v1.2 → Old.Projects.v1_2: same destination folder as source folder Projects/v1_2\n",
		"• Projects/v1_2 → Old.Projects.v1_2: same destination folder as source folder Projects/v1.2\n",
		"2 folders failed",
	)
	assertIDs(t, "dst Old.Projects", dst.folderMsgIDs("Old.Projects"), "p1@x")
	if dst.hasFolder("Old.Projects.v1_2") {
		t.Error("destination holds Old.Projects.v1_2: the clashing sources must not merge")
	}
	for _, name := range []string{"Projects/v1.2", "Projects/v1_2"} {
		if slices.Contains(src.capturedNames("EXAMINE"), name) {
			t.Errorf("source EXAMINE names = %q, want no %q: a clashing folder must not be scanned", src.capturedNames("EXAMINE"), name)
		}
	}
}

func Test_maybeDiff_When_SourceAndDestinationCountsVary_Then_PlanAndRecheckPickTheSameMessages(t *testing.T) {
	t.Parallel()

	// The UIDs rise in steps of five and the sizes differ, so a wrong pick shows in both.
	three := []client.MessageRef{{UID: 10, Size: 100}, {UID: 15, Size: 200}, {UID: 20, Size: 300}}
	tests := []struct {
		name     string
		src      []client.MessageRef
		dstSizes []uint32
		wantUIDs []uint32
	}{
		{name: "three messages and none on the destination", src: three, wantUIDs: []uint32{10, 15, 20}},
		{name: "three messages and one on the destination", src: three, dstSizes: []uint32{7}, wantUIDs: []uint32{15, 20}},
		{name: "three messages and three on the destination", src: three, dstSizes: []uint32{7, 7, 7}},
		{name: "one message and two on the destination", src: three[:1], dstSizes: []uint32{7, 7}},
		{
			name:     "the destination copy has the size of the last source copy",
			src:      []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}},
			dstSizes: []uint32{30},
			wantUIDs: []uint32{4, 5},
		},
		{
			name:     "the destination copies have the sizes of the first and the last source copy",
			src:      []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}},
			dstSizes: []uint32{10, 30},
			wantUIDs: []uint32{5},
		},
		{
			name:     "one destination copy matches by size and the other falls back to the lowest UID",
			src:      []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}},
			dstSizes: []uint32{30, 99},
			wantUIDs: []uint32{5},
		},
		{
			name:     "all source copies have one size",
			src:      []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 10}, {UID: 6, Size: 10}},
			dstSizes: []uint32{10},
			wantUIDs: []uint32{5, 6},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srcMap := func() map[string][]client.MessageRef {
				return map[string][]client.MessageRef{"s@x": slices.Clone(tt.src)}
			}
			dstMap := func(sizes []uint32) map[string][]client.MessageRef {
				held := make([]client.MessageRef, len(sizes))
				for i, size := range sizes {
					held[i] = client.MessageRef{UID: uint32(900 + i), Size: size}
				}
				return map[string][]client.MessageRef{"s@x": held}
			}
			plan := func(sizes []uint32) FolderSyncPlan {
				t.Helper()
				plans := make([]FolderSyncPlan, 1)
				var totalNew atomic.Int64
				var totalNewSize atomic.Uint64
				s := &folderScan{srcMap: srcMap(), dstMap: dstMap(sizes), srcMessages: len(tt.src), dstExists: true}
				maybeDiff(s, 0, []config.DirectoryMapping{mapping("S", "D")}, plans, &totalNew, &totalNewSize, nil, copyByCount)
				if !maybeDiff(s, 0, []config.DirectoryMapping{mapping("S", "D")}, plans, &totalNew, &totalNewSize, nil, copyByCount) {
					t.Fatal("maybeDiff returned false on the second call, want true")
				}
				return plans[0]
			}
			var wantSize uint64
			for _, r := range tt.src {
				if slices.Contains(tt.wantUIDs, r.UID) {
					wantSize += uint64(r.Size)
				}
			}

			planned := plan(tt.dstSizes)

			if !slices.Equal(planned.SrcUIDs, tt.wantUIDs) || planned.NewSize != wantSize {
				t.Fatalf("plan = UIDs %v, size %d, want UIDs %v, size %d", planned.SrcUIDs, planned.NewSize, tt.wantUIDs, wantSize)
			}
			// Rechecking the plan for an empty destination against the same copies keeps exactly what a plan made with those copies picks.
			rechecked, drops := rediffOnePlan(plan(nil), srcMap(), dstMap(tt.dstSizes))
			if !slices.Equal(rechecked.SrcUIDs, tt.wantUIDs) || rechecked.NewSize != wantSize {
				t.Errorf("recheck of the full plan = UIDs %v, size %d, want UIDs %v, size %d", rechecked.SrcUIDs, rechecked.NewSize, tt.wantUIDs, wantSize)
			}
			if wantHeld := len(tt.src) - len(tt.wantUIDs); drops.held != wantHeld || drops.gone != 0 {
				t.Errorf("recheck drops = %+v, want held %d and gone 0", drops, wantHeld)
			}
			// The recheck of a plan made with the same copies changes nothing.
			again, drops := rediffOnePlan(planned, srcMap(), dstMap(tt.dstSizes))
			if !slices.Equal(again.SrcUIDs, planned.SrcUIDs) || again.NewSize != planned.NewSize || drops != (recheckDrops{}) {
				t.Errorf("recheck with equal copies = UIDs %v, size %d, drops %+v, want the plan %v, %d and no drops", again.SrcUIDs, again.NewSize, drops, planned.SrcUIDs, planned.NewSize)
			}
		})
	}
}

func Test_maybeDiff_When_ThreeMappingsShareADestinationAmongOthers_Then_CountsAccumulateAndAreReleased(t *testing.T) {
	t.Parallel()

	mappings := []config.DirectoryMapping{mapping("A", "X"), mapping("B", "Y"), mapping("C", "X"), mapping("D", "X")}
	srcMaps := []map[string][]client.MessageRef{
		{"m1": msgRefs(1), "m2": msgRefs(2)},
		{"m1": msgRefs(1), "m9": msgRefs(2)},
		{"m1": msgRefs(1), "m3": msgRefs(2)},
		{"m1": msgRefs(1), "m3": msgRefs(2), "m4": msgRefs(3)},
	}
	wantUIDs := [][]uint32{{1, 2}, {1, 2}, {2}, {3}}
	// After slot k, X holds the ids that slots up to k scheduled for it.
	wantHeld := []map[string]int{{"m1": 1, "m2": 1}, {"m1": 1, "m2": 1}, {"m1": 1, "m2": 1, "m3": 1}, nil}
	shared := newSharedDestinations(mappings)
	if shared == nil {
		t.Fatal("newSharedDestinations = nil, want a set for destination X")
	}
	if last, ok := shared.lastIndex("X"); !ok || last != 3 {
		t.Errorf("lastIndex(X) = %d, %t, want 3, true", last, ok)
	}
	if _, ok := shared.lastIndex("Y"); ok {
		t.Error("lastIndex(Y) found an entry, want none for a destination with one mapping")
	}
	plans := make([]FolderSyncPlan, len(mappings))
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64

	for i := range mappings {
		s := &folderScan{srcMap: srcMaps[i], srcMessages: countRefs(srcMaps[i]), srcUIDValidity: uint32(10 + i), dstExists: true}
		maybeDiff(s, i, mappings, plans, &totalNew, &totalNewSize, shared, copyByCount)
		if !maybeDiff(s, i, mappings, plans, &totalNew, &totalNewSize, shared, copyByCount) {
			t.Fatalf("slot %d: maybeDiff returned false on the second call, want true", i)
		}

		if !slices.Equal(plans[i].SrcUIDs, wantUIDs[i]) {
			t.Errorf("slot %d: SrcUIDs = %v, want %v", i, plans[i].SrcUIDs, wantUIDs[i])
		}
		if held := reservedCounts(shared.reserved["X"]); !maps.Equal(held, wantHeld[i]) {
			t.Errorf("slot %d: reserved[X] counts = %v, want %v", i, held, wantHeld[i])
		}
		if _, ok := shared.reserved["Y"]; ok {
			t.Errorf("slot %d: reserved[Y] is set, want a destination with one mapping to stay out", i)
		}
	}

	if len(shared.reserved) != 0 {
		t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
	}
	if got := totalNew.Load(); got != 6 {
		t.Errorf("totalNew = %d, want 6", got)
	}
	// C and D hold standbys for the m1 of A, and D holds one for the m3 of C. Y has one mapping, and D is the last one.
	wantStandbys := [][]standbyView{
		{{planned: 1, folder: "C", uidValidity: 12, uid: 1}, {planned: 1, folder: "D", uidValidity: 13, uid: 1}},
		nil,
		{{planned: 2, folder: "D", uidValidity: 13, uid: 2}},
		nil,
	}
	for i, want := range wantStandbys {
		sources, copies := shared.standbysFor(i)
		if got := resolveStandbys(t, sources, copies); !slices.Equal(got, want) {
			t.Errorf("slot %d: standbys = %+v, want %+v", i, got, want)
		}
	}
}

func Test_lastIndex_When_ReceiverIsNil_Then_ReportsNoSharedDestination(t *testing.T) {
	t.Parallel()

	var shared *sharedDestinations

	if i, ok := shared.lastIndex("X"); ok || i != 0 {
		t.Errorf("lastIndex on a nil set = %d, %t, want 0, false", i, ok)
	}
}

func Test_buildSyncPlan_When_SharedDestinationScansFinishInAnyOrder_Then_PlansAreIdentical(t *testing.T) {
	t.Parallel()

	const lag = 120 * time.Millisecond
	// The lag fields count scan commands from 1, and 0 adds no lag. Y does not exist, so destination scans 1, 2 and 3 serve slots 0, 2 and 3.
	tests := []struct {
		name     string
		srcLagAt int
		dstLagAt int
	}{
		{name: "no delay"},
		{name: "first source scan is slow", srcLagAt: 1},
		{name: "first destination scan is slow", dstLagAt: 1},
		{name: "third source scan is slow", srcLagAt: 3},
		{name: "second destination scan is slow", dstLagAt: 2},
		{name: "last destination scan is slow", dstLagAt: 3},
		{name: "first source and last destination scans are slow", srcLagAt: 1, dstLagAt: 3},
	}
	type planView struct {
		src, dst string
		uids     []uint32
		standbys []standbyView
	}
	want := []planView{
		{src: "A", dst: "X", uids: []uint32{1, 2}, standbys: []standbyView{
			{planned: 1, folder: "C", uidValidity: 13, uid: 1},
			{planned: 1, folder: "D", uidValidity: 14, uid: 1},
		}},
		{src: "B", dst: "Y", uids: []uint32{1, 2}},
		{src: "C", dst: "X", uids: []uint32{2}, standbys: []standbyView{{planned: 2, folder: "D", uidValidity: 14, uid: 2}}},
		{src: "D", dst: "X", uids: []uint32{3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m9@x"),
				"C": fakeMsgs("m1@x", "m3@x"), "D": fakeMsgs("m1@x", "m3@x", "m4@x"),
			})
			for i, folder := range []string{"A", "B", "C", "D"} {
				src.setUIDValidity(folder, uint32(11+i))
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			lagOnNth := func(srv *fakeServer, nth int, verbs ...string) {
				if nth == 0 {
					return
				}
				var mu sync.Mutex
				var seen int
				for _, verb := range verbs {
					srv.onCommand(verb, func() {
						mu.Lock()
						seen++
						hit := seen == nth
						mu.Unlock()
						if hit {
							time.Sleep(lag)
						}
					})
				}
			}
			lagOnNth(src, tt.srcLagAt, "EXAMINE")
			// Slots 2 and 3 find X selected, so their scans send STATUS.
			lagOnNth(dst, tt.dstLagAt, "EXAMINE", "STATUS")
			srcTr, dstTr := makePlanTrackers()
			mappings := []config.DirectoryMapping{mapping("A", "X"), mapping("B", "Y"), mapping("C", "X"), mapping("D", "X")}

			summary, err := buildSyncPlan(e2eContext(t), newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), mappings, srcTr, dstTr, "src", "dst")

			if err != nil {
				t.Fatalf("buildSyncPlan: %v", err)
			}
			got := make([]planView, len(summary.Plans))
			for i, p := range summary.Plans {
				got[i] = planView{src: p.SourceFolder, dst: p.DestinationFolder, uids: p.SrcUIDs, standbys: resolveStandbys(t, p.standbySources, p.standbys)}
			}
			if !slices.EqualFunc(got, want, func(a, b planView) bool {
				return a.src == b.src && a.dst == b.dst && slices.Equal(a.uids, b.uids) && slices.Equal(a.standbys, b.standbys)
			}) {
				t.Errorf("plans = %+v, want %+v", got, want)
			}
			if summary.TotalNew != 6 {
				t.Errorf("TotalNew = %d, want 6", summary.TotalNew)
			}
		})
	}
}

func TestRunSync_When_TheLastSharedMappingFailsItsScan_Then_EarlierPlansStillCopy(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
	src.refuseOpenWith("B", "NO [CANNOT] folder gone")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("A", "X"), mapping("B", "X"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(),
		folderFailuresHeader,
		"• B → X: cannot scan source folder: ",
		"• A → X will copy 2 messages",
		"1 folders failed",
	)
	assertContainsNone(t, out.String(), "B → X will copy")
	assertIDs(t, "dst X", dst.folderMsgIDs("X"), "m1@x", "m2@x")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "X")
}

func TestRunSync_When_TheFirstSharedMappingFailsAtCopyTime_Then_TheStandbyCopiesTheSharedMessageAndTheNextRunCopiesTheRest(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("A", "X"), mapping("B", "X"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.rebuildFolder("A", 2, 1, 0) }}

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first runSync error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(),
		"• A → X will copy 2 messages",
		"• B → X will copy 1 messages",
		"2 messages uploaded, 1 errors occurred",
		"source folder changed after the sync plan, run sync again",
	)
	assertIDs(t, "dst X after the first run", dst.folderMsgIDs("X"), "m1@x", "m3@x")

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "Sync completed successfully")
	assertIDs(t, "dst X after the second run", slices.Sorted(slices.Values(dst.folderMsgIDs("X"))), "m1@x", "m2@x", "m3@x")
	assertIDs(t, "dst APPENDs over both runs", slices.Sorted(slices.Values(dst.appended("X"))), "m1@x", "m2@x", "m3@x")
	assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_CreateReportsExistingEmptyFolderAndSourceIsRebuilt_Then_TheStreamStopsBeforeAnyAppend(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": nil})
	dst.hideFromList("Work")
	var once sync.Once
	dst.onCommand("CREATE", func() { once.Do(func() { src.rebuildFolderFrom("Work", 2, 10, 2, 0, 1) }) })
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first runSync error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), "0 messages uploaded, 3 errors occurred", "source folder changed after the sync plan, run sync again")
	assertContainsNone(t, out.String(), "Sync completed successfully", "All folders already synced!")
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"))
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("destination saw %d APPEND commands, want 0", n)
	}
	if n := src.callCount("FETCH"); n != 1 {
		t.Errorf("source saw %d Message-Id scans, want 1: an empty destination needs no source rescan, so only the stream can catch the change", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "Sync completed successfully")
	assertIDs(t, "dst Work after the second run", slices.Sorted(slices.Values(dst.folderMsgIDs("Work"))), "m1@x", "m2@x", "m3@x")
	assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
	if n := dst.callCount("APPEND"); n != 3 {
		t.Errorf("destination saw %d APPEND commands over three runs, want 3", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_OneOfTwoSourceFoldersIsRebuiltAfterThePlan_Then_OnlyThatFolderFails(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("w1@x", "w2@x"), "Other": fakeMsgs("o1@x", "o2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 2, mapping("Work", "Work"), mapping("Other", "Other"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.rebuildFolder("Work", 2, 1, 0) }}

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first runSync error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, out.String(), "2 messages uploaded, 2 errors occurred, 0 folders failed")
	assertIDs(t, "dst Other", dst.folderMsgIDs("Other"), "o1@x", "o2@x")
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"))
	if n := strings.Count(out.String(), "source folder changed after the sync plan"); n != 1 {
		t.Errorf("output names the changed source %d times, want 1\noutput: %q", n, out.String())
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "• Work → Work will copy 2 messages")
	assertContainsNone(t, second, "Other → Other")
	assertIDs(t, "dst Work after the second run", slices.Sorted(slices.Values(dst.folderMsgIDs("Work"))), "w1@x", "w2@x")
	if n := len(dst.appended("Other")); n != 2 {
		t.Errorf("destination saw %d APPENDs to Other over both runs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_RebuiltFolderNameCarriesControlCharacters_Then_FailureLinesShowEscapesAndWireNamesStayExact(t *testing.T) {
	t.Parallel()

	const reason = "source folder changed after the sync plan, run sync again (UIDVALIDITY 1, now 2)"
	tests := []struct {
		dstFolder map[string][]fakeMsg
		name      string
		wantLines []string
		hidden    bool
	}{
		{
			name:      "the stream catches the change",
			dstFolder: map[string][]fakeMsg{"INBOX": nil},
			wantLines: []string{"Stream error for folder " + hostileShownName + ": ", "folder " + hostileShownName + ": " + reason},
		},
		{
			name:      "the recheck after CREATE catches the change",
			dstFolder: map[string][]fakeMsg{"INBOX": nil, hostileWireName: fakeMsgs("m1@x", "m2@x")},
			hidden:    true,
			wantLines: []string{"• " + hostileShownName + " → " + hostileShownName + ": " + reason},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, hostileWireName: fakeMsgs("m1@x", "m2@x", "m3@x")})
			dst := newStoreServer(t, "/", tt.dstFolder)
			if tt.hidden {
				dst.hideFromList(hostileWireName)
				var once sync.Once
				dst.onCommand("CREATE", func() { once.Do(func() { src.rebuildFolderFrom(hostileWireName, 2, 10, 2, 0, 1) }) })
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			if tt.hidden {
				opts.autoConfirm = true
			} else {
				opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.rebuildFolder(hostileWireName, 2, 0, 1, 2) }}
			}

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("runSync error = %v, want ErrSilentExit", err)
			}
			assertContainsAll(t, out.String(), tt.wantLines...)
			assertContainsNone(t, out.String(), hostileName, "\u202e", "\x1b[2J", "\rx")
			for _, name := range append(src.capturedNames("EXAMINE"), dst.capturedNames("CREATE")...) {
				if name != "INBOX" && name != hostileWireName {
					t.Errorf("a command carried %q, want only the exact wire names %q and INBOX", name, hostileWireName)
				}
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("destination saw %d APPEND commands, want 0", n)
			}
		})
	}
}

func TestRunSync_When_SourceBodyStaysEmpty_Then_EveryRunFailsLoudAndTheDestinationNeverGrows(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("good@x", "empty@x")})
	src.answerBody("empty@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))

	for run, wantUploaded := range []int{1, 0, 0} {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm, opts.verbose = true, true

		err := runSync(e2eContext(t), cfg, opts)

		if !errors.Is(err, ErrSilentExit) {
			t.Fatalf("run %d error = %v, want ErrSilentExit", run+1, err)
		}
		assertContainsAll(t, out.String(),
			fmt.Sprintf("%d messages uploaded, 1 errors occurred, 0 folders failed", wantUploaded),
			"source returned an empty body",
		)
		assertContainsNone(t, out.String(), "without Message-Id", "Sync completed successfully")
		assertIDs(t, fmt.Sprintf("dst INBOX after run %d", run+1), dst.folderMsgIDs("INBOX"), "good@x")
		if n := dst.callCount("APPEND"); n != 1 {
			t.Errorf("destination saw %d APPEND commands after run %d, want 1", n, run+1)
		}
	}
}

func Test_destinationDiff_When_CountsAndPlanVary_Then_ResultIsASubsetOfThePlan(t *testing.T) {
	t.Parallel()

	ids := []string{"a@x", "b@x", "c@x", "d@x"}
	rng := rand.New(rand.NewPCG(7, 11))
	for round := range 500 {
		srcMap := make(map[string][]client.MessageRef)
		sizeOf := make(map[uint32]uint32)
		var nextUID uint32
		for _, id := range ids {
			for range rng.IntN(5) {
				nextUID += uint32(1 + rng.IntN(3))
				size := uint32(100 * (1 + rng.IntN(3)))
				srcMap[id] = append(srcMap[id], client.MessageRef{UID: nextUID, Size: size})
				sizeOf[nextUID] = size
			}
		}
		dstMap := make(map[string][]client.MessageRef)
		dstSizes := make(map[string][]uint32)
		for _, id := range ids {
			for i := range rng.IntN(6) {
				size := uint32(100 * (1 + rng.IntN(3)))
				dstMap[id] = append(dstMap[id], client.MessageRef{UID: uint32(900 + i), Size: size})
				dstSizes[id] = append(dstSizes[id], size)
			}
		}
		// The plan holds a random share of the source UIDs and some UIDs that the source never had.
		var planned []uint32
		for uid := uint32(1); uid <= nextUID+3; uid++ {
			if _, inSource := sizeOf[uid]; rng.IntN(3) > 0 && (inSource || uid > nextUID) {
				planned = append(planned, uid)
			}
		}
		plan := FolderSyncPlan{SourceFolder: "S", DestinationFolder: "D", SrcUIDs: planned, NewMessages: len(planned)}
		before := slices.Clone(planned)

		got, drops := rediffOnePlan(plan, srcMap, dstMap)

		if !slices.Equal(plan.SrcUIDs, before) {
			t.Fatalf("round %d: destinationDiff changed the slice of the plan: %v, was %v", round, plan.SrcUIDs, before)
		}
		fresh := make(map[uint32]bool)
		for id, refs := range srcMap {
			for i, owner := range referenceOwners(refs, dstSizes[id]) {
				if owner < 0 {
					fresh[refs[i].UID] = true
				}
			}
		}
		var wantKept []uint32
		var wantSize uint64
		var wantHeld, wantGone int
		for _, uid := range planned {
			_, inSource := sizeOf[uid]
			switch {
			case fresh[uid]:
				wantKept = append(wantKept, uid)
				wantSize += uint64(sizeOf[uid])
			case inSource:
				wantHeld++
			default:
				wantGone++
			}
		}
		if !slices.Equal(got.SrcUIDs, wantKept) {
			t.Fatalf("round %d: SrcUIDs = %v, want %v\nsource %v\ndestination %v\nplan %v", round, got.SrcUIDs, wantKept, srcMap, dstMap, planned)
		}
		for _, uid := range got.SrcUIDs {
			if !slices.Contains(planned, uid) {
				t.Fatalf("round %d: result holds UID %d that the plan did not hold", round, uid)
			}
		}
		if got.NewMessages != len(wantKept) || got.NewSize != wantSize {
			t.Fatalf("round %d: NewMessages, NewSize = %d, %d, want %d, %d", round, got.NewMessages, got.NewSize, len(wantKept), wantSize)
		}
		if drops.held != wantHeld || drops.gone != wantGone {
			t.Fatalf("round %d: held, gone = %d, %d, want %d, %d (held: planned and already on the destination, gone: planned and not in the source)", round, drops.held, drops.gone, wantHeld, wantGone)
		}
		if drops.held+drops.gone+len(got.SrcUIDs) != len(planned) {
			t.Fatalf("round %d: held %d + gone %d + kept %d != planned %d", round, drops.held, drops.gone, len(got.SrcUIDs), len(planned))
		}
	}
}

func TestRunSync_When_SharedDestinationNameCarriesControlCharacters_Then_PreviewListsItOnceEscapedAndServerGetsOneExactCreate(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("A", hostileName), mapping("B", hostileName)), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got,
		"• A → "+hostileShownName+" will copy 2 messages",
		"• B → "+hostileShownName+" will copy 1 messages",
		"Folders to be created on destination:\n• "+hostileShownName+"\n\n",
	)
	if n := strings.Count(got, "• "+hostileShownName+"\n"); n != 1 {
		t.Errorf("output lists the shared folder %d times under the folders to create, want 1\noutput: %q", n, got)
	}
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\rx")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), hostileWireName)
	assertIDs(t, "dst folder", slices.Sorted(slices.Values(dst.folderMsgIDs(hostileWireName))), "m1@x", "m2@x", "m3@x")
}

func TestRunSync_When_TheFirstSharedMappingHasAnEmptyBody_Then_TheLaterMappingsCopyArrivesInTheSameRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workers   int
		dstHasXIn bool
	}{
		{name: "one worker and a missing destination", workers: 1},
		{name: "two workers and a missing destination", workers: 2},
		{name: "one worker and an existing destination", workers: 1, dstHasXIn: true},
		{name: "two workers and an existing destination", workers: 2, dstHasXIn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bodyOfB := subjectBody("m1@x", "from B")
			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("m1@x", "m2@x"),
				"B": {{msgID: "m1@x", body: bodyOfB}, {msgID: "m3@x"}},
			})
			src.answerBodyIn("A", "m1@x", "NIL")
			dstFolders := map[string][]fakeMsg{"INBOX": nil}
			if tt.dstHasXIn {
				dstFolders["X"] = nil
			}
			dst := newStoreServer(t, "/", dstFolders)
			cfg := e2eConfig(src, dst, tt.workers, mapping("A", "X"), mapping("B", "X"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), cfg, opts)

			if err != nil {
				t.Fatalf("first runSync error = %v, want nil\noutput: %s", err, out.String())
			}
			assertContainsAll(t, out.String(),
				"• A → X will copy 2 messages",
				"• B → X will copy 1 messages",
				"Total new messages to sync: 3",
				"Sync completed successfully",
			)
			assertContainsNone(t, out.String(), "errors occurred", "empty body")
			assertIDs(t, "dst X", slices.Sorted(slices.Values(dst.folderMsgIDs("X"))), "m1@x", "m2@x", "m3@x")
			if !slices.Contains(dst.folderBodies("X"), bodyOfB) {
				t.Errorf("dst X bodies = %q, want the copy of m1 from B %q", dst.folderBodies("X"), bodyOfB)
			}
			if n := dst.callCount("APPEND"); n != 3 {
				t.Errorf("destination saw %d APPEND commands, want 3", n)
			}

			assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != 3 {
				t.Errorf("destination saw %d APPEND commands after the second run, want 3", n)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

// diffBothSides plays the two scan goroutines: the first call counts one side done, the second one runs the diff.
func diffBothSides(t *testing.T, s *folderScan, idx int, mappings []config.DirectoryMapping, plans []FolderSyncPlan, totalNew *atomic.Int64, totalNewSize *atomic.Uint64, shared *sharedDestinations) {
	t.Helper()

	maybeDiff(s, idx, mappings, plans, totalNew, totalNewSize, shared, copyByCount)
	if !maybeDiff(s, idx, mappings, plans, totalNew, totalNewSize, shared, copyByCount) {
		t.Fatalf("slot %d: maybeDiff returned false on the second call, want true", idx)
	}
}

// standbyView is a standby with its source folder resolved, so tests compare plans without the source indexes.
type standbyView struct {
	folder      string
	planned     uint32
	uidValidity uint32
	uid         uint32
}

// resolveStandbys fails the test when a source folder appears twice or an index points outside the sources.
func resolveStandbys(t *testing.T, sources []standbySource, copies []standbyCopy) []standbyView {
	t.Helper()

	seen := make(map[string]bool, len(sources))
	for _, src := range sources {
		if seen[src.folder] {
			t.Errorf("standby sources %+v hold %q twice, want each folder once", sources, src.folder)
		}
		seen[src.folder] = true
	}
	var views []standbyView
	for _, c := range copies {
		if int(c.source) >= len(sources) {
			t.Fatalf("standby %+v points at source %d, but there are %d sources", c, c.source, len(sources))
		}
		src := sources[c.source]
		views = append(views, standbyView{folder: src.folder, planned: c.planned, uidValidity: src.uidValidity, uid: c.uid})
	}
	return views
}

// mergedStandbys reads the standbys of a slot through the merge that buildSyncPlan applies. It merges into a copy of plan.
func mergedStandbys(t *testing.T, plan FolderSyncPlan, shared *sharedDestinations, slot int) []standbyView {
	t.Helper()

	plan.mergeStandbys(shared.standbysFor(slot))
	return resolveStandbys(t, plan.standbySources, plan.standbys)
}

func Test_maybeDiff_When_SharedDestinationHasStandbyCopies_Then_EachPlannedCopyGetsThemInMapOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sources      []string
		refs         [][]uint32
		wantUIDs     [][]uint32
		wantStandbys [][]standbyView
		have         int
	}{
		{
			name:     "none of the copies is on the destination",
			sources:  []string{"A", "B", "C"},
			refs:     [][]uint32{{1}, {11, 12}, {21, 22, 23}},
			wantUIDs: [][]uint32{{1}, {12}, {23}},
			wantStandbys: [][]standbyView{
				{{planned: 1, folder: "B", uidValidity: 101, uid: 11}, {planned: 1, folder: "C", uidValidity: 102, uid: 21}},
				{{planned: 12, folder: "C", uidValidity: 102, uid: 22}},
				nil,
			},
		},
		{
			name:     "one copy is on the destination",
			have:     1,
			sources:  []string{"A", "B", "C"},
			refs:     [][]uint32{{1}, {11, 12}, {21, 22, 23}},
			wantUIDs: [][]uint32{nil, {12}, {23}},
			wantStandbys: [][]standbyView{
				nil,
				{{planned: 12, folder: "C", uidValidity: 102, uid: 22}},
				nil,
			},
		},
		{
			name:     "two mappings share a source folder next to a third one",
			sources:  []string{"S", "S", "C"},
			refs:     [][]uint32{{1}, {11, 12}, {21, 22, 23}},
			wantUIDs: [][]uint32{{1}, {12}, {23}},
			wantStandbys: [][]standbyView{
				{{planned: 1, folder: "C", uidValidity: 102, uid: 21}},
				{{planned: 12, folder: "C", uidValidity: 102, uid: 22}},
				nil,
			},
		},
		{
			name:         "only two mappings and they share a source folder",
			sources:      []string{"S", "S"},
			refs:         [][]uint32{{1}, {11}},
			wantUIDs:     [][]uint32{{1}, nil},
			wantStandbys: [][]standbyView{nil, nil},
		},
		{
			name:         "the later mapping has no copy",
			sources:      []string{"A", "B"},
			refs:         [][]uint32{{1}, nil},
			wantUIDs:     [][]uint32{{1}, nil},
			wantStandbys: [][]standbyView{nil, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := make([]config.DirectoryMapping, len(tt.sources))
			for i, source := range tt.sources {
				mappings[i] = mapping(source, "X")
			}
			shared := newSharedDestinations(mappings)
			plans := make([]FolderSyncPlan, len(mappings))
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64

			for i := range mappings {
				srcMap := map[string][]client.MessageRef{}
				if len(tt.refs[i]) > 0 {
					srcMap["m@x"] = msgRefs(tt.refs[i]...)
				}
				dstMap := map[string][]client.MessageRef{}
				if tt.have > 0 {
					dstMap["m@x"] = msgRefs(make([]uint32, tt.have)...)
				}
				s := &folderScan{srcMap: srcMap, dstMap: dstMap, srcMessages: countRefs(srcMap), srcUIDValidity: uint32(100 + i), dstExists: true}
				diffBothSides(t, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
			}

			for i := range mappings {
				if !slices.Equal(plans[i].SrcUIDs, tt.wantUIDs[i]) {
					t.Errorf("slot %d: SrcUIDs = %v, want %v", i, plans[i].SrcUIDs, tt.wantUIDs[i])
				}
				sources, copies := shared.standbysFor(i)
				if got := resolveStandbys(t, sources, copies); !slices.Equal(got, tt.wantStandbys[i]) {
					t.Errorf("slot %d: standbys = %+v, want %+v", i, got, tt.wantStandbys[i])
				}
			}
		})
	}
}

func Test_maybeDiff_When_SharedCopiesDifferInSize_Then_SizesPairReservationsAndStandbys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		srcs         [][]client.MessageRef
		dst          []client.MessageRef
		wantUIDs     [][]uint32
		wantSize     []uint64
		wantStandbys [][]standbyView
	}{
		{
			name:     "the later source holds the larger copy first",
			srcs:     [][]client.MessageRef{{{UID: 10, Size: 100}}, {{UID: 1, Size: 200}, {UID: 2, Size: 100}}},
			wantUIDs: [][]uint32{{10}, {1}},
			wantSize: []uint64{100, 200},
			wantStandbys: [][]standbyView{
				{{planned: 10, folder: "B", uidValidity: 101, uid: 2}},
				nil,
			},
		},
		{
			name:     "every copy of the later source stands by for the copy of the same size",
			srcs:     [][]client.MessageRef{{{UID: 1, Size: 100}, {UID: 2, Size: 200}}, {{UID: 11, Size: 200}, {UID: 12, Size: 100}}},
			wantUIDs: [][]uint32{{1, 2}, nil},
			wantSize: []uint64{300, 0},
			wantStandbys: [][]standbyView{
				{{planned: 1, folder: "B", uidValidity: 101, uid: 12}, {planned: 2, folder: "B", uidValidity: 101, uid: 11}},
				nil,
			},
		},
		{
			name:         "the destination copy has the size of the later copy of the later source",
			srcs:         [][]client.MessageRef{{{UID: 1, Size: 100}}, {{UID: 11, Size: 100}, {UID: 12, Size: 200}}},
			dst:          []client.MessageRef{{UID: 90, Size: 200}},
			wantUIDs:     [][]uint32{nil, {11}},
			wantSize:     []uint64{0, 100},
			wantStandbys: [][]standbyView{nil, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{mapping("A", "X"), mapping("B", "X")}
			shared := newSharedDestinations(mappings)
			plans := make([]FolderSyncPlan, len(mappings))
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64

			for i, refs := range tt.srcs {
				srcMap := map[string][]client.MessageRef{"m@x": slices.Clone(refs)}
				dstMap := map[string][]client.MessageRef{}
				if len(tt.dst) > 0 {
					dstMap["m@x"] = slices.Clone(tt.dst)
				}
				s := &folderScan{srcMap: srcMap, dstMap: dstMap, srcMessages: len(refs), srcUIDValidity: uint32(100 + i), dstExists: true}
				diffBothSides(t, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
			}

			for i := range mappings {
				if !slices.Equal(plans[i].SrcUIDs, tt.wantUIDs[i]) || plans[i].NewSize != tt.wantSize[i] {
					t.Errorf("slot %d: SrcUIDs, NewSize = %v, %d, want %v, %d", i, plans[i].SrcUIDs, plans[i].NewSize, tt.wantUIDs[i], tt.wantSize[i])
				}
				sources, copies := shared.standbysFor(i)
				if got := resolveStandbys(t, sources, copies); !slices.Equal(got, tt.wantStandbys[i]) {
					t.Errorf("slot %d: standbys = %+v, want %+v", i, got, tt.wantStandbys[i])
				}
			}
			if len(shared.reserved) != 0 {
				t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
			}
		})
	}
}

func Test_maybeDiff_When_SharedDestinationCountsVary_Then_StandbysFollowTheReservations(t *testing.T) {
	t.Parallel()

	ids := []string{"a@x", "b@x", "c@x"}
	rng := rand.New(rand.NewPCG(13, 17))
	for round := range 500 {
		sharing := 2 + rng.IntN(3)
		otherPos := rng.IntN(sharing + 1)
		slots := sharing + 1
		mappings := make([]config.DirectoryMapping, slots)
		srcMaps := make([]map[string][]client.MessageRef, slots)
		idOf := make([]map[uint32]string, slots)
		sizeOf := make([]map[uint32]uint32, slots)
		slotOfFolder := make(map[string]int, slots)
		haveSizes := make(map[string][]uint32, len(ids))
		for _, id := range ids {
			for range rng.IntN(4) {
				haveSizes[id] = append(haveSizes[id], uint32(100*(1+rng.IntN(3))))
			}
		}
		for i := range slots {
			destination := "X"
			if i == otherPos {
				destination = "Y"
			}
			mappings[i] = mapping("S"+strconv.Itoa(i), destination)
			slotOfFolder[mappings[i].Source] = i
			srcMaps[i] = make(map[string][]client.MessageRef)
			idOf[i] = make(map[uint32]string)
			sizeOf[i] = make(map[uint32]uint32)
			var nextUID uint32
			for _, id := range ids {
				for range rng.IntN(4) {
					nextUID += uint32(1 + rng.IntN(3))
					size := uint32(100 * (1 + rng.IntN(3)))
					srcMaps[i][id] = append(srcMaps[i][id], client.MessageRef{UID: nextUID, Size: size})
					idOf[i][nextUID] = id
					sizeOf[i][nextUID] = size
				}
			}
		}
		shared := newSharedDestinations(mappings)
		plans := make([]FolderSyncPlan, slots)
		var totalNew atomic.Int64
		var totalNewSize atomic.Uint64

		for i := range slots {
			dstMap := map[string][]client.MessageRef{}
			if mappings[i].Destination == "X" {
				for id, sizes := range haveSizes {
					for k, size := range sizes {
						dstMap[id] = append(dstMap[id], client.MessageRef{UID: uint32(900 + k), Size: size})
					}
				}
			}
			s := &folderScan{srcMap: maps.Clone(srcMaps[i]), dstMap: dstMap, srcMessages: countRefs(srcMaps[i]), srcUIDValidity: uint32(50 + i), dstExists: true}
			diffBothSides(t, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
		}

		// The reference: destination copies account for refs first, then earlier reservations pair with the rest. A plan that is not last in X reserves what stays missing.
		type copyAt struct {
			slot int
			uid  uint32
		}
		type pairing struct {
			planned copyAt
			standby copyAt
		}
		reservedRefs := make(map[string][]copyAt, len(ids))
		wantPairs := make(map[pairing]bool)
		lastX := slots - 1
		if otherPos == slots-1 {
			lastX = slots - 2
		}
		for i := range slots {
			var wantUIDs []uint32
			var wantSize uint64
			for id, refs := range srcMaps[i] {
				var held []copyAt
				var dstSizes, heldSizes []uint32
				if mappings[i].Destination == "X" {
					held = reservedRefs[id]
					dstSizes = haveSizes[id]
					for _, h := range held {
						heldSizes = append(heldSizes, sizeOf[h.slot][h.uid])
					}
				}
				var left []client.MessageRef
				for k, owner := range referenceOwners(refs, dstSizes) {
					if owner < 0 {
						left = append(left, refs[k])
					}
				}
				for k, owner := range referenceOwners(left, heldSizes) {
					ref := left[k]
					if owner >= 0 {
						wantPairs[pairing{planned: held[owner], standby: copyAt{i, ref.UID}}] = true
						continue
					}
					if mappings[i].Destination == "X" && i != lastX {
						reservedRefs[id] = append(reservedRefs[id], copyAt{i, ref.UID})
					}
					wantUIDs = append(wantUIDs, ref.UID)
					wantSize += uint64(ref.Size)
				}
			}
			slices.Sort(wantUIDs)
			if !slices.Equal(plans[i].SrcUIDs, wantUIDs) || plans[i].NewSize != wantSize {
				t.Fatalf("round %d slot %d: SrcUIDs, NewSize = %v, %d, want %v, %d (the reference)", round, i, plans[i].SrcUIDs, plans[i].NewSize, wantUIDs, wantSize)
			}
		}
		gotPairs := make(map[pairing]bool, len(wantPairs))

		type slotUID struct {
			slot int
			uid  uint32
		}
		used := make(map[slotUID]uint32)
		for r := range slots {
			sources, copies := shared.standbysFor(r)
			if mappings[r].Destination == "Y" && len(copies) > 0 {
				t.Fatalf("round %d slot %d: standbys %+v on a destination with one mapping, want none", round, r, copies)
			}
			for k, c := range copies {
				src := sources[c.source]
				s := slotOfFolder[src.folder]
				switch {
				case s <= r:
					t.Fatalf("round %d: the standby for slot %d comes from slot %d, want a later slot", round, r, s)
				case src.uidValidity != uint32(50+s):
					t.Fatalf("round %d: standby UIDVALIDITY = %d, want %d", round, src.uidValidity, 50+s)
				case !slices.Contains(plans[r].SrcUIDs, c.planned):
					t.Fatalf("round %d: planned UID %d of slot %d is not in its plan %v", round, c.planned, r, plans[r].SrcUIDs)
				case idOf[r][c.planned] != idOf[s][c.uid] || idOf[s][c.uid] == "":
					t.Fatalf("round %d: standby UID %d of slot %d has Message-Id %q, want %q of UID %d in slot %d", round, c.uid, s, idOf[s][c.uid], idOf[r][c.planned], c.planned, r)
				case slices.Contains(plans[s].SrcUIDs, c.uid):
					t.Fatalf("round %d: standby UID %d of slot %d is also in its own plan %v", round, c.uid, s, plans[s].SrcUIDs)
				}
				if owner, taken := used[slotUID{s, c.uid}]; taken {
					t.Fatalf("round %d: standby UID %d of slot %d serves planned UIDs %d and %d", round, c.uid, s, owner, c.planned)
				}
				used[slotUID{s, c.uid}] = c.planned
				gotPairs[pairing{planned: copyAt{r, c.planned}, standby: copyAt{s, c.uid}}] = true
				if k > 0 {
					prev := copies[k-1]
					if prev.planned > c.planned || (prev.planned == c.planned && prev.source >= c.source) {
						t.Fatalf("round %d slot %d: standbys %+v are not sorted by planned UID and increasing source", round, r, copies)
					}
				}
			}
		}
		if !maps.Equal(gotPairs, wantPairs) {
			t.Fatalf("round %d: standbys = %v, want %v (the reference pairing)", round, gotPairs, wantPairs)
		}
		if len(shared.reserved) != 0 {
			t.Fatalf("round %d: reserved = %v after the last slot, want none", round, shared.reserved)
		}
	}
}

func Test_standbysFor_When_ReceiverIsNilOrPlanHasNoStandbys_Then_ReturnsNothing(t *testing.T) {
	t.Parallel()

	var none *sharedDestinations
	if sources, copies := none.standbysFor(0); sources != nil || copies != nil {
		t.Errorf("standbysFor on a nil set = %v, %v, want nil, nil", sources, copies)
	}
	shared := newSharedDestinations([]config.DirectoryMapping{mapping("A", "X"), mapping("B", "X")})
	if sources, copies := shared.standbysFor(1); sources != nil || copies != nil {
		t.Errorf("standbysFor for a slot without standbys = %v, %v, want nil, nil", sources, copies)
	}
}

func TestFolderSyncPlan_standbysOf(t *testing.T) {
	t.Parallel()

	plan := FolderSyncPlan{standbys: []standbyCopy{
		{planned: 2, uid: 20, source: 0}, {planned: 2, uid: 21, source: 1}, {planned: 5, uid: 50, source: 1}, {planned: 9, uid: 90, source: 0},
	}}
	tests := []struct {
		name    string
		want    []standbyCopy
		planned uint32
	}{
		{name: "first planned UID with two sources", planned: 2, want: plan.standbys[:2]},
		{name: "middle planned UID", planned: 5, want: plan.standbys[2:3]},
		{name: "last planned UID", planned: 9, want: plan.standbys[3:]},
		{name: "UID below all", planned: 1},
		{name: "UID between two", planned: 6},
		{name: "UID above all", planned: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := plan.standbysOf(tt.planned); !slices.Equal(got, tt.want) {
				t.Errorf("standbysOf(%d) = %+v, want %+v", tt.planned, got, tt.want)
			}
		})
	}
	if got := (&FolderSyncPlan{}).standbysOf(1); len(got) != 0 {
		t.Errorf("standbysOf on a plan without standbys = %+v, want none", got)
	}
}

func TestFolderSyncPlan_standbysOf_When_Called_Then_DoesNotAllocate(t *testing.T) {
	plan := FolderSyncPlan{standbys: []standbyCopy{{planned: 2, uid: 20}, {planned: 2, uid: 21, source: 1}, {planned: 5, uid: 50}}}

	if n := testing.AllocsPerRun(100, func() { _ = plan.standbysOf(2) }); n != 0 {
		t.Errorf("standbysOf allocates %v times per call, want 0", n)
	}
}

func Test_buildSyncPlan_When_DestinationIsNotSharedOrSharesNoMessageId_Then_PlansHaveNoStandbys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "every destination has one mapping", mappings: []config.DirectoryMapping{mapping("A", "X"), mapping("B", "Y")}},
		{name: "a shared destination and no common Message-Id", mappings: []config.DirectoryMapping{mapping("A", "X"), mapping("C", "X")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x"), "C": fakeMsgs("m3@x"),
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": nil})
			srcTr, dstTr := makePlanTrackers()

			summary, err := buildSyncPlan(e2eContext(t), newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), tt.mappings, srcTr, dstTr, "src", "dst")

			if err != nil {
				t.Fatalf("buildSyncPlan: %v", err)
			}
			if len(summary.Plans) != 2 {
				t.Fatalf("plans = %+v, want 2", summary.Plans)
			}
			for _, p := range summary.Plans {
				if p.standbySources != nil || p.standbys != nil {
					t.Errorf("plan %s → %s has standbys %+v from %+v, want nil", p.SourceFolder, p.DestinationFolder, p.standbys, p.standbySources)
				}
			}
		})
	}
}

func TestRunSync_When_EverySharedCopyHasAnEmptyBody_Then_EachRunFailsAndNothingEmptyIsAppended(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m3@x")})
	src.answerBody("m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("A", "X"), mapping("B", "X"))

	for run, wantUploaded := range []int{2, 0} {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true

		err := runSync(e2eContext(t), cfg, opts)

		if !errors.Is(err, ErrSilentExit) {
			t.Fatalf("run %d error = %v, want ErrSilentExit", run+1, err)
		}
		assertContainsAll(t, out.String(), fmt.Sprintf("%d messages uploaded, 1 errors occurred, 0 folders failed", wantUploaded))
		assertContainsNone(t, out.String(), "Sync completed successfully")
		if slices.Contains(dst.folderMsgIDs("X"), "m1@x") || slices.Contains(dst.appended("X"), "m1@x") {
			t.Errorf("after run %d dst X holds %v and saw APPENDs %v, want no m1@x", run+1, dst.folderMsgIDs("X"), dst.appended("X"))
		}
		assertIDs(t, fmt.Sprintf("dst X after run %d", run+1), slices.Sorted(slices.Values(dst.folderMsgIDs("X"))), "m2@x", "m3@x")
	}
	if n := dst.callCount("APPEND"); n != 2 {
		t.Errorf("destination saw %d APPEND commands over both runs, want 2", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SharedDestinationIsRecheckedAndTheReservedCopyHasAnEmptyBody_Then_TheStandbyArrives(t *testing.T) {
	t.Parallel()

	bodyOfB := subjectBody("m1@x", "from B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": fakeMsgs("m1@x", "m2@x"),
		"B": {{msgID: "m1@x", body: bodyOfB}, {msgID: "m3@x"}},
	})
	src.answerBodyIn("A", "m1@x", "NIL")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "X": fakeMsgs("other@x")})
	dst.hideFromList("X")
	cfg := e2eConfig(src, dst, 2, mapping("A", "X"), mapping("B", "X"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), cfg, opts)

	if err != nil {
		t.Fatalf("runSync error = %v, want nil\noutput: %s", err, out.String())
	}
	assertContainsAll(t, out.String(), "Sync completed successfully")
	assertContainsNone(t, out.String(), "errors occurred")
	assertIDs(t, "dst X", slices.Sorted(slices.Values(dst.folderMsgIDs("X"))), "m1@x", "m2@x", "m3@x", "other@x")
	if !slices.Contains(dst.folderBodies("X"), bodyOfB) {
		t.Errorf("dst X bodies = %q, want the copy of m1 from B %q", dst.folderBodies("X"), bodyOfB)
	}
	if n := dst.callCount("APPEND"); n != 3 {
		t.Errorf("destination saw %d APPEND commands, want 3", n)
	}
	if n := dst.callCount("CREATE"); n != 1 {
		t.Errorf("destination saw %d CREATE commands, want 1: the recheck path needs the CREATE that reports the existing folder", n)
	}
	assertReadOnly(t, "source", src)
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func Test_copyRule_sourceCopies(t *testing.T) {
	three := []client.MessageRef{{UID: 4, Size: 10}, {UID: 5, Size: 20}, {UID: 6, Size: 30}}
	tests := []struct {
		name     string
		refs     []client.MessageRef
		wantLen  int
		rule     copyRule
		sameSlot bool
	}{
		{name: "copy by count keeps an empty group", rule: copyByCount, refs: nil, wantLen: 0},
		{name: "copy by count keeps one copy", rule: copyByCount, refs: three[:1], wantLen: 1, sameSlot: true},
		{name: "copy by count keeps three copies", rule: copyByCount, refs: three, wantLen: 3, sameSlot: true},
		{name: "one copy per Message-Id keeps an empty group", rule: copyOnePerMessageID, refs: nil, wantLen: 0},
		{name: "one copy per Message-Id keeps one copy", rule: copyOnePerMessageID, refs: three[:1], wantLen: 1, sameSlot: true},
		{name: "one copy per Message-Id cuts three copies to the lowest UID", rule: copyOnePerMessageID, refs: three, wantLen: 1, sameSlot: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.rule.sourceCopies(tt.refs)

			if len(got) != tt.wantLen {
				t.Fatalf("sourceCopies returned %d refs, want %d", len(got), tt.wantLen)
			}
			if tt.sameSlot && &got[0] != &tt.refs[0] {
				t.Errorf("sourceCopies = %v, want the first ref of the input", got)
			}
			if n := testing.AllocsPerRun(100, func() { tt.rule.sourceCopies(tt.refs) }); n != 0 {
				t.Errorf("sourceCopies allocates %v times per call, want 0", n)
			}
		})
	}
}

// planUnder runs one folder diff under rule and returns its plan and totals.
func planUnder(t *testing.T, rule copyRule, src, dst map[string][]client.MessageRef) (plan FolderSyncPlan, totalNew int64, totalNewSize uint64) {
	t.Helper()

	plans := make([]FolderSyncPlan, 1)
	var newMessages atomic.Int64
	var newSize atomic.Uint64
	mappings := []config.DirectoryMapping{mapping("S", "D")}
	s := &folderScan{srcMap: src, dstMap: dst, srcMessages: countRefs(src), dstExists: true}
	maybeDiff(s, 0, mappings, plans, &newMessages, &newSize, nil, rule)
	if !maybeDiff(s, 0, mappings, plans, &newMessages, &newSize, nil, rule) {
		t.Fatal("maybeDiff returned false on the second call, want true")
	}
	if s.srcMap != nil || s.dstMap != nil {
		t.Errorf("maps not freed: srcMap=%v dstMap=%v", s.srcMap, s.dstMap)
	}
	return plans[0], newMessages.Load(), newSize.Load()
}

func Test_maybeDiff_When_DestinationKeepsOneCopyPerMessageID_Then_PlansTheLowestUIDOnce(t *testing.T) {
	t.Parallel()

	three := []client.MessageRef{{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}
	tests := []struct {
		src          map[string][]client.MessageRef
		dst          map[string][]client.MessageRef
		name         string
		wantUIDs     []uint32
		wantSize     uint64
		wantRepeated int
	}{
		{
			name:         "three copies and none on the destination",
			src:          map[string][]client.MessageRef{"x@x": three},
			wantUIDs:     []uint32{1},
			wantSize:     10,
			wantRepeated: 2,
		},
		{
			name: "the destination holds one copy with the size of the last source copy",
			src:  map[string][]client.MessageRef{"x@x": three},
			dst:  map[string][]client.MessageRef{"x@x": {{UID: 90, Size: 30}}},
		},
		{
			name: "the destination holds five copies",
			src:  map[string][]client.MessageRef{"x@x": three},
			dst:  map[string][]client.MessageRef{"x@x": msgRefs(90, 91, 92, 93, 94)},
		},
		{
			name:         "three copies of one id and one of another, which the destination holds",
			src:          map[string][]client.MessageRef{"x@x": three, "y@x": {{UID: 4, Size: 5}}},
			dst:          map[string][]client.MessageRef{"y@x": {{UID: 90, Size: 5}}},
			wantUIDs:     []uint32{1},
			wantSize:     10,
			wantRepeated: 2,
		},
		{
			name:         "two ids with repeated copies add up",
			src:          map[string][]client.MessageRef{"x@x": three, "y@x": {{UID: 4, Size: 5}, {UID: 5, Size: 6}}},
			wantUIDs:     []uint32{1, 4},
			wantSize:     15,
			wantRepeated: 3,
		},
		{
			name:     "single copies only",
			src:      map[string][]client.MessageRef{"a@x": {{UID: 1, Size: 10}}, "b@x": {{UID: 2, Size: 20}}},
			dst:      map[string][]client.MessageRef{"b@x": {{UID: 90, Size: 20}}},
			wantUIDs: []uint32{1},
			wantSize: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan, totalNew, totalNewSize := planUnder(t, copyOnePerMessageID, tt.src, tt.dst)

			if len(tt.wantUIDs) == 0 {
				if plan.SourceFolder != "" || totalNew != 0 || totalNewSize != 0 {
					t.Errorf("plan = %+v, totals = %d, %d, want no plan", plan, totalNew, totalNewSize)
				}
				return
			}
			if !slices.Equal(plan.SrcUIDs, tt.wantUIDs) {
				t.Errorf("SrcUIDs = %v, want %v", plan.SrcUIDs, tt.wantUIDs)
			}
			if plan.NewMessages != len(tt.wantUIDs) || plan.NewSize != tt.wantSize {
				t.Errorf("NewMessages, NewSize = %d, %d, want %d, %d", plan.NewMessages, plan.NewSize, len(tt.wantUIDs), tt.wantSize)
			}
			if plan.RepeatedCopies != tt.wantRepeated {
				t.Errorf("RepeatedCopies = %d, want %d", plan.RepeatedCopies, tt.wantRepeated)
			}
			if plan.ExtraCopies != 0 {
				t.Errorf("ExtraCopies = %d, want 0: a destination that holds the id plans none of its copies", plan.ExtraCopies)
			}
			if plan.copyRule != copyOnePerMessageID {
				t.Errorf("copyRule = %d, want %d so the recheck applies the same rule", plan.copyRule, copyOnePerMessageID)
			}
			if totalNew != int64(len(tt.wantUIDs)) || totalNewSize != tt.wantSize {
				t.Errorf("totals = %d, %d, want %d, %d: only planned copies count", totalNew, totalNewSize, len(tt.wantUIDs), tt.wantSize)
			}
		})
	}
}

func Test_maybeDiff_When_DestinationCountsByCopies_Then_NothingIsLeftOut(t *testing.T) {
	t.Parallel()

	src := map[string][]client.MessageRef{"x@x": {{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}}

	plan, _, _ := planUnder(t, copyByCount, src, nil)

	if plan.RepeatedCopies != 0 || plan.copyRule != copyByCount || !slices.Equal(plan.SrcUIDs, []uint32{1, 2, 3}) {
		t.Errorf("plan = %+v, want all three UIDs, no repeated copies and the count rule", plan)
	}
}

// diffBothSidesUnder is diffBothSides for a destination that follows rule.
func diffBothSidesUnder(t *testing.T, rule copyRule, s *folderScan, idx int, mappings []config.DirectoryMapping, plans []FolderSyncPlan, totalNew *atomic.Int64, totalNewSize *atomic.Uint64, shared *sharedDestinations) {
	t.Helper()

	maybeDiff(s, idx, mappings, plans, totalNew, totalNewSize, shared, rule)
	if !maybeDiff(s, idx, mappings, plans, totalNew, totalNewSize, shared, rule) {
		t.Fatalf("slot %d: maybeDiff returned false on the second call, want true", idx)
	}
}

func Test_maybeDiff_When_OneCopyDestinationIsShared_Then_LaterMappingsGetStandbysNotPlans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		refs         [][]uint32
		wantUIDs     [][]uint32
		wantRepeated []int
		wantStandbys [][]standbyView
	}{
		{
			name:         "the first mapping holds two copies and the second holds one",
			refs:         [][]uint32{{1, 2}, {10}},
			wantUIDs:     [][]uint32{{1}, nil},
			wantRepeated: []int{1, 0},
			wantStandbys: [][]standbyView{
				{{planned: 1, folder: "A", uidValidity: 100, uid: 2}, {planned: 1, folder: "B", uidValidity: 101, uid: 10}},
				nil,
			},
		},
		{
			name:         "the first mapping holds none and the second holds two",
			refs:         [][]uint32{nil, {10, 11}},
			wantUIDs:     [][]uint32{nil, {10}},
			wantRepeated: []int{0, 1},
			wantStandbys: [][]standbyView{nil, {{planned: 10, folder: "B", uidValidity: 101, uid: 11}}},
		},
		{
			name:         "both mappings hold two copies",
			refs:         [][]uint32{{1, 2}, {10, 11}},
			wantUIDs:     [][]uint32{{1}, nil},
			wantRepeated: []int{1, 0},
			wantStandbys: [][]standbyView{
				{
					{planned: 1, folder: "A", uidValidity: 100, uid: 2},
					{planned: 1, folder: "B", uidValidity: 101, uid: 10},
					{planned: 1, folder: "B", uidValidity: 101, uid: 11},
				},
				nil,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{mapping("A", "D"), mapping("B", "D")}
			shared := newSharedDestinations(mappings)
			plans := make([]FolderSyncPlan, len(mappings))
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64

			for i := range mappings {
				srcMap := map[string][]client.MessageRef{}
				if len(tt.refs[i]) > 0 {
					srcMap["x@x"] = msgRefs(tt.refs[i]...)
				}
				s := &folderScan{srcMap: srcMap, srcMessages: countRefs(srcMap), srcUIDValidity: uint32(100 + i), dstExists: true}
				diffBothSidesUnder(t, copyOnePerMessageID, s, i, mappings, plans, &totalNew, &totalNewSize, shared)

				if i == 0 && len(shared.reserved["D"]["x@x"]) != len(tt.wantUIDs[0]) {
					t.Errorf("after slot 0, reserved copies of x = %v, want %d", shared.reserved["D"]["x@x"], len(tt.wantUIDs[0]))
				}
			}

			for i := range mappings {
				if !slices.Equal(plans[i].SrcUIDs, tt.wantUIDs[i]) {
					t.Errorf("slot %d: SrcUIDs = %v, want %v", i, plans[i].SrcUIDs, tt.wantUIDs[i])
				}
				if plans[i].RepeatedCopies != tt.wantRepeated[i] {
					t.Errorf("slot %d: RepeatedCopies = %d, want %d", i, plans[i].RepeatedCopies, tt.wantRepeated[i])
				}
				if got := mergedStandbys(t, plans[i], shared, i); !slices.Equal(got, tt.wantStandbys[i]) {
					t.Errorf("slot %d: standbys = %+v, want %+v", i, got, tt.wantStandbys[i])
				}
			}
			if len(shared.reserved) != 0 {
				t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
			}
			if got := totalNew.Load(); got != 1 {
				t.Errorf("totalNew = %d, want 1: the shared message lands once", got)
			}
		})
	}
}

func Test_destinationDiff_When_PlanKeepsOneCopyPerMessageID_Then_TheRecheckAppliesTheSameRule(t *testing.T) {
	t.Parallel()

	// The stale values are not zero, so each row shows that the recheck computes them again.
	const stale = 7
	planOf := func(uids ...uint32) FolderSyncPlan {
		return FolderSyncPlan{
			SourceFolder: "Src", DestinationFolder: "Dst", SrcUIDs: uids, NewMessages: len(uids),
			NewSize: 999, ExtraCopies: stale, RepeatedCopies: stale, copyRule: copyOnePerMessageID,
		}
	}
	x := []client.MessageRef{{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}
	y := []client.MessageRef{{UID: 4, Size: 40}, {UID: 5, Size: 50}}
	tests := []struct {
		srcMap       map[string][]client.MessageRef
		dstMap       map[string][]client.MessageRef
		name         string
		wantUIDs     []uint32
		plan         FolderSyncPlan
		wantSize     uint64
		wantRepeated int
		wantHeld     int
		wantGone     int
	}{
		{
			name:     "the destination now holds the id",
			plan:     planOf(1),
			srcMap:   map[string][]client.MessageRef{"x@x": x},
			dstMap:   map[string][]client.MessageRef{"x@x": msgRefs(51)},
			wantHeld: 1,
		},
		{
			name:     "the destination copy has the size of the last source copy",
			plan:     planOf(1),
			srcMap:   map[string][]client.MessageRef{"x@x": x},
			dstMap:   map[string][]client.MessageRef{"x@x": {{UID: 51, Size: 30}}},
			wantHeld: 1,
		},
		{
			name:         "the destination is still empty for the id",
			plan:         planOf(1),
			srcMap:       map[string][]client.MessageRef{"x@x": x},
			dstMap:       map[string][]client.MessageRef{},
			wantUIDs:     []uint32{1},
			wantSize:     10,
			wantRepeated: 2,
		},
		{
			name:     "the source lost the planned copy and holds two more",
			plan:     planOf(1),
			srcMap:   map[string][]client.MessageRef{"x@x": x[1:]},
			dstMap:   map[string][]client.MessageRef{},
			wantGone: 1,
		},
		{
			name:     "the plan holds a copy that is not the lowest",
			plan:     planOf(2),
			srcMap:   map[string][]client.MessageRef{"x@x": x},
			dstMap:   map[string][]client.MessageRef{},
			wantHeld: 1,
		},
		{
			name:         "two ids and both stay",
			plan:         planOf(1, 4),
			srcMap:       map[string][]client.MessageRef{"x@x": x, "y@x": y},
			dstMap:       map[string][]client.MessageRef{},
			wantUIDs:     []uint32{1, 4},
			wantSize:     50,
			wantRepeated: 3,
		},
		{
			name:         "the destination now holds one of two ids",
			plan:         planOf(1, 4),
			srcMap:       map[string][]client.MessageRef{"x@x": x, "y@x": y},
			dstMap:       map[string][]client.MessageRef{"y@x": msgRefs(51, 52)},
			wantUIDs:     []uint32{1},
			wantSize:     10,
			wantRepeated: 2,
			wantHeld:     1,
		},
		{
			name:         "a single copy leaves nothing out",
			plan:         planOf(4),
			srcMap:       map[string][]client.MessageRef{"y@x": y[:1]},
			dstMap:       map[string][]client.MessageRef{},
			wantUIDs:     []uint32{4},
			wantSize:     40,
			wantRepeated: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := slices.Clone(tt.plan.SrcUIDs)

			got, drops := rediffOnePlan(tt.plan, tt.srcMap, tt.dstMap)

			if !slices.Equal(got.SrcUIDs, tt.wantUIDs) {
				t.Errorf("SrcUIDs = %v, want %v", got.SrcUIDs, tt.wantUIDs)
			}
			if got.NewMessages != len(tt.wantUIDs) || got.NewSize != tt.wantSize {
				t.Errorf("NewMessages, NewSize = %d, %d, want %d, %d", got.NewMessages, got.NewSize, len(tt.wantUIDs), tt.wantSize)
			}
			if got.RepeatedCopies != tt.wantRepeated {
				t.Errorf("RepeatedCopies = %d, want %d", got.RepeatedCopies, tt.wantRepeated)
			}
			if got.ExtraCopies != 0 {
				t.Errorf("ExtraCopies = %d, want 0 under the one-copy rule", got.ExtraCopies)
			}
			if drops.held != tt.wantHeld || drops.gone != tt.wantGone {
				t.Errorf("drops = held %d, gone %d, want held %d, gone %d", drops.held, drops.gone, tt.wantHeld, tt.wantGone)
			}
			if total := drops.held + drops.gone + len(got.SrcUIDs); total != len(tt.plan.SrcUIDs) {
				t.Errorf("held %d + gone %d + kept %d = %d, want %d planned UIDs", drops.held, drops.gone, len(got.SrcUIDs), total, len(tt.plan.SrcUIDs))
			}
			if got.copyRule != copyOnePerMessageID {
				t.Errorf("copyRule = %d, want %d", got.copyRule, copyOnePerMessageID)
			}
			if !slices.Equal(tt.plan.SrcUIDs, before) {
				t.Errorf("input SrcUIDs = %v, want %v: the plan slice must not change", tt.plan.SrcUIDs, before)
			}
		})
	}
}

func Test_maybeDiff_When_DestinationKeepsOneCopyAndCountsVary_Then_PlanAndRecheckPickTheSameMessages(t *testing.T) {
	t.Parallel()

	for _, srcCount := range []int{1, 2, 3, 5} {
		for _, dstCount := range []int{0, 1, 2, 5} {
			t.Run(fmt.Sprintf("%d source copies and %d on the destination", srcCount, dstCount), func(t *testing.T) {
				t.Parallel()

				// The UIDs rise in steps of five and the sizes differ, so a wrong pick shows in both.
				src := make([]client.MessageRef, srcCount)
				for i := range src {
					src[i] = client.MessageRef{UID: uint32(10 + 5*i), Size: uint32(100 * (i + 1))}
				}
				srcMap := func() map[string][]client.MessageRef {
					return map[string][]client.MessageRef{"s@x": slices.Clone(src)}
				}
				dstMap := func(n int) map[string][]client.MessageRef {
					held := make([]client.MessageRef, n)
					for i := range held {
						held[i] = client.MessageRef{UID: uint32(900 + i), Size: 7}
					}
					return map[string][]client.MessageRef{"s@x": held}
				}
				var wantUIDs []uint32
				var wantSize uint64
				if dstCount == 0 {
					wantUIDs, wantSize = []uint32{src[0].UID}, uint64(src[0].Size)
				}

				planned, _, _ := planUnder(t, copyOnePerMessageID, srcMap(), dstMap(dstCount))

				if !slices.Equal(planned.SrcUIDs, wantUIDs) || planned.NewSize != wantSize {
					t.Fatalf("plan = UIDs %v, size %d, want UIDs %v, size %d", planned.SrcUIDs, planned.NewSize, wantUIDs, wantSize)
				}
				// Rechecking the plan for an empty destination against the real copies keeps exactly what a plan made with those copies picks.
				full, _, _ := planUnder(t, copyOnePerMessageID, srcMap(), dstMap(0))
				rechecked, drops := rediffOnePlan(full, srcMap(), dstMap(dstCount))
				if !slices.Equal(rechecked.SrcUIDs, wantUIDs) || rechecked.NewSize != wantSize {
					t.Errorf("recheck of the full plan = UIDs %v, size %d, want UIDs %v, size %d", rechecked.SrcUIDs, rechecked.NewSize, wantUIDs, wantSize)
				}
				if wantHeld := 1 - len(wantUIDs); drops.held != wantHeld || drops.gone != 0 {
					t.Errorf("recheck drops = %+v, want held %d and gone 0", drops, wantHeld)
				}
				if len(wantUIDs) > 0 && rechecked.RepeatedCopies != srcCount-1 {
					t.Errorf("recheck RepeatedCopies = %d, want %d", rechecked.RepeatedCopies, srcCount-1)
				}
				// The recheck of a plan made with the same copies changes nothing.
				again, drops := rediffOnePlan(planned, srcMap(), dstMap(dstCount))
				if !slices.Equal(again.SrcUIDs, planned.SrcUIDs) || again.NewSize != planned.NewSize || again.RepeatedCopies != planned.RepeatedCopies || drops != (recheckDrops{}) {
					t.Errorf("recheck with equal copies = UIDs %v, size %d, repeated %d, drops %+v, want the plan %v, %d, %d and no drops",
						again.SrcUIDs, again.NewSize, again.RepeatedCopies, drops, planned.SrcUIDs, planned.NewSize, planned.RepeatedCopies)
				}
			})
		}
	}
}

func Test_destinationDiff_When_OneCopyRuleAndPlanVary_Then_ResultIsASubsetOfThePlan(t *testing.T) {
	t.Parallel()

	ids := []string{"a@x", "b@x", "c@x", "d@x"}
	rng := rand.New(rand.NewPCG(13, 17))
	for round := range 500 {
		srcMap := make(map[string][]client.MessageRef)
		sizeOf := make(map[uint32]uint32)
		idOf := make(map[uint32]string)
		var nextUID uint32
		for _, id := range ids {
			for range rng.IntN(5) {
				nextUID += uint32(1 + rng.IntN(3))
				size := uint32(100 * (1 + rng.IntN(3)))
				srcMap[id] = append(srcMap[id], client.MessageRef{UID: nextUID, Size: size})
				sizeOf[nextUID] = size
				idOf[nextUID] = id
			}
		}
		dstMap := make(map[string][]client.MessageRef)
		for _, id := range ids {
			for i := range rng.IntN(3) {
				dstMap[id] = append(dstMap[id], client.MessageRef{UID: uint32(900 + i), Size: uint32(100 * (1 + rng.IntN(3)))})
			}
		}
		var planned []uint32
		for uid := uint32(1); uid <= nextUID+3; uid++ {
			if _, inSource := sizeOf[uid]; rng.IntN(3) > 0 && (inSource || uid > nextUID) {
				planned = append(planned, uid)
			}
		}
		plan := FolderSyncPlan{SourceFolder: "S", DestinationFolder: "D", SrcUIDs: planned, NewMessages: len(planned), RepeatedCopies: 9, copyRule: copyOnePerMessageID}
		before := slices.Clone(planned)

		got, drops := rediffOnePlan(plan, srcMap, dstMap)

		if !slices.Equal(plan.SrcUIDs, before) {
			t.Fatalf("round %d: destinationDiff changed the slice of the plan: %v, was %v", round, plan.SrcUIDs, before)
		}
		// Under the rule a group is fresh only when the destination holds none of it, and only its lowest UID is.
		fresh := make(map[uint32]int)
		for id, refs := range srcMap {
			if len(dstMap[id]) == 0 {
				fresh[refs[0].UID] = len(refs) - 1
			}
		}
		var wantKept []uint32
		var wantSize uint64
		var wantRepeated, wantHeld, wantGone int
		for _, uid := range planned {
			_, inSource := sizeOf[uid]
			others, isFresh := fresh[uid]
			switch {
			case isFresh:
				wantKept = append(wantKept, uid)
				wantSize += uint64(sizeOf[uid])
				wantRepeated += others
			case inSource:
				wantHeld++
			default:
				wantGone++
			}
		}
		if !slices.Equal(got.SrcUIDs, wantKept) {
			t.Fatalf("round %d: SrcUIDs = %v, want %v\nsource %v\ndestination %v\nplan %v", round, got.SrcUIDs, wantKept, srcMap, dstMap, planned)
		}
		perID := make(map[string]int)
		for _, uid := range got.SrcUIDs {
			if !slices.Contains(planned, uid) {
				t.Fatalf("round %d: result holds UID %d that the plan did not hold", round, uid)
			}
			if perID[idOf[uid]]++; perID[idOf[uid]] > 1 {
				t.Fatalf("round %d: result holds two UIDs of %s: %v", round, idOf[uid], got.SrcUIDs)
			}
		}
		if got.NewMessages != len(wantKept) || got.NewSize != wantSize || got.RepeatedCopies != wantRepeated || got.ExtraCopies != 0 {
			t.Fatalf("round %d: NewMessages, NewSize, RepeatedCopies, ExtraCopies = %d, %d, %d, %d, want %d, %d, %d, 0",
				round, got.NewMessages, got.NewSize, got.RepeatedCopies, got.ExtraCopies, len(wantKept), wantSize, wantRepeated)
		}
		if drops.held != wantHeld || drops.gone != wantGone {
			t.Fatalf("round %d: held, gone = %d, %d, want %d, %d", round, drops.held, drops.gone, wantHeld, wantGone)
		}
		if drops.held+drops.gone+len(got.SrcUIDs) != len(planned) {
			t.Fatalf("round %d: held %d + gone %d + kept %d != planned %d", round, drops.held, drops.gone, len(got.SrcUIDs), len(planned))
		}
	}
}

// repeatedCopiesMarker is part of the preview line about source copies that a destination with one message for each Message-Id leaves out.
const repeatedCopiesMarker = "repeated copies of these messages are left out"

func repeatedCopiesLine(n int) string {
	return fmt.Sprintf("  ℹ️  %d repeated copies of these messages are left out: the destination server keeps one message for each Message-Id\n", n)
}

// newGmailDestination starts a destination that advertises X-GM-EXT-1 and keeps one message for each Message-Id, as Gmail does.
func newGmailDestination(t *testing.T, folders map[string][]fakeMsg) *fakeServer {
	t.Helper()

	dst := newStoreServer(t, "/", folders)
	dst.advertiseGmailExtension()
	dst.keepOnePerMessageID()
	return dst
}

func TestRunSync_When_GmailDestinationGetsRepeatedMessageIds_Then_OneCopyArrivesAndTheNextRunCopiesNothing(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
	dst := newGmailDestination(t, map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)\n", len(bodies[0]))+repeatedCopiesLine(2),
		fmt.Sprintf("Total new messages to sync: 1 (≈ %d B)", len(bodies[0])),
		"Sync completed successfully",
	)
	assertContainsNone(t, first, extraCopiesMarker)
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Scans"), id)
	if got, want := dst.folderBodies("Scans"), []string{bodies[0]}; !slices.Equal(got, want) {
		t.Errorf("dst Scans bodies = %q, want the copy with the lowest source UID %q", got, want)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages", "Sync completed successfully", repeatedCopiesMarker, extraCopiesMarker)
	if n := len(dst.appended("Scans")); n != 1 {
		t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_GmailDestinationHoldsTheMessageId_Then_NoCopyOfItIsPlanned(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": append(sharedIDMsgs(id, bodies...), fakeMsg{msgID: "z@x"})})
	dst := newGmailDestination(t, map[string][]fakeMsg{"Scans": sharedIDMsgs(id, subjectBody(id, "held"))})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "• Scans → Scans will copy 1 messages", "Sync completed successfully")
	assertContainsNone(t, first, extraCopiesMarker, repeatedCopiesMarker)
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Scans"), "z@x")

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	if n := len(dst.appended("Scans")); n != 1 {
		t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_TwoMappingsShareAGmailDestination_Then_TheSharedMessageArrivesOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		workers         int
		firstCopyIsGone bool
	}{
		{name: "one worker", workers: 1},
		{name: "two workers", workers: 2},
		{name: "one worker and an empty body on the lowest copy of the first mapping", workers: 1, firstCopyIsGone: true},
		{name: "two workers and an empty body on the lowest copy of the first mapping", workers: 2, firstCopyIsGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "x@x"
			fromA := []string{subjectBody(id, "a1"), subjectBody(id, "a2")}
			fromB := subjectBody(id, "b1")
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": sharedIDMsgs(id, fromA...), "B": sharedIDMsgs(id, fromB)})
			if tt.firstCopyIsGone {
				src.answerBodyIn("A", id, "NIL")
			}
			dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, tt.workers, mapping("A", "Shared"), mapping("B", "Shared"))
			wantBody := fromA[0]
			if tt.firstCopyIsGone {
				wantBody = fromB
			}

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				fmt.Sprintf("• A → Shared will copy 1 messages (≈ %d B)\n", len(fromA[0]))+repeatedCopiesLine(1),
				"Total new messages to sync: 1",
				"Sync completed successfully",
			)
			assertContainsNone(t, first, "• B → Shared will copy", "errors occurred", extraCopiesMarker)
			assertIDs(t, "dst APPENDs after the first run", dst.appended("Shared"), id)
			if got := dst.folderBodies("Shared"); !slices.Equal(got, []string{wantBody}) {
				t.Errorf("dst Shared bodies = %q, want %q", got, []string{wantBody})
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := len(dst.appended("Shared")); n != 1 {
				t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_HiddenGmailDestinationHoldsTheMessageId_Then_TheRecheckAppliesTheOneCopyRule(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": append(sharedIDMsgs(id, bodies...), fakeMsg{msgID: "z@x"})})
	// The held copy has the size of the third source copy, so a count rule would take the lowest UID as not held.
	dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil, "Work": sharedIDMsgs(id, bodies[2])})
	dst.hideFromList("Work")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		"• Work → Work will copy 2 messages",
		repeatedCopiesLine(2),
		"  ℹ️  Destination folder Work already exists and holds 1 of the 2 planned messages from Work, so they are not copied\n",
		"Sync completed successfully",
	)
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Work"), "z@x")
	if n := dst.callCount("CREATE"); n != 1 {
		t.Errorf("destination saw %d CREATE commands, want 1: the recheck needs the CREATE that reports the existing folder", n)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "holds 2 of the 2 planned messages", "All folders already synced!")
	assertContainsNone(t, second, "Syncing messages", "Sync completed successfully")
	if n := len(dst.appended("Work")); n != 1 {
		t.Errorf("dst saw %d APPENDs over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_GmailDestinationRunsQuiet_Then_PrintsNoRepeatedCopiesLine(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
	dst := newGmailDestination(t, map[string][]fakeMsg{})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.quiet = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Scans", "Scans")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsNone(t, out.String(), repeatedCopiesMarker, "will copy")
	assertIDs(t, "dst APPENDs", dst.appended("Scans"), id)
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_VerbosePreviewHasRepeatedCopies_Then_LineComesBeforeTheUIDList(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
	dst := newGmailDestination(t, map[string][]fakeMsg{})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Scans", "Scans")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	assertContainsAll(t, got, fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)\n", len(bodies[0]))+repeatedCopiesLine(2))
	willCopy := strings.Index(got, "will copy 1 messages")
	note := strings.Index(got, repeatedCopiesMarker)
	firstUID := strings.Index(got, "  • UID ")
	if willCopy < 0 || note < 0 || firstUID < 0 || willCopy >= note || note >= firstUID {
		t.Errorf("positions: will copy line %d, repeated line %d, first UID line %d, want them in this order\noutput: %q", willCopy, note, firstUID, got)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_OnlyTheSourceAdvertisesGmailExtension_Then_CopyByCountStays(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	bodies := []string{subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
	src.advertiseGmailExtension()
	dst := newStoreServer(t, "/", map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "• Scans → Scans will copy 3 messages", "Sync completed successfully")
	assertContainsNone(t, first, repeatedCopiesMarker)
	if got := dst.folderBodies("Scans"); !slices.Equal(got, bodies) {
		t.Errorf("dst Scans bodies = %q, want the three source bodies %q", got, bodies)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationAdvertisesGmailExtension_Then_VerboseRunNamesTheOneCopyRule(t *testing.T) {
	t.Parallel()

	const (
		ruleLine   = "  ℹ️  [dst] The destination server advertises X-GM-EXT-1, so each destination folder gets at most one copy of each Message-Id\n"
		ruleMarker = "advertises X-GM-EXT-1"
		delimiters = "  Destination [dst]: \"/\"\n\n"
	)
	tests := []struct {
		dst          func(t *testing.T) *fakeServer
		name         string
		wantAll      []string
		wantNone     []string
		srcAdvertise bool
		verbose      bool
		quiet        bool
		wantLine     bool
	}{
		{
			name:     "destination advertises it and the run is verbose",
			dst:      func(t *testing.T) *fakeServer { return newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil}) },
			verbose:  true,
			wantLine: true,
			wantAll:  []string{delimiters + ruleLine + "\n", "Messages to be copied to destination"},
		},
		{
			name:     "destination advertises it and the run is not verbose",
			dst:      func(t *testing.T) *fakeServer { return newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil}) },
			wantAll:  []string{"Messages to be copied to destination"},
			wantNone: []string{ruleMarker},
		},
		{
			name:     "destination advertises it and the run is verbose and quiet",
			dst:      func(t *testing.T) *fakeServer { return newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil}) },
			verbose:  true,
			quiet:    true,
			wantNone: []string{ruleMarker},
		},
		{
			name:         "only the source advertises it and the run is verbose",
			dst:          func(t *testing.T) *fakeServer { return newStoreServer(t, "/", map[string][]fakeMsg{}) },
			srcAdvertise: true,
			verbose:      true,
			wantAll:      []string{"Server delimiters", "Messages to be copied to destination"},
			wantNone:     []string{ruleMarker},
		},
		{
			name: "destination advertises it and already holds the Message-Id",
			dst: func(t *testing.T) *fakeServer {
				return newGmailDestination(t, map[string][]fakeMsg{"Scans": fakeMsgs("m1@x")})
			},
			verbose:  true,
			wantLine: true,
			wantAll:  []string{delimiters + ruleLine + "\n", "All folders already synced!"},
			wantNone: []string{"Messages to be copied to destination"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": fakeMsgs("m1@x")})
			if tt.srcAdvertise {
				src.advertiseGmailExtension()
			}
			dst := tt.dst(t)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose, opts.quiet = true, tt.verbose, tt.quiet

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Scans", "Scans")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			got := out.String()
			assertContainsAll(t, got, tt.wantAll...)
			assertContainsNone(t, got, tt.wantNone...)
			if tt.wantLine {
				if n := strings.Count(got, ruleLine); n != 1 {
					t.Errorf("output holds the rule line %d times, want 1\noutput: %q", n, got)
				}
				line := strings.Index(got, ruleLine)
				next := strings.Index(got, "Messages to be copied to destination")
				if next < 0 {
					next = strings.Index(got, "All folders already synced!")
				}
				if line < 0 || next < 0 || line >= next {
					t.Errorf("positions: rule line %d, next section %d, want the rule line first\noutput: %q", line, next, got)
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_LowestGmailCopyHasAnEmptyBody_Then_AHigherCopyOfTheMessageIdArrivesInTheSameRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		emptied map[uint32]string
		name    string
		copies  int
		hidden  bool
	}{
		{name: "two copies and the lowest has no body", copies: 2, emptied: map[uint32]string{1: "NIL"}},
		{name: "three copies and the two lowest have no body", copies: 3, emptied: map[uint32]string{1: "NIL", 2: "{0}\r\n"}},
		{name: "two copies and the destination folder is hidden from LIST", copies: 2, emptied: map[uint32]string{1: "NIL"}, hidden: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "scan@printer"
			bodies := make([]string, tt.copies)
			for i := range bodies {
				bodies[i] = subjectBody(id, fmt.Sprintf("copy %d", i+1))
			}
			var wantBody string
			for i, body := range bodies {
				if _, empty := tt.emptied[uint32(i+1)]; !empty {
					wantBody = body
					break
				}
			}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, bodies...)})
			for uid, value := range tt.emptied {
				src.answerBodyAt("Scans", uid, value)
			}
			held := fakeMsgs("other@x")
			dstFolders := map[string][]fakeMsg{}
			var wantBodies []string
			if tt.hidden {
				dstFolders = map[string][]fakeMsg{"INBOX": nil, "Scans": held}
				wantBodies = append(wantBodies, imapFullBody("other@x"))
			}
			wantBodies = append(wantBodies, wantBody)
			dst := newGmailDestination(t, dstFolders)
			if tt.hidden {
				dst.hideFromList("Scans")
			}
			cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)\n", len(bodies[0]))+repeatedCopiesLine(tt.copies-1),
				"Total new messages to sync: 1",
				"Sync completed successfully",
			)
			assertContainsNone(t, first, "errors occurred", "empty body", extraCopiesMarker)
			if got := dst.folderBodies("Scans"); !slices.Equal(got, wantBodies) {
				t.Errorf("dst Scans bodies = %q, want %q", got, wantBodies)
			}
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands, want 1", n)
			}
			if tt.hidden {
				if n := dst.callCount("CREATE"); n != 1 {
					t.Errorf("destination saw %d CREATE commands, want 1: the recheck needs the CREATE that reports the existing folder", n)
				}
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			assertContainsNone(t, second, "Sync completed successfully")
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands over both runs, want 1", n)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_FirstMappingOfAGmailDestinationHasAnEmptyBody_Then_AnotherCopyOfTheMessageIdArrivesInTheSameRun(t *testing.T) {
	t.Parallel()

	const id = "x@x"
	tests := []struct {
		name         string
		wantBody     string
		fromA        []string
		fromB        []string
		emptiedA     []uint32
		emptiedB     []uint32
		wantRepeated int
		hidden       bool
		listed       bool
	}{
		{
			name:         "the lowest copy of the first mapping is empty and it holds a second copy",
			fromA:        []string{subjectBody(id, "a1"), subjectBody(id, "a2")},
			fromB:        []string{subjectBody(id, "b1")},
			emptiedA:     []uint32{1},
			wantBody:     subjectBody(id, "a2"),
			wantRepeated: 1,
		},
		{
			name:         "the only copy of the first mapping is empty and the second mapping holds a usable second copy",
			fromA:        []string{subjectBody(id, "a1")},
			fromB:        []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA:     []uint32{1},
			emptiedB:     []uint32{1},
			wantBody:     subjectBody(id, "b2"),
			wantRepeated: 0,
		},
		{
			name:         "the destination folder is hidden from LIST",
			fromA:        []string{subjectBody(id, "a1"), subjectBody(id, "a2")},
			fromB:        []string{subjectBody(id, "b1")},
			emptiedA:     []uint32{1},
			wantBody:     subjectBody(id, "a2"),
			wantRepeated: 1,
			hidden:       true,
		},
		{
			name:     "the only copy of the first mapping is empty and the lowest copy of the second is usable",
			fromA:    []string{subjectBody(id, "a1")},
			fromB:    []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA: []uint32{1},
			wantBody: subjectBody(id, "b1"),
		},
		{
			name:     "the only copy of the first mapping is empty, the lowest copy of the second is usable and the destination folder is listed",
			fromA:    []string{subjectBody(id, "a1")},
			fromB:    []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA: []uint32{1},
			wantBody: subjectBody(id, "b1"),
			listed:   true,
		},
		{
			name:     "the only copy of the first mapping is empty, the lowest copy of the second is usable and the destination folder is hidden from LIST",
			fromA:    []string{subjectBody(id, "a1")},
			fromB:    []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA: []uint32{1},
			wantBody: subjectBody(id, "b1"),
			hidden:   true,
		},
		{
			name:     "the only copy of the first mapping is empty, the lowest copy of the second is empty too and the destination folder is listed",
			fromA:    []string{subjectBody(id, "a1")},
			fromB:    []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA: []uint32{1},
			emptiedB: []uint32{1},
			wantBody: subjectBody(id, "b2"),
			listed:   true,
		},
		{
			name:     "the only copy of the first mapping is empty, the lowest copy of the second is empty too and the destination folder is hidden from LIST",
			fromA:    []string{subjectBody(id, "a1")},
			fromB:    []string{subjectBody(id, "b1"), subjectBody(id, "b2")},
			emptiedA: []uint32{1},
			emptiedB: []uint32{1},
			wantBody: subjectBody(id, "b2"),
			hidden:   true,
		},
	}
	for _, tt := range tests {
		for _, workers := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s with %d workers", tt.name, workers), func(t *testing.T) {
				t.Parallel()

				src := newStoreServer(t, "/", map[string][]fakeMsg{"A": sharedIDMsgs(id, tt.fromA...), "B": sharedIDMsgs(id, tt.fromB...)})
				for _, uid := range tt.emptiedA {
					src.answerBodyAt("A", uid, "NIL")
				}
				for _, uid := range tt.emptiedB {
					src.answerBodyAt("B", uid, "NIL")
				}
				dstFolders := map[string][]fakeMsg{"INBOX": nil}
				if tt.listed {
					dstFolders["X"] = nil
				}
				var wantBodies []string
				if tt.hidden {
					dstFolders["X"] = fakeMsgs("other@x")
					wantBodies = append(wantBodies, imapFullBody("other@x"))
				}
				wantBodies = append(wantBodies, tt.wantBody)
				dst := newGmailDestination(t, dstFolders)
				if tt.hidden {
					dst.hideFromList("X")
				}
				cfg := e2eConfig(src, dst, workers, mapping("A", "X"), mapping("B", "X"))

				first := runSyncOnce(t, cfg)

				want := fmt.Sprintf("• A → X will copy 1 messages (≈ %d B)\n", len(tt.fromA[0]))
				if tt.wantRepeated > 0 {
					want += repeatedCopiesLine(tt.wantRepeated)
				}
				assertContainsAll(t, first, want, "Total new messages to sync: 1", "Sync completed successfully")
				assertContainsNone(t, first, "• B → X will copy", "errors occurred", "empty body", extraCopiesMarker)
				if got := dst.folderBodies("X"); !slices.Equal(got, wantBodies) {
					t.Errorf("dst X bodies = %q, want %q", got, wantBodies)
				}
				if n := dst.callCount("APPEND"); n != 1 {
					t.Errorf("destination saw %d APPEND commands, want 1", n)
				}

				second := runSyncOnce(t, cfg)

				assertContainsAll(t, second, "All folders already synced!")
				assertContainsNone(t, second, "Sync completed successfully")
				if n := dst.callCount("APPEND"); n != 1 {
					t.Errorf("destination saw %d APPEND commands over both runs, want 1", n)
				}
				assertReadOnly(t, "source", src)
			})
		}
	}
}

func Test_maybeDiff_When_OneCopyPlanHasNoSharedDestination_Then_CutCopiesOfItsOwnFolderAreStandbys(t *testing.T) {
	t.Parallel()

	three := []client.MessageRef{{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}
	two := []client.MessageRef{{UID: 4, Size: 5}, {UID: 5, Size: 6}}
	tests := []struct {
		src          map[string][]client.MessageRef
		dst          map[string][]client.MessageRef
		name         string
		wantUIDs     []uint32
		wantStandbys []standbyView
		wantSize     uint64
		wantRepeated int
	}{
		{
			name:         "three copies of one id",
			src:          map[string][]client.MessageRef{"x@x": three},
			wantUIDs:     []uint32{1},
			wantSize:     10,
			wantRepeated: 2,
			wantStandbys: []standbyView{{planned: 1, folder: "A", uidValidity: 77, uid: 2}, {planned: 1, folder: "A", uidValidity: 77, uid: 3}},
		},
		{
			name:         "ids with three copies and with two copies",
			src:          map[string][]client.MessageRef{"x@x": three, "y@x": two},
			wantUIDs:     []uint32{1, 4},
			wantSize:     15,
			wantRepeated: 3,
			wantStandbys: []standbyView{
				{planned: 1, folder: "A", uidValidity: 77, uid: 2},
				{planned: 1, folder: "A", uidValidity: 77, uid: 3},
				{planned: 4, folder: "A", uidValidity: 77, uid: 5},
			},
		},
		{
			name:     "single copies only",
			src:      map[string][]client.MessageRef{"x@x": three[:1], "y@x": two[:1]},
			wantUIDs: []uint32{1, 4},
			wantSize: 15,
		},
		{
			name: "the destination holds the id",
			src:  map[string][]client.MessageRef{"x@x": three},
			dst:  map[string][]client.MessageRef{"x@x": msgRefs(90)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{mapping("A", "D")}
			plans := make([]FolderSyncPlan, 1)
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64
			s := &folderScan{srcMap: tt.src, dstMap: tt.dst, srcMessages: countRefs(tt.src), srcUIDValidity: 77, dstExists: true}

			diffBothSidesUnder(t, copyOnePerMessageID, s, 0, mappings, plans, &totalNew, &totalNewSize, nil)

			plan := plans[0]
			if !slices.Equal(plan.SrcUIDs, tt.wantUIDs) || plan.NewMessages != len(tt.wantUIDs) || plan.NewSize != tt.wantSize {
				t.Errorf("SrcUIDs, NewMessages, NewSize = %v, %d, %d, want %v, %d, %d", plan.SrcUIDs, plan.NewMessages, plan.NewSize, tt.wantUIDs, len(tt.wantUIDs), tt.wantSize)
			}
			if plan.RepeatedCopies != tt.wantRepeated || plan.ExtraCopies != 0 {
				t.Errorf("RepeatedCopies, ExtraCopies = %d, %d, want %d, 0", plan.RepeatedCopies, plan.ExtraCopies, tt.wantRepeated)
			}
			if totalNew.Load() != int64(len(tt.wantUIDs)) || totalNewSize.Load() != tt.wantSize {
				t.Errorf("totals = %d, %d, want %d, %d: only planned copies count", totalNew.Load(), totalNewSize.Load(), len(tt.wantUIDs), tt.wantSize)
			}
			if got := resolveStandbys(t, plan.standbySources, plan.standbys); !slices.Equal(got, tt.wantStandbys) {
				t.Errorf("standbys = %+v, want %+v", got, tt.wantStandbys)
			}
			if len(tt.wantStandbys) == 0 && (plan.standbySources != nil || plan.standbys != nil) {
				t.Errorf("standbySources, standbys = %v, %v, want nil", plan.standbySources, plan.standbys)
			}
			for _, c := range plan.standbys {
				if slices.Contains(plan.SrcUIDs, c.uid) {
					t.Errorf("standby UID %d is also a planned UID in %v", c.uid, plan.SrcUIDs)
				}
			}
		})
	}
}

func Test_maybeDiff_When_ThreeOneCopyMappingsShareADestination_Then_OnlyTheFirstPlansAndTheOthersAreStandbys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dst          map[string][]client.MessageRef
		name         string
		refs         [][]uint32
		wantUIDs     [][]uint32
		wantRepeated []int
		wantStandbys [][]standbyView
		wantTotal    int64
	}{
		{
			name:         "none of the copies is on the destination",
			refs:         [][]uint32{{1, 2}, {10}, {20, 21, 22}},
			wantUIDs:     [][]uint32{{1}, nil, nil},
			wantRepeated: []int{1, 0, 0},
			wantStandbys: [][]standbyView{
				{
					{planned: 1, folder: "A", uidValidity: 100, uid: 2},
					{planned: 1, folder: "B", uidValidity: 101, uid: 10},
					{planned: 1, folder: "C", uidValidity: 102, uid: 20},
					{planned: 1, folder: "C", uidValidity: 102, uid: 21},
					{planned: 1, folder: "C", uidValidity: 102, uid: 22},
				},
				nil,
				nil,
			},
			wantTotal: 1,
		},
		{
			name:         "the first mapping lacks the id",
			refs:         [][]uint32{nil, {10, 11}, {20}},
			wantUIDs:     [][]uint32{nil, {10}, nil},
			wantRepeated: []int{0, 1, 0},
			wantStandbys: [][]standbyView{
				nil,
				{{planned: 10, folder: "B", uidValidity: 101, uid: 11}, {planned: 10, folder: "C", uidValidity: 102, uid: 20}},
				nil,
			},
			wantTotal: 1,
		},
		{
			name:         "the destination holds the id",
			dst:          map[string][]client.MessageRef{"m@x": msgRefs(900)},
			refs:         [][]uint32{{1, 2}, {10, 11}, {20, 21, 22}},
			wantUIDs:     [][]uint32{nil, nil, nil},
			wantRepeated: []int{0, 0, 0},
			wantStandbys: [][]standbyView{nil, nil, nil},
		},
		{
			name:         "the destination holds a copy that came without a UID",
			dst:          map[string][]client.MessageRef{"m@x": msgRefs(0)},
			refs:         [][]uint32{{1, 2}, {10, 11}, {20, 21, 22}},
			wantUIDs:     [][]uint32{nil, nil, nil},
			wantRepeated: []int{0, 0, 0},
			wantStandbys: [][]standbyView{nil, nil, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mappings := []config.DirectoryMapping{mapping("A", "D"), mapping("B", "D"), mapping("C", "D")}
			shared := newSharedDestinations(mappings)
			plans := make([]FolderSyncPlan, len(mappings))
			var totalNew atomic.Int64
			var totalNewSize atomic.Uint64

			for i := range mappings {
				srcMap := map[string][]client.MessageRef{}
				if len(tt.refs[i]) > 0 {
					srcMap["m@x"] = msgRefs(tt.refs[i]...)
				}
				s := &folderScan{srcMap: srcMap, dstMap: maps.Clone(tt.dst), srcMessages: countRefs(srcMap), srcUIDValidity: uint32(100 + i), dstExists: true}
				diffBothSidesUnder(t, copyOnePerMessageID, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
			}

			for i := range mappings {
				if !slices.Equal(plans[i].SrcUIDs, tt.wantUIDs[i]) {
					t.Errorf("slot %d: SrcUIDs = %v, want %v", i, plans[i].SrcUIDs, tt.wantUIDs[i])
				}
				if plans[i].RepeatedCopies != tt.wantRepeated[i] || plans[i].ExtraCopies != 0 {
					t.Errorf("slot %d: RepeatedCopies, ExtraCopies = %d, %d, want %d, 0", i, plans[i].RepeatedCopies, plans[i].ExtraCopies, tt.wantRepeated[i])
				}
				if len(tt.wantUIDs[i]) > 0 && plans[i].copyRule != copyOnePerMessageID {
					t.Errorf("slot %d: copyRule = %d, want %d", i, plans[i].copyRule, copyOnePerMessageID)
				}
				if got := mergedStandbys(t, plans[i], shared, i); !slices.Equal(got, tt.wantStandbys[i]) {
					t.Errorf("slot %d: standbys = %+v, want %+v", i, got, tt.wantStandbys[i])
				}
			}
			if len(shared.reserved) != 0 {
				t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
			}
			if got := totalNew.Load(); got != tt.wantTotal {
				t.Errorf("totalNew = %d, want %d", got, tt.wantTotal)
			}
		})
	}
}

func Test_maybeDiff_When_PlanCountsByCopiesAndHasNoSharedDestination_Then_PlanHasNoStandbys(t *testing.T) {
	t.Parallel()

	src := map[string][]client.MessageRef{"x@x": {{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}}

	plan, _, _ := planUnder(t, copyByCount, src, nil)

	if !slices.Equal(plan.SrcUIDs, []uint32{1, 2, 3}) {
		t.Errorf("SrcUIDs = %v, want [1 2 3]: every copy is planned", plan.SrcUIDs)
	}
	if plan.standbySources != nil || plan.standbys != nil {
		t.Errorf("standbySources, standbys = %v, %v, want nil, nil", plan.standbySources, plan.standbys)
	}
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func TestFolderSyncPlan_mergeStandbys(t *testing.T) {
	own := func() *FolderSyncPlan {
		return &FolderSyncPlan{
			standbySources: []standbySource{{folder: "A", uidValidity: 1}},
			standbys:       []standbyCopy{{planned: 1, uid: 2}, {planned: 3, uid: 4}, {planned: 3, uid: 5}},
		}
	}
	sharedSources := []standbySource{{folder: "B", uidValidity: 2}, {folder: "C", uidValidity: 3}}
	sharedCopies := []standbyCopy{{planned: 1, uid: 10}, {planned: 1, uid: 20, source: 1}, {planned: 2, uid: 12}, {planned: 3, uid: 11}}

	t.Run("only the own folder", func(t *testing.T) {
		p := own()
		want := own()

		p.mergeStandbys(nil, nil)

		if !slices.Equal(p.standbySources, want.standbySources) || !slices.Equal(p.standbys, want.standbys) {
			t.Errorf("standbySources, standbys = %v, %v, want %v, %v", p.standbySources, p.standbys, want.standbySources, want.standbys)
		}
		if n := testing.AllocsPerRun(100, func() { q := *p; q.mergeStandbys(nil, nil) }); n != 0 {
			t.Errorf("mergeStandbys allocates %v times per call, want 0", n)
		}
	})
	t.Run("only the shared set", func(t *testing.T) {
		var p FolderSyncPlan
		beforeSources, beforeCopies := slices.Clone(sharedSources), slices.Clone(sharedCopies)

		p.mergeStandbys(sharedSources, sharedCopies)

		if !slices.Equal(p.standbySources, sharedSources) || !slices.Equal(p.standbys, sharedCopies) {
			t.Errorf("standbySources, standbys = %v, %v, want the shared %v, %v", p.standbySources, p.standbys, sharedSources, sharedCopies)
		}
		if !slices.Equal(sharedSources, beforeSources) || !slices.Equal(sharedCopies, beforeCopies) {
			t.Errorf("shared slices changed to %v, %v, want %v, %v", sharedSources, sharedCopies, beforeSources, beforeCopies)
		}
		if n := testing.AllocsPerRun(100, func() { var q FolderSyncPlan; q.mergeStandbys(sharedSources, sharedCopies) }); n != 0 {
			t.Errorf("mergeStandbys allocates %v times per call, want 0", n)
		}
	})
	t.Run("neither", func(t *testing.T) {
		var p FolderSyncPlan

		p.mergeStandbys(nil, nil)

		if p.standbySources != nil || p.standbys != nil {
			t.Errorf("standbySources, standbys = %v, %v, want nil, nil", p.standbySources, p.standbys)
		}
		if n := testing.AllocsPerRun(100, func() { var q FolderSyncPlan; q.mergeStandbys(nil, nil) }); n != 0 {
			t.Errorf("mergeStandbys allocates %v times per call, want 0", n)
		}
	})
	t.Run("both", func(t *testing.T) {
		p := own()
		beforeSources, beforeCopies := slices.Clone(sharedSources), slices.Clone(sharedCopies)

		p.mergeStandbys(sharedSources, sharedCopies)

		wantSources := []standbySource{{folder: "A", uidValidity: 1}, {folder: "B", uidValidity: 2}, {folder: "C", uidValidity: 3}}
		wantCopies := []standbyCopy{
			{planned: 1, uid: 2, source: 0}, {planned: 1, uid: 10, source: 1}, {planned: 1, uid: 20, source: 2},
			{planned: 2, uid: 12, source: 1},
			{planned: 3, uid: 4, source: 0}, {planned: 3, uid: 5, source: 0}, {planned: 3, uid: 11, source: 1},
		}
		if !slices.Equal(p.standbySources, wantSources) {
			t.Errorf("standbySources = %v, want the own folder first: %v", p.standbySources, wantSources)
		}
		if !slices.Equal(p.standbys, wantCopies) {
			t.Errorf("standbys = %v, want %v", p.standbys, wantCopies)
		}
		if !slices.Equal(sharedSources, beforeSources) || !slices.Equal(sharedCopies, beforeCopies) {
			t.Errorf("shared slices changed to %v, %v, want %v, %v", sharedSources, sharedCopies, beforeSources, beforeCopies)
		}
		if got := p.standbysOf(3); !slices.Equal(got, wantCopies[4:]) {
			t.Errorf("standbysOf(3) = %v, want %v", got, wantCopies[4:])
		}
	})
}

func Test_standbysFor_When_ManyCopiesOfOneSourceStandByForOnePlannedUID_Then_UIDsAscend(t *testing.T) {
	t.Parallel()

	shared := newSharedDestinations([]config.DirectoryMapping{mapping("A", "X"), mapping("B", "X")})
	source := standbySource{folder: "B", uidValidity: 7}
	var added []standbyCopy
	for planned := uint32(1); planned <= 2; planned++ {
		for k := range uint32(40) {
			added = append(added, standbyCopy{planned: planned, uid: 100*planned + k})
		}
	}
	rng := rand.New(rand.NewPCG(3, 5))
	rng.Shuffle(len(added), func(i, j int) { added[i], added[j] = added[j], added[i] })
	shared.mu.Lock()
	for _, c := range added {
		shared.addStandby(reservation{uid: c.planned, slot: 0}, source, c.uid)
	}
	shared.mu.Unlock()

	sources, copies := shared.standbysFor(0)

	if !slices.Equal(sources, []standbySource{source}) {
		t.Fatalf("sources = %v, want only %v", sources, source)
	}
	want := slices.Clone(added)
	slices.SortFunc(want, func(a, b standbyCopy) int {
		return cmp.Or(cmp.Compare(a.planned, b.planned), cmp.Compare(a.uid, b.uid))
	})
	if !slices.Equal(copies, want) {
		t.Errorf("standbys = %v, want planned-major order with ascending UIDs: %v", copies, want)
	}
}

func Test_buildSyncPlan_When_OneCopyDestinationHasCutCopies_Then_PlansCarryOwnStandbysBeforeLaterMappings(t *testing.T) {
	t.Parallel()

	const lag = 120 * time.Millisecond
	type planView struct {
		src, dst string
		uids     []uint32
		standbys []standbyView
	}
	scenarios := []struct {
		folders  map[string][]fakeMsg
		name     string
		mappings []config.DirectoryMapping
		want     []planView
		total    int
	}{
		{
			name:     "one mapping",
			folders:  map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m1@x", "m2@x", "m1@x")},
			mappings: []config.DirectoryMapping{mapping("A", "X")},
			want: []planView{{src: "A", dst: "X", uids: []uint32{1, 3}, standbys: []standbyView{
				{planned: 1, folder: "A", uidValidity: 11, uid: 2},
				{planned: 1, folder: "A", uidValidity: 11, uid: 4},
			}}},
			total: 2,
		},
		{
			name: "three mappings share the destination",
			folders: map[string][]fakeMsg{
				"A": fakeMsgs("m1@x", "m1@x", "m2@x"), "B": fakeMsgs("m1@x", "m1@x"), "C": fakeMsgs("m1@x", "m3@x"),
			},
			mappings: []config.DirectoryMapping{mapping("A", "X"), mapping("B", "X"), mapping("C", "X")},
			want: []planView{
				{src: "A", dst: "X", uids: []uint32{1, 3}, standbys: []standbyView{
					{planned: 1, folder: "A", uidValidity: 11, uid: 2},
					{planned: 1, folder: "B", uidValidity: 12, uid: 1},
					{planned: 1, folder: "B", uidValidity: 12, uid: 2},
					{planned: 1, folder: "C", uidValidity: 13, uid: 1},
				}},
				{src: "C", dst: "X", uids: []uint32{2}},
			},
			total: 3,
		},
	}
	lags := []struct {
		name     string
		srcLagAt int
		dstLagAt int
	}{
		{name: "no delay"},
		{name: "first source scan is slow", srcLagAt: 1},
		{name: "last destination scan is slow", dstLagAt: 3},
		{name: "first source and first destination scans are slow", srcLagAt: 1, dstLagAt: 1},
	}
	for _, sc := range scenarios {
		for _, lg := range lags {
			t.Run(sc.name+" and "+lg.name, func(t *testing.T) {
				t.Parallel()

				src := newStoreServer(t, "/", sc.folders)
				for i, folder := range []string{"A", "B", "C"} {
					if _, ok := sc.folders[folder]; ok {
						src.setUIDValidity(folder, uint32(11+i))
					}
				}
				dst := newGmailDestination(t, map[string][]fakeMsg{"X": nil})
				lagOnNth := func(srv *fakeServer, nth int, verbs ...string) {
					if nth == 0 {
						return
					}
					var mu sync.Mutex
					var seen int
					for _, verb := range verbs {
						srv.onCommand(verb, func() {
							mu.Lock()
							seen++
							hit := seen == nth
							mu.Unlock()
							if hit {
								time.Sleep(lag)
							}
						})
					}
				}
				lagOnNth(src, lg.srcLagAt, "EXAMINE")
				lagOnNth(dst, lg.dstLagAt, "EXAMINE", "STATUS")
				srcTr, dstTr := makePlanTrackers()

				summary, err := buildSyncPlan(e2eContext(t), newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), sc.mappings, srcTr, dstTr, "src", "dst")

				if err != nil {
					t.Fatalf("buildSyncPlan: %v", err)
				}
				got := make([]planView, len(summary.Plans))
				for i, p := range summary.Plans {
					got[i] = planView{src: p.SourceFolder, dst: p.DestinationFolder, uids: p.SrcUIDs, standbys: resolveStandbys(t, p.standbySources, p.standbys)}
				}
				if !slices.EqualFunc(got, sc.want, func(a, b planView) bool {
					return a.src == b.src && a.dst == b.dst && slices.Equal(a.uids, b.uids) && slices.Equal(a.standbys, b.standbys)
				}) {
					t.Errorf("plans = %+v, want %+v", got, sc.want)
				}
				if summary.TotalNew != sc.total {
					t.Errorf("TotalNew = %d, want %d", summary.TotalNew, sc.total)
				}
			})
		}
	}
}

func TestRunSync_When_EveryCopyOfAGmailMessageIdHasAnEmptyBody_Then_EachRunCountsOneErrorAndAppendsNothing(t *testing.T) {
	t.Parallel()

	for _, verbose := range []bool{false, true} {
		t.Run(fmt.Sprintf("verbose %t", verbose), func(t *testing.T) {
			t.Parallel()

			const id = "scan@printer"
			src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": sharedIDMsgs(id, subjectBody(id, "one"), subjectBody(id, "two"), subjectBody(id, "three"))})
			src.answerBody(id, "NIL")
			dst := newGmailDestination(t, map[string][]fakeMsg{})
			cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

			for run := 1; run <= 2; run++ {
				out := &syncBuffer{}
				opts := e2eOptions(out, "")
				opts.autoConfirm, opts.verbose = true, verbose

				err := runSync(e2eContext(t), cfg, opts)

				if !errors.Is(err, ErrSilentExit) {
					t.Fatalf("run %d error = %v, want ErrSilentExit", run, err)
				}
				assertContainsAll(t, out.String(), "Sync completed with errors. 0 messages uploaded, 1 errors occurred, 0 folders failed")
				assertContainsNone(t, out.String(), "Sync completed successfully")
				if verbose {
					assertContainsAll(t, out.String(), "source returned an empty body")
				}
				if n := dst.callCount("APPEND"); n != 0 {
					t.Errorf("destination saw %d APPEND commands after run %d, want 0: an empty message must never reach it", n, run)
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func Test_maybeDiff_When_OneCopyMappingsShareASourceFolder_Then_TheFolderStandsByOnlyAsItsOwnPlan(t *testing.T) {
	t.Parallel()

	mappings := []config.DirectoryMapping{mapping("S", "D"), mapping("S", "D")}
	shared := newSharedDestinations(mappings)
	plans := make([]FolderSyncPlan, len(mappings))
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64

	for i := range mappings {
		srcMap := map[string][]client.MessageRef{"m@x": msgRefs(1, 2)}
		s := &folderScan{srcMap: srcMap, srcMessages: countRefs(srcMap), srcUIDValidity: 9, dstExists: true}
		diffBothSidesUnder(t, copyOnePerMessageID, s, i, mappings, plans, &totalNew, &totalNewSize, shared)
	}

	want := []standbyView{{planned: 1, folder: "S", uidValidity: 9, uid: 2}}
	if got := mergedStandbys(t, plans[0], shared, 0); !slices.Equal(got, want) {
		t.Errorf("standbys = %+v, want %+v: the second mapping adds nothing for its own folder", got, want)
	}
	if plans[1].SrcUIDs != nil || plans[1].standbys != nil {
		t.Errorf("slot 1: SrcUIDs, standbys = %v, %v, want none", plans[1].SrcUIDs, plans[1].standbys)
	}
	if len(shared.reserved) != 0 {
		t.Errorf("reserved = %v after the last slot, want none", shared.reserved)
	}
}

func TestRunSync_When_MappingsGoToDifferentGmailFolders_Then_EachFolderGetsItsOwnCopy(t *testing.T) {
	t.Parallel()

	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d workers", workers), func(t *testing.T) {
			t.Parallel()

			const id = "m@x"
			fromS1 := []string{subjectBody(id, "s1 first"), subjectBody(id, "s1 second")}
			fromS2 := subjectBody(id, "s2")
			src := newStoreServer(t, "/", map[string][]fakeMsg{"S1": sharedIDMsgs(id, fromS1...), "S2": sharedIDMsgs(id, fromS2)})
			dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, workers, mapping("S1", "D1"), mapping("S2", "D2"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				fmt.Sprintf("• S1 → D1 will copy 1 messages (≈ %d B)\n", len(fromS1[0]))+repeatedCopiesLine(1),
				fmt.Sprintf("• S2 → D2 will copy 1 messages (≈ %d B)\n", len(fromS2)),
				fmt.Sprintf("Total new messages to sync: 2 (≈ %d B)", totalSize(fromS1[0], fromS2)),
				"Sync completed successfully",
			)
			if n := strings.Count(first, repeatedCopiesMarker); n != 1 {
				t.Errorf("output holds %d repeated-copies lines, want 1: only S1 has a repeated copy\noutput: %q", n, first)
			}
			assertIDs(t, "dst APPENDs to D1", dst.appended("D1"), id)
			assertIDs(t, "dst APPENDs to D2", dst.appended("D2"), id)
			if got := dst.folderBodies("D1"); !slices.Equal(got, fromS1[:1]) {
				t.Errorf("dst D1 bodies = %q, want the lowest copy of S1 %q", got, fromS1[:1])
			}
			if got := dst.folderBodies("D2"); !slices.Equal(got, []string{fromS2}) {
				t.Errorf("dst D2 bodies = %q, want %q", got, []string{fromS2})
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != 2 {
				t.Errorf("destination saw %d APPENDs over both runs, want 2", n)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_GmailFolderHoldsRepeatedAndSingleMessageIds_Then_OnlyTheLowestCopyOfEachIdIsPlanned(t *testing.T) {
	t.Parallel()

	// The lowest copy of m@x is the largest and the lowest copy of n@x is the smallest, so a pick by size differs from a pick by UID.
	m := []string{subjectBody("m@x", "the first copy has a long subject"), subjectBody("m@x", "b"), subjectBody("m@x", "cc")}
	n := []string{subjectBody("n@x", "d"), subjectBody("n@x", "the second copy has a long subject")}
	o := subjectBody("o@x", "o")
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"Scans": slices.Concat(sharedIDMsgs("m@x", m...), sharedIDMsgs("n@x", n...), sharedIDMsgs("o@x", o)),
	})
	dst := newGmailDestination(t, map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))
	planned := []string{m[0], n[0], o}

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• Scans → Scans will copy 3 messages (≈ %d B)\n", totalSize(planned...))+repeatedCopiesLine(3),
		fmt.Sprintf("Total new messages to sync: 3 (≈ %d B)", totalSize(planned...)),
		"Sync completed successfully",
	)
	assertContainsNone(t, first, extraCopiesMarker, "errors occurred", folderFailuresHeader, "Sync completed with errors")
	assertIDs(t, "dst APPENDs", dst.appended("Scans"), "m@x", "n@x", "o@x")
	if got := dst.folderBodies("Scans"); !slices.Equal(got, planned) {
		t.Errorf("dst Scans bodies = %q, want the lowest copy of each Message-Id %q", got, planned)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	if got := dst.callCount("APPEND"); got != 3 {
		t.Errorf("destination saw %d APPENDs over both runs, want 3", got)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_GmailDestinationHoldsOneOfTwoMessageIds_Then_OnlyTheOtherIdIsPlanned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                      string
		destinationScanSendsNoUID bool
	}{
		{name: "the destination scan sends the UID of the held copy"},
		{name: "the destination scan sends no UID for the held copy", destinationScanSendsNoUID: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := []string{subjectBody("m@x", "one"), subjectBody("m@x", "two"), subjectBody("m@x", "three")}
			n := []string{subjectBody("n@x", "one"), subjectBody("n@x", "two")}
			held := subjectBody("m@x", "held")
			src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": slices.Concat(sharedIDMsgs("m@x", m...), sharedIDMsgs("n@x", n...))})
			dst := newGmailDestination(t, map[string][]fakeMsg{"Scans": sharedIDMsgs("m@x", held)})
			if tt.destinationScanSendsNoUID {
				dst.omitScanUIDIn("Scans")
			}
			cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

			first := runSyncOnce(t, cfg)

			assertContainsAll(t, first,
				fmt.Sprintf("• Scans → Scans will copy 1 messages (≈ %d B)\n", len(n[0]))+repeatedCopiesLine(1),
				"Sync completed successfully",
			)
			assertContainsNone(t, first, extraCopiesMarker)
			assertIDs(t, "dst APPENDs", dst.appended("Scans"), "n@x")
			if got, want := dst.folderBodies("Scans"), []string{held, n[0]}; !slices.Equal(got, want) {
				t.Errorf("dst Scans bodies = %q, want the held copy and the lowest copy of n@x %q", got, want)
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if got := len(dst.appended("Scans")); got != 1 {
				t.Errorf("destination saw %d APPENDs over both runs, want 1", got)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func Test_maybeDiff_When_OneCopyDestinationHoldsTheIdAtTheFirstSlot_Then_NothingIsReserved(t *testing.T) {
	t.Parallel()

	mappings := []config.DirectoryMapping{mapping("A", "D"), mapping("B", "D")}
	shared := newSharedDestinations(mappings)
	plans := make([]FolderSyncPlan, len(mappings))
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64
	srcMap := map[string][]client.MessageRef{"x@x": msgRefs(1, 2)}
	s := &folderScan{srcMap: srcMap, dstMap: map[string][]client.MessageRef{"x@x": msgRefs(90)}, srcMessages: 2, dstExists: true}

	diffBothSidesUnder(t, copyOnePerMessageID, s, 0, mappings, plans, &totalNew, &totalNewSize, shared)

	if got := shared.reserved["D"]["x@x"]; len(got) != 0 {
		t.Errorf("reserved copies of x@x after slot 0 = %v, want none: a plan that copies nothing reserves nothing", got)
	}
}

func TestRunSync_When_ThreeMappingsShareAGmailDestination_Then_OneCopyArrivesAndTheNextRunCopiesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		destination map[string][]fakeMsg
		name        string
		workers     int
		wantAppends int
	}{
		{name: "one worker and a missing destination", workers: 1, destination: map[string][]fakeMsg{"INBOX": nil}, wantAppends: 1},
		{name: "two workers and a missing destination", workers: 2, destination: map[string][]fakeMsg{"INBOX": nil}, wantAppends: 1},
		{name: "two workers and a destination that holds the id", workers: 2, destination: map[string][]fakeMsg{"X": sharedIDMsgs("x@x", subjectBody("x@x", "held"))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "x@x"
			fromA := []string{subjectBody(id, "a1"), subjectBody(id, "a2")}
			fromB := []string{subjectBody(id, "b1")}
			fromC := []string{subjectBody(id, "c1"), subjectBody(id, "c2"), subjectBody(id, "c3")}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": sharedIDMsgs(id, fromA...), "B": sharedIDMsgs(id, fromB...), "C": sharedIDMsgs(id, fromC...)})
			dst := newGmailDestination(t, tt.destination)
			cfg := e2eConfig(src, dst, tt.workers, mapping("A", "X"), mapping("B", "X"), mapping("C", "X"))

			first := runSyncOnce(t, cfg)

			if tt.wantAppends == 0 {
				assertContainsAll(t, first, "All folders already synced!")
			} else {
				assertContainsAll(t, first,
					fmt.Sprintf("• A → X will copy 1 messages (≈ %d B)\n", len(fromA[0]))+repeatedCopiesLine(1),
					"Total new messages to sync: 1",
					"Sync completed successfully",
				)
			}
			assertContainsNone(t, first, "• B → X will copy", "• C → X will copy", extraCopiesMarker, "errors occurred")
			if n := dst.callCount("APPEND"); n != tt.wantAppends {
				t.Errorf("destination saw %d APPENDs, want %d", n, tt.wantAppends)
			}
			if tt.wantAppends == 1 {
				if got := dst.folderBodies("X"); !slices.Equal(got, fromA[:1]) {
					t.Errorf("dst X bodies = %q, want the lowest copy of the first mapping %q", got, fromA[:1])
				}
			}

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("APPEND"); n != tt.wantAppends {
				t.Errorf("destination saw %d APPENDs over both runs, want %d", n, tt.wantAppends)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func Test_recheckExistingDestinations_When_OneCopyPlanLosesAHeldId_Then_KeptPlanCountsOnlyItsOwnRepeatedCopies(t *testing.T) {
	t.Parallel()

	m := []string{subjectBody("m@x", "m1"), subjectBody("m@x", "m22"), subjectBody("m@x", "m333")}
	n := []string{subjectBody("n@x", "n1 is the lowest"), subjectBody("n@x", "n2")}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": slices.Concat(sharedIDMsgs("m@x", m...), sharedIDMsgs("n@x", n...))})
	// The held copy has the size of the third source copy, so a count rule would take the lowest UID as not held.
	dst := newGmailDestination(t, map[string][]fakeMsg{"Work": sharedIDMsgs("m@x", m[2])})
	srcC := newAppClient(t, src, "src")
	dstC := newAppClient(t, dst, "dst")
	plans := []FolderSyncPlan{{
		SourceFolder:      "Work",
		DestinationFolder: "Work",
		SrcUIDs:           []uint32{1, 4},
		NewMessages:       2,
		NewSize:           uint64(len(m[0]) + len(n[0])),
		RepeatedCopies:    3,
		SrcUIDValidity:    1,
		copyRule:          copyOnePerMessageID,
	}}
	out := &syncBuffer{}

	kept, failures, err := recheckExistingDestinations(t.Context(), out, false, srcC, dstC, plans, map[string]struct{}{"Work": {}}, &throttleStop{})

	if err != nil || failures != nil {
		t.Fatalf("error = %v, failures = %v, want both nil", err, failures)
	}
	if len(kept) != 1 {
		t.Fatalf("kept = %+v, want one plan", kept)
	}
	got := kept[0]
	if !slices.Equal(got.SrcUIDs, []uint32{4}) || got.NewMessages != 1 || got.NewSize != uint64(len(n[0])) {
		t.Errorf("kept SrcUIDs, NewMessages, NewSize = %v, %d, %d, want [4], 1, %d", got.SrcUIDs, got.NewMessages, got.NewSize, len(n[0]))
	}
	if got.RepeatedCopies != 1 || got.ExtraCopies != 0 || got.copyRule != copyOnePerMessageID {
		t.Errorf("kept RepeatedCopies, ExtraCopies, copyRule = %d, %d, %d, want 1, 0, %d", got.RepeatedCopies, got.ExtraCopies, got.copyRule, copyOnePerMessageID)
	}
	want := "  ℹ️  Destination folder Work already exists and holds 1 of the 2 planned messages from Work, so they are not copied\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestRunSync_When_HiddenGmailDestinationHoldsOneOfTwoMessageIds_Then_OnlyTheOtherIdIsCopied(t *testing.T) {
	t.Parallel()

	m := []string{subjectBody("m@x", "m1"), subjectBody("m@x", "m22"), subjectBody("m@x", "m333")}
	n := []string{subjectBody("n@x", "n1"), subjectBody("n@x", "n2")}
	// The held copy has the size of the third source copy, so a count rule would take the lowest UID as not held.
	held := m[2]
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": slices.Concat(sharedIDMsgs("m@x", m...), sharedIDMsgs("n@x", n...))})
	dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil, "Work": sharedIDMsgs("m@x", held)})
	dst.hideFromList("Work")
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• Work → Work will copy 2 messages (≈ %d B)\n", len(m[0])+len(n[0]))+repeatedCopiesLine(3),
		"  ℹ️  Destination folder Work already exists and holds 1 of the 2 planned messages from Work, so they are not copied\n",
		"Sync completed successfully",
	)
	assertContainsNone(t, first, extraCopiesMarker, "no longer holds")
	assertIDs(t, "dst APPENDs", dst.appended("Work"), "n@x")
	if got, want := dst.folderBodies("Work"), []string{held, n[0]}; !slices.Equal(got, want) {
		t.Errorf("dst Work bodies = %q, want the held copy and the lowest copy of n@x %q", got, want)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "holds 2 of the 2 planned messages", "All folders already synced!")
	if got := len(dst.appended("Work")); got != 1 {
		t.Errorf("destination saw %d APPENDs over both runs, want 1", got)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_PlannedLowestGmailCopyLeavesTheSourceBeforeTheRecheck_Then_TheHigherCopyIsNotAddedUntilTheNextRun(t *testing.T) {
	t.Parallel()

	const (
		heldLine = "  ℹ️  Destination folder Work already exists and holds 1 of the 2 planned messages from Work, so they are not copied\n"
		goneLine = "  ℹ️  Source folder Work no longer holds 1 of the 2 planned messages for Work, so they are not copied\n"
	)
	m := []string{subjectBody("m@x", "m1"), subjectBody("m@x", "m22"), subjectBody("m@x", "m333")}
	n := []string{subjectBody("n@x", "n1"), subjectBody("n@x", "n2")}
	// The held copy has the size of the third source copy, so a count rule would take the lowest UID as not held.
	held := m[2]
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": slices.Concat(sharedIDMsgs("m@x", m...), sharedIDMsgs("n@x", n...))})
	dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil, "Work": sharedIDMsgs("m@x", held)})
	dst.hideFromList("Work")
	var once sync.Once
	dst.onCommand("CREATE", func() { once.Do(func() { src.removeMessageAt("Work", 4) }) })
	cfg := e2eConfig(src, dst, 1, mapping("Work", "Work"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• Work → Work will copy 2 messages (≈ %d B)\n", len(m[0])+len(n[0]))+repeatedCopiesLine(3),
		heldLine, goneLine, "All folders already synced!",
	)
	assertContainsNone(t, first, "holds 2 of the 2", "Syncing messages")
	assertIDs(t, "dst APPENDs after the first run", dst.appended("Work"))
	if got := src.callCount("UID FETCH"); got != 0 {
		t.Errorf("source saw %d UID FETCH commands, want 0: the recheck must not add the higher copy", got)
	}
	if got := dst.callCount("LOGIN"); got != 1 {
		t.Errorf("destination saw %d LOGIN commands, want 1: no worker may connect", got)
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, fmt.Sprintf("• Work → Work will copy 2 messages (≈ %d B)\n", len(m[0])+len(n[1]))+repeatedCopiesLine(2), heldLine, "Sync completed successfully")
	assertContainsNone(t, second, "no longer holds")
	assertIDs(t, "dst APPENDs after the second run", dst.appended("Work"), "n@x")
	if got, want := dst.folderBodies("Work"), []string{held, n[1]}; !slices.Equal(got, want) {
		t.Errorf("dst Work bodies = %q, want the held copy and the remaining copy of n@x %q", got, want)
	}
}

func TestRunSync_When_MirrorRunHasAGmailDestination_Then_EveryFolderAndSubfolderGetsOneCopyPerMessageId(t *testing.T) {
	t.Parallel()

	inbox := []string{subjectBody("i@x", "inbox one"), subjectBody("i@x", "inbox two")}
	work := []string{subjectBody("w@x", "work one"), subjectBody("w@x", "work two"), subjectBody("w@x", "work three")}
	sub := []string{subjectBody("k@x", "sub one"), subjectBody("k@x", "sub two")}
	sent := []string{subjectBody("s@x", "sent one"), subjectBody("s@x", "sent two")}
	single := imapFullBody("x@x")
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":        sharedIDMsgs("i@x", inbox...),
		"Work":         slices.Concat(sharedIDMsgs("w@x", work...), fakeMsgs("x@x")),
		"Work/Sub":     sharedIDMsgs("k@x", sub...),
		"[Gmail]/Sent": sharedIDMsgs("s@x", sent...),
	})
	src.markNoSelect("[Gmail]")
	dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil, "[Gmail]/Sent": nil})
	dst.markNoSelect("[Gmail]")
	cfg := e2eConfig(src, dst, 2)

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first,
		fmt.Sprintf("• INBOX → INBOX will copy 1 messages (≈ %d B)\n", len(inbox[0]))+repeatedCopiesLine(1),
		fmt.Sprintf("• Work → Work will copy 2 messages (≈ %d B)\n", len(work[0])+len(single))+repeatedCopiesLine(2),
		fmt.Sprintf("• Work/Sub → Work/Sub will copy 1 messages (≈ %d B)\n", len(sub[0]))+repeatedCopiesLine(1),
		fmt.Sprintf("• [Gmail]/Sent → [Gmail]/Sent will copy 1 messages (≈ %d B)\n", len(sent[0]))+repeatedCopiesLine(1),
		"Total new messages to sync: 5",
		"Sync completed successfully",
	)
	assertContainsNone(t, first, extraCopiesMarker, "errors occurred", "cannot be synced")
	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i@x")
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "w@x", "x@x")
	assertIDs(t, "dst Work/Sub", dst.folderMsgIDs("Work/Sub"), "k@x")
	assertIDs(t, "dst [Gmail]/Sent", dst.folderMsgIDs("[Gmail]/Sent"), "s@x")
	for folder, want := range map[string]string{"INBOX": inbox[0], "Work/Sub": sub[0], "[Gmail]/Sent": sent[0]} {
		if got := dst.folderBodies(folder); !slices.Equal(got, []string{want}) {
			t.Errorf("dst %s bodies = %q, want the lowest source copy %q", folder, got, want)
		}
	}
	for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
		if slices.Contains(srv.capturedNames("EXAMINE"), "[Gmail]") {
			t.Errorf("%s opened [Gmail], a folder its LIST marks \\Noselect", name)
		}
	}

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "All folders already synced!")
	if n := dst.callCount("APPEND"); n != 5 {
		t.Errorf("destination saw %d APPENDs over both runs, want 5", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_ServerRefusesCapability_Then_RunFailsBeforeAnyScanAndNamesTheLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		wantStep     string
		wantLabel    string
		refuseSource bool
	}{
		{name: "source", refuseSource: true, wantStep: "source connection failed", wantLabel: "[src] capability"},
		{name: "destination", wantStep: "destination connection failed", wantLabel: "[dst] capability"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			refusing := dst
			if tt.refuseSource {
				refusing = src
			}
			refusing.addConnHandler(scriptedHandler(refusing, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
				if verb == "CAPABILITY" {
					return "NO [CANNOT] capability refused", true
				}
				return "", false
			}))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

			if err == nil {
				t.Fatalf("runSync error = nil, want the capability failure\noutput: %s", out.String())
			}
			for _, want := range []string{tt.wantStep, "failed to read server capabilities", tt.wantLabel, "capability refused"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			assertContainsNone(t, out.String(), "will copy", "Syncing messages")
			for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
				for _, verb := range []string{"EXAMINE", "STATUS", "FETCH", "CREATE", "APPEND"} {
					if n := srv.callCount(verb); n != 0 {
						t.Errorf("%s saw %d %s commands, want 0: the run must fail before any scan or write", name, n, verb)
					}
				}
			}
		})
	}
}

func TestRunSync_When_GmailRunUsesTwoWorkers_Then_EachConnectionAsksForCapabilitiesOnce(t *testing.T) {
	t.Parallel()

	// The store server greeting lists its capabilities, so go-imap sends no CAPABILITY before LOGIN
	// and the count per connection is the one check that New makes after LOGIN.
	const folders = 6
	srcFolders := make(map[string][]fakeMsg, folders)
	mappings := make([]config.DirectoryMapping, 0, folders)
	for i := range folders {
		name := fmt.Sprintf("F%d", i)
		srcFolders[name] = sharedIDMsgs(fmt.Sprintf("m%d@x", i), subjectBody(fmt.Sprintf("m%d@x", i), "one"), subjectBody(fmt.Sprintf("m%d@x", i), "two"))
		mappings = append(mappings, mapping(name, name))
	}
	src := newStoreServer(t, "/", srcFolders)
	dst := newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 2, mappings...)

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "Total new messages to sync: 6", "Sync completed successfully")
	const connections = 3
	for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
		if n := srv.callCount("LOGIN"); n != connections {
			t.Errorf("%s saw %d LOGIN commands, want %d: one planning client and two workers", name, n, connections)
		}
		if n := srv.callCount("CAPABILITY"); n != connections {
			t.Errorf("%s saw %d CAPABILITY commands, want %d: one per connection and none per plan", name, n, connections)
		}
	}
}

func Test_copyRule_When_ZeroValue_Then_CopiesByCount(t *testing.T) {
	t.Parallel()

	var rule copyRule
	three := []client.MessageRef{{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}

	if rule != copyByCount || (FolderSyncPlan{}).copyRule != copyByCount {
		t.Errorf("zero copyRule = %d and zero plan rule = %d, want %d", rule, (FolderSyncPlan{}).copyRule, copyByCount)
	}
	if got := rule.sourceCopies(three); len(got) != 3 {
		t.Errorf("sourceCopies of the zero rule returned %d refs, want 3", len(got))
	}
}

func Test_destinationDiff_When_PlanHasNoCopyRule_Then_CountsByCopiesAndLeavesNothingOut(t *testing.T) {
	t.Parallel()

	x := []client.MessageRef{{UID: 1, Size: 10}, {UID: 2, Size: 20}, {UID: 3, Size: 30}}
	plan := FolderSyncPlan{SourceFolder: "S", DestinationFolder: "D", SrcUIDs: []uint32{1, 2, 3}, NewMessages: 3, NewSize: 60, RepeatedCopies: 9}

	tests := []struct {
		name         string
		dst          map[string][]client.MessageRef
		wantUIDs     []uint32
		wantExtra    int
		wantHeld     int
		wantSizeLeft uint64
	}{
		{name: "the destination holds none", dst: map[string][]client.MessageRef{}, wantUIDs: []uint32{1, 2, 3}, wantSizeLeft: 60},
		{name: "the destination holds the copy of the lowest size", dst: map[string][]client.MessageRef{"x@x": {{UID: 90, Size: 10}}}, wantUIDs: []uint32{2, 3}, wantExtra: 2, wantHeld: 1, wantSizeLeft: 50},
		{name: "the destination holds three copies", dst: map[string][]client.MessageRef{"x@x": msgRefs(90, 91, 92)}, wantHeld: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, drops := rediffOnePlan(plan, map[string][]client.MessageRef{"x@x": slices.Clone(x)}, tt.dst)

			if !slices.Equal(got.SrcUIDs, tt.wantUIDs) || got.NewSize != tt.wantSizeLeft {
				t.Errorf("SrcUIDs, NewSize = %v, %d, want %v, %d", got.SrcUIDs, got.NewSize, tt.wantUIDs, tt.wantSizeLeft)
			}
			if got.RepeatedCopies != 0 || got.ExtraCopies != tt.wantExtra {
				t.Errorf("RepeatedCopies, ExtraCopies = %d, %d, want 0, %d", got.RepeatedCopies, got.ExtraCopies, tt.wantExtra)
			}
			if drops.held != tt.wantHeld || drops.gone != 0 {
				t.Errorf("drops = %+v, want held %d and gone 0", drops, tt.wantHeld)
			}
		})
	}
}

func Test_copyRuleOf_When_DestinationAdvertisesGmailExtension_Then_ChoosesTheOneCopyRule(t *testing.T) {
	t.Parallel()

	gmail := newAppClient(t, newGmailDestination(t, map[string][]fakeMsg{"INBOX": nil}), "gmail")
	plain := newAppClient(t, newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil}), "plain")

	if got := copyRuleOf(gmail); got != copyOnePerMessageID {
		t.Errorf("copyRuleOf(gmail) = %d, want %d", got, copyOnePerMessageID)
	}
	if got := copyRuleOf(plain); got != copyByCount {
		t.Errorf("copyRuleOf(plain) = %d, want %d", got, copyByCount)
	}
}

func TestRunSync_When_SourceMessagesHaveFlags_Then_EachCopyKeepsTheFlagsOfItsSource(t *testing.T) {
	t.Parallel()

	rows := []struct {
		id         string
		sourceList string
		wantList   string
		wantStored string
	}{
		{id: "m1@x", sourceList: ``, wantList: ``, wantStored: ``},
		{id: "m2@x", sourceList: `\Seen`, wantList: `(\Seen)`, wantStored: `\Seen`},
		{id: "m3@x", sourceList: `\Flagged $Forwarded`, wantList: `(\Flagged $Forwarded)`, wantStored: `\Flagged $Forwarded`},
		{id: "m4@x", sourceList: `\Seen \Answered \Recent \Deleted NonJunk`, wantList: `(\Seen \Answered NonJunk)`, wantStored: `\Seen \Answered NonJunk`},
		{id: "m5@x", sourceList: `\Draft \FLAGGED`, wantList: `(\Draft \FLAGGED)`, wantStored: `\Draft \FLAGGED`},
		{id: "m6@x", sourceList: `\Seen \X-Ext $Label`, wantList: `(\Seen $Label)`, wantStored: `\Seen $Label`},
		{id: "m7@x", sourceList: `\seen \ANSWERED \draft`, wantList: `(\seen \ANSWERED \draft)`, wantStored: `\seen \ANSWERED \draft`},
		{id: "m8@x", sourceList: `\X-Ext \EXCEEDEDQUOTA \Junk`, wantList: ``, wantStored: ``},
	}
	msgs := make([]fakeMsg, len(rows))
	ids := make([]string, len(rows))
	wantLists := make([]string, len(rows))
	for i, row := range rows {
		msgs[i] = fakeMsg{msgID: row.id, flags: row.sourceList}
		ids[i] = row.id
		wantLists[i] = row.wantList
	}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": msgs})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), ids...)
	assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("INBOX"), wantLists...)
	for _, row := range rows {
		if got := dst.flagsOf("INBOX", row.id); got != row.wantStored {
			t.Errorf("flags of %s on the destination = %q, want %q", row.id, got, row.wantStored)
		}
		if got := src.flagsOf("INBOX", row.id); got != row.sourceList {
			t.Errorf("flags of %s on the source = %q, want them unchanged %q", row.id, got, row.sourceList)
		}
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_DestinationRefusesFlagExtensions_Then_TheMessageIsCopiedOnceWithoutThem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		msgs      []fakeMsg
		wantIDs   []string
		wantLists []string
		wantFlags []string
	}{
		{
			name:      "unknown extension flag",
			msgs:      []fakeMsg{{msgID: "m1@x", flags: `\Seen \X-Ext $Label`}, {msgID: "m2@x", flags: `\Flagged`}},
			wantIDs:   []string{"m1@x", "m2@x"},
			wantLists: []string{`(\Seen $Label)`, `(\Flagged)`},
			wantFlags: []string{`\Seen $Label`, `\Flagged`},
		},
		{
			name:      "extension flag that the refusal would echo as a full quota",
			msgs:      []fakeMsg{{msgID: "m1@x", flags: `\Seen \EXCEEDEDQUOTA`}},
			wantIDs:   []string{"m1@x"},
			wantLists: []string{`(\Seen)`},
			wantFlags: []string{`\Seen`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": tt.msgs})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseFlagExtensions()
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
			first := &syncBuffer{}
			firstOpts := e2eOptions(first, "")
			firstOpts.autoConfirm = true

			if err := runSync(e2eContext(t), cfg, firstOpts); err != nil {
				t.Fatalf("first runSync: %v\noutput: %s", err, first.String())
			}

			assertContainsAll(t, first.String(), "Sync completed successfully")
			assertContainsNone(t, first.String(), "errors occurred", "Sync stopped")
			assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), tt.wantIDs...)
			assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("INBOX"), tt.wantLists...)
			for i, id := range tt.wantIDs {
				if got := dst.flagsOf("INBOX", id); got != tt.wantFlags[i] {
					t.Errorf("flags of %s on the destination = %q, want %q", id, got, tt.wantFlags[i])
				}
			}
			assertReadOnly(t, "source", src)

			appends := dst.callCount("APPEND")
			second := &syncBuffer{}
			secondOpts := e2eOptions(second, "")
			secondOpts.autoConfirm = true
			if err := runSync(e2eContext(t), cfg, secondOpts); err != nil {
				t.Fatalf("second runSync: %v\noutput: %s", err, second.String())
			}
			assertContainsAll(t, second.String(), "All folders already synced!")
			if n := dst.callCount("APPEND"); n != appends {
				t.Errorf("APPEND count went from %d to %d on the second run", appends, n)
			}
		})
	}
}

func TestRunSync_When_SourceMessagesHaveInternalDates_Then_EachCopyKeepsTheDateOfItsSource(t *testing.T) {
	t.Parallel()

	rows := []struct {
		id           string
		internalDate string
		envelopeDate string
		flags        string
		wantDate     string
		wantList     string
	}{
		{
			id: "m1@x", internalDate: "17-Jul-2012 09:30:00 +0200", envelopeDate: "Tue, 17 Jul 2012 07:29:58 +0000",
			wantDate: "17-Jul-2012 09:30:00 +0200",
		},
		{id: "m2@x", internalDate: "02-Jan-2006 15:04:05 -0700", wantDate: " 2-Jan-2006 15:04:05 -0700"},
		{id: "m3@x", envelopeDate: "Mon, 02 Jan 2006 15:04:05 +0100", wantDate: " 2-Jan-2006 15:04:05 +0100"},
		{
			id: "m4@x", internalDate: "not a date", envelopeDate: "Tue, 17 Jul 2012 07:29:58 +0000",
			wantDate: "17-Jul-2012 07:29:58 +0000",
		},
		{id: "m5@x"},
		{
			id: "m6@x", flags: `\Flagged`, internalDate: "17-Jul-2012 09:30:00 +0200",
			wantDate: "17-Jul-2012 09:30:00 +0200", wantList: `(\Flagged)`,
		},
	}
	msgs := make([]fakeMsg, len(rows))
	ids := make([]string, len(rows))
	wantDates := make([]string, len(rows))
	wantLists := make([]string, len(rows))
	for i, row := range rows {
		msgs[i] = fakeMsg{msgID: row.id, internalDate: row.internalDate, envelopeDate: row.envelopeDate, flags: row.flags}
		ids[i] = row.id
		wantDates[i] = row.wantDate
		wantLists[i] = row.wantList
	}
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": msgs})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	first := e2eOptions(&syncBuffer{}, "")
	first.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, first); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), ids...)
	assertIDs(t, "dst APPEND dates", dst.appendedDates("INBOX"), wantDates...)
	assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("INBOX"), wantLists...)
	for _, row := range rows {
		if got := dst.internalDateOf("INBOX", row.id); got != row.wantDate {
			t.Errorf("INTERNALDATE of %s on the destination = %q, want %q", row.id, got, row.wantDate)
		}
		if got := src.internalDateOf("INBOX", row.id); got != row.internalDate {
			t.Errorf("INTERNALDATE of %s on the source = %q, want it unchanged %q", row.id, got, row.internalDate)
		}
	}
	assertReadOnly(t, "source", src)

	appends := dst.callCount("APPEND")
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
}

func TestRunSync_When_SourceFlagsChangeAfterTheCopy_Then_ALaterRunLeavesTheDestinationFlags(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": {
		{msgID: "m1@x", flags: `\Seen`},
		{msgID: "m2@x", flags: `\Flagged $Forwarded`},
		{msgID: "m3@x"},
	}})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	first := e2eOptions(&syncBuffer{}, "")
	first.autoConfirm = true
	if err := runSync(e2eContext(t), cfg, first); err != nil {
		t.Fatalf("first runSync: %v", err)
	}
	appends := dst.callCount("APPEND")
	wantFlags := []string{`\Seen`, `\Flagged $Forwarded`, ``}
	for i, id := range []string{"m1@x", "m2@x", "m3@x"} {
		if got := dst.flagsOf("INBOX", id); got != wantFlags[i] {
			t.Fatalf("flags of %s on the destination after the first run = %q, want %q", id, got, wantFlags[i])
		}
	}

	src.setFlagsIn("INBOX", "m1@x", `\Flagged`)
	src.setFlagsIn("INBOX", "m2@x", ``)
	src.setFlagsIn("INBOX", "m3@x", `\Seen \Answered`)
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
	for _, verb := range []string{"STORE", "UID STORE"} {
		if n := dst.callCount(verb); n != 0 {
			t.Errorf("destination saw %d %s commands, want 0", n, verb)
		}
	}
	for i, id := range []string{"m1@x", "m2@x", "m3@x"} {
		if got := dst.flagsOf("INBOX", id); got != wantFlags[i] {
			t.Errorf("flags of %s on the destination after the second run = %q, want them unchanged %q", id, got, wantFlags[i])
		}
	}
}

func TestRunSync_When_SourceFlagListHoldsCommandText_Then_DestinationGetsOnlyValidFlagsAndNoOtherCommand(t *testing.T) {
	t.Parallel()

	const injected = "x)\r\nA1 DELETE \"Work\""
	hostile := fmt.Sprintf("\\Flagged {%d}\r\n%s y[a) {5}]", len(injected), injected)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": {{msgID: "m1@x", flags: hostile}}})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("w1@x")})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	opts := e2eOptions(&syncBuffer{}, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), "m1@x")
	assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("INBOX"), `(\Flagged)`)
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands, want 1", n)
	}
	if n := dst.callCount("DELETE"); n != 0 {
		t.Errorf("destination saw %d DELETE commands, want 0", n)
	}
	if !dst.hasFolder("Work") {
		t.Error("destination folder Work is gone")
	}
	assertIDs(t, "dst Work", dst.folderMsgIDs("Work"), "w1@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceSendsABodylessResponseBeforeEachFullOne_Then_RunSucceedsWithoutErrors(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	src.omitBodyFirstIn("INBOX", "m1@x")
	src.omitBodyFirstIn("INBOX", "m2@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), cfg, opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	assertContainsAll(t, out.String(), "Sync completed successfully")
	assertContainsNone(t, out.String(), "errors occurred", "no longer holds")
	assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), "m1@x", "m2@x")
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: every message arrived", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceLeavesAHeldMessageOutOfUIDFetch_Then_EveryRunFailsAndTheDestinationNeverGetsIt(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("good@x", "held@x")})
	src.omitInUIDFetch("INBOX", "held@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))

	for run, wantPlanned := range []int{2, 1} {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true

		err := runSync(e2eContext(t), cfg, opts)

		if !errors.Is(err, ErrSilentExit) {
			t.Fatalf("run %d error = %v, want ErrSilentExit", run+1, err)
		}
		assertContainsAll(t, out.String(),
			fmt.Sprintf("INBOX → INBOX will copy %d messages", wantPlanned),
			fmt.Sprintf("%d messages uploaded, 1 errors occurred, 0 folders failed", wantPlanned-1),
			"Sync completed with errors",
		)
		assertContainsNone(t, out.String(), "Sync completed successfully", "no longer holds")
		assertIDs(t, fmt.Sprintf("dst INBOX after run %d", run+1), dst.folderMsgIDs("INBOX"), "good@x")
	}

	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2", "2")
	if n := dst.callCount("APPEND"); n != 1 {
		t.Errorf("destination saw %d APPEND commands over both runs, want 1", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_PlannedMessageLeavesTheSourceBeforeTheCopy_Then_RunSucceedsAndNamesTheGoneMessage(t *testing.T) {
	t.Parallel()

	const goneLine = "  ℹ️  Source folder INBOX no longer holds 1 of the 2 planned messages for INBOX, so they are not copied"
	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "after the prompt"},
		{name: "with --quiet and --confirm", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			if tt.quiet {
				opts.quiet, opts.autoConfirm = true, true
				var once sync.Once
				src.onCommand("UID", func() { once.Do(func() { src.removeMessage("INBOX", "m2@x") }) })
			} else {
				opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.removeMessage("INBOX", "m2@x") }}
			}

			if err := runSync(e2eContext(t), cfg, opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertContainsAll(t, out.String(), "Sync completed successfully")
			assertContainsNone(t, out.String(), "errors occurred")
			if tt.quiet {
				assertContainsNone(t, out.String(), "no longer holds")
			} else if n := strings.Count(out.String(), goneLine); n != 1 {
				t.Errorf("output holds the gone line %d times, want 1\noutput: %q", n, out.String())
			}
			assertIDs(t, "dst APPENDs to INBOX", dst.appended("INBOX"), "m1@x")
			assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2")
			if n := dst.callCount("CREATE"); n != 0 {
				t.Errorf("destination saw %d CREATE commands, want 0: the folder is listed, so no check after CREATE runs", n)
			}

			assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_OneMessageCannotBeFetched_Then_TheRestOfTheFolderIsCopied(t *testing.T) {
	t.Parallel()

	ids := numberedIDs(1200)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs(ids...)})
	src.omitInUIDFetch("INBOX", ids[2])
	src.endUIDFetchThatOmitsWith("INBOX", gmailUnreadableReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
	const streamLine = "Stream error for folder INBOX: "

	first := &syncBuffer{}
	opts := e2eOptions(first, "")
	opts.autoConfirm = true
	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first run error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, first.String(), "1199 messages uploaded, 1 errors occurred")
	if n := strings.Count(first.String(), streamLine); n != 1 {
		t.Errorf("first run printed the stream line %d times, want 1\noutput: %q", n, first.String())
	}
	if got := len(dst.folderMsgIDs("INBOX")); got != 1199 {
		t.Errorf("destination INBOX holds %d messages after the first run, want 1199", got)
	}
	if n := src.callCount("UID FETCH"); n != 3 {
		t.Errorf("source saw %d UID FETCH commands in the first run, want 3: one for each batch of 500", n)
	}

	second := &syncBuffer{}
	opts = e2eOptions(second, "")
	opts.autoConfirm = true
	err = runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("second run error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, second.String(), "INBOX → INBOX will copy 1 messages", "0 messages uploaded, 1 errors occurred")
	if got := len(dst.folderMsgIDs("INBOX")); got != 1199 {
		t.Errorf("destination INBOX holds %d messages after the second run, want 1199", got)
	}
	assertReadOnly(t, "source", src)
}

// timesNamed counts how often a captured command argument equals name.
func timesNamed(names []string, name string) int {
	n := 0
	for _, got := range names {
		if got == name {
			n++
		}
	}
	return n
}

const (
	gmailThrottleReply   = "NO [OVERQUOTA] Account exceeded command or bandwidth limits. (Failure)"
	dovecotQuotaReply    = "NO [OVERQUOTA] Quota exceeded (mailbox for user is full)"
	destinationStopStart = "⛔ Sync stopped: the destination server [dst] reports a rate limit or a full quota: "
)

func TestRunSync_When_DestinationThrottlesAppend_Then_RunStopsWithoutDownloadingTheRest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reply  string
		reason string
		quiet  bool
	}{
		{name: "Gmail reply", reply: gmailThrottleReply, reason: "Account exceeded command or bandwidth limits. (Failure)"},
		{name: "Dovecot reply", reply: dovecotQuotaReply, reason: "Quota exceeded (mailbox for user is full)"},
		{name: "Gmail reply with quiet", reply: gmailThrottleReply, reason: "Account exceeded command or bandwidth limits. (Failure)", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(600)...), "B": fakeMsgs("b1@x", "b2@x", "b3@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
			dst.refuseAppendWith(tt.reply)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.quiet = tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			if n := dst.callCount("APPEND"); n != 1 {
				t.Errorf("destination saw %d APPEND commands, want 1: the first refusal stops the run", n)
			}
			if n := dst.callCount("LOGIN"); n != 2 {
				t.Errorf("destination saw %d LOGIN commands, want 2: planning and the worker", n)
			}
			assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500")
			if n := timesNamed(src.capturedNames("EXAMINE"), "B"); n != 1 {
				t.Errorf("source examined B %d times, want 1: only the planning scan", n)
			}
			if n := src.callCount("UID SEARCH"); n != 0 {
				t.Errorf("source saw %d UID SEARCH commands, want 0", n)
			}
			got := out.String()
			if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
				t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
			}
			assertContainsAll(t, got, destinationStopStart, tt.reason, "0 messages uploaded, 603 errors occurred")
			assertContainsNone(t, got, "Cancelled", "Canceled")
		})
	}
}

func TestRunSync_When_DestinationGivesUpMidPlan_Then_NoMoreBodiesAreFetched(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(600)...), "B": fakeMsgs("b1@x", "b2@x", "b3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
	dst.dropAtAppendAndRefuseLogin(2)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	if got := len(dst.folderMsgIDs("A")) + len(dst.folderMsgIDs("B")); got != 1 {
		t.Errorf("destination holds %d messages, want 1", got)
	}
	if n := dst.callCount("APPEND"); n != 2 {
		t.Errorf("destination saw %d APPEND commands, want 2", n)
	}
	if n := dst.callCount("LOGIN"); n != 3 {
		t.Errorf("destination saw %d LOGIN commands, want 3: planning, the worker and one refused repair", n)
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500")
	if n := src.callCount("LOGIN"); n != 2 {
		t.Errorf("source saw %d LOGIN commands, want 2: planning and the worker", n)
	}
	if n := timesNamed(src.capturedNames("EXAMINE"), "B"); n != 1 {
		t.Errorf("source examined B %d times, want 1: only the planning scan", n)
	}
	assertContainsAll(t, out.String(), "1 messages uploaded, 602 errors occurred")
	assertContainsNone(t, out.String(), "Sync stopped")
}

func TestRunSync_When_SourceThrottlesTheBodyFetch_Then_RunStops(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(600)...), "B": fakeMsgs("b1@x", "b2@x", "b3@x")})
	src.endUIDFetchWith("A", throttleReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	assertIDs(t, "source UID FETCH sets", src.capturedNames("UID FETCH"), "1:500")
	if n := timesNamed(src.capturedNames("EXAMINE"), "B"); n != 1 {
		t.Errorf("source examined B %d times, want 1: only the planning scan", n)
	}
	if n := src.callCount("UID SEARCH"); n != 0 {
		t.Errorf("source saw %d UID SEARCH commands, want 0: a throttled source gets no check", n)
	}
	if n := dst.callCount("APPEND"); n != 500 {
		t.Errorf("destination saw %d APPEND commands, want 500: the delivered first batch", n)
	}
	got := out.String()
	if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
		t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
	}
	assertContainsAll(t, got,
		"⛔ Sync stopped: the source server [src] reports a rate limit or a full quota: ",
		"Account exceeded command or bandwidth limits",
		"500 messages uploaded, 103 errors occurred, 0 folders failed")
	assertContainsNone(t, got, "Cancelled", "Canceled")
}

func TestRunSync_When_TwoWorkersAndTheDestinationThrottles_Then_NoPlanStartsAfterTheStop(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": fakeMsgs(numberedIDs(600)...),
		"B": fakeMsgs(prefixedIDs("b", 600)...),
		"C": fakeMsgs("c1@x", "c2@x", "c3@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil, "C": nil})
	dst.refuseAppendWith(gmailThrottleReply)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("A", "A"), mapping("B", "B"), mapping("C", "C")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	if n := dst.callCount("APPEND"); n > 2 {
		t.Errorf("destination saw %d APPEND commands, want at most 2: one for each running plan", n)
	}
	sets := src.capturedNames("UID FETCH")
	if len(sets) > 2 || slices.ContainsFunc(sets, func(set string) bool { return set != "1:500" }) {
		t.Errorf("source UID FETCH sets = %v, want at most two, each 1:500", sets)
	}
	if n := timesNamed(src.capturedNames("EXAMINE"), "C"); n != 1 {
		t.Errorf("source examined C %d times, want 1: only the planning scan", n)
	}
	got := out.String()
	if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
		t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
	}
	assertContainsAll(t, got, destinationStopStart, "0 messages uploaded, 1203 errors occurred, 0 folders failed")
}

func TestRunSync_When_ThrottleStopsTheRun_Then_TheNextRunCopiesTheRest(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(6)...), "B": fakeMsgs("b1@x", "b2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil})
	dst.onCommand("APPEND", func() {
		if dst.callCount("APPEND") == 4 {
			dst.refuseAppendWith(gmailThrottleReply)
		}
	})
	cfg := e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"))

	first := &syncBuffer{}
	opts := e2eOptions(first, "")
	opts.autoConfirm = true
	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first run error = %v, want ErrSilentExit\noutput: %s", err, first.String())
	}
	assertContainsAll(t, first.String(), destinationStopStart, "3 messages uploaded, 5 errors occurred, 0 folders failed")
	assertIDs(t, "destination A after the first run", dst.folderMsgIDs("A"), "n1@x", "n2@x", "n3@x")
	assertIDs(t, "destination B after the first run", dst.folderMsgIDs("B"))

	dst.refuseAppendWith("")
	second := &syncBuffer{}
	opts = e2eOptions(second, "")
	opts.autoConfirm = true
	err = runSync(e2eContext(t), cfg, opts)

	if err != nil {
		t.Fatalf("second run error = %v, want success\noutput: %s", err, second.String())
	}
	assertContainsAll(t, second.String(), "Sync completed successfully")
	assertContainsNone(t, second.String(), "Sync stopped")
	assertIDs(t, "destination A after the second run", dst.folderMsgIDs("A"), numberedIDs(6)...)
	assertIDs(t, "destination B after the second run", dst.folderMsgIDs("B"), "b1@x", "b2@x")
}

func TestRunSync_When_StopReasonCarriesControlCharacters_Then_TheStopLineShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseAppendWith("NO [OVERQUOTA] Quota \x1b[2J\u202eevil exceeded")
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	_, rest, found := strings.Cut(out.String(), "⛔ Sync stopped")
	if !found {
		t.Fatalf("output has no stop line\noutput: %q", out.String())
	}
	line, _, _ := strings.Cut(rest, "\n")
	assertContainsAll(t, line, `Quota \x1b[2J\u202eevil exceeded`)
	assertContainsNone(t, line, "\x1b", "\u202e")
}

// prefixedIDs returns the Message-Ids prefix1@x to prefixN@x.
func prefixedIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d@x", prefix, i+1)
	}
	return ids
}

func TestRunSync_When_SourceEventsAreRandom_Then_NoMessageIsStoredTwiceAcrossTwoRuns(t *testing.T) {
	t.Parallel()

	for seed := uint64(1); seed <= 30; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()

			run := newRandomRun(rand.New(rand.NewPCG(seed, seed*104729)))
			n := min(len(run.events), 12)
			folderA := make([]fakeMsg, n)
			var folderX []fakeMsg
			for k := range n {
				folderA[k] = fakeMsg{msgID: run.id(k)}
				if k%5 == 2 {
					folderX = append(folderX, fakeMsg{msgID: run.id(k)})
				}
			}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": folderA})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"X": folderX})
			var held []string
			for k := range n {
				id := run.id(k)
				switch run.events[k] {
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
					continue
				}
				held = append(held, id)
				if run.dstRefuses[k] {
					dst.refuseAppendOf(id, "NO [CANNOT] message refused by policy")
				}
			}
			if run.batchRefused {
				src.endUIDFetchWith("A", "NO [CANNOT] fetch cut short")
			}
			if run.searchRefused {
				src.refuseUIDSearchWith("A", "NO [CANNOT] search refused")
			}
			switch run.throttle {
			case throttleDestinationAppend:
				dst.refuseAppendOf(run.id(run.throttleAt%n), gmailThrottleReply)
			// refuseOpenWith would also refuse the EXAMINE of the planning scan, so no plan would run. The EXAMINE kind throttles the body FETCH here instead.
			case throttleSourceFetch, throttleSourceExamine:
				src.endUIDFetchWith("A", throttleReply)
			case throttleSourceSearch:
				src.refuseUIDSearchWith("A", throttleReply)
			}
			cfg := e2eConfig(src, dst, 2, mapping("A", "X"))
			prefilled := dst.folderMsgIDs("X")

			var attempts [2]int
			var stored [2][]string
			for i := range 2 {
				out := &syncBuffer{}
				opts := e2eOptions(out, "")
				opts.autoConfirm = true
				if err := runSync(e2eContext(t), cfg, opts); err != nil && !errors.Is(err, ErrSilentExit) {
					t.Fatalf("run %d: %v\noutput: %s", i+1, err, out.String())
				}
				attempts[i] = dst.callCount("APPEND")
				stored[i] = dst.appended("X")
			}

			firstRun, secondRun := stored[0], stored[1][len(stored[0]):]
			for _, id := range secondRun {
				if slices.Contains(firstRun, id) || slices.Contains(prefilled, id) {
					t.Errorf("the second run stored %s again", id)
				}
			}
			inFolder := slices.Sorted(slices.Values(dst.folderMsgIDs("X")))
			if compacted := slices.Compact(slices.Clone(inFolder)); len(compacted) != len(inFolder) {
				t.Errorf("destination holds a Message-Id twice: %v", inFolder)
			}
			for _, body := range dst.folderBodies("X") {
				if body == "" {
					t.Error("destination holds an empty message")
				}
			}
			missingAfterFirstRun := 0
			for _, id := range held {
				if !slices.Contains(firstRun, id) && !slices.Contains(prefilled, id) {
					missingAfterFirstRun++
				}
			}
			if sent := attempts[1] - attempts[0]; sent > missingAfterFirstRun {
				t.Errorf("the second run sent %d APPEND commands for %d missing messages", sent, missingAfterFirstRun)
			}
			if run.throttle == throttleNone {
				for k := range n {
					deliverable := (run.events[k] == eventNone || run.events[k] == eventBodylessFirst) && !run.dstRefuses[k]
					id := run.id(k)
					if deliverable && !slices.Contains(firstRun, id) && !slices.Contains(prefilled, id) {
						t.Errorf("the first run did not copy %s, which the source delivers and the destination accepts", id)
					}
				}
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_ExistenceCheckAnswersOrIsRefused_Then_OnlyAnAnsweredCheckCountsAMessageAsGone(t *testing.T) {
	t.Parallel()

	const goneLine = "Source folder INBOX no longer holds "
	tests := []struct {
		name         string
		wantSummary  string
		wantGoneLine string
		refuseSearch bool
	}{
		{
			name:         "the check works",
			wantSummary:  "1 messages uploaded, 1 errors occurred, 0 folders failed",
			wantGoneLine: goneLine + "1 of the 3 planned messages for INBOX, so they are not copied",
		},
		{name: "the check is refused", refuseSearch: true, wantSummary: "1 messages uploaded, 2 errors occurred, 0 folders failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("good@x", "held@x", "gone@x")})
			src.omitInUIDFetch("INBOX", "held@x")
			if tt.refuseSearch {
				src.refuseUIDSearchWith("INBOX", "NO [CANNOT] search refused")
			}
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.removeMessage("INBOX", "gone@x") }}

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertContainsAll(t, out.String(), tt.wantSummary, "Sync completed with errors")
			assertContainsNone(t, out.String(), "Sync completed successfully")
			if tt.wantGoneLine != "" {
				assertContainsAll(t, out.String(), tt.wantGoneLine)
			} else {
				assertContainsNone(t, out.String(), goneLine)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "good@x")
			assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2:3")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_AWholeBatchIsRefusedInQuietMode_Then_OtherBatchesCopyAndTheSummaryCountsEveryUndeliveredMessage(t *testing.T) {
	t.Parallel()

	idsB := prefixedIDs("b", 1200)
	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"A": fakeMsgs(prefixedIDs("a", 3)...),
		"B": fakeMsgs(idsB...),
		"C": fakeMsgs(prefixedIDs("c", 2)...),
	})
	for _, id := range idsB[clientBatchSize : 2*clientBatchSize] {
		src.omitInUIDFetch("B", id)
	}
	src.endUIDFetchThatOmitsWith("B", gmailUnreadableReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1)
	first := &syncBuffer{}
	opts := e2eOptions(first, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first run error = %v, want ErrSilentExit\noutput: %s", err, first.String())
	}
	assertContainsAll(t, first.String(), "705 messages uploaded, 500 errors occurred, 0 folders failed")
	if got := len(dst.folderMsgIDs("B")); got != 700 {
		t.Errorf("destination B holds %d messages after the first run, want 700: the first and the last batch", got)
	}
	if n := src.callCount("UID FETCH"); n != 5 {
		t.Errorf("source saw %d UID FETCH commands, want 5: one for each batch of B and one for each of A and C", n)
	}

	second := &syncBuffer{}
	opts = e2eOptions(second, "")
	opts.quiet, opts.autoConfirm = true, true
	err = runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("second run error = %v, want ErrSilentExit\noutput: %s", err, second.String())
	}
	assertContainsAll(t, second.String(), "0 messages uploaded, 500 errors occurred, 0 folders failed")
	if got := len(dst.folderMsgIDs("B")); got != 700 {
		t.Errorf("destination B holds %d messages after the second run, want 700", got)
	}
	if n := dst.callCount("APPEND"); n != 705 {
		t.Errorf("destination saw %d APPEND commands over both runs, want 705", n)
	}
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceAnswersWithoutUID_Then_TheMessageIsAnErrorOnEveryRun(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("good@x", "nouid@x")})
	src.omitUIDInUIDFetch("INBOX", "nouid@x")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1, mapping("INBOX", "INBOX"))

	for run, wantPlanned := range []int{2, 1} {
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true

		err := runSync(e2eContext(t), cfg, opts)

		if !errors.Is(err, ErrSilentExit) {
			t.Fatalf("run %d error = %v, want ErrSilentExit\noutput: %s", run+1, err, out.String())
		}
		assertContainsAll(t, out.String(),
			fmt.Sprintf("INBOX → INBOX will copy %d messages", wantPlanned),
			fmt.Sprintf("%d messages uploaded, 1 errors occurred, 0 folders failed", wantPlanned-1),
		)
		assertContainsNone(t, out.String(), "no longer holds", "Sync completed successfully")
	}

	assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "good@x")
	assertIDs(t, "source UID SEARCH sets", src.capturedNames("UID SEARCH"), "2", "2")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_FirstPlanOfAHiddenSharedDestinationIsRebuiltBeforeTheRecheck_Then_OnlyItFailsAndTheNextRunCompletesTheFolder(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("x@x", "a1@x"), "B": fakeMsgs("x@x", "b1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "D": fakeMsgs("d0@x")})
	dst.hideFromList("D")
	cfg := e2eConfig(src, dst, 1, mapping("A", "D"), mapping("B", "D"))
	first := &syncBuffer{}
	opts := e2eOptions(first, "")
	opts.in = &hookReader{r: strings.NewReader("y\n"), hook: func() { src.rebuildFolder("A", 2, 1, 0) }}

	err := runSync(e2eContext(t), cfg, opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("first run error = %v, want ErrSilentExit\noutput: %s", err, first.String())
	}
	assertContainsAll(t, first.String(),
		"• A → D will copy 2 messages",
		"• B → D will copy 1 messages",
		folderFailuresHeader,
		"• A → D: source folder changed after the sync plan, run sync again",
		"1 messages uploaded, 0 errors occurred, 1 folders failed",
	)
	assertIDs(t, "dst D after the first run", dst.folderMsgIDs("D"), "d0@x", "b1@x")

	second := runSyncOnce(t, cfg)

	assertContainsAll(t, second, "Sync completed successfully")
	assertIDs(t, "dst D after the second run", slices.Sorted(slices.Values(dst.folderMsgIDs("D"))), "a1@x", "b1@x", "d0@x", "x@x")
	assertIDs(t, "dst APPENDs over both runs", slices.Sorted(slices.Values(dst.appended("D"))), "a1@x", "b1@x", "x@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_CreateRefusalCarriesControlCharactersInQuietMode_Then_TheFailureListShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{hostileWireName: fakeMsgs("a@x"), "Good": fakeMsgs("g@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreateOf(hostileWireName, "NO "+hostileReply)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.quiet, opts.autoConfirm = true, true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(hostileName, hostileName), mapping("Good", "Good")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertInOrder(t, got, folderFailuresHeader,
		"• "+hostileShownName+" → "+hostileShownName+": cannot create destination folder: [dst] failed to create mailbox "+hostileShownName+": "+hostileReplyShown)
	assertContainsAll(t, got, "1 messages uploaded, 0 errors occurred, 1 folders failed")
	assertContainsNone(t, got, hostileName, "\u202e", "\x1b[2J", "\x1b]0;pwned", "Failed to create folder")
	assertIDs(t, "dst Good", dst.folderMsgIDs("Good"), "g@x")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_SourceThrottlesAfterTheLastBodyIsCopied_Then_RunStopsAndTheNextRunHasNothingToCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "default output"},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x")})
			src.endUIDFetchWith("A", throttleReply)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil})
			cfg := e2eConfig(src, dst, 1, mapping("A", "A"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit: the stop is recorded although every body was copied\noutput: %s", err, out.String())
			}
			assertContainsAll(t, out.String(), "⛔ Sync stopped: the source server [src] reports a rate limit or a full quota", "3 messages uploaded, 0 errors occurred, 0 folders failed")
			assertContainsNone(t, out.String(), "Sync completed successfully")
			assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x", "a2@x", "a3@x")
			if n := src.callCount("UID SEARCH"); n != 0 {
				t.Errorf("source saw %d UID SEARCH commands, want 0: nothing was left undelivered", n)
			}

			fetches := src.callCount("UID FETCH")
			assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
			if n := src.callCount("UID FETCH"); n != fetches {
				t.Errorf("source saw %d more UID FETCH commands in the second run, want 0", n-fetches)
			}
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_DestinationThrottlesWithBodiesInFlight_Then_TheClosedSourceSessionCostsNoLogoutWait(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs(numberedIDs(600)...)})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil})
	dst.refuseAppendWith(gmailThrottleReply)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	start := time.Now()
	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A")), opts)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	// The default logout limit is 2 s. A closed session must fail its LOGOUT at once.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("runSync took %s, want it to end without waiting for the logout limit of a closed session", elapsed)
	}
	// The planning session logs out. The worker session was closed with 499 bodies in flight, so its LOGOUT never reaches the server.
	if n, logins := src.callCount("LOGOUT"), src.callCount("LOGIN"); n != 1 || logins != 2 {
		t.Errorf("source LOGOUT, LOGIN counts = %d, %d, want 1, 2", n, logins)
	}
	if n, logins := dst.callCount("LOGOUT"), dst.callCount("LOGIN"); n != 2 || logins != 2 {
		t.Errorf("destination LOGOUT, LOGIN counts = %d, %d, want 2, 2: its sessions stay usable", n, logins)
	}
}

func TestRunSync_When_DestinationThrottlesBeforeTheNextPlan_Then_ThePlansThatNeverStartedShowStopped(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "B": fakeMsgs("b1@x", "b2@x"), "C": fakeMsgs("c1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "B": nil, "C": nil})
	dst.refuseAppendWith(gmailThrottleReply)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"), mapping("C", "C")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	got := out.String()
	assertContainsAll(t, got, "1/3 Synced messages 0 (errors: 3) A → A", "2/3 Stopped B → B", "3/3 Stopped C → C", "0 messages uploaded, 6 errors occurred, 0 folders failed")
	for _, folder := range []string{"B", "C"} {
		if n := timesNamed(src.capturedNames("EXAMINE"), folder); n != 1 {
			t.Errorf("source examined %s %d times, want 1: only the planning scan", folder, n)
		}
	}
}

func TestRunSync_When_LowestGmailCopyHasAnEmptyBody_Then_TheHigherCopyKeepsItsOwnFlagsAndDate(t *testing.T) {
	t.Parallel()

	const id = "scan@printer"
	src := newStoreServer(t, "/", map[string][]fakeMsg{"Scans": {
		{msgID: id, body: subjectBody(id, "copy 1"), flags: `\Seen`, internalDate: "01-Jan-2020 10:00:00 +0100"},
		{msgID: id, body: subjectBody(id, "copy 2"), flags: `\Flagged $Label1`, internalDate: " 2-Feb-2021 11:30:00 -0500"},
	}})
	src.answerBodyAt("Scans", 1, "NIL")
	dst := newGmailDestination(t, map[string][]fakeMsg{})
	cfg := e2eConfig(src, dst, 1, mapping("Scans", "Scans"))

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "Sync completed successfully")
	assertIDs(t, "dst APPENDs to Scans", dst.appended("Scans"), id)
	assertIDs(t, "dst APPEND flag lists", dst.appendedFlagLists("Scans"), `(\Flagged $Label1)`)
	assertIDs(t, "dst APPEND dates", dst.appendedDates("Scans"), " 2-Feb-2021 11:30:00 -0500")
	if bodies := dst.folderBodies("Scans"); !slices.Equal(bodies, []string{subjectBody(id, "copy 2")}) {
		t.Errorf("dst Scans bodies = %q, want only the second copy", bodies)
	}
	assertContainsAll(t, runSyncOnce(t, cfg), "All folders already synced!")
	assertReadOnly(t, "source", src)
}

func TestRunSync_When_CreateGetsAThrottledReply_Then_RunStopsBeforeAnyWorker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "default output"},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x"), "B": fakeMsgs("b1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseCreateOf("A", gmailThrottleReply)
			cfg := e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"))
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), cfg, opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A")
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LOGIN"); n != 1 {
					t.Errorf("%s LOGIN count = %d, want 1: no worker connection after the stop", name, n)
				}
			}
			for _, verb := range []string{"EXAMINE", "STATUS", "FETCH"} {
				if n := dst.callCount(verb); n != 0 {
					t.Errorf("dst saw %d %s commands, want 0: both folders are missing", n, verb)
				}
			}
			if n := src.callCount("UID FETCH"); n != 0 {
				t.Errorf("src saw %d UID FETCH commands, want 0", n)
			}
			got := out.String()
			if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
				t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
			}
			assertInOrder(t, got, destinationStopStart, "Account exceeded command or bandwidth limits. (Failure)", "0 messages uploaded, 3 errors occurred, 0 folders failed")
			assertContainsNone(t, got, "Syncing messages", folderFailuresHeader, "Sync completed successfully", "Cancelled", "Canceled")
			if tt.quiet {
				assertContainsNone(t, got, "Failed to create folder")
			} else {
				assertContainsAll(t, got, `Failed to create folder "A"`)
			}

			dst.refuseCreateOf("A", "")
			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "Sync completed successfully")
			assertContainsNone(t, second, "Sync stopped")
			assertIDs(t, "dst A after the second run", dst.folderMsgIDs("A"), "a1@x", "a2@x")
			assertIDs(t, "dst B after the second run", dst.folderMsgIDs("B"), "b1@x")
			assertReadOnly(t, "source", src)
		})
	}
}

func TestRunSync_When_CreateIsRefusedAndThenThrottled_Then_OnlyTheRefusedFolderFails(t *testing.T) {
	t.Parallel()

	const failureLine = "• A → A: cannot create destination folder: [dst] failed to create mailbox A: Invalid folder name"
	tests := []struct {
		name       string
		wantHeader int
		quiet      bool
	}{
		{name: "default output", wantHeader: 2},
		{name: "quiet output", wantHeader: 1, quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x", "b2@x"), "C": fakeMsgs("c1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.refuseCreateOf("A", "NO [CANNOT] Invalid folder name")
			dst.refuseCreateOf("B", gmailThrottleReply)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B"), mapping("C", "C")), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "A", "B")
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LOGIN"); n != 1 {
					t.Errorf("%s LOGIN count = %d, want 1: no worker connection after the stop", name, n)
				}
			}
			got := out.String()
			if n := strings.Count(got, folderFailuresHeader); n != tt.wantHeader {
				t.Errorf("output holds the failure header %d times, want %d\noutput: %q", n, tt.wantHeader, got)
			}
			if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
				t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
			}
			if tt.quiet {
				assertInOrder(t, got, folderFailuresHeader, failureLine, destinationStopStart, "0 messages uploaded, 3 errors occurred, 1 folders failed")
			} else {
				assertInOrder(t, got, folderFailuresHeader, failureLine, folderFailuresHeader, failureLine, destinationStopStart, "0 messages uploaded, 3 errors occurred, 1 folders failed")
			}
			assertContainsNone(t, got, "Syncing messages", "Sync completed successfully", "Cancelled", "Canceled")
			assertContainsNone(t, got, "• B → B:", "• C → C:")
		})
	}
}

// hiddenRecheckServers returns two servers whose destination folders H1 and H2 are hidden from LIST and each hold one of the two messages of the same source folder.
func hiddenRecheckServers(t *testing.T) (src, dst *fakeServer) {
	t.Helper()
	src = newStoreServer(t, "/", map[string][]fakeMsg{"H1": fakeMsgs("a1@x", "a2@x"), "H2": fakeMsgs("b1@x", "b2@x")})
	dst = newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "H1": fakeMsgs("a1@x"), "H2": fakeMsgs("b1@x")})
	dst.hideFromList("H1", "H2")
	return src, dst
}

func TestRunSync_When_DestinationRecheckIsThrottled_Then_RunStopsBeforeAnyWorker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "default output"},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := hiddenRecheckServers(t)
			dst.refuseOpenWith("H1", gmailThrottleReply)
			sourceCommands := func() int64 {
				return int64(src.callCount("EXAMINE") + src.callCount("STATUS") + src.callCount("FETCH") + src.callCount("UID"))
			}
			// The planning scan is over when the first CREATE arrives.
			var afterPlanning atomic.Int64
			dst.onCommand("CREATE", func() { afterPlanning.Store(sourceCommands()) })
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("H1", "H1"), mapping("H2", "H2")), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "H1", "H2")
			assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "H1")
			for _, verb := range []string{"STATUS", "FETCH", "APPEND"} {
				if n := dst.callCount(verb); n != 0 {
					t.Errorf("dst saw %d %s commands, want 0", n, verb)
				}
			}
			for _, folder := range []string{"H1", "H2"} {
				if n := timesNamed(src.capturedNames("EXAMINE"), folder); n != 1 {
					t.Errorf("src examined %s %d times, want 1: only the planning scan", folder, n)
				}
			}
			if n := src.callCount("STATUS"); n != 0 {
				t.Errorf("src saw %d STATUS commands, want 0", n)
			}
			if got, want := sourceCommands(), afterPlanning.Load(); got != want || want == 0 {
				t.Errorf("src saw %d scan commands in the whole run and %d during planning, want the same non-zero count", got, want)
			}
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LOGIN"); n != 1 {
					t.Errorf("%s LOGIN count = %d, want 1: no worker connection after the stop", name, n)
				}
			}
			got := out.String()
			if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
				t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
			}
			assertInOrder(t, got, destinationStopStart, "Account exceeded command or bandwidth limits. (Failure)", "0 messages uploaded, 4 errors occurred, 0 folders failed")
			assertContainsNone(t, got, "already exists and holds", "no longer holds", folderFailuresHeader, "Syncing messages", "Sync completed successfully", "Cancelled", "Canceled")
			assertIDs(t, "dst H1", dst.folderMsgIDs("H1"), "a1@x")
			assertIDs(t, "dst H2", dst.folderMsgIDs("H2"), "b1@x")
		})
	}
}

func TestRunSync_When_SourceRecheckIsThrottled_Then_RunStopsBeforeAnyWorker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		quiet bool
	}{
		{name: "default output"},
		{name: "quiet output", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := hiddenRecheckServers(t)
			// The recheck can reuse the selected session and send STATUS, so both commands get the reply.
			dst.onCommand("CREATE", func() {
				src.refuseOpenWith("H1", gmailThrottleReply)
				src.refuseStatusWith("H1", gmailThrottleReply)
			})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("H1", "H1"), mapping("H2", "H2")), opts)

			if !errors.Is(err, ErrSilentExit) {
				t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
			}
			assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "H1")
			if n := timesNamed(src.capturedNames("EXAMINE"), "H1"); n != 2 {
				t.Errorf("src examined H1 %d times, want 2: the planning scan and the recheck", n)
			}
			if n := timesNamed(src.capturedNames("EXAMINE"), "H2"); n != 1 {
				t.Errorf("src examined H2 %d times, want 1: only the planning scan", n)
			}
			if n := src.callCount("UID FETCH"); n != 0 {
				t.Errorf("src saw %d UID FETCH commands, want 0", n)
			}
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LOGIN"); n != 1 {
					t.Errorf("%s LOGIN count = %d, want 1: no worker connection after the stop", name, n)
				}
			}
			got := out.String()
			if n := strings.Count(got, "⛔ Sync stopped"); n != 1 {
				t.Errorf("output holds the stop line %d times, want 1\noutput: %q", n, got)
			}
			assertInOrder(t, got,
				"⛔ Sync stopped: the source server [src] reports a rate limit or a full quota: ",
				"Account exceeded command or bandwidth limits. (Failure)",
				"0 messages uploaded, 4 errors occurred, 0 folders failed")
			assertContainsNone(t, got, "already exists and holds", "no longer holds", folderFailuresHeader, "Syncing messages", "Sync completed successfully", "Cancelled", "Canceled")
		})
	}
}

func Test_recheckExistingDestinations_When_DestinationScanIsThrottled_Then_ReturnsThePlansAndSendsNothingMore(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x"), "B": fakeMsgs("b1@x", "b2@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"D": fakeMsgs("a1@x"), "E": fakeMsgs("b1@x")})
	dst.refuseOpenWith("D", gmailThrottleReply)
	plans := []FolderSyncPlan{sharedPlan("A", "D", 200, 1, 2), sharedPlan("B", "E", 200, 1, 2)}
	existing := map[string]struct{}{"D": {}, "E": {}}
	out := &syncBuffer{}
	var stop throttleStop

	kept, failures, err := recheckExistingDestinations(t.Context(), out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, existing, &stop)

	if err != nil || failures != nil {
		t.Fatalf("error = %v, failures = %v, want both nil", err, failures)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %+v, want both input plans", kept)
	}
	for i, want := range []struct{ src, dst string }{{"A", "D"}, {"B", "E"}} {
		got := kept[i]
		if got.SourceFolder != want.src || got.DestinationFolder != want.dst || !slices.Equal(got.SrcUIDs, []uint32{1, 2}) || got.NewMessages != 2 || got.NewSize != 200 {
			t.Errorf("kept[%d] = %+v, want the input plan %s → %s with UIDs 1 and 2", i, got, want.src, want.dst)
		}
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
	cause := stop.get()
	if cause == nil || cause.side != sideDestination {
		t.Fatalf("stop = %+v, want the destination side", cause)
	}
	if !strings.Contains(cause.err.Error(), "Account exceeded command or bandwidth limits") {
		t.Errorf("stop error = %v, want the throttle reply", cause.err)
	}
	assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "D")
	for _, verb := range []string{"STATUS", "FETCH"} {
		if n := dst.callCount(verb); n != 0 {
			t.Errorf("dst saw %d %s commands, want 0", n, verb)
		}
	}
	for _, verb := range []string{"EXAMINE", "STATUS", "FETCH"} {
		if n := src.callCount(verb); n != 0 {
			t.Errorf("src saw %d %s commands, want 0", n, verb)
		}
	}
}

func Test_recheckExistingDestinations_When_SharedSourceScanIsThrottled_Then_NoLaterSourceIsScanned(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x"), "B": fakeMsgs("b1@x")})
	src.refuseOpenWith("A", gmailThrottleReply)
	src.refuseStatusWith("A", gmailThrottleReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"S": fakeMsgs("a1@x")})
	plans := []FolderSyncPlan{sharedPlan("A", "S", 200, 1, 2), sharedPlan("B", "S", 100, 1)}
	out := &syncBuffer{}
	var stop throttleStop

	kept, failures, err := recheckExistingDestinations(t.Context(), out, false, newAppClient(t, src, "src"), newAppClient(t, dst, "dst"), plans, map[string]struct{}{"S": {}}, &stop)

	if err != nil || failures != nil {
		t.Fatalf("error = %v, failures = %v, want both nil", err, failures)
	}
	if len(kept) != 2 || kept[0].SourceFolder != "A" || kept[1].SourceFolder != "B" {
		t.Errorf("kept = %+v, want both input plans in order", kept)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty", out.String())
	}
	cause := stop.get()
	if cause == nil || cause.side != sideSource {
		t.Fatalf("stop = %+v, want the source side", cause)
	}
	assertIDs(t, "src EXAMINE names", src.capturedNames("EXAMINE"), "A")
	if n := src.callCount("STATUS"); n != 0 {
		t.Errorf("src saw %d STATUS commands, want 0: B must not be scanned", n)
	}
	assertIDs(t, "dst EXAMINE names", dst.capturedNames("EXAMINE"), "S")
}

func TestRunSync_When_ContextEndsWithAThrottledReply_Then_ReturnsCanceledWithoutStopLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		build func(t *testing.T, cancel context.CancelFunc) (src, dst *fakeServer, mappings []config.DirectoryMapping)
		name  string
	}{
		{
			name: "the first CREATE gets the reply",
			build: func(t *testing.T, cancel context.CancelFunc) (*fakeServer, *fakeServer, []config.DirectoryMapping) {
				src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
				dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
				dst.addConnHandler(scriptedHandler(dst, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
					if verb != "CREATE" {
						return "", false
					}
					cancel()
					return gmailThrottleReply, true
				}))
				return src, dst, []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B")}
			},
		},
		{
			name: "the scan of the check after CREATE gets the reply",
			build: func(t *testing.T, cancel context.CancelFunc) (*fakeServer, *fakeServer, []config.DirectoryMapping) {
				src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x")})
				dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")})
				dst.hideFromList("Work")
				dst.refuseOpenWith("Work", gmailThrottleReply)
				// Work is not cached, so the planning scan sends no EXAMINE and this one is the check's.
				dst.onCommand("EXAMINE", cancel)
				return src, dst, []config.DirectoryMapping{mapping("Work", "Work")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(e2eContext(t))
			t.Cleanup(cancel)
			src, dst, mappings := tt.build(t, cancel)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(ctx, e2eConfig(src, dst, 1, mappings...), opts)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled\noutput: %s", err, out.String())
			}
			assertContainsNone(t, out.String(), "Sync stopped", "Sync completed", "errors occurred")
			if n := dst.callCount("APPEND"); n != 0 {
				t.Errorf("dst saw %d APPEND commands, want 0", n)
			}
		})
	}
}

// waitForCopyFrame returns after the first frame of the copy trackers, so a later redraw has rows to rewind. It runs on a server goroutine, so it uses Errorf.
func waitForCopyFrame(t *testing.T, out *syncBuffer) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, after, ok := strings.Cut(out.String(), "📥 Syncing messages..."); ok && strings.Contains(after, "→") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the progress writer drew no frame of the copy trackers within 5s")
}

// redrawRuns returns, for each redraw in raw, how many tracker rows it rewinds.
func redrawRuns(raw string) []int {
	const rewind = "\x1b[A\x1b[K"
	var runs []int
	for {
		i := strings.Index(raw, rewind)
		if i < 0 {
			return runs
		}
		raw = raw[i:]
		n := 0
		for strings.HasPrefix(raw, rewind) {
			n++
			raw = raw[len(rewind):]
		}
		runs = append(runs, n)
	}
}

// waitForRedraws returns after n redraws follow the output offset from, so the last one was sized after from. It runs on a server goroutine, so it uses Errorf.
func waitForRedraws(t *testing.T, out *syncBuffer, from, n int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(redrawRuns(out.String()[from:])) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the progress writer drew fewer than %d redraws within 5s", n)
}

func TestRunSync_When_PlanAndWorkerCountsDiffer_Then_EachRedrawHoldsAtMostOneTrackerPerBusyWorker(t *testing.T) {
	t.Parallel()

	const copyMarker = "📥 Syncing messages..."
	tests := []struct {
		setup   func(dst *fakeServer)
		name    string
		workers int
		plans   int
	}{
		{name: "one worker, four plans", workers: 1, plans: 4},
		{name: "two workers, six plans", workers: 2, plans: 6},
		{name: "two plans and four workers", workers: 4, plans: 2},
		{
			name:    "throttle stop",
			workers: 1,
			plans:   5,
			setup:   func(dst *fakeServer) { dst.refuseAppendWith(gmailThrottleReply) },
		},
		{
			name:    "throttle stop with two workers",
			workers: 2,
			plans:   6,
			setup:   func(dst *fakeServer) { dst.refuseAppendWith(gmailThrottleReply) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			names := []string{"A", "B", "C", "D", "E", "F"}[:tt.plans]
			srcFolders := make(map[string][]fakeMsg, len(names))
			dstFolders := make(map[string][]fakeMsg, len(names))
			mappings := make([]config.DirectoryMapping, 0, len(names))
			for _, name := range names {
				srcFolders[name] = fakeMsgs(strings.ToLower(name) + "@x")
				dstFolders[name] = nil
				mappings = append(mappings, mapping(name, name))
			}
			src := newStoreServer(t, "/", srcFolders)
			dst := newStoreServer(t, "/", dstFolders)
			if tt.setup != nil {
				tt.setup(dst)
			}
			out := &syncBuffer{}
			// Each APPEND waits for two redraws that follow it, and the second one is sized after the first.
			dst.onCommand("APPEND", func() { waitForRedraws(t, out, len(out.String()), 2) })
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, tt.workers, mappings...), opts)

			raw := out.String()
			throttled := tt.setup != nil
			if throttled {
				if !errors.Is(err, ErrSilentExit) {
					t.Fatalf("error = %v, want ErrSilentExit\noutput: %q", err, raw)
				}
				assertContainsAll(t, raw, fmt.Sprintf("0 messages uploaded, %d errors occurred, 0 folders failed", tt.plans))
			} else {
				if err != nil {
					t.Fatalf("runSync: %v\noutput: %q", err, raw)
				}
				assertContainsAll(t, raw, "Sync completed successfully")
			}
			assertContainsNone(t, raw, "Waiting")
			// A worker connects only when the run has a plan for it.
			bound := min(tt.workers, tt.plans)
			for _, srv := range []*fakeServer{src, dst} {
				if n := srv.callCount("LOGIN"); n != 1+bound {
					t.Errorf("LOGIN count = %d, want %d: the planning connection and one for each worker", n, 1+bound)
				}
			}

			start := strings.LastIndex(raw, copyMarker)
			if start < 0 {
				t.Fatalf("output does not hold %q\noutput: %q", copyMarker, raw)
			}
			runs := redrawRuns(raw[start:])
			if len(runs) == 0 {
				t.Fatalf("the copy drew no redraw\noutput: %q", raw[start:])
			}
			if len(runs) < tt.plans/tt.workers && !throttled {
				t.Errorf("the copy drew %d redraws, want at least %d: the bound would hold for the wrong reason", len(runs), tt.plans/tt.workers)
			}
			if largest := slices.Max(runs); largest > bound {
				t.Errorf("a redraw rewinds %d tracker rows, want at most %d (one for each worker that has a plan); runs: %v", largest, bound, runs)
			}

			rows := screenRows(t, raw)
			for k, name := range names {
				prefix := fmt.Sprintf("%d/%d ", k+1, tt.plans)
				var final []string
				switch {
				case !throttled:
					final = []string{prefix + "Synced messages 1 " + name + " → " + name, "✓ done"}
				case k == 0 && tt.workers == 1:
					final = []string{prefix + "Synced messages 0 (errors: 1) " + name + " → " + name, "✗ error"}
				case k < tt.workers:
					// Which of the first plans sees the stop before its APPEND depends on the workers, so only the mark is fixed.
					final = []string{prefix, "✗ error"}
				default:
					final = []string{prefix + "Stopped " + name + " → " + name, "✗ error"}
				}
				assertOneRow(t, rows, prefix)
				assertOneRow(t, rows, final...)
				if throttled && k >= tt.workers {
					if n := strings.Count(raw, prefix+"Stopped"); n != 1 {
						t.Errorf("output holds %q %d times, want 1", prefix+"Stopped", n)
					}
				}
			}
			if stale := rowsHolding(rows, "→", "%"); len(stale) != 0 {
				t.Errorf("rows %v hold a tracker that is still in progress\nscreen:\n%s", stale, strings.Join(rows, "\n"))
			}
		})
	}
}

func TestRunSync_When_TheCopyEnds_Then_TheLinesAfterItStayOnScreen(t *testing.T) {
	t.Parallel()

	const (
		successLine = "✨ Sync completed successfully. ✨"
		failureLine = "• Bad → Bad: cannot scan source folder: [src] cannot select folder Bad: Internal error"
	)
	tests := []struct {
		src         map[string][]fakeMsg
		dst         map[string][]fakeMsg
		setup       func(src, dst *fakeServer)
		wantErr     error
		name        string
		firstAfter  string
		last        string
		mappings    []config.DirectoryMapping
		trackerRows [][]string
		wantTail    []string
		firstRows   int
		quiet       bool
	}{
		{
			name:        "success",
			src:         map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x", "m3@x", "m4@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			mappings:    []config.DirectoryMapping{mapping("INBOX", "INBOX")},
			firstAfter:  successLine,
			last:        successLine,
			trackerRows: [][]string{{"1/1 Synced messages 4 INBOX → INBOX", "✓ done"}},
			firstRows:   1,
			wantTail:    []string{successLine},
		},
		{
			name:       "refused appends",
			src:        map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x")},
			dst:        map[string][]fakeMsg{"INBOX": nil, "A": nil},
			setup:      func(_, dst *fakeServer) { dst.refuseAppend() },
			wantErr:    ErrSilentExit,
			mappings:   []config.DirectoryMapping{mapping("A", "A")},
			firstAfter: "❌ Sync completed with errors. 0 messages uploaded, 3 errors occurred, 0 folders failed",
			last:       "❌ Sync completed with errors. 0 messages uploaded, 3 errors occurred, 0 folders failed",
			trackerRows: [][]string{
				{"1/1 Synced messages 0 (errors: 3) A → A", "✗ error"},
			},
			wantTail:  []string{"❌ Sync completed with errors. 0 messages uploaded, 3 errors occurred, 0 folders failed"},
			firstRows: 1,
		},
		{
			name:       "a folder that cannot be scanned",
			src:        map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "Bad": fakeMsgs("z@x")},
			dst:        map[string][]fakeMsg{"INBOX": nil, "A": nil},
			setup:      func(src, _ *fakeServer) { src.refuseOpenWith("Bad", "NO [SERVERBUG] Internal error") },
			wantErr:    ErrSilentExit,
			mappings:   []config.DirectoryMapping{mapping("A", "A"), mapping("Bad", "Bad")},
			firstAfter: folderFailuresHeader,
			last:       "❌ Sync completed with errors. 1 messages uploaded, 0 errors occurred, 1 folders failed",
			trackerRows: [][]string{
				{"1/1 Synced messages 1 A → A", "✓ done"},
			},
			wantTail: []string{
				"",
				folderFailuresHeader,
				failureLine,
				"❌ Sync completed with errors. 1 messages uploaded, 0 errors occurred, 1 folders failed",
			},
			firstRows: 2,
		},
		{
			name: "refused appends and a failed folder",
			src:  map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "Bad": fakeMsgs("b1@x")},
			dst:  map[string][]fakeMsg{"INBOX": nil, "A": nil},
			setup: func(src, dst *fakeServer) {
				src.refuseOpenWith("Bad", "NO [SERVERBUG] Internal error")
				dst.refuseAppend()
			},
			wantErr:     ErrSilentExit,
			mappings:    []config.DirectoryMapping{mapping("A", "A"), mapping("Bad", "Bad")},
			firstAfter:  folderFailuresHeader,
			last:        "❌ Sync completed with errors. 0 messages uploaded, 3 errors occurred, 1 folders failed",
			trackerRows: [][]string{{"1/1 Synced messages 0 (errors: 3) A → A", "✗ error"}},
			wantTail: []string{
				"",
				folderFailuresHeader,
				failureLine,
				"❌ Sync completed with errors. 0 messages uploaded, 3 errors occurred, 1 folders failed",
			},
			firstRows: 2,
		},
		{
			name:        "throttle stop",
			src:         map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "B": fakeMsgs("b1@x", "b2@x")},
			dst:         map[string][]fakeMsg{"A": nil, "B": nil},
			setup:       func(_, dst *fakeServer) { dst.refuseAppendWith(gmailThrottleReply) },
			wantErr:     ErrSilentExit,
			mappings:    []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B")},
			firstAfter:  destinationStopStart,
			last:        "❌ Sync completed with errors. 0 messages uploaded, 5 errors occurred, 0 folders failed",
			trackerRows: [][]string{{"1/2 Synced messages 0 (errors: 3) A → A", "✗ error"}, {"2/2 Stopped B → B", "✗ error"}},
			wantTail: []string{
				destinationStopStart,
				"❌ Sync completed with errors. 0 messages uploaded, 5 errors occurred, 0 folders failed",
			},
			firstRows: 1,
		},
		{
			name: "a throttle stop with an unstarted plan and a folder that cannot be scanned",
			src: map[string][]fakeMsg{
				"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "B": fakeMsgs("b1@x", "b2@x"), "Bad": fakeMsgs("z@x"),
			},
			dst: map[string][]fakeMsg{"A": nil, "B": nil},
			setup: func(src, dst *fakeServer) {
				src.refuseOpenWith("Bad", "NO [SERVERBUG] Internal error")
				dst.refuseAppendWith(gmailThrottleReply)
			},
			wantErr:    ErrSilentExit,
			mappings:   []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B"), mapping("Bad", "Bad")},
			firstAfter: folderFailuresHeader,
			last:       "❌ Sync completed with errors. 0 messages uploaded, 5 errors occurred, 1 folders failed",
			trackerRows: [][]string{
				{"1/2 Synced messages 0 (errors: 3) A → A", "✗ error"},
				{"2/2 Stopped B → B", "✗ error"},
			},
			wantTail: []string{
				"",
				folderFailuresHeader,
				failureLine,
				destinationStopStart,
				"❌ Sync completed with errors. 0 messages uploaded, 5 errors occurred, 1 folders failed",
			},
			firstRows: 2,
		},
		{
			name:       "throttle stop with quiet",
			src:        map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "B": fakeMsgs("b1@x", "b2@x")},
			dst:        map[string][]fakeMsg{"A": nil, "B": nil},
			setup:      func(_, dst *fakeServer) { dst.refuseAppendWith(gmailThrottleReply) },
			wantErr:    ErrSilentExit,
			mappings:   []config.DirectoryMapping{mapping("A", "A"), mapping("B", "B")},
			firstAfter: destinationStopStart,
			last:       "❌ Sync completed with errors. 0 messages uploaded, 5 errors occurred, 0 folders failed",
			quiet:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, "/", tt.dst)
			if tt.setup != nil {
				tt.setup(src, dst)
			}
			out := &syncBuffer{}
			if !tt.quiet {
				dst.onCommand("APPEND", func() { waitForCopyFrame(t, out) })
			}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet = true, tt.quiet

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v\noutput: %q", err, tt.wantErr, out.String())
			}
			raw := out.String()
			if !strings.HasSuffix(raw, tt.last+"\n") {
				t.Errorf("output does not end with the summary line %q\noutput: %q", tt.last, raw)
			}
			if tt.quiet {
				if strings.Contains(raw, "\x1b[") {
					t.Errorf("quiet output holds an escape sequence\noutput: %q", raw)
				}
				if n := strings.Count(raw, tt.firstAfter); n != 1 {
					t.Errorf("quiet output holds %q %d times, want 1\noutput: %q", tt.firstAfter, n, raw)
				}
				return
			}
			start := strings.LastIndex(raw, tt.firstAfter)
			if start < 0 {
				t.Fatalf("output does not hold %q\noutput: %q", tt.firstAfter, raw)
			}
			for _, rewind := range []string{"\x1b[A", "\x1b[K"} {
				if strings.Contains(raw[start:], rewind) {
					t.Errorf("output holds %q after %q\ntail: %q", rewind, tt.firstAfter, raw[start:])
				}
			}

			rows := screenRows(t, raw)
			if got := rows[len(rows)-1]; got != tt.last {
				t.Errorf("last row = %q, want %q\nscreen:\n%s", got, tt.last, strings.Join(rows, "\n"))
			}
			assertOneRow(t, rows, tt.last)
			if n := len(rowsHolding(rows, tt.firstAfter)); n != tt.firstRows {
				t.Errorf("%d rows hold %q, want %d\nscreen:\n%s", n, tt.firstAfter, tt.firstRows, strings.Join(rows, "\n"))
			}
			lastTracker := -1
			for _, parts := range tt.trackerRows {
				i := assertOneRow(t, rows, parts...)
				if i < 0 {
					return
				}
				lastTracker = max(lastTracker, i)
			}
			tail := rows[lastTracker+1:]
			if len(tail) != len(tt.wantTail) {
				t.Fatalf("%d rows follow the last tracker row, want %d\nscreen:\n%s", len(tail), len(tt.wantTail), strings.Join(rows, "\n"))
			}
			for i, want := range tt.wantTail {
				if (want == "" && tail[i] != "") || !strings.Contains(tail[i], want) {
					t.Errorf("row %d after the trackers = %q, want it to hold %q\nscreen:\n%s", i, tail[i], want, strings.Join(rows, "\n"))
				}
			}
			if stale := rowsHolding(rows, "→", "%"); len(stale) != 0 {
				t.Errorf("rows %v hold a tracker that is still in progress\nscreen:\n%s", stale, strings.Join(rows, "\n"))
			}
		})
	}
}

func TestRunSync_When_WorkerSourceReconnects_Then_VerboseRunShowsTheWorkerLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "verbose", verbose: true},
		{name: "not verbose", verbose: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msgs := []fakeMsg{{msgID: "m1@x", envelopeID: "<m1@x>"}, {msgID: "m2@x", envelopeID: "<m2@x>"}}
			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": msgs})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			// The first LOGIN is the planning client and the second is the worker, so only the worker's EXAMINE gets the BYE.
			src.onCommand("LOGIN", func() {
				if src.callCount("LOGIN") == 2 {
					src.dropOnceOnOpen("INBOX")
				}
			})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.verbose = tt.verbose

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			if n := src.callCount("DROP"); n != 1 {
				t.Errorf("source dropped %d connections, want 1", n)
			}
			if n := src.callCount("LOGIN"); n != 3 {
				t.Errorf("source saw %d LOGIN commands, want 3", n)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
			assertContainsAll(t, out.String(), "Sync completed successfully")
			if tt.verbose {
				// Each line shows once, inside the copy section, so a writer that stops early would lose it.
				rows := screenRows(t, out.String())
				section := assertOneRow(t, rows, "📥 Syncing messages...")
				summary := assertOneRow(t, rows, "✨ Sync completed successfully. ✨")
				for _, line := range []string{
					"[src-w1] 🔄 Reconnect attempt 1...",
					"[src-w1] 🔄 Reconnected successfully",
					`[dst-w1] Message "<m1@x>" appended to INBOX`,
					`[dst-w1] Message "<m2@x>" appended to INBOX`,
				} {
					if i := assertOneRow(t, rows, line); i >= 0 && section >= 0 && summary >= 0 && (i <= section || i >= summary) {
						t.Errorf("row %d (%q) is outside the copy section, rows %d to %d", i, line, section, summary)
					}
				}
				return
			}
			assertContainsNone(t, out.String(), "Reconnect attempt", "[src-w1]", "[dst-w1]")
		})
	}
}

func TestRunSync_When_WorkerSourceGoesSilent_Then_TheTripLineShowsWithoutVerbose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		quiet    bool
		wantTrip bool
	}{
		{name: "normal", wantTrip: true},
		{name: "quiet", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			// The planning client sends no UID command, so the stall hits the UID FETCH of the worker.
			src.stallNth(t, "UID", 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.quiet = tt.quiet
			opts.clientOpts.IdleTimeout = time.Second

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			if n := src.callCount("LOGIN"); n != 3 {
				t.Errorf("source saw %d LOGIN commands, want 3", n)
			}
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "m1@x", "m2@x")
			assertContainsAll(t, out.String(), "Sync completed successfully")
			if tt.wantTrip {
				assertContainsAll(t, out.String(), "[src-w1] No data from server for 1s, reconnecting")
				assertOneRow(t, screenRows(t, out.String()), "[src-w1] No data from server for 1s, reconnecting")
				return
			}
			assertContainsNone(t, out.String(), "No data from server")
		})
	}
}

func TestRunSync_When_DestinationGoesSilentDuringCreate_Then_TheTripLineShows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verbose bool
	}{
		{name: "not verbose"},
		{name: "verbose", verbose: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			dst.stallNth(t, "CREATE", 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.verbose = tt.verbose
			opts.clientOpts.IdleTimeout = time.Second

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "A")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			assertInOrder(t, out.String(), "[dst] No data from server for 1s, reconnecting", "📥 Syncing messages")
			assertOneRow(t, screenRows(t, out.String()), "[dst] No data from server for 1s, reconnecting")
			if tt.verbose {
				assertContainsAll(t, out.String(), "[dst] 🔄 Reconnect attempt 1...", "[dst] 🔄 Reconnected successfully")
			} else {
				assertContainsNone(t, out.String(), "Reconnect attempt")
			}
			assertIDs(t, "dst A", dst.folderMsgIDs("A"), "m1@x", "m2@x")
		})
	}
}

func TestRunSync_When_DestinationGoesSilentDuringTheCheckAfterCreate_Then_TheTripLineShows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		quiet    bool
		wantTrip bool
	}{
		{name: "normal", wantTrip: true},
		{name: "quiet", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "A": fakeMsgs("m1@x")})
			dst.hideFromList("A")
			// A hidden folder gets no destination scan while the plan is built, so the stall hits the check after CREATE.
			dst.stallNth(t, "FETCH", 1)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.quiet = tt.quiet
			opts.clientOpts.IdleTimeout = time.Second

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			if n := dst.callCount("FETCH"); n != 2 {
				t.Errorf("destination saw %d FETCH commands, want 2: the stalled one and its retry", n)
			}
			assertIDs(t, "dst APPENDs to A", dst.appended("A"), "m2@x")
			if tt.wantTrip {
				assertInOrder(t, out.String(),
					"[dst] No data from server for 1s, reconnecting",
					"already exists and holds 1 of the 2 planned messages",
					"📥 Syncing messages",
				)
				return
			}
			assertContainsNone(t, out.String(), "No data from server")
		})
	}
}

func TestRunSync_When_SourceGoesSilentDuringTheCheckAfterCreate_Then_TheTripLineShows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		quiet    bool
		wantTrip bool
	}{
		{name: "normal", wantTrip: true},
		{name: "quiet", quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "A": fakeMsgs("m1@x")})
			dst.hideFromList("A")
			// The plan scan sends the first source FETCH and the copy sends only UID FETCH, so the second FETCH is the one in the check after CREATE.
			src.stallNth(t, "FETCH", 2)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true
			opts.quiet = tt.quiet
			opts.clientOpts.IdleTimeout = time.Second

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			if n := src.callCount("FETCH"); n != 3 {
				t.Errorf("source saw %d FETCH commands, want 3: the plan scan, the stalled one and its retry", n)
			}
			assertIDs(t, "dst APPENDs to A", dst.appended("A"), "m2@x")
			if tt.wantTrip {
				assertInOrder(t, out.String(),
					"[src] No data from server for 1s, reconnecting",
					"already exists and holds 1 of the 2 planned messages",
					"📥 Syncing messages",
				)
				return
			}
			assertContainsNone(t, out.String(), "No data from server")
		})
	}
}

// writeRecorder keeps each Write call apart, so a test can tell one write per line from several.
type writeRecorder struct {
	writes []string
}

func (r *writeRecorder) Write(p []byte) (int, error) {
	r.writes = append(r.writes, string(p))
	return len(p), nil
}

func Test_lineLog_When_Logged_Then_WritesOneLinePerCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
		want string
		args []any
	}{
		{name: "formats the arguments", msg: "copied %d of %s", args: []any{2, "INBOX"}, want: "copied 2 of INBOX\n"},
		{name: "keeps a percent sign when there are no arguments", msg: "100% of %s", want: "100% of %s\n"},
		{name: "keeps a percent sign inside an argument", msg: "%s", args: []any{"50%d"}, want: "50%d\n"},
		{name: "keeps a control character raw", msg: "a\x1b[2Jb", want: "a\x1b[2Jb\n"},
		{name: "keeps an empty message as an empty line", msg: "", want: "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := &writeRecorder{}

			lineLog{out: rec}.Log(tt.msg, tt.args...)

			if !slices.Equal(rec.writes, []string{tt.want}) {
				t.Errorf("writes = %q, want one write %q", rec.writes, tt.want)
			}
		})
	}

	t.Run("writes each call on its own", func(t *testing.T) {
		t.Parallel()

		rec := &writeRecorder{}
		sink := lineLog{out: rec}

		sink.Log("first")
		sink.Log("%s second", "the")

		if want := []string{"first\n", "the second\n"}; !slices.Equal(rec.writes, want) {
			t.Errorf("writes = %q, want %q", rec.writes, want)
		}
	})
}

// forceColours turns go-pretty colours on until the test ends. The flag is process-wide, so only a sequential test may call it.
func forceColours(t *testing.T) {
	t.Helper()
	wereOn := text.Bold.Sprint("x") != "x"
	text.EnableColors()
	t.Cleanup(func() {
		if !wereOn {
			text.DisableColors()
		}
	})
	if got := text.Bold.Sprint("x"); got != "\x1b[1mx\x1b[0m" {
		t.Fatalf("forced colours give %q, want bold codes around x", got)
	}
}

// Sequential on purpose: forceColours flips go-pretty's process-wide colour flag. Parallel tests stay paused until this one ends.
func TestRunSync_When_ColoursAreForcedOn_Then_QuietOutputHoldsNoEscapeByte(t *testing.T) {
	forceColours(t)

	type servers func(t *testing.T) (src, dst *fakeServer, mappings []config.DirectoryMapping)
	one := func(src, dst map[string][]fakeMsg, setup func(t *testing.T, src, dst *fakeServer), mappings ...config.DirectoryMapping) servers {
		return func(t *testing.T) (*fakeServer, *fakeServer, []config.DirectoryMapping) {
			t.Helper()
			s, d := newStoreServer(t, "/", src), newStoreServer(t, "/", dst)
			if setup != nil {
				setup(t, s, d)
			}
			return s, d, mappings
		}
	}
	twoPlans := map[string][]fakeMsg{"A": fakeMsgs("a1@x", "a2@x", "a3@x"), "B": fakeMsgs("b1@x", "b2@x")}
	tests := []struct {
		build   servers
		wantErr error
		name    string
		want    []string
		idle    time.Duration
		verbose bool
	}{
		{
			name:  "copy into new nested folders",
			build: one(map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")}, map[string][]fakeMsg{"INBOX": nil}, nil, mapping("INBOX", "Old/2020/Inbox")),
			want:  []string{"Sync completed successfully"},
		},
		{
			name:    "copy into new nested folders with verbose",
			build:   one(map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")}, map[string][]fakeMsg{"INBOX": nil}, nil, mapping("INBOX", "Old/2020/Inbox")),
			verbose: true,
			want:    []string{"Sync completed successfully"},
		},
		{
			name:  "nothing to copy",
			build: one(map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")}, map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")}, nil, mapping("INBOX", "INBOX")),
		},
		{
			name: "every CREATE refused",
			build: one(map[string][]fakeMsg{"Work": fakeMsgs("m1@x")}, map[string][]fakeMsg{"INBOX": nil},
				func(_ *testing.T, _, dst *fakeServer) { dst.refuseCreate() }, mapping("Work", "Work")),
			wantErr: ErrSilentExit,
			want:    []string{"1 folders failed"},
		},
		{
			name: "throttled CREATE of a parent",
			build: one(map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")}, map[string][]fakeMsg{"INBOX": nil},
				func(_ *testing.T, _, dst *fakeServer) { dst.refuseCreateOf("Old", gmailThrottleReply) }, mapping("A", "Old/A"), mapping("B", "B")),
			wantErr: ErrSilentExit,
			want:    []string{destinationStopStart},
		},
		{
			name: "throttled APPEND with a plan that never starts",
			build: one(twoPlans, map[string][]fakeMsg{"A": nil, "B": nil},
				func(_ *testing.T, _, dst *fakeServer) { dst.refuseAppendWith(gmailThrottleReply) }, mapping("A", "A"), mapping("B", "B")),
			wantErr: ErrSilentExit,
			want:    []string{destinationStopStart, "0 messages uploaded, 5 errors occurred"},
		},
		{
			name: "refused APPEND",
			build: one(twoPlans, map[string][]fakeMsg{"A": nil, "B": nil},
				func(_ *testing.T, _, dst *fakeServer) { dst.refuseAppend() }, mapping("A", "A"), mapping("B", "B")),
			wantErr: ErrSilentExit,
			want:    []string{"0 messages uploaded, 5 errors occurred"},
		},
		{
			name: "folder failure",
			build: one(map[string][]fakeMsg{"A": fakeMsgs("a@x"), "Bad": fakeMsgs("z@x")}, map[string][]fakeMsg{"A": nil},
				func(_ *testing.T, src, _ *fakeServer) { src.refuseOpenWith("Bad", "NO [SERVERBUG] Internal error") }, mapping("A", "A"), mapping("Bad", "Bad")),
			wantErr: ErrSilentExit,
			want:    []string{folderFailuresHeader, "1 messages uploaded, 0 errors occurred, 1 folders failed"},
		},
		{
			name: "folder that LIST hides and that already holds a message",
			build: one(map[string][]fakeMsg{"A": fakeMsgs("m1@x", "m2@x")}, map[string][]fakeMsg{"INBOX": nil, "A": fakeMsgs("m1@x")},
				func(_ *testing.T, _, dst *fakeServer) { dst.hideFromList("A") }, mapping("A", "A")),
			want: []string{"Sync completed successfully"},
		},
		{
			name: "worker source goes silent",
			build: one(map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x", "m2@x")}, map[string][]fakeMsg{"INBOX": nil},
				func(t *testing.T, src, _ *fakeServer) { src.stallNth(t, "UID", 1) }, mapping("INBOX", "INBOX")),
			idle: time.Second,
			want: []string{"Sync completed successfully"},
		},
	}

	t.Run("control: output without quiet holds the colour codes", func(t *testing.T) {
		t.Parallel()

		src, dst, mappings := one(map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")}, map[string][]fakeMsg{"INBOX": nil}, nil, mapping("INBOX", "INBOX"))(t)
		out := &syncBuffer{}
		opts := e2eOptions(out, "")
		opts.autoConfirm = true

		if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mappings...), opts); err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}

		assertContainsAll(t, out.String(), "\x1b[96m", "\x1b[32m✓ done")
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst, mappings := tt.build(t)
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.quiet, opts.verbose = true, true, tt.verbose
			if tt.idle > 0 {
				opts.clientOpts.IdleTimeout = tt.idle
			}

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mappings...), opts)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v\noutput: %q", err, tt.wantErr, out.String())
			}
			raw := out.String()
			assertNoEscapeSequence(t, raw)
			assertContainsAll(t, raw, tt.want...)
			if tt.idle > 0 {
				assertContainsNone(t, raw, "No data from server")
			}
		})
	}
}

func TestRunSync_When_LastSourceFolderWarns_Then_WarningStaysOnScreenWhicheverSideFinishesLast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		slowSide   string
		firstTrack string
		lastTrack  string
	}{
		{name: "the source side finishes last", slowSide: "src", firstTrack: "[dst] Scanned B (2/2)", lastTrack: "[src] Scanned B (2/2)"},
		{name: "the source side finishes first", slowSide: "dst", firstTrack: "[src] Scanned B (2/2)", lastTrack: "[dst] Scanned B (2/2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"A": {{body: bodyOfSize("", 900)}, {msgID: "a1@x"}},
				"B": {{body: bodyOfSize("", 800)}, {msgID: "b1@x"}},
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x")})
			slow := map[string]*fakeServer{"src": src, "dst": dst}[tt.slowSide]
			out := &syncBuffer{}
			// The slow side's second scan FETCH is folder B. It waits until the other side shows B as done, so the slow side ends in a later frame.
			slow.onCommand("FETCH", func() {
				if slow.callCount("FETCH") == 2 {
					waitForScreenRow(t, out, tt.firstTrack, "✓ done")
				}
			})
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "A"), mapping("B", "B")), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			rows := screenRows(t, out.String())
			summary := assertOneRow(t, rows, "✅ All folders already synced!")
			for _, warning := range []string{
				"[src] ⚠️  A: 1 message(s) without Message-Id will be skipped - sync cannot track them",
				"[src] ⚠️  B: 1 message(s) without Message-Id will be skipped - sync cannot track them",
			} {
				if i := assertOneRow(t, rows, warning); i >= 0 && summary >= 0 && i > summary {
					t.Errorf("warning row %d comes after the summary row %d: %q", i, summary, warning)
				}
			}
			first := assertOneRow(t, rows, tt.firstTrack, "✓ done")
			last := assertOneRow(t, rows, tt.lastTrack, "✓ done")
			if first >= 0 && last >= 0 && first > last {
				t.Errorf("row %q is row %d and row %q is row %d, want the side that finished first on top", tt.firstTrack, first, tt.lastTrack, last)
			}
			if stale := rowsHolding(rows, "Scanning"); len(stale) != 0 {
				t.Errorf("rows %v hold a scan that is still in progress\nscreen:\n%s", stale, strings.Join(rows, "\n"))
			}
		})
	}
}

func previewedFolders(raw string) []string {
	const header = "Folders to be created on destination:\n"
	_, after, ok := strings.Cut(raw, header)
	if !ok {
		return nil
	}
	block, _, _ := strings.Cut(after, "\n\n")
	var names []string
	for line := range strings.SplitSeq(block, "\n") {
		names = append(names, strings.TrimPrefix(line, "• "))
	}
	return names
}

func TestRunSync_When_DestinationParentsAreCreated_Then_PreviewWireCounterAndLogsAgree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src         map[string][]fakeMsg
		dst         map[string][]fakeMsg
		setup       func(dst *fakeServer)
		check       func(t *testing.T, dst *fakeServer)
		name        string
		dstDelim    string
		mappings    []config.DirectoryMapping
		wantFolders []string
		wantRows    [][]string
		wantLines   []string
		noRows      [][]string
	}{
		{
			name:        "a typed destination names the INBOX level in lower case",
			src:         map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "inbox/Old/X")},
			wantFolders: []string{"INBOX/Old", "INBOX/Old/X"},
			wantRows:    [][]string{{"Created 2 folders", "✓ done"}},
			wantLines: []string{
				"[dst] Creating parent folder: INBOX/Old",
				"[dst] Created parent folder: INBOX/Old",
				`Created folder "INBOX/Old/X"`,
			},
			noRows: [][]string{{"Parent folder already exists"}},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst INBOX/Old/X", dst.folderMsgIDs("INBOX/Old/X"), "m1@x")
			},
		},
		{
			name:        "the destination delimiter is a dot",
			src:         map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    ".",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old.2020.Inbox")},
			wantFolders: []string{"Old", "Old.2020", "Old.2020.Inbox"},
			wantRows:    [][]string{{"Created 3 folders", "✓ done"}},
			wantLines: []string{
				"[dst] Created parent folder: Old",
				"[dst] Created parent folder: Old.2020",
				`Created folder "Old.2020.Inbox"`,
			},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst Old.2020.Inbox", dst.folderMsgIDs("Old.2020.Inbox"), "m1@x")
			},
		},
		{
			name:        "mappings in another order than the byte order of the destinations",
			src:         map[string][]fakeMsg{"Z": fakeMsgs("z1@x"), "A": fakeMsgs("a1@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("Z", "Old/Z"), mapping("A", "New/A")},
			wantFolders: []string{"New", "New/A", "Old", "Old/Z"},
			wantRows:    [][]string{{"Created 4 folders", "✓ done"}},
			wantLines: []string{
				"[dst] Created parent folder: New",
				"[dst] Created parent folder: Old",
				`Created folder "New/A"`,
				`Created folder "Old/Z"`,
			},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst New/A", dst.folderMsgIDs("New/A"), "a1@x")
				assertIDs(t, "dst Old/Z", dst.folderMsgIDs("Old/Z"), "z1@x")
			},
		},
		{
			name: "a hidden parent answers Duplicate folder name",
			src:  map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			dst:  map[string][]fakeMsg{"INBOX": nil},
			setup: func(dst *fakeServer) {
				dst.refuseCreateOf("Old", "NO [ALREADYEXISTS] Duplicate folder name Old (Failure)")
			},
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020")},
			wantFolders: []string{"Old", "Old/2020"},
			wantRows:    [][]string{{"Created 1 folders", "✓ done"}},
			wantLines: []string{
				"[dst] Creating parent folder: Old",
				"[dst] Parent folder already exists: Old",
				`Created folder "Old/2020"`,
			},
			noRows: [][]string{{"Created parent folder"}, {"✗ error"}, {"Failed to create folder"}},
			check: func(t *testing.T, dst *fakeServer) {
				t.Helper()
				assertIDs(t, "dst Old/2020", dst.folderMsgIDs("Old/2020"), "m1@x")
			},
		},
		{
			name:        "parents of a folder that LIST hides",
			src:         map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")},
			dst:         map[string][]fakeMsg{"INBOX": nil, "Old": nil},
			setup:       func(dst *fakeServer) { dst.hideFromList("Old") },
			dstDelim:    "/",
			mappings:    []config.DirectoryMapping{mapping("INBOX", "Old/2020")},
			wantFolders: []string{"Old", "Old/2020"},
			wantRows:    [][]string{{"Created 1 folders", "✓ done"}},
			wantLines:   []string{"[dst] Parent folder already exists: Old"},
			noRows:      [][]string{{"Created parent folder"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", tt.src)
			dst := newStoreServer(t, tt.dstDelim, tt.dst)
			if tt.setup != nil {
				tt.setup(dst)
			}
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm, opts.verbose = true, true

			if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, tt.mappings...), opts); err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}

			raw := out.String()
			previewed := previewedFolders(raw)
			if !slices.Equal(previewed, tt.wantFolders) {
				t.Errorf("preview lists %q, want %q: parents before children, each name once", previewed, tt.wantFolders)
			}
			if wire := dst.capturedNames("CREATE"); !slices.Equal(wire, previewed) {
				t.Errorf("CREATE names on the wire = %q, preview = %q, want the same names", wire, previewed)
			}
			if slices.Contains(dst.capturedNames("CREATE"), "INBOX") {
				t.Error("dst received CREATE for INBOX, which it lists")
			}
			// The planning client and the one worker list once each: finding the missing levels reads the cache only.
			for name, srv := range map[string]*fakeServer{"src": src, "dst": dst} {
				if n := srv.callCount("LIST"); n != 2 {
					t.Errorf("%s LIST count = %d, want 2", name, n)
				}
			}
			rows := screenRows(t, raw)
			for _, parts := range tt.wantRows {
				assertOneRow(t, rows, parts...)
			}
			for _, line := range tt.wantLines {
				n := len(slices.DeleteFunc(slices.Clone(rows), func(row string) bool { return row != line }))
				if n != 1 {
					t.Errorf("screen holds the row %q %d times, want 1\nscreen:\n%s", line, n, strings.Join(rows, "\n"))
				}
			}
			for _, parts := range tt.noRows {
				if found := rowsHolding(rows, parts...); len(found) != 0 {
					t.Errorf("rows %v hold all of %q, want none\nscreen:\n%s", found, parts, strings.Join(rows, "\n"))
				}
			}
			if tt.check != nil {
				tt.check(t, dst)
			}
		})
	}
}

func TestRunSync_When_ParentCreateIsThrottled_Then_NoFurtherCreateAndCounterShowsTheRefusal(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst.refuseCreateOf("Old", gmailThrottleReply)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "Old/A"), mapping("B", "B")), opts)

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("error = %v, want ErrSilentExit\noutput: %s", err, out.String())
	}
	// B sorts before Old/A, so the throttled parent is the last CREATE.
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "B", "Old")
	if n := dst.callCount("APPEND"); n != 0 {
		t.Errorf("dst saw %d APPEND commands, want 0: the stop comes before the copy", n)
	}
	raw := out.String()
	assertContainsNone(t, raw, "Syncing messages")
	rows := screenRows(t, raw)
	counter := assertOneRow(t, rows, "Created 1 of 3 folders, 1 refused", "✗ error")
	assertOneRow(t, rows, `Failed to create folder "Old/A"`, "failed to create parent folder Old")
	stopped := assertOneRow(t, rows, destinationStopStart)
	summary := assertOneRow(t, rows, "❌ Sync completed with errors. 0 messages uploaded, 2 errors occurred, 0 folders failed")
	if summary >= 0 && summary != len(rows)-1 {
		t.Errorf("summary row = %d of %d, want the last row\nscreen:\n%s", summary, len(rows), strings.Join(rows, "\n"))
	}
	if stopped >= 0 && counter >= 0 && stopped < counter {
		t.Errorf("stop line is row %d and the counter is row %d, want the counter first", stopped, counter)
	}
	if n := strings.Count(raw, destinationStopStart); n != 1 {
		t.Errorf("output holds the stop line %d times, want 1", n)
	}
}

func TestRunSync_When_FolderNameHoldsFormatVerbs_Then_EveryPhaseShowsItVerbatim(t *testing.T) {
	t.Parallel()

	const folder = "100%d %s"
	src := newStoreServer(t, "/", map[string][]fakeMsg{folder: {
		{body: bodyOfSize("", 900)}, {msgID: "m1@x"}, {msgID: "m2@x", envelopeID: "<m2@x>"},
	}})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, folder: fakeMsgs("m1@x")})
	dst.hideFromList(folder)
	out := &syncBuffer{}
	opts := e2eOptions(out, "")
	opts.autoConfirm, opts.verbose = true, true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(folder, folder)), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	raw := out.String()
	assertContainsNone(t, raw, "%!")
	rows := screenRows(t, raw)
	assertOneRow(t, rows, "[src] ⚠️  "+folder+": 1 message(s) without Message-Id will be skipped")
	assertOneRow(t, rows, "[dst] Fetching folder "+folder+"...")
	assertOneRow(t, rows, "Folder "+folder+" already exists")
	assertOneRow(t, rows, "[src-w1] Streaming 1 messages from "+folder)
	assertOneRow(t, rows, `[dst-w1] Message "<m2@x>" appended to `+folder)
	assertIDs(t, "dst appends", dst.appended(folder), "m2@x")
}

func TestRunSync_When_PlanRuns_Then_ItsRowShowsEscapedNamesAndTheServerGetsTheExactOnes(t *testing.T) {
	t.Parallel()

	srcWire, srcName, srcShown := hostileFolder("A")
	dstWire, dstName, dstShown := hostileFolder("B")
	src := newStoreServer(t, "/", map[string][]fakeMsg{srcWire: fakeMsgs("e1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	dst.onCommand("APPEND", func() { waitForCopyFrame(t, out) })
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping(srcName, dstName)), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	raw := out.String()
	_, copyPart, _ := strings.Cut(raw, "📥 Syncing messages...")
	firstFrame, _, _ := strings.Cut(copyPart, "\x1b[A")
	if !strings.Contains(firstFrame, "1/1 "+srcShown+" → "+dstShown) {
		t.Errorf("the first frame of the copy does not hold the running row with the escaped names\nframe: %q", firstFrame)
	}
	assertContainsNone(t, raw, srcName, dstName, "\x1b[2J")
	assertOneRow(t, screenRows(t, raw), "1/1 Synced messages 1 "+srcShown, "✓ done")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), dstWire)
	assertIDs(t, "dst APPENDs", dst.appended(dstWire), "e1@x")
}

// waitForScreenRow blocks until one row of the screen that out draws holds every part. It runs on a server goroutine, so it uses Errorf.
func waitForScreenRow(t *testing.T, out *syncBuffer, parts ...string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A read can end inside an escape sequence, which is an error and only means the frame is not whole yet.
		if rows, err := renderScreen(out.String()); err == nil && len(rowsHolding(rows, parts...)) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the screen had no row with all of %q within 5s", parts)
}

// waitForOutput blocks until out holds want. It runs on a server goroutine, so it uses Errorf.
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the output did not hold %q within 5s", want)
}

func TestRunSync_When_ParentsAreCreatedBeforeALaterFolder_Then_TheCounterTextCountsThemToo(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a@x"), "B": fakeMsgs("b@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}
	// Old/A sorts first and makes Old and Old/A, so Z is the third of the three names that the preview lists.
	dst.onCommand("CREATE", func() {
		if dst.callCount("CREATE") == 3 {
			waitForOutput(t, out, "(3/3) Creating Z")
		}
	})
	opts := e2eOptions(out, "")
	opts.autoConfirm = true

	if err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("A", "Old/A"), mapping("B", "Z")), opts); err != nil {
		t.Fatalf("runSync: %v\noutput: %s", err, out.String())
	}

	raw := out.String()
	assertContainsAll(t, raw, "(3/3) Creating Z")
	assertContainsNone(t, raw, "(2/3) Creating", "(3/4) Creating")
	assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Old", "Old/A", "Z")
	assertOneRow(t, screenRows(t, raw), "Created 3 folders", "✓ done")
}

func TestRunSync_When_MaxConnectionsIsSet_Then_NoSideHoldsMoreSessionsThanTheCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		workers     int
		maxConn     int
		plans       int
		wantWorkers int
	}{
		{name: "cap of one", workers: 4, maxConn: 1, plans: 3, wantWorkers: 1},
		{name: "cap of three with more plans than workers", workers: 4, maxConn: 3, plans: 5, wantWorkers: 3},
		{name: "no cap", workers: 2, maxConn: 0, plans: 3, wantWorkers: 2},
		{name: "cap and workers above the plans", workers: 4, maxConn: 3, plans: 2, wantWorkers: 2},
		{name: "no cap and workers above the plans", workers: 4, maxConn: 0, plans: 2, wantWorkers: 2},
		{name: "cap above the plans and the workers", workers: 4, maxConn: 9, plans: 2, wantWorkers: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			names := []string{"A", "B", "C", "D", "E"}[:tt.plans]
			srcFolders := make(map[string][]fakeMsg, len(names))
			mappings := make([]config.DirectoryMapping, 0, len(names))
			for _, name := range names {
				srcFolders[name] = fakeMsgs(strings.ToLower(name) + "1@x")
				mappings = append(mappings, mapping(name, name))
			}
			src := newStoreServer(t, "/", srcFolders)
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			// The limit is the cap of a provider that refuses the session above it.
			sides := []struct {
				srv      *fakeServer
				sessions *sessionLog
				name     string
			}{
				{srv: src, sessions: src.watchSessions(tt.wantWorkers), name: "source"},
				{srv: dst, sessions: dst.watchSessions(tt.wantWorkers), name: "destination"},
			}
			cfg := e2eConfig(src, dst, tt.workers, mappings...)
			cfg.RateLimit.MaxConnections = tt.maxConn
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), cfg, opts)

			if err != nil {
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}
			assertContainsAll(t, out.String(), "Sync completed successfully")
			for _, name := range names {
				assertIDs(t, "dst "+name, dst.folderMsgIDs(name), strings.ToLower(name)+"1@x")
			}
			for _, side := range sides {
				if n := side.sessions.refusedLogins(); n != 0 {
					t.Errorf("%s refused %d LOGIN commands, want 0", side.name, n)
				}
				if n := side.sessions.peakOpen(); n != tt.wantWorkers {
					t.Errorf("%s peak of open sessions = %d, want %d", side.name, n, tt.wantWorkers)
				}
				if n, want := side.srv.callCount("LOGIN"), 1+tt.wantWorkers; n != want {
					t.Errorf("%s saw %d LOGIN commands, want %d: one planning client and %d workers, no reconnect", side.name, n, want, tt.wantWorkers)
				}
				if logins, logouts := side.srv.callCount("LOGIN"), side.srv.callCount("LOGOUT"); logouts != logins {
					t.Errorf("%s saw %d LOGOUT commands for %d LOGIN commands, want one each", side.name, logouts, logins)
				}
				if events := side.sessions.eventList(); len(events) < 2 || events[0] != "LOGIN" || events[1] != "LOGOUT" {
					t.Errorf("%s events = %v, want the planning session to end before the first worker logs in", side.name, events)
				}
			}
		})
	}
}

func TestRunSync_When_RunEndsBeforeTheCopy_Then_EachPlanningSessionLogsOutOnce(t *testing.T) {
	t.Parallel()

	planningOnly := []string{"LOGIN", "LOGOUT"}
	tests := []struct {
		name        string
		dstFolders  map[string][]fakeMsg
		setup       func(dst *fakeServer)
		input       string
		wantIs      error
		wantErrText string
		wantOutput  string
		wantEvents  []string
	}{
		{
			name:       "the destination holds every message",
			dstFolders: map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x")},
			wantOutput: "All folders already synced!",
			wantEvents: planningOnly,
		},
		{
			name:       "the user declines the prompt",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			input:      "n\n",
			wantOutput: "Sync canceled by user",
			wantEvents: planningOnly,
		},
		{
			name:       "a throttled CREATE stops the run",
			dstFolders: map[string][]fakeMsg{"INBOX": nil},
			setup:      func(dst *fakeServer) { dst.refuseCreateWith(gmailThrottleReply) },
			wantIs:     ErrSilentExit,
			wantOutput: "Sync stopped",
			wantEvents: planningOnly,
		},
		{
			name:        "the worker pool cannot connect",
			dstFolders:  map[string][]fakeMsg{"INBOX": nil, "Work": nil},
			setup:       func(dst *fakeServer) { dst.refuseLoginAt(2) },
			wantErrText: "worker 1 destination connect",
			wantEvents:  []string{"LOGIN", "LOGOUT", "LOGIN", "LOGOUT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", tt.dstFolders)
			if tt.setup != nil {
				tt.setup(dst)
			}
			srcSessions, dstSessions := src.watchSessions(0), dst.watchSessions(0)
			out := &syncBuffer{}
			opts := e2eOptions(out, tt.input)
			opts.autoConfirm = tt.input == ""

			err := runSync(e2eContext(t), e2eConfig(src, dst, 2, mapping("Work", "Work")), opts)

			switch {
			case tt.wantIs != nil && !errors.Is(err, tt.wantIs):
				t.Fatalf("error = %v, want %v\noutput: %s", err, tt.wantIs, out.String())
			case tt.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrText)):
				t.Fatalf("error = %v, want text %q", err, tt.wantErrText)
			case tt.wantIs == nil && tt.wantErrText == "" && err != nil:
				t.Fatalf("runSync: %v\noutput: %s", err, out.String())
			}
			if tt.wantOutput != "" {
				assertContainsAll(t, out.String(), tt.wantOutput)
			}
			assertContainsNone(t, out.String(), "Sync completed successfully")
			assertIDs(t, "source events", srcSessions.eventList(), tt.wantEvents...)
			assertIDs(t, "destination events", dstSessions.eventList(), tt.wantEvents...)
		})
	}
}

func TestRunSync_When_ExistingDestinationNeedsARecheck_Then_PlanningSessionsLogOutAfterItAndBeforeTheWorkers(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"Work": fakeMsgs("m1@x", "m2@x", "m3@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Work": fakeMsgs("m1@x", "m2@x")})
	dst.hideFromList("Work")
	srcSessions, dstSessions := src.watchSessions(0), dst.watchSessions(0)

	first := runSyncOnce(t, e2eConfig(src, dst, 1, mapping("Work", "Work")))

	assertContainsAll(t, first, "Destination folder Work already exists and holds 2 of the 3 planned messages", "Sync completed successfully")
	assertIDs(t, "dst APPENDs", dst.appended("Work"), "m3@x")
	// A recheck after the logout would show as a reconnect LOGIN.
	wantEvents := []string{"LOGIN", "LOGOUT", "LOGIN", "LOGOUT"}
	assertIDs(t, "source events", srcSessions.eventList(), wantEvents...)
	assertIDs(t, "destination events", dstSessions.eventList(), wantEvents...)
}

func TestRunSync_When_OnePlanningSideFailsToConnect_Then_TheOtherSideLogsOutOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantErr   string
		refuseSrc bool
	}{
		{name: "source login refused", refuseSrc: true, wantErr: "source connection failed"},
		{name: "destination login refused", wantErr: "destination connection failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("m1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			failing, healthy := dst, src
			if tt.refuseSrc {
				failing, healthy = src, dst
			}
			failingSessions, healthySessions := failing.watchSessions(0), healthy.watchSessions(0)
			failing.refuseLogin()
			// The refusal waits for the last connect command of the healthy side, then pauses, so that side connects before the group fails.
			ready := make(chan struct{})
			healthy.onCommand("CAPABILITY", sync.OnceFunc(func() { close(ready) }))
			failing.onNthCommand("LOGIN", func(int) {
				select {
				case <-ready:
					time.Sleep(100 * time.Millisecond)
				case <-time.After(e2eTimeout):
				}
			})
			out := &syncBuffer{}
			opts := e2eOptions(out, "")
			opts.autoConfirm = true

			err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("INBOX", "INBOX")), opts)

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			assertIDs(t, "healthy side events", healthySessions.eventList(), "LOGIN", "LOGOUT")
			assertIDs(t, "failing side events", failingSessions.eventList(), "LOGIN", "LOGOUT")
			for name, srv := range map[string]*fakeServer{"source": src, "destination": dst} {
				for _, verb := range []string{"EXAMINE", "STATUS", "FETCH", "CREATE", "APPEND"} {
					if n := srv.callCount(verb); n != 0 {
						t.Errorf("%s saw %d %s commands, want 0: the run must fail before any scan or write", name, n, verb)
					}
				}
			}
		})
	}
}

type firstStamp struct {
	at time.Time
	mu sync.Mutex
}

func (f *firstStamp) set() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at.IsZero() {
		f.at = time.Now()
	}
}

func (f *firstStamp) get() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func TestRunSync_When_PlanningServersNeverAnswerLogout_Then_WorkersConnectAfterOneLogoutLimit(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"A": fakeMsgs("a1@x"), "B": fakeMsgs("b1@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 4, mapping("A", "A"), mapping("B", "B"))
	cfg.RateLimit.MaxConnections = 1
	type side struct {
		srv      *fakeServer
		sessions *sessionLog
		logout   *firstStamp
		login    *firstStamp
		name     string
	}
	sides := []*side{{srv: src, name: "source"}, {srv: dst, name: "destination"}}
	for _, s := range sides {
		s.sessions = s.srv.watchSessions(1)
		s.logout, s.login = &firstStamp{}, &firstStamp{}
		// The first LOGOUT is the planning session and the second LOGIN is the first worker. The stamps come before the stall.
		s.srv.onNthCommand("LOGOUT", func(n int) {
			if n == 1 {
				s.logout.set()
			}
		})
		s.srv.onNthCommand("LOGIN", func(n int) {
			if n == 2 {
				s.login.set()
			}
		})
		s.srv.stallNth(t, "LOGOUT", 1)
	}

	first := runSyncOnce(t, cfg)

	assertContainsAll(t, first, "Sync completed successfully")
	assertIDs(t, "dst A", dst.folderMsgIDs("A"), "a1@x")
	assertIDs(t, "dst B", dst.folderMsgIDs("B"), "b1@x")
	for _, s := range sides {
		if n := s.sessions.refusedLogins(); n != 0 {
			t.Errorf("%s refused %d LOGIN commands, want 0", s.name, n)
		}
		if n := s.sessions.peakOpen(); n != 1 {
			t.Errorf("%s peak of open sessions = %d, want 1", s.name, n)
		}
		// The client closes the socket after the logout limit of 2 s. Both planning sessions wait together.
		gap := s.login.get().Sub(s.logout.get())
		if gap < 1500*time.Millisecond || gap > 3500*time.Millisecond {
			t.Errorf("%s: the worker logged in %s after the planning LOGOUT, want about one logout limit of 2s", s.name, gap)
		}
	}
}

func TestRunSync_When_TypedMappingsHaveAnEmptySourceFolder_Then_OnlyFoldersWithMessagesGetADestination(t *testing.T) {
	t.Parallel()

	t.Run("map item with an empty source next to one with mail", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"Empty": nil, "Full": fakeMsgs("f1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
		cfg := e2eConfig(src, dst, 1, mapping("Empty", "EmptyDst"), mapping("Full", "FullDst"))

		first := runSyncOnce(t, cfg)

		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "FullDst")
		if dst.hasFolder("EmptyDst") {
			t.Error("dst has the folder EmptyDst, want none")
		}
		assertContainsAll(t, first, foldersToCreateHeader+folderBullets("FullDst")+"\n📨 Total new messages to sync: 1")
		assertContainsNone(t, first, "Empty →", "• EmptyDst")
		assertReadOnly(t, "source", src)

		src.addMessage("Empty", "e1@x")

		second := runSyncOnce(t, cfg)

		assertIDs(t, "dst CREATE names over both runs", dst.capturedNames("CREATE"), "FullDst", "EmptyDst")
		assertIDs(t, "dst EmptyDst", dst.folderMsgIDs("EmptyDst"), "e1@x")
		assertContainsAll(t, second, foldersToCreateHeader+folderBullets("EmptyDst")+"\n📨 Total new messages to sync: 1")
	})

	t.Run("every map item has an empty source", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"Empty1": nil, "Empty2": nil})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
		out := &syncBuffer{}
		opts := e2eOptions(out, "")

		err := runSync(e2eContext(t), e2eConfig(src, dst, 1, mapping("Empty1", "D1"), mapping("Empty2", "D2")), opts)

		if err != nil {
			t.Fatalf("runSync: %v\noutput: %s", err, out.String())
		}
		assertContainsAll(t, out.String(), "All folders already synced!")
		assertContainsNone(t, out.String(), "Folders to be created on destination", "Proceed with synchronization?")
		if n := dst.callCount("CREATE"); n != 0 {
			t.Errorf("dst saw %d CREATE commands, want 0", n)
		}
		if n := dst.callCount("APPEND"); n != 0 {
			t.Errorf("dst saw %d APPEND commands, want 0", n)
		}
	})

	t.Run("folder options name an empty folder with a subfolder that has mail", func(t *testing.T) {
		t.Parallel()

		src := newStoreServer(t, "/", map[string][]fakeMsg{"A": nil, "A/Sub": fakeMsgs("s1@x")})
		dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
		cfg := e2eConfig(src, dst, 1)

		first := runSyncConfirmed(t, cfg, "A", "B")

		assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "B", "B/Sub")
		assertIDs(t, "dst B", dst.folderMsgIDs("B"))
		assertIDs(t, "dst B/Sub", dst.folderMsgIDs("B/Sub"), "s1@x")
		assertContainsAll(t, first, foldersToCreateHeader+folderBullets("B", "B/Sub")+"\n📨 Total new messages to sync: 1")
		assertContainsNone(t, first, "A → B will copy")

		second := runSyncConfirmed(t, cfg, "A", "B")

		assertContainsAll(t, second, "All folders already synced!")
		assertIDs(t, "dst CREATE names over both runs", dst.capturedNames("CREATE"), "B", "B/Sub")
		assertReadOnly(t, "source", src)
	})

	orders := []struct {
		name     string
		mappings []config.DirectoryMapping
	}{
		{name: "the empty source comes first", mappings: []config.DirectoryMapping{mapping("Empty", "Archive"), mapping("Full", "Archive")}},
		{name: "the empty source comes last", mappings: []config.DirectoryMapping{mapping("Full", "Archive"), mapping("Empty", "Archive")}},
	}
	for _, tt := range orders {
		t.Run("an empty source and a source with mail share one destination, "+tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"Empty": nil, "Full": fakeMsgs("f1@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			cfg := e2eConfig(src, dst, 1, tt.mappings...)

			first := runSyncOnce(t, cfg)

			assertIDs(t, "dst CREATE names", dst.capturedNames("CREATE"), "Archive")
			assertIDs(t, "dst Archive", dst.folderMsgIDs("Archive"), "f1@x")
			assertContainsAll(t, first,
				"• Full → Archive will copy 1 messages",
				foldersToCreateHeader+folderBullets("Archive")+"\n📨 Total new messages to sync: 1")
			assertContainsNone(t, first, "Empty → Archive")

			second := runSyncOnce(t, cfg)

			assertContainsAll(t, second, "All folders already synced!")
			if n := dst.callCount("CREATE"); n != 1 {
				t.Errorf("dst saw %d CREATE commands over both runs, want 1", n)
			}
		})
	}
}

func TestRunSync_When_FolderOptionsNameTheInboxOfADotServer_Then_EachSubfolderGoesBelowTheDestinationInbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dstFolders  map[string][]fakeMsg
		name        string
		wantCreates []string
		wantTrash   string
		wantAppends []string
		bareInbox   bool
	}{
		{
			name:        "the destination lists only INBOX",
			dstFolders:  map[string][]fakeMsg{"INBOX": nil},
			wantCreates: []string{"INBOX/Junk", "INBOX/Trash"},
			wantTrash:   "INBOX/Trash",
			wantAppends: []string{"i1@x", "j1@x", "t1@x", "t2@x"},
		},
		{
			name:        "the destination lists the Trash folder with INBOX in another letter case",
			dstFolders:  map[string][]fakeMsg{"INBOX": nil, "Inbox/Trash": fakeMsgs("t1@x")},
			bareInbox:   true,
			wantCreates: []string{"INBOX/Junk"},
			wantTrash:   "Inbox/Trash",
			wantAppends: []string{"i1@x", "j1@x", "t2@x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, ".", map[string][]fakeMsg{
				"INBOX":       fakeMsgs("i1@x"),
				"INBOX.Trash": fakeMsgs("t1@x", "t2@x"),
				"INBOX.Junk":  fakeMsgs("j1@x"),
			})
			dst := newStoreServer(t, "/", tt.dstFolders)
			if tt.bareInbox {
				dst.foldOnlyBareInbox()
			}
			cfg := e2eConfig(src, dst, 1)

			first := runSyncConfirmed(t, cfg, "INBOX", "INBOX")

			assertIDs(t, "dst CREATE names", slices.Sorted(slices.Values(dst.capturedNames("CREATE"))), tt.wantCreates...)
			assertIDs(t, "dst INBOX", dst.folderMsgIDs("INBOX"), "i1@x")
			assertIDs(t, "dst Trash", slices.Sorted(slices.Values(dst.folderMsgIDs(tt.wantTrash))), "t1@x", "t2@x")
			assertIDs(t, "dst INBOX/Junk", dst.folderMsgIDs("INBOX/Junk"), "j1@x")
			allAppends := slices.Concat(dst.appended("INBOX"), dst.appended(tt.wantTrash), dst.appended("INBOX/Junk"))
			assertIDs(t, "dst APPENDs", slices.Sorted(slices.Values(allAppends)), tt.wantAppends...)
			assertContainsNone(t, first, "delimiter mismatch", "Fix folder delimiters", "is renamed to", folderFailuresHeader)
			for _, name := range []string{"INBOX.Trash", "INBOX.Junk"} {
				if dst.hasFolder(name) {
					t.Errorf("dst has the folder %s, want the slash form below INBOX", name)
				}
			}

			second := runSyncConfirmed(t, cfg, "INBOX", "INBOX")

			assertContainsAll(t, second, "All folders already synced!")
			if n := len(dst.appended("INBOX")) + len(dst.appended(tt.wantTrash)) + len(dst.appended("INBOX/Junk")); n != len(tt.wantAppends) {
				t.Errorf("dst saw %d APPENDs over both runs, want %d", n, len(tt.wantAppends))
			}
			assertReadOnly(t, "source", src)
		})
	}
}
