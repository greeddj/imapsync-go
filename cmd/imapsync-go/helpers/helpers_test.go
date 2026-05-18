package helpers

import (
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
