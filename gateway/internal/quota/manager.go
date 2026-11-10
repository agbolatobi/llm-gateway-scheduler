package quota

import (
	"fmt"
	"sync"
	"time"
)

type Decision struct {
	Allowed    bool
	Code       string
	RetryAfter time.Duration
}

type budget struct{ spent, reserved int64 }

// Manager is shared by all handlers in ONE gateway process. Restart resets usage.
type Manager struct {
	mu                         sync.Mutex
	now                        func() time.Time
	userLimit, globalLimit     Limits
	requestWindow, tokenWindow time.Duration
	requestStart, tokenStart   time.Time
	requests                   map[string]int64
	globalRequests             int64
	budgets                    map[string]*budget
	globalBudget               *budget
	modelLimits                map[string]Limits
	modelRequests              map[string]int64
	modelBudgets               map[string]*budget
}

func New(c Config, now func() time.Time) (*Manager, error) {
	global, err := c.EffectiveGlobal()
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	start := now()
	return &Manager{now: now, userLimit: c.User, globalLimit: global,
		requestWindow: c.requestWindow(), tokenWindow: c.tokenWindow(),
		requestStart: start, tokenStart: start, requests: make(map[string]int64),
		budgets: make(map[string]*budget), globalBudget: &budget{},
		modelLimits: modelLimits(c), modelRequests: make(map[string]int64), modelBudgets: make(map[string]*budget)}, nil
}

func modelLimits(c Config) map[string]Limits {
	limits := make(map[string]Limits, len(c.Models))
	for _, model := range c.Models {
		limits[model.ID] = model.Limits
	}
	return limits
}

// Reconfigure validates first, then updates limits without resetting any usage.
// Changing window lengths requires restart, rather than silently reinterpreting usage.
func (m *Manager) Reconfigure(c Config) error {
	global, err := c.EffectiveGlobal()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.requestWindow() != m.requestWindow || c.tokenWindow() != m.tokenWindow {
		return fmt.Errorf("changing quota window lengths requires a restart")
	}
	m.userLimit, m.globalLimit = c.User, global
	m.modelLimits = modelLimits(c) // Keep old counters: remove/re-add cannot reset usage.
	return nil
}

func (m *Manager) refresh(now time.Time) {
	if elapsed := now.Sub(m.requestStart); elapsed >= m.requestWindow {
		m.requestStart = m.requestStart.Add((elapsed / m.requestWindow) * m.requestWindow)
		m.requests = make(map[string]int64)
		m.globalRequests = 0
		m.modelRequests = make(map[string]int64)
	}
	if elapsed := now.Sub(m.tokenStart); elapsed >= m.tokenWindow {
		m.tokenStart = m.tokenStart.Add((elapsed / m.tokenWindow) * m.tokenWindow)
		// Outstanding reservations retain their original bucket pointers.
		// Late settlement must never credit/debit the new admission window.
		m.budgets = make(map[string]*budget)
		m.globalBudget = &budget{}
		m.modelBudgets = make(map[string]*budget)
	}
}

func available(b *budget, limit int64) int64 {
	if b == nil {
		return limit
	}
	return limit - b.spent - b.reserved
}

// CheckAndCount performs ALL checks and increments BOTH request counters under
// one lock. It neither tokenizes a prompt nor reserves tokens.
func (m *Manager) CheckAndCount(userID string) Decision {
	if userID == "" {
		return Decision{Code: "quota_unavailable"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.refresh(now)
	var denied Decision
	check := func(blocked bool, code string, reset time.Time) {
		if !blocked {
			return
		}
		if denied.Code == "" {
			denied.Code = code
		}
		denied.RetryAfter = max(denied.RetryAfter, reset.Sub(now))
	}
	requestReset, tokenReset := m.requestStart.Add(m.requestWindow), m.tokenStart.Add(m.tokenWindow)
	check(m.requests[userID] >= m.userLimit.Requests, "user_request_limit", requestReset)
	check(m.globalRequests >= m.globalLimit.Requests, "global_request_limit", requestReset)
	check(available(m.budgets[userID], m.userLimit.Tokens) <= 0, "user_token_limit", tokenReset)
	check(available(m.globalBudget, m.globalLimit.Tokens) <= 0, "global_token_limit", tokenReset)
	if denied.Code != "" {
		return denied
	}
	m.requests[userID]++
	m.globalRequests++
	return Decision{Allowed: true}
}
