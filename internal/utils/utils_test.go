package utils

import (
	"testing"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name     string
		bytes    uint64
		expected string
	}{
		{
			name:     "bytes",
			bytes:    512,
			expected: "512 B",
		},
		{
			name:     "kilobytes",
			bytes:    2048,
			expected: "2.00 KB",
		},
		{
			name:     "megabytes",
			bytes:    5242880,
			expected: "5.00 MB",
		},
		{
			name:     "gigabytes",
			bytes:    3221225472,
			expected: "3.00 GB",
		},
		{
			name:     "zero bytes",
			bytes:    0,
			expected: "0 B",
		},
		{
			name:     "1 KB exactly",
			bytes:    1024,
			expected: "1.00 KB",
		},
		{
			name:     "1 MB exactly",
			bytes:    1048576,
			expected: "1.00 MB",
		},
		{
			name:     "1 GB exactly",
			bytes:    1073741824,
			expected: "1.00 GB",
		},
		{
			name:     "fractional KB",
			bytes:    1536,
			expected: "1.50 KB",
		},
		{
			name:     "fractional MB",
			bytes:    1572864,
			expected: "1.50 MB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSize(tt.bytes)
			if result != tt.expected {
				t.Errorf("FormatSize(%d) = %s; want %s", tt.bytes, result, tt.expected)
			}
		})
	}
}
