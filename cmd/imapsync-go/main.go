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
	"github.com/greeddj/imapsync-go/internal/termsafe"

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
	cli.VersionPrinter = printVersion

	installQuitDump(os.Stderr, os.Exit)

	ctx, stop := notifySignals()
	defer stop()

	return execute(ctx, os.Args, os.Stdout, os.Stderr)
}

// printVersion shows only the version.
func printVersion(c *cli.Command) {
	_, _ = fmt.Fprintln(c.Writer, helpers.LatestVersion(Version))
}

func newApp(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:                   "imapsync-go",
		Usage:                  "IMAP to IMAP synchronization tool",
		HideHelpCommand:        true,
		UseShortOptionHandling: true,
		Version:                helpers.Version(Version, Commit, Date, BuiltBy),
		Writer:                 stdout,
		ErrWriter:              stderr,
		Action:                 rootAction,
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
}

// execute runs the root command and returns the exit code, so that no code path
// exits inside Run and skips the deferred stop of the signal context.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := newApp(stdout, stderr).Run(ctx, args); err != nil {
		return reportError(stderr, err, context.Cause(ctx))
	}
	return 0
}

const helpWord = "help"

// HideHelpCommand removes urfave's help command, and without an Action urfave
// exits with code 3 inside Run. So this action serves help [command] and
// refuses other words.
func rootAction(ctx context.Context, c *cli.Command) error {
	args := c.Args()
	if args.Len() == 0 {
		return cli.ShowRootCommandHelp(c)
	}

	name := args.First()
	if name == helpWord {
		if args.Len() == 1 {
			return cli.ShowRootCommandHelp(c)
		}
		topic := args.Get(1)
		if c.Command(topic) != nil {
			if args.Len() > 2 {
				return fmt.Errorf("unexpected argument %q, run %q for usage", args.Get(2), c.FullName()+" --help")
			}
			return cli.ShowCommandHelp(ctx, c, topic)
		}
		name = topic
	}
	return fmt.Errorf("unknown command %q, run %q for usage", name, c.FullName()+" --help")
}

type signalError struct{ sig os.Signal }

func (e signalError) Error() string { return e.sig.String() + " signal received" }

// exitInterrupt is 128 + SIGINT. A cancel that no signal names also exits with it.
const exitInterrupt = 128 + int(syscall.SIGINT)

// cancelExitCode gives 128 + the signal number, so the exit code tells SIGTERM from Ctrl-C.
func cancelExitCode(cause error) int {
	if se, ok := errors.AsType[signalError](cause); ok {
		if n, ok := se.sig.(syscall.Signal); ok {
			return 128 + int(n)
		}
	}
	return exitInterrupt
}

func notifySignals() (context.Context, context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := cancelOnSignal(context.Background(), sigs)
	return ctx, func() {
		cancel()
		signal.Stop(sigs)
	}
}

// cancelOnSignal replaces signal.NotifyContext, whose cause is an unexported
// type. A signalError cause, set before Done closes, lets the exit code read
// the signal number.
func cancelOnSignal(parent context.Context, signals <-chan os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		select {
		case s := <-signals:
			cancel(signalError{sig: s})
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

// The error can quote a reply the IMAP server sent, so the text is escaped.
// The cause sets only the exit code of the cancel branch.
func reportError(w io.Writer, err, cause error) int {
	switch {
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(w, "Cancelled.")
		return cancelExitCode(cause)
	case errors.Is(err, appkg.ErrSilentExit):
		// The command has already printed its failures on stdout.
		return 1
	default:
		_, _ = fmt.Fprintf(w, "Error: %s\n", termsafe.Escape(err.Error()))
		return 1
	}
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
