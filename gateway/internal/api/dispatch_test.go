package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/quota"
	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
)

// Test-only reply consumer. This is deliberately not a production proxy:
// GATE-06 must implement response validation, streaming, timeouts and timing.
func dispatchHarness(client *routing.Client) func(http.ResponseWriter, *http.Request, *preparedRequest) {
	return func(w http.ResponseWriter, r *http.Request, prepared *preparedRequest) {
		response, err := prepared.dispatch(r.Context(), client)
		if err != nil {
			writeDispatchError(w, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
		if err != nil {
			writeDispatchError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		w.Write(body)
	}
}

func fakeRoutes(t *testing.T, generate func(int, http.ResponseWriter, *http.Request)) registry.Config {
	t.Helper()
	c := registry.Default()
	second := c.Models[0]
	second.ID, second.Aliases = "second", []string{"engineer"}
	second.Backend.RuntimeID, second.Backend.ExpectedModel = "second-runtime", "second-backend"
	c.Models = append(c.Models, second)
	for i := range c.Models {
		i, identity := i, c.Models[i].Backend.ExpectedModel
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			case "/v1/models":
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": identity}}})
			case "/slots":
				w.Write([]byte(`[{"id":0,"n_ctx":4096,"is_processing":false}]`))
			case "/apply-template":
				w.Write([]byte(`{"prompt":"<chat>hello</chat>"}`))
			case "/tokenize":
				w.Write([]byte(`{"tokens":[1,2,3]}`))
			case ChatCompletionsPath:
				generate(i, w, r)
			default:
				t.Error("unexpected backend path", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(func() { backend.CloseClientConnections(); backend.Close() })
		c.Models[i].Backend.BaseURL = backend.URL
	}
	return c
}

func routingClient(t *testing.T) *routing.Client {
	t.Helper()
	client := routing.NewClient()
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func TestDispatchTwoBackendsAndPayload(t *testing.T) {
	var calls [2]atomic.Int64
	c := fakeRoutes(t, func(i int, w http.ResponseWriter, r *http.Request) {
		calls[i].Add(1)
		for _, name := range []string{"Authorization", "Cookie", "Idempotency-Key", "X-Idempotency-Key", "X-Forwarded-For"} {
			if r.Header.Get(name) != "" {
				t.Error("private/client header forwarded", name)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		expected := []string{"local-llm", "second-backend"}[i]
		if body["model"] != expected {
			t.Error("wrong backend identity", body)
		}
		if body["max_tokens"] != float64(128) && body["max_tokens"] != float64(64) {
			t.Error("effective cap missing", body)
		}
		if _, ok := body["request_class"]; ok {
			t.Error("request class leaked")
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Error("cap was not normalized")
		}
		json.NewEncoder(w).Encode(map[string]string{"served_by": expected})
	})
	h := registeredTest(t, c, nil)
	h.afterAdmission = dispatchHarness(routingClient(t))
	for _, tc := range []struct{ name, key, extra, expected string }{
		{"default", "alice-one", ``, "local-llm"},
		{"engineer", "bob", `,"max_completion_tokens":64`, "second-backend"},
		{"local-llm", "bob", `,"max_tokens":64`, "local-llm"},
		{"second", "alice-two", ``, "second-backend"},
	} {
		body := `{"model":"` + tc.name + `","messages":[{"role":"user","content":"hello"}],"request_class":"background"` + tc.extra + `}`
		r := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tc.key)
		for _, name := range []string{"Cookie", "Idempotency-Key", "X-Idempotency-Key", "X-Forwarded-For"} {
			r.Header.Set(name, "do-not-forward")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.expected) {
			t.Fatal(w.Code, w.Body)
		}
	}
	if calls[0].Load() != 2 || calls[1].Load() != 2 {
		t.Fatal("wrong routing", calls[0].Load(), calls[1].Load())
	}
	for _, tc := range []struct {
		key, body string
		status    int
	}{
		{"", validBody, 401}, {"invalid", validBody, 401},
		{"alice-one", strings.Replace(validBody, "local-llm", "unknown", 1), 404},
		{"alice-one", strings.TrimSuffix(validBody, "}") + `,"max_tokens":1025}`, 400},
	} {
		w := quotaRequest(h, "POST", ChatCompletionsPath, tc.key, tc.body)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body)
		}
	}
	if calls[0].Load() != 2 || calls[1].Load() != 2 {
		t.Fatal("rejected request generated work")
	}
}

func TestValidationOnlySeamDoesNotGenerate(t *testing.T) {
	var generations atomic.Int64
	c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) { generations.Add(1); w.Write([]byte(`{}`)) })
	h := registeredTest(t, c, nil) // Explicitly isolated validation-only test seam.
	w := quotaRequest(h, "POST", ChatCompletionsPath, "alice-one", validBody)
	if w.Code != 501 || generations.Load() != 0 {
		t.Fatal("stub performed generation", w.Code, generations.Load())
	}
	r, d := h.quotas.ReserveTokens("alice", 40000)
	if !d.Allowed {
		t.Fatal("stub leaked reservation", d)
	}
	h.quotas.SettleTokens(r, 0)
}

func TestDispatchCancellationFromHTTPClient(t *testing.T) {
	started, cancelled, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	h := registeredTest(t, c, nil)
	harness := dispatchHarness(routingClient(t))
	h.afterAdmission = func(w http.ResponseWriter, r *http.Request, p *preparedRequest) {
		defer close(handled)
		harness(w, r, p)
		// Finish before notifying the test; the handler's deferred finish is a no-op.
		if err := p.finish(); err != nil {
			t.Error(err)
		}
	}
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+ChatCompletionsPath, strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer alice-one")
	req.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("backend did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend did not observe cancellation")
	}
	select {
	case <-handled:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not clean up")
	}
	if err := <-result; err == nil {
		t.Fatal("client did not report cancellation")
	}
	// 3 input + 128 output are conservatively charged, not refunded as zero.
	r, d := h.quotas.ReserveTokens("alice", 40000-131)
	if !d.Allowed {
		t.Fatal("reservation leak", d)
	}
	if _, d := h.quotas.ReserveTokens("alice", 1); d.Allowed {
		t.Fatal("ambiguous generation was refunded")
	}
	h.quotas.SettleTokens(r, 0)
}

func TestReloadDuringSlowDispatchUsesSnapshot(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := fakeRoutes(t, func(i int, w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if i == 0 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]int{"backend": i})
	})
	h := registeredTest(t, c, nil)
	h.afterAdmission = dispatchHarness(routingClient(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(strings.Replace(validBody, "local-llm", "default", 1))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer alice-one")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch not started")
	}
	next := c.Clone()
	next.Models[0].Aliases = nil
	next.Models[1].Aliases = []string{"engineer", "default"}
	reloaded := make(chan error, 1)
	go func() { reloaded <- h.Reload(next) }()
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow backend blocked reload")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch stuck")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"backend":0`) {
		t.Fatal("in-flight route changed", w.Code, w.Body)
	}
	second := quotaRequest(h, "POST", ChatCompletionsPath, "alice-one", strings.Replace(validBody, "local-llm", "default", 1))
	if second.Code != 200 || !strings.Contains(second.Body.String(), `"backend":1`) {
		t.Fatal("new route not applied", second.Code, second.Body)
	}
}

func TestPreparedSnapshotAndCleanup(t *testing.T) {
	for _, scenario := range []string{"unused", "missing route", "refused connection", "backend error", "success"} {
		t.Run(scenario, func(t *testing.T) {
			c := registry.Default()
			c.Quotas.User.Tokens = 200
			c.Quotas.GlobalCeiling.Tokens = 200
			c.Models[0].Limits.Tokens = 200
			q, err := quota.New(c.QuotaConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			reservation, d := q.ReserveForModel("alice", "local-llm", 133)
			if !d.Allowed {
				t.Fatal(d)
			}
			content := "original"
			temperature := 0.5
			request := ChatRequest{Messages: []Message{{Role: "user", Content: &content}}, Temperature: &temperature}
			if scenario == "missing route" {
				c.Models[0].Backend.BaseURL = ""
			}
			if scenario == "refused connection" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				c.Models[0].Backend.BaseURL = "http://" + listener.Addr().String()
				listener.Close()
			}
			if scenario == "backend error" || scenario == "success" {
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					if scenario == "backend error" {
						w.WriteHeader(500)
					}
					w.Write([]byte(`{}`))
				}))
				defer backend.Close()
				c.Models[0].Backend.BaseURL = backend.URL
			}
			p, err := newPreparedRequest(c.Models[0], request, 128, q, reservation, 133)
			if err != nil {
				t.Fatal(err)
			}
			content = "modified"
			temperature = 1
			c.Models[0].Backend.ExpectedModel = "changed"
			c.Models[0].Aliases[0] = "changed"
			if strings.Contains(string(p.payload.Messages), "modified") || *p.payload.Temperature != 0.5 || p.model.Backend.ExpectedModel != "local-llm" || p.model.Aliases[0] != "default" {
				t.Fatal("prepared request shared mutable input")
			}
			client := routingClient(t)
			if scenario != "unused" {
				_, err = p.dispatch(context.Background(), client)
				if (err == nil) != (scenario == "success") {
					t.Fatal(scenario, err)
				}
				if _, err := p.dispatch(context.Background(), client); err == nil {
					t.Fatal("double dispatch accepted")
				}
			}
			if err := p.finish(); err != nil {
				t.Fatal(err)
			}
			if err := p.finish(); err != nil {
				t.Fatal("cleanup not idempotent", err)
			}
			if _, err := p.dispatch(context.Background(), client); err == nil {
				t.Fatal("dispatch after cleanup")
			}
			remaining := int64(200)
			if scenario == "backend error" || scenario == "success" {
				remaining -= 133
			}
			r, d := q.ReserveForModel("alice", "local-llm", remaining)
			if !d.Allowed {
				t.Fatal("hold leaked", d)
			}
			if _, d := q.ReserveForModel("alice", "local-llm", 1); d.Allowed {
				t.Fatal("incorrect token accounting")
			}
			q.SettleTokens(r, 0)
		})
	}
}
