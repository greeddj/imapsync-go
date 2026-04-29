// Package config provides configuration management for imapsync.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// Sentinel errors returned by Config.validate. Callers can match on these
// with errors.Is to distinguish missing fields from other failures.
var (
	ErrSrcServerRequired = errors.New("source server is required")
	ErrSrcUserRequired   = errors.New("source user is required")
	ErrSrcPassRequired   = errors.New("source password is required")
	ErrDstServerRequired = errors.New("destination server is required")
	ErrDstUserRequired   = errors.New("destination user is required")
	ErrDstPassRequired   = errors.New("destination password is required")
)

const (
	// minWorkers is the lower bound on the parallel worker count.
	minWorkers = 1
	// maxWorkers is the upper bound on the parallel worker count.
	maxWorkers = 10
	// defaultSourceLabel is the default label for source server.
	defaultSourceLabel = "src"
	// defaultDestLabel is the default label for destination server.
	defaultDestLabel = "dst"
)

// Config holds the entire configuration for the application.
type Config struct {
	Src     Credentials        `json:"src" yaml:"src"`
	Dst     Credentials        `json:"dst" yaml:"dst"`
	Map     []DirectoryMapping `json:"map" yaml:"map"`
	Workers int                `json:"-"   yaml:"-"`
}

// Credentials holds IMAP connection data.
type Credentials struct {
	Label  string `json:"label"  yaml:"label"`  // Human-readable label for the server
	Server string `json:"server" yaml:"server"` // Server address (host:port)
	User   string `json:"user"   yaml:"user"`   // Username
	Pass   string `json:"pass"   yaml:"pass"`   // Password
}

// DirectoryMapping holds source and destination folder names.
type DirectoryMapping struct {
	Source      string `json:"src" yaml:"src"` // Source folder name
	Destination string `json:"dst" yaml:"dst"` // Destination folder name
}

// New loads configuration from the file specified in CLI context.
// It automatically detects the format (JSON or YAML) based on file extension.
// Supported extensions: .json, .yaml, .yml
// It returns an error if the file cannot be read or contains invalid data.
func New(c *cli.Command) (*Config, error) {
	configPath := c.String("config")
	filePath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for config file %q: %w", filePath, err)
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file %q does not exist", filePath)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", filePath, err)
	}

	var cfg Config
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("invalid JSON in config file %q: %w", filePath, err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("invalid YAML in config file %q: %w", filePath, err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file format %q; supported: .json, .yaml, .yml", ext)
	}

	// Set default labels if not provided in config.
	if cfg.Src.Label == "" {
		cfg.Src.Label = defaultSourceLabel
	}

	if cfg.Dst.Label == "" {
		cfg.Dst.Label = defaultDestLabel
	}

	// Clamp worker count to [minWorkers, maxWorkers]. The user-facing default
	// (4) lives on the CLI flag — config only enforces the safe range, so an
	// out-of-range value is corrected without silently falling back to 1.
	cfg.Workers = clampWorkers(c.Int("workers"))

	// Validate required fields.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// clampWorkers bounds the worker count to [minWorkers, maxWorkers].
func clampWorkers(n int) int {
	switch {
	case n < minWorkers:
		return minWorkers
	case n > maxWorkers:
		return maxWorkers
	default:
		return n
	}
}

// validate checks that all required configuration fields are present.
func (c *Config) validate() error {
	if c.Src.Server == "" {
		return ErrSrcServerRequired
	}
	if c.Src.User == "" {
		return ErrSrcUserRequired
	}
	if c.Src.Pass == "" {
		return ErrSrcPassRequired
	}
	if c.Dst.Server == "" {
		return ErrDstServerRequired
	}
	if c.Dst.User == "" {
		return ErrDstUserRequired
	}
	if c.Dst.Pass == "" {
		return ErrDstPassRequired
	}
	return nil
}
