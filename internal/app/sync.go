package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
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

	// TLS handshake to a remote IMAP server is the dominant cost of startup;
	// running src+dst in parallel halves time-to-first-fetch when both sides
	// are slow (e.g. residential mail providers + commercial IMAP).
	var srcClient, dstClient *client.Client
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		c, err := client.New(gCtx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
		if err != nil {
			return fmt.Errorf("source connection failed: %w", err)
		}
		c.SetPrefix(cfg.Src.Label)
		srcClient = c
		return nil
	})
	g.Go(func() error {
		c, err := client.New(gCtx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
		if err != nil {
			return fmt.Errorf("destination connection failed: %w", err)
		}
		c.SetPrefix(cfg.Dst.Label)
		dstClient = c
		return nil
	})
	groupErr := g.Wait()
	defer func() {
		if srcClient != nil {
			_ = srcClient.Logout()
		}
		if dstClient != nil {
			_ = dstClient.Logout()
		}
	}()
	if groupErr != nil {
		return groupErr
	}
	if err := ctx.Err(); err != nil {
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
		if srcDelimiter != "" {
			if oldDelim, ok := folderDelimiter(mapping.Source, srcDelimiter); !ok {
				validationErrors = append(validationErrors,
					fmt.Sprintf("Mapping %d: Source folder %q uses delimiter %q, server expects %q",
						i+1, mapping.Source, oldDelim, srcDelimiter))
				needsFix = true
			}
		}

		// Check destination folder compatibility
		if dstDelimiter != "" {
			if oldDelim, ok := folderDelimiter(mapping.Destination, dstDelimiter); !ok {
				validationErrors = append(validationErrors,
					fmt.Sprintf("Mapping %d: Destination folder %q uses delimiter %q, server expects %q",
						i+1, mapping.Destination, oldDelim, dstDelimiter))
				needsFix = true
			}
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
			for i := range mappings {
				if srcDelimiter != "" {
					if oldDelim, _ := folderDelimiter(mappings[i].Source, srcDelimiter); oldDelim != "none" && oldDelim != srcDelimiter {
						oldPath := mappings[i].Source
						mappings[i].Source = strings.ReplaceAll(mappings[i].Source, oldDelim, srcDelimiter)
						fmt.Printf("  ✓ Fixed source: %q → %q\n", oldPath, mappings[i].Source)
					}
				}
				if dstDelimiter != "" {
					if oldDelim, _ := folderDelimiter(mappings[i].Destination, dstDelimiter); oldDelim != "none" && oldDelim != dstDelimiter {
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
				// Preview only the folders we will actually create — the
				// real creation loop below filters the same way, so showing
				// already-existing folders here just misleads the user.
				if !plan.DestinationFolderExists {
					foldersToCreate = append(foldersToCreate, plan.DestinationFolder)
				}
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

	// Pre-creation MUST stay a pre-stage: each worker holds its own mailbox
	// cache, so a worker-side CreateMailbox would race against other workers'
	// stale caches.
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
				creationPW.Log("Failed to create folder %q: %v", folder, err)
				failedCount++
			case created:
				if verbose {
					creationPW.Log("Created folder %q", folder)
				}
				createdCount++
			default:
				if verbose {
					creationPW.Log("Folder %s already exists", folder)
				}
				createdCount++
			}

			creationTracker.Increment(1)
		}

		creationTracker.UpdateMessage(fmt.Sprintf("Created %d folders", createdCount))
		creationTracker.MarkAsDone()
		creationPW.Stop()

		if failedCount > 0 {
			return fmt.Errorf("failed to create %d folders", failedCount)
		}
	}

	if !quiet {
		fmt.Println("\n📥 Syncing messages...")
	}

	effectiveWorkers := computeEffectiveWorkers(cfg.Workers, cfg.RateLimit.MaxConnections, len(activePlans))

	workers, err := newSyncWorkerPool(ctx, cfg, srcOpts, dstOpts, effectiveWorkers)
	if err != nil {
		return err
	}
	defer workers.close()

	// One progress writer for the whole sync, with a tracker per plan
	// up front. Reusing the writer across all plans replaces the older
	// "writer-per-chunk" approach that produced visible flicker and forced
	// a 100ms sleep between chunks.
	syncPW := progress.NewWriter(len(activePlans), quiet)
	syncPW.Start()
	defer syncPW.Stop()

	trackers := make([]*progress.Tracker, len(activePlans))
	for i := range activePlans {
		trackers[i] = progress.NewTracker(
			fmt.Sprintf("%d/%d Waiting...", i+1, len(activePlans)),
			100,
		)
		syncPW.AppendTracker(trackers[i])
	}

	// free is a bounded semaphore of pre-built workers. Whoever runs first
	// pulls a worker, syncs one plan, returns the worker.
	free := make(chan *syncWorker, effectiveWorkers)
	for _, w := range workers.all {
		free <- w
	}

	var (
		wg          sync.WaitGroup
		totalSynced atomic.Int64
		totalErrors atomic.Int64
	)

	for i, plan := range activePlans {
		if ctx.Err() != nil {
			break
		}
		var w *syncWorker
		select {
		case w = <-free:
		case <-ctx.Done():
		}
		if w == nil {
			break
		}
		wg.Add(1)
		go func(idx int, p FolderSyncPlan, w *syncWorker, tr *progress.Tracker) {
			defer wg.Done()
			defer func() { free <- w }()
			synced, errs := runFolderSync(ctx, w, p, tr, idx, len(activePlans), syncPW, verbose)
			totalSynced.Add(int64(synced))
			totalErrors.Add(int64(errs))
		}(i, plan, w, trackers[i])
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}

	totalSyncedN := int(totalSynced.Load())
	totalErrorsN := int(totalErrors.Load())

	if totalErrorsN > 0 {
		fmt.Printf("❌ Sync completed with errors. %d messages uploaded, %d errors occurred\n", totalSyncedN, totalErrorsN)
		return fmt.Errorf("sync completed with %d errors", totalErrorsN)
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

// folderDelimiter inspects path and reports which of the common IMAP
// hierarchy delimiters it appears to use, plus whether that delimiter agrees
// with the server's. ok is true also when path contains no delimiter at all
// (there is nothing to mismatch).
//
// "none" is returned for the detected value when the path is flat — callers
// rely on that string to skip rewrite logic in the fix-up loop.
func folderDelimiter(path, serverDelimiter string) (detected string, ok bool) {
	for _, d := range [...]string{"/", ".", "\\"} {
		if strings.Contains(path, d) {
			return d, serverDelimiter == "" || d == serverDelimiter
		}
	}
	return "none", true
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

	// A parent mapping's subfolder expansion can collide with an explicit
	// mapping for that same subfolder; without dedup the plan ends up with
	// duplicate entries that scan and copy the folder twice.
	out, dropped := dedupeMappings(expanded)
	if verbose && !quiet {
		for _, m := range dropped {
			fmt.Printf("  ℹ️  Mapping %q skipped — already covered by a parent mapping's expansion\n", m.Source)
		}
	}
	return out, nil
}

// computeEffectiveWorkers caps the worker count by both the configured cap
// and the number of plans actually scheduled. maxConn is the per-side IMAP
// connection budget; the planning client is still open when workers start,
// so we reserve one slot for it (maxConn-1). Returns at least 1.
func computeEffectiveWorkers(workers, maxConn, planCount int) int {
	eff := workers
	if maxConn > 0 && maxConn-1 < eff {
		eff = maxConn - 1
	}
	if eff > planCount {
		eff = planCount
	}
	if eff < 1 {
		eff = 1
	}
	return eff
}

// dedupeMappings keeps the first occurrence of each Source. The second return
// value lists every dropped entry so callers can surface them under verbose
// without coupling the dedup to a logger.
func dedupeMappings(in []config.DirectoryMapping) (out, dropped []config.DirectoryMapping) {
	out = make([]config.DirectoryMapping, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, m := range in {
		if _, ok := seen[m.Source]; ok {
			dropped = append(dropped, m)
			continue
		}
		seen[m.Source] = struct{}{}
		out = append(out, m)
	}
	return out, dropped
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
