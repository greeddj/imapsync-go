// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v2"
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

	if !quiet && verbose {
		fmt.Println("Fetching config...")
	}
	cfg, err := config.New(cCtx)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	if !quiet && verbose {
		fmt.Printf("Starting sync with %d workers\n", cfg.Workers)
	}

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

	if !quiet && verbose {
		fmt.Println("Connecting to servers...")
	}

	srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("source connection failed: %w", err)
	}
	defer func() {
		_ = srcClient.Logout()
	}()
	srcClient.SetPrefix(cfg.Src.Label)

	dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("destination connection failed: %w", err)
	}
	defer func() {
		_ = dstClient.Logout()
	}()
	dstClient.SetPrefix(cfg.Dst.Label)

	// Check delimiters
	srcDelimiter := srcClient.GetDelimiter()
	dstDelimiter := dstClient.GetDelimiter()

	if !quiet && verbose {
		fmt.Printf("📁 Server delimiters:\n")
		fmt.Printf("  Source [%s]: %q\n", cfg.Src.Label, srcDelimiter)
		fmt.Printf("  Destination [%s]: %q\n\n", cfg.Dst.Label, dstDelimiter)
	}

	// Validate folder paths compatibility with server delimiters
	var validationErrors []string
	needsFix := false
	for i, mapping := range mappings {
		// Check source folder compatibility
		if srcDelimiter != "" && !validateFolderPath(mapping.Source, srcDelimiter) {
			oldDelim := detectDelimiter(mapping.Source)
			validationErrors = append(validationErrors,
				fmt.Sprintf("Mapping %d: Source folder %q uses delimiter %q, server expects %q",
					i+1, mapping.Source, oldDelim, srcDelimiter))
			needsFix = true
		}

		// Check destination folder compatibility
		if dstDelimiter != "" && !validateFolderPath(mapping.Destination, dstDelimiter) {
			oldDelim := detectDelimiter(mapping.Destination)
			validationErrors = append(validationErrors,
				fmt.Sprintf("Mapping %d: Destination folder %q uses delimiter %q, server expects %q",
					i+1, mapping.Destination, oldDelim, dstDelimiter))
			needsFix = true
		}
	}

	if needsFix {
		fmt.Printf("⚠️  Folder path delimiter mismatch detected:\n")
		for _, err := range validationErrors {
			fmt.Printf("  • %s\n", err)
		}
		fmt.Println()

		var shouldFix bool
		if autoConfirm {
			shouldFix = true
			fmt.Println("✅ Auto-confirming delimiter fix...")
		} else {
			confirmed, err := utils.AskConfirm("🔧 Fix folder delimiters to match server configuration?")
			if err != nil {
				return err
			}
			shouldFix = confirmed
		}

		if shouldFix {
			// Fix delimiters in mappings
			for i := range mappings {
				if srcDelimiter != "" {
					oldDelim := detectDelimiter(mappings[i].Source)
					if oldDelim != "none" && oldDelim != srcDelimiter {
						oldPath := mappings[i].Source
						mappings[i].Source = strings.ReplaceAll(mappings[i].Source, oldDelim, srcDelimiter)
						fmt.Printf("  ✓ Fixed source: %q → %q\n", oldPath, mappings[i].Source)
					}
				}
				if dstDelimiter != "" {
					oldDelim := detectDelimiter(mappings[i].Destination)
					if oldDelim != "none" && oldDelim != dstDelimiter {
						oldPath := mappings[i].Destination
						mappings[i].Destination = strings.ReplaceAll(mappings[i].Destination, oldDelim, dstDelimiter)
						fmt.Printf("  ✓ Fixed destination: %q → %q\n", oldPath, mappings[i].Destination)
					}
				}
			}
			fmt.Println()
		} else {
			return fmt.Errorf("sync canceled: folder delimiters do not match server configuration")
		}
	}

	// Expand mappings to include subfolders
	if !quiet && verbose {
		fmt.Println("Checking for subfolders...")
	}
	expandedMappings, err := expandMappingsWithSubfolders(srcClient, mappings, srcDelimiter, dstDelimiter, verbose, quiet)
	if err != nil {
		return fmt.Errorf("failed to expand mappings: %w", err)
	}
	if len(expandedMappings) > len(mappings) && !quiet && verbose {
		fmt.Printf("📂 Found %d subfolders, total folders to sync: %d\n", len(expandedMappings)-len(mappings), len(expandedMappings))
	}
	mappings = expandedMappings

	// Setup progress writer for scanning phase
	pw := progress.NewWriter(2, quiet)
	pw.Start()

	// Create trackers for source and destination scanning
	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Dst.Label), 100)

	pw.AppendTracker(srcTracker)
	pw.AppendTracker(dstTracker)

	srcClient.SetProgressWriter(pw)
	srcClient.SetProgressTracker(srcTracker)
	dstClient.SetProgressWriter(pw)
	dstClient.SetProgressTracker(dstTracker)

	summary, err := buildSyncPlan(srcClient, dstClient, mappings, srcTracker, dstTracker, pw, cfg.Src.Label, cfg.Dst.Label, verbose)
	if err != nil {
		pw.Stop()
		return err
	}

	// Mark scanning as complete
	srcTracker.MarkAsDone()
	dstTracker.MarkAsDone()

	// Stop and clear progress
	pw.StopAndClear(2)

	if summary.TotalNew > 0 {
		if !quiet {
			fmt.Printf("📤 Messages to be copied to destination:\n")
			foldersToCreate := make([]string, 0, len(summary.Plans))
			for _, plan := range summary.Plans {
				foldersToCreate = append(foldersToCreate, plan.DestinationFolder)
				if len(plan.MessagesToSync) > 0 {
					fmt.Printf("• %s → %s will copy messages %d\n", plan.SourceFolder, plan.DestinationFolder, len(plan.MessagesToSync))
					if verbose {
						for _, msg := range plan.MessagesToSync {
							fmt.Printf("  • %s (ID: %s)\n", msg.Envelope.Subject, msg.Envelope.MessageId)
						}
						fmt.Println()
					}
				}
			}

			if len(foldersToCreate) > 0 {
				fmt.Printf("\n🗂️ Folders to be created on destination:\n")
				for _, folder := range foldersToCreate {
					fmt.Printf("• %s\n", folder)
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
		}
	} else {
		if !quiet {
			fmt.Println("✅ All folders already synced!")
		}
		return nil
	}

	// Collect active plans
	activePlans := make([]FolderSyncPlan, 0)
	for _, plan := range summary.Plans {
		if plan.NewMessages > 0 {
			activePlans = append(activePlans, plan)
		}
	}

	// Pre-create all destination folders
	if len(activePlans) > 0 {
		if !quiet {
			fmt.Println("\n📁 Creating destination folders...")
		}

		creationPW := progress.NewWriter(1, quiet)
		creationPW.Start()
		creationTracker := progress.NewTracker("Creating folders", int64(len(activePlans)))
		creationPW.AppendTracker(creationTracker)

		foldersToCreate := make(map[string]bool)
		for _, plan := range activePlans {
			if !plan.DestinationFolderExists {
				foldersToCreate[plan.DestinationFolder] = true
			}
		}

		creationTracker.UpdateTotal(int64(len(foldersToCreate)))
		createdCount := 0
		failedCount := 0

		for folder := range foldersToCreate {
			creationTracker.UpdateMessage(fmt.Sprintf("(%d/%d) Creating %s", createdCount+failedCount+1, len(foldersToCreate), folder))

			created, err := dstClient.CreateMailbox(folder)
			if err != nil {
				errMsg := err.Error()
				if strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "Mailbox exists") {
					if verbose {
						creationPW.Log("Folder %s already exists", folder)
					}
					createdCount++
				} else {
					creationPW.Log("Failed to create folder %q: %v", folder, err)
					failedCount++
				}
			} else if created {
				if verbose {
					creationPW.Log("Created folder %q", folder)
				}
				createdCount++
			} else {
				createdCount++
			}

			creationTracker.Increment(1)
		}

		// Update final message
		creationTracker.UpdateMessage(fmt.Sprintf("Created %d folders", createdCount))
		creationTracker.MarkAsDone()
		time.Sleep(100 * time.Millisecond)
		creationPW.Stop()

		if failedCount > 0 {
			return fmt.Errorf("failed to create %d folders", failedCount)
		}
	}

	totalSynced := 0
	totalErrors := 0

	if !quiet {
		fmt.Println("\n📥 Syncing messages...")
	}
	// Process plans in chunks based on worker count
	chunkSize := cfg.Workers
	for chunkStart := 0; chunkStart < len(activePlans); chunkStart += chunkSize {
		chunkEnd := min(chunkStart+chunkSize, len(activePlans))
		chunk := activePlans[chunkStart:chunkEnd]

		// Setup progress writer for this chunk
		syncPW := progress.NewWriter(len(chunk), quiet)
		syncPW.Start()

		// Create trackers for this chunk
		trackers := make([]*progress.Tracker, len(chunk))
		for i := range chunk {
			trackers[i] = progress.NewTracker(
				fmt.Sprintf("%d/%d Waiting...", chunkStart+i+1, len(activePlans)),
				100,
			)
			syncPW.AppendTracker(trackers[i])
		}

		// Process chunk in parallel
		var chunkWg sync.WaitGroup
		var chunkSyncedAtomic int64
		var chunkErrorsAtomic int64

		for i, plan := range chunk {
			chunkWg.Add(1)
			go func(idx int, planIndex int, p FolderSyncPlan, tracker *progress.Tracker) {
				defer chunkWg.Done()

				tracker.UpdateMessage(fmt.Sprintf("%d/%d Connecting...", planIndex, len(activePlans)))

				// Create a dedicated client for this goroutine
				folderClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
				if err != nil {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Failed to connect", planIndex, len(activePlans)))
					tracker.MarkAsErrored()
					atomic.AddInt64(&chunkErrorsAtomic, 1)
					syncPW.Log("Failed to connect for folder %s: %v", p.DestinationFolder, err)
					return
				}
				defer func() {
					_ = folderClient.Logout()
				}()
				folderClient.SetPrefix(fmt.Sprintf("%s-folder-%d", cfg.Dst.Label, planIndex))

				// Folders are already created in the pre-creation phase
				tracker.UpdateTotal(int64(p.NewMessages))
				tracker.UpdateMessage(fmt.Sprintf("%d/%d %s → %s", planIndex, len(activePlans), p.SourceFolder, p.DestinationFolder))
				synced, errors := syncFolders(cfg, p.SourceFolder, p.DestinationFolder, p.MessagesToSync, folderClient, verbose, syncPW, tracker, planIndex, len(activePlans), p.NewMessages)
				atomic.AddInt64(&chunkSyncedAtomic, int64(synced))
				atomic.AddInt64(&chunkErrorsAtomic, int64(errors))

				if errors > 0 {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d (errors: %d) %s → %s", planIndex, len(activePlans), synced, errors, p.SourceFolder, p.DestinationFolder))
					tracker.MarkAsErrored()
				} else {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d %s → %s", planIndex, len(activePlans), synced, p.SourceFolder, p.DestinationFolder))
					tracker.MarkAsDone()
				}
			}(i, chunkStart+i+1, plan, trackers[i])
		}

		// Wait for chunk to complete
		chunkWg.Wait()

		// Update totals
		totalSynced += int(atomic.LoadInt64(&chunkSyncedAtomic))
		totalErrors += int(atomic.LoadInt64(&chunkErrorsAtomic))

		// Give trackers time to update final state
		time.Sleep(100 * time.Millisecond)

		// Stop progress for this chunk
		syncPW.Stop()
	}

	if totalErrors > 0 {
		fmt.Printf("❌ Sync completed with errors. %d messages uploaded, %d errors occurred\n", totalSynced, totalErrors)
		return fmt.Errorf("sync completed with %d errors", totalErrors)
	}

	fmt.Println("✨ Sync completed successfully. ✨")
	return nil
}

// syncFolders syncs messages using a single client (already connected and with folder created).
func syncFolders(cfg *config.Config, srcFolder, dstFolder string, messages []*imap.Message, dstClient *client.Client, verbose bool, pw *progress.Writer, tracker *progress.Tracker, planIndex, totalPlans, totalMessages int) (int, int) {
	var syncedCount int64
	var errorCount int64

	for i, msg := range messages {
		if err := dstClient.AppendMessage(dstFolder, msg); err != nil {
			pw.Log("Failed to append message %d/%d: %v", i+1, len(messages), err)
			atomic.AddInt64(&errorCount, 1)
		} else {
			synced := atomic.AddInt64(&syncedCount, 1)
			tracker.Increment(1)
			tracker.UpdateMessage(fmt.Sprintf("%d/%d (%d/%d) %s → %s", planIndex, totalPlans, synced, totalMessages, srcFolder, dstFolder))
			if verbose {
				pw.Log("Synced %d/%d messages to %s, processed msg id %s", synced, len(messages), dstFolder, msg.Envelope.MessageId)
			}
		}
	}

	return int(syncedCount), int(errorCount)
}

// buildSyncPlan compares source and destination folders to determine what needs syncing.
func buildSyncPlan(srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, srcTracker, dstTracker *progress.Tracker, pw *progress.Writer, srcLabel, dstLabel string, verbose bool) (*SyncSummary, error) {
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
		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", srcLabel, srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", dstLabel, dstFolder, idx+1, len(mappings)))

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
			if verbose {
				pw.Log("⚠️ Failed to fetch source folder %s, skipping by error: %v", srcFolder, srcErr)
			}
			continue
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
		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Fetching from %s (%d/%d)", srcLabel, srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Waiting for %s (%d/%d)", dstLabel, srcFolder, idx+1, len(mappings)))
		messagesToSync, err := srcClient.FetchMessagesByIDs(srcFolder, newIDs, srcTracker, len(newIDs))
		if err != nil {
			return nil, fmt.Errorf("failed to fetch messages from %s: %w", srcFolder, err)
		}
		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", srcLabel, srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", dstLabel, dstFolder, idx+1, len(mappings)))
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

// validateFolderPath checks if folder path uses the correct delimiter for the server
func validateFolderPath(folderPath, serverDelimiter string) bool {
	if serverDelimiter == "" {
		return true
	}

	// Check if path contains any common delimiters
	commonDelimiters := []string{"/", ".", "\\"}
	for _, delim := range commonDelimiters {
		if delim != serverDelimiter && strings.Contains(folderPath, delim) {
			return false
		}
	}

	return true
}

// detectDelimiter tries to detect which delimiter is used in the folder path
func detectDelimiter(folderPath string) string {
	commonDelimiters := []string{"/", ".", "\\"}
	for _, delim := range commonDelimiters {
		if strings.Contains(folderPath, delim) {
			return delim
		}
	}
	return "none"
}

// expandMappingsWithSubfolders expands each mapping to include all subfolders
func expandMappingsWithSubfolders(srcClient *client.Client, mappings []config.DirectoryMapping, srcDelimiter, dstDelimiter string, verbose, quiet bool) ([]config.DirectoryMapping, error) {
	expanded := make([]config.DirectoryMapping, 0, len(mappings))

	for _, mapping := range mappings {
		// Add the original mapping
		expanded = append(expanded, mapping)

		// Get subfolders for this source folder
		subfolders, err := getSubfolders(srcClient, mapping.Source, srcDelimiter)
		if err != nil {
			if verbose && !quiet {
				fmt.Printf("  ⚠️  Failed to get subfolders for %s: %v\n", mapping.Source, err)
			}
			continue
		}

		// Add mapping for each subfolder
		for _, subfolder := range subfolders {
			// Calculate relative path from source root
			var relativePath string
			if srcDelimiter != "" {
				relativePath = strings.TrimPrefix(subfolder, mapping.Source+srcDelimiter)
			} else {
				relativePath = strings.TrimPrefix(subfolder, mapping.Source)
			}

			// Build destination path
			var dstPath string
			if dstDelimiter != "" && relativePath != "" {
				// Convert delimiter if needed
				if srcDelimiter != "" && srcDelimiter != dstDelimiter {
					relativePath = strings.ReplaceAll(relativePath, srcDelimiter, dstDelimiter)
				}
				dstPath = mapping.Destination + dstDelimiter + relativePath
			} else {
				dstPath = mapping.Destination
			}

			expanded = append(expanded, config.DirectoryMapping{
				Source:      subfolder,
				Destination: dstPath,
			})

			if verbose && !quiet {
				fmt.Printf("  📁 Found subfolder: %s → %s\n", subfolder, dstPath)
			}
		}
	}

	return expanded, nil
}

// getSubfolders returns all subfolders of a given folder
func getSubfolders(c *client.Client, folder string, delimiter string) ([]string, error) {
	pattern := folder
	if delimiter != "" {
		pattern = folder + delimiter + "*"
	} else {
		pattern = folder + "/*"
	}

	mailboxes := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.List("", pattern, mailboxes)
	}()

	var subfolders []string
	for mbox := range mailboxes {
		// Skip the folder itself
		if mbox.Name != folder {
			subfolders = append(subfolders, mbox.Name)
		}
	}

	if err := <-done; err != nil {
		return nil, err
	}

	return subfolders, nil
}
