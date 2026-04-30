package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v3"
	"golang.org/x/time/rate"
)

// FolderSyncPlan describes how a single source folder should be copied to its
// destination.
//
// SrcUIDs holds the source UIDs (sorted) for messages present on src but
// missing on dst; bodies are deliberately not fetched at planning time so a
// confirm prompt can show counts without materializing potentially many GB
// of mail in memory.
type FolderSyncPlan struct {
	SourceFolder            string
	DestinationFolder       string
	SrcUIDs                 []uint32
	NewMessages             int
	DestinationFolderExists bool
}

// SyncSummary aggregates the per-folder plans along with total message counts.
type SyncSummary struct {
	Plans    []FolderSyncPlan
	TotalNew int
}

// ActionSync copies messages between IMAP servers according to the provided configuration.
func ActionSync(ctx context.Context, c *cli.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	srcFolder := c.String("src-folder")
	dstFolder := c.String("dest-folder")
	quiet := c.Bool("quiet")
	verbose := c.Bool("verbose")
	autoConfirm := c.Bool("confirm")
	if !quiet && verbose {
		fmt.Println("Fetching config...")
	}
	cfg, err := config.New(c)
	if err != nil {
		fmt.Printf("Config error: %v\n", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if !quiet && verbose {
		fmt.Printf("Starting sync with %d workers\n", cfg.Workers)
	}

	// Rate-limit budgets are shared across every Client that talks to the
	// same side: src.ReadLimiter governs all download traffic, dst.WriteLimiter
	// all upload traffic. Either may be nil ("unlimited").
	srcReadLim := ratelimit.NewLimiter(cfg.RateLimit.DownBPS)
	dstWriteLim := ratelimit.NewLimiter(cfg.RateLimit.UpBPS)
	srcOpts := client.Options{UseTLS: true, Verbose: verbose, ReadLimiter: srcReadLim}
	dstOpts := client.Options{UseTLS: true, Verbose: verbose, WriteLimiter: dstWriteLim}

	if !quiet {
		if w := buildProviderWarning(cfg, srcReadLim, dstWriteLim); w != "" {
			fmt.Print(w)
		}
	}

	var mappings []config.DirectoryMapping

	switch {
	case srcFolder != "" && dstFolder != "":
		mappings = []config.DirectoryMapping{
			{Source: srcFolder, Destination: dstFolder},
		}
	case srcFolder != "" || dstFolder != "":
		fmt.Println("both --src-folder and --dest-folder must be specified")
		return errors.New("both --src-folder and --dest-folder must be specified")
	default:
		if len(cfg.Map) == 0 {
			return errors.New("no folder mappings in config")
		}
		mappings = cfg.Map
	}

	if !quiet && verbose {
		fmt.Println("Connecting to servers...")
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	srcClient, err := client.New(ctx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
	if err != nil {
		return fmt.Errorf("source connection failed: %w", err)
	}
	defer func() {
		_ = srcClient.Logout()
	}()
	srcClient.SetPrefix(cfg.Src.Label)

	if err := ctx.Err(); err != nil {
		srcClient.Cancel()
		return err
	}
	dstClient, err := client.New(ctx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
	if err != nil {
		return fmt.Errorf("destination connection failed: %w", err)
	}
	defer func() {
		_ = dstClient.Logout()
	}()
	dstClient.SetPrefix(cfg.Dst.Label)
	if err := ctx.Err(); err != nil {
		dstClient.Cancel()
		return err
	}

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
		if err := ctx.Err(); err != nil {
			return err
		}
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
			if err := ctx.Err(); err != nil {
				return err
			}
			confirmed, err := utils.AskConfirm(ctx, "🔧 Fix folder delimiters to match server configuration?")
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
		} else {
			fmt.Println()
			fmt.Println("⚠️ Please note if delimiter is not corresponding to the server configuration, the folder structure may not be correctly interpreted.")
		}
		fmt.Println()
	}

	// Expand mappings to include subfolders
	if !quiet && verbose {
		fmt.Println("Checking for subfolders...")
	}
	expandedMappings, err := expandMappingsWithSubfolders(ctx, srcClient, mappings, srcDelimiter, dstDelimiter, verbose, quiet)
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

	summary, err := buildSyncPlan(ctx, srcClient, dstClient, mappings, srcTracker, dstTracker, pw, cfg.Src.Label, cfg.Dst.Label, verbose)
	if err != nil {
		pw.Stop()
		return err
	}

	// Mark scanning as complete
	srcTracker.MarkAsDone()
	dstTracker.MarkAsDone()

	// Stop and clear progress
	pw.StopAndClear(2)
	if err := ctx.Err(); err != nil {
		return err
	}

	if summary.TotalNew > 0 {
		if !quiet {
			fmt.Printf("📤 Messages to be copied to destination:\n")
			foldersToCreate := make([]string, 0, len(summary.Plans))
			for _, plan := range summary.Plans {
				foldersToCreate = append(foldersToCreate, plan.DestinationFolder)
				if plan.NewMessages > 0 {
					fmt.Printf("• %s → %s will copy messages %d\n", plan.SourceFolder, plan.DestinationFolder, plan.NewMessages)
					if verbose {
						for _, uid := range plan.SrcUIDs {
							fmt.Printf("  • UID %d\n", uid)
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
				if err := ctx.Err(); err != nil {
					return err
				}
				confirmed, err := utils.AskConfirm(ctx, "✍️ Proceed with synchronization?")
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
			if err := ctx.Err(); err != nil {
				creationPW.Stop()
				return err
			}
			creationTracker.UpdateMessage(fmt.Sprintf("(%d/%d) Creating %s", createdCount+failedCount+1, len(foldersToCreate), folder))

			created, err := dstClient.CreateMailbox(ctx, folder)
			switch {
			case err != nil && ctx.Err() != nil:
				creationPW.Stop()
				return ctx.Err()
			case err != nil:
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
			case created:
				if verbose {
					creationPW.Log("Created folder %q", folder)
				}
				createdCount++
			default:
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
		var chunkSynced, chunkErrors atomic.Int64

		for i, plan := range chunk {
			chunkWg.Add(1)
			go func(planIndex int, p FolderSyncPlan, tracker *progress.Tracker) {
				defer chunkWg.Done()

				if err := ctx.Err(); err != nil {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Canceled", planIndex, len(activePlans)))
					tracker.MarkAsErrored()
					return
				}

				tracker.UpdateMessage(fmt.Sprintf("%d/%d Connecting...", planIndex, len(activePlans)))

				// Each worker owns its own src+dst connections. go-imap clients
				// are not safe for concurrent use across folders, so sharing the
				// outer srcClient/dstClient between workers is not an option.
				folderSrcClient, err := client.New(ctx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
				if err != nil {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Failed to connect (src)", planIndex, len(activePlans)))
					tracker.MarkAsErrored()
					chunkErrors.Add(1)
					syncPW.Log("Failed to connect (src) for folder %s: %v", p.SourceFolder, err)
					return
				}
				defer func() { _ = folderSrcClient.Logout() }()
				folderSrcClient.SetPrefix(fmt.Sprintf("%s-folder-%d", cfg.Src.Label, planIndex))

				folderDstClient, err := client.New(ctx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
				if err != nil {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Failed to connect (dst)", planIndex, len(activePlans)))
					tracker.MarkAsErrored()
					chunkErrors.Add(1)
					syncPW.Log("Failed to connect (dst) for folder %s: %v", p.DestinationFolder, err)
					return
				}
				defer func() { _ = folderDstClient.Logout() }()
				folderDstClient.SetPrefix(fmt.Sprintf("%s-folder-%d", cfg.Dst.Label, planIndex))

				tracker.UpdateTotal(int64(p.NewMessages))
				tracker.UpdateMessage(fmt.Sprintf("%d/%d %s → %s", planIndex, len(activePlans), p.SourceFolder, p.DestinationFolder))

				var synced, errors int
				// Throttle UpdateMessage so a 100k-message sync doesn't spend
				// measurable CPU on fmt.Sprintf and tracker churn. The
				// progress writer renders at ~10 Hz anyway; updating faster
				// just produces work the renderer drops.
				var lastUpdate time.Time
				streamErr := folderSrcClient.StreamMessagesByUIDs(ctx, p.SourceFolder, p.SrcUIDs, func(msg *imap.Message) error {
					if err := folderDstClient.AppendMessage(ctx, p.DestinationFolder, msg); err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						errors++
						syncPW.Log("Failed to append message to %s: %v", p.DestinationFolder, err)
						return nil
					}
					synced++
					tracker.Increment(1)
					if now := time.Now(); now.Sub(lastUpdate) > 100*time.Millisecond {
						lastUpdate = now
						tracker.UpdateMessage(fmt.Sprintf("%d/%d (%d/%d) %s → %s", planIndex, len(activePlans), synced, p.NewMessages, p.SourceFolder, p.DestinationFolder))
					}
					if verbose {
						syncPW.Log("Synced %d/%d to %s, processed msg id %s", synced, p.NewMessages, p.DestinationFolder, msg.Envelope.MessageId)
					}
					return nil
				})
				if ctx.Err() != nil {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Canceled %s → %s", planIndex, len(activePlans), p.SourceFolder, p.DestinationFolder))
					tracker.MarkAsErrored()
					return
				}
				if streamErr != nil {
					syncPW.Log("Stream error for folder %s: %v", p.SourceFolder, streamErr)
					errors++
				}

				chunkSynced.Add(int64(synced))
				chunkErrors.Add(int64(errors))

				if errors > 0 {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d (errors: %d) %s → %s", planIndex, len(activePlans), synced, errors, p.SourceFolder, p.DestinationFolder))
					tracker.MarkAsErrored()
				} else {
					tracker.UpdateMessage(fmt.Sprintf("%d/%d Synced messages %d %s → %s", planIndex, len(activePlans), synced, p.SourceFolder, p.DestinationFolder))
					tracker.MarkAsDone()
				}
			}(chunkStart+i+1, plan, trackers[i])
		}

		// Wait for chunk to complete
		chunkWg.Wait()

		if err := ctx.Err(); err != nil {
			syncPW.Stop()
			return err
		}

		// Update totals
		totalSynced += int(chunkSynced.Load())
		totalErrors += int(chunkErrors.Load())

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

// buildSyncPlan compares source and destination folders to determine what needs syncing.
func buildSyncPlan(ctx context.Context, srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, srcTracker, dstTracker *progress.Tracker, pw *progress.Writer, srcLabel, dstLabel string, verbose bool) (*SyncSummary, error) {
	summary := &SyncSummary{
		Plans: make([]FolderSyncPlan, 0),
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Set tracker totals based on number of folders to scan
	srcTracker.UpdateTotal(int64(len(mappings)))
	dstTracker.UpdateTotal(int64(len(mappings)))

	for idx, mapping := range mappings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		srcFolder := mapping.Source
		dstFolder := mapping.Destination
		var (
			dstFolderExists bool
			srcMessageMap   map[string]uint32
			dstMessageIDs   map[string]struct{}
			srcErr, dstErr  error
		)

		// Fetch IDs from both servers in parallel — both calls are
		// envelope-only (BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID), so
		// they're cheap relative to body fetches.
		var wg sync.WaitGroup

		// Update trackers
		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", srcLabel, srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", dstLabel, dstFolder, idx+1, len(mappings)))

		// Fetch source Message-Id → UID. We need the UIDs later to drive
		// the body fetch in StreamMessagesByUIDs without re-scanning the
		// folder.
		wg.Go(func() {
			srcMessageMap, srcErr = srcClient.FetchMessageMap(ctx, srcFolder)
		})

		// Probe destination existence first; only fetch IDs if it exists.
		// A "missing folder" is benign (we'll create it); a transport error
		// must be fatal — otherwise we'd mistake it for empty and re-upload
		// everything.
		wg.Go(func() {
			exists, err := dstClient.MailboxExists(ctx, dstFolder)
			if err != nil {
				dstErr = err
				return
			}
			if !exists {
				return
			}
			dstFolderExists = true
			fetchedIDs, err := dstClient.FetchMessageIDSet(ctx, dstFolder)
			if err != nil {
				dstErr = err
				return
			}
			dstMessageIDs = fetchedIDs
		})

		wg.Wait()
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if srcErr != nil {
			if verbose {
				pw.Log("⚠️ Failed to fetch source folder %s, skipping by error: %v", srcFolder, srcErr)
			}
			continue
		}
		if dstErr != nil {
			return nil, fmt.Errorf("scan destination folder %q: %w", dstFolder, dstErr)
		}

		// Diff: collect source UIDs whose Message-Id is not on dst.
		newUIDs := make([]uint32, 0)
		for id, uid := range srcMessageMap {
			if _, present := dstMessageIDs[id]; !present {
				newUIDs = append(newUIDs, uid)
			}
		}

		if len(newUIDs) == 0 {
			srcTracker.Increment(1)
			dstTracker.Increment(1)
			continue
		}
		// Sort here so the planning preview prints in a stable order and
		// the downstream UID FETCH benefits from compactable ranges.
		slices.Sort(newUIDs)

		srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", srcLabel, srcFolder, idx+1, len(mappings)))
		dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", dstLabel, dstFolder, idx+1, len(mappings)))
		srcTracker.Increment(1)
		dstTracker.Increment(1)

		summary.Plans = append(summary.Plans, FolderSyncPlan{
			SourceFolder:            srcFolder,
			DestinationFolder:       dstFolder,
			DestinationFolderExists: dstFolderExists,
			NewMessages:             len(newUIDs),
			SrcUIDs:                 newUIDs,
		})
		summary.TotalNew += len(newUIDs)
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
func expandMappingsWithSubfolders(ctx context.Context, srcClient *client.Client, mappings []config.DirectoryMapping, srcDelimiter, dstDelimiter string, verbose, quiet bool) ([]config.DirectoryMapping, error) {
	expanded := make([]config.DirectoryMapping, 0, len(mappings))
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, mapping := range mappings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Add the original mapping
		expanded = append(expanded, mapping)

		// Get subfolders for this source folder
		subfolders, err := getSubfolders(ctx, srcClient, mapping.Source, srcDelimiter)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if verbose && !quiet {
				fmt.Printf("  ⚠️  Failed to get subfolders for %s: %v\n", mapping.Source, err)
			}
			continue
		}

		// Add mapping for each subfolder
		for _, subfolder := range subfolders {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
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
func getSubfolders(ctx context.Context, c *client.Client, folder string, delimiter string) ([]string, error) {
	return c.ListSubfolders(ctx, folder, delimiter)
}

// buildProviderWarning produces a human-readable banner for accounts on
// providers with known IMAP limits. Empty string when neither side is a known
// provider, so the caller can print unconditionally.
//
// The banner is informational, not blocking — it sits before the confirm
// prompt in the sync flow so the user has a chance to abort and re-run with
// safer flags before kicking off a transfer that might hit a daily quota.
func buildProviderWarning(cfg *config.Config, srcReadLim, dstWriteLim *rate.Limiter) string {
	type sideHit struct {
		limiter  *rate.Limiter
		side     string // "source" / "destination"
		host     string
		provider client.Provider
		isUpload bool
	}

	var hits []sideHit
	if p, ok := client.DetectProvider(cfg.Src.Server); ok {
		hits = append(hits, sideHit{
			limiter: srcReadLim, side: "source", host: cfg.Src.Server, provider: p, isUpload: false,
		})
	}
	if p, ok := client.DetectProvider(cfg.Dst.Server); ok {
		hits = append(hits, sideHit{
			limiter: dstWriteLim, side: "destination", host: cfg.Dst.Server, provider: p, isUpload: true,
		})
	}
	if len(hits) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n⚠️  Provider with known IMAP limits detected:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "   %s [%s] — %s\n", h.side, h.host, h.provider.Name)
		if h.provider.MaxConnections > 0 {
			fmt.Fprintf(&b, "     • max simultaneous connections: %d\n", h.provider.MaxConnections)
			if cfg.Workers >= h.provider.MaxConnections {
				fmt.Fprintf(&b, "       (current --workers=%d may exceed this; consider lowering)\n", cfg.Workers)
			}
		}
		if h.provider.DailyDownMB > 0 || h.provider.DailyUpMB > 0 {
			fmt.Fprintf(&b, "     • daily quota: %d MB down, %d MB up\n",
				h.provider.DailyDownMB, h.provider.DailyUpMB)
		}
		if h.limiter == nil {
			recommended := h.provider.DownBPS
			flag := "--bps-down"
			if h.isUpload {
				recommended = h.provider.UpBPS
				flag = "--bps-up"
			}
			if recommended > 0 {
				fmt.Fprintf(&b, "     • no rate limit set — recommended: %s %d\n", flag, recommended)
			}
		}
		if h.provider.Notes != "" {
			fmt.Fprintf(&b, "     • notes: %s\n", h.provider.Notes)
		}
	}
	b.WriteString("\n")
	return b.String()
}
