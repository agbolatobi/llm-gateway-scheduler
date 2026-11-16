package capacity

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAtomicLimitsAndIdempotentRelease(testContext *testing.T) {
	capacityManager := New(2)
	firstLease, err := capacityManager.Acquire("a")
	if err != nil {
		testContext.Fatal(err)
	}
	if _, err := capacityManager.Acquire("a"); err != ErrFull {
		testContext.Fatal(err)
	}
	secondLease, err := capacityManager.Acquire("b")
	if err != nil {
		testContext.Fatal(err)
	}
	if _, err := capacityManager.Acquire("c"); err != ErrFull {
		testContext.Fatal(err)
	}
	capacityManager.SetLimit(1)
	firstLease.Release()
	firstLease.Release()
	if _, err := capacityManager.Acquire("c"); err != ErrFull {
		testContext.Fatal("limit reduction discarded active work", err)
	}
	secondLease.Release()
	thirdLease, err := capacityManager.Acquire("c")
	if err != nil {
		testContext.Fatal(err)
	}
	thirdLease.Release()
}
func TestConcurrentAcquire(testContext *testing.T) {
	capacityManager := New(1)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for index := 0; index < 64; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := capacityManager.Acquire("a"); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		testContext.Fatal(wins.Load())
	}
}
func TestQuarantineRequiresProof(testContext *testing.T) {
	capacityManager := New(1)
	lease, _ := capacityManager.Acquire("a")
	lease.Quarantine(false)
	if capacityManager.Recover(context.Background(), "a", func(context.Context) (bool, error) {
		testContext.Fatal("ambiguous dispatch was probed")
		return true, nil
	}) {
		testContext.Fatal("unknown dispatch recovered")
	}
	if _, err := capacityManager.Acquire("a"); err != ErrUnconfirmed {
		testContext.Fatal(err)
	}
	lease.Release()
	lease, _ = capacityManager.Acquire("a")
	lease.Quarantine(true)
	if capacityManager.Recover(context.Background(), "a", func(context.Context) (bool, error) { return false, nil }) {
		testContext.Fatal("busy recovered")
	}
	if !capacityManager.Recover(context.Background(), "a", func(context.Context) (bool, error) { return true, nil }) {
		testContext.Fatal("idle not recovered")
	}
	lease.Release()
	if _, err := capacityManager.Acquire("b"); err != nil {
		testContext.Fatal(err)
	}
}
