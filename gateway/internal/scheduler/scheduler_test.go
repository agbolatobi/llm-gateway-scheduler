package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
)

func waitCount(testContext *testing.T, scheduler *Scheduler, expectedWaitingCount int) {
	testContext.Helper()
	end := time.Now().Add(2 * time.Second)
	for scheduler.Waiting() != expectedWaitingCount && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if scheduler.Waiting() != expectedWaitingCount {
		testContext.Fatalf("waiting=%d want=%d", scheduler.Waiting(), expectedWaitingCount)
	}
}
func enqueue(testContext *testing.T, scheduler *Scheduler, ctx context.Context, address, class string, expectedWaitingCount int) <-chan Result {
	testContext.Helper()
	result := make(chan Result, 1)
	go func() { result <- scheduler.Wait(ctx, address, class) }()
	waitCount(testContext, scheduler, expectedWaitingCount)
	return result
}
func receive(testContext *testing.T, result <-chan Result) Result {
	testContext.Helper()
	select {
	case queueResult := <-result:
		return queueResult
	case <-time.After(2 * time.Second):
		testContext.Fatal("scheduler stalled")
		return Result{}
	}
}
func release(scheduler *Scheduler, result Result) { result.Lease.Release(); scheduler.Wake() }

func TestFIFOAndCapacityEligibility(testContext *testing.T) {
	slots := capacity.New(2)
	held, _ := slots.Acquire("a")
	scheduler := New(Default(), slots)
	defer scheduler.Close()
	first := enqueue(testContext, scheduler, context.Background(), "a", "background", 1)
	second := enqueue(testContext, scheduler, context.Background(), "a", "interactive", 2)
	independent := scheduler.Wait(context.Background(), "b", "interactive")
	if independent.Err != nil || independent.Lease == nil {
		testContext.Fatal(independent)
	}
	held.Release()
	scheduler.Wake()
	queueResult := receive(testContext, first)
	if queueResult.Reason != "fifo" {
		testContext.Fatal(queueResult)
	}
	select {
	case <-second:
		testContext.Fatal("same-runtime request ran concurrently")
	default:
	}
	release(scheduler, queueResult)
	release(scheduler, receive(testContext, second))
	release(scheduler, independent)
}

func TestPriorityAgingAndReload(testContext *testing.T) {
	for _, aged := range []bool{false, true} {
		slots := capacity.New(1)
		held, _ := slots.Acquire("a")
		scheduler := New(Default(), slots)
		background := enqueue(testContext, scheduler, context.Background(), "a", "background", 1)
		interactive := enqueue(testContext, scheduler, context.Background(), "a", "interactive", 2)
		scheduler.mu.Lock()
		if aged {
			scheduler.jobs[0].entered = time.Now().Add(-6 * time.Second)
		}
		original := scheduler.jobs[0].entered
		scheduler.mu.Unlock()
		config := Default()
		config.Policy = Priority
		scheduler.Reconfigure(config)
		scheduler.mu.Lock()
		if !scheduler.jobs[0].entered.Equal(original) {
			testContext.Error("reload reset arrival time")
		}
		scheduler.mu.Unlock()
		held.Release()
		scheduler.Wake()
		first, second, reason := interactive, background, "interactive"
		if aged {
			first, second, reason = background, interactive, "aged"
		}
		queueResult := receive(testContext, first)
		if queueResult.Reason != reason || queueResult.Policy != Priority {
			testContext.Fatal(queueResult)
		}
		release(scheduler, queueResult)
		release(scheduler, receive(testContext, second))
		scheduler.Close()
	}
}

func TestBoundsCancellationExpiryAndClose(testContext *testing.T) {
	slots := capacity.New(1)
	held, _ := slots.Acquire("a")
	defer held.Release()
	config := Default()
	config.MaxWaiting = 2
	config.MaxPerRuntime = 1
	scheduler := New(config, slots)
	ctx, cancel := context.WithCancel(context.Background())
	first := enqueue(testContext, scheduler, ctx, "a", "interactive", 1)
	if queueResult := scheduler.Wait(context.Background(), "a", "interactive"); !errors.Is(queueResult.Err, ErrFull) {
		testContext.Fatal(queueResult)
	}
	other := enqueue(testContext, scheduler, context.Background(), "b", "background", 2)
	if queueResult := scheduler.Wait(context.Background(), "c", "interactive"); !errors.Is(queueResult.Err, ErrFull) {
		testContext.Fatal(queueResult)
	}
	config.MaxWaiting = 1
	scheduler.Reconfigure(config)
	if scheduler.Waiting() != 2 {
		testContext.Fatal("lower limit evicted existing work")
	}
	cancel()
	if queueResult := receive(testContext, first); !errors.Is(queueResult.Err, context.Canceled) {
		testContext.Fatal(queueResult)
	}
	// Advance this queued deadline deterministically instead of sleeping.
	scheduler.mu.Lock()
	scheduler.jobs[0].deadline = time.Now().Add(-time.Second)
	scheduler.mu.Unlock()
	scheduler.Wake()
	if queueResult := receive(testContext, other); !errors.Is(queueResult.Err, ErrTimeout) {
		testContext.Fatal(queueResult)
	}
	last := enqueue(testContext, scheduler, context.Background(), "a", "interactive", 1)
	scheduler.Close()
	if queueResult := receive(testContext, last); !errors.Is(queueResult.Err, ErrClosed) {
		testContext.Fatal(queueResult)
	}
	if queueResult := scheduler.Wait(context.Background(), "b", "interactive"); !errors.Is(queueResult.Err, ErrClosed) {
		testContext.Fatal(queueResult)
	}
}

func TestCancellationSelectionRaceDoesNotLeak(testContext *testing.T) {
	for expectedWaitingCount := 0; expectedWaitingCount < 100; expectedWaitingCount++ {
		slots := capacity.New(1)
		held, _ := slots.Acquire("a")
		scheduler := New(Default(), slots)
		ctx, cancel := context.WithCancel(context.Background())
		pending := enqueue(testContext, scheduler, ctx, "a", "interactive", 1)
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); cancel() }()
		go func() { defer group.Done(); held.Release(); scheduler.Wake() }()
		group.Wait()
		queueResult := receive(testContext, pending)
		if queueResult.Lease != nil {
			queueResult.Lease.Release()
		} // Ownership may already have transferred.
		if slots.Occupied("a") || scheduler.Waiting() != 0 {
			testContext.Fatal("permit or waiting entry leaked")
		}
		scheduler.Close()
	}
}

func TestQuarantineDoesNotReleaseSlot(testContext *testing.T) {
	slots := capacity.New(1)
	held, _ := slots.Acquire("a")
	scheduler := New(Default(), slots)
	pending := enqueue(testContext, scheduler, context.Background(), "a", "interactive", 1)
	held.Quarantine(true)
	scheduler.Wake()
	queueResult := receive(testContext, pending)
	if !errors.Is(queueResult.Err, capacity.ErrUnconfirmed) || !slots.Occupied("a") {
		testContext.Fatal(queueResult)
	}
	held.Release()
	scheduler.Close()
}
