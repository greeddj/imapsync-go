package client

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// readTracker records whether go-imap read the literal. A body that go-imap never
// read never reached the server, so another session can send it without a duplicate.
type readTracker struct {
	imap.Literal
	read bool
}

func (r *readTracker) Read(p []byte) (int, error) {
	r.read = true
	return r.Literal.Read(p)
}

// sessionEnded reports whether the reader of cli stopped, after a BYE or a closed socket.
// An APPEND on such a session gets no continuation and can block forever.
func sessionEnded(cli *imapclient.Client) bool {
	select {
	case <-cli.LoggedOut():
		return true
	default:
		return false
	}
}

// AppendMessage streams the body with no copy. A failure before go-imap reads it
// gets one reconnect and one retry. A later failure is left to the next run's Message-Id diff.
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
	// An earlier reconnect gave up, or the server ended the session while it was idle.
	// Either way reconnect decides: it logs in again or returns the stored give-up.
	reconnected := cli == nil || sessionEnded(cli)
	if reconnected {
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
	literal := &readTracker{Literal: body}
	send := func(cli *imapclient.Client) error { return cli.Append(folder, flags, date, literal) }

	err := c.call(cli, send)
	if err != nil && isRetryable(err) && !literal.read && !reconnected && ctx.Err() == nil && !c.isCancelled() {
		c.noteIdleTrip(err)
		if cli, err = c.reconnectSession(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		reconnected = true
		err = c.call(cli, send)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// go-imap read part of the body, so this message cannot be retried. Repair the
		// session so the next APPEND does not fail too, unless this call already reconnected.
		if isRetryable(err) && !reconnected && !c.isCancelled() {
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
