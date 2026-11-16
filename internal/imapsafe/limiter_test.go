package imapsafe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionLimiter_QueuesBeyondLimit(t *testing.T) {
	l := NewLimiter(4)
	var cur, peak, done atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, _, err := l.Acquire(context.Background(), "alice")
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			cur.Add(-1)
			release()
			release() // a second call must be a no-op
			done.Add(1)
		}()
	}
	wg.Wait()
	if peak.Load() != 4 {
		t.Errorf("peak = %d, want 4", peak.Load())
	}
	if done.Load() != 10 {
		t.Errorf("done = %d, want 10", done.Load())
	}
}

func TestSessionLimiter_PerUser(t *testing.T) {
	l := NewLimiter(1)
	relA, _, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer relA()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	relB, waited, err := l.Acquire(ctx, "b")
	if err != nil {
		t.Fatalf("user b blocked: %v", err)
	}
	relB()
	if waited != 0 {
		t.Errorf("waited = %v, want 0", waited)
	}
}

func TestSessionLimiter_ContextCancel(t *testing.T) {
	l := NewLimiter(1)
	rel, _, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := l.Acquire(ctx, "a")
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after cancel")
	}

	// The slot is still usable after a cancelled wait.
	rel()
	rel2, waited, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if waited != 0 {
		t.Errorf("waited = %v, want 0", waited)
	}
}

func TestSessionLimiter_ReportsWait(t *testing.T) {
	l := NewLimiter(1)
	rel, _, _ := l.Acquire(context.Background(), "a")
	time.AfterFunc(30*time.Millisecond, rel)
	rel2, waited, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if waited < 20*time.Millisecond {
		t.Errorf("waited = %v", waited)
	}
}

func TestSessionLimiter_UsernameCaseShareSlot(t *testing.T) {
	l := NewLimiter(1)
	rel, _, err := l.Acquire(context.Background(), "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := l.Acquire(ctx, "alice@example.com"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("case variant got its own slot: err = %v", err)
	}
	rel()
	if len(l.users) != 0 {
		t.Errorf("users left: %v", l.users)
	}
}
