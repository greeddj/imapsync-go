package app

import (
	"fmt"

	"github.com/urfave/cli/v3"
)

// rejectPositionalArgs refuses operands the command does not define. urfave/cli
// parses unrecognised operands into Args() and hands them to the action instead
// of erroring, so without this guard a typo reaches the servers: `sync typo`
// connects to both accounts and copies mail. cli/v3 v3.12.0 also stopped
// resolving `sync help` to an implicit help subcommand, which used to absorb at
// least that one spelling before it got that far.
func rejectPositionalArgs(c *cli.Command) error {
	if c.Args().Len() == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q, run %q for usage", c.Args().First(), c.FullName()+" --help")
}
