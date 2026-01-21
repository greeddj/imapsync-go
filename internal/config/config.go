// Package config provides configuration management for imapsync.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

const (
	// maxWorkers defines the maximum number of parallel workers allowed.
	maxWorkers = 10
	// defaultWorkers is the default number of workers if not specified.
	defaultWorkers = 1
	// defaultSourceLabel is the default label for source server.
	defaultSourceLabel = "src"
	// defaultDestLabel is the default label for destination server.
	defaultDestLabel = "dst"
)

// Config holds the entire configuration for the application.
type Config struct {
	Workers int                `json:"-"   yaml:"-"`   // Number of parallel workers (from CLI)
	Src     Credentials        `json:"src" yaml:"src"` // Source IMAP credentials
	Dst     Credentials        `json:"dst" yaml:"dst"` // Destination IMAP credentials
	Map     []DirectoryMapping `json:"map" yaml:"map"` // Folder mapping rules
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

	// Validate and set worker count.
	workers := c.Int("workers")
	if workers == 0 || workers > maxWorkers {
		cfg.Workers = defaultWorkers
	} else {
		cfg.Workers = workers
	}

	// Validate required fields.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks that all required configuration fields are present.
func (c *Config) validate() error {
	if c.Src.Server == "" {
		return fmt.Errorf("source server is required")
	}
	if c.Src.User == "" {
		return fmt.Errorf("source user is required")
	}
	if c.Src.Pass == "" {
		return fmt.Errorf("source password is required")
	}
	if c.Dst.Server == "" {
		return fmt.Errorf("destination server is required")
	}
	if c.Dst.User == "" {
		return fmt.Errorf("destination user is required")
	}
	if c.Dst.Pass == "" {
		return fmt.Errorf("destination password is required")
	}
	return nil
}
