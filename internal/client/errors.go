package client

import (
	"errors"
	"io"
	"net"
	"strconv"
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
	// failure (EOF, closed conn, timeout), a server-side logout or an
	// authentication service outage. Reconnect-and-retry is appropriate.
	ClassTransient
	// ClassPermanent indicates an authoritative refusal (auth failure, locked or
	// disabled account, bad command, missing mailbox). Retry will not help.
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

// ErrSelectRefused marks a NO or BAD reply to SELECT or EXAMINE on a working session, in a Message-Id scan, a body stream or an existence check.
var ErrSelectRefused = errors.New("cannot select folder")

// ErrEmptyBody marks a message whose source body is empty. AppendMessage returns it before it sends any command.
var ErrEmptyBody = errors.New("source returned an empty body")

// ErrMissingBody marks a message that has no body section. AppendMessage returns it before it sends any command.
var ErrMissingBody = errors.New("message has no body")

// errMissingUID marks a scanned message that has a Message-Id but no UID; go-imap would send UID 0 as "*".
var errMissingUID = errors.New("server sent no UID for a message")

// ErrUIDValidityChanged marks a source folder that no longer reports the UIDVALIDITY its planned UIDs belong to.
var ErrUIDValidityChanged = errors.New("source folder changed after the sync plan, run sync again")

// errStreamStopped ends a StreamMessagesByUIDs attempt after a halt or an onMessage error. Its text matches no class, so safeCall returns it without a retry.
var errStreamStopped = errors.New("stream stopped by the caller")

// StreamError is the result of StreamMessagesByUIDs when a batch failed. First is the first failure and Stop, if set, the one that ended the stream.
type StreamError struct {
	First         error
	Stop          error
	FailedBatches int
}

func (e *StreamError) Error() string {
	if e.FailedBatches <= 1 {
		return e.First.Error()
	}
	return e.First.Error() + " (" + strconv.Itoa(e.FailedBatches) + " batches failed)"
}

// Stop comes first so classifyLeaves reads it before First. classifyError checks io.EOF and net.Error on the whole tree first, so callers pass Stop to IsThrottled.
func (e *StreamError) Unwrap() []error {
	if e.Stop == nil {
		return []error{e.First}
	}
	return []error{e.Stop, e.First}
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

// Only leaf errors decide: wrappers add our folder names and labels, which
// must not choose the class.
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
	// A login to a locked or disabled account can extend the lock, so this state
	// outranks the throttle and outage phrases and allows no reconnect or revive.
	case strings.Contains(msg, "locked"),
		strings.Contains(msg, "lockout"),
		strings.Contains(msg, "disabled"),
		strings.Contains(msg, "suspended"),
		strings.Contains(msg, "blocked"):
		return ClassPermanent

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

	// Server-side session logout (Gmail's idle disconnect, post-quota
	// kick). The connection is dead but a fresh login will succeed.
	case strings.Contains(msg, "not logged in"):
		return ClassTransient
	// go-imap's unexported errors for a socket closed under a pending
	// command; safeCall has already ruled out our own Cancel.
	case strings.Contains(msg, "imap: connection closed"):
		return ClassTransient
	// Auth-backend outage: Dovecot's [UNAVAILABLE] text or "temporarily unavailable".
	// go-imap drops the code, so the text decides. Must precede the permanent auth rule.
	case strings.Contains(msg, "temporary authentication failure"),
		strings.Contains(msg, "authentication") && strings.Contains(msg, "temporarily unavailable"):
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

// IsThrottled reports whether classifyError puts err in ClassThrottled, a rate limit or a full quota.
func IsThrottled(err error) bool {
	return classifyError(err) == ClassThrottled
}

// isRetryable reports whether reconnect-and-retry should be attempted for err.
// ClassThrottled is intentionally NOT retryable here - the caller is expected
// to surface it to the user, which gets clearer behaviour than a stuck loop.
func isRetryable(err error) bool {
	return classifyError(err) == ClassTransient
}

// canRevive excludes Unknown, because servers say "LOGIN failed" for wrong credentials too.
func canRevive(err error) bool {
	class := classifyError(err)
	return class == ClassTransient || class == ClassThrottled
}

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
