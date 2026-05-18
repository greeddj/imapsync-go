package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/imapsync-go/internal/config"
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
