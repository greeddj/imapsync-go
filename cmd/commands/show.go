// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"os"
	"sync"

	"github.com/greeddj/imapsync-go/internal/cache"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/stdout"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/urfave/cli/v2"
)

// Show displays information about mailboxes in source and destination IMAP accounts.
func Show(cCtx *cli.Context) error {
	verbose := cCtx.Bool("verbose")
	useCached := cCtx.Bool("cached")

	spin := stdout.New(false, verbose)
	defer spin.Stop()

	spin.Update("Loading configuration...")
	cfg, err := config.New(cCtx)
	if err != nil {
		spin.Error(fmt.Sprintf("load config: %v", err))
		return fmt.Errorf("load config: %w", err)
	}

	spin.Update("Preparing cache manager...")

	cacheManager, err := cache.NewCacheManager(cfg.Src, cfg.Dst)
	if err != nil {
		spin.Error(fmt.Sprintf("cache manager error: %v", err))
		return fmt.Errorf("cache manager error: %v", err)
	}

	var srcMailboxes, dstMailboxes []*client.MailboxInfo

	if useCached {
		spin.Update("Loading from cache...")

		err := cacheManager.Load()
		if err == nil {
			srcCached := cacheManager.GetSourceMailboxes()
			dstCached := cacheManager.GetDestMailboxes()

			if len(srcCached) > 0 || len(dstCached) > 0 {
				spin.Update("Using cached mailbox metadata")

				srcMailboxes = make([]*client.MailboxInfo, len(srcCached))
				for i, m := range srcCached {
					srcMailboxes[i] = &client.MailboxInfo{
						Name:     m.Name,
						Messages: m.Messages,
						Size:     m.Size,
					}
				}

				dstMailboxes = make([]*client.MailboxInfo, len(dstCached))
				for i, m := range dstCached {
					dstMailboxes[i] = &client.MailboxInfo{
						Name:     m.Name,
						Messages: m.Messages,
						Size:     m.Size,
					}
				}

				printAccountInfo("Source (cached)", cfg.Src.Server, cfg.Src.User, srcMailboxes, spin)
				fmt.Println()
				printAccountInfo("Destination (cached)", cfg.Dst.Server, cfg.Dst.User, dstMailboxes, spin)
				return nil
			}
		}
		spin.Update("⚠️ Cache missing or stale, fetching live data...")
	}

	// accountResult holds the result of fetching account information.
	type accountResult struct {
		client    *client.Client        // IMAP client connection.
		mailboxes []*client.MailboxInfo // List of mailboxes.
		err       error                 // Error if fetch failed.
	}

	srcResult := make(chan accountResult, 1)
	dstResult := make(chan accountResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		result := accountResult{}

		spin.Update(fmt.Sprintf("[%s] Connecting to source...", cfg.Src.Label))

		srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, verbose, true, nil)
		if err != nil {
			result.err = fmt.Errorf("[%s] source connection failed: %v", cfg.Src.Label, err)
			srcResult <- result
			return
		}
		result.client = srcClient
		srcClient.SetProgress(spin)
		srcClient.SetPrefix(cfg.Src.Label)

		mailboxes, err := srcClient.ListMailboxes()
		if err != nil {
			result.err = fmt.Errorf("[%s] failed to list source mailboxes: %v", cfg.Src.Label, err)
			srcResult <- result
			return
		}

		result.mailboxes = mailboxes
		srcResult <- result
	}()

	go func() {
		defer wg.Done()
		result := accountResult{}

		spin.Update(fmt.Sprintf("[%s] Connecting to destination...", cfg.Dst.Label))
		dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
		if err != nil {
			result.err = fmt.Errorf("[%s] destination connection failed: %v", cfg.Dst.Label, err)
			dstResult <- result
			return
		}
		result.client = dstClient
		dstClient.SetProgress(spin)
		dstClient.SetPrefix(cfg.Dst.Label)

		mailboxes, err := dstClient.ListMailboxes()
		if err != nil {
			result.err = fmt.Errorf("[%s] failed to list destination mailboxes: %v", cfg.Dst.Label, err)
			dstResult <- result
			return
		}

		result.mailboxes = mailboxes
		dstResult <- result
	}()

	wg.Wait()
	close(srcResult)
	close(dstResult)

	srcRes := <-srcResult
	dstRes := <-dstResult

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

	spin.Success("Mailbox metadata collected.")

	if !useCached {
		for _, mbox := range srcRes.mailboxes {
			cacheManager.SourceCache.Mailboxes[mbox.Name] = &cache.MailboxCache{
				Mailbox:      mbox.Name,
				MessageCount: mbox.Messages,
				TotalSize:    mbox.Size,
				Messages:     make(map[string]*cache.MessageInfo),
			}
		}

		for _, mbox := range dstRes.mailboxes {
			cacheManager.DestCache.Mailboxes[mbox.Name] = &cache.MailboxCache{
				Mailbox:      mbox.Name,
				MessageCount: mbox.Messages,
				TotalSize:    mbox.Size,
				Messages:     make(map[string]*cache.MessageInfo),
			}
		}

		if err := cacheManager.Save(); err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
			}
		}
	}

	printAccountInfo("Source", cfg.Src.Server, cfg.Src.User, srcRes.mailboxes, spin)
	fmt.Println()
	printAccountInfo("Destination", cfg.Dst.Server, cfg.Dst.User, dstRes.mailboxes, spin)

	return nil
}

// printAccountInfo displays mailbox information in a formatted table.
func printAccountInfo(title, server, user string, mailboxes []*client.MailboxInfo, spin *stdout.Spinner) {
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
		spin.Error("No folders found")
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
