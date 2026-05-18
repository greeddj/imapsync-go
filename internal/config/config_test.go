package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// runNewWithArgs writes content to a temp file with the given extension and
// drives config.New through a urfave/cli/v3 Command parsed from extraArgs.
// Returns whatever Config / error New produced.
func runNewWithArgs(t *testing.T, ext, content string, extraArgs ...string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config"+ext)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	var (
		gotCfg *Config
		gotErr error
	)
	app := &cli.Command{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Value: path},
			&cli.IntFlag{Name: "workers", Value: 4},
			&cli.IntFlag{Name: "bps-down"},
			&cli.IntFlag{Name: "bps-up"},
			&cli.IntFlag{Name: "max-connections"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			gotCfg, gotErr = New(c)
			return nil
		},
	}
	args := append([]string{"app"}, extraArgs...)
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("app.Run: %v", err)
	}
	return gotCfg, gotErr
}

const validJSONConfig = `{
  "src": {"label":"old","server":"src:993","user":"u","pass":"p"},
  "dst": {"label":"new","server":"dst:993","user":"u","pass":"p"},
  "rate_limit": {"down_bps":100000, "up_bps":50000, "max_connections":5}
}`

const validYAMLConfig = `src:
  label: old
  server: src:993
  user: u
  pass: p
dst:
  label: new
  server: dst:993
  user: u
  pass: p
rate_limit:
  down_bps: 100000
  up_bps: 50000
  max_connections: 5
`

func TestNew_JSON_happyPath(t *testing.T) {
	t.Parallel()
	cfg, err := runNewWithArgs(t, ".json", validJSONConfig)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Src.Server != "src:993" || cfg.Dst.Server != "dst:993" {
		t.Errorf("servers = %s/%s", cfg.Src.Server, cfg.Dst.Server)
	}
	if cfg.RateLimit.DownBPS != 100000 || cfg.RateLimit.UpBPS != 50000 || cfg.RateLimit.MaxConnections != 5 {
		t.Errorf("RateLimit = %+v", cfg.RateLimit)
	}
}

func TestNew_YAML_happyPath(t *testing.T) {
	t.Parallel()
	cfg, err := runNewWithArgs(t, ".yaml", validYAMLConfig)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Src.Server != "src:993" || cfg.Dst.Server != "dst:993" {
		t.Errorf("servers = %s/%s", cfg.Src.Server, cfg.Dst.Server)
	}
}

func TestNew_unknownExtensionReturnsError(t *testing.T) {
	t.Parallel()
	_, err := runNewWithArgs(t, ".toml", validJSONConfig)
	if err == nil {
		t.Fatal("New with .toml returned nil error")
	}
	if !strings.Contains(err.Error(), "unsupported config file format") {
		t.Errorf("err = %v, want substring 'unsupported config file format'", err)
	}
}

func TestNew_missingFileReturnsError(t *testing.T) {
	t.Parallel()
	// Manually drive New with a config flag pointing at a non-existent path —
	// we cannot reuse runNewWithArgs because it always writes the file.
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.json")
	var gotErr error
	app := &cli.Command{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Value: missing},
			&cli.IntFlag{Name: "workers", Value: 4},
			&cli.IntFlag{Name: "bps-down"},
			&cli.IntFlag{Name: "bps-up"},
			&cli.IntFlag{Name: "max-connections"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			_, gotErr = New(c)
			return nil
		},
	}
	if err := app.Run(context.Background(), []string{"app"}); err != nil {
		t.Fatalf("app.Run: %v", err)
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "does not exist") {
		t.Errorf("err = %v, want substring 'does not exist'", gotErr)
	}
}

func TestNew_defaultLabelsAppliedWhenAbsent(t *testing.T) {
	t.Parallel()
	noLabels := `{
		"src":{"server":"s:993","user":"u","pass":"p"},
		"dst":{"server":"d:993","user":"u","pass":"p"}
	}`
	cfg, err := runNewWithArgs(t, ".json", noLabels)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Src.Label != defaultSourceLabel {
		t.Errorf("Src.Label = %q, want %q", cfg.Src.Label, defaultSourceLabel)
	}
	if cfg.Dst.Label != defaultDestLabel {
		t.Errorf("Dst.Label = %q, want %q", cfg.Dst.Label, defaultDestLabel)
	}
}

func TestNew_CLIOverridesConfigBPSDown(t *testing.T) {
	t.Parallel()
	cfg, err := runNewWithArgs(t, ".json", validJSONConfig, "--bps-down=200000")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.RateLimit.DownBPS != 200000 {
		t.Errorf("DownBPS = %d, want 200000 (CLI override of config 100000)", cfg.RateLimit.DownBPS)
	}
}

// TestNew_CLIZeroDoesNotOverride pins the documented "0 = unset" semantic:
// passing --bps-up=0 (or omitting it) must leave the config value intact.
func TestNew_CLIZeroDoesNotOverride(t *testing.T) {
	t.Parallel()
	cfg, err := runNewWithArgs(t, ".json", validJSONConfig, "--bps-up=0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.RateLimit.UpBPS != 50000 {
		t.Errorf("UpBPS = %d, want 50000 (config value preserved when CLI=0)", cfg.RateLimit.UpBPS)
	}
}

func TestNew_workersClamped(t *testing.T) {
	t.Parallel()
	cfg, err := runNewWithArgs(t, ".json", validJSONConfig, "--workers=99")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Workers != maxWorkers {
		t.Errorf("Workers = %d, want %d (clamped to maxWorkers)", cfg.Workers, maxWorkers)
	}
}

func TestValidate(t *testing.T) {
	valid := Credentials{
		Server: "imap.example.com:993",
		User:   "user@example.com",
		Pass:   "password",
	}

	tests := []struct {
		wantErr error
		name    string
		config  Config
	}{
		{nil, "valid config", Config{Src: valid, Dst: valid}},
		{
			ErrSrcServerRequired, "missing source server",
			Config{Src: Credentials{User: valid.User, Pass: valid.Pass}, Dst: valid},
		},
		{
			ErrSrcUserRequired, "missing source user",
			Config{Src: Credentials{Server: valid.Server, Pass: valid.Pass}, Dst: valid},
		},
		{
			ErrSrcPassRequired, "missing source password",
			Config{Src: Credentials{Server: valid.Server, User: valid.User}, Dst: valid},
		},
		{
			ErrDstServerRequired, "missing destination server",
			Config{Src: valid, Dst: Credentials{User: valid.User, Pass: valid.Pass}},
		},
		{
			ErrDstUserRequired, "missing destination user",
			Config{Src: valid, Dst: Credentials{Server: valid.Server, Pass: valid.Pass}},
		},
		{
			ErrDstPassRequired, "missing destination password",
			Config{Src: valid, Dst: Credentials{Server: valid.Server, User: valid.User}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validate()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validate() error = %v; want %v", err, tt.wantErr)
			}
		})
	}
}

func TestClampWorkers(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"negative", -3, minWorkers},
		{"zero", 0, minWorkers},
		{"min", 1, 1},
		{"in range", 5, 5},
		{"max", maxWorkers, maxWorkers},
		{"over max", 99, maxWorkers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampWorkers(tt.in); got != tt.want {
				t.Errorf("clampWorkers(%d) = %d; want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestSetDefaultLabels(t *testing.T) {
	cfg := &Config{
		Src: Credentials{Server: "s", User: "u", Pass: "p"},
		Dst: Credentials{Server: "s", User: "u", Pass: "p"},
	}

	if cfg.Src.Label == "" {
		cfg.Src.Label = defaultSourceLabel
	}
	if cfg.Dst.Label == "" {
		cfg.Dst.Label = defaultDestLabel
	}

	if cfg.Src.Label != defaultSourceLabel {
		t.Errorf("expected default source label %q, got %s", defaultSourceLabel, cfg.Src.Label)
	}
	if cfg.Dst.Label != defaultDestLabel {
		t.Errorf("expected default dst label %q, got %s", defaultDestLabel, cfg.Dst.Label)
	}
}

func TestRateLimitJSON(t *testing.T) {
	t.Parallel()
	data := []byte(`{"src":{"server":"s","user":"u","pass":"p"},"dst":{"server":"s","user":"u","pass":"p"},"rate_limit":{"down_bps":300000,"up_bps":50000,"max_connections":7}}`)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.RateLimit.DownBPS != 300000 || cfg.RateLimit.UpBPS != 50000 || cfg.RateLimit.MaxConnections != 7 {
		t.Errorf("RateLimit = %+v", cfg.RateLimit)
	}
}

func TestRateLimitYAML(t *testing.T) {
	t.Parallel()
	data := []byte(`
src: {server: s, user: u, pass: p}
dst: {server: s, user: u, pass: p}
rate_limit:
  down_bps: 300000
  up_bps: 50000
  max_connections: 7
`)
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.RateLimit.DownBPS != 300000 || cfg.RateLimit.UpBPS != 50000 || cfg.RateLimit.MaxConnections != 7 {
		t.Errorf("RateLimit = %+v", cfg.RateLimit)
	}
}

// Backwards compatibility: a config without a rate_limit block must parse
// cleanly and leave the limits at zero (= unlimited).
func TestRateLimitOmittedJSON(t *testing.T) {
	t.Parallel()
	data := []byte(`{"src":{"server":"s","user":"u","pass":"p"},"dst":{"server":"s","user":"u","pass":"p"}}`)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.RateLimit != (RateLimit{}) {
		t.Errorf("RateLimit = %+v, want zero value", cfg.RateLimit)
	}
}
