package client

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// A body that go-imap never read never reached the server, so another
// session can send it without a duplicate.
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

// A flag is written raw into the command line, so a flag that is not an atom could inject commands into the destination session.
func validFlag(flag string) bool {
	atom := strings.TrimPrefix(flag, `\`)
	if atom == "" {
		return false
	}
	for i := 0; i < len(atom); i++ {
		switch b := atom[i]; {
		case b < 0x21 || b > 0x7e:
			return false
		case strings.IndexByte(`(){%*"\]`, b) >= 0:
			return false
		}
	}
	return true
}

// APPEND cannot set \Recent, \Deleted would let an EXPUNGE remove the copy, and a destination can refuse an unknown system flag on every run.
func carriedFlag(flag string) bool {
	if !validFlag(flag) {
		return false
	}
	return !strings.HasPrefix(flag, `\`) ||
		strings.EqualFold(flag, imap.SeenFlag) ||
		strings.EqualFold(flag, imap.AnsweredFlag) ||
		strings.EqualFold(flag, imap.FlaggedFlag) ||
		strings.EqualFold(flag, imap.DraftFlag)
}

// appendFlags returns flags itself when it drops nothing, and nil when no flag remains, so go-imap sends no flag list.
func appendFlags(flags []string) []string {
	if len(flags) == 0 {
		return nil
	}
	if slices.ContainsFunc(flags, func(flag string) bool { return !carriedFlag(flag) }) {
		kept := make([]string, 0, len(flags))
		for _, flag := range flags {
			if carriedFlag(flag) {
				kept = append(kept, flag)
			}
		}
		if len(kept) == 0 {
			return nil
		}
		return kept
	}
	return flags
}

// The source INTERNALDATE is the arrival time; the Date header is only the claim of the sender. A zero result makes go-imap send no date.
func appendDate(m *imap.Message) time.Time {
	if !m.InternalDate.IsZero() {
		return m.InternalDate
	}
	if m.Envelope != nil {
		return m.Envelope.Date
	}
	return time.Time{}
}

// AppendMessage streams the body with no copy, so only a failure before go-imap
// reads it is retried; the next run's Message-Id diff covers the rest.
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
		return fmt.Errorf("[%s] %w", c.prefix, ErrMissingBody)
	}
	body := msg.m.GetBody(fullBodyPeekSection)
	if body == nil {
		return fmt.Errorf("[%s] %w", c.prefix, ErrMissingBody)
	}
	// go-imap turns BODY[] NIL, {0} and quoted strings into an empty body, and an empty APPEND has no Message-Id, so every later run would add one more.
	if body.Len() == 0 {
		return fmt.Errorf("[%s] %w", c.prefix, ErrEmptyBody)
	}

	cli := c.c.Load()
	// After a give-up, or a BYE or closed socket while idle, no session is usable.
	// reconnect logs in again or returns the stored give-up.
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

	date := appendDate(msg.m)
	flags := appendFlags(msg.m.Flags)
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
