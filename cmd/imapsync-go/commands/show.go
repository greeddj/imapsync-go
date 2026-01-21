// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"context"

	"github.com/greeddj/imapsync-go/internal/app"
	"github.com/urfave/cli/v3"
)

func Show() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "show IMAP dirs in source and destination servers",
		Action: func(ctx context.Context, c *cli.Command) error {
			return app.ActionShow(ctx, c)
		},
	}
}
