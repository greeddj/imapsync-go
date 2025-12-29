// Package cmd wires CLI configuration and subcommands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/imapsync-go/cmd/imapsync-go/commands"
	"github.com/greeddj/imapsync-go/cmd/imapsync-go/helpers"

	"github.com/urfave/cli/v2"
)

//nolint:gochecknoglobals
var (
	Version string
	Commit  string
	Date    string
	BuiltBy string
)

// main is the entry point for the imapsync-go application.
func main() {
	os.Exit(run())
}

// run configures and executes the imapsync-go CLI application.
func run() int {
	app := &cli.App{
		Name:                   "imapsync-go",
		Usage:                  "IMAP to IMAP synchronization tool",
		UseShortOptionHandling: true,
		Version:                helpers.Version(Version, Commit, Date, BuiltBy),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   "config.json",
				Usage:   "path to configuration file (JSON or YAML)",
				EnvVars: []string{"IMAPSYNC_CONFIG"},
			},
		},
		Commands: []*cli.Command{
			{
				Name:   "show",
				Usage:  "show IMAP dirs in source and destination servers",
				Action: commands.Show,
			},
			{
				Name:   "sync",
				Usage:  "sync IMAP dir(s) between two servers",
				Action: commands.Sync,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "src-folder",
						Aliases: []string{"s"},
						EnvVars: []string{"IMAPSYNC_SOURCE_FOLDER"},
					},
					&cli.StringFlag{
						Name:    "dest-folder",
						Aliases: []string{"d"},
						EnvVars: []string{"IMAPSYNC_DESTINATION_FOLDER"},
					},
					&cli.IntFlag{
						Name:    "workers",
						Aliases: []string{"w"},
						Value:   4,
						EnvVars: []string{"IMAPSYNC_WORKERS"},
					},
					&cli.BoolFlag{
						Name:    "verbose",
						Aliases: []string{"V"},
						EnvVars: []string{"IMAPSYNC_VERBOSE"},
					},
					&cli.BoolFlag{
						Name:    "quiet",
						Aliases: []string{"q"},
						EnvVars: []string{"IMAPSYNC_QUIET"},
					},
					&cli.BoolFlag{
						Name:    "confirm",
						Aliases: []string{"y", "yes"},
						Usage:   "auto-confirm (skip confirmation prompt)",
						EnvVars: []string{"IMAPSYNC_CONFIRM"},
					},
				},
			},
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	if err := app.RunContext(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}
