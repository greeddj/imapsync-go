package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/urfave/cli/v3"
)

// The test sets environment variables, so it does not call t.Parallel.
func TestShow_When_RateLimitEnvVarsAreSet_Then_ConfigLimitsStay(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(rateLimitedConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("IMAPSYNC_BPS_DOWN", "0")
	t.Setenv("IMAPSYNC_BPS_UP", "0")
	t.Setenv("IMAPSYNC_MAX_CONNECTIONS", "0")

	var (
		got    *config.Config
		gotErr error
	)
	showCmd := Show()
	showCmd.Action = func(_ context.Context, c *cli.Command) error {
		got, gotErr = config.New(c)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := rootWith(showCmd).Run(ctx, []string{"imapsync-go", "-c", cfgPath, "show"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotErr != nil {
		t.Fatalf("config.New: %v", gotErr)
	}
	if want := (config.RateLimit{DownBPS: 300000, UpBPS: 300000, MaxConnections: 5}); got.RateLimit != want {
		t.Errorf("RateLimit = %+v, want %+v: show declares no rate-limit flags", got.RateLimit, want)
	}
}
