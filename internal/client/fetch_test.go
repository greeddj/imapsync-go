package client

import (
	"strings"
	"testing"
)

func TestParseMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "angle brackets",
			in:   "Message-Id: <abc@example.com>\r\n\r\n",
			want: "abc@example.com",
		},
		{
			name: "no brackets",
			in:   "Message-Id: bare-id@example.com\r\n\r\n",
			want: "bare-id@example.com",
		},
		{
			name: "case-insensitive header",
			in:   "MESSAGE-ID: <id@host>\r\n\r\n",
			want: "id@host",
		},
		{
			name: "lowercase header",
			in:   "message-id: <id@host>\r\n\r\n",
			want: "id@host",
		},
		{
			name: "missing terminator (defensive append)",
			in:   "Message-Id: <id@host>\r\n",
			want: "id@host",
		},
		{
			name: "no Message-Id present",
			in:   "Subject: hi\r\n\r\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "garbage",
			in:   "not a valid header\r\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseMessageID(strings.NewReader(tt.in))
			if got != tt.want {
				t.Errorf("parseMessageID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimAngleBrackets(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"<abc>":    "abc",
		"abc":      "abc",
		"<abc":     "<abc",
		"abc>":     "abc>",
		"":         "",
		"<>":       "",
		"<a@b.c>":  "a@b.c",
		"<<nest>>": "<nest>",
	}
	for in, want := range cases {
		if got := trimAngleBrackets(in); got != want {
			t.Errorf("trimAngleBrackets(%q) = %q, want %q", in, got, want)
		}
	}
}
