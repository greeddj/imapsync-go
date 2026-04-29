package client

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// AppendMessage uploads a single message to the destination folder.
func (c *Client) AppendMessage(ctx context.Context, folder string, msg *imap.Message) error {
	stop := c.withCancel(ctx)
	defer stop()

	if err := ctx.Err(); err != nil {
		return err
	}

	body := msg.GetBody(&imap.BodySectionName{})
	if body == nil {
		return fmt.Errorf("[%s] message has no body", c.prefix)
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("[%s] read body: %w", c.prefix, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	flags := []string{imap.SeenFlag}
	date := msg.Envelope.Date

	err = c.safeCall(func(cli *imapclient.Client) error {
		return cli.Append(folder, flags, date, bytes.NewReader(raw))
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("[%s] append: %w", c.prefix, err)
	}
	if c.verbose {
		c.log("[%s] Message %q appended to %s", c.prefix, msg.Envelope.MessageId, folder)
	}
	return nil
}
