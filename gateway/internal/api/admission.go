package api

import (
	"errors"
	"log"
	"net/http"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

// Admission validates the selected model and reserves its token budget before
// waiting for execution capacity. The registry lock pins one coherent snapshot.
func (handler *RegisteredHandler) handleAdmittedRequest(responseWriter http.ResponseWriter, httpRequest *http.Request, request ChatRequest) {
	prepared := handler.validateAndReserveRequest(responseWriter, httpRequest, request)
	if prepared == nil {
		return
	}
	defer func() {
		if err := prepared.settleUsageAndReleaseCapacity(); err != nil {
			log.Print("Request reservation cleanup failed")
		}
		if prepared.releaseRoute != nil {
			prepared.releaseRoute()
		}
		handler.scheduler.Wake()
		// Cleanup may outlast the last data-write deadline. Allow net/http to
		// send its terminating HTTP chunk after cleanup, including error streams.
		http.NewResponseController(responseWriter).SetWriteDeadline(time.Now().Add(handler.options.WriteTimeout))
	}()
	// Admission released the registry lock. Dispatch and response handling must
	// not hold it: a slow model cannot block configuration reload for the whole generation.
	if handler.afterAdmission != nil {
		handler.afterAdmission(responseWriter, httpRequest, prepared)
		return
	}
	// Validation-only test seam. Production always installs the proxy consumer.
	writeError(responseWriter, 501, "not_implemented", "Validation-only handler; token reservation released.", "")
}

func (handler *RegisteredHandler) validateAndReserveRequest(responseWriter http.ResponseWriter, httpRequest *http.Request, request ChatRequest) *preparedRequest {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	registeredModel, ok := handler.config.Lookup(request.Model)
	trace := requestTraceFromContext(httpRequest.Context())
	if trace != nil {
		trace.class = request.RequestClass
		if trace.class == "" {
			trace.class = "interactive"
		}
	}
	if !ok {
		writeError(responseWriter, 404, "model_not_found", "Model is not registered. Add it explicitly to the gateway configuration.", "model")
		return nil
	}
	principal, _ := PrincipalFromContext(httpRequest.Context())
	if trace != nil {
		trace.user = principal.UserID
		trace.model = registeredModel.ID
		trace.runtime = registeredModel.Backend.RuntimeID
	}
	outputTokenLimit := registeredModel.DefaultOutputTokens
	if request.MaxCompletionTokens != nil {
		outputTokenLimit = *request.MaxCompletionTokens
	} else if request.MaxTokens != nil {
		outputTokenLimit = *request.MaxTokens
	}
	if outputTokenLimit > registeredModel.MaxOutputTokens {
		writeError(responseWriter, 400, "output_limit_exceeded", "Requested output exceeds the model output cap.", "max_completion_tokens")
		return nil
	}
	inputTokens, err := handler.counter.CountInput(httpRequest.Context(), registeredModel, request.Messages)
	if err != nil {
		code := "model_unavailable"
		var detail *registry.RuntimeError
		if errors.As(err, &detail) {
			switch detail.Code {
			case "model_identity_mismatch", "model_context_mismatch", "model_metadata_invalid":
				code = detail.Code
			}
		}
		writeError(responseWriter, 503, code, "The configured model could not be verified or tokenized. Check its runtime status.", "model")
		return nil
	}
	if inputTokens < 1 || inputTokens > registeredModel.ContextLimit-outputTokenLimit {
		writeError(responseWriter, 400, "context_length_exceeded", "Chat-templated input plus output cap exceeds the model context.", "messages")
		return nil
	}
	if httpRequest.Context().Err() != nil {
		writeError(responseWriter, 504, "request_timeout", "The request exceeded its time limit.", "")
		return nil
	}
	reservation, quotaDecision := handler.quotas.ReserveForModel(principal.UserID, registeredModel.ID, int64(inputTokens+outputTokenLimit))
	if !writeQuotaDecision(responseWriter, quotaDecision) {
		return nil
	}
	prepared, err := newPreparedRequest(registeredModel, request, outputTokenLimit, handler.quotas, reservation, int64(inputTokens+outputTokenLimit))
	if err != nil {
		handler.quotas.SettleTokens(reservation, 0)
		writeError(responseWriter, 500, "dispatch_invalid", "Could not prepare the backend request.", "")
		return nil
	}
	prepared.trace = trace
	base, _ := registry.LocalURL(registeredModel.Backend.BaseURL)
	prepared.releaseRoute = handler.scheduler.Retain(base)
	return prepared
}
