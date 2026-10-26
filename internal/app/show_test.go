package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v3"
)

var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripANSI removes colour codes so assertions see the text a user reads.
func stripANSI(s string) string { return ansiSequence.ReplaceAllString(s, "") }

// e2eShowOptions aims runShow at plain-TCP servers.
func e2eShowOptions(out io.Writer, quiet bool) showOptions {
	return showOptions{
		out:        out,
		quiet:      quiet,
		clientOpts: client.Options{UseTLS: false, DialTimeout: 5 * time.Second},
	}
}

// assertFolderRow checks that section has a table row for name showing count
// messages and the formatted size.
func assertFolderRow(t *testing.T, section, name string, count int, size uint64) {
	t.Helper()
	want := fmt.Sprintf("%d %s", count, utils.FormatSize(size))
	for line := range strings.SplitSeq(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != name {
			continue
		}
		if got := strings.Join(fields[1:], " "); got != want {
			t.Errorf("row %q shows %q, want %q", name, got, want)
		}
		return
	}
	t.Errorf("no row for folder %q in\n%s", name, section)
}

// assertTotalsRow checks the footer row of a folder table.
func assertTotalsRow(t *testing.T, section string, folders, messages int, size uint64) {
	t.Helper()
	want := fmt.Sprintf("TOTAL FOLDERS %d %d %s", folders, messages, utils.FormatSize(size))
	for line := range strings.SplitSeq(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "TOTAL" {
			continue
		}
		if got := strings.Join(fields, " "); got != want {
			t.Errorf("totals row = %q, want %q", got, want)
		}
		return
	}
	t.Errorf("no totals row in\n%s", section)
}

func TestRunShow_When_BothAccountsReachable_Then_PrintsFolderTables(t *testing.T) {
	t.Parallel()

	for _, quiet := range []bool{true, false} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": fakeMsgs("a@x", "b@x"),
				"Sent":  nil,
			})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("c@x")})
			cfg := e2eConfig(src, dst, 1)
			out := &syncBuffer{}

			if err := runShow(e2eContext(t), cfg, e2eShowOptions(out, quiet)); err != nil {
				t.Fatalf("runShow: %v", err)
			}

			got := stripANSI(out.String())
			_, tables, ok := strings.Cut(got, "Source")
			if !ok {
				t.Fatalf("output has no Source title:\n%s", got)
			}
			srcSection, dstSection, ok := strings.Cut(tables, "Destination")
			if !ok {
				t.Fatalf("output has no Destination title:\n%s", got)
			}

			srcSize := uint64(2 * len(imapFullBody("a@x")))
			dstSize := uint64(len(imapFullBody("c@x")))
			assertContainsAll(t, srcSection, src.ln.Addr().String(), "alice")
			assertFolderRow(t, srcSection, "INBOX", 2, srcSize)
			assertFolderRow(t, srcSection, "Sent", 0, 0)
			assertTotalsRow(t, srcSection, 2, 2, srcSize)
			assertContainsAll(t, dstSection, dst.ln.Addr().String(), "bob")
			assertFolderRow(t, dstSection, "INBOX", 1, dstSize)
			assertTotalsRow(t, dstSection, 1, 1, dstSize)
			assertContainsNone(t, got, e2ePassword)
			assertReadOnly(t, "source", src)
			assertReadOnly(t, "destination", dst)
		})
	}
}

func TestRunShow_When_LoginRefused_Then_NamesTheFailingAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		want      string
		refuseSrc bool
		refuseDst bool
	}{
		{name: "source login refused", refuseSrc: true, want: "[alpha] connect"},
		{name: "destination login refused", refuseDst: true, want: "[beta] connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
			if tt.refuseSrc {
				src.refuseLogin()
			}
			if tt.refuseDst {
				dst.refuseLogin()
			}
			cfg := e2eConfig(src, dst, 1)
			cfg.Src.Label, cfg.Dst.Label = "alpha", "beta"

			err := runShow(e2eContext(t), cfg, e2eShowOptions(&syncBuffer{}, true))

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), e2ePassword) {
				t.Errorf("error %q leaks the password", err)
			}
		})
	}
}

func TestRunShow_When_ContextAlreadyCanceled_Then_ReturnsBeforeConnecting(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runShow(ctx, e2eConfig(src, dst, 1), e2eShowOptions(&syncBuffer{}, true))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertNoLogin(t, src, dst)
}

func TestPrintAccountInfo_When_MailboxesGiven_Then_RendersHeaderRowsAndTotals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mailboxes []*client.MailboxInfo
	}{
		{
			name: "two mailboxes",
			mailboxes: []*client.MailboxInfo{
				{Name: "INBOX", Messages: 3, Size: 2048},
				{Name: "Archive", Messages: 10, Size: 5 * 1024 * 1024},
			},
		},
		{name: "no mailboxes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			printAccountInfo(&out, "Source", "mail.example.test:993", "carol", tt.mailboxes)

			got := stripANSI(out.String())
			assertContainsAll(t, got, "Source", "Server  mail.example.test:993", "User    carol")

			if len(tt.mailboxes) == 0 {
				assertContainsNone(t, got, "FOLDER", "MESSAGES", "TOTAL")
				return
			}
			assertContainsAll(t, got, "FOLDER", "MESSAGES", "SIZE")
			var messages int
			var size uint64
			for _, mb := range tt.mailboxes {
				assertFolderRow(t, got, mb.Name, int(mb.Messages), mb.Size)
				messages += int(mb.Messages)
				size += mb.Size
			}
			assertTotalsRow(t, got, len(tt.mailboxes), messages, size)
		})
	}
}

// showFlags mirrors the flag names ActionShow reads.
func showFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"V"}},
		&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}},
	}
}

func TestActionShow_When_ServerIsNotTLS_Then_FailsWithoutSendingCredentials(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	cfgPath := writeJSONConfig(t, src, dst)

	err := runCLIAction(t, ActionShow, "show", showFlags(), "-c", cfgPath, "show", "-q")

	if err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("error = %v, want a TLS handshake failure", err)
	}
	assertNoLogin(t, src, dst)
}

func TestActionShow_When_ConfigIsMissing_Then_ReportsLoadFailure(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.json")

	err := runCLIAction(t, ActionShow, "show", showFlags(), "-c", missing, "show", "-q")

	if err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("error = %v, want %q", err, "load config")
	}
}

func TestActionShow_When_ConfigHasUnknownKey_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	srcDialed, dstDialed := watchDials(src), watchDials(dst)
	cfgPath := writeRawConfig(t, ".json", src, dst,
		`,"maps":[{"src":"INBOX","dst":"INBOX"}],"rate-limit":{"down_bps":300000}`)

	err := runCLIAction(t, ActionShow, "show", showFlags(), "-c", cfgPath, "show", "-q")

	if err == nil {
		t.Fatal("error = nil, want a config load failure")
	}
	if !strings.Contains(err.Error(), "load config") || !strings.Contains(err.Error(), cfgPath) {
		t.Errorf("error = %v, want %q and the config path", err, "load config")
	}
	if msg := strings.ReplaceAll(err.Error(), cfgPath, ""); !strings.Contains(msg, "maps") {
		t.Errorf("error = %q, want the key %q", msg, "maps")
	}
	if srcDialed.Load() || dstDialed.Load() {
		t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
	}
	assertNoLogin(t, src, dst)
}

func TestRunShow_When_ListingMailboxesFails_Then_NamesTheFailingAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    string
		failSrc bool
	}{
		{name: "source listing fails", failSrc: true, want: "[alpha] list mailboxes"},
		{name: "destination listing fails", want: "[beta] list mailboxes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("b@x")})
			failing := dst
			if tt.failSrc {
				failing = src
			}
			failing.addConnHandler(listFailsAfterConnectHandler(failing, "INBOX"))
			cfg := e2eConfig(src, dst, 1)
			cfg.Src.Label, cfg.Dst.Label = "alpha", "beta"

			err := runShow(e2eContext(t), cfg, e2eShowOptions(&syncBuffer{}, true))

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestActionShow_When_ContextAlreadyCanceled_Then_ReturnsBeforeReadingConfig(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ActionShow(ctx, &cli.Command{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRunShow_When_CanceledWhileListing_Then_ReportsCancelNotAccountFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(e2eContext(t))
	t.Cleanup(cancel)
	src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x")})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("b@x")})
	src.addConnHandler(scriptedHandler(src, []string{"INBOX"}, func(verb string, nth int) (string, bool) {
		if verb == "LIST" && nth > 1 {
			cancel()
			return "", true
		}
		return "", false
	}))
	cfg := e2eConfig(src, dst, 1)
	cfg.Src.Label, cfg.Dst.Label = "alpha", "beta"

	err := runShow(ctx, cfg, e2eShowOptions(&syncBuffer{}, true))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "list mailboxes") {
		t.Errorf("error %q reports an account failure for a Ctrl+C", err)
	}
}

func TestRunShow_When_FolderNameCarriesControlCharacters_Then_TableShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":         nil,
		hostileWireName: fakeMsgs("a@x"),
	})
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	cfg := e2eConfig(src, dst, 1)
	out := &syncBuffer{}

	if err := runShow(e2eContext(t), cfg, e2eShowOptions(out, false)); err != nil {
		t.Fatalf("runShow: %v", err)
	}

	got := stripANSI(out.String())
	_, tables, ok := strings.Cut(got, "Source")
	if !ok {
		t.Fatalf("output has no Source title:\n%s", got)
	}
	srcSection, _, ok := strings.Cut(tables, "Destination")
	if !ok {
		t.Fatalf("output has no Destination title:\n%s", got)
	}
	assertFolderRow(t, srcSection, hostileShownName, 1, uint64(len(imapFullBody("a@x"))))
	assertContainsNone(t, out.String(), hostileName, "\u202e", "\x1b[2J")
}
