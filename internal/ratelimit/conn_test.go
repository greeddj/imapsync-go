package ratelimit

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestNewLimiter(t *testing.T) {
	t.Parallel()

	if got := NewLimiter(0); got != nil {
		t.Errorf("NewLimiter(0) = %v, want nil", got)
	}
	if got := NewLimiter(-1); got != nil {
		t.Errorf("NewLimiter(-1) = %v, want nil", got)
	}

	l := NewLimiter(1000)
	if l == nil {
		t.Fatal("NewLimiter(1000) returned nil")
	}
	if l.Burst() < minBurst {
		t.Errorf("burst = %d, want >= %d", l.Burst(), minBurst)
	}

	l2 := NewLimiter(10 * 1024 * 1024)
	if l2.Burst() != 10*1024*1024 {
		t.Errorf("burst = %d, want %d", l2.Burst(), 10*1024*1024)
	}
}

// fakeConn is a minimal net.Conn that returns canned bytes on Read and
// captures writes. Other net.Conn methods are unused by these tests and panic
// to fail loudly if hit.
type fakeConn struct {
	readBuf  []byte
	writeBuf []byte
}

func (f *fakeConn) Read(p []byte) (int, error) {
	if len(f.readBuf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.readBuf)
	f.readBuf = f.readBuf[n:]
	return n, nil
}

func (f *fakeConn) Write(p []byte) (int, error) {
	f.writeBuf = append(f.writeBuf, p...)
	return len(p), nil
}

func (f *fakeConn) Close() error                     { return nil }
func (f *fakeConn) LocalAddr() net.Addr              { panic("unused") }
func (f *fakeConn) RemoteAddr() net.Addr             { panic("unused") }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestConnWriteThrottles(t *testing.T) {
	t.Parallel()

	// 1 KB/s throttle, burst = minBurst (64 KB).
	lim := NewLimiter(1024)
	fake := &fakeConn{}
	c := New(fake, nil, lim)

	// Drain the bucket first so subsequent writes must wait.
	if err := lim.WaitN(context.Background(), lim.Burst()); err != nil {
		t.Fatalf("priming wait: %v", err)
	}

	start := time.Now()
	// Write 256 bytes - at 1 KB/s, this should take ~250 ms.
	payload := make([]byte, 256)
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("Write returned in %s, expected throttling delay", elapsed)
	}
}

func TestConnCloseCancelsWait(t *testing.T) {
	t.Parallel()

	// Tight bucket: 1 byte/s, large request will block.
	lim := NewLimiter(1)
	fake := &fakeConn{}
	c := New(fake, nil, lim)

	// Drain so next Write blocks.
	_ = lim.WaitN(context.Background(), lim.Burst())

	done := make(chan error, 1)
	go func() {
		_, err := c.Write(make([]byte, lim.Burst()))
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Write err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not return after Close")
	}
}

func TestConnReadPassthroughWhenUnlimited(t *testing.T) {
	t.Parallel()

	fake := &fakeConn{readBuf: []byte("hello")}
	c := New(fake, nil, nil)

	buf := make([]byte, 5)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Errorf("Read returned %q (n=%d)", buf[:n], n)
	}
}

// Sanity: WaitN with chunk > burst must succeed via splitting.
func TestWaitNChunksLargerThanBurst(t *testing.T) {
	t.Parallel()

	lim := rate.NewLimiter(1024*1024, 64*1024) // 1 MB/s, 64 KB burst
	if err := waitN(context.Background(), lim, 256*1024); err != nil {
		t.Fatalf("waitN: %v", err)
	}
}

// putInDebt drains the bucket and then borrows one more burst, so a call that waits
// for tokens would block for a very long time.
func putInDebt(lim *rate.Limiter) {
	lim.ReserveN(time.Now(), lim.Burst())
	lim.ReserveN(time.Now(), lim.Burst())
}

// returnsWithin fails the test when fn is still running after d.
func returnsWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatalf("%s did not return within %s", what, d)
	}
}

func TestConn_When_WaitIsOff_Then_ReadAndWriteTakeTokensWithoutWaiting(t *testing.T) {
	t.Parallel()

	const burst = 1 << 12
	// A near-zero refill rate keeps the token counts stable during the test.
	readLim := rate.NewLimiter(rate.Limit(1e-9), burst)
	writeLim := rate.NewLimiter(rate.Limit(1e-9), burst)
	putInDebt(readLim)
	putInDebt(writeLim)
	readBefore, writeBefore := readLim.Tokens(), writeLim.Tokens()

	fake := &fakeConn{readBuf: []byte("hello")}
	c := New(fake, readLim, writeLim)
	t.Cleanup(func() { _ = c.Close() })
	c.SetWait(false)

	payload := make([]byte, 256)
	buf := make([]byte, 5)
	var (
		wn, rn     int
		werr, rerr error
	)
	returnsWithin(t, time.Second, "Write and Read", func() {
		wn, werr = c.Write(payload)
		rn, rerr = c.Read(buf)
	})

	if werr != nil || wn != len(payload) {
		t.Errorf("Write = (%d, %v), want (%d, nil)", wn, werr, len(payload))
	}
	if got := string(buf[:rn]); rerr != nil || got != "hello" {
		t.Errorf("Read = (%q, %v), want (%q, nil)", got, rerr, "hello")
	}
	if len(fake.writeBuf) != len(payload) {
		t.Errorf("the connection received %d bytes, want %d", len(fake.writeBuf), len(payload))
	}
	if spent := writeBefore - writeLim.Tokens(); spent < float64(len(payload))-1 || spent > float64(len(payload))+1 {
		t.Errorf("write limiter spent %v tokens, want %d", spent, len(payload))
	}
	if spent := readBefore - readLim.Tokens(); spent < 4 || spent > 6 {
		t.Errorf("read limiter spent %v tokens, want 5", spent)
	}
}

func TestConn_When_WaitIsOff_Then_ChargeSplitsAtTheBurst(t *testing.T) {
	t.Parallel()

	const burst = 1 << 10
	lim := rate.NewLimiter(rate.Limit(1e-9), burst)
	putInDebt(lim)
	before := lim.Tokens()

	fake := &fakeConn{}
	c := New(fake, nil, lim)
	t.Cleanup(func() { _ = c.Close() })
	c.SetWait(false)

	size := 3*burst + 1
	var (
		n   int
		err error
	)
	returnsWithin(t, time.Second, "Write", func() { n, err = c.Write(make([]byte, size)) })

	if err != nil || n != size {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, size)
	}
	if spent := before - lim.Tokens(); spent < float64(size)-1 || spent > float64(size)+1 {
		t.Errorf("limiter spent %v tokens, want %d", spent, size)
	}
}

func TestConn_When_WaitTurnsBackOn_Then_WritesWaitAgain(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(1024)
	fake := &fakeConn{}
	c := New(fake, nil, lim)
	t.Cleanup(func() { _ = c.Close() })

	c.SetWait(false)
	c.SetWait(true)

	// Drain the bucket first so the write must wait.
	if err := lim.WaitN(context.Background(), lim.Burst()); err != nil {
		t.Fatalf("priming wait: %v", err)
	}

	start := time.Now()
	if _, err := c.Write(make([]byte, 256)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("Write returned in %s, expected throttling delay", elapsed)
	}
}

func TestConn_When_WaitTurnsOnDuringARead_Then_ThatReadWaits(t *testing.T) {
	t.Parallel()

	local, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	lim := rate.NewLimiter(1, 1024)
	putInDebt(lim)
	c := New(local, lim, nil)
	t.Cleanup(func() { _ = c.Close() })
	c.SetWait(false)

	type result struct {
		err error
		n   int
	}
	done := make(chan result, 1)
	go func() {
		n, err := c.Read(make([]byte, 16))
		done <- result{n: n, err: err}
	}()

	time.Sleep(20 * time.Millisecond)
	c.SetWait(true)
	if _, err := peer.Write([]byte("hello")); err != nil {
		t.Fatalf("peer Write: %v", err)
	}

	select {
	case r := <-done:
		t.Fatalf("Read returned (%d, %v), want it blocked on tokens", r.n, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case r := <-done:
		if r.n != 5 || !errors.Is(r.err, context.Canceled) {
			t.Errorf("Read = (%d, %v), want (5, context.Canceled)", r.n, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after Close")
	}
}

func TestConn_When_LimitersAreNilAndWaitIsOff_Then_PassesThrough(t *testing.T) {
	t.Parallel()

	fake := &fakeConn{readBuf: []byte("hello")}
	c := New(fake, nil, nil)
	t.Cleanup(func() { _ = c.Close() })
	c.SetWait(false)

	buf := make([]byte, 5)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Errorf("Read = (%q, %v), want (%q, nil)", buf[:n], err, "hello")
	}
	n, err = c.Write([]byte("abc"))
	if err != nil || n != 3 || string(fake.writeBuf) != "abc" {
		t.Errorf("Write = (%d, %v), the connection got %q, want (3, nil) and %q", n, err, fake.writeBuf, "abc")
	}
}

func Test_chargeN_When_BurstIsZero_Then_Returns(t *testing.T) {
	t.Parallel()

	lim := rate.NewLimiter(1, 0)
	returnsWithin(t, time.Second, "chargeN", func() { chargeN(lim, 100) })
}
