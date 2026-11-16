// Package scheduler bounds waiting work; capacity remains owned by GATE-07.
package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
)

var ErrFull = errors.New("queue_full")
var ErrTimeout = errors.New("queue_timeout")
var ErrClosed = errors.New("scheduler_closed")

type Result struct {
	SelectionSequence uint64
	Lease             *capacity.Lease
	Wait              time.Duration
	Policy, Reason    string
	Err               error
}
type job struct {
	ctx               context.Context
	address, class    string
	entered, deadline time.Time
	ready             chan struct{}
	done              bool
	result            Result
}
type Scheduler struct {
	mu         sync.Mutex
	config     Config
	capacity   *capacity.Manager
	jobs       []*job
	references map[string]int
	closed     bool
	selections uint64
}

func New(config Config, slots *capacity.Manager) *Scheduler {
	return &Scheduler{config: config, capacity: slots, references: make(map[string]int)}
}

// Retain pins the route between admission and final cleanup, including the small
// interval before Wait. Call while holding the registry's read lock.
func (scheduler *Scheduler) Retain(address string) func() {
	scheduler.mu.Lock()
	scheduler.references[address]++
	scheduler.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			scheduler.mu.Lock()
			scheduler.references[address]--
			if scheduler.references[address] == 0 {
				delete(scheduler.references, address)
			}
			scheduler.mu.Unlock()
		})
	}
}
func (scheduler *Scheduler) InUse(address string) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.references[address] != 0
}
func (scheduler *Scheduler) Waiting() int {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return len(scheduler.jobs)
}
func (scheduler *Scheduler) Reconfigure(config Config) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.config = config
	// Existing requests keep their original waiting deadline and enqueue time.
	scheduler.dispatchLocked(time.Now())
}
func (scheduler *Scheduler) Wake() {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.dispatchLocked(time.Now())
}
func (scheduler *Scheduler) Close() {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.closed = true
	for len(scheduler.jobs) > 0 {
		scheduler.completeLocked(scheduler.jobs[0], Result{Err: ErrClosed}, time.Now())
	}
}

func (scheduler *Scheduler) completeLocked(queuedRequest *job, result Result, now time.Time) {
	for queueIndex, candidate := range scheduler.jobs {
		if candidate == queuedRequest {
			scheduler.jobs = append(scheduler.jobs[:queueIndex], scheduler.jobs[queueIndex+1:]...)
			break
		}
	}
	result.Wait = now.Sub(queuedRequest.entered)
	result.Policy = scheduler.config.Policy
	queuedRequest.result = result
	queuedRequest.done = true
	close(queuedRequest.ready)
}

// No network calls under this mutex. Acquire atomically checks global/runtime
// permits; skipping a full runtime allows eligible work elsewhere to proceed.
func (scheduler *Scheduler) dispatchLocked(now time.Time) {
	for _, queuedRequest := range append([]*job(nil), scheduler.jobs...) {
		if err := queuedRequest.ctx.Err(); err != nil {
			scheduler.completeLocked(queuedRequest, Result{Err: err}, now)
		} else if !now.Before(queuedRequest.deadline) {
			scheduler.completeLocked(queuedRequest, Result{Err: ErrTimeout}, now)
		}
	}
	candidates := append([]*job(nil), scheduler.jobs...)
	sort.SliceStable(candidates, func(leftIndex, rightIndex int) bool {
		return schedulingPriority(candidates[leftIndex], scheduler.config, now) < schedulingPriority(candidates[rightIndex], scheduler.config, now)
	})
	for _, queuedRequest := range candidates {
		if err := queuedRequest.ctx.Err(); err != nil {
			scheduler.completeLocked(queuedRequest, Result{Err: err}, now)
			continue
		}
		lease, err := scheduler.capacity.Acquire(queuedRequest.address)
		if errors.Is(err, capacity.ErrFull) {
			continue
		}
		reason := "fifo"
		if scheduler.config.Policy == Priority {
			reason = "interactive"
			if queuedRequest.class == "background" {
				reason = "background"
			}
			if schedulingPriority(queuedRequest, scheduler.config, now) == 0 {
				reason = "aged"
			}
		}
		var sequence uint64
		if lease != nil {
			scheduler.selections++
			sequence = scheduler.selections
		} else {
			reason = ""
		}
		scheduler.completeLocked(queuedRequest, Result{Lease: lease, Reason: reason, SelectionSequence: sequence, Err: err}, now)
	}
}
