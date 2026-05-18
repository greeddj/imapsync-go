package client

import (
	"errors"
	"io"
	"net"
	"strings"
)

// ErrClass categorizes IMAP errors so that the reconnect machinery can pick a
// strategy: retry quickly, give up, or back off for a long time.
//
// Classification is heuristic — IMAP responses are textual and there is no
// standard error code, so we match on error strings emitted by go-imap and the
// most common server-side messages (Gmail in particular).
type ErrClass int

const (
	// ClassUnknown is the zero value: the error has not been classified.
	// Caller should treat it as non-retryable to avoid loops on novel errors.
	ClassUnknown ErrClass = iota
	// ClassTransient indicates a network-level failure (EOF, closed conn,
	// timeout). Reconnect-and-retry is appropriate.
	ClassTransient
	// ClassPermanent indicates an authoritative refusal (auth failure, bad
	// command, missing mailbox). Retry will not help.
	ClassPermanent
	// ClassThrottled indicates a server-signaled rate limit or quota hit.
	// Reconnecting immediately would compound the problem; back off.
	ClassThrottled
)

// String makes the constants self-describing in logs.
func (c ErrClass) String() string {
	switch c {
	case ClassTransient:
		return "transient"
	case ClassPermanent:
		return "permanent"
	case ClassThrottled:
		return "throttled"
	default:
		return "unknown"
	}
}

// classifyError inspects err and returns its class.
func classifyError(err error) ErrClass {
	if err == nil {
		return ClassUnknown
	}

	// Network-level: cheapest checks first.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return ClassTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ClassTransient
	}

	msg := strings.ToLower(err.Error())
	switch {
	// Server-signaled throttling: Gmail and Workspace use these phrasings.
	case strings.Contains(msg, "too many simultaneous"):
		return ClassThrottled
	case strings.Contains(msg, "session expired"):
		return ClassThrottled
	case strings.Contains(msg, "exceeded") &&
		(strings.Contains(msg, "bandwidth") ||
			strings.Contains(msg, "limit") ||
			strings.Contains(msg, "quota")):
		return ClassThrottled
	case strings.Contains(msg, "lockout"):
		return ClassThrottled

	// Server-side session logout (Gmail's idle disconnect, post-quota
	// kick). The connection is dead but a fresh login will succeed.
	case strings.Contains(msg, "not logged in"):
		return ClassTransient

	// Permanent: authentication or addressing problems.
	case strings.Contains(msg, "authentication") && strings.Contains(msg, "fail"):
		return ClassPermanent
	case strings.Contains(msg, "invalid credentials"):
		return ClassPermanent
	case strings.Contains(msg, "no such mailbox"):
		return ClassPermanent
	}

	return ClassUnknown
}

// isRetryable reports whether reconnect-and-retry should be attempted for err.
// ClassThrottled is intentionally NOT retryable here — the caller is expected
// to surface it to the user, which gets clearer behaviour than a stuck loop.
func isRetryable(err error) bool {
	return classifyError(err) == ClassTransient
}
