package config

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		wantErr     bool
		errContains string
	}{
		{
			name: "valid config",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "user@source.com",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "user@dest.com",
					Pass:   "password",
				},
			},
			wantErr: false,
		},
		{
			name: "missing source server",
			config: Config{
				Src: Credentials{
					Server: "",
					User:   "user@source.com",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "user@dest.com",
					Pass:   "password",
				},
			},
			wantErr:     true,
			errContains: "source server is required",
		},
		{
			name: "missing source user",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "user@dest.com",
					Pass:   "password",
				},
			},
			wantErr:     true,
			errContains: "source user is required",
		},
		{
			name: "missing source password",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "user@source.com",
					Pass:   "",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "user@dest.com",
					Pass:   "password",
				},
			},
			wantErr:     true,
			errContains: "source password is required",
		},
		{
			name: "missing destination server",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "user@source.com",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "",
					User:   "user@dest.com",
					Pass:   "password",
				},
			},
			wantErr:     true,
			errContains: "destination server is required",
		},
		{
			name: "missing destination user",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "user@source.com",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "",
					Pass:   "password",
				},
			},
			wantErr:     true,
			errContains: "destination user is required",
		},
		{
			name: "missing destination password",
			config: Config{
				Src: Credentials{
					Server: "imap.source.com:993",
					User:   "user@source.com",
					Pass:   "password",
				},
				Dst: Credentials{
					Server: "imap.dest.com:993",
					User:   "user@dest.com",
					Pass:   "",
				},
			},
			wantErr:     true,
			errContains: "destination password is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSetDefaultLabels(t *testing.T) {
	cfg := &Config{
		Src: Credentials{
			Server: "imap.source.com:993",
			User:   "user@source.com",
			Pass:   "password",
		},
		Dst: Credentials{
			Server: "imap.dest.com:993",
			User:   "user@dest.com",
			Pass:   "password",
		},
	}

	if cfg.Src.Label == "" {
		cfg.Src.Label = "src"
	}
	if cfg.Dst.Label == "" {
		cfg.Dst.Label = "dst"
	}

	if cfg.Src.Label != "src" {
		t.Errorf("expected default source label 'src', got %s", cfg.Src.Label)
	}
	if cfg.Dst.Label != "dst" {
		t.Errorf("expected default dest label 'dst', got %s", cfg.Dst.Label)
	}
}
