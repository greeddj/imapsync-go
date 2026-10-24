package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

var errIdleTimeout = errors.New("no data from server")

const (
	idleOpen int32 = iota
	idleClosed
	idleTripped
)

// idleConn closes the transport when a span is open, a Read or Write is blocked
// on it, and no byte has moved for limit.
type idleConn struct {
	net.Conn
	timer  *time.Timer
	start  time.Time
	limit  time.Duration
	last   atomic.Int64
	active atomic.Int32
	busy   atomic.Int32
	state  atomic.Int32
}

func newIdleConn(conn net.Conn, limit time.Duration) *idleConn {
	w := &idleConn{Conn: conn, start: time.Now(), limit: limit}
	w.touch()
	w.timer = time.AfterFunc(limit, w.fire)
	w.timer.Stop()
	return w
}

// touch stamps the monotonic offset from start; a wall-clock stamp could trip
// the watchdog on a clock step.
func (w *idleConn) touch() { w.last.Store(int64(time.Since(w.start))) }

// Read stamps before it counts itself busy, so a fire that sees the call busy
// also sees its entry stamp.
func (w *idleConn) Read(p []byte) (int, error) {
	w.touch()
	w.busy.Add(1)
	n, err := w.Conn.Read(p)
	if n > 0 {
		w.touch()
	}
	w.busy.Add(-1)
	return n, err
}

func (w *idleConn) Write(p []byte) (int, error) {
	w.touch()
	w.busy.Add(1)
	n, err := w.Conn.Write(p)
	if n > 0 {
		w.touch()
	}
	w.busy.Add(-1)
	return n, err
}

// begin stamps first: the reader's Read has been pending since the last command,
// and a stale fire must not see that old stamp with an open span.
func (w *idleConn) begin() {
	if w == nil {
		return
	}
	w.touch()
	if w.active.Add(1) == 1 {
		w.rearm(w.limit)
	}
}

// end leaves the timer alone: stopping it could race a concurrent begin's
// Reset and disarm an open span. A pending fire finds no span and goes dormant.
func (w *idleConn) end() {
	if w == nil {
		return
	}
	w.active.Add(-1)
}

func (w *idleConn) fire() {
	if w.state.Load() != idleOpen || w.active.Load() == 0 {
		return
	}
	// Nothing waits on the server: our own work, or a rate-limit wait outside this wrapper.
	if w.busy.Load() == 0 {
		w.rearm(w.limit)
		return
	}
	idle := time.Duration(int64(time.Since(w.start)) - w.last.Load())
	if idle < w.limit {
		w.rearm(w.limit - idle)
		return
	}
	if w.state.CompareAndSwap(idleOpen, idleTripped) {
		_ = w.Conn.Close()
	}
}

// rearm rechecks the state after Reset, so a Close that ran in between cannot
// leave a live timer behind.
func (w *idleConn) rearm(d time.Duration) {
	w.timer.Reset(d)
	if w.state.Load() != idleOpen {
		w.timer.Stop()
	}
}

// Close keeps a tripped state tripped, so idleErr still tells the trip from a
// close by Cancel.
func (w *idleConn) Close() error {
	w.state.CompareAndSwap(idleOpen, idleClosed)
	w.timer.Stop()
	return w.Conn.Close()
}

func (w *idleConn) tripped() bool {
	return w != nil && w.state.Load() == idleTripped
}

// idleErr marks err as a watchdog timeout when the watchdog closed the
// connection. The chain keeps err and os.ErrDeadlineExceeded, which classifyError
// treats as transient.
func (w *idleConn) idleErr(err error) error {
	if err == nil || !w.tripped() {
		return err
	}
	return fmt.Errorf("%w: %w within %s: %w", err, errIdleTimeout, w.limit, os.ErrDeadlineExceeded)
}
