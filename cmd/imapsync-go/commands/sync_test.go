package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

// unreachableConfig names servers nothing listens on: every case below must
// fail before a connection is attempted.
const unreachableConfig = `{
  "src": {"server": "127.0.0.1:1", "user": "alice", "pass": "secret"},
  "dst": {"server": "127.0.0.1:1", "user": "bob", "pass": "secret"},
  "map": [{"src": "INBOX", "dst": "INBOX"}]
}`

// newRoot mirrors the command tree in cmd/imapsync-go/main.go.
func newRoot() *cli.Command {
	return &cli.Command{
		Name:            "imapsync-go",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Value: "config.json"},
		},
		Commands: []*cli.Command{Sync(), Show()},
	}
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
