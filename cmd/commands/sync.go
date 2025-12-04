// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v2"
)

const (
	// jobChannelBuffer defines the buffer size for the worker job channel.
	jobChannelBuffer = 100
)

// FolderSyncPlan describes how a single source folder should be copied to its destination.
type FolderSyncPlan struct {
	SourceFolder            string
	DestinationFolder       string
	DestinationFolderExists bool
	NewMessages             int
	MessagesToSync          []*imap.Message
}

// SyncSummary aggregates the per-folder plans along with total message counts.
type SyncSummary struct {
	TotalNew int
	Plans    []FolderSyncPlan
}

// Sync copies messages between IMAP servers according to the provided configuration.
func Sync(cCtx *cli.Context) error {
	srcFolder := cCtx.String("src-folder")
	dstFolder := cCtx.String("dest-folder")
	quiet := cCtx.Bool("quiet")
	verbose := cCtx.Bool("verbose")
	autoConfirm := cCtx.Bool("confirm")

	fmt.Println("Fetching config...")
	cfg, err := config.New(cCtx)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	fmt.Printf("Starting sync with %d workers\n", cfg.Workers)

	var mappings []config.DirectoryMapping

	if srcFolder != "" && dstFolder != "" {
		mappings = []config.DirectoryMapping{
			{Source: srcFolder, Destination: dstFolder},
		}
	} else if srcFolder != "" || dstFolder != "" {
		fmt.Println("both --src-folder and --dest-folder must be specified")
		return fmt.Errorf("both --src-folder and --dest-folder must be specified")
	} else {
		if len(cfg.Map) == 0 {
			return fmt.Errorf("no folder mappings in config")
		}
		mappings = cfg.Map
	}

	fmt.Println("Fetching source...")
	srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("source connection failed: %w", err)
	}
	defer func() {
		_ = srcClient.Logout()
	}()
	srcClient.SetPrefix(cfg.Src.Label)

	fmt.Println("Fetching destination...")
	dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("destination connection failed: %w", err)
	}
	defer func() {
		_ = dstClient.Logout()
	}()
	dstClient.SetPrefix(cfg.Dst.Label)

	fmt.Println("Building sync plan...")
	summary, err := buildSyncPlan(srcClient, dstClient, mappings)
	if err != nil {
		return err
	}

	if summary.TotalNew > 0 && !quiet {
		fmt.Printf("Messages to be copied to destination:\n")
		foldersToCreate := make([]string, 0, len(summary.Plans))
		for _, plan := range summary.Plans {
			foldersToCreate = append(foldersToCreate, plan.DestinationFolder)
			if len(plan.MessagesToSync) > 0 {
				fmt.Printf("· %s → %s will copy messages %d\n", plan.SourceFolder, plan.DestinationFolder, len(plan.MessagesToSync))
				if verbose {
					for _, msg := range plan.MessagesToSync {
						fmt.Printf("  · %s (ID: %s)\n", msg.Envelope.Subject, msg.Envelope.MessageId)
					}
					fmt.Println()
				}
			}
		}

		if len(foldersToCreate) > 0 {
			fmt.Printf("\nFolders to be created on destination:\n")
			for _, folder := range foldersToCreate {
				fmt.Printf("· %s\n", folder)
			}
		}
		fmt.Printf("\nTotal new messages to sync: %d\n", summary.TotalNew)

		if !autoConfirm {
			confirmed, err := utils.AskConfirm("Proceed with synchronization?")
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Println("Sync canceled by user")
				return nil
			}
		}
	} else {
		fmt.Println("All folders already synced!")
		return nil
	}

	totalSynced := 0
	totalErrors := 0

	for i, plan := range summary.Plans {
		if plan.NewMessages == 0 {
			continue
		}

		fmt.Printf("Syncing folder %d/%d: %s → %s (%d messages)\n", i+1, len(summary.Plans), plan.SourceFolder, plan.DestinationFolder, plan.NewMessages)

		fmt.Printf("Checking folder: %s\n", plan.DestinationFolder)
		if !plan.DestinationFolderExists {
			created, err := dstClient.CreateMailbox(plan.DestinationFolder)
			if err != nil {
				fmt.Printf("Failed to create folder %s: %v\n", plan.DestinationFolder, err)
				totalErrors++
				continue
			}
			if created {
				fmt.Printf("Created destination folder: %s\n", plan.DestinationFolder)
			}
		}

		synced, errors := syncFolders(cfg, plan.DestinationFolder, plan.MessagesToSync, cfg.Workers, verbose)
		totalSynced += synced
		totalErrors += errors
	}

	if totalErrors > 0 {
		fmt.Printf("Sync completed with errors. %d messages uploaded, %d errors occurred\n", totalSynced, totalErrors)
		return fmt.Errorf("sync completed with %d errors", totalErrors)
	}

	fmt.Printf("Sync completed successfully. %d new messages uploaded\n", totalSynced)
	return nil
}

// syncFolders syncs messages using multiple parallel workers.
func syncFolders(cfg *config.Config, dstFolder string, messages []*imap.Message, numWorkers int, verbose bool) (int, int) {
	jobs := make(chan *imap.Message, jobChannelBuffer)
	var wg sync.WaitGroup
	var syncedCount int64
	var errorCount int64

	for i := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			workerClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
			if err != nil {
				fmt.Printf("Worker %d: failed to connect: %v\n", workerID, err)
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer func() {
				_ = workerClient.Logout()
			}()
			workerClient.SetPrefix(fmt.Sprintf("%s-%d", cfg.Dst.Label, workerID))

			for msg := range jobs {
				if err := workerClient.AppendMessage(dstFolder, msg); err != nil {
					fmt.Printf("Worker %d: error syncing dir %s on message: %v\n", workerID, dstFolder, err)
					atomic.AddInt64(&errorCount, 1)
				} else {
					atomic.AddInt64(&syncedCount, 1)
					fmt.Printf("Syncing dir %s [messages %d/%d]\n", dstFolder, atomic.LoadInt64(&syncedCount), len(messages))
				}
			}
		}(i)
	}

	for _, msg := range messages {
		jobs <- msg
	}
	close(jobs)

	wg.Wait()

	return int(syncedCount), int(errorCount)
}

// buildSyncPlan compares source and destination folders to determine what needs syncing.
func buildSyncPlan(srcClient, dstClient *client.Client, mappings []config.DirectoryMapping) (*SyncSummary, error) {
	summary := &SyncSummary{
		Plans: make([]FolderSyncPlan, 0),
	}

	for _, mapping := range mappings {
		srcFolder := mapping.Source
		dstFolder := mapping.Destination
		var dstFolderExists bool
		var srcMessageIDs map[string]bool
		var dstMessageIDs map[string]bool
		var srcErr error

		// Fetch IDs from both servers in parallel (fast - envelopes only)
		var wg sync.WaitGroup

		// Fetch source message IDs
		wg.Go(func() {
			fmt.Printf("Fetching source IDs: %s\n", srcFolder)
			srcMessageIDs, srcErr = srcClient.FetchMessageIDs(srcFolder)
		})

		wg.Go(func() {
			dstMessageIDs = make(map[string]bool)

			fmt.Printf("Fetching destination IDs: %s\n", dstFolder)
			fetchedIDs, err := dstClient.FetchMessageIDs(dstFolder)
			if err != nil {
				// Folder might not exist yet, not a fatal error
				fmt.Printf("Destination folder %s not found or empty, will create\n", dstFolder)
			} else {
				dstFolderExists = true
				dstMessageIDs = fetchedIDs
			}
		})

		wg.Wait()

		if srcErr != nil {
			return nil, fmt.Errorf("failed to fetch source folder %s: %w", srcFolder, srcErr)
		}

		// Find IDs that need syncing (exist in source but not in destination)
		newIDs := make(map[string]bool)
		for id := range srcMessageIDs {
			if !dstMessageIDs[id] {
				newIDs[id] = true
			}
		}

		if len(newIDs) == 0 {
			fmt.Printf("Folder %s: all %d messages already synced\n", srcFolder, len(srcMessageIDs))
			continue
		}

		fmt.Printf("Folder %s: %d new messages to sync (of %d total)\n", srcFolder, len(newIDs), len(srcMessageIDs))
		// Fetch full bodies only for messages that need syncing
		messagesToSync, err := srcClient.FetchMessagesByIDs(srcFolder, newIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch messages from %s: %w", srcFolder, err)
		}
		// Body fetch completed

		if len(messagesToSync) > 0 {
			plan := FolderSyncPlan{
				SourceFolder:            srcFolder,
				DestinationFolder:       dstFolder,
				DestinationFolderExists: dstFolderExists,
				NewMessages:             len(messagesToSync),
				MessagesToSync:          messagesToSync,
			}
			summary.Plans = append(summary.Plans, plan)
			summary.TotalNew += len(messagesToSync)
		}
	}

	return summary, nil
}
