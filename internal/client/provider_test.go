package client

import (
	"testing"

	"github.com/greeddj/imapsync-go/internal/ratelimit"
)

func TestDetectProvider(t *testing.T) {
	t.Parallel()

	gmail := Provider{
		Name:           "Gmail",
		Notes:          "Workspace IMAP bandwidth limits & 15 simultaneous connections per account",
		DownBPS:        27_000,
		UpBPS:          5_000,
		DailyDownMB:    2500,
		DailyUpMB:      500,
		MaxConnections: 15,
	}
	tests := []struct {
		addr    string
		want    Provider
		matched bool
	}{
		{"imap.gmail.com:993", gmail, true},
		{"imap.gmail.com", gmail, true},
		{"IMAP.gmail.com:993", gmail, true},
		{"  imap.gmail.com  :993", gmail, true},
		{"imap.example.org:993", Provider{}, false},
		{"", Provider{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			got, ok := DetectProvider(tt.addr)
			if ok != tt.matched {
				t.Fatalf("matched = %v, want %v", ok, tt.matched)
			}
			if got != tt.want {
				t.Errorf("provider = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKnownProviders_When_RecommendedRateRunsForADay_Then_StaysUnderTheDailyQuota(t *testing.T) {
	t.Parallel()

	const secondsPerDay = 86_400
	directions := []struct {
		bps     func(Provider) int
		quotaMB func(Provider) int
		name    string
	}{
		{
			name:    "down",
			bps:     func(p Provider) int { return p.DownBPS },
			quotaMB: func(p Provider) int { return p.DailyDownMB },
		},
		{
			name:    "up",
			bps:     func(p Provider) int { return p.UpBPS },
			quotaMB: func(p Provider) int { return p.DailyUpMB },
		},
	}
	for host, p := range knownProviders {
		for _, d := range directions {
			bps, quotaMB := d.bps(p), d.quotaMB(p)
			if quotaMB <= 0 {
				continue
			}
			t.Run(host+"/"+d.name, func(t *testing.T) {
				t.Parallel()

				quotaBytes := int64(quotaMB) * 1_000_000
				if want := int(quotaBytes * 95 / 100 / secondsPerDay / 500 * 500); bps != want {
					t.Errorf("recommended rate = %d B/s, want %d B/s", bps, want)
				}
				lim := ratelimit.NewLimiter(bps)
				if lim == nil {
					t.Fatalf("NewLimiter(%d) = nil, want a limiter", bps)
				}
				perDay := int64(bps)*secondsPerDay + int64(lim.Burst())
				if limit := quotaBytes * 95 / 100; perDay > limit {
					t.Errorf("one day at %d B/s moves %d bytes, want at most %d (95%% of the quota)", bps, perDay, limit)
				}
			})
		}
	}
}
