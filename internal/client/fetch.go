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
	"github.com/greeddj/imapsync-go/internal/termsafe"
)

// messageIDHeaderSection is the smallest fetch we can ask for that still
// gives us what we need for diffing: just the Message-Id header plus the
// terminating CRLF, with PEEK so the source's \Seen flag is unchanged.
//
// Every Fetch method below uses this section instead of FetchEnvelope. On
// large folders this is dramatically less data on the wire - Envelope also
// pulls From/To/Cc/Bcc/Subject/Date/References/InReplyTo, which we don't
// need for the planning diff.
var messageIDHeaderSection = &imap.BodySectionName{
	Specifier: imap.HeaderSpecifier,
	Fields:    []string{"Message-Id"},
	Peek:      true,
}

// fullBodyPeekSection requests the entire RFC822 body without flipping the
// \Seen flag on the source. This matters: a sync tool must not mutate the
// source mailbox state. The previous implementation used FetchRFC822 which
// is functionally equivalent to BODY[] (RFC 3501 §6.4.5) and *does* mark
// messages as read.
var fullBodyPeekSection = &imap.BodySectionName{Peek: true}

// Message is one message fetched by StreamMessagesByUIDs. It hides the go-imap
// type so internal/app never handles *imap.* values directly.
type Message struct{ m *imap.Message }

// MessageID returns the envelope Message-Id, or "" when the server sent no
// envelope.
func (m Message) MessageID() string {
	if m.m == nil || m.m.Envelope == nil {
		return ""
	}
	return m.m.Envelope.MessageId
}

// FetchMessageMap returns Message-Id → UID for every message in folder, plus
// the sum of RFC822.SIZE across all messages.
//
// One pass over the folder yields all three pieces. Messages without a usable
// Message-Id are counted and reported once via the progress writer - without
// that header the diff has no key to match on, so they cannot be tracked
// across servers and will be silently skipped. The folder-wide size total is
// used by callers (sync preview) to estimate transfer volume; messages without
// a Message-Id still contribute to the total because they exist on the wire.
//
// The returned map is suitable both as the source side of a Message-Id diff
// and (with UIDs ignored) as the destination side; callers that only need
// the keys can use FetchMessageIDSet for a slightly thinner allocation.
func (c *Client) FetchMessageMap(ctx context.Context, folder string) (map[string]uint32, uint64, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	var (
		ids          map[string]uint32
		totalSize    uint64
		missingCount int
	)
	err := c.safeCall(func(cli *imapclient.Client) error {
		ids = nil
		totalSize = 0
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

		ids = make(map[string]uint32, min(total, maxMessageMapHint))
		seqset := new(imap.SeqSet)
		seqset.AddRange(1, total)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		// RFC822.SIZE is part of the same FETCH so the size total comes
		// free with the diff scan - no extra round-trip per folder.
		items := []imap.FetchItem{messageIDHeaderSection.FetchItem(), imap.FetchUid, imap.FetchRFC822Size}
		go func() { done <- cli.Fetch(seqset, items, messages) }()

		for msg := range messages {
			if ctx.Err() != nil {
				continue
			}
			totalSize += uint64(msg.Size)
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
			pw.Log("[%s] ⚠️  %s: %d message(s) without Message-Id will be skipped - sync cannot track them",
				c.prefix, termsafe.Escape(folder), missingCount)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return ids, totalSize, nil
}

// FetchMessageIDSet returns the set of Message-Ids in folder, dropping the
// UIDs. Convenience wrapper around FetchMessageMap for the destination side
// of a sync where UIDs are irrelevant.
func (c *Client) FetchMessageIDSet(ctx context.Context, folder string) (map[string]struct{}, error) {
	m, _, err := c.FetchMessageMap(ctx, folder)
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
// UIDs already filtered against the destination - typically the result of a
// Message-Id diff produced from two FetchMessageMap calls.
//
// Streaming avoids materializing every body into memory at once; large
// mailboxes can produce many GB of cumulative body data.
//
// If onMessage returns an error, the channel from the in-flight batch is
// drained (so the producer goroutine exits cleanly) and the error is
// returned without scheduling further batches.
func (c *Client) StreamMessagesByUIDs(ctx context.Context, folder string, uids []uint32, onMessage func(Message) error) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}

	// Sorted so imap.SeqSet collapses runs into ranges, and so the retry
	// bookkeeping can binary-search each batch.
	uids = slices.Clone(uids)
	slices.Sort(uids)

	c.log("[%s] Streaming %d messages from %s", c.prefix, len(uids), folder)

	delivered := make([]bool, min(len(uids), uidFetchBatchSize))
	var cbErr error
	for start := 0; start < len(uids); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := min(start+uidFetchBatchSize, len(uids))
		batch := uids[start:end]
		seen := delivered[:len(batch)]
		clear(seen)

		err := c.safeCall(func(cli *imapclient.Client) error {
			// A retry re-fetches only UIDs whose body was not yet handed to
			// onMessage, so the destination gets no duplicate APPEND.
			uidSet := new(imap.SeqSet)
			for i, uid := range batch {
				if !seen[i] {
					uidSet.AddNum(uid)
				}
			}
			if uidSet.Empty() {
				return nil
			}
			// A retry after a reconnect runs on a fresh session, which has no
			// selection, so selectIfNeeded selects the folder again.
			if _, err := c.selectIfNeeded(cli, folder); err != nil {
				return fmt.Errorf("[%s] select folder %s: %w", c.prefix, folder, err)
			}
			messages := make(chan *imap.Message, messageChanBuffer)
			batchDone := make(chan error, 1)
			items := []imap.FetchItem{imap.FetchEnvelope, fullBodyPeekSection.FetchItem()}
			go func() { batchDone <- cli.UidFetch(uidSet, items, messages) }()

			w := c.idle.Load()
			for msg := range messages {
				// Once cancelled or the callback errored, just drain so the
				// producer goroutine can exit and we don't leak it.
				if ctx.Err() != nil || cbErr != nil {
					continue
				}
				i, found := slices.BinarySearch(batch, msg.Uid)
				if !found || seen[i] {
					continue
				}
				// Only a response with the body can become an APPEND, so only it
				// marks the UID. Read before onMessage, which may consume the body.
				hasBody := msg.GetBody(fullBodyPeekSection) != nil
				// onMessage is our own work, usually an APPEND. At a batch
				// tail the source is done and silent, so that time must not count.
				w.end()
				e := onMessage(Message{m: msg})
				w.begin()
				if hasBody {
					seen[i] = true
				}
				if e != nil {
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
