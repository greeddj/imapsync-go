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
	// Write 256 bytes — at 1 KB/s, this should take ~250 ms.
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
