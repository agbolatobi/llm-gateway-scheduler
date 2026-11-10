package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/auth"
	"llm-gateway-scheduler/gateway/internal/quota"
)

type userVerifier struct{}

func (userVerifier) Verify(key string) (auth.Principal, error) {
	user := ""
	switch key {
	case "alice-one", "alice-two":
		user = "alice"
	case "bob":
		user = "bob"
	}
	if user == "" {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	return auth.Principal{UserID: user, KeyID: key}, nil
}

func quotaRequest(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestQuotaOrderingAndSharedKeys(t *testing.T) {
	c := quota.DefaultConfig()
	c.User.Requests = 2
	m, err := quota.New(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	h := newHandler(userVerifier{}, m, func(w http.ResponseWriter, r *http.Request, request ChatRequest) { called++; w.WriteHeader(204) })
	for _, tc := range []struct {
		method, path, key, body string
		want                    int
	}{
		{"POST", "/v1/chat/completions", "bad", validBody, 401},
		{"POST", "/v1/chat/completions", "alice-one", "{", 400},
		{"GET", "/v1/chat/completions", "alice-one", "", 405},
		{"GET", "/health", "", "", 200},
		{"GET", "/v1/models", "alice-one", "", 501},
		{"POST", "/v1/chat/completions", "alice-one", validBody, 204},
		{"POST", "/v1/chat/completions", "alice-two", strings.TrimSuffix(validBody, "}") + `,"stream":true}`, 204},
		{"POST", "/v1/chat/completions", "alice-one", validBody, 429},
		{"POST", "/v1/chat/completions", "alice-two", validBody, 429},
		{"POST", "/v1/chat/completions", "bob", validBody, 204},
		{"GET", "/health", "", "", 200},
		{"GET", "/v1/models", "alice-one", "", 501},
	} {
		w := quotaRequest(h, tc.method, tc.path, tc.key, tc.body)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d want %d: %s", tc.method, tc.path, w.Code, tc.want, w.Body)
		}
	}
	if called != 3 {
		t.Fatalf("rejected request reached downstream boundary: %d calls", called)
	}
}

type fixedDecision struct{ decision quota.Decision }

func (f fixedDecision) CheckAndCount(string) quota.Decision { return f.decision }

func TestQuotaErrors(t *testing.T) {
	for _, code := range []string{"user_request_limit", "global_request_limit", "user_token_limit", "global_token_limit"} {
		h := newHandler(userVerifier{}, fixedDecision{quota.Decision{Code: code, RetryAfter: 1500 * time.Millisecond}}, func(http.ResponseWriter, *http.Request, ChatRequest) { t.Fatal("downstream called") })
		w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody)
		var body ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 429 || body.Error.Type != "rate_limit_error" || body.Error.Code != code || body.Error.Param != nil || w.Header().Get("Retry-After") != "2" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("bad error: %d %s %v", w.Code, w.Body, w.Header())
		}
	}
	for _, checker := range []QuotaChecker{nil, fixedDecision{quota.Decision{Code: "sensitive internal failure"}}} {
		w := quotaRequest(NewHandler(userVerifier{}, checker), "POST", "/v1/chat/completions", "alice-one", validBody)
		if w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatal("must fail closed and sanitized")
		}
	}
}

func TestExhaustedTokensRejectBeforeDownstream(t *testing.T) {
	m, err := quota.New(quota.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := m.ReserveTokens("alice", 40000); !d.Allowed {
		t.Fatal(d)
	}
	h := newHandler(userVerifier{}, m, func(http.ResponseWriter, *http.Request, ChatRequest) {
		t.Fatal("token-exhausted request reached model stage")
	})
	w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "user_token_limit") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestStubCountsRequestsButNotTokens(t *testing.T) {
	c := quota.DefaultConfig()
	c.User.Requests = 1
	m, err := quota.New(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(userVerifier{}, m)
	if w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody); w.Code != 501 {
		t.Fatal(w.Code)
	}
	if w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-two", validBody); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if _, d := m.ReserveTokens("alice", 40000); !d.Allowed {
		t.Fatal("stub spent tokens", d)
	}
}
