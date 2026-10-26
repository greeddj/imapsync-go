// Package config provides configuration management for imapsync.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// Sentinel errors returned by Config.validate. Callers can match on these
// with errors.Is to distinguish missing or unsupported fields from other failures.
var (
	ErrSrcServerRequired = errors.New("source server is required")
	ErrSrcUserRequired   = errors.New("source user is required")
	ErrSrcPassRequired   = errors.New("source password is required")
	ErrDstServerRequired = errors.New("destination server is required")
	ErrDstUserRequired   = errors.New("destination user is required")
	ErrDstPassRequired   = errors.New("destination password is required")
	ErrMapSrcRequired    = errors.New(`"src" folder name is required`)
	ErrMapDstRequired    = errors.New(`"dst" folder name is required`)
	ErrAuthUnsupported   = errors.New("unsupported auth mechanism")
)

var errTrailingContent = errors.New("unexpected content after the first document")

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
	Src       Credentials        `json:"src"        yaml:"src"`
	Dst       Credentials        `json:"dst"        yaml:"dst"`
	Map       []DirectoryMapping `json:"map"        yaml:"map"`
	RateLimit RateLimit          `json:"rate_limit" yaml:"rate_limit"`
	Workers   int                `json:"-"          yaml:"-"`
}

// RateLimit caps client-side throughput. Zero values mean "unlimited" and the
// corresponding limiter is not constructed at all.
//
// MaxConnections caps the simultaneous IMAP sessions per side. Many providers
// enforce this server-side (Gmail = 15) - exceeding it earns a temporary ban.
type RateLimit struct {
	DownBPS        int `json:"down_bps"        yaml:"down_bps"`
	UpBPS          int `json:"up_bps"          yaml:"up_bps"`
	MaxConnections int `json:"max_connections" yaml:"max_connections"`
}

// Credentials holds IMAP connection data.
type Credentials struct {
	Label  string `json:"label"  yaml:"label"`  // Human-readable label for the server
	Server string `json:"server" yaml:"server"` // Server address (host:port)
	User   string `json:"user"   yaml:"user"`   // Username
	Pass   string `json:"pass"   yaml:"pass"`   // Password
	Auth   string `json:"auth"   yaml:"auth"`   // [ "", "login", "cram-md5" ] Default is login ("").
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
		if err := decodeJSON(data, &cfg); err != nil {
			return nil, fmt.Errorf("invalid JSON in config file %q: %w", filePath, err)
		}
	case ".yaml", ".yml":
		if err := decodeYAML(data, &cfg); err != nil {
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
	// (4) lives on the CLI flag - config only enforces the safe range, so an
	// out-of-range value is corrected without silently falling back to 1.
	cfg.Workers = clampWorkers(c.Int("workers"))

	// CLI flags take precedence when the user explicitly sets them; config
	// values fill in only the unset fields. We use 0 as "unset" - that also
	// happens to be the "unlimited" sentinel, which is fine: a config-set
	// value of 0 means the same thing.
	if v := c.Int("bps-down"); v != 0 {
		cfg.RateLimit.DownBPS = v
	}
	if v := c.Int("bps-up"); v != 0 {
		cfg.RateLimit.UpBPS = v
	}
	if v := c.Int("max-connections"); v != 0 {
		cfg.RateLimit.MaxConnections = v
	}

	// Validate required fields.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// decodeJSON rejects unknown keys: a misspelled map or rate_limit key would
// otherwise be dropped and change what is synced.
func decodeJSON(data []byte, cfg *Config) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	// The Decoder reads only the first value, so look for anything after it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errTrailingContent
	}
	return nil
}

// decodeYAML rejects unknown keys: a misspelled map or rate_limit key would
// otherwise be dropped and change what is synced.
func decodeYAML(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(cfg)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return err
	}
	// The Decoder reads only the first document, so look for another one.
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return errTrailingContent
	}
	return restoreNullMapEntries(data, cfg)
}

// yaml.v3 drops a null sequence item. Keep it as an empty entry, so validate
// rejects it and reports its position in the file.
func restoreNullMapEntries(data []byte, cfg *Config) error {
	var raw struct {
		Map []*DirectoryMapping `yaml:"map"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Map) == len(cfg.Map) {
		return nil
	}
	cfg.Map = make([]DirectoryMapping, len(raw.Map))
	for i, p := range raw.Map {
		if p != nil {
			cfg.Map[i] = *p
		}
	}
	return nil
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

// checkAuth lowers the value the way the client does, so both accept the same
// inputs. The error carries the value, never a password.
func checkAuth(side, auth string) error {
	switch strings.ToLower(auth) {
	case "", "login", "cram-md5":
		return nil
	}
	return fmt.Errorf("%s: %w %q, use \"login\" or \"cram-md5\"", side, ErrAuthUnsupported, auth)
}

// validate checks the required fields and the values of auth and map items.
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
	if err := checkAuth("src", c.Src.Auth); err != nil {
		return err
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
	if err := checkAuth("dst", c.Dst.Auth); err != nil {
		return err
	}
	for i := range c.Map {
		if c.Map[i].Source == "" {
			return fmt.Errorf("map entry %d: %w", i+1, ErrMapSrcRequired)
		}
		if c.Map[i].Destination == "" {
			return fmt.Errorf("map entry %d: %w", i+1, ErrMapDstRequired)
		}
	}
	return nil
}
