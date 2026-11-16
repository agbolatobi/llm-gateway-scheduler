// Package capacity owns execution permits independently of token quotas.
package capacity

import (
	"context"
	"errors"
	"sync"
)

var ErrFull = errors.New("capacity_exceeded")
var ErrUnconfirmed = errors.New("capacity_unconfirmed")

// One permit per backend address. Aliases or renamed runtime IDs cannot create
// additional physical capacity. Global and runtime acquisition is atomic.
type Manager struct {
	mu     sync.Mutex
	limit  int
	active map[string]*Lease
}
type Lease struct {
	owner                                        *Manager
	address                                      string
	quarantined, recoverable, checking, released bool
}

func New(global int) *Manager { return &Manager{limit: global, active: make(map[string]*Lease)} }
func (capacityManager *Manager) SetLimit(global int) {
	capacityManager.mu.Lock()
	defer capacityManager.mu.Unlock()
	capacityManager.limit = global
}
func (capacityManager *Manager) Occupied(address string) bool {
	capacityManager.mu.Lock()
	defer capacityManager.mu.Unlock()
	return capacityManager.active[address] != nil
}
func (capacityManager *Manager) Acquire(address string) (*Lease, error) {
	capacityManager.mu.Lock()
	defer capacityManager.mu.Unlock()
	if current := capacityManager.active[address]; current != nil {
		if current.quarantined {
			return nil, ErrUnconfirmed
		}
		return nil, ErrFull
	}
	if len(capacityManager.active) >= capacityManager.limit {
		return nil, ErrFull
	}
	lease := &Lease{owner: capacityManager, address: address}
	capacityManager.active[address] = lease
	return lease, nil
}
func (lease *Lease) Release() {
	if lease == nil {
		return
	}
	capacityManager := lease.owner
	capacityManager.mu.Lock()
	defer capacityManager.mu.Unlock()
	if lease.released {
		return
	}
	lease.released = true
	if capacityManager.active[lease.address] == lease {
		delete(capacityManager.active, lease.address)
	}
}
func (lease *Lease) Quarantine(recoverable bool) {
	if lease == nil {
		return
	}
	capacityManager := lease.owner
	capacityManager.mu.Lock()
	defer capacityManager.mu.Unlock()
	if !lease.released {
		lease.quarantined = true
		lease.recoverable = recoverable
	}
}

// Recover performs at most one probe on the next request for this runtime.
// An ambiguous dispatch with no observed backend result cannot be cleared by an
// idle snapshot: its generation request might still be arriving at the backend.
func (capacityManager *Manager) Recover(ctx context.Context, address string, idle func(context.Context) (bool, error)) bool {
	capacityManager.mu.Lock()
	lease := capacityManager.active[address]
	if lease == nil || !lease.quarantined || !lease.recoverable || lease.checking {
		capacityManager.mu.Unlock()
		return false
	}
	lease.checking = true
	capacityManager.mu.Unlock()
	ok, err := idle(ctx)
	capacityManager.mu.Lock()
	lease.checking = false
	if err == nil && ok && !lease.released && capacityManager.active[address] == lease {
		lease.released = true
		delete(capacityManager.active, address)
		capacityManager.mu.Unlock()
		return true
	}
	capacityManager.mu.Unlock()
	return false
}
