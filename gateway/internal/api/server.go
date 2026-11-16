package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/chatui"
)

const MaxBodyBytes = 1 << 20 // 1 MiB, including trailing input.

type Authenticator interface {
	Verify(string) (auth.Principal, error)
}

type principalContextKey struct{}

// PrincipalFromContext makes the verified user available to later quota checks.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal, ok
}

// requiresAuthentication lists the routes that need an API key.
// Add another endpoint constant to the case below to protect a new route.
func requiresAuthentication(path string) bool {
	switch path {
	case ChatCompletionsPath, ModelsPath:
		return true
	default:
		return false
	}
}

// NewHandler authenticates protected routes before inspecting the request body.
// Authorized inference requests still return 501 until inference is implemented.
func NewHandler(verifier Authenticator, quotas QuotaChecker) http.Handler {
	return newHandler(verifier, quotas, inferenceNotImplemented)
}

// afterQuota is the boundary for future model lookup/admission/routing.
type chatHandler func(http.ResponseWriter, *http.Request, ChatRequest)

func newHandler(verifier Authenticator, quotas QuotaChecker, afterQuota chatHandler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if chatui.Serve(responseWriter, request) {
			return
		}
		responseWriter.Header().Set("Cache-Control", "no-store")
		if requiresAuthentication(request.URL.Path) {
			var ok bool
			request, ok = authenticate(responseWriter, request, verifier)
			if !ok {
				return
			}
		}
		switch request.URL.Path {
		case HealthPath:
			if !requireMethod(responseWriter, request, http.MethodGet) {
				return
			}
			writeJSON(responseWriter, http.StatusOK, map[string]string{"status": "ok"})
		case ChatCompletionsPath:
			if !requireMethod(responseWriter, request, http.MethodPost) {
				return
			}
			chat(responseWriter, request, quotas, afterQuota)
		case ModelsPath:
			if !requireMethod(responseWriter, request, http.MethodGet) {
				return
			}
			writeError(responseWriter, http.StatusNotImplemented, "not_implemented", "The model registry is not implemented.", "")
		default:
			writeError(responseWriter, http.StatusNotFound, "not_found", "Endpoint not found.", "")
		}
	})
}

func authenticate(responseWriter http.ResponseWriter, request *http.Request, verifier Authenticator) (*http.Request, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		unauthorized(responseWriter)
		return request, false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		unauthorized(responseWriter)
		return request, false
	}
	if verifier == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "authentication_unavailable", "API-key verification is temporarily unavailable.", "")
		return request, false
	}
	principal, err := verifier.Verify(parts[1])
	if errors.Is(err, auth.ErrInvalidKey) {
		unauthorized(responseWriter)
		return request, false
	}
	if err != nil || principal.UserID == "" || principal.KeyID == "" {
		writeError(responseWriter, http.StatusServiceUnavailable, "authentication_unavailable", "API-key verification is temporarily unavailable.", "")
		return request, false
	}
	if trace := requestTraceFromContext(request.Context()); trace != nil {
		trace.user = principal.UserID
	}
	return request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)), true
}

func unauthorized(responseWriter http.ResponseWriter) {
	responseWriter.Header().Set("WWW-Authenticate", `Bearer realm="llm-gateway"`)
	writeJSON(responseWriter, http.StatusUnauthorized, ErrorResponse{Error: APIError{
		Message: "A valid API key is required. Use Authorization: Bearer <api_key>.",
		Type:    "authentication_error", Code: "invalid_api_key",
	}})
}

func requireMethod(responseWriter http.ResponseWriter, request *http.Request, method string) bool {
	if request.Method == method {
		return true
	}
	responseWriter.Header().Set("Allow", method)
	writeError(responseWriter, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+method+" for this endpoint.", "")
	return false
}

func chat(responseWriter http.ResponseWriter, httpRequest *http.Request, quotas QuotaChecker, afterQuota chatHandler) {
	mediaType, _, err := mime.ParseMediaType(httpRequest.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || httpRequest.Header.Get("Content-Encoding") != "" {
		writeError(responseWriter, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send an uncompressed application/json body.", "")
		return
	}
	httpRequest.Body = http.MaxBytesReader(responseWriter, httpRequest.Body, MaxBodyBytes)
	defer httpRequest.Body.Close()
	decoder := json.NewDecoder(httpRequest.Body)
	decoder.DisallowUnknownFields()
	var request *ChatRequest
	if err := decoder.Decode(&request); err != nil {
		decodeError(responseWriter, err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		decodeError(responseWriter, err)
		return
	}
	if request == nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request", "The body must be a JSON object.", "")
		return
	}
	if param, message := validateChatRequest(*request); message != "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request", message, param)
		return
	}
	if !checkQuota(responseWriter, httpRequest, quotas) {
		return
	}
	afterQuota(responseWriter, httpRequest, *request)
}

func inferenceNotImplemented(responseWriter http.ResponseWriter, request *http.Request, chatRequest ChatRequest) {
	writeError(responseWriter, http.StatusNotImplemented, "not_implemented", "Chat inference is not implemented. This endpoint validates requests and checks quotas only.", "")
}

func decodeError(responseWriter http.ResponseWriter, err error) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeError(responseWriter, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 1 MiB.", "")
		return
	}
	// Do not echo decoder errors: unknown field names can contain client secrets.
	writeError(responseWriter, http.StatusBadRequest, "invalid_json", "Send one JSON object with supported fields and field types.", "")
}

func validateChatRequest(chatRequest ChatRequest) (string, string) {
	if strings.TrimSpace(chatRequest.Model) == "" {
		return "model", "model must be a non-empty string."
	}
	if len(chatRequest.Messages) == 0 {
		return "messages", "messages must contain at least one text message."
	}
	for messageIndex, message := range chatRequest.Messages {
		param := fmt.Sprintf("messages[%d]", messageIndex)
		switch message.Role {
		case "system", "developer", "user", "assistant":
		default:
			return param + ".role", "Supported roles: system, developer, user, assistant."
		}
		if message.Content == nil {
			return param + ".content", "content must be a text string; null and multimodal content are unsupported."
		}
	}
	if chatRequest.MaxTokens != nil && chatRequest.MaxCompletionTokens != nil {
		return "max_completion_tokens", "Use max_completion_tokens or max_tokens, not both."
	}
	if chatRequest.MaxTokens != nil && *chatRequest.MaxTokens < 1 {
		return "max_tokens", "max_tokens must be positive."
	}
	if chatRequest.MaxCompletionTokens != nil && *chatRequest.MaxCompletionTokens < 1 {
		return "max_completion_tokens", "max_completion_tokens must be positive."
	}
	if chatRequest.Temperature != nil && (*chatRequest.Temperature < 0 || *chatRequest.Temperature > 2) {
		return "temperature", "temperature must be between 0 and 2."
	}
	if chatRequest.StreamOptions != nil && !chatRequest.Stream {
		return "stream_options", "stream_options requires stream: true."
	}
	if chatRequest.RequestClass != "" && chatRequest.RequestClass != "interactive" && chatRequest.RequestClass != "background" {
		return "request_class", "request_class must be interactive or background."
	}
	return "", ""
}

func writeError(responseWriter http.ResponseWriter, status int, code, message, param string) {
	var field *string
	if param != "" {
		field = &param
	}
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(responseWriter, status, ErrorResponse{Error: APIError{Message: message, Type: kind, Param: field, Code: code}})
}

func writeJSON(responseWriter http.ResponseWriter, status int, value any) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(status)
	_ = json.NewEncoder(responseWriter).Encode(value)
}
