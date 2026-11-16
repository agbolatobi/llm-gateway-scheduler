// Package routing selects and contacts one configured llama.cpp backend.
// It does not resolve user aliases, schedule work, or proxy responses.
package routing

import (
	"net/http"
	"strings"

	"llm-gateway-scheduler/gateway/internal/registry"
)

const ChatCompletionsPath = "/v1/chat/completions"

// Route is an immutable destination copied from the model validated at admission.
// Its fields are private so callers cannot replace it with a request-supplied URL.
type Route struct {
	modelID, runtimeID, baseURL, backendModel string
	maxOutputTokens                           int
}

// Select uses the sole configured backend, never another model or a fallback.
// The caller must first verify this snapshot's identity/readiness and tokenize
// against it. That check is a point-in-time observation, not a health guarantee.
func Select(model registry.Model) (Route, error) {
	backend := model.Backend
	base, err := registry.LocalURL(backend.BaseURL)
	if err != nil || model.ID == "" || backend.RuntimeID == "" || backend.Engine != "llama.cpp" ||
		strings.TrimSpace(backend.ExpectedModel) == "" || backend.ExpectedModel != strings.TrimSpace(backend.ExpectedModel) || model.MaxOutputTokens < 1 {
		return Route{}, failure(http.StatusServiceUnavailable, "route_unavailable", "No eligible configured backend is available.", nil)
	}
	return Route{modelID: model.ID, runtimeID: backend.RuntimeID, baseURL: base,
		backendModel: backend.ExpectedModel, maxOutputTokens: model.MaxOutputTokens}, nil
}

// Error exposes a safe API message while retaining the original cause for
// errors.Is / errors.As. Never echo the cause, which can contain private URLs.
type Error struct {
	Status        int
	Code, Message string
	cause         error
}

func (routingError *Error) Error() string { return routingError.Message }
func (routingError *Error) Unwrap() error { return routingError.cause }
func failure(status int, code, message string, cause error) error {
	return &Error{Status: status, Code: code, Message: message, cause: cause}
}
