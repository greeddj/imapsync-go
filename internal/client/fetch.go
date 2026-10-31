package client

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
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

var errFetchNotEnoughFields = errors.New("fetch response has no message data")

// bodyFetchHandler is responses.Fetch for a UID FETCH that keeps the flag spelling of the server. Like responses.Fetch, it sends each message on the reader goroutine.
type bodyFetchHandler struct {
	messages chan<- *imap.Message
	uids     *imap.SeqSet
}

func (h *bodyFetchHandler) Handle(resp imap.Resp) error {
	name, fields, ok := imap.ParseNamedResp(resp)
	if !ok || name != "FETCH" {
		return responses.ErrUnhandled
	}
	if len(fields) < 2 {
		return errFetchNotEnoughFields
	}
	seqNum, err := imap.ParseNumber(fields[0])
	if err != nil {
		return err
	}
	list, ok := fields[1].([]any)
	if !ok {
		return errFetchNotEnoughFields
	}
	msg := &imap.Message{SeqNum: seqNum}
	if err := msg.Parse(list); err != nil {
		return err
	}
	// A response with no UID, or with a UID that was not requested, is unilateral data.
	if msg.Uid == 0 || !h.uids.Contains(msg.Uid) {
		return responses.ErrUnhandled
	}
	restoreFlagSpelling(msg, list)
	h.messages <- msg
	return nil
}

// go-imap lower-cases keywords, and the copy must keep the spelling of the source. A literal or NIL item keeps the parsed value.
func restoreFlagSpelling(msg *imap.Message, list []any) {
	var raw []any
	for i := 0; i+1 < len(list); i += 2 {
		if key, ok := atomText(list[i]); ok && strings.EqualFold(key, string(imap.FetchFlags)) {
			raw, _ = list[i+1].([]any)
		}
	}
	for i, field := range raw[:min(len(raw), len(msg.Flags))] {
		if text, ok := atomText(field); ok {
			msg.Flags[i] = text
		}
	}
}

// atomText returns the text of an atom or a quoted string, which go-imap reads as a string.
func atomText(field any) (string, bool) {
	switch f := field.(type) {
	case string:
		return f, true
	case imap.RawString:
		return string(f), true
	default:
		return "", false
	}
}

// uidFetchBodies is UidFetch with bodyFetchHandler. Like go-imap, it closes messages only after Execute returns and sends no command outside the selected state.
func uidFetchBodies(cli *imapclient.Client, uids *imap.SeqSet, items []imap.FetchItem, messages chan<- *imap.Message) error {
	defer close(messages)

	if cli.State() != imap.SelectedState {
		return imapclient.ErrNoMailboxSelected
	}
	status, err := cli.Execute(
		&commands.Uid{Cmd: &commands.Fetch{SeqSet: uids, Items: items}},
		&bodyFetchHandler{messages: messages, uids: uids},
	)
	if err != nil {
		return err
	}
	return status.Err()
}

// selectChecked selects folder when the session has not selected it, then compares the UIDVALIDITY of the session with uidValidity. 0 skips the comparison.
func (c *Client) selectChecked(cli *imapclient.Client, folder string, uidValidity uint32) error {
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
	return nil
}

// uidSearchIDs runs UID SEARCH UID with no charset, because a server without UTF-8 support would refuse one. Like uidFetchBodies, it sends nothing outside the selected state.
func uidSearchIDs(cli *imapclient.Client, uids *imap.SeqSet) ([]uint32, error) {
	if cli.State() != imap.SelectedState {
		return nil, imapclient.ErrNoMailboxSelected
	}
	res := new(responses.Search)
	status, err := cli.Execute(&commands.Uid{Cmd: &commands.Search{Criteria: &imap.SearchCriteria{Uid: uids}}}, res)
	if err != nil {
		return nil, err
	}
	if err := status.Err(); err != nil {
		return nil, err
	}
	return res.Ids, nil
}

// ExistingUIDs returns the UIDs of uids that folder still holds, ascending, unique and never more than asked. It reads no body. A UIDVALIDITY mismatch returns ErrUIDValidityChanged; 0 skips.
func (c *Client) ExistingUIDs(ctx context.Context, folder string, uidValidity uint32, uids []uint32) ([]uint32, error) {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// UID 0 would go out as "*", which selects the largest UID.
	asked := slices.DeleteFunc(slices.Clone(uids), func(uid uint32) bool { return uid == 0 })
	slices.Sort(asked)
	asked = slices.Compact(asked)
	if len(asked) == 0 {
		return nil, nil
	}

	held := make([]uint32, 0, len(asked))
	hits := make([]bool, min(len(asked), uidFetchBatchSize))
	for start := 0; start < len(asked); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		chunk := asked[start:min(start+uidFetchBatchSize, len(asked))]
		found := hits[:len(chunk)]
		err := c.safeCall(func(cli *imapclient.Client) error {
			clear(found)
			if err := c.selectChecked(cli, folder, uidValidity); err != nil {
				return err
			}
			set := new(imap.SeqSet)
			set.AddNum(chunk...)
			named, err := uidSearchIDs(cli, set)
			if err != nil {
				return fmt.Errorf("[%s] check UIDs in %s: %w", c.prefix, folder, err)
			}
			// A server can name UIDs that nobody asked for, and repeat them, so only the index in chunk counts.
			for _, uid := range named {
				if i, ok := slices.BinarySearch(chunk, uid); ok {
					found[i] = true
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		for i, uid := range chunk {
			if found[i] {
				held = append(held, uid)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return held, nil
}

// StreamMessagesByUIDs feeds onMessage, which must not call c. A halt or onMessage error ends the stream unchanged and closes a busy session. A nil halt and uidValidity 0 skip checks.
func (c *Client) StreamMessagesByUIDs(ctx context.Context, folder string, uidValidity uint32, uids []uint32, halt func() error, onMessage func(Message) error) error {
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
	var (
		cbErr  error
		failed *StreamError
	)
	for start := 0; start < len(uids); start += uidFetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if halt != nil {
			if err := halt(); err != nil {
				return err
			}
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
			// A retry runs on a new session, so a stop since the batch began must send no UID FETCH.
			if halt != nil {
				if err := halt(); err != nil {
					cbErr = err
					return errStreamStopped
				}
			}
			if err := c.selectChecked(cli, folder, uidValidity); err != nil {
				return err
			}
			// No buffer: go-imap holds each body fully in memory, so every slot would keep one more message per worker.
			messages := make(chan *imap.Message)
			batchDone := make(chan error, 1)
			items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, fullBodyPeekSection.FetchItem()}
			go func() { batchDone <- uidFetchBodies(cli, uidSet, items, messages) }()

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
				// A flag update has no body. Skipping it leaves the UID unseen, so the full response is still delivered and a retry still fetches it.
				if msg.GetBody(fullBodyPeekSection) == nil {
					continue
				}
				// onMessage is our own work, usually an APPEND. At a batch
				// tail the source is done and silent, so that time must not count.
				w.end()
				e := onMessage(Message{m: msg})
				w.begin()
				seen[i] = true
				if e != nil {
					cbErr = e
					// go-imap cannot end a FETCH in flight, so the drain would download every body left in the batch.
					if slices.Contains(seen, false) {
						_ = cli.Terminate()
					}
				}
			}
			berr := <-batchDone
			// After a caller stop the batch error proves nothing, and a retry would fetch again.
			if cbErr != nil {
				return errStreamStopped
			}
			if berr != nil {
				return fmt.Errorf("[%s] body fetch: %w", c.prefix, berr)
			}
			return nil
		})
		if cerr := c.cancelErr(ctx); cerr != nil {
			return cerr
		}
		if cbErr != nil {
			return cbErr
		}
		if err == nil {
			continue
		}
		if failed == nil {
			failed = &StreamError{First: err}
		}
		failed.FailedBatches++
		// A refused batch costs only its own messages. A UIDVALIDITY change makes the planned UIDs unsafe, and more commands after a throttled reply extend the limit.
		if errors.Is(err, ErrUIDValidityChanged) || classifyError(err) == ClassThrottled {
			failed.Stop = err
			return failed
		}
	}
	if failed != nil {
		return failed
	}
	return nil
}
