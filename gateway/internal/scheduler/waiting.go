package scheduler

import (
	"context"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
)

// Wait either transfers a capacity lease to the caller or returns an error.
// A queued request owns no execution slot. Cancellation and selection can race,
// so the scheduler returns an unused lease before reporting cancellation.
func (scheduler *Scheduler) Wait(ctx context.Context, address, class string) Result {
	scheduler.mu.Lock()
	now := time.Now()
	if scheduler.closed {
		scheduler.mu.Unlock()
		return Result{Err: ErrClosed}
	}
	scheduler.dispatchLocked(now) // Existing eligible jobs always get first consideration.
	queuedRequest := &job{
		ctx:      ctx,
		address:  address,
		class:    class,
		entered:  now,
		deadline: now.Add(time.Duration(scheduler.config.MaxWaitSeconds) * time.Second),
		ready:    make(chan struct{}),
	}
	scheduler.jobs = append(scheduler.jobs, queuedRequest)
	scheduler.dispatchLocked(now) // A request dispatched immediately doesn't consume waiting space.
	scheduler.enforceWaitingLimitsLocked(queuedRequest, now)
	deadline := queuedRequest.deadline
	scheduler.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-queuedRequest.ready:
	case <-ctx.Done():
	case <-timer.C:
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if !queuedRequest.done {
		err := ctx.Err()
		if err == nil {
			err = ErrTimeout
		}
		scheduler.completeLocked(queuedRequest, Result{Err: err}, time.Now())
	}
	result := queuedRequest.result
	// Cancellation racing selection returns an unused permit here. Once this
	// function returns a lease, the preparedRequest owns its cleanup instead.
	if err := ctx.Err(); err != nil {
		if result.Lease != nil {
			result.Lease.Release()
			result.Lease = nil
		}
		result.Err = err
	}
	scheduler.dispatchLocked(time.Now())
	return result
}

// Called only with mu held; immediate dispatch consumes no waiting space.
func (scheduler *Scheduler) enforceWaitingLimitsLocked(queuedRequest *job, now time.Time) {
	if !queuedRequest.done {
		runtimeWaitingCount := 0
		for _, pending := range scheduler.jobs {
			if pending.address == queuedRequest.address {
				runtimeWaitingCount++
			}
		}
		if len(scheduler.jobs) > scheduler.config.MaxWaiting || runtimeWaitingCount > scheduler.config.MaxPerRuntime {
			err := ErrFull
			if scheduler.config.MaxWaiting == 0 {
				err = capacity.ErrFull
			}
			scheduler.completeLocked(queuedRequest, Result{Err: err}, now)
		}
	}
}
