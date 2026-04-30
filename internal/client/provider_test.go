package client

import "testing"

func TestDetectProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr    string
		want    string
		matched bool
	}{
		{"imap.gmail.com:993", "Gmail", true},
		{"imap.gmail.com", "Gmail", true},
		{"IMAP.gmail.com:993", "Gmail", true},
		{"  imap.gmail.com  :993", "Gmail", true},
		{"imap.example.org:993", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			got, ok := DetectProvider(tt.addr)
			if ok != tt.matched {
				t.Fatalf("matched = %v, want %v", ok, tt.matched)
			}
			if ok && got.Name != tt.want {
				t.Errorf("Name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}
