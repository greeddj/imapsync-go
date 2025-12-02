// Package stdout provides progress reporting using a terminal spinner.
package stdout

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
)

// labelRegex matches messages starting with [label] prefix
var labelRegex = regexp.MustCompile(`^\[([^\]]+)\]\s*(.*)$`)

// Spinner wraps spinner.Spinner with additional methods for progress reporting.
// It implements a simple progress reporter interface with spinner visualization.
// Supports concurrent updates from multiple goroutines, displaying all active
// operations in a combined status line.
type Spinner struct {
	spin    *spinner.Spinner  // The underlying spinner instance.
	quiet   bool              // If true, suppresses all output.
	verbose bool              // If true, prints verbose messages.
	mu      sync.Mutex        // Protects concurrent access.
	lanes   map[string]string // Active status messages by label.
}

// New creates a new Spinner instance.
// If quiet is true, no output will be displayed.
func New(quiet, verbose bool) *Spinner {
	s := &Spinner{
		quiet:   quiet,
		verbose: verbose,
		lanes:   make(map[string]string),
	}
	if !quiet {
		s.spin = spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithColor("green"))
		s.spin.Start()
	}
	return s
}

// Update sets the spinner's current message.
// Messages with [label] prefix are tracked separately and displayed together.
func (s *Spinner) Update(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		if s.verbose {
			fmt.Printf("\r%s\n", message)
		} else {
			// Parse label from message like "[old] Fetching..."
			if matches := labelRegex.FindStringSubmatch(message); matches != nil {
				label := matches[1]
				content := matches[2]
				s.lanes[label] = content
				s.updateSuffix()
			} else {
				// No label - show as single message, clear lanes
				s.lanes = make(map[string]string)
				s.spin.Suffix = " " + message
			}
		}
	}
}

// updateSuffix combines all active lanes into a single status line.
// Must be called with mutex held.
func (s *Spinner) updateSuffix() {
	if len(s.lanes) == 0 {
		s.spin.Suffix = ""
		return
	}

	// Sort labels for consistent ordering
	labels := make([]string, 0, len(s.lanes))
	for label := range s.lanes {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	// Build combined status
	var parts []string
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("[%s] %s", label, s.lanes[label]))
	}

	s.spin.Suffix = " " + strings.Join(parts, " | ")
}

// Flush prints the current status as a permanent line and clears lanes.
// Call this when a phase/step completes to preserve progress in scroll history.
func (s *Spinner) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil && len(s.lanes) > 0 {
		// Build the final status line
		labels := make([]string, 0, len(s.lanes))
		for label := range s.lanes {
			labels = append(labels, label)
		}
		sort.Strings(labels)

		var parts []string
		for _, label := range labels {
			parts = append(parts, fmt.Sprintf("[%s] %s", label, s.lanes[label]))
		}

		// Print as permanent line
		fmt.Printf("\r✓ %s\n", strings.Join(parts, " | "))

		// Clear lanes and reset suffix
		s.lanes = make(map[string]string)
		s.spin.Suffix = ""
	}
}

// ClearLane removes a specific label from the active lanes.
func (s *Spinner) ClearLane(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lanes, label)
	if s.spin != nil {
		s.updateSuffix()
	}
}

// Print writes a raw message without changing the spinner suffix logic.
func (s *Spinner) Print(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		fmt.Printf("\r%s\n", message)
	}
}

// UpdatePrefix sets the spinner's prefix.
func (s *Spinner) UpdatePrefix(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		s.spin.Prefix = prefix + " "
	}
}

// Success stops the spinner with a success message.
func (s *Spinner) Success(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		s.lanes = make(map[string]string)
		s.spin.FinalMSG = "✅ " + message + "\n"
		s.spin.Stop()
	}
}

// Error stops the spinner with an error message.
func (s *Spinner) Error(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		s.lanes = make(map[string]string)
		s.spin.FinalMSG = "❌ " + message + "\n"
		s.spin.Stop()
	}
}

// Stop stops the spinner.
func (s *Spinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		s.spin.Stop()
	}
}

// Restart restarts the spinner animation when it was previously stopped.
func (s *Spinner) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.quiet && s.spin != nil {
		s.lanes = make(map[string]string)
		s.spin.Restart()
	}
}

// IsQuiet returns true if quiet mode is enabled.
func (s *Spinner) IsQuiet() bool {
	return s.quiet
}
