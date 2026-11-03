//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/cmd/imapsync-go/helpers"
	appkg "github.com/greeddj/imapsync-go/internal/app"

	"github.com/urfave/cli/v3"
)

// Not parallel: the test sends SIGQUIT to its own process, and
// installQuitDump must be the only SIGQUIT listener while it runs.
func TestInstallQuitDump_When_SIGQUITArrives_Then_DumpsEveryGoroutineAndExits131(t *testing.T) {
	var dump bytes.Buffer
	codes := make(chan int, 1)
	stop := installQuitDump(&dump, func(code int) { codes <- code })
	t.Cleanup(stop)

	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		t.Fatalf("send SIGQUIT: %v", err)
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var code int
	select {
	case code = <-codes:
	case <-timer.C:
		t.Fatal("no exit within 5s of SIGQUIT")
	}

	if code != exitQuit {
		t.Errorf("exit code = %d, want %d", code, exitQuit)
	}
	// The test goroutine is blocked in the select above, not in the handler,
	// so finding it proves the dump covers other goroutines too.
	if got := dump.String(); !strings.Contains(got, t.Name()) {
		t.Errorf("dump does not include the waiting test goroutine %q:\n%s", t.Name(), got)
	}
}

// otherSignal is an os.Signal that is not a syscall.Signal.
type otherSignal struct{}

func (otherSignal) Signal()        {}
func (otherSignal) String() string { return "other" }

func TestReportError(t *testing.T) {
	t.Parallel()

	cancelled := fmt.Errorf("sync: %w", context.Canceled)
	tests := []struct {
		err      error
		cause    error
		name     string
		want     string
		wantCode int
	}{
		{
			name:     "wrapped cancellation prints Cancelled and exits 130",
			err:      cancelled,
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation after SIGTERM exits 143",
			err:      cancelled,
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "Cancelled.\n",
			wantCode: 143,
		},
		{
			name:     "cancellation after SIGINT exits 130",
			err:      cancelled,
			cause:    signalError{sig: os.Interrupt},
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation with a plain cancel cause exits 130",
			err:      cancelled,
			cause:    context.Canceled,
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "cancellation with a wrapped signal cause finds the signal",
			err:      cancelled,
			cause:    fmt.Errorf("stop: %w", signalError{sig: syscall.SIGTERM}),
			want:     "Cancelled.\n",
			wantCode: 143,
		},
		{
			name:     "cancellation with a signal that is not a syscall signal exits 130",
			err:      cancelled,
			cause:    signalError{sig: otherSignal{}},
			want:     "Cancelled.\n",
			wantCode: 130,
		},
		{
			name:     "wrapped silent exit prints nothing and exits 1",
			err:      fmt.Errorf("sync: %w", appkg.ErrSilentExit),
			want:     "",
			wantCode: 1,
		},
		{
			name:     "silent exit after SIGTERM still exits 1",
			err:      fmt.Errorf("sync: %w", appkg.ErrSilentExit),
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "",
			wantCode: 1,
		},
		{
			name:     "plain error is printed with the Error prefix",
			err:      errors.New("connection refused"),
			want:     "Error: connection refused\n",
			wantCode: 1,
		},
		{
			name:     "plain error after SIGTERM is printed and exits 1",
			err:      errors.New("connection refused"),
			cause:    signalError{sig: syscall.SIGTERM},
			want:     "Error: connection refused\n",
			wantCode: 1,
		},
		{
			name:     "server text with control characters is escaped",
			err:      errors.New("\x1b]0;pwned\x07\r\n\u202e"),
			want:     "Error: " + `\x1b]0;pwned\a\r\n\u202e` + "\n",
			wantCode: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			code := reportError(&out, tt.err, tt.cause)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func waitDone(t *testing.T, ctx context.Context) {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		t.Fatal("context not done within 5s")
	}
}

func Test_cancelOnSignal_When_SignalArrives_Then_CauseNamesIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sig      os.Signal
		name     string
		wantText string
		wantCode int
	}{
		{
			name:     "SIGTERM exits 143",
			sig:      syscall.SIGTERM,
			wantText: "terminated signal received",
			wantCode: 143,
		},
		{
			name:     "SIGINT exits 130",
			sig:      os.Interrupt,
			wantText: "interrupt signal received",
			wantCode: 130,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signals := make(chan os.Signal, 1)
			ctx, stop := cancelOnSignal(context.Background(), signals)
			t.Cleanup(stop)

			signals <- tt.sig
			waitDone(t, ctx)

			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
			cause := context.Cause(ctx)
			if _, ok := errors.AsType[signalError](cause); !ok {
				t.Fatalf("cause = %T %v, want signalError", cause, cause)
			}
			if got := cause.Error(); got != tt.wantText {
				t.Errorf("cause text = %q, want %q", got, tt.wantText)
			}
			if code := cancelExitCode(cause); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func Test_cancelOnSignal_When_StopRunsBeforeAnySignal_Then_LaterSignalChangesNothing(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(context.Background(), signals)
	stop()
	waitDone(t, ctx)

	signals <- syscall.SIGTERM

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, context.Canceled) {
		t.Errorf("cause = %v, want context.Canceled", cause)
	}
	if code := cancelExitCode(cause); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func Test_cancelOnSignal_When_TwoSignalsArrive_Then_FirstWins(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(context.Background(), signals)
	t.Cleanup(stop)

	signals <- syscall.SIGTERM
	waitDone(t, ctx)
	signals <- os.Interrupt

	if code := cancelExitCode(context.Cause(ctx)); code != 143 {
		t.Errorf("exit code = %d, want 143", code)
	}
}

func Test_cancelOnSignal_When_ParentEnds_Then_CauseIsACancelWithNoSignal(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	ctx, stop := cancelOnSignal(parent, signals)
	t.Cleanup(stop)

	cancelParent()
	waitDone(t, ctx)

	cause := context.Cause(ctx)
	if !errors.Is(cause, context.Canceled) {
		t.Errorf("cause = %v, want context.Canceled", cause)
	}
	if code := cancelExitCode(cause); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

// Not parallel: the test sends SIGTERM and SIGINT to its own process, and
// notifySignals must hold both signals while it runs.
func Test_notifySignals_When_SignalArrives_Then_ExitCodeNamesIt(t *testing.T) {
	tests := []struct {
		name     string
		sig      syscall.Signal
		wantCode int
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 143},
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, stop := notifySignals()
			t.Cleanup(stop)

			if err := syscall.Kill(os.Getpid(), tt.sig); err != nil {
				t.Fatalf("send %v: %v", tt.sig, err)
			}
			waitDone(t, ctx)

			var out bytes.Buffer
			code := reportError(&out, fmt.Errorf("sync: %w", ctx.Err()), context.Cause(ctx))

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := out.String(); got != "Cancelled.\n" {
				t.Errorf("output = %q, want %q", got, "Cancelled.\n")
			}
		})
	}
}

// runCLI runs the real root command. It fails the test when urfave/cli tries to
// exit inside Run or writes to its global error writer, because both skip reportError.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	prevExiter, prevErrWriter := cli.OsExiter, cli.ErrWriter
	var global bytes.Buffer
	cli.OsExiter = func(code int) {
		t.Errorf("urfave/cli exited with code %d inside Run, args %q", code, args)
	}
	cli.ErrWriter = &global
	t.Cleanup(func() { cli.OsExiter, cli.ErrWriter = prevExiter, prevErrWriter })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out, errOut bytes.Buffer
	code = execute(ctx, append([]string{"imapsync-go"}, args...), &out, &errOut)

	if global.Len() != 0 {
		t.Errorf("urfave/cli wrote %q to its global error writer, args %q", global.String(), args)
	}
	return code, out.String(), errOut.String()
}

func isOneLine(s string) bool {
	return strings.HasSuffix(s, "\n") && strings.Count(s, "\n") == 1
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
func Test_execute_When_FirstArgumentNamesNoCommand_Then_PrintsOneErrorLineAndExits1(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	unknown := func(quoted string) string {
		return "Error: unknown command " + quoted + `, run "imapsync-go --help" for usage` + "\n"
	}
	tests := []struct {
		name string
		want string
		args []string
	}{
		{name: "misspelt command", args: []string{"snyc"}, want: unknown(`"snyc"`)},
		{name: "misspelt command with an operand", args: []string{"snyc", "extra"}, want: unknown(`"snyc"`)},
		{
			name: "misspelt command after a config file that does not exist",
			args: []string{"-c", missing, "snyc"},
			want: unknown(`"snyc"`),
		},
		{name: "misspelt command after the end of the flags", args: []string{"--", "snyc"}, want: unknown(`"snyc"`)},
		{name: "empty operand", args: []string{""}, want: unknown(`""`)},
		{name: "alias h is not a command", args: []string{"h"}, want: unknown(`"h"`)},
		{name: "help names no command", args: []string{"help", "x"}, want: unknown(`"x"`)},
		{name: "help names no command and has an operand", args: []string{"help", "x", "extra"}, want: unknown(`"x"`)},
		{name: "help names itself", args: []string{"help", "help"}, want: unknown(`"help"`)},
		{
			name: "help for a command has one operand too many",
			args: []string{"help", "sync", "extra"},
			want: `Error: unexpected argument "extra", run "imapsync-go --help" for usage` + "\n",
		},
		{
			name: "line break in the operand stays on one line",
			args: []string{"snyc\nError: fake"},
			want: unknown(`"snyc\nError: fake"`),
		},
		{
			name: "control characters in the operand are escaped",
			args: []string{"\x1b]0;pwned\a\u202e"},
			want: unknown(`"\x1b]0;pwned\a\u202e"`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if stderr != tt.want {
				t.Errorf("stderr = %q, want %q", stderr, tt.want)
			}
			if !isOneLine(stderr) {
				t.Errorf("stderr = %q, want exactly one line", stderr)
			}
		})
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter,
// and the version cases replace VersionPrinter and set an environment variable.
func Test_execute_When_FormIsValid_Then_KeepsItsOutputAndExits0(t *testing.T) {
	prevPrinter := cli.VersionPrinter
	cli.VersionPrinter = printVersion
	t.Cleanup(func() { cli.VersionPrinter = prevPrinter })
	t.Setenv("IMAPSYNC_OFFLINE", "1")

	reference := func(args ...string) string {
		t.Helper()

		code, stdout, stderr := runCLI(t, args...)
		if code != 0 || stderr != "" || stdout == "" {
			t.Fatalf("%q: exit code = %d, stderr = %q, stdout = %q, want 0, empty and some help", args, code, stderr, stdout)
		}
		return stdout
	}
	rootHelp := reference("--help")
	syncHelp := reference("sync", "--help")
	showHelp := reference("show", "--help")

	if want := "NAME:\n   imapsync-go - IMAP to IMAP synchronization tool\n"; !strings.HasPrefix(rootHelp, want) {
		t.Errorf("root help does not start with %q:\n%s", want, rootHelp)
	}
	wantCommands := "\nCOMMANDS:\n" +
		"   sync  sync IMAP dir(s) between two servers\n" +
		"   show  show IMAP dirs in source and destination servers\n\n"
	if !strings.Contains(rootHelp, wantCommands) {
		t.Errorf("root help does not list only sync and show under COMMANDS:\n%s", rootHelp)
	}
	if want := "NAME:\n   imapsync-go sync - sync IMAP dir(s) between two servers\n"; !strings.HasPrefix(syncHelp, want) {
		t.Errorf("sync help does not start with %q:\n%s", want, syncHelp)
	}
	if want := "NAME:\n   imapsync-go show - show IMAP dirs in source and destination servers\n"; !strings.HasPrefix(showHelp, want) {
		t.Errorf("show help does not start with %q:\n%s", want, showHelp)
	}

	wantVersion := helpers.LatestVersion(Version) + "\n"
	tests := []struct {
		name string
		want string
		args []string
	}{
		{name: "no operand shows the root help", args: nil, want: rootHelp},
		{name: "end of flags shows the root help", args: []string{"--"}, want: rootHelp},
		{name: "short help flag shows the root help", args: []string{"-h"}, want: rootHelp},
		{name: "help shows the root help", args: []string{"help"}, want: rootHelp},
		{name: "help sync shows the sync help", args: []string{"help", "sync"}, want: syncHelp},
		{name: "help show shows the show help", args: []string{"help", "show"}, want: showHelp},
		{
			name: "help sync after the config flag shows the sync help",
			args: []string{"-c", "x.json", "help", "sync"},
			want: syncHelp,
		},
		{name: "help flag before the command shows its help", args: []string{"--help", "sync"}, want: syncHelp},
		{name: "version flag prints the version", args: []string{"--version"}, want: wantVersion},
		{name: "short version flag prints the version", args: []string{"-v"}, want: wantVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
		})
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
// The text comes from urfave/cli, so the test pins the shape and not the words.
func Test_execute_When_HelpFlagNamesNoCommand_Then_PrintsOneErrorLineAndExits1(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "long help flag before the word", args: []string{"--help", "x"}},
		{name: "long help flag after the word", args: []string{"x", "--help"}},
		{name: "short help flag before the word", args: []string{"-h", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !isOneLine(stderr) || !strings.HasPrefix(stderr, "Error: ") || !strings.Contains(stderr, "x") {
				t.Errorf("stderr = %q, want one line that starts with \"Error: \" and names x", stderr)
			}
		})
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
func Test_execute_When_FirstArgumentNamesACommand_Then_TheCommandHandlesTheRest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	tests := []struct {
		name       string
		want       string
		wantPrefix string
		args       []string
	}{
		{
			name: "sync refuses the word help",
			args: []string{"sync", "help"},
			want: `Error: unexpected argument "help", run "imapsync-go sync --help" for usage` + "\n",
		},
		{
			name: "show refuses an extra operand",
			args: []string{"show", "extra"},
			want: `Error: unexpected argument "extra", run "imapsync-go show --help" for usage` + "\n",
		},
		{
			name:       "show loads the config file",
			args:       []string{"-c", missing, "show"},
			wantPrefix: "load config: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if tt.want != "" && stderr != tt.want {
				t.Errorf("stderr = %q, want %q", stderr, tt.want)
			}
			if want := "Error: " + tt.wantPrefix; !strings.HasPrefix(stderr, want) {
				t.Errorf("stderr = %q, want prefix %q", stderr, want)
			}
			if !isOneLine(stderr) {
				t.Errorf("stderr = %q, want exactly one line", stderr)
			}
			if strings.Contains(stderr, "unknown command") {
				t.Errorf("stderr = %q, want no unknown command error", stderr)
			}
		})
	}
}
