package client

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// FetchMessageIDs scans a folder and returns all message IDs.
//
// Messages without a usable Message-Id are counted and reported once via the
// progress writer — without that header the diff has no key to match on, so
// they cannot be tracked across servers and will be silently skipped.
func (c *Client) FetchMessageIDs(ctx context.Context, folder string) (map[string]bool, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	var ids map[string]bool
	var missingCount int
	err := c.safeCall(func(cli *imapclient.Client) error {
		ids = make(map[string]bool)
		missingCount = 0
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)
		if mbox.Messages == 0 {
			return nil
		}
		c.log("[%s] Fetching %d message IDs from %s...", c.prefix, mbox.Messages, folder)

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() {
			done <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
		}()

		for msg := range messages {
			if ctx.Err() != nil {
				continue
			}
			if msg.Envelope == nil || msg.Envelope.MessageId == "" {
				missingCount++
				continue
			}
			msgID := strings.Trim(msg.Envelope.MessageId, "<>")
			if msgID == "" {
				missingCount++
				continue
			}
			ids[msgID] = true
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch IDs: %w", c.prefix, err)
		}
		return nil
	})

	if err == nil && missingCount > 0 && c.pw != nil {
		c.pw.Log("[%s] ⚠️  %s: %d message(s) without Message-Id will be skipped — sync cannot track them",
			c.prefix, folder, missingCount)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// FetchMessages retrieves full message envelopes and bodies for a folder.
func (c *Client) FetchMessages(ctx context.Context, folder string) ([]*imap.Message, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var all []*imap.Message
	err := c.safeCall(func(cli *imapclient.Client) error {
		all = nil
		c.log("[%s] Fetching folder %s...", c.prefix, folder)
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, mbox.Messages)
		if mbox.Messages == 0 {
			return nil
		}
		c.log("[%s] Fetching %d messages from %s...", c.prefix, mbox.Messages, folder)

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		go func() {
			done <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
		}()

		count := 0
		for msg := range messages {
			if ctx.Err() == nil {
				all = append(all, msg)
			}
			count++
			if count%progressUpdateInterval == 0 {
				c.log("[%s] Processed %d/%d messages from %s...", c.prefix, count, mbox.Messages, folder)
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return all, nil
}

// StreamMessagesByIDs fetches messages matching targetIDs and invokes
// onMessage for each one as it arrives, batched by uidFetchBatchSize.
//
// Streaming avoids materializing every body into memory at once — important
// for large mailboxes where the cumulative body size can be many GB.
//
// If onMessage returns an error, the channel from the in-flight batch is
// drained (so the producer goroutine exits cleanly) and the error is
// returned without scheduling further batches.
func (c *Client) StreamMessagesByIDs(ctx context.Context, folder string, targetIDs map[string]bool, onMessage func(*imap.Message) error) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if len(targetIDs) == 0 {
		return nil
	}

	c.log("[%s] Streaming %d specific messages from %s...", c.prefix, len(targetIDs), folder)

	// Stage 1: envelope scan — map Message-Id to UID.
	var targetUIDs []uint32
	err := c.safeCall(func(cli *imapclient.Client) error {
		targetUIDs = nil
		mbox, err := cli.Select(folder, true)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		if mbox.Messages == 0 {
			return nil
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, mbox.Messages)

		envMessages := make(chan *imap.Message, messageChanBuffer)
		envDone := make(chan error, 1)
		go func() {
			envDone <- cli.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, envMessages)
		}()

		for msg := range envMessages {
			if ctx.Err() == nil && msg.Envelope != nil && msg.Envelope.MessageId != "" {
				msgID := strings.Trim(msg.Envelope.MessageId, "<>")
				if targetIDs[msgID] {
					targetUIDs = append(targetUIDs, msg.Uid)
				}
			}
		}
		if err := <-envDone; err != nil {
			return fmt.Errorf("[%s] envelope fetch: %w", c.prefix, err)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(targetUIDs) == 0 {
		return nil
	}

	c.log("[%s] Streaming %d messages from %s", c.prefix, len(targetUIDs), folder)
	slices.Sort(targetUIDs)

	// Stage 2: batched body fetch, streamed to onMessage.
	var cbErr error
	for start := 0; start < len(targetUIDs); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cbErr != nil {
			return cbErr
		}

		end := min(start+uidFetchBatchSize, len(targetUIDs))
		uids := targetUIDs[start:end]

		err := c.safeCall(func(cli *imapclient.Client) error {
			// Re-Select so a reconnect-then-retry has the right state.
			if _, err := cli.Select(folder, true); err != nil {
				return fmt.Errorf("[%s] reselect folder %s: %w", c.prefix, folder, err)
			}
			uidSet := new(imap.SeqSet)
			for _, uid := range uids {
				uidSet.AddNum(uid)
			}
			messages := make(chan *imap.Message, messageChanBuffer)
			batchDone := make(chan error, 1)
			go func() {
				batchDone <- cli.UidFetch(uidSet, []imap.FetchItem{imap.FetchEnvelope, imap.FetchRFC822}, messages)
			}()

			for msg := range messages {
				// Once cancelled or callback errored, just drain so the
				// producer goroutine can exit and we don't leak it.
				if ctx.Err() != nil || cbErr != nil {
					continue
				}
				if e := onMessage(msg); e != nil {
					cbErr = e
				}
			}
			if err := <-batchDone; err != nil {
				return fmt.Errorf("[%s] body fetch: %w", c.prefix, err)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if cbErr != nil {
			return cbErr
		}
	}
	return nil
}
