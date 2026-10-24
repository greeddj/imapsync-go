package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// verboseUIDLimit caps the per-plan UID dump in --verbose preview so a
// large plan (tens of thousands of UIDs) doesn't bury the confirm prompt.
const verboseUIDLimit = 20

// traceTracker writes a one-line diagnostic to stderr when IMAPSYNC_TRACE is
// set, to surface duplicate Tracker allocations for the same plan.
func traceTracker(site, message string) {
	if os.Getenv("IMAPSYNC_TRACE") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[trace] tracker created: site=%s message=%q\n", site, message)
}

// newProgressWriter builds a progress writer that renders to out unless quiet
// silences it.
func newProgressWriter(numTrackers int, quiet bool, out io.Writer) *progress.Writer {
	pw := progress.NewWriter(numTrackers, quiet)
	if !quiet {
		pw.SetOutputWriter(out)
	}
	return pw
}

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
	NewSize                 uint64
	DestinationFolderExists bool
}

// SyncSummary aggregates the per-folder plans along with total message counts
// and an estimated total byte volume.
//
// TotalNewSize is approximate: it scales the source folder's full RFC822.SIZE
// total by the new-to-total message ratio (NewMessages / SourceMessages). The
// exact size would require an extra UID FETCH per new UID, which is not worth
// the round-trips for what is only a preview number.
type SyncSummary struct {
	Plans        []FolderSyncPlan
	TotalNew     int
	TotalNewSize uint64
}

// syncOptions carries what runSync needs besides the config. clientOpts holds
// the shared transport settings; runSync adds the per-side fields.
type syncOptions struct {
	out         io.Writer
	in          io.Reader
	srcFolder   string
	dstFolder   string
	clientOpts  client.Options
	quiet       bool
	verbose     bool
	autoConfirm bool
}

// ActionSync copies messages between IMAP servers according to the provided configuration.
func ActionSync(ctx context.Context, c *cli.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := rejectPositionalArgs(c); err != nil {
		return err
	}

	opts := syncOptions{
		out:         os.Stdout,
		in:          os.Stdin,
		srcFolder:   c.String("src-folder"),
		dstFolder:   c.String("dest-folder"),
		clientOpts:  client.Options{UseTLS: true},
		quiet:       c.Bool("quiet"),
		verbose:     c.Bool("verbose"),
		autoConfirm: c.Bool("confirm"),
	}
	if !opts.quiet && opts.verbose {
		_, _ = fmt.Fprintln(opts.out, "Fetching config...")
	}
	cfg, err := config.New(c)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	return runSync(ctx, cfg, opts)
}

func runSync(ctx context.Context, cfg *config.Config, opts syncOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	quiet, verbose, autoConfirm := opts.quiet, opts.verbose, opts.autoConfirm
	out := opts.out
	// One reader for every prompt: a reader per prompt would buffer past the
	// first answer and lose the rest of piped input.
	in := bufio.NewReader(opts.in)

	if !quiet && verbose {
		_, _ = fmt.Fprintf(out, "Starting sync with %d workers\n", cfg.Workers)
	}

	// Rate-limit budgets are shared across every Client that talks to the
	// same side: src.ReadLimiter governs all download traffic, dst.WriteLimiter
	// all upload traffic. Either may be nil ("unlimited").
	srcReadLim := ratelimit.NewLimiter(cfg.RateLimit.DownBPS)
	dstWriteLim := ratelimit.NewLimiter(cfg.RateLimit.UpBPS)
	srcOpts := opts.clientOpts
	srcOpts.Auth = cfg.Src.Auth
	srcOpts.Verbose = verbose
	srcOpts.ReadLimiter = srcReadLim
	dstOpts := opts.clientOpts
	dstOpts.Auth = cfg.Dst.Auth
	dstOpts.Verbose = verbose
	dstOpts.WriteLimiter = dstWriteLim

	if !quiet {
		if w := buildProviderWarning(cfg, srcReadLim, dstWriteLim); w != "" {
			_, _ = fmt.Fprint(out, w)
		}
	}

	var mappings []config.DirectoryMapping

	switch {
	case opts.srcFolder != "" && opts.dstFolder != "":
		mappings = []config.DirectoryMapping{
			{Source: opts.srcFolder, Destination: opts.dstFolder},
		}
	case opts.srcFolder != "" || opts.dstFolder != "":
		return errors.New("both --src-folder and --dest-folder must be specified")
	default:
		if len(cfg.Map) > 0 {
			mappings = cfg.Map
		} else {
			// dynamically build the mappings from the source folders
			lister, err := client.New(ctx, cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, srcOpts)
			if err != nil {
				return fmt.Errorf("source connection failed: %w", err)
			}
			mailboxes, err := lister.ListMailboxes(ctx)
			if err != nil {
				return fmt.Errorf("source connection list mailbox failed: %w", err)
			}
			for _, mb := range mailboxes {
				mappings = append(mappings, config.DirectoryMapping{
					Source:      mb.Name,
					Destination: mb.Name,
				})
			}
		}
	}

	if !quiet && verbose {
		_, _ = fmt.Fprintln(out, "Connecting to servers...")
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
		_, _ = fmt.Fprintf(out, "📁 Server delimiters:\n")
		_, _ = fmt.Fprintf(out, "  Source [%s]: %q\n", cfg.Src.Label, srcDelimiter)
		_, _ = fmt.Fprintf(out, "  Destination [%s]: %q\n\n", cfg.Dst.Label, dstDelimiter)
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
		_, _ = fmt.Fprintf(out, "⚠️  Folder path delimiter mismatch detected:\n")
		for _, err := range validationErrors {
			_, _ = fmt.Fprintf(out, "  • %s\n", err)
		}
		_, _ = fmt.Fprintln(out)

		var shouldFix bool
		if autoConfirm {
			shouldFix = true
			_, _ = fmt.Fprintln(out, "✅ Auto-confirming delimiter fix...")
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			confirmed, err := utils.AskConfirm(ctx, in, out, "🔧 Fix folder delimiters to match server configuration?")
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
						_, _ = fmt.Fprintf(out, "  ✓ Fixed source: %q → %q\n", oldPath, mappings[i].Source)
					}
				}
				if dstDelimiter != "" {
					if oldDelim, _ := folderDelimiter(mappings[i].Destination, dstDelimiter); oldDelim != "none" && oldDelim != dstDelimiter {
						oldPath := mappings[i].Destination
						mappings[i].Destination = strings.ReplaceAll(mappings[i].Destination, oldDelim, dstDelimiter)
						_, _ = fmt.Fprintf(out, "  ✓ Fixed destination: %q → %q\n", oldPath, mappings[i].Destination)
					}
				}
			}
		} else {
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, "⚠️ Please note if delimiter is not corresponding to the server configuration, the folder structure may not be correctly interpreted.")
		}
		_, _ = fmt.Fprintln(out)
	}

	// Expand mappings to include subfolders
	if !quiet && verbose {
		_, _ = fmt.Fprintln(out, "Checking for subfolders...")
	}
	expandedMappings, err := expandMappingsWithSubfolders(ctx, out, srcClient, mappings, srcDelimiter, dstDelimiter, verbose, quiet)
	if err != nil {
		return fmt.Errorf("failed to expand mappings: %w", err)
	}
	if len(expandedMappings) > len(mappings) && !quiet && verbose {
		_, _ = fmt.Fprintf(out, "📂 Found %d subfolders, total folders to sync: %d\n", len(expandedMappings)-len(mappings), len(expandedMappings))
	}
	mappings = expandedMappings

	// Setup progress writer for scanning phase
	pw := newProgressWriter(2, quiet, out)
	pw.Start()

	// Create trackers for source and destination scanning
	srcTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Src.Label), 100)
	dstTracker := progress.NewTracker(fmt.Sprintf("[%s] Scanning folders", cfg.Dst.Label), 100)

	traceTracker("scan-src", srcTracker.Message)
	pw.AppendTracker(srcTracker)
	traceTracker("scan-dst", dstTracker.Message)
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
	pw.StopAndClear()
	if err := ctx.Err(); err != nil {
		return err
	}

	if summary.TotalNew > 0 {
		if !quiet {
			_, _ = fmt.Fprintf(out, "📤 Messages to be copied to destination:\n")
			foldersToCreate := make([]string, 0, len(summary.Plans))
			for _, plan := range summary.Plans {
				// Preview only the folders we will actually create — the
				// real creation loop below filters the same way, so showing
				// already-existing folders here just misleads the user.
				if !plan.DestinationFolderExists {
					foldersToCreate = append(foldersToCreate, plan.DestinationFolder)
				}
				if plan.NewMessages > 0 {
					_, _ = fmt.Fprintf(out, "• %s → %s will copy %d messages (≈ %s)\n",
						plan.SourceFolder, plan.DestinationFolder, plan.NewMessages, utils.FormatSize(plan.NewSize))
					if verbose {
						// Dumping every UID before the confirm-prompt
						// drowns the user in screens of integers — a
						// 10k-message plan turned the prompt invisible.
						// Show a sample and a count.
						n := len(plan.SrcUIDs)
						limit := min(n, verboseUIDLimit)
						for _, uid := range plan.SrcUIDs[:limit] {
							_, _ = fmt.Fprintf(out, "  • UID %d\n", uid)
						}
						if n > verboseUIDLimit {
							_, _ = fmt.Fprintf(out, "  • … and %d more\n", n-verboseUIDLimit)
						}
						_, _ = fmt.Fprintln(out)
					}
				}
			}

			if len(foldersToCreate) > 0 {
				_, _ = fmt.Fprintf(out, "\n🗂️ Folders to be created on destination:\n")
				for _, folder := range foldersToCreate {
					_, _ = fmt.Fprintf(out, "• %s\n", folder)
				}
			}
			_, _ = fmt.Fprintf(out, "\n📨 Total new messages to sync: %d (≈ %s)\n", summary.TotalNew, utils.FormatSize(summary.TotalNewSize))

			if !autoConfirm {
				if err := ctx.Err(); err != nil {
					return err
				}
				confirmed, err := utils.AskConfirm(ctx, in, out, "✍️ Proceed with synchronization?")
				if err != nil {
					return err
				}
				if !confirmed {
					_, _ = fmt.Fprintln(out, "❌ Sync canceled by user")
					return nil
				}
			}
		}
	} else {
		if !quiet {
			_, _ = fmt.Fprintln(out, "✅ All folders already synced!")
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
	foldersToCreate := make(map[string]bool)
	for _, plan := range activePlans {
		if !plan.DestinationFolderExists {
			foldersToCreate[plan.DestinationFolder] = true
		}
	}

	if len(foldersToCreate) > 0 {
		if !quiet {
			_, _ = fmt.Fprintln(out, "\n📁 Creating destination folders...")
		}

		creationPW := newProgressWriter(1, quiet, out)
		creationPW.Start()
		creationTracker := progress.NewTracker("Creating folders", int64(len(foldersToCreate)))
		traceTracker("folder-create", creationTracker.Message)
		creationPW.AppendTracker(creationTracker)

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
		creationPW.StopAndClear()

		if failedCount > 0 {
			return fmt.Errorf("failed to create %d folders", failedCount)
		}
	}

	if !quiet {
		_, _ = fmt.Fprintln(out, "\n📥 Syncing messages...")
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
	syncPW := newProgressWriter(len(activePlans), quiet, out)
	syncPW.Start()
	defer syncPW.Stop()

	trackers := make([]*progress.Tracker, len(activePlans))
	for i, plan := range activePlans {
		trackers[i] = progress.NewTracker(
			fmt.Sprintf("%d/%d Waiting: %s → %s", i+1, len(activePlans), plan.SourceFolder, plan.DestinationFolder),
			100,
		)
		traceTracker("plan", trackers[i].Message)
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
		_, _ = fmt.Fprintf(out, "❌ Sync completed with errors. %d messages uploaded, %d errors occurred\n", totalSyncedN, totalErrorsN)
		// Friendly summary already printed; signal non-zero exit without
		// asking main to repeat the same information through stderr.
		return ErrSilentExit
	}

	_, _ = fmt.Fprintln(out, "✨ Sync completed successfully. ✨")
	return nil
}

// folderScan holds the per-slot results from the parallel src and dst scans.
// done is incremented by each side; when it reaches 2, maybeDiff fires.
// Pointer fields precede non-pointer fields to minimise the GC scan range.
//
// srcFolderSize and srcFolderCount are recorded before srcMap is freed so the
// proportional size estimate in maybeDiff can run after the diff.
type folderScan struct {
	srcMap         map[string]uint32
	dstIDs         map[string]struct{}
	srcErr         error
	dstErr         error
	srcFolderSize  uint64
	srcFolderCount int
	dstExists      bool
	mu             sync.Mutex
	done           atomic.Int32
}

func buildSyncPlan(ctx context.Context, srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, srcTracker, dstTracker *progress.Tracker, pw *progress.Writer, srcLabel, dstLabel string, verbose bool) (*SyncSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	n := len(mappings)
	srcTracker.UpdateTotal(int64(n))
	dstTracker.UpdateTotal(int64(n))

	scans := make([]folderScan, n)
	plans := make([]FolderSyncPlan, n)
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64

	// errgroup.WithContext: if either goroutine returns a non-nil error,
	// gCtx is cancelled, which causes the other goroutine's next gCtx.Err()
	// check to bail out immediately — no need to drain all remaining folders.
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		for idx, m := range mappings {
			if err := gCtx.Err(); err != nil {
				return err
			}
			srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", srcLabel, m.Source, idx+1, n))
			mp, size, err := srcClient.FetchMessageMap(gCtx, m.Source)
			if err != nil {
				scans[idx].srcErr = err
			} else {
				scans[idx].srcMap = mp
				scans[idx].srcFolderSize = size
				scans[idx].srcFolderCount = len(mp)
			}
			srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", srcLabel, m.Source, idx+1, n))
			srcTracker.Increment(1)
			maybeDiff(&scans[idx], idx, mappings, plans, &totalNew, &totalNewSize)
		}
		return nil
	})

	g.Go(func() error {
		for idx, m := range mappings {
			if err := gCtx.Err(); err != nil {
				return err
			}
			dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", dstLabel, m.Destination, idx+1, n))
			exists, err := dstClient.MailboxExists(gCtx, m.Destination)
			switch {
			case err != nil:
				scans[idx].dstErr = err
			case !exists:
				// dstExists stays false, dstIDs stays nil — benign; folder will be created.
			default:
				scans[idx].dstExists = true
				ids, err := dstClient.FetchMessageIDSet(gCtx, m.Destination)
				if err != nil {
					scans[idx].dstErr = err
				} else {
					scans[idx].dstIDs = ids
				}
			}
			dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", dstLabel, m.Destination, idx+1, n))
			dstTracker.Increment(1)
			maybeDiff(&scans[idx], idx, mappings, plans, &totalNew, &totalNewSize)
			if scans[idx].dstErr != nil {
				return fmt.Errorf("scan destination folder %q: %w", m.Destination, scans[idx].dstErr)
			}
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	summary := &SyncSummary{Plans: make([]FolderSyncPlan, 0, n)}
	for idx := range scans {
		if scans[idx].srcErr != nil {
			if verbose {
				pw.Log("⚠️ Failed to fetch source folder %s, skipping by error: %v", mappings[idx].Source, scans[idx].srcErr)
			}
			continue
		}
		if plans[idx].SourceFolder == "" {
			continue
		}
		summary.Plans = append(summary.Plans, plans[idx])
	}
	summary.TotalNew = int(totalNew.Load())
	summary.TotalNewSize = totalNewSize.Load()
	return summary, nil
}

// maybeDiff fires the Message-Id diff exactly once, when both the src and dst
// sides have completed for slot idx. Returns false if only one side is done.
// Frees srcMap and dstIDs immediately after the diff to keep peak memory
// proportional to one folder at a time, not len(mappings).
//
// NewSize is estimated as srcFolderSize × newCount / srcFolderCount — the
// exact size would require an extra UID FETCH RFC822.SIZE for each new UID
// before the user has even confirmed, which is too eager. The proportion is
// good enough for the preview header.
func maybeDiff(s *folderScan, idx int, mappings []config.DirectoryMapping, plans []FolderSyncPlan, totalNew *atomic.Int64, totalNewSize *atomic.Uint64) bool {
	if s.done.Add(1) != 2 {
		return false
	}
	// done==2 guarantees we are the only goroutine here, but mu is still
	// required: the race detector sees srcMap/dstIDs written by the other
	// goroutine and read here without synchronisation unless we take the lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srcErr != nil || s.dstErr != nil {
		s.srcMap, s.dstIDs = nil, nil
		return true
	}
	newUIDs := make([]uint32, 0, len(s.srcMap))
	for id, uid := range s.srcMap {
		if _, present := s.dstIDs[id]; !present {
			newUIDs = append(newUIDs, uid)
		}
	}
	s.srcMap, s.dstIDs = nil, nil
	if len(newUIDs) == 0 {
		return true
	}
	// Sort for stable preview ordering and compactable UID FETCH ranges.
	slices.Sort(newUIDs)
	var newSize uint64
	if s.srcFolderCount > 0 {
		newSize = uint64(float64(s.srcFolderSize) * float64(len(newUIDs)) / float64(s.srcFolderCount))
	}
	plans[idx] = FolderSyncPlan{
		SourceFolder:            mappings[idx].Source,
		DestinationFolder:       mappings[idx].Destination,
		DestinationFolderExists: s.dstExists,
		NewMessages:             len(newUIDs),
		NewSize:                 newSize,
		SrcUIDs:                 newUIDs,
	}
	totalNew.Add(int64(len(newUIDs)))
	totalNewSize.Add(newSize)
	return true
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
func expandMappingsWithSubfolders(ctx context.Context, out io.Writer, srcClient *client.Client, mappings []config.DirectoryMapping, srcDelimiter, dstDelimiter string, verbose, quiet bool) ([]config.DirectoryMapping, error) {
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
				_, _ = fmt.Fprintf(out, "  ⚠️  Failed to get subfolders for %s: %v\n", mapping.Source, err)
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
				_, _ = fmt.Fprintf(out, "  📁 Found subfolder: %s → %s\n", subfolder, dstPath)
			}
		}
	}

	// A parent mapping's subfolder expansion can collide with an explicit
	// mapping for that same subfolder; without dedup the plan ends up with
	// duplicate entries that scan and copy the folder twice.
	deduped, dropped := dedupeMappings(expanded)
	if verbose && !quiet {
		for _, m := range dropped {
			_, _ = fmt.Fprintf(out, "  ℹ️  Mapping %q skipped \u2014 already covered by a parent mapping's expansion\n", m.Source)
		}
	}
	return deduped, nil
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
