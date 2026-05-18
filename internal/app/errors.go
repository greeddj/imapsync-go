package app

import "errors"

// ErrSilentExit signals main to exit with a non-zero code without printing
// "Error: ...". Use it when ActionSync has already shown a human-friendly
// summary to stdout and a second copy of the same information through
// stderr would just clutter the screen.
var ErrSilentExit = errors.New("silent exit")
