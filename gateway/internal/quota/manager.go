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

func New(config Config, now func() time.Time) (*Manager, error) {
	global, err := config.EffectiveGlobal()
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	start := now()
	return &Manager{now: now, userLimit: config.User, globalLimit: global,
		requestWindow: config.requestWindow(), tokenWindow: config.tokenWindow(),
		requestStart: start, tokenStart: start, requests: make(map[string]int64),
		budgets: make(map[string]*budget), globalBudget: &budget{},
		modelLimits: modelLimits(config), modelRequests: make(map[string]int64), modelBudgets: make(map[string]*budget)}, nil
}

func modelLimits(config Config) map[string]Limits {
	limits := make(map[string]Limits, len(config.Models))
	for _, model := range config.Models {
		limits[model.ID] = model.Limits
	}
	return limits
}

// Reconfigure validates first, then updates limits without resetting any usage.
// Changing window lengths requires restart, rather than silently reinterpreting usage.
func (quotaManager *Manager) Reconfigure(config Config) error {
	global, err := config.EffectiveGlobal()
	if err != nil {
		return err
	}
	quotaManager.mu.Lock()
	defer quotaManager.mu.Unlock()
	if config.requestWindow() != quotaManager.requestWindow || config.tokenWindow() != quotaManager.tokenWindow {
		return fmt.Errorf("changing quota window lengths requires a restart")
	}
	quotaManager.userLimit, quotaManager.globalLimit = config.User, global
	quotaManager.modelLimits = modelLimits(config) // Keep old counters: remove/re-add cannot reset usage.
	return nil
}

func (quotaManager *Manager) refresh(now time.Time) {
	if elapsed := now.Sub(quotaManager.requestStart); elapsed >= quotaManager.requestWindow {
		quotaManager.requestStart = quotaManager.requestStart.Add((elapsed / quotaManager.requestWindow) * quotaManager.requestWindow)
		quotaManager.requests = make(map[string]int64)
		quotaManager.globalRequests = 0
		quotaManager.modelRequests = make(map[string]int64)
	}
	if elapsed := now.Sub(quotaManager.tokenStart); elapsed >= quotaManager.tokenWindow {
		quotaManager.tokenStart = quotaManager.tokenStart.Add((elapsed / quotaManager.tokenWindow) * quotaManager.tokenWindow)
		// Outstanding reservations retain their original bucket pointers.
		// Late settlement must never credit/debit the new admission window.
		quotaManager.budgets = make(map[string]*budget)
		quotaManager.globalBudget = &budget{}
		quotaManager.modelBudgets = make(map[string]*budget)
	}
}

func available(tokenBudget *budget, limit int64) int64 {
	if tokenBudget == nil {
		return limit
	}
	return limit - tokenBudget.spent - tokenBudget.reserved
}

// CheckAndCount performs ALL checks and increments BOTH request counters under
// one lock. It neither tokenizes a prompt nor reserves tokens.
func (quotaManager *Manager) CheckAndCount(userID string) Decision {
	if userID == "" {
		return Decision{Code: "quota_unavailable"}
	}
	quotaManager.mu.Lock()
	defer quotaManager.mu.Unlock()
	now := quotaManager.now()
	quotaManager.refresh(now)
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
	requestReset, tokenReset := quotaManager.requestStart.Add(quotaManager.requestWindow), quotaManager.tokenStart.Add(quotaManager.tokenWindow)
	check(quotaManager.requests[userID] >= quotaManager.userLimit.Requests, "user_request_limit", requestReset)
	check(quotaManager.globalRequests >= quotaManager.globalLimit.Requests, "global_request_limit", requestReset)
	check(available(quotaManager.budgets[userID], quotaManager.userLimit.Tokens) <= 0, "user_token_limit", tokenReset)
	check(available(quotaManager.globalBudget, quotaManager.globalLimit.Tokens) <= 0, "global_token_limit", tokenReset)
	if denied.Code != "" {
		return denied
	}
	quotaManager.requests[userID]++
	quotaManager.globalRequests++
	return Decision{Allowed: true}
}
