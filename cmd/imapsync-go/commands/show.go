// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"os"
	"sync"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/urfave/cli/v2"
)

// Show displays information about mailboxes in source and destination IMAP accounts.
func Show(cCtx *cli.Context) error {
	// Setup progress writer
	pw := progress.NewWriter(2, false)
	pw.Start()

	pw.Log("Loading configuration...")
	cfg, err := config.New(cCtx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// accountResult holds the result of fetching account information.
	type accountResult struct {
		client    *client.Client        // IMAP client connection.
		mailboxes []*client.MailboxInfo // List of mailboxes.
		err       error                 // Error if fetch failed.
	}

	srcResult := make(chan accountResult, 1)
	dstResult := make(chan accountResult, 1)

	// Create trackers for both servers
	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Loading mailboxes", cfg.Dst.Label), 100)

	pw.AppendTracker(srcTracker)
	pw.AppendTracker(dstTracker)

	var wg sync.WaitGroup

	wg.Go(func() {
		result := accountResult{}
		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Connecting...", cfg.Src.Label))
		srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, false, true, nil)
		if err != nil {
			result.err = fmt.Errorf("[%s] source connection failed: %v", cfg.Src.Label, err)
			srcTracker.MarkAsErrored()
			srcResult <- result
			return
		}
		result.client = srcClient
		srcClient.SetPrefix(cfg.Src.Label)
		srcClient.SetProgressWriter(pw)
		srcClient.SetProgressTracker(srcTracker)

		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Fetching mailboxes...", cfg.Src.Label))
		mailboxes, err := srcClient.ListMailboxes()
		if err != nil {
			result.err = fmt.Errorf("[%s] failed to list source mailboxes: %v", cfg.Src.Label, err)
			srcTracker.MarkAsErrored()
			srcResult <- result
			return
		}

		result.mailboxes = mailboxes
		srcTracker.MarkAsDone()
		srcResult <- result
	})

	wg.Go(func() {
		result := accountResult{}
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Connecting...", cfg.Dst.Label))
		dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, false, true, nil)
		if err != nil {
			result.err = fmt.Errorf("[%s] destination connection failed: %v", cfg.Dst.Label, err)
			dstTracker.MarkAsErrored()
			dstResult <- result
			return
		}
		result.client = dstClient
		dstClient.SetPrefix(cfg.Dst.Label)
		dstClient.SetProgressWriter(pw)
		dstClient.SetProgressTracker(dstTracker)

		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Fetching mailboxes...", cfg.Dst.Label))
		mailboxes, err := dstClient.ListMailboxes()
		if err != nil {
			result.err = fmt.Errorf("[%s] failed to list destination mailboxes: %v", cfg.Dst.Label, err)
			dstTracker.MarkAsErrored()
			dstResult <- result
			return
		}

		result.mailboxes = mailboxes
		dstTracker.MarkAsDone()
		dstResult <- result
	})

	wg.Wait()
	close(srcResult)
	close(dstResult)

	srcRes := <-srcResult
	dstRes := <-dstResult

	// Stop progress and clear output
	pw.StopAndClear(2)

	// Cleanup function to logout from both clients.
	cleanup := func() {
		if srcRes.client != nil {
			_ = srcRes.client.Logout()
		}
		if dstRes.client != nil {
			_ = dstRes.client.Logout()
		}
	}
	defer cleanup()

	// Check for errors from either account.
	if srcRes.err != nil {
		return srcRes.err
	}
	if dstRes.err != nil {
		return dstRes.err
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
	headerTable.SetTitle(title)

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
