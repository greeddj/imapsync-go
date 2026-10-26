package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// writeConfigFile writes content to a temp file with the given extension and
// returns its absolute path.
func writeConfigFile(t *testing.T, ext, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config"+ext)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// runNewAt drives config.New through a urfave/cli/v3 Command parsed from
// extraArgs, with the config flag set to path.
// Returns whatever Config / error New produced.
func runNewAt(t *testing.T, path string, extraArgs ...string) (*Config, error) {
	t.Helper()
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

// runNewWithArgs writes content to a temp file with the given extension and
// runs config.New on it.
func runNewWithArgs(t *testing.T, ext, content string, extraArgs ...string) (*Config, error) {
	t.Helper()
	return runNewAt(t, writeConfigFile(t, ext, content), extraArgs...)
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
	// Manually drive New with a config flag pointing at a non-existent path -
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

func TestNew_When_RateLimitFlagIsSet_Then_OverridesConfigValue(t *testing.T) {
	t.Parallel()

	allZero := []string{"--bps-down=0", "--bps-up=0", "--max-connections=0"}
	tests := []struct {
		name    string
		ext     string
		content string
		args    []string
		want    RateLimit
	}{
		{
			name: "no rate flags keeps the config values", ext: ".json", content: validJSONConfig,
			want: RateLimit{DownBPS: 100000, UpBPS: 50000, MaxConnections: 5},
		},
		{
			name: "bps-down of 0 lifts the config limit", ext: ".json", content: validJSONConfig,
			args: []string{"--bps-down=0"},
			want: RateLimit{DownBPS: 0, UpBPS: 50000, MaxConnections: 5},
		},
		{
			name: "bps-up of 0 lifts the config limit", ext: ".json", content: validJSONConfig,
			args: []string{"--bps-up=0"},
			want: RateLimit{DownBPS: 100000, UpBPS: 0, MaxConnections: 5},
		},
		{
			name: "max-connections of 0 lifts the config cap", ext: ".json", content: validJSONConfig,
			args: []string{"--max-connections=0"},
			want: RateLimit{DownBPS: 100000, UpBPS: 50000, MaxConnections: 0},
		},
		{
			name: "all three flags of 0 lift every config limit", ext: ".json", content: validJSONConfig,
			args: allZero,
			want: RateLimit{},
		},
		{
			name: "bps-down above 0 replaces the config value", ext: ".json", content: validJSONConfig,
			args: []string{"--bps-down=200000"},
			want: RateLimit{DownBPS: 200000, UpBPS: 50000, MaxConnections: 5},
		},
		{
			name: "all three flags of 0 lift every limit of a YAML config", ext: ".yaml", content: validYAMLConfig,
			args: allZero,
			want: RateLimit{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content, tt.args...)

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cfg.RateLimit != tt.want {
				t.Errorf("RateLimit = %+v, want %+v", cfg.RateLimit, tt.want)
			}
		})
	}
}

func TestNew_When_CommandDeclaresNoRateLimitFlags_Then_KeepsConfigValues(t *testing.T) {
	t.Parallel()

	var (
		gotCfg *Config
		gotErr error
	)
	app := &cli.Command{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Value: writeConfigFile(t, ".json", validJSONConfig)},
			&cli.IntFlag{Name: "workers", Value: 4},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			gotCfg, gotErr = New(c)
			return nil
		},
	}

	if err := app.Run(context.Background(), []string{"app"}); err != nil {
		t.Fatalf("app.Run: %v", err)
	}

	if gotErr != nil {
		t.Fatalf("New: %v", gotErr)
	}
	if want := (RateLimit{DownBPS: 100000, UpBPS: 50000, MaxConnections: 5}); gotCfg.RateLimit != want {
		t.Errorf("RateLimit = %+v, want %+v", gotCfg.RateLimit, want)
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
		{
			nil, "two valid map items",
			Config{Src: valid, Dst: valid, Map: []DirectoryMapping{
				{Source: "INBOX", Destination: "INBOX"},
				{Source: "Sent", Destination: "Sent Items"},
			}},
		},
		{
			ErrMapSrcRequired, "first map item has no source",
			Config{Src: valid, Dst: valid, Map: []DirectoryMapping{{Destination: "INBOX"}}},
		},
		{
			ErrMapDstRequired, "second map item has no destination",
			Config{Src: valid, Dst: valid, Map: []DirectoryMapping{
				{Source: "INBOX", Destination: "INBOX"},
				{Source: "Sent"},
			}},
		},
		{
			ErrMapSrcRequired, "map item with neither folder reports the source",
			Config{Src: valid, Dst: valid, Map: []DirectoryMapping{{}}},
		},
		{
			ErrSrcServerRequired, "missing source server together with a bad map item",
			Config{
				Src: Credentials{User: valid.User, Pass: valid.Pass}, Dst: valid,
				Map: []DirectoryMapping{{Source: "INBOX"}},
			},
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

func TestNew_When_ConfigHasUnknownKey_Then_ReturnsErrorNamingKeyAndFile(t *testing.T) {
	t.Parallel()

	const (
		jsonSrc = `"src":{"server":"s:993","user":"u","pass":"p"}`
		jsonDst = `"dst":{"server":"d:993","user":"u","pass":"p"}`
		yamlSrc = "src: {server: 's:993', user: u, pass: p}\n"
		yamlDst = "dst: {server: 'd:993', user: u, pass: p}\n"
	)
	tests := []struct {
		name    string
		ext     string
		content string
		key     string
	}{
		{
			name: "JSON top level maps", ext: ".json", key: "maps",
			content: `{` + jsonSrc + `,` + jsonDst + `,"maps":[{"src":"INBOX","dst":"INBOX"}]}`,
		},
		{
			name: "JSON top level rate-limit", ext: ".json", key: "rate-limit",
			content: `{` + jsonSrc + `,` + jsonDst + `,"rate-limit":{"down_bps":300000}}`,
		},
		{
			name: "JSON src host", ext: ".json", key: "host",
			content: `{"src":{"server":"s:993","user":"u","pass":"p","host":"x"},` + jsonDst + `}`,
		},
		{
			name: "JSON dst password", ext: ".json", key: "password",
			content: `{` + jsonSrc + `,"dst":{"server":"d:993","user":"u","pass":"p","password":"x"}}`,
		},
		{
			name: "JSON rate_limit down", ext: ".json", key: "down",
			content: `{` + jsonSrc + `,` + jsonDst + `,"rate_limit":{"down":300000}}`,
		},
		{
			name: "JSON map entry source", ext: ".json", key: "source",
			content: `{` + jsonSrc + `,` + jsonDst + `,"map":[{"source":"INBOX","dst":"INBOX"}]}`,
		},
		{
			name: "JSON top level workers", ext: ".json", key: "workers",
			content: `{` + jsonSrc + `,` + jsonDst + `,"workers":8}`,
		},
		{
			name: "YAML top level mapping", ext: ".yaml", key: "mapping",
			content: yamlSrc + yamlDst + "mapping:\n  - src: INBOX\n    dst: INBOX\n",
		},
		{
			name: "YAML top level ratelimit", ext: ".yaml", key: "ratelimit",
			content: yamlSrc + yamlDst + "ratelimit:\n  down_bps: 300000\n",
		},
		{
			name: "YAML src host", ext: ".yaml", key: "host",
			content: "src: {server: 's:993', user: u, pass: p, host: x}\n" + yamlDst,
		},
		{
			name: "YAML rate_limit downbps", ext: ".yaml", key: "downbps",
			content: yamlSrc + yamlDst + "rate_limit:\n  downbps: 300000\n",
		},
		{
			name: "YAML map entry source", ext: ".yaml", key: "source",
			content: yamlSrc + yamlDst + "map:\n  - source: INBOX\n    dst: INBOX\n",
		},
		{
			name: "YAML top level workers", ext: ".yaml", key: "workers",
			content: yamlSrc + yamlDst + "workers: 8\n",
		},
		{
			name: "yml top level maps", ext: ".yml", key: "maps",
			content: yamlSrc + yamlDst + "maps:\n  - src: INBOX\n    dst: INBOX\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, tt.ext, tt.content)

			cfg, err := runNewAt(t, path)

			if cfg != nil {
				t.Errorf("Config = %+v, want nil", cfg)
			}
			if err == nil {
				t.Fatal("New returned nil error, want an unknown key error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want the config path %q", err, path)
			}
			// The temp dir name carries the subtest name, so match the key
			// on the text without the path.
			if msg := strings.ReplaceAll(err.Error(), path, ""); !strings.Contains(msg, tt.key) {
				t.Errorf("error = %q, want the key %q", msg, tt.key)
			}
		})
	}
}

func TestNew_When_ContentFollowsFirstDocument_Then_ReturnsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{"JSON second object", ".json", validJSONConfig + `{"map":[{"src":"INBOX","dst":"INBOX"}]}`},
		{"JSON garbage", ".json", validJSONConfig + "\ngarbage"},
		{"JSON closing brace", ".json", validJSONConfig + "}"},
		{"YAML second document", ".yaml", validYAMLConfig + "---\nmap:\n  - src: INBOX\n    dst: INBOX\n"},
		{"YAML bare scalar document", ".yaml", validYAMLConfig + "---\nfoo\n"},
		{"YAML sequence document", ".yaml", validYAMLConfig + "---\n- a\n"},
		{"YAML empty flow mapping document", ".yaml", validYAMLConfig + "---\n{}\n"},
		{"YAML quoted tilde document", ".yaml", validYAMLConfig + "---\n\"~\"\n"},
		{"YAML malformed document", ".yaml", validYAMLConfig + "---\n[\n"},
		{"YAML null document then a document with a key", ".yaml", validYAMLConfig + "---\n~\n---\nmap: []\n"},
		{"YAML empty document then a document with a key", ".yaml", validYAMLConfig + "---\n---\nmap: []\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfigFile(t, tt.ext, tt.content)

			cfg, err := runNewAt(t, path)

			if cfg != nil {
				t.Errorf("Config = %+v, want nil", cfg)
			}
			if !errors.Is(err, errTrailingContent) {
				t.Fatalf("error = %v, want errTrailingContent", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want the config path %q", err, path)
			}
		})
	}
}

func TestNew_When_TrailingWhitespaceFollowsDocument_Then_Loads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{"JSON trailing whitespace", ".json", validJSONConfig + "\n\n  "},
		{"YAML document end marker", ".yaml", validYAMLConfig + "...\n"},
		{"YAML document start marker", ".yaml", "---\n" + validYAMLConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cfg.Src.Server != "src:993" || cfg.RateLimit.MaxConnections != 5 {
				t.Errorf("Config = %+v, want the file values", cfg)
			}
		})
	}
}

func TestNew_When_NullOrEmptyDocumentFollowsDocument_Then_Loads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{"separator only", ".yaml", validYAMLConfig + "---\n"},
		{"separator without final newline", ".yaml", validYAMLConfig + "---"},
		{"comment only document", ".yaml", validYAMLConfig + "---\n# comment\n"},
		{"tilde null document", ".yaml", validYAMLConfig + "---\n~\n"},
		{"null word document", ".yaml", validYAMLConfig + "---\nnull\n"},
		{"tagged null document", ".yaml", validYAMLConfig + "--- !<tag:yaml.org,2002:null>\n"},
		{"two null documents", ".yaml", validYAMLConfig + "---\n~\n---\n"},
		{"end marker then separator", ".yaml", validYAMLConfig + "...\n---\n"},
		{"yml separator only", ".yml", validYAMLConfig + "---\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cfg.Src.Server != "src:993" || cfg.RateLimit.MaxConnections != 5 {
				t.Errorf("Config = %+v, want the file values", cfg)
			}
		})
	}
}

func TestNew_When_FileIsEmpty_Then_KeepsTheFormatError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantIs  error
		name    string
		ext     string
		content string
		wantMsg string
	}{
		{name: "empty JSON", ext: ".json", wantMsg: "invalid JSON in config file"},
		{name: "whitespace only JSON", ext: ".json", content: " \n\t\n", wantMsg: "invalid JSON in config file"},
		{name: "null JSON", ext: ".json", content: "null", wantIs: ErrSrcServerRequired},
		{name: "empty YAML", ext: ".yaml", wantIs: ErrSrcServerRequired},
		{name: "comments only YAML", ext: ".yaml", content: "# nothing here\n", wantIs: ErrSrcServerRequired},
		{name: "separators only YAML", ext: ".yaml", content: "---\n---\n", wantIs: ErrSrcServerRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if cfg != nil {
				t.Errorf("Config = %+v, want nil", cfg)
			}
			if err == nil {
				t.Fatal("New returned nil error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want substring %q", err, tt.wantMsg)
			}
		})
	}
}

func TestNew_When_ExampleConfigIsLoaded_Then_Succeeds(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"config.example.json", "config.example.yaml"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewAt(t, filepath.Join("..", "..", name))

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cfg.Src.Label != "old" || cfg.Dst.Label != "new" {
				t.Errorf("labels = %q/%q, want old/new", cfg.Src.Label, cfg.Dst.Label)
			}
			if len(cfg.Map) != 3 {
				t.Fatalf("len(Map) = %d, want 3", len(cfg.Map))
			}
			if want := (DirectoryMapping{Source: "Archive.2023", Destination: "Archive/2023"}); cfg.Map[2] != want {
				t.Errorf("Map[2] = %+v, want %+v", cfg.Map[2], want)
			}
		})
	}
}

const (
	mapTestJSONCreds = `"src":{"server":"s:993","user":"u","pass":"p"},"dst":{"server":"d:993","user":"u","pass":"p"}`
	mapTestYAMLCreds = "src: {server: 's:993', user: u, pass: p}\ndst: {server: 'd:993', user: u, pass: p}\n"
)

func jsonWithMap(mapJSON string) string {
	return "{" + mapTestJSONCreds + "," + mapJSON + "}"
}

func TestNew_When_MapEntryHasNoFolderName_Then_ReturnsErrorNamingEntryAndKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr   error
		name      string
		ext       string
		content   string
		wantEntry int
	}{
		{
			name: "JSON empty dst", ext: ".json", wantEntry: 1, wantErr: ErrMapDstRequired,
			content: jsonWithMap(`"map":[{"src":"INBOX","dst":""}]`),
		},
		{
			name: "JSON absent dst in second entry", ext: ".json", wantEntry: 2, wantErr: ErrMapDstRequired,
			content: jsonWithMap(`"map":[{"src":"INBOX","dst":"INBOX"},{"src":"Sent"}]`),
		},
		{
			name: "JSON empty src", ext: ".json", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: jsonWithMap(`"map":[{"src":"","dst":"Archive"}]`),
		},
		{
			name: "JSON null src", ext: ".json", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: jsonWithMap(`"map":[{"src":null,"dst":"Archive"}]`),
		},
		{
			name: "JSON both empty reports src", ext: ".json", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: jsonWithMap(`"map":[{"src":"","dst":""}]`),
		},
		{
			name: "JSON empty object as third entry", ext: ".json", wantEntry: 3, wantErr: ErrMapSrcRequired,
			content: jsonWithMap(`"map":[{"src":"A","dst":"A"},{"src":"B","dst":"B"},{}]`),
		},
		{
			name: "JSON null entry", ext: ".json", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: jsonWithMap(`"map":[null]`),
		},
		{
			name: "YAML empty dst", ext: ".yaml", wantEntry: 1, wantErr: ErrMapDstRequired,
			content: mapTestYAMLCreds + "map:\n  - src: INBOX\n    dst: \"\"\n",
		},
		{
			name: "YAML absent dst", ext: ".yaml", wantEntry: 1, wantErr: ErrMapDstRequired,
			content: mapTestYAMLCreds + "map:\n  - src: INBOX\n",
		},
		{
			name: "YAML src without value", ext: ".yaml", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - src:\n    dst: Archive\n",
		},
		{
			name: "YAML null src", ext: ".yaml", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - src: ~\n    dst: Archive\n",
		},
		{
			name: "YAML empty flow mapping as second entry", ext: ".yaml", wantEntry: 2, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - {src: INBOX, dst: INBOX}\n  - {}\n",
		},
		{
			name: "YAML null entry", ext: ".yaml", wantEntry: 1, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - ~\n",
		},
		{
			name: "YAML bare dash keeps its position", ext: ".yaml", wantEntry: 2, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - src: A\n    dst: A\n  -\n  - src: B\n    dst: B\n",
		},
		{
			name: "yml single quoted empty dst", ext: ".yml", wantEntry: 1, wantErr: ErrMapDstRequired,
			content: mapTestYAMLCreds + "map:\n  - src: INBOX\n    dst: ''\n",
		},
		{
			name: "YAML null entry before a trailing separator", ext: ".yaml", wantEntry: 2, wantErr: ErrMapSrcRequired,
			content: mapTestYAMLCreds + "map:\n  - src: A\n    dst: A\n  - ~\n---\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if cfg != nil {
				t.Errorf("Config = %+v, want nil", cfg)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			other, key := ErrMapSrcRequired, `"dst"`
			if errors.Is(tt.wantErr, ErrMapSrcRequired) {
				other, key = ErrMapDstRequired, `"src"`
			}
			if errors.Is(err, other) {
				t.Errorf("error = %v, also matches %v", err, other)
			}
			if want := fmt.Sprintf("map entry %d: ", tt.wantEntry); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want the text %q", err, want)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want the key %s", err, key)
			}
		})
	}
}

func TestNew_When_MapFolderNameHasSpaces_Then_LoadsItUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{
			name: "JSON", ext: ".json",
			content: jsonWithMap(`"map":[{"src":" Sent Items ","dst":" "}]`),
		},
		{
			name: "YAML", ext: ".yaml",
			content: mapTestYAMLCreds + "map:\n  - src: \" Sent Items \"\n    dst: \" \"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if len(cfg.Map) != 1 {
				t.Fatalf("len(Map) = %d, want 1", len(cfg.Map))
			}
			if want := (DirectoryMapping{Source: " Sent Items ", Destination: " "}); cfg.Map[0] != want {
				t.Errorf("Map[0] = %q, want %q", cfg.Map[0], want)
			}
		})
	}
}

func TestNew_When_MapIsEmptyOrNull_Then_LoadsWithNoEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{"JSON empty array", ".json", jsonWithMap(`"map":[]`)},
		{"JSON null", ".json", jsonWithMap(`"map":null`)},
		{"YAML empty sequence", ".yaml", mapTestYAMLCreds + "map: []\n"},
		{"YAML key without value", ".yaml", mapTestYAMLCreds + "map:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := runNewWithArgs(t, tt.ext, tt.content)

			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if len(cfg.Map) != 0 {
				t.Errorf("Map = %+v, want no entries", cfg.Map)
			}
		})
	}
}

func Test_restoreNullMapEntries_When_YAMLIsMalformed_Then_ReturnsError(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := restoreNullMapEntries([]byte("map: ["), &cfg)

	if err == nil {
		t.Fatal("restoreNullMapEntries returned nil error, want a YAML error")
	}
	if cfg.Map != nil {
		t.Errorf("Map = %+v, want nil", cfg.Map)
	}
}

const authTestPass = "s3cret-auth-pass"

// authConfig builds a config file of the given format. Each auth value is a raw
// JSON or YAML scalar, and an empty one leaves the key out.
func authConfig(ext, srcAuth, dstAuth string) string {
	if ext == ".json" {
		key := func(raw string) string {
			if raw == "" {
				return ""
			}
			return `,"auth":` + raw
		}
		return `{"src":{"server":"s:993","user":"u","pass":"` + authTestPass + `"` + key(srcAuth) + `},` +
			`"dst":{"server":"d:993","user":"u","pass":"` + authTestPass + `"` + key(dstAuth) + `}}`
	}
	line := func(raw string) string {
		if raw == "" {
			return ""
		}
		return "  auth: " + raw + "\n"
	}
	return "src:\n  server: s:993\n  user: u\n  pass: " + authTestPass + "\n" + line(srcAuth) +
		"dst:\n  server: d:993\n  user: u\n  pass: " + authTestPass + "\n" + line(dstAuth)
}

var authExtensions = []string{".json", ".yaml", ".yml"}

func TestNew_When_AuthIsSupported_Then_LoadsItUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		want     string
		yamlOnly bool
	}{
		{name: "key absent"},
		{name: "empty string", raw: `""`},
		{name: "null", raw: "null"},
		{name: "YAML tilde", raw: "~", yamlOnly: true},
		{name: "lower case login", raw: `"login"`, want: "login"},
		{name: "upper case login", raw: `"LOGIN"`, want: "LOGIN"},
		{name: "mixed case login", raw: `"Login"`, want: "Login"},
		{name: "lower case cram-md5", raw: `"cram-md5"`, want: "cram-md5"},
		{name: "upper case cram-md5", raw: `"CRAM-MD5"`, want: "CRAM-MD5"},
		{name: "mixed case cram-md5", raw: `"Cram-Md5"`, want: "Cram-Md5"},
	}
	for _, ext := range authExtensions {
		for _, tt := range tests {
			if tt.yamlOnly && ext == ".json" {
				continue
			}
			for _, side := range []string{"src", "dst"} {
				t.Run(ext+" "+tt.name+" on "+side, func(t *testing.T) {
					t.Parallel()

					srcAuth, dstAuth := tt.raw, ""
					if side == "dst" {
						srcAuth, dstAuth = "", tt.raw
					}

					cfg, err := runNewWithArgs(t, ext, authConfig(ext, srcAuth, dstAuth))

					if err != nil {
						t.Fatalf("New: %v", err)
					}
					wantSrc, wantDst := tt.want, ""
					if side == "dst" {
						wantSrc, wantDst = "", tt.want
					}
					if cfg.Src.Auth != wantSrc || cfg.Dst.Auth != wantDst {
						t.Errorf("Auth = %q/%q, want %q/%q", cfg.Src.Auth, cfg.Dst.Auth, wantSrc, wantDst)
					}
				})
			}
		}
	}
}

func TestNew_When_AuthIsUnsupported_Then_ReturnsErrorNamingSideAndValue(t *testing.T) {
	t.Parallel()

	values := []string{"cram_md5", "crammd5", "xoauth2", "plain", " login", "cram-md5 "}
	const hint = `"login" or "cram-md5"`

	for _, ext := range authExtensions {
		for _, value := range values {
			for _, side := range []string{"src", "dst"} {
				t.Run(ext+" "+strconv.Quote(value)+" on "+side, func(t *testing.T) {
					t.Parallel()

					srcAuth, dstAuth, other := strconv.Quote(value), `"CRAM-MD5"`, "dst"
					if side == "dst" {
						srcAuth, dstAuth, other = `"CRAM-MD5"`, strconv.Quote(value), "src"
					}

					cfg, err := runNewWithArgs(t, ext, authConfig(ext, srcAuth, dstAuth))

					if cfg != nil {
						t.Errorf("Config = %+v, want nil", cfg)
					}
					if !errors.Is(err, ErrAuthUnsupported) {
						t.Fatalf("error = %v, want %v", err, ErrAuthUnsupported)
					}
					msg := err.Error()
					if !strings.HasPrefix(msg, side+": ") {
						t.Errorf("error = %q, want the prefix %q", msg, side+": ")
					}
					if !strings.Contains(msg, strconv.Quote(value)) {
						t.Errorf("error = %q, want the quoted value %s", msg, strconv.Quote(value))
					}
					if !strings.Contains(msg, hint) {
						t.Errorf("error = %q, want the hint %s", msg, hint)
					}
					if strings.Contains(msg, authTestPass) {
						t.Errorf("error = %q, want no password", msg)
					}
					if strings.Contains(msg, other+": ") {
						t.Errorf("error = %q, want no mention of %q", msg, other)
					}
				})
			}
		}
	}
}

func TestValidate_When_AuthIsUnsupported_Then_ChecksBlocksInOrder(t *testing.T) {
	t.Parallel()

	valid := Credentials{Server: "imap.example.com:993", User: "user@example.com", Pass: "password"}
	withAuth := func(c Credentials, auth string) Credentials {
		c.Auth = auth
		return c
	}
	badSrc, badDst := withAuth(valid, "xoauth2"), withAuth(valid, "plain")

	tests := []struct {
		wantErr    error
		name       string
		wantPrefix string
		config     Config
	}{
		{
			name:    "bad source auth with a missing source server",
			wantErr: ErrSrcServerRequired,
			config:  Config{Src: Credentials{User: "u", Pass: "p", Auth: "xoauth2"}, Dst: valid},
		},
		{
			name:    "bad source auth with a missing source user",
			wantErr: ErrSrcUserRequired,
			config:  Config{Src: Credentials{Server: "s:993", Pass: "p", Auth: "xoauth2"}, Dst: valid},
		},
		{
			name:    "bad source auth with a missing source password",
			wantErr: ErrSrcPassRequired,
			config:  Config{Src: Credentials{Server: "s:993", User: "u", Auth: "xoauth2"}, Dst: valid},
		},
		{
			name: "bad source auth with a missing destination server", wantErr: ErrAuthUnsupported, wantPrefix: "src: ",
			config: Config{Src: badSrc, Dst: Credentials{User: "u", Pass: "p"}},
		},
		{
			name: "bad source auth with a bad destination auth", wantErr: ErrAuthUnsupported, wantPrefix: "src: ",
			config: Config{Src: badSrc, Dst: badDst},
		},
		{
			name:    "bad destination auth with a missing destination password",
			wantErr: ErrDstPassRequired,
			config:  Config{Src: valid, Dst: Credentials{Server: "d:993", User: "u", Auth: "plain"}},
		},
		{
			name: "bad destination auth with a map item without a source", wantErr: ErrAuthUnsupported, wantPrefix: "dst: ",
			config: Config{Src: valid, Dst: badDst, Map: []DirectoryMapping{{Destination: "INBOX"}}},
		},
		{
			name:   "CRAM-MD5 on both sides",
			config: Config{Src: withAuth(valid, "CRAM-MD5"), Dst: withAuth(valid, "CRAM-MD5")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.config.validate()

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("validate() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				return
			}
			if !errors.Is(tt.wantErr, ErrAuthUnsupported) {
				if errors.Is(err, ErrAuthUnsupported) {
					t.Errorf("validate() error = %v, also matches %v", err, ErrAuthUnsupported)
				}
				return
			}
			if !strings.HasPrefix(err.Error(), tt.wantPrefix) {
				t.Errorf("validate() error = %q, want the prefix %q", err, tt.wantPrefix)
			}
		})
	}
}
