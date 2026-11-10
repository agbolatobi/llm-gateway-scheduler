package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
)

type InputCounter interface {
	CountInput(context.Context, registry.Model, any) (int, error)
}

// RegisteredHandler keeps registry and quota reloads coherent. Read locks cover
// validation (bounded by the runtime inspector), not any future generation stream.
type RegisteredHandler struct {
	mu        sync.RWMutex
	config    registry.Config
	quotas    *quota.Manager
	verifier  Authenticator
	counter   InputCounter
	client    *routing.Client
	options   ProxyOptions
	capacity  *capacity.Manager
	execution *registry.ExecutionProbe
	report    func(TimingRecord)
	// Tests can replace the response consumer before serving requests.
	afterAdmission func(http.ResponseWriter, *http.Request, *preparedRequest)
}

func NewRegisteredHandler(v Authenticator, c registry.Config, counter InputCounter) (*RegisteredHandler, error) {
	return NewRegisteredHandlerWithOptions(v, c, counter, DefaultProxyOptions())
}

func NewRegisteredHandlerWithOptions(v Authenticator, c registry.Config, counter InputCounter, options ProxyOptions) (*RegisteredHandler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	q, err := quota.New(c.QuotaConfig(), nil)
	if err != nil {
		return nil, err
	}
	if counter == nil {
		counter = registry.NewInspector()
	}
	h := &RegisteredHandler{config: c.Clone(), quotas: q, verifier: v, counter: counter, client: routing.NewClientWithHeaderTimeout(options.RequestTimeout), options: options, report: logTiming, capacity: capacity.New(c.GlobalActiveLimit()), execution: registry.NewExecutionProbe()}
	h.afterAdmission = h.proxy
	return h, nil
}

func (h *RegisteredHandler) Close() { h.client.CloseIdleConnections(); h.execution.Close() }

func (h *RegisteredHandler) Reload(c registry.Config) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.config.CompatibleReload(c); err != nil {
		return err
	}
	for _, old := range h.config.Models {
		base, _ := registry.LocalURL(old.Backend.BaseURL)
		if h.capacity.Occupied(base) {
			found := false
			for _, next := range c.Models {
				if next.ID == old.ID && next.Backend == old.Backend {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("cannot remove or retarget an occupied runtime")
			}
		}
	}
	if err := h.quotas.Reconfigure(c.QuotaConfig()); err != nil {
		return err
	}
	h.config = c.Clone()
	h.capacity.SetLimit(c.GlobalActiveLimit())
	return nil
}

func (h *RegisteredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == ChatCompletionsPath {
		trace, err := newTrace(r.Header.Get("X-Request-ID"))
		if err != nil {
			writeError(w, 503, "request_id_unavailable", "Could not create a request identifier.", "")
			return
		}
		w.Header().Set("X-Request-ID", trace.id)
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), traceKey{}, trace), h.options.RequestTimeout)
		defer cancel()
		defer func() { h.report(trace.record()) }()
		r = r.WithContext(ctx)
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.options.WriteTimeout))
	}
	if r.URL.Path != ModelsPath {
		newHandler(h.verifier, h.quotas, h.prepare).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r, ok := authenticate(w, r, h.verifier)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.mu.RLock()
	data := make([]map[string]any, 0)
	for _, m := range h.config.Models {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": "local"})
	}
	h.mu.RUnlock()
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (h *RegisteredHandler) prepare(w http.ResponseWriter, r *http.Request, request ChatRequest) {
	prepared := h.admit(w, r, request)
	if prepared == nil {
		return
	}
	defer func() {
		if err := prepared.finish(); err != nil {
			log.Print("Request reservation cleanup failed")
		}
		// Cleanup may outlast the last data-write deadline. Allow net/http to
		// send its terminating HTTP chunk after cleanup, including error streams.
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.options.WriteTimeout))
	}()
	// admit released the registry lock. Dispatch/response handling must not hold
	// it: a slow model cannot block configuration reload for the whole generation.
	if h.afterAdmission != nil {
		h.afterAdmission(w, r, prepared)
		return
	}
	// Validation-only test seam. Production always installs the proxy consumer.
	writeError(w, 501, "not_implemented", "Validation-only handler; token reservation released.", "")
}

func (h *RegisteredHandler) admit(w http.ResponseWriter, r *http.Request, request ChatRequest) *preparedRequest {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m, ok := h.config.Lookup(request.Model)
	trace := traceFrom(r.Context())
	if trace != nil {
		trace.class = request.RequestClass
		if trace.class == "" {
			trace.class = "interactive"
		}
	}
	if !ok {
		writeError(w, 404, "model_not_found", "Model is not registered. Add it explicitly to the gateway configuration.", "model")
		return nil
	}
	p, _ := PrincipalFromContext(r.Context())
	if trace != nil {
		trace.user = p.UserID
		trace.model = m.ID
		trace.runtime = m.Backend.RuntimeID
	}
	cap := m.DefaultOutputTokens
	if request.MaxCompletionTokens != nil {
		cap = *request.MaxCompletionTokens
	} else if request.MaxTokens != nil {
		cap = *request.MaxTokens
	}
	if cap > m.MaxOutputTokens {
		writeError(w, 400, "output_limit_exceeded", "Requested output exceeds the model output cap.", "max_completion_tokens")
		return nil
	}
	input, err := h.counter.CountInput(r.Context(), m, request.Messages)
	if err != nil {
		code := "model_unavailable"
		var detail *registry.RuntimeError
		if errors.As(err, &detail) {
			switch detail.Code {
			case "model_identity_mismatch", "model_context_mismatch", "model_metadata_invalid":
				code = detail.Code
			}
		}
		writeError(w, 503, code, "The configured model could not be verified or tokenized. Check its runtime status.", "model")
		return nil
	}
	if input < 1 || input > m.ContextLimit-cap {
		writeError(w, 400, "context_length_exceeded", "Chat-templated input plus output cap exceeds the model context.", "messages")
		return nil
	}
	if r.Context().Err() != nil {
		writeError(w, 504, "request_timeout", "The request exceeded its time limit.", "")
		return nil
	}
	reservation, d := h.quotas.ReserveForModel(p.UserID, m.ID, int64(input+cap))
	if !writeQuotaDecision(w, d) {
		return nil
	}
	prepared, err := newPreparedRequest(m, request, cap, h.quotas, reservation, int64(input+cap))
	if err != nil {
		h.quotas.SettleTokens(reservation, 0)
		writeError(w, 500, "dispatch_invalid", "Could not prepare the backend request.", "")
		return nil
	}
	prepared.trace = trace
	return prepared
}
