package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
	"llm-gateway-scheduler/gateway/internal/scheduler"
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
	scheduler *scheduler.Scheduler
	execution *registry.ExecutionProbe
	report    func(TimingRecord)
	// Tests can replace the response consumer before serving requests.
	afterAdmission func(http.ResponseWriter, *http.Request, *preparedRequest)
}

func NewRegisteredHandler(authenticator Authenticator, config registry.Config, counter InputCounter) (*RegisteredHandler, error) {
	return NewRegisteredHandlerWithOptions(authenticator, config, counter, DefaultProxyOptions())
}

func NewRegisteredHandlerWithOptions(authenticator Authenticator, gatewayConfig registry.Config, counter InputCounter, options ProxyOptions) (*RegisteredHandler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := gatewayConfig.Validate(); err != nil {
		return nil, err
	}
	quotaManager, err := quota.New(gatewayConfig.QuotaConfig(), nil)
	if err != nil {
		return nil, err
	}
	if counter == nil {
		counter = registry.NewInspector()
	}
	handler := &RegisteredHandler{
		config:    gatewayConfig.Clone(),
		quotas:    quotaManager,
		verifier:  authenticator,
		counter:   counter,
		client:    routing.NewClientWithHeaderTimeout(options.RequestTimeout),
		options:   options,
		report:    logTiming,
		capacity:  capacity.New(gatewayConfig.GlobalActiveLimit()),
		execution: registry.NewExecutionProbe(),
	}
	handler.scheduler = scheduler.New(gatewayConfig.SchedulerConfig(), handler.capacity)
	handler.afterAdmission = handler.forwardBackendResponse
	return handler, nil
}

func (handler *RegisteredHandler) StopWaiting() { handler.scheduler.Close() }
func (handler *RegisteredHandler) Close() {
	handler.StopWaiting()
	handler.client.CloseIdleConnections()
	handler.execution.Close()
}

func (handler *RegisteredHandler) Reload(gatewayConfig registry.Config) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if err := handler.config.CompatibleReload(gatewayConfig); err != nil {
		return err
	}
	for _, old := range handler.config.Models {
		base, _ := registry.LocalURL(old.Backend.BaseURL)
		if handler.capacity.Occupied(base) || handler.scheduler.InUse(base) {
			found := false
			for _, next := range gatewayConfig.Models {
				if next.ID == old.ID && next.Backend == old.Backend {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("cannot remove or retarget a runtime with admitted, queued, or active work")
			}
		}
	}
	if err := handler.quotas.Reconfigure(gatewayConfig.QuotaConfig()); err != nil {
		return err
	}
	handler.config = gatewayConfig.Clone()
	handler.capacity.SetLimit(gatewayConfig.GlobalActiveLimit())
	handler.scheduler.Reconfigure(gatewayConfig.SchedulerConfig())
	return nil
}

func (handler *RegisteredHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.URL.Path == ChatCompletionsPath {
		trace, err := newRequestTrace(request.Header.Get("X-Request-ID"))
		if err != nil {
			writeError(responseWriter, 503, "request_id_unavailable", "Could not create a request identifier.", "")
			return
		}
		responseWriter.Header().Set("X-Request-ID", trace.id)
		ctx, cancel := context.WithTimeout(context.WithValue(request.Context(), traceKey{}, trace), handler.options.RequestTimeout)
		defer cancel()
		defer func() { handler.report(trace.buildTimingRecord()) }()
		request = request.WithContext(ctx)
		http.NewResponseController(responseWriter).SetWriteDeadline(time.Now().Add(handler.options.WriteTimeout))
	}
	if request.URL.Path != ModelsPath {
		newHandler(handler.verifier, handler.quotas, handler.handleAdmittedRequest).ServeHTTP(responseWriter, request)
		return
	}
	responseWriter.Header().Set("Cache-Control", "no-store")
	request, ok := authenticate(responseWriter, request, handler.verifier)
	if !ok {
		return
	}
	if !requireMethod(responseWriter, request, http.MethodGet) {
		return
	}
	handler.mu.RLock()
	data := make([]map[string]any, 0)
	for _, model := range handler.config.Models {
		data = append(data, map[string]any{"id": model.ID, "object": "model", "created": 0, "owned_by": "local"})
	}
	handler.mu.RUnlock()
	writeJSON(responseWriter, 200, map[string]any{"object": "list", "data": data})
}
