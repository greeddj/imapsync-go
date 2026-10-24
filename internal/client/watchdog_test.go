package client

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// newPipedConn wraps one end of an in-memory pipe in a watchdog. The returned
// peer is the other end; it stays silent unless the test writes to it.
func newPipedConn(t *testing.T, limit time.Duration) (*idleConn, net.Conn) {
	t.Helper()

	local, peer := net.Pipe()
	w := newIdleConn(local, limit)
	t.Cleanup(func() {
		_ = w.Close()
		_ = peer.Close()
	})
	return w, peer
}

// startRead blocks one Read on w in a goroutine and delivers its error.
func startRead(w *idleConn) <-chan error {
	errc := make(chan error, 1)
	go func() {
		_, err := w.Read(make([]byte, 16))
		errc <- err
	}()
	return errc
}

// tripConn opens a span on a silent pipe and returns once the watchdog has cut
// the pending Read, with the error that Read returned.
func tripConn(t *testing.T, limit time.Duration) (*idleConn, error) {
	t.Helper()

	w, _ := newPipedConn(t, limit)
	errc := startRead(w)
	w.begin()
	err := awaitError(t, errc, "the Read")
	if err == nil {
		t.Fatal("Read returned nil, want an error after the trip")
	}
	return w, err
}

func Test_idleConn_When_SpanOpenAndPeerSilent_Then_ReadFailsAfterLimit(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, tripLimit)
	errc := startRead(w)

	start := time.Now()
	w.begin()
	err := awaitError(t, errc, "the Read")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Read returned nil, want an error after the trip")
	}
	if elapsed < tripLimit {
		t.Errorf("Read failed after %s, want at least the %s limit", elapsed, tripLimit)
	}
	if !w.tripped() {
		t.Error("tripped() = false, want true")
	}

	got := w.idleErr(err)
	for _, want := range []error{errIdleTimeout, os.ErrDeadlineExceeded, err} {
		if !errors.Is(got, want) {
			t.Errorf("idleErr(%q) does not wrap %q", got, want)
		}
	}
	if class := classifyError(got); class != ClassTransient {
		t.Errorf("classifyError(%q) = %s, want %s", got, class, ClassTransient)
	}
	if !strings.Contains(got.Error(), tripLimit.String()) {
		t.Errorf("idleErr text %q does not name the %s limit", got, tripLimit)
	}
}

func Test_idleConn_When_WriteBlockedAndSpanOpen_Then_WriteFailsAfterLimit(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, tripLimit)
	errc := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("never read by the peer"))
		errc <- err
	}()

	w.begin()
	if err := awaitError(t, errc, "the Write"); err == nil {
		t.Error("Write returned nil, want an error after the trip")
	}
	if !w.tripped() {
		t.Error("tripped() = false, want true")
	}
}

func Test_idleConn_When_NoSpanOpen_Then_SilentReadIsNotCut(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, holdLimit)
	errc := startRead(w)

	timer := time.NewTimer(3*holdLimit + holdLimit/2)
	defer timer.Stop()
	select {
	case err := <-errc:
		t.Fatalf("Read ended without a span: %v", err)
	case <-timer.C:
	}
	if w.tripped() {
		t.Error("tripped() = true without a span")
	}
}

func Test_idleConn_When_SpanOpenButNothingBlockedOnTransport_Then_DoesNotTrip(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, holdLimit)
	w.begin()

	time.Sleep(3*holdLimit + holdLimit/2)
	if w.tripped() {
		t.Error("tripped() = true while no Read or Write was blocked")
	}
}

func Test_idleConn_When_PeerTricklesWithinLimit_Then_DoesNotTrip(t *testing.T) {
	t.Parallel()

	const (
		gap   = holdLimit / 10
		count = 40
	)
	if gap*count < 3*holdLimit {
		t.Fatalf("trickle lasts %s, want at least 3 limits", gap*count)
	}

	w, peer := newPipedConn(t, holdLimit)
	w.begin()
	go func() {
		for range count {
			if _, err := peer.Write([]byte{'x'}); err != nil {
				return
			}
			time.Sleep(gap)
		}
	}()

	buf := make([]byte, 16)
	for i := range count {
		if _, err := w.Read(buf); err != nil {
			t.Fatalf("Read %d: %v, want the trickle to keep the connection alive", i, err)
		}
	}
	if w.tripped() {
		t.Error("tripped() = true while bytes kept arriving")
	}
}

func Test_idleConn_When_ClosedDuringSpan_Then_ItIsNotATrip(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, tripLimit)
	errc := startRead(w)
	w.begin()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := awaitError(t, errc, "the Read")
	if err == nil {
		t.Fatal("Read returned nil after Close, want an error")
	}

	// A fire that raced the Close must find the connection closed and stand down.
	w.fire()
	if w.tripped() {
		t.Error("tripped() = true after a plain Close")
	}
	got := w.idleErr(err)
	if !errors.Is(got, err) || got.Error() != err.Error() {
		t.Errorf("idleErr(%q) = %q, want the error unchanged", err, got)
	}
	if errors.Is(got, errIdleTimeout) {
		t.Error("a closed connection reads as an idle timeout")
	}
	if w.timer.Stop() {
		t.Error("timer still armed after Close")
	}
}

func Test_idleConn_When_BeginRunsAfterClose_Then_NoTimerSurvives(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, tripLimit)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w.begin()
	if w.timer.Stop() {
		t.Error("begin armed a timer on a closed connection")
	}
}

func Test_idleConn_When_Nil_Then_SpansAreNoOps(t *testing.T) {
	t.Parallel()

	var w *idleConn
	w.begin()
	w.end()

	if w.tripped() {
		t.Error("nil tripped() = true")
	}
	if got := w.idleErr(nil); got != nil {
		t.Errorf("nil idleErr(nil) = %v, want nil", got)
	}
	want := errors.New("boom")
	if got := w.idleErr(want); !errors.Is(got, want) || got.Error() != want.Error() {
		t.Errorf("nil idleErr(%q) = %q, want it unchanged", want, got)
	}
}

func Test_idleConn_When_Tripped_Then_NilErrorStaysNil(t *testing.T) {
	t.Parallel()

	w, err := tripConn(t, tripLimit)
	if got := w.idleErr(nil); got != nil {
		t.Errorf("idleErr(nil) = %v after a trip, want nil", got)
	}
	if !errors.Is(w.idleErr(err), errIdleTimeout) {
		t.Error("a non-nil error after a trip does not read as an idle timeout")
	}
}

func Test_idleConn_When_ClosedAfterTrip_Then_StaysTripped(t *testing.T) {
	t.Parallel()

	w, err := tripConn(t, tripLimit)
	_ = w.Close()

	if !w.tripped() {
		t.Error("tripped() = false after Close following a trip, want true")
	}
	if !errors.Is(w.idleErr(err), errIdleTimeout) {
		t.Error("idleErr lost the trip after Close")
	}
}

func Test_idleConn_When_SpansNest_Then_OpenUntilTheLastEnds(t *testing.T) {
	t.Parallel()

	w, _ := newPipedConn(t, tripLimit)
	errc := startRead(w)
	w.begin()
	w.begin()
	w.end()

	if err := awaitError(t, errc, "the Read"); err == nil {
		t.Error("Read returned nil, want the trip of the span that is still open")
	}
	if !w.tripped() {
		t.Error("tripped() = false, want true")
	}
}

// blockedCall runs call in a goroutine and delivers the moment it returned.
func blockedCall(call func()) <-chan time.Time {
	done := make(chan time.Time, 1)
	go func() {
		call()
		done <- time.Now()
	}()
	return done
}

func awaitInstant(t *testing.T, ch <-chan time.Time, what string) time.Time {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case at := <-ch:
		return at
	case <-timer.C:
		t.Fatalf("%s still blocked 5s after the trip was due", what)
		return time.Time{}
	}
}

// The assertion is a lower bound on the trip time, so a late timer cannot fail it.
func Test_idleConn_When_ActivityRestartsIdleClock_Then_TripWaitsFullLimitAfterIt(t *testing.T) {
	t.Parallel()

	const (
		limit = 2 * holdLimit
		pause = limit / 2
		floor = limit * 3 / 4
	)

	tests := []struct {
		activity func(t *testing.T, w *idleConn, peer net.Conn) (restart time.Time, tripped <-chan time.Time)
		name     string
	}{
		{
			name: "a second begin",
			activity: func(t *testing.T, w *idleConn, _ net.Conn) (time.Time, <-chan time.Time) {
				t.Helper()

				tripped := blockedCall(func() { _, _ = w.Read(make([]byte, 1)) })
				w.begin()
				time.Sleep(pause)
				w.begin()
				return time.Now(), tripped
			},
		},
		{
			name: "a Read that returns bytes",
			activity: func(t *testing.T, w *idleConn, peer net.Conn) (time.Time, <-chan time.Time) {
				t.Helper()

				tripped := blockedCall(func() { _, _ = w.Write([]byte("never read by the peer")) })
				read := blockedCall(func() { _, _ = w.Read(make([]byte, 1)) })
				w.begin()
				time.Sleep(pause)
				if _, err := peer.Write([]byte{'x'}); err != nil {
					t.Fatalf("peer Write: %v", err)
				}
				return awaitInstant(t, read, "the Read"), tripped
			},
		},
		{
			name: "a Write that completes",
			activity: func(t *testing.T, w *idleConn, peer net.Conn) (time.Time, <-chan time.Time) {
				t.Helper()

				tripped := blockedCall(func() { _, _ = w.Read(make([]byte, 1)) })
				written := blockedCall(func() { _, _ = w.Write([]byte{'x'}) })
				w.begin()
				time.Sleep(pause)
				if _, err := peer.Read(make([]byte, 1)); err != nil {
					t.Fatalf("peer Read: %v", err)
				}
				return awaitInstant(t, written, "the Write"), tripped
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, peer := newPipedConn(t, limit)
			restart, tripped := tt.activity(t, w, peer)
			trip := awaitInstant(t, tripped, "the blocked call")

			if got := trip.Sub(restart); got < floor {
				t.Errorf("trip came %s after the activity, want at least %s of the %s limit", got, floor, limit)
			}
			if !w.tripped() {
				t.Error("tripped() = false, want true after the silence that followed")
			}
		})
	}
}

func TestMinUploadBPS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uploaders int
		want      int
	}{
		{uploaders: 0, want: 110},
		{uploaders: 1, want: 110},
		{uploaders: 10, want: 1093},
	}
	for _, tt := range tests {
		got := MinUploadBPS(tt.uploaders)
		if got != tt.want {
			t.Errorf("MinUploadBPS(%d) = %d, want %d", tt.uploaders, got, tt.want)
		}
		// At the returned limit each uploader's share moves one literal write
		// within the idle limit.
		share := float64(got) / float64(max(tt.uploaders, 1))
		if wait := time.Duration(float64(maxLiteralWrite) / share * float64(time.Second)); wait > idleTimeout {
			t.Errorf("MinUploadBPS(%d) = %d leaves a %s wait for one write, want at most %s", tt.uploaders, got, wait, idleTimeout)
		}
	}
}
