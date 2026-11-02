package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway-scheduler/gateway/internal/auth"
)

const validBody = `{"model":"local-llm","messages":[{"role":"user","content":"Hello"}]}`

type testVerifier struct{}

func (testVerifier) Verify(token string) (auth.Principal, error) {
	if token != "test-key" {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	return auth.Principal{KeyID: "test-id", UserID: "test-user"}, nil
}

func TestEndpoints(t *testing.T) {
	tests := []struct {
		name, method, path, body, contentType string
		status                                int
		code, param, allow                    string
	}{
		{name: "health", method: "GET", path: "/health", status: 200},
		{name: "chat stub", method: "POST", path: "/v1/chat/completions", body: validBody, contentType: "application/json", status: 501, code: "not_implemented"},
		{name: "models stub", method: "GET", path: "/v1/models", status: 501, code: "not_implemented"},
		{name: "chat method", method: "GET", path: "/v1/chat/completions", status: 405, code: "method_not_allowed", allow: "POST"},
		{name: "health method", method: "POST", path: "/health", status: 405, code: "method_not_allowed", allow: "GET"},
		{name: "models method", method: "POST", path: "/v1/models", status: 405, code: "method_not_allowed", allow: "GET"},
		{name: "unknown", method: "GET", path: "/v1/responses", status: 404, code: "not_found"},
		{name: "missing content type", body: validBody, status: 415, code: "unsupported_media_type"},
		{name: "wrong content type", body: validBody, contentType: "text/plain", status: 415, code: "unsupported_media_type"},
		{name: "empty body", contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "malformed", body: `{`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "null object", body: `null`, contentType: "application/json", status: 400, code: "invalid_request"},
		{name: "array object", body: `[]`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "multiple objects", body: validBody + validBody, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "model"},
		{name: "empty model", body: `{"model":"  ","messages":[]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "model"},
		{name: "empty messages", body: `{"model":"m","messages":[]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "messages"},
		{name: "unsupported role", body: `{"model":"m","messages":[{"role":"tool","content":"hi"}]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "messages[0].role"},
		{name: "missing content", body: `{"model":"m","messages":[{"role":"user"}]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "messages[0].content"},
		{name: "null content", body: `{"model":"m","messages":[{"role":"user","content":null}]}`, contentType: "application/json", status: 400, code: "invalid_request", param: "messages[0].content"},
		{name: "multimodal", body: `{"model":"m","messages":[{"role":"user","content":[]}]}`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "unsupported tools", body: strings.TrimSuffix(validBody, "}") + `,"tools":[]}`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "unsupported message field", body: `{"model":"m","messages":[{"role":"user","content":"hi","extra":1}]}`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "negative cap", body: strings.TrimSuffix(validBody, "}") + `,"max_tokens":-1}`, contentType: "application/json", status: 400, code: "invalid_request", param: "max_tokens"},
		{name: "zero cap", body: strings.TrimSuffix(validBody, "}") + `,"max_completion_tokens":0}`, contentType: "application/json", status: 400, code: "invalid_request", param: "max_completion_tokens"},
		{name: "conflicting caps", body: strings.TrimSuffix(validBody, "}") + `,"max_tokens":1,"max_completion_tokens":1}`, contentType: "application/json", status: 400, code: "invalid_request", param: "max_completion_tokens"},
		{name: "fractional cap", body: strings.TrimSuffix(validBody, "}") + `,"max_tokens":1.5}`, contentType: "application/json", status: 400, code: "invalid_json"},
		{name: "bad temperature", body: strings.TrimSuffix(validBody, "}") + `,"temperature":3}`, contentType: "application/json", status: 400, code: "invalid_request", param: "temperature"},
		{name: "nonstream options", body: strings.TrimSuffix(validBody, "}") + `,"stream_options":{"include_usage":true}}`, contentType: "application/json", status: 400, code: "invalid_request", param: "stream_options"},
		{name: "bad class", body: strings.TrimSuffix(validBody, "}") + `,"request_class":"urgent"}`, contentType: "application/json", status: 400, code: "invalid_request", param: "request_class"},
		{name: "too large", body: strings.Repeat(" ", MaxBodyBytes+1), contentType: "application/json", status: 413, code: "body_too_large"},
		{name: "large trailing data", body: validBody + strings.Repeat(" ", MaxBodyBytes), contentType: "application/json", status: 413, code: "body_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			method, path := test.method, test.path
			if method == "" {
				method = "POST"
			}
			if path == "" {
				path = "/v1/chat/completions"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Authorization", "Bearer test-key")
			response := httptest.NewRecorder()
			NewHandler(testVerifier{}).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body)
			}
			if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing JSON/no-store headers")
			}
			if response.Header().Get("Allow") != test.allow {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), test.allow)
			}
			if test.status == http.StatusOK {
				var health map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil || health["status"] != "ok" {
					t.Fatalf("invalid health response: %s", response.Body)
				}
				return
			}
			var result ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != test.code || result.Error.Message == "" || result.Error.Type == "" {
				t.Fatalf("invalid error response: %+v", result)
			}
			if test.param == "" && result.Error.Param != nil || test.param != "" && (result.Error.Param == nil || *result.Error.Param != test.param) {
				t.Fatalf("unexpected error param: %+v", result.Error)
			}
		})
	}
}

func TestSupportedOptionsRemainStubs(t *testing.T) {
	for _, role := range []string{"system", "developer", "user", "assistant"} {
		for _, cap := range []string{"max_tokens", "max_completion_tokens"} {
			t.Run(role+"/"+cap, func(t *testing.T) {
				body := `{"model":"local-llm","messages":[{"role":"` + role + `","content":"","name":"demo"}],"` + cap + `":64,"temperature":0,"stream":true,"stream_options":{"include_usage":true},"request_class":"background"}`
				request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json; charset=utf-8")
				request.Header.Set("Authorization", "Bearer test-key")
				response := httptest.NewRecorder()
				NewHandler(testVerifier{}).ServeHTTP(response, request)
				if response.Code != http.StatusNotImplemented || strings.Contains(response.Body.String(), "[DONE]") {
					t.Fatalf("stub must not fabricate a stream: %d %s", response.Code, response.Body)
				}
			})
		}
	}
}

func TestCompressedBodyRejected(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(validBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	NewHandler(testVerifier{}).ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatal(response.Code)
	}
}

func TestResponseSchema(t *testing.T) {
	text := "hello"
	completion := Completion{
		ID: "chatcmpl-example", Object: "chat.completion", Created: 1, Model: "local-llm",
		Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: &text}, FinishReason: "stop"}},
		Usage:   &Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	data, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil || result["object"] != "chat.completion" {
		t.Fatalf("invalid completion: %s", data)
	}
	chunk := Chunk{ID: "chatcmpl-example", Object: "chat.completion.chunk", Created: 1, Model: "local-llm", Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: &text}}}}
	data, err = json.Marshal(chunk)
	if err != nil || !strings.Contains(string(data), `"finish_reason":null`) || strings.Contains(string(data), `"message"`) {
		t.Fatalf("invalid chunk: %s, %v", data, err)
	}
}
