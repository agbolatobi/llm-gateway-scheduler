package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
)

// preparedRequest owns one reservation and one immutable model/payload snapshot.
// One handler owns its response until finish. Do not use response.Body after finish.
type preparedRequest struct {
	model                           registry.Model
	payload                         routing.Request
	quotas                          *quota.Manager
	reservation                     *quota.Reservation
	amount                          int64
	mu                              sync.Mutex
	attempted, mayHaveRun, finished bool
	response                        *http.Response
	lease                           *capacity.Lease
	idle                            func(context.Context) (bool, error)
	facts                           responseFacts
	trace                           *requestTrace
}

func newPreparedRequest(model registry.Model, request ChatRequest, cap int, q *quota.Manager, reservation *quota.Reservation, amount int64) (*preparedRequest, error) {
	messages, err := json.Marshal(request.Messages)
	if err != nil {
		return nil, err
	}
	payload := routing.Request{Messages: messages, OutputTokens: cap, Stream: request.Stream}
	if request.Temperature != nil {
		temperature := *request.Temperature
		payload.Temperature = &temperature
	}
	if request.StreamOptions != nil {
		payload.IncludeUsage = request.StreamOptions.IncludeUsage
	}
	// Deep copy the registry entry too: reload/caller mutations cannot retarget it.
	snapshot := (registry.Config{Models: []registry.Model{model}}).Clone().Models[0]
	return &preparedRequest{model: snapshot, payload: payload, quotas: q, reservation: reservation, amount: amount, facts: responseFacts{includeUsage: payload.IncludeUsage}}, nil
}

func (p *preparedRequest) dispatch(ctx context.Context, client *routing.Client) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempted || p.finished {
		return nil, errors.New("prepared request cannot be dispatched twice or after cleanup")
	}
	p.attempted = true
	route, err := routing.Select(p.model)
	if err != nil {
		return nil, err
	}
	attempt, err := client.Send(ctx, route, p.payload)
	p.mayHaveRun, p.response = attempt.MayHaveRun, attempt.Response
	return p.response, err
}

// finish owns settlement and capacity cleanup once, including panic/cancellation.
// Context cancellation has already closed the outbound request before this runs.
func (p *preparedRequest) finish() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return nil
	}
	p.finished = true
	if p.response != nil {
		p.response.Body.Close()
	}
	actual := int64(0)
	source := "not_dispatched"
	if p.mayHaveRun {
		actual = p.amount
		source = "fallback"
		if p.facts.complete {
			if usage := p.facts.usage.validated(p.amount-int64(p.payload.OutputTokens), int64(p.payload.OutputTokens)); usage != nil {
				actual = int64(usage.TotalTokens)
				source = "backend"
				if p.trace != nil {
					p.trace.usage = usage
				}
			}
		}
	}
	err := p.quotas.SettleTokens(p.reservation, actual)
	state := "not_acquired"
	if p.lease != nil {
		state = "released"
		if !p.mayHaveRun {
			p.lease.Release()
		} else if !p.facts.observed {
			p.lease.Quarantine(false)
			state = "quarantined_unknown_dispatch"
		} else {
			// Independent bounded cleanup: the client's context is often cancelled.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			confirmed := false
			for ctx.Err() == nil {
				idle, probeErr := p.idle(ctx)
				if probeErr == nil && idle {
					confirmed = true
					break
				}
				timer := time.NewTimer(50 * time.Millisecond)
				select {
				case <-ctx.Done():
				case <-timer.C:
				}
				timer.Stop()
			}
			cancel()
			if confirmed {
				p.lease.Release()
			} else {
				p.lease.Quarantine(true)
				state = "quarantined_pending_idle"
			}
		}
	}
	if p.trace != nil {
		p.trace.reserved = p.amount
		p.trace.charged = actual
		p.trace.usageSource = source
		p.trace.capacityState = state
		if err != nil {
			p.trace.usageSource = "settlement_error"
		}
	}
	return err
}

func writeDispatchError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	} // The caller has disconnected.
	var failure *routing.Error
	if errors.As(err, &failure) {
		writeError(w, failure.Status, failure.Code, failure.Message, "")
		return
	}
	writeError(w, 502, "dispatch_failed", "The backend request could not be dispatched.", "")
}
