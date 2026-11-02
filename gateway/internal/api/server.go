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
func NewHandler(verifier Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if requiresAuthentication(r.URL.Path) {
			var ok bool
			r, ok = authenticate(w, r, verifier)
			if !ok {
				return
			}
		}
		switch r.URL.Path {
		case HealthPath:
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		case ChatCompletionsPath:
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			chat(w, r)
		case ModelsPath:
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			writeError(w, http.StatusNotImplemented, "not_implemented", "The model registry is not implemented.", "")
		default:
			writeError(w, http.StatusNotFound, "not_found", "Endpoint not found.", "")
		}
	})
}

func authenticate(w http.ResponseWriter, r *http.Request, verifier Authenticator) (*http.Request, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		unauthorized(w)
		return r, false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		unauthorized(w)
		return r, false
	}
	if verifier == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "API-key verification is temporarily unavailable.", "")
		return r, false
	}
	principal, err := verifier.Verify(parts[1])
	if errors.Is(err, auth.ErrInvalidKey) {
		unauthorized(w)
		return r, false
	}
	if err != nil || principal.UserID == "" || principal.KeyID == "" {
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "API-key verification is temporarily unavailable.", "")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)), true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="llm-gateway"`)
	writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: APIError{
		Message: "A valid API key is required. Use Authorization: Bearer <api_key>.",
		Type:    "authentication_error", Code: "invalid_api_key",
	}})
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+method+" for this endpoint.", "")
	return false
}

func chat(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.Header.Get("Content-Encoding") != "" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send an uncompressed application/json body.", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request *ChatRequest
	if err := decoder.Decode(&request); err != nil {
		decodeError(w, err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		decodeError(w, err)
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "The body must be a JSON object.", "")
		return
	}
	if param, message := validate(*request); message != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", message, param)
		return
	}
	writeError(w, http.StatusNotImplemented, "not_implemented", "Chat inference is not implemented. This endpoint validates requests only.", "")
}

func decodeError(w http.ResponseWriter, err error) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 1 MiB.", "")
		return
	}
	// Do not echo decoder errors: unknown field names can contain client secrets.
	writeError(w, http.StatusBadRequest, "invalid_json", "Send one JSON object with supported fields and field types.", "")
}

func validate(r ChatRequest) (string, string) {
	if strings.TrimSpace(r.Model) == "" {
		return "model", "model must be a non-empty string."
	}
	if len(r.Messages) == 0 {
		return "messages", "messages must contain at least one text message."
	}
	for i, message := range r.Messages {
		param := fmt.Sprintf("messages[%d]", i)
		switch message.Role {
		case "system", "developer", "user", "assistant":
		default:
			return param + ".role", "Supported roles: system, developer, user, assistant."
		}
		if message.Content == nil {
			return param + ".content", "content must be a text string; null and multimodal content are unsupported."
		}
	}
	if r.MaxTokens != nil && r.MaxCompletionTokens != nil {
		return "max_completion_tokens", "Use max_completion_tokens or max_tokens, not both."
	}
	if r.MaxTokens != nil && *r.MaxTokens < 1 {
		return "max_tokens", "max_tokens must be positive."
	}
	if r.MaxCompletionTokens != nil && *r.MaxCompletionTokens < 1 {
		return "max_completion_tokens", "max_completion_tokens must be positive."
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return "temperature", "temperature must be between 0 and 2."
	}
	if r.StreamOptions != nil && !r.Stream {
		return "stream_options", "stream_options requires stream: true."
	}
	if r.RequestClass != "" && r.RequestClass != "interactive" && r.RequestClass != "background" {
		return "request_class", "request_class must be interactive or background."
	}
	return "", ""
}

func writeError(w http.ResponseWriter, status int, code, message, param string) {
	var field *string
	if param != "" {
		field = &param
	}
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(w, status, ErrorResponse{Error: APIError{Message: message, Type: kind, Param: field, Code: code}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
