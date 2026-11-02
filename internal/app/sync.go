package app

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/progress"
	"github.com/greeddj/imapsync-go/internal/ratelimit"
	"github.com/greeddj/imapsync-go/internal/termsafe"
	"github.com/greeddj/imapsync-go/internal/utils"
	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

const (
	skipReasonNoSelect = `the source server marks it \Noselect`
	skipReasonUnlisted = "the source server lists only its subfolders and refuses to open it"
)

// verboseUIDLimit caps the per-plan UID dump in --verbose preview so a
// large plan (tens of thousands of UIDs) doesn't bury the confirm prompt.
const verboseUIDLimit = 20

// previewFolderLimit caps the folders-to-create list: a name of L bytes has at most L/2 non-empty parent levels, so a full list prints O(L^2) bytes.
const previewFolderLimit = 20

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

// lineLog prints a Client line at once, for the phases that run no progress writer: go-pretty drops a log while no tracker is active.
type lineLog struct{ out io.Writer }

func (l lineLog) Log(msg string, a ...any) {
	if len(a) > 0 {
		msg = fmt.Sprintf(msg, a...)
	}
	_, _ = fmt.Fprintln(l.out, msg)
}

// Client.log prefers a tracker, and only the scan gives a Client one, so the scan tracker must go or it swallows the lines.
func routeClientLogs(w client.ProgressWriter, clients ...*client.Client) {
	for _, c := range clients {
		c.SetProgressWriter(w)
		c.SetProgressTracker(nil)
	}
}

// FolderSyncPlan holds source UIDs that mean something only while the source folder reports SrcUIDValidity; 0 means the server sent none.
type FolderSyncPlan struct {
	SourceFolder            string
	DestinationFolder       string
	SrcUIDs                 []uint32
	standbySources          []standbySource
	standbys                []standbyCopy
	NewMessages             int
	NewSize                 uint64
	ExtraCopies             int
	RepeatedCopies          int // source copies that the one-copy rule leaves out of the plan
	SrcUIDValidity          uint32
	copyRule                copyRule
	DestinationFolderExists bool
}

// copyRule says how a plan counts the copies of one Message-Id. The zero value copies by count.
type copyRule uint8

const (
	copyByCount         copyRule = iota
	copyOnePerMessageID          // at most one copy, for a destination that keeps one message for each Message-Id
)

func copyRuleOf(dst *client.Client) copyRule {
	if dst.KeepsOneCopyPerMessageID() {
		return copyOnePerMessageID
	}
	return copyByCount
}

// sourceCopies cuts a group to its lowest UID under the one-copy rule. The scan sorts refs by ascending UID.
func (r copyRule) sourceCopies(refs []client.MessageRef) []client.MessageRef {
	if r == copyOnePerMessageID && len(refs) > 1 {
		return refs[:1]
	}
	return refs
}

type standbySource struct {
	folder      string
	uidValidity uint32
}

// standbyCopy says that uid in standbySources[source] has the Message-Id of the planned UID.
type standbyCopy struct {
	planned uint32
	uid     uint32
	source  uint32
}

func compareStandbyToPlanned(c standbyCopy, planned uint32) int {
	return cmp.Compare(c.planned, planned)
}

func compareStandbyCopies(a, b standbyCopy) int {
	return cmp.Or(cmp.Compare(a.planned, b.planned), cmp.Compare(a.source, b.source), cmp.Compare(a.uid, b.uid))
}

// Own-folder standbys come first, so a planned copy tries its folder before later mappings. The inputs belong to the shared set, so this never writes to them.
func (p *FolderSyncPlan) mergeStandbys(sources []standbySource, copies []standbyCopy) {
	if len(copies) == 0 {
		return
	}
	if len(p.standbys) == 0 {
		p.standbySources, p.standbys = sources, copies
		return
	}
	shift := uint32(len(p.standbySources))
	merged := make([]standbyCopy, len(p.standbys), len(p.standbys)+len(copies))
	copy(merged, p.standbys)
	for _, c := range copies {
		c.source += shift
		merged = append(merged, c)
	}
	slices.SortFunc(merged, compareStandbyCopies)
	p.standbySources = slices.Concat(p.standbySources, sources)
	p.standbys = merged
}

// standbys is sorted by planned UID, then by source, then by UID. The binary search finds a group that is already in source order, and each source's copies ascend.
func (p *FolderSyncPlan) standbysOf(planned uint32) []standbyCopy {
	i, _ := slices.BinarySearchFunc(p.standbys, planned, compareStandbyToPlanned)
	end := i
	for end < len(p.standbys) && p.standbys[end].planned == planned {
		end++
	}
	return p.standbys[i:end]
}

// FolderFailure names a mapping that the plan cannot copy, and why.
type FolderFailure struct {
	Err               error
	SourceFolder      string
	DestinationFolder string
}

// FolderSkip names a source folder the plan leaves out without failing the
// run, and the server refusal behind it.
type FolderSkip struct {
	Err          error
	SourceFolder string
}

// SyncSummary is the result of the planning scan; TotalNewSize adds up the RFC822.SIZE values that the scan already read.
type SyncSummary struct {
	Plans        []FolderSyncPlan
	Failures     []FolderFailure
	Skipped      []FolderSkip
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
	if quiet && !autoConfirm {
		return errQuietWithoutConfirm
	}

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
	srcOpts.Label = cfg.Src.Label
	srcOpts.Verbose = verbose
	srcOpts.ReadLimiter = srcReadLim
	dstOpts := opts.clientOpts
	dstOpts.Auth = cfg.Dst.Auth
	dstOpts.Label = cfg.Dst.Label
	dstOpts.Verbose = verbose
	dstOpts.WriteLimiter = dstWriteLim

	if !quiet {
		if w := buildProviderWarning(cfg, srcReadLim, dstWriteLim); w != "" {
			_, _ = fmt.Fprint(out, w)
		}
		if w := buildUploadLimitWarning(cfg.RateLimit.UpBPS, cfg.Workers); w != "" {
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
	case len(cfg.Map) > 0:
		// A delimiter rewrite edits entries in place, and cfg.Map belongs to the caller.
		mappings = slices.Clone(cfg.Map)
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
		srcClient = c
		return nil
	})
	g.Go(func() error {
		c, err := client.New(gCtx, cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, dstOpts)
		if err != nil {
			return fmt.Errorf("destination connection failed: %w", err)
		}
		dstClient = c
		return nil
	})
	groupErr := g.Wait()
	defer func() { logoutAll(srcClient, dstClient) }()
	if groupErr != nil {
		return groupErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	printInvalidNames(out, cfg.Src.Label, srcClient.InvalidMailboxNames())
	printInvalidNames(out, cfg.Dst.Label, dstClient.InvalidMailboxNames())

	typed := len(mappings) > 0
	srcDelimiter := srcClient.GetDelimiter()
	dstDelimiter := dstClient.GetDelimiter()

	if !quiet && verbose {
		_, _ = fmt.Fprintf(out, "📁 Server delimiters:\n")
		_, _ = fmt.Fprintf(out, "  Source [%s]: %q\n", cfg.Src.Label, srcDelimiter)
		_, _ = fmt.Fprintf(out, "  Destination [%s]: %q\n\n", cfg.Dst.Label, dstDelimiter)
		if dstClient.KeepsOneCopyPerMessageID() {
			_, _ = fmt.Fprintf(out, "  ℹ️  [%s] The destination server advertises X-GM-EXT-1, so each destination folder gets at most one copy of each Message-Id\n\n", cfg.Dst.Label)
		}
	}

	var roots []folderMapping
	if typed {
		if err := offerDelimiterFixes(ctx, out, in, srcClient, dstClient, mappings, autoConfirm); err != nil {
			return err
		}
		// Typed names resolve to the listed spelling, so the conflict check, the clash check and the subfolder cut compare the server's bytes.
		resolveMappings(mappings, srcClient, dstClient)
		if err := checkSourceConflicts(mappings); err != nil {
			return err
		}
		destinationFlag := ""
		if opts.dstFolder != "" {
			destinationFlag = "--dest-folder"
		}
		if err := checkDestinationLevels(ctx, dstClient, mappings, destinationFlag); err != nil {
			return err
		}
		roots = typedMappings(mappings)
	} else {
		// The names come from the planning client's mailbox cache, so no extra session opens.
		roots = mirrorMappings(srcClient.MailboxNames(), srcDelimiter, dstDelimiter)
	}

	// Expand mappings to include subfolders
	if !quiet && verbose {
		_, _ = fmt.Fprintln(out, "Checking for subfolders...")
	}
	expanded, err := expandMappingsWithSubfolders(ctx, out, srcClient, dstClient, roots, srcDelimiter, dstDelimiter, verbose, quiet)
	if err != nil {
		return fmt.Errorf("failed to expand mappings: %w", err)
	}
	if len(expanded) > len(roots) && !quiet && verbose {
		_, _ = fmt.Fprintf(out, "📂 Found %d subfolders, total folders to sync: %d\n", len(expanded)-len(roots), len(expanded))
	}
	planned, clashes := splitDestinationClashes(expanded)
	printFolderRenames(out, planned, dstDelimiter)
	mappings = directoryMappings(planned)
	printCaseVariantDestinations(out, mappings, dstClient.MailboxNames())

	lineOut := out
	if quiet {
		lineOut = io.Discard
	}
	lines := lineLog{out: lineOut}

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

	summary, err := buildSyncPlan(ctx, srcClient, dstClient, mappings, srcTracker, dstTracker, cfg.Src.Label, cfg.Dst.Label)
	if err != nil {
		pw.Stop()
		return err
	}

	// Mark scanning as complete
	srcTracker.MarkAsDone()
	dstTracker.MarkAsDone()

	pw.Stop()
	routeClientLogs(lines, srcClient, dstClient)
	if err := ctx.Err(); err != nil {
		return err
	}
	summary.Failures = slices.Concat(clashes, summary.Failures)

	if !quiet && len(summary.Skipped) > 0 {
		printFolderSkips(out, summary.Skipped)
	}

	// Quiet mode shows this too: like the final error line, it reports a failure.
	if len(summary.Failures) > 0 {
		printFolderFailures(out, summary.Failures)
	}

	newFolders, err := foldersToCreate(ctx, summary.Plans, dstClient)
	if err != nil {
		return err
	}

	if summary.TotalNew > 0 {
		if !quiet {
			_, _ = fmt.Fprintf(out, "📤 Messages to be copied to destination:\n")
			for _, plan := range summary.Plans {
				if plan.NewMessages > 0 {
					_, _ = fmt.Fprintf(out, "• %s → %s will copy %d messages (≈ %s)\n",
						termsafe.Escape(plan.SourceFolder), termsafe.Escape(plan.DestinationFolder),
						plan.NewMessages, utils.FormatSize(plan.NewSize))
					if plan.ExtraCopies > 0 {
						_, _ = fmt.Fprintf(out, "  ℹ️  %d of these messages have a Message-Id that the destination folder already holds\n", plan.ExtraCopies)
					}
					if plan.RepeatedCopies > 0 {
						_, _ = fmt.Fprintf(out, "  ℹ️  %d repeated copies of these messages are left out: the destination server keeps one message for each Message-Id\n", plan.RepeatedCopies)
					}
					if verbose {
						// Dumping every UID before the confirm-prompt
						// drowns the user in screens of integers - a
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

			printFoldersToCreate(out, newFolders)
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
		return endRunWithoutCopy(out, quiet, len(summary.Failures))
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
	planFolders := make(map[string]bool)
	for _, plan := range activePlans {
		if !plan.DestinationFolderExists {
			planFolders[plan.DestinationFolder] = true
		}
	}
	// Folders whose plan may have no diff against the real folder: CREATE said it exists, or the destination LIST left out names, so an OK proves nothing.
	existing := make(map[string]struct{})
	var stop throttleStop

	if len(planFolders) > 0 {
		if !quiet {
			_, _ = fmt.Fprintln(out, "\n📁 Creating destination folders...")
		}

		creationPW := newProgressWriter(1, quiet, out)
		creationPW.Start()
		creationTracker := progress.NewTracker("Creating folders", int64(len(newFolders)))
		// The loop decides the final state: an auto-stop on the last Increment would make MarkAsErrored do nothing and render the (k/k) Creating text.
		creationTracker.AutoStopDisabled = true
		traceTracker("folder-create", creationTracker.Message)
		creationPW.AppendTracker(creationTracker)
		routeClientLogs(creationPW, dstClient)

		createdCount, refusedCount, settled := 0, 0, 0
		refused := make(map[string]error)
		dstListIncomplete := len(dstClient.InvalidMailboxNames()) > 0

		// Sorted, so a parent path is created before its children and the log
		// order is stable.
	createFolders:
		for _, folder := range slices.Sorted(maps.Keys(planFolders)) {
			if err := ctx.Err(); err != nil {
				creationPW.Stop()
				return err
			}
			creationTracker.UpdateMessage(fmt.Sprintf("(%d/%d) Creating %s", min(settled+1, len(newFolders)), len(newFolders), termsafe.Escape(folder)))

			created, parents, err := dstClient.CreateMailbox(ctx, folder)
			// A refused folder can follow parents that this call made.
			createdCount += parents
			switch {
			case err != nil && ctx.Err() != nil:
				creationPW.Stop()
				return ctx.Err()
			case err != nil:
				creationPW.Log("Failed to create folder %q: %s", folder, termsafe.Escape(err.Error()))
				refusedCount++
				// A throttled reply limits the account, not this folder, so no more CREATE follows.
				if stop.recordThrottled(sideDestination, err) {
					break createFolders
				}
				refused[folder] = err
			case created:
				if verbose {
					creationPW.Log("Created folder %q", folder)
				}
				if dstListIncomplete {
					existing[folder] = struct{}{}
				}
				createdCount++
			default:
				if verbose {
					creationPW.Log("Folder %s already exists", termsafe.Escape(folder))
				}
				existing[folder] = struct{}{}
			}

			settled += parents + 1
			creationTracker.Increment(int64(parents + 1))
		}

		if refusedCount > 0 {
			creationTracker.UpdateMessage(fmt.Sprintf("Created %d of %d folders, %d refused", createdCount, len(newFolders), refusedCount))
			creationTracker.MarkAsErrored()
		} else {
			creationTracker.UpdateMessage(fmt.Sprintf("Created %d folders", createdCount))
			creationTracker.MarkAsDone()
		}
		creationPW.Stop()
		routeClientLogs(lines, dstClient)

		if len(refused) > 0 {
			var refusals []FolderFailure
			activePlans, refusals = splitRefusedDestinations(activePlans, refused)
			summary.Failures = append(summary.Failures, refusals...)
			printFolderFailures(out, refusals)
		}
	}

	if len(existing) > 0 && !stop.stopped() {
		var newFailures []FolderFailure
		activePlans, newFailures, err = recheckExistingDestinations(ctx, out, quiet, srcClient, dstClient, activePlans, existing, &stop)
		if err != nil {
			return err
		}
		if len(newFailures) > 0 {
			summary.Failures = append(summary.Failures, newFailures...)
			printFolderFailures(out, newFailures)
		}
	}

	if stop.stopped() {
		if err := ctx.Err(); err != nil {
			return err
		}
		planned := 0
		for _, plan := range activePlans {
			planned += plan.NewMessages
		}
		return endRunWithErrors(out, cfg, quiet, summary.Failures, stop.get(), 0, planned)
	}

	if len(activePlans) == 0 {
		return endRunWithoutCopy(out, quiet, len(summary.Failures))
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

	// One writer serves the whole copy. go-pretty has no terminal-height limit, so a plan's tracker is appended only when a worker takes it: at most one row per worker.
	syncPW := newProgressWriter(len(activePlans), quiet, out)
	syncPW.Start()
	defer syncPW.Stop()

	for _, w := range workers.all {
		routeClientLogs(syncPW, w.src, w.dst)
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

	firstUnstarted := len(activePlans)
	for i, plan := range activePlans {
		if ctx.Err() != nil || stop.stopped() {
			firstUnstarted = i
			break
		}
		var w *syncWorker
		select {
		case w = <-free:
		case <-ctx.Done():
		}
		if w == nil {
			firstUnstarted = i
			break
		}
		// A cancel or a stop can land while the loop waits for a worker.
		if ctx.Err() != nil || stop.stopped() {
			free <- w
			firstUnstarted = i
			break
		}
		tr := newPlanTracker(plan, i, len(activePlans))
		traceTracker("plan", tr.Message)
		syncPW.AppendTracker(tr)
		wg.Add(1)
		go func(idx int, p FolderSyncPlan, w *syncWorker, tr *progress.Tracker) {
			defer wg.Done()
			defer func() { free <- w }()
			synced, errs, _ := runFolderSync(ctx, w, p, &stop, tr, idx, len(activePlans), syncPW, verbose)
			totalSynced.Add(int64(synced))
			totalErrors.Add(int64(errs))
		}(i, plan, w, tr)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}

	cause := stop.get()
	if cause != nil {
		for i := firstUnstarted; i < len(activePlans); i++ {
			totalErrors.Add(int64(activePlans[i].NewMessages))
			tr := newStoppedPlanTracker(activePlans[i], i, len(activePlans))
			traceTracker("plan-stopped", tr.Message)
			syncPW.AppendTracker(tr)
		}
	}

	// Stop redraws the last frame over the active tracker rows, so a line printed before it would be erased. The stopped rows are appended before Stop, so they render.
	syncPW.Stop()

	totalSyncedN := int(totalSynced.Load())
	totalErrorsN := int(totalErrors.Load())

	if totalErrorsN > 0 || len(summary.Failures) > 0 || cause != nil {
		return endRunWithErrors(out, cfg, quiet, summary.Failures, cause, totalSyncedN, totalErrorsN)
	}

	_, _ = fmt.Fprintln(out, "✨ Sync completed successfully. ✨")
	return nil
}

// Quiet mode lists the failed folders only once, because the caller printed them already.
func endRunWithErrors(out io.Writer, cfg *config.Config, quiet bool, failures []FolderFailure, cause *throttleCause, synced, errs int) error {
	if len(failures) > 0 && !quiet {
		printFolderFailures(out, failures)
	}
	if cause != nil {
		printThrottleStop(out, cfg, cause)
	}
	printSyncErrors(out, synced, errs, len(failures))
	// The summary is printed already, so the exit code needs no second message from main.
	return ErrSilentExit
}

// The caller has already printed the failed folders.
func endRunWithoutCopy(out io.Writer, quiet bool, failedFolders int) error {
	if failedFolders > 0 {
		printSyncErrors(out, 0, 0, failedFolders)
		return ErrSilentExit
	}
	if !quiet {
		_, _ = fmt.Fprintln(out, "✅ All folders already synced!")
	}
	return nil
}

// MissingLevels decides the names, so the preview and the creation counter match the CREATE commands while the server accepts each one.
func foldersToCreate(ctx context.Context, plans []FolderSyncPlan, dst *client.Client) ([]string, error) {
	names := make([]string, 0, len(plans))
	listed := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.NewMessages == 0 || plan.DestinationFolderExists {
			continue
		}
		levels, err := dst.MissingLevels(ctx, plan.DestinationFolder)
		if err != nil {
			return nil, err
		}
		for _, level := range levels {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, dup := listed[level]; dup {
				continue
			}
			listed[level] = struct{}{}
			names = append(names, level)
		}
	}
	return names, nil
}

func printFoldersToCreate(out io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n🗂️ Folders to be created on destination:\n")
	for _, folder := range names[:min(len(names), previewFolderLimit)] {
		_, _ = fmt.Fprintf(out, "• %s\n", termsafe.Escape(folder))
	}
	if len(names) > previewFolderLimit {
		_, _ = fmt.Fprintf(out, "• … and %d more\n", len(names)-previewFolderLimit)
	}
}

// The folder that would receive their messages does not exist, so each plan of a refused destination fails and the others stay.
func splitRefusedDestinations(plans []FolderSyncPlan, refused map[string]error) (kept []FolderSyncPlan, failures []FolderFailure) {
	kept = make([]FolderSyncPlan, 0, len(plans))
	for _, plan := range plans {
		err, ok := refused[plan.DestinationFolder]
		if !ok {
			kept = append(kept, plan)
			continue
		}
		failures = append(failures, FolderFailure{
			SourceFolder:      plan.SourceFolder,
			DestinationFolder: plan.DestinationFolder,
			Err:               fmt.Errorf("%w: %w", errDestinationCreate, err),
		})
	}
	return kept, failures
}

// The prompt already showed the plans, so a plan only loses messages. Lines print after every check, so a cancel leaves no output.
func recheckExistingDestinations(ctx context.Context, out io.Writer, quiet bool, srcClient, dstClient *client.Client, plans []FolderSyncPlan, existing map[string]struct{}, stop *throttleStop) ([]FolderSyncPlan, []FolderFailure, error) {
	groupOf := make(map[string]int)
	var groups [][]int
	for i, plan := range plans {
		if _, ok := existing[plan.DestinationFolder]; !ok {
			continue
		}
		g, seen := groupOf[plan.DestinationFolder]
		if !seen {
			g = len(groups)
			groupOf[plan.DestinationFolder] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}

	results := make([]recheckResult, len(plans))
	for _, indexes := range groups {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		group := make([]FolderSyncPlan, len(indexes))
		for slot, i := range indexes {
			group[slot] = plans[i]
		}
		rechecked, err := recheckDestination(ctx, srcClient, dstClient, group, stop)
		if err != nil {
			return nil, nil, err
		}
		// The run ends on the stop and counts every planned message as an error, so earlier groups report nothing.
		if stop.stopped() {
			return plans, nil, nil
		}
		for slot, i := range indexes {
			results[i] = rechecked[slot]
		}
	}

	kept := make([]FolderSyncPlan, 0, len(plans))
	var failures []FolderFailure
	for i, plan := range plans {
		if _, ok := existing[plan.DestinationFolder]; !ok {
			kept = append(kept, plan)
			continue
		}
		result := results[i]
		if result.err != nil {
			failures = append(failures, FolderFailure{
				SourceFolder:      plan.SourceFolder,
				DestinationFolder: plan.DestinationFolder,
				Err:               result.err,
			})
			continue
		}
		if result.drops.held > 0 && !quiet {
			_, _ = fmt.Fprintf(out, "  ℹ️  Destination folder %s already exists and holds %d of the %d planned messages from %s, so they are not copied\n",
				termsafe.Escape(plan.DestinationFolder), result.drops.held, plan.NewMessages, termsafe.Escape(plan.SourceFolder))
		}
		if result.drops.gone > 0 && !quiet {
			_, _ = fmt.Fprintf(out, "  ℹ️  Source folder %s no longer holds %d of the %d planned messages for %s, so they are not copied\n",
				termsafe.Escape(plan.SourceFolder), result.drops.gone, plan.NewMessages, termsafe.Escape(plan.DestinationFolder))
		}
		if len(result.plan.SrcUIDs) > 0 {
			kept = append(kept, result.plan)
		}
	}
	return kept, failures, nil
}

type recheckResult struct {
	err   error
	plan  FolderSyncPlan
	drops recheckDrops
}

// The plans share one destination, in mapping order. A throttled reply sets the stop and returns no results. The error is only for an ended context.
func recheckDestination(ctx context.Context, srcClient, dstClient *client.Client, plans []FolderSyncPlan, stop *throttleStop) ([]recheckResult, error) {
	results := make([]recheckResult, len(plans))
	for slot, plan := range plans {
		results[slot].plan = plan
	}
	dstMap, err := dstClient.FetchMessageCopies(ctx, plans[0].DestinationFolder)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if stop.recordThrottled(sideDestination, err) {
			return nil, nil
		}
		for slot := range results {
			results[slot].err = fmt.Errorf("%w: %w", errDestinationRecheck, err)
		}
		return results, nil
	}
	// An empty folder holds nothing to drop, so it needs no source scan.
	if len(dstMap) == 0 {
		for slot := range results {
			results[slot].plan.DestinationFolderExists = true
		}
		return results, nil
	}

	diff := newDestinationDiff(plans, dstMap)
	added := make([]bool, len(plans))
	for slot, plan := range plans {
		// The planning scan already warned about messages without a Message-Id. RescanMessageMap returns the context error before it sends a command.
		srcMap, uidValidity, err := srcClient.RescanMessageMap(ctx, plan.SourceFolder)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if stop.recordThrottled(sideSource, err) {
				return nil, nil
			}
			results[slot].err = fmt.Errorf("%w: %w", errSourceScan, err)
			continue
		}
		// A new UIDVALIDITY means the planned UIDs point elsewhere, and a diff would mix two UID spaces.
		if err := client.CheckUIDValidity(plan.SrcUIDValidity, uidValidity); err != nil {
			results[slot].err = err
			continue
		}
		diff.addSource(slot, srcMap)
		added[slot] = true
	}
	joint := diff.results()
	for slot := range results {
		if added[slot] {
			results[slot] = joint[slot]
		}
	}
	return results, nil
}

// held counts the dropped planned messages that the source still lists. gone counts those that the source no longer lists.
type recheckDrops struct {
	held, gone int
}

// destinationDiff pairs each source with the sources before it, as maybeDiff does for an empty destination, then lets the destination cover the rest.
type destinationDiff struct {
	dstMap map[string][]client.MessageRef
	// union holds the copies the destination needs if no source changes. A reservation names its slot, because UIDs of two folders collide.
	union map[string][]reservation
	plans []FolderSyncPlan
	slots []slotDiff
	refs  []client.MessageRef
	m     copyMatcher
}

// slotDiff keeps what one source adds to the diff. listed holds the planned UIDs that the new scan lists, once for each listed copy.
type slotDiff struct {
	repeated []repeatedCopies
	kept     []keptCopy
	listed   []uint32
}

type keptCopy struct {
	uid   uint32
	size  uint32
	extra bool
}

func newDestinationDiff(plans []FolderSyncPlan, dstMap map[string][]client.MessageRef) *destinationDiff {
	return &destinationDiff{
		dstMap: dstMap,
		union:  make(map[string][]reservation),
		plans:  plans,
		slots:  make([]slotDiff, len(plans)),
	}
}

func isPlanned(plan *FolderSyncPlan, uid uint32) bool {
	_, ok := slices.BinarySearch(plan.SrcUIDs, uid)
	return ok
}

// addSource takes the maps one at a time, so peak memory stays proportional to one source folder and the union.
func (d *destinationDiff) addSource(slot int, srcMap map[string][]client.MessageRef) {
	plan := &d.plans[slot]
	s := &d.slots[slot]
	for id, all := range srcMap {
		for _, ref := range all {
			if isPlanned(plan, ref.UID) {
				s.listed = append(s.listed, ref.UID)
			}
		}
		refs := plan.copyRule.sourceCopies(all)
		entries := d.union[id]
		var owner []int32
		if len(entries) > 0 && len(refs) > 0 {
			owner = d.m.reservationOwners(refs, entries)
		}
		others := len(all) - len(refs)
		for i, ref := range refs {
			if len(owner) > 0 && owner[i] >= 0 {
				continue
			}
			d.union[id] = append(d.union[id], reservation{uid: ref.UID, size: ref.Size, slot: int32(slot)})
			// Only the one-copy rule cuts a group, so a plan that counts by copies never has others.
			if others > 0 && isPlanned(plan, ref.UID) {
				s.repeated = append(s.repeated, repeatedCopies{uid: ref.UID, others: others})
			}
		}
	}
}

// The result gets new UID slices, because plan.SrcUIDs backs summary.Plans. Call it once, after the last addSource.
func (d *destinationDiff) results() []recheckResult {
	for id, entries := range d.union {
		dst := d.dstMap[id]
		if len(dst) >= len(entries) {
			continue
		}
		var owner []int32
		if len(dst) > 0 {
			d.refs = d.refs[:0]
			for _, e := range entries {
				d.refs = append(d.refs, client.MessageRef{UID: e.uid, Size: e.size})
			}
			owner = d.m.destinationOwners(d.refs, dst)
		}
		// Owners map back by entry index, never by UID.
		for i, e := range entries {
			if len(owner) > 0 && owner[i] >= 0 {
				continue
			}
			if isPlanned(&d.plans[e.slot], e.uid) {
				d.slots[e.slot].kept = append(d.slots[e.slot].kept, keptCopy{uid: e.uid, size: e.size, extra: len(dst) > 0})
			}
		}
	}

	out := make([]recheckResult, len(d.plans))
	for slot := range d.plans {
		plan := d.plans[slot]
		s := &d.slots[slot]
		slices.SortFunc(s.kept, func(a, b keptCopy) int { return cmp.Compare(a.uid, b.uid) })
		slices.SortFunc(s.repeated, func(a, b repeatedCopies) int { return cmp.Compare(a.uid, b.uid) })
		slices.Sort(s.listed)
		kept := make([]uint32, 0, len(s.kept))
		var drops recheckDrops
		var newSize uint64
		var extraCopies, repeatedTotal int
		for _, uid := range plan.SrcUIDs {
			i, ok := slices.BinarySearchFunc(s.kept, uid, compareKeptToUID)
			if !ok {
				// A server can list one UID under two Message-Ids, so counting listed copies could pass the planned count.
				if _, listed := slices.BinarySearch(s.listed, uid); listed {
					drops.held++
				} else {
					drops.gone++
				}
				continue
			}
			kept = append(kept, uid)
			newSize += uint64(s.kept[i].size)
			if s.kept[i].extra {
				extraCopies++
			}
			if j, ok := slices.BinarySearchFunc(s.repeated, uid, compareRepeatedToUID); ok {
				repeatedTotal += s.repeated[j].others
			}
		}
		plan.SrcUIDs = kept
		plan.NewMessages = len(kept)
		plan.NewSize = newSize
		plan.ExtraCopies = extraCopies
		plan.RepeatedCopies = repeatedTotal
		plan.DestinationFolderExists = true
		out[slot] = recheckResult{plan: plan, drops: drops}
	}
	return out
}

func compareKeptToUID(c keptCopy, uid uint32) int { return cmp.Compare(c.uid, uid) }

// repeatedCopies says that the planned uid stands for others more source copies of its Message-Id.
type repeatedCopies struct {
	uid    uint32
	others int
}

func compareRepeatedToUID(r repeatedCopies, uid uint32) int { return cmp.Compare(r.uid, uid) }

// copyMatcher is the scratch of one diff, reused across Message-Id groups. Never share it between goroutines.
type copyMatcher struct {
	left      []client.MessageRef
	owner     []int32
	refOrder  []int32
	copyOrder []int32
	sizes     []uint32
	matched   []bool
}

// destinationLeft reads only dst sizes, so the plan and the recheck agree. The result is valid until the next call.
func (m *copyMatcher) destinationLeft(refs, dst []client.MessageRef) []client.MessageRef {
	if len(dst) == 0 {
		return refs
	}
	if len(dst) >= len(refs) {
		return nil
	}
	owner := m.destinationOwners(refs, dst)
	m.left = m.left[:0]
	for i, ref := range refs {
		if owner[i] < 0 {
			m.left = append(m.left, ref)
		}
	}
	return m.left
}

// destinationOwners is match with the sizes of the destination copies. Valid until the next call.
func (m *copyMatcher) destinationOwners(refs, dst []client.MessageRef) []int32 {
	m.sizes = m.sizes[:0]
	for _, d := range dst {
		m.sizes = append(m.sizes, d.Size)
	}
	return m.match(refs, m.sizes)
}

// reservationOwners pairs the refs that remain after the destination step with the reserved copies. Call it with at least one of each.
func (m *copyMatcher) reservationOwners(left []client.MessageRef, held []reservation) []int32 {
	m.sizes = m.sizes[:0]
	for _, r := range held {
		m.sizes = append(m.sizes, r.size)
	}
	return m.match(left, m.sizes)
}

// match returns, for each ref in ascending UID order, the index of the copy that accounts for it, or -1. Valid until the next call.
func (m *copyMatcher) match(refs []client.MessageRef, sizes []uint32) []int32 {
	m.owner = m.owner[:0]
	m.refOrder = m.refOrder[:0]
	for i := range refs {
		m.owner = append(m.owner, -1)
		m.refOrder = append(m.refOrder, int32(i))
	}
	m.copyOrder = m.copyOrder[:0]
	m.matched = m.matched[:0]
	for c := range sizes {
		m.copyOrder = append(m.copyOrder, int32(c))
		m.matched = append(m.matched, false)
	}
	// Sorting keeps this O(k log k): a hostile server can send many copies of one Message-Id, so a loop over refs x copies is not an option.
	slices.SortFunc(m.refOrder, func(a, b int32) int {
		return cmp.Or(cmp.Compare(refs[a].Size, refs[b].Size), cmp.Compare(a, b))
	})
	slices.SortFunc(m.copyOrder, func(a, b int32) int {
		return cmp.Or(cmp.Compare(sizes[a], sizes[b]), cmp.Compare(a, b))
	})
	// Size only picks which refs of one Message-Id a copy accounts for; counts never change, so a server that rewrites messages still converges.
	r, c := 0, 0
	for r < len(m.refOrder) && c < len(m.copyOrder) {
		ref, cp := m.refOrder[r], m.copyOrder[c]
		switch {
		case refs[ref].Size < sizes[cp]:
			r++
		case refs[ref].Size > sizes[cp]:
			c++
		default:
			m.owner[ref] = cp
			m.matched[cp] = true
			r++
			c++
		}
	}
	// A copy that no size matches takes the lowest free UID, so equal or rewritten sizes pick the first refs as a plain count would.
	p := 0
	for cp := range sizes {
		if m.matched[cp] {
			continue
		}
		for p < len(refs) && m.owner[p] >= 0 {
			p++
		}
		if p == len(refs) {
			break
		}
		m.owner[p] = int32(cp)
		p++
	}
	return m.owner
}

func countRefs(byID map[string][]client.MessageRef) int {
	var n int
	for _, refs := range byID {
		n += len(refs)
	}
	return n
}

// Folder names and Err can carry server text, so every part goes through termsafe.Escape.
func printFolderFailures(out io.Writer, failures []FolderFailure) {
	_, _ = fmt.Fprint(out, "\n❌ Folders that cannot be synced:\n")
	for _, f := range failures {
		_, _ = fmt.Fprintf(out, "• %s → %s: %s\n",
			termsafe.Escape(f.SourceFolder), termsafe.Escape(f.DestinationFolder), termsafe.Escape(f.Err.Error()))
	}
}

// The folder name and Err can carry server text, so both go through termsafe.Escape.
func printFolderSkips(out io.Writer, skips []FolderSkip) {
	for _, s := range skips {
		_, _ = fmt.Fprintf(out, "  ℹ️  Folder %s skipped - %s: %s\n",
			termsafe.Escape(s.SourceFolder), skipReasonUnlisted, termsafe.Escape(s.Err.Error()))
	}
}

// The reason is server text, so it goes through termsafe.Escape. Labels come from the operator.
func printThrottleStop(out io.Writer, cfg *config.Config, cause *throttleCause) {
	label := cfg.Dst.Label
	if cause.side == sideSource {
		label = cfg.Src.Label
	}
	_, _ = fmt.Fprintf(out, "⛔ Sync stopped: the %s server [%s] reports a rate limit or a full quota: %s. Run sync again later: it continues where this run stopped.\n",
		cause.side, label, termsafe.Escape(cause.err.Error()))
}

func printSyncErrors(out io.Writer, synced, errs, failedFolders int) {
	_, _ = fmt.Fprintf(out, "❌ Sync completed with errors. %d messages uploaded, %d errors occurred, %d folders failed\n",
		synced, errs, failedFolders)
}

// folderScan counts finished sides in done, and maybeDiff fires at 2. srcMessages is set before srcMap is freed, so the \Noselect failure can use it after the diff.
type folderScan struct {
	srcMap         map[string][]client.MessageRef
	dstMap         map[string][]client.MessageRef
	srcErr         error
	dstErr         error
	srcMessages    int
	srcUIDValidity uint32
	dstExists      bool
	dstNoSelect    bool
	mu             sync.Mutex
	done           atomic.Int32
}

type reservation struct {
	uid  uint32
	size uint32
	slot int32
}

type standbySet struct {
	sources []standbySource
	copies  []standbyCopy
}

// Slot k is diffed before slot k+1, because both scans walk in index order.
type sharedDestinations struct {
	// last maps a shared destination to its last mapping index. It is read-only, so reads need no lock.
	last map[string]int
	// reserved keeps an entry only until the last mapping of its destination is diffed.
	reserved map[string]map[string][]reservation
	// standbys is indexed by the slot of the reserving plan. It outlives reserved, because buildSyncPlan reads it after the scans.
	standbys []standbySet
	// mu guards reserved and standbys and keeps the race detector independent of the plan order.
	mu sync.Mutex
}

// newSharedDestinations returns nil when no destination has two mappings, so a plain run costs nothing.
func newSharedDestinations(mappings []config.DirectoryMapping) *sharedDestinations {
	seen := make(map[string]struct{}, len(mappings))
	var last map[string]int
	for i, m := range mappings {
		if _, repeated := seen[m.Destination]; repeated {
			if last == nil {
				last = make(map[string]int)
			}
			last[m.Destination] = i
		}
		seen[m.Destination] = struct{}{}
	}
	if last == nil {
		return nil
	}
	return &sharedDestinations{
		last:     last,
		reserved: make(map[string]map[string][]reservation, len(last)),
		standbys: make([]standbySet, len(mappings)),
	}
}

func (d *sharedDestinations) lastIndex(destination string) (int, bool) {
	if d == nil {
		return 0, false
	}
	i, ok := d.last[destination]
	return i, ok
}

// The caller holds d.mu.
func (d *sharedDestinations) addStandby(r reservation, src standbySource, uid uint32) {
	set := &d.standbys[r.slot]
	// Slots are diffed in order, so the source is usually the last one.
	i := len(set.sources) - 1
	for i >= 0 && set.sources[i] != src {
		i--
	}
	if i < 0 {
		set.sources = append(set.sources, src)
		i = len(set.sources) - 1
	}
	set.copies = append(set.copies, standbyCopy{planned: r.uid, uid: uid, source: uint32(i)})
}

// Call it only after the scans end.
func (d *sharedDestinations) standbysFor(slot int) ([]standbySource, []standbyCopy) {
	if d == nil {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	set := &d.standbys[slot]
	slices.SortFunc(set.copies, compareStandbyCopies)
	return set.sources, set.copies
}

func buildSyncPlan(ctx context.Context, srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, srcTracker, dstTracker *progress.Tracker, srcLabel, dstLabel string) (*SyncSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	n := len(mappings)
	srcTracker.UpdateTotal(int64(n))
	dstTracker.UpdateTotal(int64(n))

	scans := make([]folderScan, n)
	plans := make([]FolderSyncPlan, n)
	shared := newSharedDestinations(mappings)
	rule := copyRuleOf(dstClient)
	var totalNew atomic.Int64
	var totalNewSize atomic.Uint64

	// errgroup.WithContext: if either goroutine returns a non-nil error,
	// gCtx is cancelled, which causes the other goroutine's next gCtx.Err()
	// check to bail out immediately - no need to drain all remaining folders.
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		for idx, m := range mappings {
			if err := gCtx.Err(); err != nil {
				return err
			}
			srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", srcLabel, termsafe.Escape(m.Source), idx+1, n))
			mp, uidValidity, err := srcClient.FetchMessageMap(gCtx, m.Source)
			if err != nil {
				scans[idx].srcErr = err
			} else {
				scans[idx].srcMap = mp
				scans[idx].srcMessages = countRefs(mp)
				scans[idx].srcUIDValidity = uidValidity
			}
			srcTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", srcLabel, termsafe.Escape(m.Source), idx+1, n))
			srcTracker.Increment(1)
			maybeDiff(&scans[idx], idx, mappings, plans, &totalNew, &totalNewSize, shared, rule)
		}
		return nil
	})

	g.Go(func() error {
		for idx, m := range mappings {
			if err := gCtx.Err(); err != nil {
				return err
			}
			dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanning %s (%d/%d)", dstLabel, termsafe.Escape(m.Destination), idx+1, n))
			exists, err := dstClient.MailboxExists(gCtx, m.Destination)
			switch {
			case err != nil:
				scans[idx].dstErr = err
			case !exists:
				// dstExists stays false, dstMap stays nil - benign; folder will be created.
			case dstClient.IsNoSelect(m.Destination):
				// A \Noselect folder is listed but refuses EXAMINE, so scanning it would
				// abort the whole plan.
				scans[idx].dstExists = true
				scans[idx].dstNoSelect = true
			default:
				scans[idx].dstExists = true
				mp, err := dstClient.FetchMessageCopies(gCtx, m.Destination)
				if err != nil {
					scans[idx].dstErr = err
				} else {
					scans[idx].dstMap = mp
				}
			}
			dstTracker.UpdateMessage(fmt.Sprintf("[%s] Scanned %s (%d/%d)", dstLabel, termsafe.Escape(m.Destination), idx+1, n))
			dstTracker.Increment(1)
			maybeDiff(&scans[idx], idx, mappings, plans, &totalNew, &totalNewSize, shared, rule)
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
		if srcErr := scans[idx].srcErr; srcErr != nil {
			// An unlisted name with listed children is a hierarchy level that
			// refuses to open, not a broken folder. Any other refusal stays a failure.
			if errors.Is(srcErr, client.ErrSelectRefused) {
				level, err := isHierarchyLevel(ctx, srcClient, mappings[idx].Source)
				if err != nil {
					return nil, err
				}
				if level {
					summary.Skipped = append(summary.Skipped, FolderSkip{SourceFolder: mappings[idx].Source, Err: srcErr})
					continue
				}
			}
			summary.Failures = append(summary.Failures, FolderFailure{
				SourceFolder:      mappings[idx].Source,
				DestinationFolder: mappings[idx].Destination,
				Err:               fmt.Errorf("%w: %w", errSourceScan, srcErr),
			})
			continue
		}
		if scans[idx].dstNoSelect {
			if scans[idx].srcMessages > 0 {
				summary.Failures = append(summary.Failures, FolderFailure{
					SourceFolder:      mappings[idx].Source,
					DestinationFolder: mappings[idx].Destination,
					Err:               fmt.Errorf("%d messages not copied: %w", scans[idx].srcMessages, errDestinationNoSelect),
				})
			}
			continue
		}
		if plans[idx].SourceFolder == "" {
			continue
		}
		plan := plans[idx]
		plan.mergeStandbys(shared.standbysFor(idx))
		summary.Plans = append(summary.Plans, plan)
	}
	summary.TotalNew = int(totalNew.Load())
	summary.TotalNewSize = totalNewSize.Load()
	return summary, nil
}

// maybeDiff frees srcMap and dstMap right after the diff, so peak memory stays proportional to one folder, not len(mappings).
func maybeDiff(s *folderScan, idx int, mappings []config.DirectoryMapping, plans []FolderSyncPlan, totalNew *atomic.Int64, totalNewSize *atomic.Uint64, shared *sharedDestinations, rule copyRule) bool {
	if s.done.Add(1) != 2 {
		return false
	}
	// done==2 makes this the only goroutine here, but the race detector needs mu to see srcMap and dstMap that the other goroutine wrote.
	s.mu.Lock()
	defer s.mu.Unlock()

	destination := mappings[idx].Destination
	var reserved map[string][]reservation
	reserve := false
	if last, ok := shared.lastIndex(destination); ok {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		reserved = shared.reserved[destination]
		reserve = idx != last
		if !reserve {
			// Deferred, so a failed last slot releases the reservations too. It runs before the unlock.
			defer delete(shared.reserved, destination)
		}
	}

	if s.srcErr != nil || s.dstErr != nil || s.dstNoSelect {
		s.srcMap, s.dstMap = nil, nil
		return true
	}
	standby := standbySource{folder: mappings[idx].Source, uidValidity: s.srcUIDValidity}
	newUIDs := make([]uint32, 0, s.srcMessages)
	var newSize uint64
	var extra, repeated int
	var ownStandbys []standbyCopy
	var m copyMatcher
	for id, all := range s.srcMap {
		onDst := s.dstMap[id]
		refs := rule.sourceCopies(all)
		cut := all[len(refs):]
		left := m.destinationLeft(refs, onDst)
		held := reserved[id]
		var owner []int32
		if len(held) > 0 && len(left) > 0 {
			// Pairing by size decides which standby replaces which reserved copy, also when every ref is taken.
			owner = m.reservationOwners(left, held)
		}
		var holder reservation
		paired := false
		for i, ref := range left {
			if len(owner) > 0 && owner[i] >= 0 {
				holder, paired = held[owner[i]], true
				if mappings[holder.slot].Source != standby.folder {
					shared.addStandby(holder, standby, ref.UID)
				}
				continue
			}
			if reserve {
				if reserved == nil {
					reserved = make(map[string][]reservation)
					shared.reserved[destination] = reserved
				}
				reserved[id] = append(reserved[id], reservation{uid: ref.UID, size: ref.Size, slot: int32(idx)})
			}
			newUIDs = append(newUIDs, ref.UID)
			newSize += uint64(ref.Size)
			repeated += len(all) - len(refs)
			// Only copies that the destination already holds count. A copy reserved in this run is not there yet, and the next run sees it if the server keeps it.
			if len(onDst) > 0 {
				extra++
			}
		}
		// The one-copy rule keeps one ref and cuts the rest. The cut copies stand by for it, in case its body is empty or missing.
		if len(cut) > 0 && len(left) == 1 {
			switch {
			case !paired:
				for _, c := range cut {
					ownStandbys = append(ownStandbys, standbyCopy{planned: left[0].UID, uid: c.UID})
				}
			case mappings[holder.slot].Source != standby.folder:
				for _, c := range cut {
					shared.addStandby(holder, standby, c.UID)
				}
			}
		}
	}
	s.srcMap, s.dstMap = nil, nil
	if len(newUIDs) == 0 {
		return true
	}
	// Sort for stable preview ordering and compactable UID FETCH ranges.
	slices.Sort(newUIDs)
	plans[idx] = FolderSyncPlan{
		SourceFolder:            mappings[idx].Source,
		DestinationFolder:       destination,
		DestinationFolderExists: s.dstExists,
		NewMessages:             len(newUIDs),
		NewSize:                 newSize,
		ExtraCopies:             extra,
		RepeatedCopies:          repeated,
		SrcUIDs:                 newUIDs,
		SrcUIDValidity:          s.srcUIDValidity,
		copyRule:                rule,
	}
	if len(ownStandbys) > 0 {
		slices.SortFunc(ownStandbys, compareStandbyCopies)
		plans[idx].standbySources = []standbySource{standby}
		plans[idx].standbys = ownStandbys
	}
	totalNew.Add(int64(len(newUIDs)))
	totalNewSize.Add(newSize)
	return true
}

type delimiterFix struct {
	typed     string
	rewritten string
	used      string
	index     int
	source    bool
}

type folderMapping struct {
	config.DirectoryMapping
	// renamed marks a derived destination name in which the conversion replaced a destination delimiter.
	renamed bool
}

// expansionEntry records which mapping produced a folder: root is the Source of that mapping, and equals the entry Source for the mapping itself.
type expansionEntry struct {
	root string
	folderMapping
}

func resolveMappings(mappings []config.DirectoryMapping, srcClient, dstClient *client.Client) {
	for i := range mappings {
		mappings[i].Source = srcClient.ResolveName(mappings[i].Source)
		mappings[i].Destination = dstClient.ResolveName(mappings[i].Destination)
	}
}

// checkSourceConflicts runs after the resolution, so two spellings of one source count as one source.
func checkSourceConflicts(mappings []config.DirectoryMapping) error {
	first := make(map[string]int, len(mappings))
	for i, m := range mappings {
		j, seen := first[m.Source]
		if !seen {
			first[m.Source] = i
			continue
		}
		if m.Destination != mappings[j].Destination {
			return fmt.Errorf("map entries %d and %d: %w %s", j+1, i+1, errSourceConflict, m.Source)
		}
	}
	return nil
}

// checkDestinationLevels refuses a typed destination that CREATE would split at an empty level; a listed name needs no CREATE. flag names the mapping source, empty for the config map.
func checkDestinationLevels(ctx context.Context, dst *client.Client, mappings []config.DirectoryMapping, flag string) error {
	delimiter := dst.GetDelimiter()
	for i, m := range mappings {
		if !hasEmptyLevel(m.Destination, delimiter) {
			continue
		}
		listed, err := dst.MailboxExists(ctx, m.Destination)
		if err != nil {
			return err
		}
		if listed {
			continue
		}
		where := flag
		if where == "" {
			where = fmt.Sprintf("map entry %d", i+1)
		}
		return fmt.Errorf(`%s: %w: "%s", destination delimiter "%s"`, where, errEmptyLevel, m.Destination, delimiter)
	}
	return nil
}

func hasEmptyLevel(name, delimiter string) bool {
	return delimiter != "" && (strings.HasPrefix(name, delimiter) || strings.HasSuffix(name, delimiter) || strings.Contains(name, delimiter+delimiter))
}

func typedMappings(in []config.DirectoryMapping) []folderMapping {
	out := make([]folderMapping, len(in))
	for i, m := range in {
		out[i] = folderMapping{DirectoryMapping: m}
	}
	return out
}

func mirrorMappings(names []string, srcDelimiter, dstDelimiter string) []folderMapping {
	out := make([]folderMapping, len(names))
	for i, name := range names {
		destination, renamed := convertFolderPath(name, srcDelimiter, dstDelimiter)
		out[i] = folderMapping{DirectoryMapping: config.DirectoryMapping{Source: name, Destination: destination}, renamed: renamed}
	}
	return out
}

func directoryMappings(in []folderMapping) []config.DirectoryMapping {
	out := make([]config.DirectoryMapping, len(in))
	for i, m := range in {
		out[i] = m.DirectoryMapping
	}
	return out
}

// A destination delimiter inside a source segment is name text, so it must not create a level.
func convertFolderPath(path, srcDelimiter, dstDelimiter string) (converted string, renamed bool) {
	if dstDelimiter == "" || srcDelimiter == dstDelimiter {
		return path, false
	}
	if !strings.Contains(path, dstDelimiter) {
		if srcDelimiter == "" {
			return path, false
		}
		return strings.ReplaceAll(path, srcDelimiter, dstDelimiter), false
	}
	substitute := "_"
	if dstDelimiter == "_" {
		substitute = "-"
	}
	if srcDelimiter == "" {
		return strings.ReplaceAll(path, dstDelimiter, substitute), true
	}
	segments := strings.Split(path, srcDelimiter)
	for i, segment := range segments {
		segments[i] = strings.ReplaceAll(segment, dstDelimiter, substitute)
	}
	return strings.Join(segments, dstDelimiter), true
}

// Every mapping in a rename clash fails: a winner could take a folder that an earlier run filled from another source.
func splitDestinationClashes(in []folderMapping) (kept []folderMapping, clashes []FolderFailure) {
	type group struct {
		first, second string
		count         int
		renamed       bool
	}
	groups := make(map[string]group, len(in))
	for _, m := range in {
		g := groups[m.Destination]
		switch g.count {
		case 0:
			g.first = m.Source
		case 1:
			g.second = m.Source
		}
		g.count++
		g.renamed = g.renamed || m.renamed
		groups[m.Destination] = g
	}

	kept = make([]folderMapping, 0, len(in))
	for _, m := range in {
		g := groups[m.Destination]
		if g.count < 2 || !g.renamed {
			kept = append(kept, m)
			continue
		}
		other := g.first
		if m.Source == g.first {
			other = g.second
		}
		clashes = append(clashes, FolderFailure{
			SourceFolder:      m.Source,
			DestinationFolder: m.Destination,
			Err:               fmt.Errorf("%w %s", errDestinationClash, other),
		})
	}
	return kept, clashes
}

// Each name is raw server text, so it goes through termsafe.Escape.
func printInvalidNames(out io.Writer, label string, names []string) {
	for _, name := range names {
		_, _ = fmt.Fprintf(out, "⚠️  [%s] Folder %s is left out: the server sent a name that is not valid modified UTF-7\n",
			label, termsafe.Escape(name))
	}
}

// The names and the delimiter come from the servers, so every part goes through termsafe.Escape.
func printFolderRenames(out io.Writer, mappings []folderMapping, dstDelimiter string) {
	printed := false
	for _, m := range mappings {
		if !m.renamed {
			continue
		}
		_, _ = fmt.Fprintf(out, "⚠️  Folder %s is renamed to %s: the destination server uses \"%s\" as its hierarchy delimiter\n",
			termsafe.Escape(m.Source), termsafe.Escape(m.Destination), termsafe.Escape(dstDelimiter))
		printed = true
	}
	if printed {
		_, _ = fmt.Fprintln(out)
	}
}

// RFC 3501 names are case-sensitive, so the plan keeps case variants apart, but a server that ignores case merges them. Names are server text, so they go through termsafe.Escape.
func printCaseVariantDestinations(out io.Writer, mappings []config.DirectoryMapping, listed []string) {
	firstName := make(map[string]string, len(mappings))
	var variants map[string][]string
	for _, m := range mappings {
		key := strings.ToLower(m.Destination)
		first, seen := firstName[key]
		if !seen {
			firstName[key] = m.Destination
			continue
		}
		group := variants[key]
		if group == nil {
			if m.Destination == first {
				continue
			}
			group = []string{first}
		}
		if variants == nil {
			variants = make(map[string][]string)
		}
		variants[key] = append(group, m.Destination)
	}

	if len(variants) == 0 {
		return
	}
	// MailboxNames returns names sorted in byte order, so a binary search keeps this lookup logarithmic on a server-controlled LIST.
	unlisted := func(name string) bool {
		_, found := slices.BinarySearch(listed, name)
		return !found
	}
	printed := false
	for _, key := range slices.Sorted(maps.Keys(variants)) {
		names := variants[key]
		if !slices.ContainsFunc(names, unlisted) {
			continue
		}
		slices.Sort(names)
		names = slices.Compact(names)
		for i, name := range names {
			names[i] = termsafe.Escape(name)
		}
		_, _ = fmt.Fprintf(out, "⚠️  Destination folders %s differ only in letter case. If the destination server ignores letter case, a message that their source folders share is copied more than once.\n",
			strings.Join(names, ", "))
		printed = true
	}
	if printed {
		_, _ = fmt.Fprintln(out)
	}
}

// delimiterRewrite never rewrites a listed name, because a delimiter character can be name text. It offers only a listed rewrite, one foreign delimiter at a time.
func delimiterRewrite(ctx context.Context, c *client.Client, typed string) (rewritten, used string, err error) {
	serverDelimiter := c.GetDelimiter()
	if serverDelimiter == "" {
		return "", "", nil
	}
	exists, err := c.MailboxExists(ctx, typed)
	if err != nil || exists {
		return "", "", err
	}
	for _, d := range [...]string{"/", ".", `\`} {
		if d == serverDelimiter || !strings.Contains(typed, d) {
			continue
		}
		candidate := strings.ReplaceAll(typed, d, serverDelimiter)
		exists, err := c.MailboxExists(ctx, candidate)
		if err != nil {
			return "", "", err
		}
		if exists {
			return candidate, d, nil
		}
	}
	return "", "", nil
}

func collectDelimiterFixes(ctx context.Context, srcClient, dstClient *client.Client, mappings []config.DirectoryMapping) ([]delimiterFix, error) {
	var fixes []delimiterFix
	for i, m := range mappings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rewritten, used, err := delimiterRewrite(ctx, srcClient, m.Source)
		if err != nil {
			return nil, err
		}
		if rewritten != "" {
			fixes = append(fixes, delimiterFix{index: i, source: true, typed: m.Source, rewritten: rewritten, used: used})
		}
		rewritten, used, err = delimiterRewrite(ctx, dstClient, m.Destination)
		if err != nil {
			return nil, err
		}
		if rewritten != "" {
			fixes = append(fixes, delimiterFix{index: i, typed: m.Destination, rewritten: rewritten, used: used})
		}
	}
	return fixes, nil
}

// offerDelimiterFixes asks one question for all rewrites, so piped input needs one answer however many mappings change.
func offerDelimiterFixes(ctx context.Context, out io.Writer, in *bufio.Reader, srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, autoConfirm bool) error {
	fixes, err := collectDelimiterFixes(ctx, srcClient, dstClient, mappings)
	if err != nil {
		return err
	}
	if len(fixes) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(out, "⚠️  Folder path delimiter mismatch detected:\n")
	for _, f := range fixes {
		if f.source {
			_, _ = fmt.Fprintf(out, "  • Mapping %d: Source folder %q uses delimiter %q, server expects %q\n",
				f.index+1, f.typed, f.used, srcClient.GetDelimiter())
		} else {
			_, _ = fmt.Fprintf(out, "  • Mapping %d: Destination folder %q uses delimiter %q, server expects %q\n",
				f.index+1, f.typed, f.used, dstClient.GetDelimiter())
		}
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
		for _, f := range fixes {
			if f.source {
				mappings[f.index].Source = f.rewritten
				_, _ = fmt.Fprintf(out, "  ✓ Fixed source: %q → %q\n", f.typed, f.rewritten)
			} else {
				mappings[f.index].Destination = f.rewritten
				_, _ = fmt.Fprintf(out, "  ✓ Fixed destination: %q → %q\n", f.typed, f.rewritten)
			}
		}
	} else {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "⚠️ Please note if delimiter is not corresponding to the server configuration, the folder structure may not be correctly interpreted.")
	}
	_, _ = fmt.Fprintln(out)
	return nil
}

// expandMappingsWithSubfolders lets the most specific mapping decide where a folder goes, so the order of the map items does not change the plan.
func expandMappingsWithSubfolders(ctx context.Context, out io.Writer, srcClient, dstClient *client.Client, roots []folderMapping, srcDelimiter, dstDelimiter string, verbose, quiet bool) ([]folderMapping, error) {
	entries := make([]expansionEntry, 0, len(roots))
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A \Noselect source cannot be scanned. Its subfolders still can be scanned.
	var skipped map[string]string
	skip := func(name, reason string) {
		if skipped == nil {
			skipped = make(map[string]string)
		}
		skipped[name] = reason
	}

	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A flat source has no subfolders: ListSubfolders would swap an empty delimiter for the client's own.
		var subfolders []string
		if srcDelimiter != "" {
			// The listing reads the mailbox cache, so it fails only when ctx ends.
			var err error
			subfolders, err = getSubfolders(ctx, srcClient, root.Source, srcDelimiter)
			if err != nil {
				return nil, err
			}
			// The cache lists names in map order. A fixed order keeps the plans and the failure lines the same on each run.
			slices.Sort(subfolders)
		}
		if srcClient.IsNoSelect(root.Source) {
			skip(root.Source, skipReasonNoSelect)
		} else {
			own := root
			own.Destination = dstClient.ResolveName(root.Destination)
			entries = append(entries, expansionEntry{root: root.Source, folderMapping: own})
		}

		// Add mapping for each subfolder
		for _, subfolder := range subfolders {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if srcClient.IsNoSelect(subfolder) {
				skip(subfolder, skipReasonNoSelect)
				continue
			}
			// A listed child can spell an INBOX level in another letter case than root.Source, but the length is the same.
			relativePath := subfolder[len(root.Source)+len(srcDelimiter):]

			// Build destination path
			var dstPath string
			renamed := root.renamed
			if dstDelimiter != "" {
				converted, relRenamed := convertFolderPath(relativePath, srcDelimiter, dstDelimiter)
				dstPath = root.Destination + dstDelimiter + converted
				renamed = renamed || relRenamed
			} else {
				// A flat destination has no hierarchy: the source delimiter is name text, and merging into the parent would join distinct folders.
				dstPath = root.Destination + srcDelimiter + relativePath
			}

			// A derived destination can spell an INBOX level in another letter case than the one the server lists, and the wire needs the listed bytes.
			entries = append(entries, expansionEntry{
				root: root.Source,
				folderMapping: folderMapping{
					DirectoryMapping: config.DirectoryMapping{Source: subfolder, Destination: dstClient.ResolveName(dstPath)},
					renamed:          renamed,
				},
			})
		}
	}

	winners := mostSpecific(entries)
	expanded := make([]folderMapping, 0, len(entries))
	for i, e := range entries {
		if winners[i] == i {
			expanded = append(expanded, e.folderMapping)
		}
	}
	if verbose && !quiet {
		printExpansion(out, entries, winners, skipped)
	}
	return expanded, nil
}

// A found line shows only a destination that the plan uses, so verbose never names one that lost.
func printExpansion(out io.Writer, entries []expansionEntry, winners []int, skipped map[string]string) {
	for i, e := range entries {
		if e.root != e.Source && e.Destination == entries[winners[i]].Destination {
			_, _ = fmt.Fprintf(out, "  📁 Found subfolder: %s → %s\n", termsafe.Escape(e.Source), termsafe.Escape(e.Destination))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(skipped)) {
		_, _ = fmt.Fprintf(out, "  ℹ️  Folder %s skipped - %s\n", termsafe.Escape(name), skipped[name])
	}
	for i, e := range entries {
		if winners[i] == i {
			continue
		}
		kept := entries[winners[i]]
		if e.Destination == kept.Destination {
			_, _ = fmt.Fprintf(out, "  ℹ️  Mapping %q skipped - already covered by a parent mapping's expansion\n", e.Source)
			continue
		}
		_, _ = fmt.Fprintf(out, "  ℹ️  Folder %s goes to %s, not to %s: the mapping of %s wins over the mapping of %s\n",
			termsafe.Escape(e.Source), termsafe.Escape(kept.Destination), termsafe.Escape(e.Destination),
			termsafe.Escape(kept.root), termsafe.Escape(e.root))
	}
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

// mostSpecific returns, for each entry, the index of the entry that its Source keeps.
func mostSpecific(entries []expansionEntry) []int {
	best := make(map[string]int, len(entries))
	for i, e := range entries {
		if j, seen := best[e.Source]; seen {
			prior := entries[j].root
			// Every root that produced a name is an ancestor of it, and the INBOX fold keeps the length, so the longer root is the nearer ancestor.
			if len(e.root) < len(prior) {
				continue
			}
			// Equal lengths mean two listed spellings of one INBOX level. Byte order, as in the mailbox cache, keeps the choice independent of the map order.
			if len(e.root) == len(prior) && e.root >= prior {
				continue
			}
		}
		best[e.Source] = i
	}
	winners := make([]int, len(entries))
	for i, e := range entries {
		winners[i] = best[e.Source]
	}
	return winners
}

// isHierarchyLevel reports whether LIST leaves name out but lists its subfolders. It reads
// only the mailbox cache, so it fails only when ctx ends.
func isHierarchyLevel(ctx context.Context, c *client.Client, name string) (bool, error) {
	// RFC 3501 says INBOX always exists, so an INBOX that LIST leaves out is still a mailbox.
	if strings.EqualFold(name, "INBOX") {
		return false, nil
	}
	exists, err := c.MailboxExists(ctx, name)
	if err != nil || exists {
		return false, err
	}
	subfolders, err := getSubfolders(ctx, c, name, "")
	if err != nil {
		return false, err
	}
	return len(subfolders) > 0, nil
}

// getSubfolders returns all subfolders of a given folder
func getSubfolders(ctx context.Context, c *client.Client, folder string, delimiter string) ([]string, error) {
	return c.ListSubfolders(ctx, folder, delimiter)
}

// buildProviderWarning produces a human-readable banner for accounts on
// providers with known IMAP limits. Empty string when neither side is a known
// provider, so the caller can print unconditionally.
//
// The banner is informational, not blocking - it sits before the confirm
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
		fmt.Fprintf(&b, "   %s [%s] - %s\n", h.side, h.host, h.provider.Name)
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
				fmt.Fprintf(&b, "     • no rate limit set - recommended: %s %d\n", flag, recommended)
			}
		}
		if h.provider.Notes != "" {
			fmt.Fprintf(&b, "     • notes: %s\n", h.provider.Notes)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// buildUploadLimitWarning flags an upload limit so low that one literal write
// can wait out the idle watchdog. Empty when the limit is unset or high enough.
func buildUploadLimitWarning(upBPS, workers int) string {
	if upBPS <= 0 {
		return ""
	}
	minBPS := client.MinUploadBPS(workers)
	if upBPS >= minBPS {
		return ""
	}
	return fmt.Sprintf("\n⚠️  Upload limit %d bytes/s is below %d bytes/s for %d workers.\n"+
		"   An upload can wait so long for its share that the idle watchdog closes the connection.\n"+
		"   Raise --bps-up or lower --workers.\n\n", upBPS, minBPS, workers)
}
