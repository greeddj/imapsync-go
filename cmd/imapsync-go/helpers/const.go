// Package helpers contains small CLI-side utilities shared between the entry
// point and subcommand wiring.
package helpers

const (
	defaultVersion   = "latest"
	defaultBuilder   = "go"
	userAgent        = "imapsync-go"
	latestVersionURL = "https://api.github.com/repos/greeddj/imapsync-go/releases/latest"
)
