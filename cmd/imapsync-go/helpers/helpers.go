package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/greeddj/imapsync-go/internal/termsafe"
)

// Version returns the formatted version string for the application. It never
// touches the network: the root command builds it on every run.
func Version(version, commit, date, builtBy string) string {
	if version == "" {
		version = defaultVersion
	}

	if builtBy == "" {
		builtBy = defaultBuilder
	}

	switch {
	case date != "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s @ %s) // %s", version, commit, builtBy, date, runtime.Version())
	case date == "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s) // %s", version, commit, builtBy, runtime.Version())
	case date != "" && commit == "":
		return fmt.Sprintf("%s (built by %s @ %s) // %s", version, builtBy, date, runtime.Version())
	default:
		return fmt.Sprintf("%s (built by %s) // %s", version, builtBy, runtime.Version())
	}
}

// LatestVersion returns version, or for a build without one the latest release
// tag from GitHub. Only the --version printer calls it, so other runs stay offline.
func LatestVersion(version string) string {
	if version != "" {
		return version
	}
	return latestTag()
}

// latestTag fetches the latest release tag from GitHub. Skipped entirely when
// IMAPSYNC_OFFLINE or CI is set — a surprise outbound HTTPS call on every
// --version is bad UX in air-gapped or rate-limited environments.
func latestTag() string {
	if os.Getenv("IMAPSYNC_OFFLINE") != "" || os.Getenv("CI") != "" {
		return defaultVersion
	}
	return fetchTag(latestVersionURL)
}

func fetchTag(url string) string {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return defaultVersion
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return defaultVersion
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return defaultVersion
	}

	var payload struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return defaultVersion
	}
	if payload.Tag == "" {
		return defaultVersion
	}
	// The tag is remote text printed in --help, and git allows C1 and bidi characters in tag names.
	return termsafe.Escape(payload.Tag)
}
