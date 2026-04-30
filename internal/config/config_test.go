package config

import (
	"encoding/json"
	"errors"
	"testing"

	"gopkg.in/yaml.v3"
)

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
