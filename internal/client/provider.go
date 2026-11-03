package client

import "strings"

// Provider describes a known IMAP service. DailyDownMB and DailyUpMB are the IMAP
// quotas of one account. DownBPS and UpBPS keep a non-stop run at least 5% under them.
type Provider struct {
	Name           string
	Notes          string
	DownBPS        int
	UpBPS          int
	DailyDownMB    int
	DailyUpMB      int
	MaxConnections int
}

// knownProviders maps lower-cased IMAP hostname to its Provider profile.
// Add new entries here when their limits become well-documented; this is the
// only source-of-truth.
var knownProviders = map[string]Provider{
	"imap.gmail.com": {
		Name:           "Gmail",
		Notes:          "Workspace IMAP bandwidth limits & 15 simultaneous connections per account",
		DownBPS:        27_000,
		UpBPS:          5_000,
		DailyDownMB:    2500,
		DailyUpMB:      500,
		MaxConnections: 15,
	},
}

// DetectProvider returns Provider data for serverAddr if its host is known.
// serverAddr may include a port (host:port).
func DetectProvider(serverAddr string) (Provider, bool) {
	host := serverAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		// Trim port only if what follows looks numeric - guards against IPv6
		// literals being mangled, though we don't expect them here.
		port := host[i+1:]
		if port != "" && allDigits(port) {
			host = host[:i]
		}
	}
	p, ok := knownProviders[strings.ToLower(strings.TrimSpace(host))]
	return p, ok
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
