package client

import (
	"errors"
	"io"
	"net"
	"strings"

	imapclient "github.com/emersion/go-imap/client"
)

// ErrClass tells the reconnect machinery whether to retry, give up or back off.
// go-imap v1 drops RFC 5530 codes such as [UNAVAILABLE] from errors, so
// classification matches error text.
type ErrClass int

const (
	// ClassUnknown is the zero value: the error has not been classified.
	// Caller should treat it as non-retryable to avoid loops on novel errors.
	ClassUnknown ErrClass = iota
	// ClassTransient indicates a failure a fresh session can clear: a network
	// failure (EOF, closed conn, timeout), a server-side logout or a temporary
	// authentication failure. Reconnect-and-retry is appropriate.
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
	if _, ok := errors.AsType[net.Error](err); ok {
		return ClassTransient
	}
	// go-imap returns this without sending a command when a BYE ended the session
	// after the SELECT; a fresh session clears it.
	if errors.Is(err, imapclient.ErrNoMailboxSelected) {
		return ClassTransient
	}

	return classifyLeaves(err)
}

// classifyLeaves matches text only on leaf errors. Wrappers add our folder
// names and labels, which must not choose the class.
func classifyLeaves(err error) ErrClass {
	if child := errors.Unwrap(err); child != nil {
		return classifyLeaves(child)
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		hasChild := false
		for _, child := range multi.Unwrap() {
			if child == nil {
				continue
			}
			hasChild = true
			if class := classifyLeaves(child); class != ClassUnknown {
				return class
			}
		}
		if hasChild {
			return ClassUnknown
		}
	}
	return classifyMessage(strings.ToLower(err.Error()))
}

func classifyMessage(msg string) ErrClass {
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
	// go-imap's unexported errors for a socket closed under a pending
	// command; safeCall has already ruled out our own Cancel.
	case strings.Contains(msg, "imap: connection closed"):
		return ClassTransient
	// Dovecot's [UNAVAILABLE] auth-backend outage; go-imap drops the code,
	// so the text decides. Must precede the permanent auth rule.
	case strings.Contains(msg, "authentication") && strings.Contains(msg, "temporar"):
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
// ClassThrottled is intentionally NOT retryable here - the caller is expected
// to surface it to the user, which gets clearer behaviour than a stuck loop.
func isRetryable(err error) bool {
	return classifyError(err) == ClassTransient
}

// isAlreadyExistsErr reports whether a CREATE refusal means the mailbox exists.
// go-imap v1 drops the [ALREADYEXISTS] code, so the text decides; Gmail says
// "Duplicate folder name".
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "mailbox exists") ||
		strings.Contains(msg, "duplicate folder name")
}
