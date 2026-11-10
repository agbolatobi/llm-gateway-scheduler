package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-gateway-scheduler/gateway/internal/registry"
)

type fakeCounter struct {
	calls   atomic.Int64
	count   int
	failure error
}

func (f *fakeCounter) CountInput(context.Context, registry.Model, any) (int, error) {
	f.calls.Add(1)
	return f.count, f.failure
}

func registeredTest(t *testing.T, c registry.Config, f InputCounter) *RegisteredHandler {
	t.Helper()
	h, err := NewRegisteredHandler(userVerifier{}, c, f)
	if err != nil {
		t.Fatal(err)
	}
	// These GATE-04 tests isolate admission; GATE-06 tests use the live constructor.
	h.afterAdmission = nil
	h.report = func(TimingRecord) {}
	t.Cleanup(h.Close)
	return h
}

func TestModelLookupAndBudgets(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status, count    int
	}{
		{"unknown", strings.Replace(validBody, "local-llm", "unknown", 1), "model_not_found", 404, 5},
		{"output", strings.TrimSuffix(validBody, "}") + `,"max_tokens":1025}`, "output_limit_exceeded", 400, 5},
		{"context", validBody, "context_length_exceeded", 400, 4096},
		{"default", validBody, "not_implemented", 501, 5},
		{"alias", strings.Replace(validBody, "local-llm", "default", 1), "not_implemented", 501, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := registry.Default()
			f := &fakeCounter{count: tc.count}
			h := registeredTest(t, c, f)
			w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", tc.body)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatal(w.Code, w.Body)
			}
			if tc.status == 404 || tc.code == "output_limit_exceeded" {
				if f.calls.Load() != 0 {
					t.Fatal("invalid request reached backend")
				}
			}
			// Even the successful validation stub must release its token hold.
			if _, d := h.quotas.ReserveTokens("alice", 40000); !d.Allowed {
				t.Fatal("partial or leaked token charge", d)
			}
		})
	}
}

func TestValidKeysCanListAndRequestEveryModel(t *testing.T) {
	c := registry.Default()
	second := c.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	c.Models = append(c.Models, second)
	h := registeredTest(t, c, &fakeCounter{count: 5})
	for _, key := range []string{"alice-one", "bob"} {
		w := quotaRequest(h, "GET", "/v1/models", key, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "local-llm") || strings.Contains(w.Body.String(), "base_url") {
			t.Fatal(w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "second") {
			t.Fatal("authenticated user cannot see all registrations")
		}
		for _, name := range []string{"local-llm", "default", "second"} {
			w := quotaRequest(h, "POST", ChatCompletionsPath, key, strings.Replace(validBody, "local-llm", name, 1))
			if w.Code != 501 || !strings.Contains(w.Body.String(), "not_implemented") {
				t.Fatal("valid key did not reach validated stub", name, w.Code, w.Body)
			}
		}
	}
	f := &fakeCounter{count: 5}
	h = registeredTest(t, c, f)
	for _, key := range []string{"", "invalid"} {
		if w := quotaRequest(h, "GET", ModelsPath, key, ""); w.Code != 401 {
			t.Fatal("missing/invalid key could list models", w.Code)
		}
		for _, name := range []string{"local-llm", "second", "unknown"} {
			w := quotaRequest(h, "POST", ChatCompletionsPath, key, strings.Replace(validBody, "local-llm", name, 1))
			if w.Code != 401 {
				t.Fatal("authentication did not precede model validation", w.Code)
			}
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("unauthenticated request reached runtime")
	}
}

func TestModelAliasAndReloadDoNotResetQuota(t *testing.T) {
	c := registry.Default()
	c.Models[0].Limits.Requests = 1
	second := c.Models[0]
	second.ID = "second"
	second.Aliases = nil
	second.Limits.Requests = 30
	second.Backend.RuntimeID = "second"
	second.Backend.BaseURL = "http://127.0.0.1:8082"
	c.Models = append(c.Models, second)
	h := registeredTest(t, c, &fakeCounter{count: 5})
	if w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody); w.Code != 501 {
		t.Fatal(w.Code)
	}
	if err := h.Reload(c); err != nil {
		t.Fatal(err)
	}
	w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-two", strings.Replace(validBody, "local-llm", "default", 1))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "model_request_limit") {
		t.Fatal(w.Code, w.Body)
	}
	c.Models[0].Backend.BaseURL = "http://127.0.0.1:8099"
	if err := h.Reload(c); err == nil {
		t.Fatal("runtime change accepted")
	}
}

func TestTwoFakeRunningModels(t *testing.T) {
	var tokenCalls [2]atomic.Int64
	c := registry.Default()
	second := c.Models[0]
	second.ID = "second"
	second.Aliases = []string{"engineer"}
	second.Backend.RuntimeID = "second"
	second.Backend.ExpectedModel = "second"
	c.Models = append(c.Models, second)
	for index := range c.Models {
		index := index
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			case "/v1/models":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": c.Models[index].Backend.ExpectedModel}}})
			case "/slots":
				json.NewEncoder(w).Encode([]map[string]int{{"n_ctx": 4096}})
			case "/apply-template":
				json.NewEncoder(w).Encode(map[string]string{"prompt": "templated message"})
			case "/tokenize":
				tokenCalls[index].Add(1)
				json.NewEncoder(w).Encode(map[string]any{"tokens": []int{1, 2, 3}})
			default:
				t.Error("unexpected generation request")
				http.Error(w, "no inference", 500)
			}
		}))
		t.Cleanup(backend.Close)
		c.Models[index].Backend.BaseURL = backend.URL
	}
	h := registeredTest(t, c, nil)
	for _, name := range []string{"default", "engineer"} {
		w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", strings.Replace(validBody, "local-llm", name, 1))
		if w.Code != 501 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if tokenCalls[0].Load() != 1 || tokenCalls[1].Load() != 1 {
		t.Fatal("models not independently validated")
	}
}

func TestRuntimeFailureIsSanitized(t *testing.T) {
	h := registeredTest(t, registry.Default(), &fakeCounter{failure: errors.New("private diagnostic")})
	w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestConcurrentRegistryReload(t *testing.T) {
	c := registry.Default()
	h := registeredTest(t, c, &fakeCounter{count: 5})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Reload(c); err != nil {
				t.Error(err)
			}
			w := quotaRequest(h, "GET", "/v1/models", "alice-one", "")
			if w.Code != 200 {
				t.Error(w.Code)
			}
		}()
	}
	wg.Wait()
}

func TestGlobalDenialBeforeModelLookup(t *testing.T) {
	c := registry.Default()
	c.Quotas.User.Requests = 1
	f := &fakeCounter{count: 5}
	h := registeredTest(t, c, f)
	quotaRequest(h, "POST", "/v1/chat/completions", "alice-one", validBody)
	w := quotaRequest(h, "POST", "/v1/chat/completions", "alice-two", strings.Replace(validBody, "local-llm", "unknown", 1))
	if w.Code != 429 || f.calls.Load() != 1 {
		t.Fatal("lookup preceded quota check")
	}
}
