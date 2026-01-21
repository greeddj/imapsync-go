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

	"github.com/urfave/cli/v3"
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
	// Customize the version printer to show only the version.
	cli.VersionPrinter = func(c *cli.Command) {
		_, _ = fmt.Fprintln(c.Writer, Version)
	}

	app := &cli.Command{
		Name:                   "imapsync-go",
		Usage:                  "IMAP to IMAP synchronization tool",
		HideHelpCommand:        true,
		UseShortOptionHandling: true,
		Version:                helpers.Version(Version, Commit, Date, BuiltBy),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   "config.json",
				Usage:   "path to configuration file (JSON or YAML)",
				Sources: cli.EnvVars("IMAPSYNC_CONFIG"),
			},
		},
		Commands: []*cli.Command{
			commands.Sync(),
			commands.Show(),
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}
