// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"context"

	"github.com/greeddj/imapsync-go/internal/app"
	"github.com/urfave/cli/v3"
)

func Sync() *cli.Command {
	return &cli.Command{
		Name:  "sync",
		Usage: "sync IMAP dir(s) between two servers",
		Action: func(ctx context.Context, c *cli.Command) error {
			return app.ActionSync(ctx, c)
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "src-folder",
				Aliases: []string{"s"},
				Sources: cli.EnvVars("IMAPSYNC_SOURCE_FOLDER"),
			},
			&cli.StringFlag{
				Name:    "dest-folder",
				Aliases: []string{"d"},
				Sources: cli.EnvVars("IMAPSYNC_DESTINATION_FOLDER"),
			},
			&cli.IntFlag{
				Name:    "workers",
				Aliases: []string{"w"},
				Value:   4,
				Sources: cli.EnvVars("IMAPSYNC_WORKERS"),
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"V"},
				Sources: cli.EnvVars("IMAPSYNC_VERBOSE"),
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Sources: cli.EnvVars("IMAPSYNC_QUIET"),
			},
			&cli.BoolFlag{
				Name:    "confirm",
				Aliases: []string{"y", "yes"},
				Usage:   "auto-confirm (skip confirmation prompt)",
				Sources: cli.EnvVars("IMAPSYNC_CONFIRM"),
			},
		},
	}
}
