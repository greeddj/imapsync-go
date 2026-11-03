package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/urfave/cli/v3"
)

// unreachableConfig names servers nothing listens on: every case below must
// fail before a connection is attempted.
const unreachableConfig = `{
  "src": {"server": "127.0.0.1:1", "user": "alice", "pass": "secret"},
  "dst": {"server": "127.0.0.1:1", "user": "bob", "pass": "secret"},
  "map": [{"src": "INBOX", "dst": "INBOX"}]
}`

const rateLimitedConfig = `{
  "src": {"server": "127.0.0.1:1", "user": "alice", "pass": "secret"},
  "dst": {"server": "127.0.0.1:1", "user": "bob", "pass": "secret"},
  "map": [{"src": "INBOX", "dst": "INBOX"}],
  "rate_limit": {"down_bps": 300000, "up_bps": 300000, "max_connections": 5}
}`

// rootWith mirrors the flags and the help settings of the root command in cmd/imapsync-go/main.go.
func rootWith(cmds ...*cli.Command) *cli.Command {
	return &cli.Command{
		Name:            "imapsync-go",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Value: "config.json"},
		},
		Commands: cmds,
	}
}

// newRoot mirrors the command tree in cmd/imapsync-go/main.go.
func newRoot() *cli.Command {
	return rootWith(Sync(), Show())
}

func TestCommands_When_FlagsAreGiven_Then_ActionsReadThemUnderTheirOwnNames(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(unreachableConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "sync -q alone is refused because quiet needs confirm",
			args: []string{"sync", "-q"},
			want: []string{"--quiet", "--confirm"},
		},
		{
			name: "sync -q -y gets past the quiet refusal and reads -s as the source folder",
			args: []string{"sync", "-q", "-y", "-s", "INBOX"},
			want: []string{"both --src-folder and --dest-folder must be specified"},
		},
		{
			name: "show declares -q and still refuses an operand",
			args: []string{"show", "-q", "extra"},
			want: []string{`unexpected argument "extra"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)

			err := newRoot().Run(ctx, append([]string{"imapsync-go", "-c", cfgPath}, tt.args...))

			if err == nil {
				t.Fatal("Run returned nil, want an error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// The test sets environment variables, so it does not call t.Parallel.
func TestSync_When_RateLimitIsSetToZero_Then_ConfigLimitIsLifted(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(rateLimitedConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		envs [3]string
		name string
		args []string
		want config.RateLimit
	}{
		{
			name: "flags of 0 lift every config limit",
			args: []string{"--bps-down", "0", "--bps-up", "0", "--max-connections", "0"},
			want: config.RateLimit{},
		},
		{
			name: "env vars of 0 lift every config limit",
			envs: [3]string{"0", "0", "0"},
			want: config.RateLimit{},
		},
		{
			name: "no flag and no env value keeps the config limits",
			want: config.RateLimit{DownBPS: 300000, UpBPS: 300000, MaxConnections: 5},
		},
		{
			name: "one env var replaces only its own config limit",
			envs: [3]string{"", "1000", ""},
			want: config.RateLimit{DownBPS: 300000, UpBPS: 1000, MaxConnections: 5},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("IMAPSYNC_BPS_DOWN", tt.envs[0])
			t.Setenv("IMAPSYNC_BPS_UP", tt.envs[1])
			t.Setenv("IMAPSYNC_MAX_CONNECTIONS", tt.envs[2])

			var (
				got    *config.Config
				gotErr error
			)
			syncCmd := Sync()
			syncCmd.Action = func(_ context.Context, c *cli.Command) error {
				got, gotErr = config.New(c)
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)

			args := append([]string{"imapsync-go", "-c", cfgPath, "sync"}, tt.args...)
			if err := rootWith(syncCmd).Run(ctx, args); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if gotErr != nil {
				t.Fatalf("config.New: %v", gotErr)
			}
			if got.RateLimit != tt.want {
				t.Errorf("RateLimit = %+v, want %+v", got.RateLimit, tt.want)
			}
		})
	}
}
