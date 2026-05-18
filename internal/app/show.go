// Package app holds the orchestration logic for the imapsync-go CLI subcommands.
package app

import (
	"context"
	"fmt"
	"os"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
)

// ActionShow displays information about mailboxes in source and destination IMAP accounts.
func ActionShow(ctx context.Context, c *cli.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	verbose := c.Bool("verbose")
	quiet := c.Bool("quiet")

	pw := progress.NewWriter(2, quiet)
	pw.Start()
	defer pw.Stop()

	// No pw.Log before AppendTracker: go-pretty's redraw cycle counts
	// only tracker rows when computing how far cursor-up needs to go,
	// so a log line emitted in the first render leaves the topmost
	// tracker un-erased on the next tick and StopAndClear can't reach it.
	cfg, err := config.New(c)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Dst.Label), 100)
	pw.AppendTracker(srcTracker)
	pw.AppendTracker(dstTracker)

	// loadAccount fetches mailboxes for one side and returns both the open
	// client (so we can Logout from the caller) and the mailbox slice.
	type accountResult struct {
		cli       *client.Client
		mailboxes []*client.MailboxInfo
	}
	loadAccount := func(ctx context.Context, label string, creds config.Credentials, tr *progress.Tracker) (accountResult, error) {
		tr.UpdateMessage(fmt.Sprintf("[%s] Connecting...", label))
		cli, err := client.New(ctx, creds.Server, creds.User, creds.Pass, client.Options{UseTLS: true, Verbose: verbose})
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

	defer func() {
		if srcRes.cli != nil {
			_ = srcRes.cli.Logout()
		}
		if dstRes.cli != nil {
			_ = dstRes.cli.Logout()
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	if groupErr != nil {
		return groupErr
	}

	printAccountInfo("Source", cfg.Src.Server, cfg.Src.User, srcRes.mailboxes)
	fmt.Println()
	printAccountInfo("Destination", cfg.Dst.Server, cfg.Dst.User, dstRes.mailboxes)

	return nil
}

// printAccountInfo displays mailbox information in a formatted table.
func printAccountInfo(title, server, user string, mailboxes []*client.MailboxInfo) {
	headerTable := table.NewWriter()
	headerTable.SetOutputMirror(os.Stdout)
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
	fmt.Println()

	if len(mailboxes) == 0 {
		return
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false

	t.AppendHeader(table.Row{"Folder", "Messages", "Size"})

	var totalMessages uint32
	var totalSize uint64

	for _, mbox := range mailboxes {
		totalMessages += mbox.Messages
		totalSize += mbox.Size

		t.AppendRow(table.Row{
			mbox.Name,
			mbox.Messages,
			utils.FormatSize(mbox.Size),
		})
	}

	t.AppendFooter(table.Row{
		text.Bold.Sprint(fmt.Sprintf("total folders %d", len(mailboxes))),
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
