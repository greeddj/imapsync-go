package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	imapclient "github.com/emersion/go-imap/client"
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

		{name: "transientNotLoggedIn", err: errors.New("BAD Not logged in"), want: ClassTransient},
		{name: "transientNotLoggedInLowercase", err: errors.New("nO nOt LoGgEd In"), want: ClassTransient},
		{name: "transientConnectionClosed", err: errors.New("imap: connection closed"), want: ClassTransient},
		{name: "transientConnectionClosedDuringCommand", err: fmt.Errorf("select: %w", errors.New("imap: connection closed during command execution")), want: ClassTransient},

		{name: "permAuth", err: errors.New("authentication failed"), want: ClassPermanent},
		{name: "permCreds", err: errors.New("Invalid credentials"), want: ClassPermanent},
		{name: "permMailbox", err: errors.New("NO such mailbox"), want: ClassPermanent},
		{name: "permAuthDovecot", err: errors.New("Authentication failed."), want: ClassPermanent},
		{name: "permCredsGmail", err: errors.New("Invalid credentials (Failure)"), want: ClassPermanent},

		{name: "transientTempAuthDovecot", err: errors.New("Temporary authentication failure. [mx1:2026-10-25 10:00:00]"), want: ClassTransient},
		{name: "transientTempAuthUnavailable", err: errors.New("Authentication service temporarily unavailable"), want: ClassTransient},
		{name: "unknownTempNonAuth", err: errors.New("Temporary System Problem. Try again later."), want: ClassUnknown},
		{name: "throttleBeatsTempAuth", err: errors.New("Temporary authentication failure: too many simultaneous connections"), want: ClassThrottled},

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

func TestClassifyError_When_WrapperTextHoldsAPhrase_Then_LeafDecides(t *testing.T) {
	t.Parallel()

	closedUnderFolder := fmt.Errorf("[src] cannot select folder %s: %w", "Alerts/Account Lockout", errors.New("imap: connection closed"))
	tests := []struct {
		err  error
		name string
		want ErrClass
	}{
		{name: "folder name holds a throttle phrase over a closed connection", err: closedUnderFolder, want: ClassTransient},
		{name: "folder name holds a throttle phrase over not logged in", err: fmt.Errorf("[src] status %s: %w", "Session expired", imapclient.ErrNotLoggedIn), want: ClassTransient},
		{name: "label holds a throttle phrase over a closed connection", err: fmt.Errorf("[%s] fetch IDs: %w", "bandwidth limit exceeded", errors.New("imap: connection closed")), want: ClassTransient},
		{name: "exceeded in the leaf and quota in the folder name", err: fmt.Errorf("[src] select folder %s: %w", "Quota", errors.New("NO retry count exceeded")), want: ClassUnknown},
		{name: "wrapper text never makes an error permanent", err: fmt.Errorf("[src] no such mailbox %s: %w", "Work", errors.New("unrecognized server response")), want: ClassUnknown},
		{name: "throttled leaf under a plain folder wrapper", err: fmt.Errorf("[src] select folder %s: %w", "Work", errors.New("Too many simultaneous connections")), want: ClassThrottled},
		{name: "multi-wrap node with a watchdog cause", err: fmt.Errorf("%w: %w", closedUnderFolder, errIdleTimeout), want: ClassTransient},
		{
			name: "multi-wrap node takes the first leaf that is not unknown",
			err:  fmt.Errorf("[src] %w and %w", errors.New("unrecognized server response"), errors.New("Too many simultaneous connections")),
			want: ClassThrottled,
		},
		{
			name: "multi-wrap node keeps the order of its leaves",
			err:  fmt.Errorf("[src] %w and %w", errors.New("imap: connection closed"), errors.New("Too many simultaneous connections")),
			want: ClassTransient,
		},
		{
			name: "multi-wrap node with only unknown leaves",
			err:  fmt.Errorf("[src] %w and %w", errors.New("unrecognized server response"), errors.New("another unrecognized response")),
			want: ClassUnknown,
		},
		{name: "label holds temporary over a credential failure", err: fmt.Errorf("[%s] login: %w", "Temporary", errors.New("Authentication failed.")), want: ClassPermanent},
		{
			name: "temporary authentication leaf under a reconnect wrapper",
			err:  fmt.Errorf("[src] failed to reconnect after retries: %w", errors.New("Temporary authentication failure.")),
			want: ClassTransient,
		},
		{name: "wrap node without a cause is a leaf", err: fmt.Errorf("Too many simultaneous connections: %w", nil), want: ClassThrottled},
		{name: "multi-wrap node without causes is a leaf", err: fmt.Errorf("Too many simultaneous connections: %w %w", nil, nil), want: ClassThrottled},
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

func TestIsAlreadyExistsErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "lowercaseAlreadyExists", err: errors.New("CREATE failed: mailbox already exists"), want: true},
		{name: "mixedCaseAlreadyExists", err: errors.New("Folder Already Exists"), want: true},
		{name: "dovecotMailboxExists", err: errors.New("NO Mailbox exists"), want: true},
		{name: "unrelated", err: errors.New("permission denied"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isAlreadyExistsErr(tt.err); got != tt.want {
				t.Errorf("isAlreadyExistsErr(%v) = %v, want %v", tt.err, got, tt.want)
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
