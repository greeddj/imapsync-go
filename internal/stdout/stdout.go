// Package stdout provides progress reporting using a terminal spinner.
package stdout

import (
	"fmt"
	"time"

	"github.com/briandowns/spinner"
)

// Spinner wraps spinner.Spinner with additional methods for progress reporting.
// It implements a simple progress reporter interface with spinner visualization.
type Spinner struct {
	spin    *spinner.Spinner // The underlying spinner instance.
	quiet   bool             // If true, suppresses all output.
	verbose bool             // If true, prints verbose messages.
}

// New creates a new Spinner instance.
// If quiet is true, no output will be displayed.
func New(quiet, verbose bool) *Spinner {
	s := &Spinner{
		quiet:   quiet,
		verbose: verbose,
	}
	if !quiet {
		s.spin = spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithColor("green"))
		s.spin.Start()
	}
	return s
}

// Update sets the spinner's current message.
func (s *Spinner) Update(message string) {
	if !s.quiet && s.spin != nil {
		if s.verbose {
			fmt.Printf("\r%s\n", message)
		} else {
			s.spin.Suffix = " " + message
		}
	}
}

// Print writes a raw message without changing the spinner suffix logic.
func (s *Spinner) Print(message string) {
	if !s.quiet && s.spin != nil {
		fmt.Printf("\r%s\n", message)
	}
}

// UpdatePrefix sets the spinner's prefix.
func (s *Spinner) UpdatePrefix(prefix string) {
	if !s.quiet && s.spin != nil {
		s.spin.Prefix = prefix + " "
	}
}

// Success stops the spinner with a success message.
func (s *Spinner) Success(message string) {
	if !s.quiet && s.spin != nil {
		s.spin.FinalMSG = "✅ " + message + "\n"
		s.spin.Stop()
	}
}

// Error stops the spinner with an error message.
func (s *Spinner) Error(message string) {
	if !s.quiet && s.spin != nil {
		s.spin.FinalMSG = "❌ " + message + "\n"
		s.spin.Stop()
	}
}

// Stop stops the spinner.
func (s *Spinner) Stop() {
	if !s.quiet && s.spin != nil {
		s.spin.Stop()
	}
}

// Restart restarts the spinner animation when it was previously stopped.
func (s *Spinner) Restart() {
	if !s.quiet && s.spin != nil {
		s.spin.Restart()
	}
}

// IsQuiet returns true if quiet mode is enabled.
func (s *Spinner) IsQuiet() bool {
	return s.quiet
}
