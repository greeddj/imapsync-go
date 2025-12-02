// Package cmd wires CLI configuration and subcommands.
package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/greeddj/imapsync-go/cmd/commands"

	"github.com/urfave/cli/v2"
)

var (
	// gitRef stores the version tag from build-time injection.
	gitRef = "v0.0.0-dev"
	// gitCommit stores the git commit hash from build-time injection.
	gitCommit = "0000000"
	// appName is the application name.
	appName = "imapsync-go"
)

// Run configures and executes the imapsync-go CLI application.
func Run() error {
	cli.VersionPrinter = func(cCtx *cli.Context) {
		fmt.Println(cCtx.App.Version)
	}
	app := &cli.App{
		Name:                   appName,
		Suggest:                false,
		Usage:                  "IMAP to IMAP synchronization tool",
		UseShortOptionHandling: true,
		Version:                fmt.Sprintf("%s (%s) // %s", gitRef, gitCommit, runtime.Version()),
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
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "verbose",
						Aliases: []string{"V"},
						Value:   false,
						EnvVars: []string{"IMAPSYNC_VERBOSE"},
					},
					&cli.BoolFlag{
						Name:    "cached",
						Value:   false,
						Usage:   "use cached data if available (faster, but may be outdated)",
						EnvVars: []string{"IMAPSYNC_CACHED"},
					},
				},
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
						Value:   false,
						EnvVars: []string{"IMAPSYNC_VERBOSE"},
					},
					&cli.BoolFlag{
						Name:    "quiet",
						Aliases: []string{"q"},
						Value:   false,
						EnvVars: []string{"IMAPSYNC_QUIET"},
					},
					&cli.BoolFlag{
						Name:    "no-cache",
						Value:   false,
						Usage:   "disable cache, always fetch from servers",
						EnvVars: []string{"IMAPSYNC_NO_CACHE"},
					},
					&cli.BoolFlag{
						Name:    "confirm",
						Aliases: []string{"y", "yes"},
						Value:   false,
						Usage:   "auto-confirm (skip confirmation prompt)",
						EnvVars: []string{"IMAPSYNC_CONFIRM"},
					},
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		return fmt.Errorf("app.Run: %w", err)
	}
	return nil
}
