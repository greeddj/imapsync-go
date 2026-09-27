package app

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestRejectPositionalArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantsArg string
		argv     []string
		wantErr  bool
	}{
		{name: "no arguments is accepted", argv: []string{"cmd"}},
		{name: "flags alone are accepted", argv: []string{"cmd", "--verbose"}},
		{name: "single argument is rejected", argv: []string{"cmd", "help"}, wantErr: true, wantsArg: "help"},
		{name: "first of several arguments is named", argv: []string{"cmd", "typo", "second"}, wantErr: true, wantsArg: "typo"},
		{name: "argument after a flag is rejected", argv: []string{"cmd", "--verbose", "typo"}, wantErr: true, wantsArg: "typo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got error
			app := &cli.Command{
				Name:            "cmd",
				HideHelpCommand: true,
				Flags:           []cli.Flag{&cli.BoolFlag{Name: "verbose"}},
				Action:          func(_ context.Context, c *cli.Command) error { got = rejectPositionalArgs(c); return nil },
			}
			if err := app.Run(context.Background(), tt.argv); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if !tt.wantErr {
				if got != nil {
					t.Fatalf("got error %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want an error")
			}
			if !strings.Contains(got.Error(), tt.wantsArg) {
				t.Errorf("error %q does not name the offending argument %q", got, tt.wantsArg)
			}
			if !strings.Contains(got.Error(), "--help") {
				t.Errorf("error %q does not point at --help", got)
			}
		})
	}
}

// HideHelpCommand mirrors cmd/imapsync-go/main.go. Without it urfave/cli
// installs an implicit `help` subcommand that absorbs the word before any
// action runs, and the test would pass for a reason production does not share.
// Both actions must refuse an operand before they read the config or open a
// connection, so an unexpected word can never start a sync against live
// servers. Running them with no config present proves the guard fires first:
// any later step would fail with a config error instead.
func TestActions_When_PositionalArgGiven_Then_RejectBeforeConfigLoad(t *testing.T) {
	t.Parallel()

	actions := map[string]func(context.Context, *cli.Command) error{
		"sync": ActionSync,
		"show": ActionShow,
	}

	for name, action := range actions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got error
			app := &cli.Command{
				Name:            "imapsync-go",
				HideHelpCommand: true,
				Commands: []*cli.Command{{
					Name:   name,
					Action: func(ctx context.Context, c *cli.Command) error { got = action(ctx, c); return nil },
				}},
			}
			if err := app.Run(context.Background(), []string{"imapsync-go", name, "help"}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got == nil {
				t.Fatal("got nil, want an error")
			}
			if !strings.Contains(got.Error(), `unexpected argument "help"`) {
				t.Errorf("error %q is not the positional-argument refusal", got)
			}
			if !strings.Contains(got.Error(), "imapsync-go "+name+" --help") {
				t.Errorf("error %q does not name the command's own help", got)
			}
		})
	}
}
