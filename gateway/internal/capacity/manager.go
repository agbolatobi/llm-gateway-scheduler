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

func New(global int) *Manager          { return &Manager{limit: global, active: make(map[string]*Lease)} }
func (m *Manager) SetLimit(global int) { m.mu.Lock(); defer m.mu.Unlock(); m.limit = global }
func (m *Manager) Occupied(address string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[address] != nil
}
func (m *Manager) Acquire(address string) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.active[address]; current != nil {
		if current.quarantined {
			return nil, ErrUnconfirmed
		}
		return nil, ErrFull
	}
	if len(m.active) >= m.limit {
		return nil, ErrFull
	}
	l := &Lease{owner: m, address: address}
	m.active[address] = l
	return l, nil
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	m := l.owner
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if m.active[l.address] == l {
		delete(m.active, l.address)
	}
}
func (l *Lease) Quarantine(recoverable bool) {
	if l == nil {
		return
	}
	m := l.owner
	m.mu.Lock()
	defer m.mu.Unlock()
	if !l.released {
		l.quarantined = true
		l.recoverable = recoverable
	}
}

// Recover performs at most one probe on the next request for this runtime.
// An ambiguous dispatch with no observed backend result cannot be cleared by an
// idle snapshot: its generation request might still be arriving at the backend.
func (m *Manager) Recover(ctx context.Context, address string, idle func(context.Context) (bool, error)) bool {
	m.mu.Lock()
	l := m.active[address]
	if l == nil || !l.quarantined || !l.recoverable || l.checking {
		m.mu.Unlock()
		return false
	}
	l.checking = true
	m.mu.Unlock()
	ok, err := idle(ctx)
	m.mu.Lock()
	l.checking = false
	if err == nil && ok && !l.released && m.active[address] == l {
		l.released = true
		delete(m.active, address)
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()
	return false
}
