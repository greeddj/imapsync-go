package app

import "errors"

// ErrSilentExit signals main to exit with a non-zero code without printing
// "Error: ...". Use it when ActionSync has already shown a human-friendly
// summary to stdout and a second copy of the same information through
// stderr would just clutter the screen.
var ErrSilentExit = errors.New("silent exit")

// errQuietWithoutConfirm keeps plan-confirm-execute: quiet mode prints
// neither the plan nor the prompt, so only an explicit --confirm may
// authorise writes.
var errQuietWithoutConfirm = errors.New("--quiet hides the sync plan and its confirmation prompt, so it requires --confirm (-y)")

// errDestinationNoSelect fails one mapping alone: a destination that LIST marks
// \Noselect cannot receive messages, and the rest of the sync still runs.
var errDestinationNoSelect = errors.New(`destination folder cannot hold messages (\Noselect)`)

// errSourceScan fails one mapping alone: the rest of the sync still runs, and the run ends non-zero.
var errSourceScan = errors.New("cannot scan source folder")
