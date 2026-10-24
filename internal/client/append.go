package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap"
)

// AppendMessage uploads a single message to the destination folder.
//
// The body is streamed straight from the fetched message (which already holds
// it in memory) into the APPEND literal — no extra io.ReadAll buffer. The
// trade-off is that the literal is not replayable: a transient network error
// mid-APPEND cannot be retried, because the body has been partially sent.
// That's acceptable for our flow: the next sync run is idempotent (the
// Message-Id diff will pick up the message again), and avoiding the second
// in-memory copy halves peak RAM with multi-MB attachments and 10 workers.
func (c *Client) AppendMessage(ctx context.Context, folder string, msg Message) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}
	if c.isCancelled() {
		return context.Canceled
	}

	if msg.m == nil {
		return fmt.Errorf("[%s] message has no body", c.prefix)
	}
	body := msg.m.GetBody(fullBodyPeekSection)
	if body == nil {
		return fmt.Errorf("[%s] message has no body", c.prefix)
	}

	cli := c.c.Load()
	if cli == nil {
		return errors.New("imap client not connected")
	}

	// A server may omit ENVELOPE; the zero date makes go-imap leave the
	// optional APPEND date out, so the destination stamps its own.
	var date time.Time
	if env := msg.m.Envelope; env != nil {
		date = env.Date
	}
	flags := []string{imap.SeenFlag}
	if err := cli.Append(folder, flags, date, body); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The literal has been partially consumed, so we can't retry THIS
		// message — but repair the connection so the next APPEND in the
		// same plan doesn't fail again at the IMAP layer. Without this,
		// one server-side disconnect cascades into "Not logged in" for
		// every remaining message in the folder.
		if isRetryable(err) && !c.isCancelled() {
			_ = c.reconnect()
		}
		return fmt.Errorf("[%s] append: %w", c.prefix, err)
	}
	if c.verbose {
		c.log("[%s] Message %q appended to %s", c.prefix, msg.MessageID(), folder)
	}
	return nil
}
