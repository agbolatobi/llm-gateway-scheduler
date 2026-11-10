package quota

import "fmt"

// Reservation is an opaque, in-memory hold against its original token window.
// Admission reserves user/global/model budgets; request cleanup settles usage.
type Reservation struct {
	owner        *Manager
	user, global *budget
	model        *budget
	amount       int64
	settled      bool
}

// ReserveTokens atomically reserves user/global tokens, with no request charge.
// Kept for ledger tests and callers without a model. Gateway admission must use
// ReserveForModel so all three budgets participate in the same transaction.
func (m *Manager) ReserveTokens(userID string, amount int64) (*Reservation, Decision) {
	return m.reserve(userID, "", amount)
}

// ReserveForModel charges a canonical model request and reserves all three
// token accounts in the same critical section. Aliases must be resolved first.
func (m *Manager) ReserveForModel(userID, modelID string, amount int64) (*Reservation, Decision) {
	if modelID == "" {
		return nil, Decision{Code: "model_not_found"}
	}
	return m.reserve(userID, modelID, amount)
}

func (m *Manager) reserve(userID, modelID string, amount int64) (*Reservation, Decision) {
	if userID == "" || amount <= 0 {
		return nil, Decision{Code: "invalid_reservation"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.refresh(now)
	deny := func(code string) (*Reservation, Decision) {
		return nil, Decision{Code: code, RetryAfter: m.tokenStart.Add(m.tokenWindow).Sub(now)}
	}
	if modelID != "" {
		limits, ok := m.modelLimits[modelID]
		if !ok {
			return nil, Decision{Code: "model_not_found"}
		}
		if m.modelRequests[modelID] >= limits.Requests {
			return nil, Decision{Code: "model_request_limit", RetryAfter: m.requestStart.Add(m.requestWindow).Sub(now)}
		}
		if available(m.modelBudgets[modelID], limits.Tokens) < amount {
			return deny("model_token_limit")
		}
	}
	if available(m.budgets[userID], m.userLimit.Tokens) < amount {
		return deny("user_token_limit")
	}
	if available(m.globalBudget, m.globalLimit.Tokens) < amount {
		return deny("global_token_limit")
	}
	u := m.budgets[userID]
	if u == nil {
		u = &budget{}
		m.budgets[userID] = u
	}
	u.reserved += amount
	m.globalBudget.reserved += amount
	r := &Reservation{owner: m, user: u, global: m.globalBudget, amount: amount}
	if modelID != "" {
		b := m.modelBudgets[modelID]
		if b == nil {
			b = &budget{}
			m.modelBudgets[modelID] = b
		}
		b.reserved += amount
		m.modelRequests[modelID]++
		r.model = b
	}
	return r, Decision{Allowed: true}
}

// SettleTokens releases a hold and charges actual input+output usage once.
// actual=0 releases unused work; unknown usage can conservatively charge the hold.
func (m *Manager) SettleTokens(r *Reservation, actual int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r == nil || r.owner != m || actual < 0 || actual > r.amount {
		return fmt.Errorf("invalid token settlement")
	}
	if r.settled {
		return fmt.Errorf("reservation already settled")
	}
	r.user.reserved -= r.amount
	r.global.reserved -= r.amount
	r.user.spent += actual
	r.global.spent += actual
	if r.model != nil {
		r.model.reserved -= r.amount
		r.model.spent += actual
	}
	r.settled = true
	return nil
}
