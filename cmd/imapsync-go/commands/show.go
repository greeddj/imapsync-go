// Package commands implements CLI subcommands for imapsync-go.
package commands

import (
	"github.com/greeddj/imapsync-go/internal/app"
	"github.com/urfave/cli/v3"
)

// Show returns the "show" subcommand definition.
func Show() *cli.Command {
	return &cli.Command{
		Name:   "show",
		Usage:  "show IMAP dirs in source and destination servers",
		Action: app.ActionShow,
	}
}
