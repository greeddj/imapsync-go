package client

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-imap"
)

// AppendMessage uploads a single message to the destination folder.
//
// The body is streamed straight from the fetched message (which already holds
// it in memory) into the APPEND literal - no extra io.ReadAll buffer. The
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
	revived := cli == nil
	if revived {
		// An earlier reconnect gave up, and the server can be back.
		var err error
		if cli, err = c.reconnectSession(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}

	// A server may omit ENVELOPE; the zero date makes go-imap leave the
	// optional APPEND date out, so the destination stamps its own.
	var date time.Time
	if env := msg.m.Envelope; env != nil {
		date = env.Date
	}
	flags := []string{imap.SeenFlag}
	w := c.idle.Load()
	w.begin()
	err := cli.Append(folder, flags, date, body)
	w.end()
	if err = w.idleErr(err); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The literal is partly sent, so this message cannot be retried. Repair the
		// session so the next APPEND does not fail too, unless this call already reconnected.
		if isRetryable(err) && !revived && !c.isCancelled() {
			c.noteIdleTrip(err)
			_ = c.reconnect()
		}
		return fmt.Errorf("[%s] append: %w", c.prefix, err)
	}
	if c.verbose {
		c.log("[%s] Message %q appended to %s", c.prefix, msg.MessageID(), folder)
	}
	return nil
}
