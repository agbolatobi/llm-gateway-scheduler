package api

import (
	"context"
	"time"
)

// settleUsageAndReleaseCapacity runs once, including after panic or cancellation.
// The caller has already cancelled the outbound request. Keep the mutex held
// through settlement so another cleanup cannot refund or release the same work.
func (prepared *preparedRequest) settleUsageAndReleaseCapacity() error {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.finished {
		return nil
	}
	prepared.finished = true
	if prepared.response != nil {
		prepared.response.Body.Close()
	}

	chargedTokens, usageSource := prepared.calculateChargedUsage()
	settlementError := prepared.quotas.SettleTokens(prepared.reservation, chargedTokens)
	capacityState := prepared.releaseOrQuarantineCapacity()
	if prepared.trace != nil {
		prepared.trace.reserved = prepared.reservedTokens
		prepared.trace.charged = chargedTokens
		prepared.trace.usageSource = usageSource
		prepared.trace.capacityState = capacityState
		if settlementError != nil {
			prepared.trace.usageSource = "settlement_error"
		}
	}
	return settlementError
}

// A request that never reached the backend is refunded. Once it may have run,
// charge the reservation unless a complete response supplies trustworthy usage.
// Called only by cleanup while prepared.mu is held.
func (prepared *preparedRequest) calculateChargedUsage() (int64, string) {
	if !prepared.mayHaveRun {
		return 0, "not_dispatched"
	}
	if prepared.facts.complete {
		outputTokenLimit := int64(prepared.payload.OutputTokens)
		inputTokens := prepared.reservedTokens - outputTokenLimit
		if usage := prepared.facts.usage.validated(inputTokens, outputTokenLimit); usage != nil {
			if prepared.trace != nil {
				prepared.trace.usage = usage
			}
			return int64(usage.TotalTokens), "backend"
		}
	}
	return prepared.reservedTokens, "fallback"
}

// A disconnected client does not prove that the backend stopped generating.
// Quarantine uncertain work so the next request cannot reuse its execution slot.
// Called only by cleanup while prepared.mu is held.
func (prepared *preparedRequest) releaseOrQuarantineCapacity() string {
	if prepared.lease == nil {
		return "not_acquired"
	}
	if !prepared.mayHaveRun {
		prepared.lease.Release()
		return "released"
	}
	if !prepared.facts.observed {
		prepared.lease.Quarantine(false)
		return "quarantined_unknown_dispatch"
	}
	if prepared.waitForBackendIdle() {
		prepared.lease.Release()
		return "released"
	}
	prepared.lease.Quarantine(true)
	return "quarantined_pending_idle"
}

func (prepared *preparedRequest) waitForBackendIdle() bool {
	// Cleanup needs its own deadline: the client's context is often cancelled.
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCleanup()
	for cleanupContext.Err() == nil {
		backendIdle, probeError := prepared.idle(cleanupContext)
		if probeError == nil && backendIdle {
			return true
		}
		retryTimer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-cleanupContext.Done():
		case <-retryTimer.C:
		}
		retryTimer.Stop()
	}
	return false
}
