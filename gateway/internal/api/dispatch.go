package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
)

// preparedRequest owns one reservation and one immutable model/payload snapshot.
// One handler owns its response until cleanup. Do not use response.Body after cleanup.
type preparedRequest struct {
	releaseRoute                    func()
	model                           registry.Model
	payload                         routing.Request
	quotas                          *quota.Manager
	reservation                     *quota.Reservation
	reservedTokens                  int64
	mu                              sync.Mutex
	attempted, mayHaveRun, finished bool
	response                        *http.Response
	lease                           *capacity.Lease
	idle                            func(context.Context) (bool, error)
	facts                           responseFacts
	trace                           *requestTrace
}

func newPreparedRequest(model registry.Model, request ChatRequest, outputTokenLimit int, quotaManager *quota.Manager, reservation *quota.Reservation, reservedTokens int64) (*preparedRequest, error) {
	messages, err := json.Marshal(request.Messages)
	if err != nil {
		return nil, err
	}
	payload := routing.Request{Messages: messages, OutputTokens: outputTokenLimit, Stream: request.Stream}
	if request.Temperature != nil {
		temperature := *request.Temperature
		payload.Temperature = &temperature
	}
	if request.StreamOptions != nil {
		payload.IncludeUsage = request.StreamOptions.IncludeUsage
	}
	// Deep copy the registry entry too: reload/caller mutations cannot retarget it.
	snapshot := (registry.Config{Models: []registry.Model{model}}).Clone().Models[0]
	return &preparedRequest{
		model:          snapshot,
		payload:        payload,
		quotas:         quotaManager,
		reservation:    reservation,
		reservedTokens: reservedTokens,
		facts:          responseFacts{includeUsage: payload.IncludeUsage},
	}, nil
}

func (prepared *preparedRequest) sendToBackend(ctx context.Context, client *routing.Client) (*http.Response, error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.attempted || prepared.finished {
		return nil, errors.New("prepared request cannot be dispatched twice or after cleanup")
	}
	prepared.attempted = true
	route, err := routing.Select(prepared.model)
	if err != nil {
		return nil, err
	}
	attempt, err := client.Send(ctx, route, prepared.payload)
	prepared.mayHaveRun, prepared.response = attempt.MayHaveRun, attempt.Response
	return prepared.response, err
}

func writeDispatchError(responseWriter http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	} // The caller has disconnected.
	var failure *routing.Error
	if errors.As(err, &failure) {
		writeError(responseWriter, failure.Status, failure.Code, failure.Message, "")
		return
	}
	writeError(responseWriter, 502, "dispatch_failed", "The backend request could not be dispatched.", "")
}
