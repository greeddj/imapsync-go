package app

import "errors"

// ErrSilentExit makes main exit with a non-zero code and print no "Error: ..."
// line. Return it when a command already printed its failures to stdout.
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
