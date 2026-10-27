// Package app holds the orchestration logic for the imapsync-go CLI subcommands.
package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/termsafe"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
)

// showOptions carries what runShow needs besides the config. clientOpts holds
// the shared transport settings; runShow adds the per-side fields.
type showOptions struct {
	out        io.Writer
	clientOpts client.Options
	quiet      bool
	verbose    bool
}

// ActionShow displays information about mailboxes in source and destination IMAP accounts.
func ActionShow(ctx context.Context, c *cli.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := rejectPositionalArgs(c); err != nil {
		return err
	}

	opts := showOptions{
		out:        os.Stdout,
		clientOpts: client.Options{UseTLS: true},
		quiet:      c.Bool("quiet"),
		verbose:    c.Bool("verbose"),
	}

	cfg, err := config.New(c)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return runShow(ctx, cfg, opts)
}

func runShow(ctx context.Context, cfg *config.Config, opts showOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	out := opts.out
	verbose := opts.verbose

	pw := newProgressWriter(2, opts.quiet, out)
	pw.Start()
	defer pw.Stop()

	// No pw.Log before AppendTracker: go-pretty counts only tracker rows when
	// moving the cursor up, so an early log line leaves the top tracker un-erased.
	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Dst.Label), 100)
	traceTracker("show-src", srcTracker.Message)
	pw.AppendTracker(srcTracker)
	traceTracker("show-dst", dstTracker.Message)
	pw.AppendTracker(dstTracker)

	// loadAccount fetches mailboxes for one side and returns both the open
	// client (so we can Logout from the caller) and the mailbox slice.
	type accountResult struct {
		cli       *client.Client
		mailboxes []*client.MailboxInfo
	}
	loadAccount := func(ctx context.Context, label string, creds config.Credentials, tr *progress.Tracker) (accountResult, error) {
		tr.UpdateMessage(fmt.Sprintf("[%s] Connecting...", label))
		clientOpts := opts.clientOpts
		clientOpts.Auth = creds.Auth
		clientOpts.Verbose = verbose
		cli, err := client.New(ctx, creds.Server, creds.User, creds.Pass, clientOpts)
		if err != nil {
			tr.MarkAsErrored()
			return accountResult{}, fmt.Errorf("[%s] connect: %w", label, err)
		}
		cli.SetPrefix(label)
		cli.SetProgressWriter(pw)
		cli.SetProgressTracker(tr)

		tr.UpdateMessage(fmt.Sprintf("[%s] Fetching mailboxes...", label))
		mailboxes, err := cli.ListMailboxes(ctx)
		if err != nil {
			tr.MarkAsErrored()
			return accountResult{cli: cli}, fmt.Errorf("[%s] list mailboxes: %w", label, err)
		}
		tr.MarkAsDone()
		return accountResult{cli: cli, mailboxes: mailboxes}, nil
	}

	// Run both sides in parallel; errgroup propagates the first error and
	// keeps WithContext-derived ctx in sync so the loser cancels promptly.
	g, gCtx := errgroup.WithContext(ctx)
	var srcRes, dstRes accountResult

	g.Go(func() error {
		r, err := loadAccount(gCtx, cfg.Src.Label, cfg.Src, srcTracker)
		srcRes = r
		return err
	})
	g.Go(func() error {
		r, err := loadAccount(gCtx, cfg.Dst.Label, cfg.Dst, dstTracker)
		dstRes = r
		return err
	})

	groupErr := g.Wait()

	pw.StopAndClear()

	defer func() { logoutAll(srcRes.cli, dstRes.cli) }()

	if err := ctx.Err(); err != nil {
		return err
	}
	if groupErr != nil {
		return groupErr
	}

	printAccountInfo(out, "Source", cfg.Src.Server, cfg.Src.User, srcRes.mailboxes)
	_, _ = fmt.Fprintln(out)
	printAccountInfo(out, "Destination", cfg.Dst.Server, cfg.Dst.User, dstRes.mailboxes)

	if countUnreadable(srcRes.mailboxes)+countUnreadable(dstRes.mailboxes) == 0 {
		return nil
	}
	_, _ = fmt.Fprint(out, "\n❌ Folders that cannot be read:\n")
	printUnreadableFolders(out, "Source", srcRes.mailboxes)
	printUnreadableFolders(out, "Destination", dstRes.mailboxes)
	return ErrSilentExit
}

func countUnreadable(mailboxes []*client.MailboxInfo) int {
	n := 0
	for _, mbox := range mailboxes {
		if mbox.Err != nil {
			n++
		}
	}
	return n
}

// Name and reason can carry server text, so both go through termsafe.Escape.
func printUnreadableFolders(out io.Writer, title string, mailboxes []*client.MailboxInfo) {
	for _, mbox := range mailboxes {
		if mbox.Err == nil {
			continue
		}
		_, _ = fmt.Fprintf(out, "• %s %s: %s\n", title, termsafe.Escape(mbox.Name), termsafe.Escape(mbox.Err.Error()))
	}
}

// printAccountInfo displays mailbox information in a formatted table.
func printAccountInfo(out io.Writer, title, server, user string, mailboxes []*client.MailboxInfo) {
	headerTable := table.NewWriter()
	headerTable.SetOutputMirror(out)
	headerTable.Style().Options.DrawBorder = false
	headerTable.Style().Options.SeparateColumns = false
	// Bold + bright cyan matches the tracker palette and visually breaks
	// up two adjacent borderless tables without adding layout.
	headerTable.SetTitle(text.Colors{text.Bold, text.FgHiCyan}.Sprint(title))

	headerTable.AppendRows([]table.Row{
		{"Server", server},
		{"User", user},
	})
	headerTable.Render()
	_, _ = fmt.Fprintln(out)

	if len(mailboxes) == 0 {
		return
	}

	t := table.NewWriter()
	t.SetOutputMirror(out)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false

	t.AppendHeader(table.Row{"Folder", "Messages", "Size"})

	var totalMessages uint32
	var totalSize uint64

	for _, mbox := range mailboxes {
		name := termsafe.Escape(mbox.Name)
		switch {
		case mbox.Err != nil:
			t.AppendRow(table.Row{name, "unreadable", "-"})
		case mbox.NoSelect:
			t.AppendRow(table.Row{name, "container", "-"})
		default:
			totalMessages += mbox.Messages
			totalSize += mbox.Size
			t.AppendRow(table.Row{name, mbox.Messages, utils.FormatSize(mbox.Size)})
		}
	}

	label := fmt.Sprintf("total folders %d", len(mailboxes))
	if unreadable := countUnreadable(mailboxes); unreadable > 0 {
		label = fmt.Sprintf("total folders %d (%d unreadable)", len(mailboxes), unreadable)
	}
	t.AppendFooter(table.Row{
		text.Bold.Sprint(label),
		text.Bold.Sprintf("%d", totalMessages),
		text.Bold.Sprint(utils.FormatSize(totalSize)),
	})

	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft, AlignHeader: text.AlignCenter},
		{Number: 2, Align: text.AlignRight, AlignHeader: text.AlignCenter},
		{Number: 3, Align: text.AlignRight, AlignHeader: text.AlignCenter},
	})

	t.Render()
}
