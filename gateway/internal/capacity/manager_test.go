package capacity

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAtomicLimitsAndIdempotentRelease(t *testing.T) {
	m := New(2)
	a, err := m.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire("a"); err != ErrFull {
		t.Fatal(err)
	}
	b, err := m.Acquire("b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire("c"); err != ErrFull {
		t.Fatal(err)
	}
	m.SetLimit(1)
	a.Release()
	a.Release()
	if _, err := m.Acquire("c"); err != ErrFull {
		t.Fatal("limit reduction discarded active work", err)
	}
	b.Release()
	c, err := m.Acquire("c")
	if err != nil {
		t.Fatal(err)
	}
	c.Release()
}
func TestConcurrentAcquire(t *testing.T) {
	m := New(1)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Acquire("a"); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
}
func TestQuarantineRequiresProof(t *testing.T) {
	m := New(1)
	l, _ := m.Acquire("a")
	l.Quarantine(false)
	if m.Recover(context.Background(), "a", func(context.Context) (bool, error) { t.Fatal("ambiguous dispatch was probed"); return true, nil }) {
		t.Fatal("unknown dispatch recovered")
	}
	if _, err := m.Acquire("a"); err != ErrUnconfirmed {
		t.Fatal(err)
	}
	l.Release()
	l, _ = m.Acquire("a")
	l.Quarantine(true)
	if m.Recover(context.Background(), "a", func(context.Context) (bool, error) { return false, nil }) {
		t.Fatal("busy recovered")
	}
	if !m.Recover(context.Background(), "a", func(context.Context) (bool, error) { return true, nil }) {
		t.Fatal("idle not recovered")
	}
	l.Release()
	if _, err := m.Acquire("b"); err != nil {
		t.Fatal(err)
	}
}
