// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/utils"
	gopretty "github.com/jedib0t/go-pretty/v6/progress"
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

	// Setup progress writer for scanning phase
	pw := progress.NewWriter(2)
	pw.Start()

	// Create trackers for source and destination scanning
	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Dst.Label), 100)

	pw.AppendTracker(srcTracker)
	pw.AppendTracker(dstTracker)

	srcTracker.UpdateMessage(fmt.Sprintf("[%s] Connecting...", cfg.Src.Label))

	srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, verbose, true, nil)
	if err != nil {
		srcTracker.MarkAsErrored()
		pw.Stop()
		return fmt.Errorf("source connection failed: %w", err)
	}
	defer func() {
		_ = srcClient.Logout()
	}()
	srcClient.SetPrefix(cfg.Src.Label)
	srcClient.SetProgressWriter(pw)
	srcClient.SetProgressTracker(srcTracker)

	dstTracker.UpdateMessage(fmt.Sprintf("[%s] Connecting...", cfg.Dst.Label))

	dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
	if err != nil {
		dstTracker.MarkAsErrored()
		pw.Stop()
		return fmt.Errorf("destination connection failed: %w", err)
	}
	defer func() {
		_ = dstClient.Logout()
	}()
	dstClient.SetPrefix(cfg.Dst.Label)
	dstClient.SetProgressWriter(pw)
	dstClient.SetProgressTracker(dstTracker)

	summary, err := buildSyncPlan(srcClient, dstClient, mappings, srcTracker, dstTracker)
	if err != nil {
		pw.Stop()
		return err
	}

	// Mark scanning as complete
	srcTracker.MarkAsDone()
	dstTracker.MarkAsDone()

	// Stop and clear progress
	pw.StopAndClear(2)

	if summary.TotalNew > 0 && !quiet {
		fmt.Printf("📤 Messages to be copied to destination:\n")
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
			fmt.Printf("\n🗂️ Folders to be created on destination:\n")
			for _, folder := range foldersToCreate {
				fmt.Printf("· %s\n", folder)
			}
		}
		fmt.Printf("\n📨 Total new messages to sync: %d\n", summary.TotalNew)

		if !autoConfirm {
			confirmed, err := utils.AskConfirm("✍️ Proceed with synchronization?")
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Println("❌ Sync canceled by user")
				return nil
			}
		}
	} else {
		fmt.Println("✅ All folders already synced!")
		return nil
	}

	// Setup progress writer for sync phase
	syncPW := progress.NewWriter(len(summary.Plans))
	syncPW.Start()

	// Create all trackers upfront
	folderTrackers := make([]*gopretty.Tracker, 0, len(summary.Plans))
	for i, plan := range summary.Plans {
		if plan.NewMessages == 0 {
			continue
		}
		folderTracker := progress.NewTracker(
			fmt.Sprintf("⚙️ %d/%d %s → %s", i+1, len(summary.Plans), plan.SourceFolder, plan.DestinationFolder),
			int64(plan.NewMessages),
		)
		syncPW.AppendTracker(folderTracker)
		folderTrackers = append(folderTrackers, folderTracker)
	}
	totalSynced := 0
	totalErrors := 0

	trackerIdx := 0
	for i, plan := range summary.Plans {
		if plan.NewMessages == 0 {
			continue
		}

		folderTracker := folderTrackers[trackerIdx]
		trackerIdx++
		if !plan.DestinationFolderExists {
			folderTracker.UpdateMessage(fmt.Sprintf("⚙️ %d/%d Creating %s", i+1, len(summary.Plans), plan.DestinationFolder))
			_, err := dstClient.CreateMailbox(plan.DestinationFolder)
			if err != nil {
				folderTracker.MarkAsErrored()
				totalErrors++
				continue
			}
		}

		folderTracker.UpdateMessage(fmt.Sprintf("⚙️ %d/%d %s → %s", i+1, len(summary.Plans), plan.SourceFolder, plan.DestinationFolder))
		synced, errors := syncFolders(cfg, plan.DestinationFolder, plan.MessagesToSync, cfg.Workers, verbose, syncPW, folderTracker)
		totalSynced += synced
		totalErrors += errors

		if errors > 0 {
			folderTracker.MarkAsErrored()
		} else {
			folderTracker.MarkAsDone()
		}
	}

	// Stop and clear progress
	syncPW.StopAndClear(len(folderTrackers))

	if totalErrors > 0 {
		fmt.Printf("❌ Sync completed with errors. %d messages uploaded, %d errors occurred\n", totalSynced, totalErrors)
		return fmt.Errorf("sync completed with %d errors", totalErrors)
	}

	fmt.Println("✨ Sync completed successfully. ✨")
	return nil
}

// syncFolders syncs messages using multiple parallel workers.
func syncFolders(cfg *config.Config, dstFolder string, messages []*imap.Message, numWorkers int, verbose bool, pw *progress.Writer, tracker *gopretty.Tracker) (int, int) {
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
				pw.Log("Worker %d: failed to connect: %v", workerID, err)
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer func() {
				_ = workerClient.Logout()
			}()
			workerClient.SetPrefix(fmt.Sprintf("%s-%d", cfg.Dst.Label, workerID))

			for msg := range jobs {
				if err := workerClient.AppendMessage(dstFolder, msg); err != nil {
					atomic.AddInt64(&errorCount, 1)
				} else {
					synced := atomic.AddInt64(&syncedCount, 1)
					tracker.Increment(1)
					if verbose {
						pw.Log("Synced %d/%d messages to %s", synced, len(messages), dstFolder)
					}
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
func buildSyncPlan(srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, srcTracker, dstTracker *gopretty.Tracker) (*SyncSummary, error) {
	summary := &SyncSummary{
		Plans: make([]FolderSyncPlan, 0),
	}

	// Set tracker totals based on number of folders to scan
	srcTracker.UpdateTotal(int64(len(mappings)))
	dstTracker.UpdateTotal(int64(len(mappings)))

	for idx, mapping := range mappings {
		srcFolder := mapping.Source
		dstFolder := mapping.Destination
		var dstFolderExists bool
		var srcMessageIDs map[string]bool
		var dstMessageIDs map[string]bool
		var srcErr error

		// Fetch IDs from both servers in parallel (fast - envelopes only)
		var wg sync.WaitGroup

		// Update trackers
		srcTracker.UpdateMessage(fmt.Sprintf("Scanning %s (%d/%d)", srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("Scanning %s (%d/%d)", dstFolder, idx+1, len(mappings)))

		// Fetch source message IDs
		wg.Go(func() {
			srcMessageIDs, srcErr = srcClient.FetchMessageIDs(srcFolder)
		})

		wg.Go(func() {
			dstMessageIDs = make(map[string]bool)

			fetchedIDs, err := dstClient.FetchMessageIDs(dstFolder)
			if err != nil {
				// Folder might not exist yet, not a fatal error
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
			srcTracker.Increment(1)
			dstTracker.Increment(1)
			continue
		}

		// Fetch full bodies only for messages that need syncing
		messagesToSync, err := srcClient.FetchMessagesByIDs(srcFolder, newIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch messages from %s: %w", srcFolder, err)
		}
		srcTracker.Increment(1)
		dstTracker.Increment(1)

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
