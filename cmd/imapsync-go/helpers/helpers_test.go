package helpers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersion_offlineFlagSkipsNetwork(t *testing.T) {
	// IMAPSYNC_OFFLINE forces latestTag to return defaultVersion without
	// touching the network. The release-binary path is already proven —
	// goreleaser always sets a non-empty version — so this test guards
	// the dev/source-build path where latestTag() used to fire blindly.
	t.Setenv("IMAPSYNC_OFFLINE", "1")
	t.Setenv("CI", "")

	got := Version("", "", "", "")
	if !strings.Contains(got, defaultVersion) {
		t.Errorf("Version with IMAPSYNC_OFFLINE=1 = %q, want substring %q", got, defaultVersion)
	}
}

func TestVersion_ciFlagSkipsNetwork(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("IMAPSYNC_OFFLINE", "")

	got := Version("", "", "", "")
	if !strings.Contains(got, defaultVersion) {
		t.Errorf("Version with CI=true = %q, want substring %q", got, defaultVersion)
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
