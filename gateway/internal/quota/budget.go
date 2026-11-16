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
func (quotaManager *Manager) ReserveTokens(userID string, amount int64) (*Reservation, Decision) {
	return quotaManager.reserve(userID, "", amount)
}

// ReserveForModel charges a canonical model request and reserves all three
// token accounts in the same critical section. Aliases must be resolved first.
func (quotaManager *Manager) ReserveForModel(userID, modelID string, amount int64) (*Reservation, Decision) {
	if modelID == "" {
		return nil, Decision{Code: "model_not_found"}
	}
	return quotaManager.reserve(userID, modelID, amount)
}

func (quotaManager *Manager) reserve(userID, modelID string, amount int64) (*Reservation, Decision) {
	if userID == "" || amount <= 0 {
		return nil, Decision{Code: "invalid_reservation"}
	}
	quotaManager.mu.Lock()
	defer quotaManager.mu.Unlock()
	now := quotaManager.now()
	quotaManager.refresh(now)
	deny := func(code string) (*Reservation, Decision) {
		return nil, Decision{Code: code, RetryAfter: quotaManager.tokenStart.Add(quotaManager.tokenWindow).Sub(now)}
	}
	if modelID != "" {
		limits, ok := quotaManager.modelLimits[modelID]
		if !ok {
			return nil, Decision{Code: "model_not_found"}
		}
		if quotaManager.modelRequests[modelID] >= limits.Requests {
			return nil, Decision{Code: "model_request_limit", RetryAfter: quotaManager.requestStart.Add(quotaManager.requestWindow).Sub(now)}
		}
		if available(quotaManager.modelBudgets[modelID], limits.Tokens) < amount {
			return deny("model_token_limit")
		}
	}
	if available(quotaManager.budgets[userID], quotaManager.userLimit.Tokens) < amount {
		return deny("user_token_limit")
	}
	if available(quotaManager.globalBudget, quotaManager.globalLimit.Tokens) < amount {
		return deny("global_token_limit")
	}
	userBudget := quotaManager.budgets[userID]
	if userBudget == nil {
		userBudget = &budget{}
		quotaManager.budgets[userID] = userBudget
	}
	userBudget.reserved += amount
	quotaManager.globalBudget.reserved += amount
	reservation := &Reservation{owner: quotaManager, user: userBudget, global: quotaManager.globalBudget, amount: amount}
	if modelID != "" {
		modelBudget := quotaManager.modelBudgets[modelID]
		if modelBudget == nil {
			modelBudget = &budget{}
			quotaManager.modelBudgets[modelID] = modelBudget
		}
		modelBudget.reserved += amount
		quotaManager.modelRequests[modelID]++
		reservation.model = modelBudget
	}
	return reservation, Decision{Allowed: true}
}

// SettleTokens releases a hold and charges actual input+output usage once.
// actual=0 releases unused work; unknown usage can conservatively charge the hold.
func (quotaManager *Manager) SettleTokens(reservation *Reservation, actual int64) error {
	quotaManager.mu.Lock()
	defer quotaManager.mu.Unlock()
	if reservation == nil || reservation.owner != quotaManager || actual < 0 || actual > reservation.amount {
		return fmt.Errorf("invalid token settlement")
	}
	if reservation.settled {
		return fmt.Errorf("reservation already settled")
	}
	reservation.user.reserved -= reservation.amount
	reservation.global.reserved -= reservation.amount
	reservation.user.spent += actual
	reservation.global.spent += actual
	if reservation.model != nil {
		reservation.model.reserved -= reservation.amount
		reservation.model.spent += actual
	}
	reservation.settled = true
	return nil
}
