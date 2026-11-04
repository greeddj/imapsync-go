//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
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

const (
	rootHelpName = "NAME:\n   imapsync-go - IMAP to IMAP synchronization tool\n"
	syncHelpName = "NAME:\n   imapsync-go sync - sync IMAP dir(s) between two servers\n"
	showHelpName = "NAME:\n   imapsync-go show - show IMAP dirs in source and destination servers\n"
)

// runCLI runs the real root command. It fails the test when urfave/cli tries to
// exit inside Run or writes to its global error writer, because both skip reportError.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	return runCLIWithContext(t, context.Background(), args...)
}

// runCLIWithContext is runCLI for a context that the caller ends. The 5 second
// limit stops a hung command, and the cause of the caller context still reaches execute.
func runCLIWithContext(t *testing.T, parent context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	prevExiter, prevErrWriter := cli.OsExiter, cli.ErrWriter
	var global bytes.Buffer
	cli.OsExiter = func(code int) {
		t.Errorf("urfave/cli exited with code %d inside Run, args %q", code, args)
	}
	cli.ErrWriter = &global
	t.Cleanup(func() { cli.OsExiter, cli.ErrWriter = prevExiter, prevErrWriter })

	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
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

	if !strings.HasPrefix(rootHelp, rootHelpName) {
		t.Errorf("root help does not start with %q:\n%s", rootHelpName, rootHelp)
	}
	wantCommands := "\nCOMMANDS:\n" +
		"   sync  sync IMAP dir(s) between two servers\n" +
		"   show  show IMAP dirs in source and destination servers\n\n"
	if !strings.Contains(rootHelp, wantCommands) {
		t.Errorf("root help does not list only sync and show under COMMANDS:\n%s", rootHelp)
	}
	if !strings.HasPrefix(syncHelp, syncHelpName) {
		t.Errorf("sync help does not start with %q:\n%s", syncHelpName, syncHelp)
	}
	if !strings.HasPrefix(showHelp, showHelpName) {
		t.Errorf("show help does not start with %q:\n%s", showHelpName, showHelp)
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

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter,
// and the test sets an environment variable.
func Test_execute_When_RootHelpIsShown_Then_VersionSectionHoldsTheOfflineBuildString(t *testing.T) {
	t.Setenv("IMAPSYNC_OFFLINE", "1")

	code, stdout, stderr := runCLI(t, "--help")

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "\nVERSION:\n   " + helpers.Version(Version, Commit, Date, BuiltBy) + "\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("root help does not hold %q:\n%s", want, stdout)
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

type scratchRepo struct {
	t    *testing.T
	dir  string
	just string
	git  string
	env  []string
}

func newScratchRepo(t *testing.T, just, git string) *scratchRepo {
	t.Helper()

	dir := t.TempDir()
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	// A git hook sets GIT_DIR or GIT_INDEX_FILE, user config can sign commits,
	// and a parent repository could be found. None of these may reach the commands.
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
		"GIT_AUTHOR_NAME=example",
		"GIT_AUTHOR_EMAIL=example@example.invalid",
		"GIT_COMMITTER_NAME=example",
		"GIT_COMMITTER_EMAIL=example@example.invalid",
	)
	return &scratchRepo{t: t, dir: dir, env: env, just: just, git: git}
}

func (r *scratchRepo) run(name string, args ...string) string {
	r.t.Helper()

	cmd := exec.CommandContext(r.t.Context(), name, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("%s %q: %v, stderr: %s", name, args, err, stderr.String())
	}
	return stdout.String()
}

func (r *scratchRepo) runGit(args ...string) string {
	r.t.Helper()

	return strings.TrimSpace(r.run(r.git, args...))
}

func (r *scratchRepo) write(content string) {
	r.t.Helper()

	if err := os.WriteFile(filepath.Join(r.dir, "tracked.txt"), []byte(content), 0o600); err != nil {
		r.t.Fatalf("write tracked file: %v", err)
	}
}

func (r *scratchRepo) initWithCommit() {
	r.t.Helper()

	r.runGit("init", "-q")
	r.commit("first")
}

func (r *scratchRepo) commit(content string) {
	r.t.Helper()

	r.write(content)
	r.runGit("add", "tracked.txt")
	r.runGit("commit", "-q", "-m", content)
}

func (r *scratchRepo) hash() string {
	r.t.Helper()

	return r.runGit("rev-parse", "--short", "HEAD")
}

func (r *scratchRepo) evaluate(justfile, name string) string {
	r.t.Helper()

	return r.run(r.just, "--justfile", justfile, "--working-directory", r.dir, "--evaluate", name)
}

const canary = "pwned"

func (r *scratchRepo) tagObject(ref, embedded string) {
	r.t.Helper()

	body := "object " + r.runGit("rev-parse", "HEAD") + "\ntype commit\ntag " + embedded +
		"\ntagger example <example@example.invalid> 1700000000 +0000\n\nrelease\n"
	file := filepath.Join(r.t.TempDir(), "tag-object")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		r.t.Fatalf("write tag object: %v", err)
	}
	r.runGit("update-ref", "refs/tags/"+ref, r.runGit("hash-object", "-t", "tag", "-w", file))
}

func (r *scratchRepo) requireNoFile(name string) {
	r.t.Helper()

	if _, err := os.Lstat(filepath.Join(r.dir, name)); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("file %q: Lstat error = %v, want it to be missing", name, err)
	}
}

func TestJustfile_When_TreeStateVaries_Then_VersionNamesTheExactBuild(t *testing.T) {
	t.Parallel()

	justPath, err := exec.LookPath("just")
	if err != nil {
		t.Skipf("just is not in PATH: %v", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not in PATH: %v", err)
	}
	justfile, err := filepath.Abs(filepath.Join("..", "..", "Justfile"))
	if err != nil {
		t.Fatalf("resolve the Justfile path: %v", err)
	}
	// The stat makes the go test cache notice an edited Justfile, because reads by a subprocess are not tracked.
	if _, err := os.Stat(justfile); err != nil {
		t.Fatalf("the Justfile is missing: %v", err)
	}

	tests := []struct {
		setup func(r *scratchRepo) (wantVersion, wantCommit string)
		name  string
	}{
		{
			name:  "directory outside any repository",
			setup: func(*scratchRepo) (string, string) { return "dev", "" },
		},
		{
			name: "repository with no commit",
			setup: func(r *scratchRepo) (string, string) {
				r.runGit("init", "-q")
				return "dev", ""
			},
		},
		{
			name: "commit with no tag",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				return r.hash(), r.hash()
			},
		},
		{
			name: "commit with no tag and a local change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.write("a local change")
				return r.hash() + "-dirty", r.hash()
			},
		},
		{
			name: "tagged commit",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				return "v1.2.3", r.hash()
			},
		},
		{
			name: "tagged commit with a local change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.write("a local change")
				return "v1.2.3-dirty", r.hash()
			},
		},
		{
			name: "one commit after the tag",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.commit("second")
				return "v1.2.3-1-g" + r.hash(), r.hash()
			},
		},
		{
			name: "one commit after the tag with a local change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.commit("second")
				r.write("a local change")
				return "v1.2.3-1-g" + r.hash() + "-dirty", r.hash()
			},
		},
		{
			name: "annotated tag on the commit",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "-a", "v1.2.3", "-m", "release")
				return "v1.2.3", r.hash()
			},
		},
		{
			name: "annotated tag and one commit after it",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "-a", "v1.2.3", "-m", "release")
				r.commit("second")
				return "v1.2.3-1-g" + r.hash(), r.hash()
			},
		},
		{
			name: "staged change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.write("a staged change")
				r.runGit("add", "tracked.txt")
				return r.hash() + "-dirty", r.hash()
			},
		},
		{
			// git describe --dirty ignores a file that git does not track.
			name: "untracked file only",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				if err := os.WriteFile(filepath.Join(r.dir, "untracked.txt"), []byte("new"), 0o600); err != nil {
					r.t.Fatalf("write untracked file: %v", err)
				}
				return r.hash(), r.hash()
			},
		},
		{
			name: "only tag holds a command substitution",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v9$(touch${IFS}"+canary+")")
				return r.hash(), r.hash()
			},
		},
		{
			name: "unsafe tag on the commit after a safe tag",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.commit("second")
				r.runGit("tag", "v9$(id)")
				return "v1.2.3-1-g" + r.hash(), r.hash()
			},
		},
		{
			// git describe breaks a tie between lightweight tags by name, so the unsafe name sorts first.
			name: "safe and unsafe tag on the same commit",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.runGit("tag", "v0`id`")
				return "v1.2.3", r.hash()
			},
		},
		{
			name: "only tag holds a double quote",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", `v9"x`)
				return r.hash(), r.hash()
			},
		},
		{
			name: "only tag holds a single quote",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v9'x")
				return r.hash(), r.hash()
			},
		},
		{
			name: "only tag holds a semicolon",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v9;x")
				return r.hash(), r.hash()
			},
		},
		{
			name: "only tag holds a bidi control",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3\u202e")
				return r.hash(), r.hash()
			},
		},
		{
			name: "only tag holds a C1 control",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1\u0085")
				return r.hash(), r.hash()
			},
		},
		{
			name: "unsafe tag and a local change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v9$(id)")
				r.write("a local change")
				return r.hash() + "-dirty", r.hash()
			},
		},
		{
			// git describe prints the name stored in the tag object, not the ref name.
			name: "annotated tag object names an unsafe tag under a safe ref",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.tagObject("v1.2.3", "v9$(touch${IFS}"+canary+")")
				return r.hash(), r.hash()
			},
		},
		{
			name: "annotated tag object names a bidi control under a safe ref and a local change",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.tagObject("v1.2.3", "v1.2.3\u202e")
				r.write("a local change")
				return r.hash() + "-dirty", r.hash()
			},
		},
		{
			// Without a slash match in the glob, only the check of the result would catch this tag, and it gives the bare hash.
			name: "unsafe text after a slash in the tag on the next commit",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "v1.2.3")
				r.commit("second")
				r.runGit("tag", "rel/v9$(id)")
				return "v1.2.3-1-g" + r.hash(), r.hash()
			},
		},
		{
			name: "tag holds a slash and a plus sign",
			setup: func(r *scratchRepo) (string, string) {
				r.initWithCommit()
				r.runGit("tag", "release/v1.2.3+build.5")
				return "release/v1.2.3+build.5", r.hash()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newScratchRepo(t, justPath, gitPath)
			wantVersion, wantCommit := tt.setup(r)

			if got := r.evaluate(justfile, "VERSION"); got != wantVersion {
				t.Errorf("VERSION = %q, want %q", got, wantVersion)
			}
			if got := r.evaluate(justfile, "COMMIT"); got != wantCommit {
				t.Errorf("COMMIT = %q, want %q", got, wantCommit)
			}
			ldflags := r.evaluate(justfile, "LDFLAGS")
			wantFlags := " -X main.Version=" + wantVersion + " -X main.Commit=" + wantCommit + " -X main.Date="
			if !strings.Contains(ldflags, wantFlags) {
				t.Errorf("LDFLAGS = %q, want it to contain %q", ldflags, wantFlags)
			}
			if !strings.HasSuffix(ldflags, " -X main.BuiltBy=just") {
				t.Errorf("LDFLAGS = %q, want it to end with %q", ldflags, " -X main.BuiltBy=just")
			}
			r.requireNoFile(canary)
		})
	}
}

func writeFakeTool(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestJustfile_When_VersionHoldsShellSyntax_Then_BuildRecipesPassLdflagsAsOneArgument(t *testing.T) {
	justPath, err := exec.LookPath("just")
	if err != nil {
		t.Skipf("just is not in PATH: %v", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not in PATH: %v", err)
	}
	justfile, err := filepath.Abs(filepath.Join("..", "..", "Justfile"))
	if err != nil {
		t.Fatalf("resolve the Justfile path: %v", err)
	}
	// The stat makes the go test cache notice an edited Justfile, because reads by a subprocess are not tracked.
	if _, err := os.Stat(justfile); err != nil {
		t.Fatalf("the Justfile is missing: %v", err)
	}

	// The scripts exist before t.Parallel: on Linux a parallel fork can hold the write descriptor of a new script, and exec then fails with ETXTBSY.
	fakeBin := t.TempDir()
	const argvVar = "IMAPSYNC_TEST_ARGV"
	writeFakeTool(t, fakeBin, "go", `if [ "$1" = build ]; then printf '%s\0' "$@" > "$`+argvVar+`"; fi; exit 0`)
	writeFakeTool(t, fakeBin, "golangci-lint", "exit 0")
	path := fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")

	t.Parallel()

	const fixedDate = "2026-01-02T03:04:05Z"
	versions := []struct{ name, value string }{
		{name: "command substitution", value: "v1$(touch${IFS}" + canary + ")"},
		{name: "backticks", value: "v1`touch " + canary + "`"},
		{name: "single quote", value: "v1'x;touch " + canary + ";'"},
		{name: "double quote", value: `v1";touch ` + canary + `;"`},
	}
	for _, recipe := range []string{"build", "build_linux"} {
		for _, v := range versions {
			t.Run(recipe+" with "+v.name, func(t *testing.T) {
				t.Parallel()

				r := newScratchRepo(t, justPath, gitPath)
				r.initWithCommit()
				argvFile := filepath.Join(t.TempDir(), "argv")
				// exec keeps the last of two equal keys.
				r.env = append(r.env, "PATH="+path, argvVar+"="+argvFile)

				r.run(r.just, "--justfile", justfile, "--working-directory", r.dir,
					"--set", "VERSION", v.value, "--set", "DATE", fixedDate, recipe)

				raw, err := os.ReadFile(argvFile)
				if err != nil {
					t.Fatalf("read the recorded go build arguments: %v", err)
				}
				var ldflags []string
				for _, arg := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
					if strings.HasPrefix(arg, "-ldflags=") {
						ldflags = append(ldflags, arg)
					}
				}
				want := "-ldflags=-s -w -X main.Version=" + v.value + " -X main.Commit=" + r.hash() +
					" -X main.Date=" + fixedDate + " -X main.BuiltBy=just"
				if len(ldflags) != 1 || ldflags[0] != want {
					t.Errorf("-ldflags arguments = %q, want exactly [%q]", ldflags, want)
				}
				r.requireNoFile(canary)
			})
		}
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
// The words come from urfave/cli, so the test pins the shape and the two prefixes, not the reason text.
func Test_execute_When_OptionIsWrong_Then_PrintsUsageThenOneErrorLineAndExits1(t *testing.T) {
	tests := []struct {
		name     string
		wantHelp string
		args     []string
	}{
		{name: "misspelt command with a short option", args: []string{"snyc", "-y"}, wantHelp: rootHelpName},
		{name: "short option that the root does not define", args: []string{"-y"}, wantHelp: rootHelpName},
		{name: "long option that the root does not define", args: []string{"--bogus"}, wantHelp: rootHelpName},
		{name: "sync option without its value", args: []string{"sync", "-s"}, wantHelp: syncHelpName},
		{name: "sync option that does not exist", args: []string{"sync", "--bogus"}, wantHelp: syncHelpName},
		{name: "show option that does not exist", args: []string{"show", "--bogus"}, wantHelp: showHelpName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.HasPrefix(stdout, tt.wantHelp) {
				t.Errorf("stdout does not start with %q:\n%s", tt.wantHelp, stdout)
			}
			usage, last, ok := strings.Cut(stderr, "\n\nError: ")
			reason, hasPrefix := strings.CutPrefix(usage, "Incorrect Usage: ")
			if !ok || !hasPrefix || reason == "" || strings.Contains(reason, "\n") {
				t.Fatalf("stderr = %q, want a usage line, an empty line and an Error line", stderr)
			}
			if want := reason + "\n"; last != want {
				t.Errorf("Error line = %q, want %q", last, want)
			}
		})
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
func Test_execute_When_HelpFormIsRefused_Then_PrintsOneErrorLineAndExits1(t *testing.T) {
	tests := []struct {
		name string
		want string
		args []string
	}{
		{
			name: "help with the help flag",
			args: []string{"help", "--help"},
		},
		{
			name: "help for a command with the help flag",
			args: []string{"help", "sync", "--help"},
			want: `Error: unexpected argument "--help", run "imapsync-go --help" for usage` + "\n",
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
			if !isOneLine(stderr) || !strings.HasPrefix(stderr, "Error: ") {
				t.Errorf("stderr = %q, want one line that starts with \"Error: \"", stderr)
			}
			if tt.want != "" && stderr != tt.want {
				t.Errorf("stderr = %q, want %q", stderr, tt.want)
			}
		})
	}
}

// Not parallel: runCLI replaces the urfave/cli globals OsExiter and ErrWriter.
func Test_execute_When_ConfigFlagHasNoCommand_Then_ShowsRootHelpWithoutLoadingTheFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")

	code, rootHelp, stderr := runCLI(t, "--help")
	if code != 0 || stderr != "" || rootHelp == "" {
		t.Fatalf("root help: exit code = %d, stderr = %q, stdout = %q, want 0, empty and some help", code, stderr, rootHelp)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "config flag alone", args: []string{"-c", missing}},
		{name: "long config flag alone", args: []string{"--config", missing}},
		{name: "config flag and the word help", args: []string{"-c", missing, "help"}},
		{name: "config flag and the end of the flags", args: []string{"-c", missing, "--"}},
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
			if stdout != rootHelp {
				t.Errorf("stdout = %q, want the root help %q", stdout, rootHelp)
			}
		})
	}
}

// A TLS handshake against a silent server waits for the caller, so the command still runs when the signal arrives.
func listenSilent(t *testing.T) (addr string, accepted <-chan struct{}) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu     sync.Mutex
		conns  []net.Conn
		once   sync.Once
		closed bool
	)
	first := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			conns = append(conns, conn)
			mu.Unlock()
			once.Do(func() { close(first) })
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return ln.Addr().String(), first
}

// Not parallel: runCLIWithContext replaces the urfave/cli globals OsExiter and
// ErrWriter. The signal comes through a channel, so no signal reaches the test
// process.
func Test_execute_When_SignalEndsARunningCommand_Then_PrintsCancelledAndExitsWithTheSignalCode(t *testing.T) {
	commands := []struct {
		args func(cfgPath string) []string
		name string
	}{
		{name: "show", args: func(cfgPath string) []string { return []string{"-c", cfgPath, "show", "-q"} }},
		{name: "sync", args: func(cfgPath string) []string { return []string{"-c", cfgPath, "sync", "-q", "-y"} }},
	}
	signals := []struct {
		sig      os.Signal
		name     string
		wantCode int
	}{
		{name: "SIGTERM", sig: syscall.SIGTERM, wantCode: 143},
		{name: "SIGINT", sig: syscall.SIGINT, wantCode: 130},
	}
	for _, cmd := range commands {
		for _, sig := range signals {
			t.Run(cmd.name+" after "+sig.name, func(t *testing.T) {
				addr, accepted := listenSilent(t)
				cfgPath := filepath.Join(t.TempDir(), "config.json")
				cfg := fmt.Sprintf(`{
  "src": {"server": %q, "user": "alice", "pass": "secret"},
  "dst": {"server": %q, "user": "bob", "pass": "secret"},
  "map": [{"src": "INBOX", "dst": "INBOX"}]
}`, addr, addr)
				if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
				delivered := make(chan os.Signal, 1)
				ctx, stop := cancelOnSignal(context.Background(), delivered)
				t.Cleanup(stop)
				done := make(chan struct{})
				defer close(done)
				go func() {
					select {
					case <-accepted:
						delivered <- sig.sig
					case <-done:
					}
				}()

				code, stdout, stderr := runCLIWithContext(t, ctx, cmd.args(cfgPath)...)

				if code != sig.wantCode {
					t.Errorf("exit code = %d, want %d", code, sig.wantCode)
				}
				if stderr != "Cancelled.\n" {
					t.Errorf("stderr = %q, want %q", stderr, "Cancelled.\n")
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
			})
		}
	}
}

// Not parallel: runCLIWithContext replaces the urfave/cli globals OsExiter and ErrWriter.
func Test_execute_When_ContextEndedBeforeTheRun_Then_ExitCodeFollowsTheCause(t *testing.T) {
	tests := []struct {
		cause    error
		name     string
		wantCode int
	}{
		{name: "SIGTERM", cause: signalError{sig: syscall.SIGTERM}, wantCode: 143},
		{name: "SIGINT", cause: signalError{sig: os.Interrupt}, wantCode: 130},
		{name: "wrapped SIGTERM", cause: fmt.Errorf("stop: %w", signalError{sig: syscall.SIGTERM}), wantCode: 143},
		{name: "a cancel that no signal names", cause: context.Canceled, wantCode: 130},
	}
	commands := [][]string{{"show", "-q"}, {"sync", "-q", "-y"}}
	for _, tt := range tests {
		for _, args := range commands {
			t.Run(args[0]+" after "+tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(tt.cause)

				code, stdout, stderr := runCLIWithContext(t, ctx, args...)

				if code != tt.wantCode {
					t.Errorf("exit code = %d, want %d", code, tt.wantCode)
				}
				if stderr != "Cancelled.\n" {
					t.Errorf("stderr = %q, want %q", stderr, "Cancelled.\n")
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
			})
		}
	}
}

func Test_cancelOnSignal_When_ContextEnds_Then_TheGoroutineEnds(t *testing.T) {
	t.Parallel()

	// Only the case ends the context, so a goroutine that the case leaves blocked makes
	// synctest.Test fail when the bubble function returns.
	tests := []struct {
		end  func(signals chan os.Signal, parent context.CancelFunc, stop context.CancelFunc)
		name string
	}{
		{name: "stop runs", end: func(_ chan os.Signal, _, stop context.CancelFunc) { stop() }},
		{name: "signal arrives", end: func(signals chan os.Signal, _, _ context.CancelFunc) { signals <- syscall.SIGTERM }},
		{name: "parent ends", end: func(_ chan os.Signal, parent, _ context.CancelFunc) { parent() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				parent, cancelParent := context.WithCancel(context.Background())
				signals := make(chan os.Signal, 1)
				ctx, stop := cancelOnSignal(parent, signals)

				tt.end(signals, cancelParent, stop)
				synctest.Wait()

				if ctx.Err() == nil {
					t.Error("ctx.Err() = nil, want the context to end")
				}
			})
		})
	}
}
