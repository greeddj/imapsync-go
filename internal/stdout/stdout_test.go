package stdout

import (
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		quiet   bool
		verbose bool
	}{
		{
			name:    "normal mode",
			quiet:   false,
			verbose: false,
		},
		{
			name:    "quiet mode",
			quiet:   true,
			verbose: false,
		},
		{
			name:    "verbose mode",
			quiet:   false,
			verbose: true,
		},
		{
			name:    "quiet and verbose",
			quiet:   true,
			verbose: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.quiet, tt.verbose)

			if s == nil {
				t.Fatal("expected non-nil Spinner")
			}

			if s.quiet != tt.quiet {
				t.Errorf("expected quiet=%v, got %v", tt.quiet, s.quiet)
			}

			if s.verbose != tt.verbose {
				t.Errorf("expected verbose=%v, got %v", tt.verbose, s.verbose)
			}

			if !tt.quiet && s.spin == nil {
				t.Error("expected non-nil spinner in non-quiet mode")
			}

			if tt.quiet && s.spin != nil {
				t.Error("expected nil spinner in quiet mode")
			}

			s.Stop()
		})
	}
}

func TestIsQuiet(t *testing.T) {
	quietSpinner := New(true, false)
	if !quietSpinner.IsQuiet() {
		t.Error("quiet spinner should return true for IsQuiet()")
	}
	quietSpinner.Stop()

	normalSpinner := New(false, false)
	if normalSpinner.IsQuiet() {
		t.Error("normal spinner should return false for IsQuiet()")
	}
	normalSpinner.Stop()
}

func TestSpinnerOperations(t *testing.T) {
	t.Run("quiet mode operations", func(t *testing.T) {
		s := New(true, false)
		defer s.Stop()

		s.Update("test message")
		s.Print("print message")
		s.UpdatePrefix("prefix")
		s.Success("success")
		s.Error("error")
		s.Restart()
		s.Stop()
	})

	t.Run("normal mode operations", func(t *testing.T) {
		s := New(false, false)
		defer s.Stop()

		s.Update("test message")
		s.Print("print message")
		s.UpdatePrefix("prefix")

		s.Stop()
		s.Restart()

		s.Stop()
	})

	t.Run("verbose mode operations", func(t *testing.T) {
		s := New(false, true)
		defer s.Stop()

		s.Update("verbose message")
		s.Print("print in verbose")
		s.Stop()
	})
}

func TestSpinnerStopAndRestart(t *testing.T) {
	s := New(false, false)

	s.Stop()
	s.Restart()
	s.Update("after restart")
	s.Stop()
}
