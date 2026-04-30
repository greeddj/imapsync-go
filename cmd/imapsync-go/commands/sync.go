// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"github.com/greeddj/imapsync-go/internal/app"
	"github.com/urfave/cli/v3"
)

// Sync returns the "sync" subcommand definition.
func Sync() *cli.Command {
	return &cli.Command{
		Name:   "sync",
		Usage:  "sync IMAP dir(s) between two servers",
		Action: app.ActionSync,
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
			&cli.IntFlag{
				Name:    "bps-down",
				Usage:   "max bytes/sec read from the source server (0 = unlimited; for Gmail try 300000)",
				Value:   0,
				Sources: cli.EnvVars("IMAPSYNC_BPS_DOWN"),
			},
			&cli.IntFlag{
				Name:    "bps-up",
				Usage:   "max bytes/sec written to the destination server (0 = unlimited; for Gmail try 300000)",
				Value:   0,
				Sources: cli.EnvVars("IMAPSYNC_BPS_UP"),
			},
			&cli.IntFlag{
				Name:    "max-connections",
				Usage:   "hard cap on simultaneous IMAP connections per side (0 = workers)",
				Value:   0,
				Sources: cli.EnvVars("IMAPSYNC_MAX_CONNECTIONS"),
			},
		},
	}
}
