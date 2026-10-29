package client

import (
	"bytes"
	"cmp"
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

// UID returns the UID that the server sent with the message, or 0 for the zero Message.
func (m Message) UID() uint32 {
	if m.m == nil {
		return 0
	}
	return m.m.Uid
}

// MessageRef is one scanned message: its UID and its RFC822.SIZE.
type MessageRef struct {
	UID  uint32
	Size uint32
}

// missingUIDRule sets how a scan treats a response without a UID: the source plans copies by UID and fails, the destination only counts copies and keeps them.
type missingUIDRule uint8

const (
	failOnMissingUID missingUIDRule = iota
	keepMissingUID
)

// FetchMessageMap scans the source: Message-Id -> copies in ascending UID order, and the UIDVALIDITY of the UIDs, or 0 when the server sent none.
func (c *Client) FetchMessageMap(ctx context.Context, folder string) (map[string][]MessageRef, uint32, error) {
	return c.scanMessageIDs(ctx, folder, failOnMissingUID)
}

// FetchMessageCopies returns Message-Id -> copies in folder for the destination side, where only the count and sizes matter. A copy without a UID keeps UID 0 and counts alone.
func (c *Client) FetchMessageCopies(ctx context.Context, folder string) (map[string][]MessageRef, error) {
	ids, _, err := c.scanMessageIDs(ctx, folder, keepMissingUID)
	return ids, err
}

func (c *Client) scanMessageIDs(ctx context.Context, folder string, rule missingUIDRule) (map[string][]MessageRef, uint32, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	c.log("[%s] Fetching folder %s...", c.prefix, folder)

	var (
		ids          map[string][]MessageRef
		missingCount int
		missingUIDs  int
		uidValidity  uint32
		repeated     bool
	)
	err := c.safeCall(func(cli *imapclient.Client) error {
		ids = nil
		uidValidity = 0
		missingCount = 0
		missingUIDs = 0
		repeated = false
		mbox, err := c.selectIfNeeded(cli, folder)
		if err != nil {
			// Only a server answer marks a refusal: a broken session or a throttle
			// stays a folder failure upstream.
			if !canRevive(err) {
				return fmt.Errorf("[%s] %w %s: %w", c.prefix, ErrSelectRefused, folder, err)
			}
			return fmt.Errorf("[%s] cannot select folder %s: %w", c.prefix, folder, err)
		}
		// selectIfNeeded skips the round-trip when the folder is already
		// selected; it returns nil mbox in that case. Fall back to a STATUS
		// for the message count, which is cheap.
		var total uint32
		if mbox != nil {
			total = mbox.Messages
			uidValidity = mbox.UidValidity
		} else {
			// STATUS, not the session copy: it gives the value a fresh EXAMINE sees, so a change since the selection shows.
			st, serr := cli.Status(folder, []imap.StatusItem{imap.StatusMessages, imap.StatusUidValidity})
			if serr != nil {
				return fmt.Errorf("[%s] status %s: %w", c.prefix, folder, serr)
			}
			total = st.Messages
			uidValidity = st.UidValidity
		}
		c.log("[%s] Selected folder %s (%d messages)", c.prefix, folder, total)
		if total == 0 {
			ids = make(map[string][]MessageRef)
			return nil
		}
		c.log("[%s] Fetching %d message IDs from %s...", c.prefix, total, folder)

		ids = make(map[string][]MessageRef, min(total, maxMessageMapHint))
		seqset := new(imap.SeqSet)
		seqset.AddRange(1, total)
		messages := make(chan *imap.Message, messageChanBuffer)
		done := make(chan error, 1)
		// RFC822.SIZE is part of the same FETCH, so the sizes come free with the diff scan.
		items := []imap.FetchItem{messageIDHeaderSection.FetchItem(), imap.FetchUid, imap.FetchRFC822Size}
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
			// go-imap sends UID 0 as "*" in a UID set, so the source cannot plan such a message; the destination only counts copies.
			if msg.Uid == 0 && rule == failOnMissingUID {
				missingUIDs++
				continue
			}
			refs, seen := ids[id]
			if seen {
				repeated = true
			}
			ids[id] = append(refs, MessageRef{UID: msg.Uid, Size: msg.Size})
		}
		if err := <-done; err != nil {
			return fmt.Errorf("[%s] fetch IDs: %w", c.prefix, err)
		}
		if missingUIDs > 0 {
			return fmt.Errorf("[%s] fetch IDs: %w (%d of %d messages)", c.prefix, errMissingUID, missingUIDs, total)
		}
		if repeated {
			sortAndCompactRefs(ids)
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
	return ids, uidValidity, nil
}

// CheckUIDValidity returns ErrUIDValidityChanged when planned is not zero and current differs from it. A session that sends no UIDVALIDITY cannot prove that the planned UIDs still hold.
func CheckUIDValidity(planned, current uint32) error {
	switch {
	case planned == 0 || planned == current:
		return nil
	case current == 0:
		return fmt.Errorf("%w (UIDVALIDITY %d, now none)", ErrUIDValidityChanged, planned)
	default:
		return fmt.Errorf("%w (UIDVALIDITY %d, now %d)", ErrUIDValidityChanged, planned, current)
	}
}

// Sorting once after the FETCH keeps a hostile response order from costing quadratic time. Compacting stops a repeated response from counting one message twice.
func sortAndCompactRefs(ids map[string][]MessageRef) {
	for id, refs := range ids {
		if len(refs) < 2 {
			continue
		}
		slices.SortFunc(refs, func(a, b MessageRef) int { return cmp.Compare(a.UID, b.UID) })
		// Without a UID, a repeated response and a second copy look the same, so refs with UID 0 never merge.
		ids[id] = slices.CompactFunc(refs, func(a, b MessageRef) bool { return a.UID == b.UID && a.UID != 0 })
	}
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

// StreamMessagesByUIDs streams bodies, so no mailbox sits in memory. A session with another UIDVALIDITY, or none, gets no FETCH, and the stream returns ErrUIDValidityChanged; 0 skips the check.
func (c *Client) StreamMessagesByUIDs(ctx context.Context, folder string, uidValidity uint32, uids []uint32, onMessage func(Message) error) error {
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
			status, err := c.selectIfNeeded(cli, folder)
			if err != nil {
				return fmt.Errorf("[%s] select folder %s: %w", c.prefix, folder, err)
			}
			// A nil status means the folder stayed selected and no EXAMINE ran; the session still holds the value from its EXAMINE.
			if status == nil {
				status = cli.Mailbox()
			}
			if status != nil {
				if err := CheckUIDValidity(uidValidity, status.UidValidity); err != nil {
					return fmt.Errorf("[%s] folder %s: %w", c.prefix, folder, err)
				}
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
