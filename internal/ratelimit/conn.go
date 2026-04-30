// Package ratelimit provides a net.Conn wrapper that throttles read and write
// throughput using a token bucket. It is meant to be applied at the dial level
// so the underlying protocol library (go-imap) is unaware of throttling.
package ratelimit

import (
	"context"
	"net"

	"golang.org/x/time/rate"
)

// minBurst guarantees rate.Limiter.WaitN never fails with "n exceeds burst" —
// IMAP literals can be tens of KB and we always want to be able to wait for
// them, even when the configured BPS is small.
const minBurst = 64 * 1024

// Conn wraps a net.Conn and applies token-bucket rate limiting to Read and Write.
// Either limiter may be nil — that direction is then unlimited.
//
// Conn is safe for concurrent use by multiple goroutines because *rate.Limiter
// is itself concurrency-safe and net.Conn implementations are required to be
// safe for concurrent reads and writes.
type Conn struct {
	net.Conn
	read   *rate.Limiter
	write  *rate.Limiter
	ctx    context.Context
	cancel context.CancelFunc
}

// NewLimiter constructs a *rate.Limiter for the given bytes-per-second budget.
// Returns nil when bps <= 0, signaling "unlimited" to callers.
func NewLimiter(bps int) *rate.Limiter {
	if bps <= 0 {
		return nil
	}
	burst := bps
	if burst < minBurst {
		burst = minBurst
	}
	return rate.NewLimiter(rate.Limit(bps), burst)
}

// New wraps c with the provided limiters. Either limiter may be nil.
func New(c net.Conn, read, write *rate.Limiter) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{Conn: c, read: read, write: write, ctx: ctx, cancel: cancel}
}

// Close cancels any in-flight WaitN call and closes the underlying connection.
func (c *Conn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

// Read reads from the underlying connection, then waits for tokens proportional
// to the bytes returned. Throttling after the read is correct because the
// bytes are already in flight: we just delay the next read.
func (c *Conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.read != nil {
		if werr := waitN(c.ctx, c.read, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// Write throttles before sending so a large write does not produce a network
// burst that could trigger server-side rate limits.
func (c *Conn) Write(p []byte) (int, error) {
	if c.write != nil {
		if err := waitN(c.ctx, c.write, len(p)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

// waitN blocks until n tokens are available, splitting requests larger than
// the limiter's burst capacity. lim.WaitN returns an error when n > burst, so
// large IMAP literals must be chunked.
func waitN(ctx context.Context, lim *rate.Limiter, n int) error {
	burst := lim.Burst()
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := lim.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}
