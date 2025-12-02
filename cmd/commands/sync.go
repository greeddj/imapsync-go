// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap"
	"github.com/greeddj/imapsync-go/internal/cache"
	"github.com/greeddj/imapsync-go/internal/client"
	"github.com/greeddj/imapsync-go/internal/config"
	"github.com/greeddj/imapsync-go/internal/stdout"
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
	useCache := !cCtx.Bool("no-cache")

	spin := stdout.New(quiet, verbose)
	defer spin.Stop()

	spin.Update("Fetching config...")
	cfg, err := config.New(cCtx)
	if err != nil {
		spin.Error(fmt.Sprintf("Config error: %v", err))
		return fmt.Errorf("failed to load config: %w", err)
	}

	spin.Update(fmt.Sprintf("Starting sync with %d workers", cfg.Workers))

	var cacheManager *cache.CacheManager

	if useCache {
		spin.Update("Initializing cache...")
		cacheManager, err = cache.NewCacheManager(cfg.Src, cfg.Dst)
		if err != nil {
			spin.Update(fmt.Sprintf("Warning: failed to initialize cache: %v", err))
			useCache = false
		} else {
			if err := cacheManager.Load(); err != nil {
				spin.Update(fmt.Sprintf("Warning: failed to load cache: %v", err))
			} else if verbose {
				fmt.Println(cacheManager.GetCacheInfo())
			}
		}
	}

	var mappings []config.DirectoryMapping

	if srcFolder != "" && dstFolder != "" {
		mappings = []config.DirectoryMapping{
			{Source: srcFolder, Destination: dstFolder},
		}
	} else if srcFolder != "" || dstFolder != "" {
		spin.Error("both --src-folder and --dest-folder must be specified")
		return fmt.Errorf("both --src-folder and --dest-folder must be specified")
	} else {
		if len(cfg.Map) == 0 {
			return fmt.Errorf("no folder mappings in config")
		}
		mappings = cfg.Map
	}

	spin.Update("Fetching source...")
	srcClient, err := client.New(cfg.Src.Server, cfg.Src.User, cfg.Src.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("source connection failed: %w", err)
	}
	defer func() {
		_ = srcClient.Logout()
	}()
	srcClient.SetProgress(spin)
	srcClient.SetPrefix(cfg.Src.Label)

	spin.Update("Fetching destination...")
	dstClient, err := client.New(cfg.Dst.Server, cfg.Dst.User, cfg.Dst.Pass, 1, verbose, true, nil)
	if err != nil {
		return fmt.Errorf("destination connection failed: %w", err)
	}
	defer func() {
		_ = dstClient.Logout()
	}()
	dstClient.SetProgress(spin)
	dstClient.SetPrefix(cfg.Dst.Label)

	spin.Update("Building sync plan...")
	summary, err := buildSyncPlan(srcClient, dstClient, mappings, spin, cacheManager, useCache)
	if err != nil {
		return err
	}

	if summary.TotalNew > 0 && !quiet {
		spin.Stop()
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
				spin.Restart()
				spin.Error("Sync canceled by user")
				return nil
			}
		}
		spin.Restart()
	} else {
		spin.Success("All folders already synced!")
		return nil
	}

	totalSynced := 0
	totalErrors := 0

	for i, plan := range summary.Plans {
		if plan.NewMessages == 0 {
			continue
		}

		spin.Update(fmt.Sprintf("Syncing folder %d/%d: %s → %s (%d messages)", i+1, len(summary.Plans), plan.SourceFolder, plan.DestinationFolder, plan.NewMessages))

		spin.Update(fmt.Sprintf("Checking folder: %s", plan.DestinationFolder))
		if !plan.DestinationFolderExists {
			created, err := dstClient.CreateMailbox(plan.DestinationFolder)
			if err != nil {
				spin.Print(fmt.Sprintf("Failed to create folder %s: %v", plan.DestinationFolder, err))
				totalErrors++
				continue
			}
			if created {
				spin.Update(fmt.Sprintf("Created destination folder: %s", plan.DestinationFolder))
			}
		}

		synced, errors := syncFolders(cfg, plan.DestinationFolder, plan.MessagesToSync, cfg.Workers, spin, verbose)
		totalSynced += synced
		totalErrors += errors

		spin.Update(fmt.Sprintf("Folder %s: synced %d/%d messages", plan.DestinationFolder, synced, plan.NewMessages))
	}

	if useCache && cacheManager != nil {
		spin.Update("Updating cache...")
		for _, plan := range summary.Plans {
			if plan.NewMessages > 0 {
				dstMsgs, err := dstClient.FetchMessages(plan.DestinationFolder)
				if err != nil {
					spin.Update(fmt.Sprintf("Warning: failed to update cache for %s: %v", plan.DestinationFolder, err))
					continue
				}
				cacheManager.DestCache.UpdateMailbox(plan.DestinationFolder, dstMsgs)
			}
		}

		if err := cacheManager.Save(); err != nil {
			spin.Update(fmt.Sprintf("Warning: failed to save cache: %v", err))
		}
	}

	if totalErrors > 0 {
		spin.Error(fmt.Sprintf("Sync completed with errors. %d messages uploaded, %d errors occurred", totalSynced, totalErrors))
		return fmt.Errorf("sync completed with %d errors", totalErrors)
	}

	spin.Success(fmt.Sprintf("Sync completed successfully. %d new messages uploaded", totalSynced))
	return nil
}

// syncFolders syncs messages using multiple parallel workers.
func syncFolders(cfg *config.Config, dstFolder string, messages []*imap.Message, numWorkers int, spin *stdout.Spinner, verbose bool) (int, int) {
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
				spin.Update(fmt.Sprintf("Worker %d: failed to connect: %v", workerID, err))
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer func() {
				_ = workerClient.Logout()
			}()
			workerClient.SetProgress(spin)
			workerClient.SetPrefix(fmt.Sprintf("%s-%d", cfg.Dst.Label, workerID))

			for msg := range jobs {
				if err := workerClient.AppendMessage(dstFolder, msg); err != nil {
					workerClient.UpdateProgress(fmt.Sprintf("Worker %d: error syncing dir %s on message: %v", workerID, dstFolder, err))
					atomic.AddInt64(&errorCount, 1)
				} else {
					atomic.AddInt64(&syncedCount, 1)
					workerClient.UpdateProgress(fmt.Sprintf("Syncing dir %s [messages %d/%d]", dstFolder, atomic.LoadInt64(&syncedCount), len(messages)))
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
func buildSyncPlan(srcClient, dstClient *client.Client, mappings []config.DirectoryMapping, spin *stdout.Spinner, cacheManager *cache.CacheManager, useCache bool) (*SyncSummary, error) {
	summary := &SyncSummary{
		Plans: make([]FolderSyncPlan, 0),
	}

	for _, mapping := range mappings {
		srcFolder := mapping.Source
		dstFolder := mapping.Destination
		var dstFolderExists bool
		var srcMessages []*imap.Message
		var dstMessageIDs map[string]bool
		var err error

		spin.Update(fmt.Sprintf("Fetching source: %s", srcFolder))
		srcMessages, err = srcClient.FetchMessages(srcFolder)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch source folder %s: %w", srcFolder, err)
		}

		dstMessageIDs = make(map[string]bool)

		if useCache && cacheManager != nil {
			spin.Update(fmt.Sprintf("Checking cache for: %s", dstFolder))
			cachedDst := cacheManager.DestCache.GetMailbox(dstFolder)
			if cachedDst != nil && len(cachedDst.Messages) > 0 {
				for msgID := range cachedDst.Messages {
					dstMessageIDs[msgID] = true
				}
				spin.Update(fmt.Sprintf("Using cached destination for %s (%d messages)", dstFolder, len(cachedDst.Messages)))
			}
		}

		if len(dstMessageIDs) == 0 {
			spin.Update(fmt.Sprintf("Fetching destination: %s", dstFolder))
			dstMessages, err := dstClient.FetchMessages(dstFolder)
			if err != nil {
				// Folder might not exist, treat as empty
				spin.Update(fmt.Sprintf("Destination folder %s not found or empty, will create", dstFolder))
			} else {
				dstFolderExists = true
				for _, msg := range dstMessages {
					if msg.Envelope != nil && msg.Envelope.MessageId != "" {
						dstMessageIDs[strings.Trim(msg.Envelope.MessageId, "<>")] = true
					}
				}
			}
		}

		var messagesToSync []*imap.Message
		for _, msg := range srcMessages {
			if msg.Envelope == nil || msg.Envelope.MessageId == "" {
				continue
			}
			msgID := strings.Trim(msg.Envelope.MessageId, "<>")
			if !dstMessageIDs[msgID] {
				messagesToSync = append(messagesToSync, msg)
			} else {
				spin.Update(fmt.Sprintf("Message %s already exists in destination, skipping", msg.Envelope.Subject))
			}
		}

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
