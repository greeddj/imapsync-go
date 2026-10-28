package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
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

func assertRowCells(t *testing.T, section, name, want string) {
	t.Helper()
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

func assertFolderRow(t *testing.T, section, name string, count int, size uint64) {
	t.Helper()
	assertRowCells(t, section, name, fmt.Sprintf("%d %s", count, utils.FormatSize(size)))
}

func assertMarkedRow(t *testing.T, section, name, marker string) {
	t.Helper()
	assertRowCells(t, section, name, marker+" -")
}

func assertTotalsRow(t *testing.T, section string, folders, messages int, size uint64) {
	t.Helper()
	assertTotalsLabel(t, section, fmt.Sprintf("TOTAL FOLDERS %d", folders), messages, size)
}

func assertTotalsLabel(t *testing.T, section, label string, messages int, size uint64) {
	t.Helper()
	want := fmt.Sprintf("%s %d %s", label, messages, utils.FormatSize(size))
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
			assertContainsNone(t, got, e2ePassword, "cannot be read", "UNREADABLE")
			assertReadOnly(t, "source", src)
			assertReadOnly(t, "destination", dst)
		})
	}
}

func TestRunShow_When_FoldersCannotBeRead_Then_MarksRowsListsReasonsAndFails(t *testing.T) {
	t.Parallel()

	for _, quiet := range []bool{true, false} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX":        fakeMsgs("a@x", "b@x"),
				"Work":         fakeMsgs("c@x", "d@x", "e@x"),
				"Archive":      fakeMsgs("f@x"),
				"[Gmail]/Sent": fakeMsgs("g@x"),
			})
			src.markNoSelect("[Gmail]")
			src.refuseStatusWith("Work", "NO [UNAVAILABLE] Temporary failure")
			src.refuseOpenWith("Archive", "NO [UNAVAILABLE] Archive offline")
			dst := newStoreServer(t, "/", map[string][]fakeMsg{
				"INBOX": fakeMsgs("h@x"),
				"Junk":  fakeMsgs("i@x"),
			})
			dst.refuseStatusWith("Junk", "NO [UNAVAILABLE] Junk offline")
			out := &syncBuffer{}

			err := runShow(e2eContext(t), e2eConfig(src, dst, 1), e2eShowOptions(out, quiet))

			if !errors.Is(err, ErrSilentExit) {
				t.Errorf("runShow error = %v, want ErrSilentExit", err)
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

			msgSize := uint64(len(imapFullBody("a@x")))
			assertMarkedRow(t, srcSection, "Archive", "unreadable")
			assertFolderRow(t, srcSection, "INBOX", 2, 2*msgSize)
			assertMarkedRow(t, srcSection, "Work", "unreadable")
			assertMarkedRow(t, srcSection, "[Gmail]", "container")
			assertFolderRow(t, srcSection, "[Gmail]/Sent", 1, msgSize)
			assertTotalsLabel(t, srcSection, "TOTAL FOLDERS 5 (2 UNREADABLE)", 3, 3*msgSize)
			assertFolderRow(t, dstSection, "INBOX", 1, msgSize)
			assertMarkedRow(t, dstSection, "Junk", "unreadable")
			assertTotalsLabel(t, dstSection, "TOTAL FOLDERS 2 (1 UNREADABLE)", 1, msgSize)

			if n := strings.Count(got, "Folders that cannot be read"); n != 1 {
				t.Errorf("the reason header appears %d times, want 1", n)
			}
			assertInOrder(t, got,
				"Destination",
				"Folders that cannot be read",
				"• Source Archive: read folder size: Archive offline",
				"• Source Work: read message count: Temporary failure",
				"• Destination Junk: read message count: Junk offline",
			)
			assertContainsNone(t, got, "• Source [Gmail]", "• Source INBOX", "• Destination INBOX", e2ePassword)
			if slices.Contains(src.capturedNames("STATUS"), "[Gmail]") {
				t.Error("source sent STATUS for [Gmail], a folder its LIST marks \\Noselect")
			}
			assertReadOnly(t, "source", src)
			assertReadOnly(t, "destination", dst)
		})
	}
}

func TestRunShow_When_AccountHasNoSelectContainer_Then_ShowsContainerAndSucceeds(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":        fakeMsgs("a@x"),
		"[Gmail]/Sent": fakeMsgs("b@x", "c@x"),
	})
	src.markNoSelect("[Gmail]")
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}

	if err := runShow(e2eContext(t), e2eConfig(src, dst, 1), e2eShowOptions(out, true)); err != nil {
		t.Fatalf("runShow: %v", err)
	}

	got := stripANSI(out.String())
	srcSection, _, ok := strings.Cut(got, "Destination")
	if !ok {
		t.Fatalf("output has no Destination title:\n%s", got)
	}
	msgSize := uint64(len(imapFullBody("a@x")))
	assertFolderRow(t, srcSection, "INBOX", 1, msgSize)
	assertMarkedRow(t, srcSection, "[Gmail]", "container")
	assertFolderRow(t, srcSection, "[Gmail]/Sent", 2, 2*msgSize)
	assertTotalsRow(t, srcSection, 3, 3, 3*msgSize)
	assertContainsNone(t, got, "UNREADABLE", "cannot be read")
	if slices.Contains(src.capturedNames("STATUS"), "[Gmail]") || slices.Contains(src.capturedNames("EXAMINE"), "[Gmail]") {
		t.Error("source sent a command for [Gmail], a folder its LIST marks \\Noselect")
	}
	assertReadOnly(t, "source", src)
}

func TestRunShow_When_UnreadableFolderHasHostileNameAndReply_Then_ReasonListShowsEscapes(t *testing.T) {
	t.Parallel()

	src := newStoreServer(t, "/", map[string][]fakeMsg{
		"INBOX":         nil,
		hostileWireName: fakeMsgs("a@x"),
	})
	src.refuseStatusWith(hostileWireName, "NO "+hostileReply)
	dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil})
	out := &syncBuffer{}

	err := runShow(e2eContext(t), e2eConfig(src, dst, 1), e2eShowOptions(out, false))

	if !errors.Is(err, ErrSilentExit) {
		t.Fatalf("runShow error = %v, want ErrSilentExit", err)
	}
	assertContainsAll(t, stripANSI(out.String()),
		"• Source "+hostileShownName+": read message count: "+hostileReplyShown+"\n")
	assertContainsNone(t, out.String(), hostileName, "\u202e", "\x1b]0;pwned", "\x1b[2J")
	if got := src.capturedNames("STATUS"); !slices.Contains(got, hostileWireName) {
		t.Errorf("STATUS names = %q, want the exact wire name %q among them", got, hostileWireName)
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
		markers    map[string]string
		name       string
		mailboxes  []*client.MailboxInfo
		unreadable int
	}{
		{
			name: "two mailboxes",
			mailboxes: []*client.MailboxInfo{
				{Name: "INBOX", Messages: 3, Size: 2048},
				{Name: "Archive", Messages: 10, Size: 5 * 1024 * 1024},
			},
		},
		{
			// The unreadable row sets Messages and Size on purpose: the sums must leave
			// it out because of Err, not because its values are zero.
			name: "unreadable and container rows stay out of the sums",
			mailboxes: []*client.MailboxInfo{
				{Name: "INBOX", Messages: 3, Size: 2048},
				{Name: "Broken", Messages: 7, Size: 999, Err: errors.New("read message count: status refused")},
				{Name: "[Gmail]", NoSelect: true},
			},
			markers:    map[string]string{"Broken": "unreadable", "[Gmail]": "container"},
			unreadable: 1,
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
				if marker, ok := tt.markers[mb.Name]; ok {
					assertMarkedRow(t, got, mb.Name, marker)
					continue
				}
				assertFolderRow(t, got, mb.Name, int(mb.Messages), mb.Size)
				messages += int(mb.Messages)
				size += mb.Size
			}
			label := fmt.Sprintf("TOTAL FOLDERS %d", len(tt.mailboxes))
			if tt.unreadable > 0 {
				label += fmt.Sprintf(" (%d UNREADABLE)", tt.unreadable)
			}
			assertTotalsLabel(t, got, label, messages, size)
			assertContainsNone(t, got, "status refused")
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

func TestActionShow_When_AuthIsUnsupported_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	srcDialed, dstDialed := watchDials(src), watchDials(dst)
	cfgPath := writeAuthConfig(t, ".json", src, dst, "dst", "crammd5")

	err := runCLIAction(t, ActionShow, "show", showFlags(), "-c", cfgPath, "show", "-q")

	if !errors.Is(err, config.ErrAuthUnsupported) {
		t.Fatalf("error = %v, want %v", err, config.ErrAuthUnsupported)
	}
	for _, want := range []string{"load config", "dst: "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	for _, unwanted := range []string{`"crammd5"`, "crammd5"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error = %q, want no value %s", err, unwanted)
		}
	}
	if strings.Contains(err.Error(), e2ePassword) {
		t.Errorf("error = %q, want no password", err)
	}
	if srcDialed.Load() || dstDialed.Load() {
		t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
	}
	assertNoLogin(t, src, dst)
}

func TestActionShow_When_MapEntryHasNoFolderName_Then_RefusesBeforeConnecting(t *testing.T) {
	t.Parallel()

	src, dst := newFakeServer(t), newFakeServer(t)
	srcDialed, dstDialed := watchDials(src), watchDials(dst)
	cfgPath := writeRawConfig(t, ".json", src, dst, `,"map":[{"src":"INBOX","dst":""}]`)

	err := runCLIAction(t, ActionShow, "show", showFlags(), "-c", cfgPath, "show", "-q")

	if !errors.Is(err, config.ErrMapDstRequired) {
		t.Fatalf("error = %v, want %v", err, config.ErrMapDstRequired)
	}
	if !strings.Contains(err.Error(), "load config") || !strings.Contains(err.Error(), "map entry 1: ") {
		t.Errorf("error = %q, want %q and %q", err, "load config", "map entry 1: ")
	}
	if srcDialed.Load() || dstDialed.Load() {
		t.Errorf("dialed source = %v, destination = %v, want no connection", srcDialed.Load(), dstDialed.Load())
	}
	assertNoLogin(t, src, dst)
}

func TestRunShow_When_ListingMailboxesFails_Then_NamesTheFailingAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		want          string
		failSrc       bool
		failAtConnect bool
	}{
		{name: "source listing fails", failSrc: true, want: "[alpha] list mailboxes"},
		{name: "destination listing fails", want: "[beta] list mailboxes"},
		{name: "source listing fails at connect", failSrc: true, failAtConnect: true, want: "[alpha] list mailboxes"},
		{name: "destination listing fails at connect", failAtConnect: true, want: "[beta] list mailboxes"},
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
			if tt.failAtConnect {
				failing.addConnHandler(scriptedHandler(failing, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
					if verb == "LIST" {
						return "NO [CANNOT] list refused", true
					}
					return "", false
				}))
			} else {
				failing.addConnHandler(listFailsAfterConnectHandler(failing, "INBOX"))
			}
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

func TestRunShow_When_ContextEndsWhileReadingStatistics_Then_ReturnsContextErrorWithoutFailureBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		handler func(srv *fakeServer, cancel func()) func(net.Conn)
		name    string
	}{
		{
			name: "STATUS is pending",
			handler: func(srv *fakeServer, cancel func()) func(net.Conn) {
				return scriptedHandler(srv, []string{"INBOX"}, func(verb string, _ int) (string, bool) {
					if verb == "STATUS" {
						cancel()
						return "", true
					}
					return "", false
				})
			},
		},
		{
			name:    "size FETCH is pending",
			handler: stallOnSizeFetchHandler,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(e2eContext(t))
			t.Cleanup(cancel)
			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("b@x")})
			src.addConnHandler(tt.handler(src, cancel))
			out := &syncBuffer{}

			err := runShow(ctx, e2eConfig(src, dst, 1), e2eShowOptions(out, true))

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if errors.Is(err, ErrSilentExit) {
				t.Errorf("error = %v, want no ErrSilentExit for a Ctrl+C", err)
			}
			assertContainsNone(t, out.String(), "cannot be read", "unreadable", "UNREADABLE")
		})
	}
}

func TestRunShow_When_FolderNameIsNotModifiedUTF7_Then_TableLeavesItOutAndWarns(t *testing.T) {
	t.Parallel()

	const (
		hostile      = "\x1b[2J\u202eevil"
		hostileShown = "\\x1b[2J\\u202eevil"
	)
	for _, quiet := range []bool{true, false} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			t.Parallel()

			src := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": fakeMsgs("a@x"), hostile: fakeMsgs("b@x")})
			dst := newStoreServer(t, "/", map[string][]fakeMsg{"INBOX": nil, "Q&A": nil})
			out := &syncBuffer{}

			if err := runShow(e2eContext(t), e2eConfig(src, dst, 1), e2eShowOptions(out, quiet)); err != nil {
				t.Fatalf("runShow: %v", err)
			}

			got := stripANSI(out.String())
			tables, warnings, ok := strings.Cut(got, "⚠️")
			if !ok {
				t.Fatalf("output has no warning:\n%s", got)
			}
			_, tables, ok = strings.Cut(tables, "Source")
			if !ok {
				t.Fatalf("output has no Source title:\n%s", got)
			}
			srcSection, dstSection, ok := strings.Cut(tables, "Destination")
			if !ok {
				t.Fatalf("output has no Destination title:\n%s", got)
			}
			size := uint64(len(imapFullBody("a@x")))
			assertFolderRow(t, srcSection, "INBOX", 1, size)
			assertTotalsRow(t, srcSection, 1, 1, size)
			assertFolderRow(t, dstSection, "INBOX", 0, 0)
			assertTotalsRow(t, dstSection, 1, 0, 0)
			assertContainsNone(t, tables, "Q&A", "evil", "cannot be read")
			assertInOrder(t, "⚠️"+warnings,
				leftOutLine("src", hostileShown),
				leftOutLine("dst", "Q&A"),
			)
			assertContainsNone(t, out.String(), "\x1b[2J", "\u202e")
			for _, name := range append(src.capturedNames("STATUS"), dst.capturedNames("STATUS")...) {
				if name != "INBOX" {
					t.Errorf("a server got STATUS for %q, want only INBOX", name)
				}
			}
		})
	}
}
