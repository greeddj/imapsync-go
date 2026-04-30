package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		name string
		want ErrClass
	}{
		{name: "nil", err: nil, want: ClassUnknown},
		{name: "eof", err: io.EOF, want: ClassTransient},
		{name: "netClosed", err: net.ErrClosed, want: ClassTransient},
		{name: "wrappedEof", err: fmt.Errorf("read: %w", io.EOF), want: ClassTransient},
		{name: "netError", err: &net.OpError{Op: "read", Err: errors.New("connection reset")}, want: ClassTransient},

		{name: "throttledTooMany", err: errors.New("[ALERT] Too many simultaneous connections"), want: ClassThrottled},
		{name: "throttledSession", err: errors.New("BYE Session expired, please login again"), want: ClassThrottled},
		{name: "throttledBandwidth", err: errors.New("Account exceeded bandwidth limits for IMAP"), want: ClassThrottled},
		{name: "throttledQuota", err: errors.New("user has exceeded quota"), want: ClassThrottled},
		{name: "throttledLockout", err: errors.New("Account in lockout state"), want: ClassThrottled},

		{name: "permAuth", err: errors.New("authentication failed"), want: ClassPermanent},
		{name: "permCreds", err: errors.New("Invalid credentials"), want: ClassPermanent},
		{name: "permMailbox", err: errors.New("NO such mailbox"), want: ClassPermanent},

		{name: "unknown", err: errors.New("unrecognized server response"), want: ClassUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyError(tt.err); got != tt.want {
				t.Errorf("classifyError(%v) = %s, want %s", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	if !isRetryable(io.EOF) {
		t.Error("EOF must be retryable")
	}
	if isRetryable(errors.New("Too many simultaneous connections")) {
		t.Error("throttled errors must NOT be retryable (caller surfaces)")
	}
	if isRetryable(errors.New("Invalid credentials")) {
		t.Error("permanent errors must NOT be retryable")
	}
}
