package boot

import (
	"net/netip"
	"testing"
	"time"
)

// fakeClock is a settable clock for the limiter.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestWindowLimiter(t *testing.T) {
	const window = 10 * time.Minute
	a, b, c := netip.MustParseAddr("10.10.20.50"), netip.MustParseAddr("10.10.20.51"), netip.MustParseAddr("10.10.20.52")

	t.Run("limit then refuse then reopen", func(t *testing.T) {
		clock := &fakeClock{t: testNow()}
		l := newWindowLimiter[netip.Addr](8, window, 4096, clock.now)
		for i := range 8 {
			if !l.Allow(a) {
				t.Fatalf("Allow #%d = false, want true", i+1)
			}
		}
		if l.Allow(a) {
			t.Fatal("Allow #9 = true, want false")
		}

		clock.t = clock.t.Add(window + time.Second)

		if !l.Allow(a) {
			t.Error("Allow after the window = false, want true")
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		clock := &fakeClock{t: testNow()}
		l := newWindowLimiter[netip.Addr](1, window, 4096, clock.now)
		if !l.Allow(a) || l.Allow(a) {
			t.Fatal("key a: want one token")
		}
		if !l.Allow(b) {
			t.Error("Allow(b) = false, want true: b has its own window")
		}
	})

	t.Run("window is fixed from the first hit", func(t *testing.T) {
		clock := &fakeClock{t: testNow()}
		l := newWindowLimiter[netip.Addr](2, window, 4096, clock.now)
		l.Allow(a)
		clock.t = clock.t.Add(window - time.Second)
		l.Allow(a)
		if l.Allow(a) {
			t.Error("third Allow inside the window = true, want false")
		}
	})

	t.Run("capacity refuses new keys until the window passes", func(t *testing.T) {
		clock := &fakeClock{t: testNow()}
		l := newWindowLimiter[netip.Addr](8, window, 2, clock.now)
		if !l.Allow(a) || !l.Allow(b) {
			t.Fatal("the first two keys must be allowed")
		}
		if l.Allow(c) {
			t.Error("third key at capacity = true, want false")
		}
		if !l.Allow(a) {
			t.Error("a tracked key at capacity = false, want true")
		}

		clock.t = clock.t.Add(window + time.Second)

		if !l.Allow(c) {
			t.Error("third key after the windows passed = false, want true")
		}
	})
}

func TestWindowLimiter_Concurrent(t *testing.T) {
	l := newWindowLimiter[netip.Addr](100, time.Hour, 4096, testNow)
	addr := netip.MustParseAddr("10.10.20.50")
	allowed := make(chan bool, 400)

	for range 400 {
		go func() { allowed <- l.Allow(addr) }()
	}

	var n int
	for range 400 {
		if <-allowed {
			n++
		}
	}
	if n != 100 {
		t.Errorf("allowed = %d, want exactly the limit of 100", n)
	}
}
