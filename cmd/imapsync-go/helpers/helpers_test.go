package helpers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestVersion_withoutVersion_usesPlaceholderOffline(t *testing.T) {
	// The root command builds Version on every run, so even with both
	// opt-outs unset it must not ask GitHub for the latest tag.
	t.Setenv("IMAPSYNC_OFFLINE", "")
	t.Setenv("CI", "")

	got := Version("", "", "", "")
	if !strings.HasPrefix(got, defaultVersion+" ") {
		t.Errorf("Version without a version = %q, want it to start with %q", got, defaultVersion)
	}
}

func TestLatestVersion(t *testing.T) {
	tests := []struct {
		name    string
		offline string
		ci      string
		version string
		want    string
	}{
		{name: "preset version wins without a lookup", version: "v1.2.3", want: "v1.2.3"},
		{
			name:    "describe-style version passes unchanged",
			version: "v1.2.3-4-gabcdef0-dirty",
			want:    "v1.2.3-4-gabcdef0-dirty",
		},
		{name: "dev fallback passes unchanged", version: "dev", want: "dev"},
		{name: "offline flag skips the network", offline: "1", want: defaultVersion},
		{name: "CI skips the network", ci: "true", want: defaultVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("IMAPSYNC_OFFLINE", tt.offline)
			t.Setenv("CI", tt.ci)
			if got := LatestVersion(tt.version); got != tt.want {
				t.Errorf("LatestVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestVersion_When_CommitIsEmpty_Then_LeavesTheCommitOut(t *testing.T) {
	t.Parallel()

	got := Version("dev", "", "2026-11-03T00:00:00Z", "just")

	want := "dev (built by just @ 2026-11-03T00:00:00Z) // " + runtime.Version()
	if got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}

func TestVersion_presetVersionWins(t *testing.T) {
	// goreleaser-built binaries pass version directly; latestTag must
	// never be consulted.
	t.Setenv("IMAPSYNC_OFFLINE", "")
	t.Setenv("CI", "")

	got := Version("v1.2.3", "abcd", "2026-05-18", "ci")
	if !strings.Contains(got, "v1.2.3") {
		t.Errorf("Version() = %q, want substring v1.2.3", got)
	}
}

func TestFetchTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		want   string
		status int
	}{
		{
			name:   "plain tag is unchanged",
			status: http.StatusOK,
			body:   `{"tag_name":"v1.2.3"}`,
			want:   "v1.2.3",
		},
		{
			name:   "tag with C1 and bidi runes comes back as escape text",
			status: http.StatusOK,
			body:   `{"tag_name":"v1.0.0\u009b2J\u202e"}`,
			want:   `v1.0.0\u009b2J\u202e`,
		},
		{
			name:   "request carries the User-Agent",
			status: http.StatusOK,
			body:   `{"tag_name":"v2.0.0"}`,
			want:   "v2.0.0",
		},
		{
			name:   "non-200 status gives the default version",
			status: http.StatusForbidden,
			body:   `{"tag_name":"v1.2.3"}`,
			want:   defaultVersion,
		},
		{
			name:   "empty tag gives the default version",
			status: http.StatusOK,
			body:   `{"tag_name":""}`,
			want:   defaultVersion,
		},
		{
			name:   "invalid JSON gives the default version",
			status: http.StatusOK,
			body:   `{"tag_name":`,
			want:   defaultVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			userAgents := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				userAgents <- r.Header.Get("User-Agent")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)

			got := fetchTag(srv.URL)

			if got != tt.want {
				t.Errorf("fetchTag() = %q, want %q", got, tt.want)
			}
			select {
			case ua := <-userAgents:
				if ua != userAgent {
					t.Errorf("User-Agent = %q, want %q", ua, userAgent)
				}
			default:
				t.Error("the request never reached the server")
			}
		})
	}
}

func TestVersion_When_DateIsEmpty_Then_LeavesTheDateOut(t *testing.T) {
	t.Parallel()

	got := Version("v1.2.3", "abc1234", "", "just")

	want := "v1.2.3 (commit abc1234, built by just) // " + runtime.Version()
	if got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}
