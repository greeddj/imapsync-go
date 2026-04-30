package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/mail"
	"slices"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// messageIDHeaderSection is the smallest fetch we can ask for that still
// gives us what we need for diffing: just the Message-Id header plus the
// terminating CRLF, with PEEK so the source's \Seen flag is unchanged.
//
// Every Fetch method below uses this section instead of FetchEnvelope. On
// large folders this is dramatically less data on the wire — Envelope also
// pulls From/To/Cc/Bcc/Subject/Date/References/InReplyTo, which we don't
// need for the planning diff.
var messageIDHeaderSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{
		Specifier: imap.HeaderSpecifier,
		Fields:    []string{"Message-Id"},
	},
	Peek: true,
}

// fullBodyPeekSection requests the entire RFC822 body without flipping the
// \Seen flag on the source. This matters: a sync tool must not mutate the
// source mailbox state. The previous implementation used FetchRFC822 which
// is functionally equivalent to BODY[] (RFC 3501 §6.4.5) and *does* mark
// messages as read.
var fullBodyPeekSection = &imap.BodySectionName{Peek: true}

// FetchMessageMap returns Message-Id → UID for every message in folder.
//
// One pass over the folder yields both pieces. Messages without a usable
// Message-Id are counted and reported once via the progress writer — without
// that header the diff has no key to match on, so they cannot be tracked
// across servers and will be silently skipped.
//
// The returned map is suitable both as the source side of a Message-Id diff
// and (with UIDs ignored) as the destination side; callers that only need
// the keys can use FetchMessageIDSet for a slightly thinner allocation.
func (c *Client) FetchMessageMap(ctx context.Context, folder string) (map[string]uint32, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	var (
		ids          map[string]uint32
		missingCount int
	)
	err := c.safeCall(func(cli *imapclient.Client) error {
		ids = nil
		missingCount = 0
		mbox, err := c.selectIfNeeded(cli, folder)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		// selectIfNeeded skips the round-trip when the folder is already
		// selected; it returns nil mbox in that case. Fall back to a STATUS
		// for the message count, which is cheap.
		var total uint32
		if mbox != nil {
			total = mbox.Messages
		} else {
			st, serr := cli.Status(folder, []imap.StatusItem{imap.StatusMessages})
			if serr != nil {
				return fmt.Errorf("[%s] status %s: %w", c.prefix, folder, serr)
			}
			total = st.Messages
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, total)
		if total == 0 {
			ids = make(map[string]uint32)
			return nil
		}
		c.log("[%s] Fetching %d message IDs from %s...", c.prefix, total, folder)

		ids = make(map[string]uint32, total)
		seqset := new(imap.SeqSet)
		seqset.AddRange(1, total)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		items := []imap.FetchItem{messageIDHeaderSection.FetchItem(), imap.FetchUid}
		go func() { done <- cli.Fetch(seqset, items, messages) }()

		for msg := range messages {
			if ctx.Err() != nil {
				continue
			}
			id := readMessageIDHeader(msg)
			if id == "" {
				missingCount++
				continue
			}
			ids[id] = msg.Uid
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch IDs: %w", c.prefix, err)
		}
		return nil
	})

	if err == nil && missingCount > 0 {
		if pw := c.progressWriter(); pw != nil {
			pw.Log("[%s] ⚠️  %s: %d message(s) without Message-Id will be skipped — sync cannot track them",
				c.prefix, folder, missingCount)
		}
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

// FetchMessageIDSet returns the set of Message-Ids in folder, dropping the
// UIDs. Convenience wrapper around FetchMessageMap for the destination side
// of a sync where UIDs are irrelevant.
func (c *Client) FetchMessageIDSet(ctx context.Context, folder string) (map[string]struct{}, error) {
	m, err := c.FetchMessageMap(ctx, folder)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(m))
	for id := range m {
		out[id] = struct{}{}
	}
	return out, nil
}

// readMessageIDHeader extracts a normalized Message-Id from a fetched message.
// Returns empty string when the section was omitted or unparseable.
func readMessageIDHeader(msg *imap.Message) string {
	body := msg.GetBody(messageIDHeaderSection)
	if body == nil {
		// Some servers reply with a slightly different BodySectionName
		// shape; fall back to scanning all body sections for the header.
		for _, lit := range msg.Body {
			if lit == nil {
				continue
			}
			if id := parseMessageID(lit); id != "" {
				return id
			}
		}
		return ""
	}
	return parseMessageID(body)
}

// parseMessageID reads a literal that should contain just "Message-Id: ..."
// followed by a blank line, and returns the trimmed Message-Id value.
func parseMessageID(lit io.Reader) string {
	raw, err := io.ReadAll(lit)
	if err != nil || len(raw) == 0 {
		return ""
	}
	// net/mail.ReadMessage requires a header/body separator; servers always
	// supply one, but be defensive in case some implementation truncates.
	if !bytes.Contains(raw, []byte("\r\n\r\n")) && !bytes.Contains(raw, []byte("\n\n")) {
		raw = append(raw, '\r', '\n', '\r', '\n')
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	id := m.Header.Get("Message-Id")
	return trimAngleBrackets(id)
}

// trimAngleBrackets returns id without surrounding "<" and ">" without
// allocating, unlike strings.Trim which allocates on the trim path.
func trimAngleBrackets(id string) string {
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		return id[1 : len(id)-1]
	}
	return id
}

// StreamMessagesByUIDs fetches the bodies for the given UIDs in batches and
// invokes onMessage for each as it arrives. The caller is expected to feed in
// UIDs already filtered against the destination — typically the result of a
// Message-Id diff produced from two FetchMessageMap calls.
//
// Streaming avoids materializing every body into memory at once; large
// mailboxes can produce many GB of cumulative body data.
//
// If onMessage returns an error, the channel from the in-flight batch is
// drained (so the producer goroutine exits cleanly) and the error is
// returned without scheduling further batches.
func (c *Client) StreamMessagesByUIDs(ctx context.Context, folder string, uids []uint32, onMessage func(*imap.Message) error) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}

	// Sorted UIDs help imap.SeqSet collapse runs into ranges instead of
	// emitting "1,2,3,4..." over the wire.
	uids = slices.Clone(uids)
	slices.Sort(uids)

	c.log("[%s] Streaming %d messages from %s", c.prefix, len(uids), folder)

	var cbErr error
	for start := 0; start < len(uids); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cbErr != nil {
			return cbErr
		}

		end := min(start+uidFetchBatchSize, len(uids))
		batch := uids[start:end]

		err := c.safeCall(func(cli *imapclient.Client) error {
			// selectIfNeeded short-circuits when the folder is already
			// selected on this connection. After a reconnect inside this
			// safeCall, the generation flip has cleared selectedFolder so
			// we re-Select here.
			if _, err := c.selectIfNeeded(cli, folder); err != nil {
				return fmt.Errorf("[%s] select folder %s: %w", c.prefix, folder, err)
			}
			uidSet := new(imap.SeqSet)
			for _, uid := range batch {
				uidSet.AddNum(uid)
			}
			messages := make(chan *imap.Message, messageChanBuffer)
			batchDone := make(chan error, 1)
			items := []imap.FetchItem{imap.FetchEnvelope, fullBodyPeekSection.FetchItem()}
			go func() { batchDone <- cli.UidFetch(uidSet, items, messages) }()

			for msg := range messages {
				// Once cancelled or the callback errored, just drain so the
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

// FetchMessages retrieves full message envelopes and bodies for a folder.
//
// Deprecated: kept temporarily for compatibility; new code should use
// FetchMessageMap + StreamMessagesByUIDs to avoid pulling all bodies into
// memory at once. Removed in stage 4 of the perf overhaul.
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
		mbox, err := c.selectIfNeeded(cli, folder)
		if err != nil {
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		var total uint32
		if mbox != nil {
			total = mbox.Messages
		} else {
			st, serr := cli.Status(folder, []imap.StatusItem{imap.StatusMessages})
			if serr != nil {
				return fmt.Errorf("[%s] status %s: %w", c.prefix, folder, serr)
			}
			total = st.Messages
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, total)
		if total == 0 {
			return nil
		}
		c.log("[%s] Fetching %d messages from %s...", c.prefix, total, folder)

		seqset := new(imap.SeqSet)
		seqset.AddRange(1, total)
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
				c.log("[%s] Processed %d/%d messages from %s...", c.prefix, count, total, folder)
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
