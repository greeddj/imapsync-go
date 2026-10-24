// Package cmd wires CLI configuration and subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/greeddj/imapsync-go/cmd/imapsync-go/commands"
	"github.com/greeddj/imapsync-go/cmd/imapsync-go/helpers"
	appkg "github.com/greeddj/imapsync-go/internal/app"

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

	installQuitDump(os.Stderr, os.Exit)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(os.Stderr, "Cancelled.")
			return 130
		case errors.Is(err, appkg.ErrSilentExit):
			// ActionSync has already printed a human-friendly summary.
			return 1
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	}
	return 0
}

// exitQuit is 128 + SIGQUIT, matching the 130 that Ctrl-C returns.
const exitQuit = 131

// installQuitDump makes Ctrl+\ the escape hatch for a hung program: it writes
// every goroutine stack to w, then exits, because a hang may never return to run.
func installQuitDump(w io.Writer, exit func(int)) (stop func()) {
	sigquit := make(chan os.Signal, 1)
	signal.Notify(sigquit, syscall.SIGQUIT)
	go func() {
		if _, ok := <-sigquit; !ok {
			return
		}
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		_, _ = w.Write(buf[:n])
		exit(exitQuit)
	}()
	return func() {
		signal.Stop(sigquit)
		close(sigquit)
	}
}
