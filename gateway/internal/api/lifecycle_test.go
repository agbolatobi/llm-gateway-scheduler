package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-gateway-scheduler/gateway/internal/capacity"
	"llm-gateway-scheduler/gateway/internal/registry"
)

func lifecycleBackend(t *testing.T, busy *atomic.Bool, reply http.HandlerFunc) registry.Config {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			fmt.Fprintf(w, `[{"id":0,"n_ctx":4096,"is_processing":%t}]`, busy.Load())
			return
		}
		if r.URL.Path == ChatCompletionsPath {
			reply(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() { s.CloseClientConnections(); s.Close() })
	c := registry.Default()
	c.Models[0].Backend.BaseURL = s.URL
	return c
}
func lifecycleHandler(t *testing.T, c registry.Config, opts ProxyOptions) (*RegisteredHandler, <-chan TimingRecord) {
	h, reports := liveProxy(t, c, opts)
	h.counter = &fakeCounter{count: 3}
	return h, reports
}
func withUsage(body, usage string) string {
	return strings.TrimSuffix(body, "}") + `,"usage":` + usage + "}"
}

const validUsage = `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`

func TestLifecycleActualUsageRefundsBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var busy atomic.Bool
			c := lifecycleBackend(t, &busy, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					var request ChatRequest
					json.NewDecoder(r.Body).Decode(&request)
					if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
						t.Error("usage was not requested internally")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, chunkFrame("hello", "")+chunkFrame("", "stop")+`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[],"usage":`+validUsage+"}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, withUsage(completion("hello"), validUsage))
				}
			})
			c.Quotas.User.Tokens = 140
			h, reports := lifecycleHandler(t, c, DefaultProxyOptions())
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, proxyRequest("default", stream))
				if w.Code != 200 {
					t.Fatal("actual usage was not refunded", w.Code, w.Body)
				}
				if stream && strings.Contains(w.Body.String(), `"usage"`) {
					t.Fatal("unsolicited usage event leaked")
				}
				r := getReport(t, reports)
				if r.ReservedTokens != 131 || r.ChargedTokens != 5 || r.UsageSource != "backend" || r.CapacityState != "released" || r.UserID != "alice" || r.Model != "local-llm" || r.RuntimeID == "" {
					t.Fatalf("bad settlement: %+v", r)
				}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, proxyRequest("default", stream))
			if w.Code != 429 {
				t.Fatal("spent tokens were not retained", w.Code, w.Body)
			}
		})
	}
}

func TestLifecycleUsageFallback(t *testing.T) {
	for _, usage := range []string{"null", `{}`, `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":6}`, `{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}`, `{"prompt_tokens":3,"completion_tokens":129,"total_tokens":132}`, `{"prompt_tokens":3,"completion_tokens":-1,"total_tokens":2}`, `{"prompt_tokens":3,"completion_tokens":2.5,"total_tokens":5.5}`} {
		var busy atomic.Bool
		c := lifecycleBackend(t, &busy, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, withUsage(completion("hello"), usage))
		})
		c.Quotas.User.Tokens = 140
		h, reports := lifecycleHandler(t, c, DefaultProxyOptions())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, proxyRequest("local-llm", false))
		r := getReport(t, reports)
		if w.Code != 200 || r.ChargedTokens != 131 || r.UsageSource != "fallback" || r.Usage != nil {
			t.Fatal(usage, w.Code, r)
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, proxyRequest("local-llm", false))
		if w.Code != 429 {
			t.Fatal("fallback undercharged", w.Code)
		}
	}
}

func TestLifecycleBusyAliasRefundAndRecovery(t *testing.T) {
	var busy atomic.Bool
	started, release := make(chan struct{}), make(chan struct{})
	c := lifecycleBackend(t, &busy, func(w http.ResponseWriter, r *http.Request) {
		busy.Store(true)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		busy.Store(false)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, withUsage(completion("hello"), validUsage))
	})
	c.Quotas.User.Tokens = 400
	h, reports := lifecycleHandler(t, c, DefaultProxyOptions())
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), proxyRequest("local-llm", false)) }()
	<-started
	w := httptest.NewRecorder()
	h.ServeHTTP(w, proxyRequest("default", false))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "capacity_exceeded") {
		t.Fatal(w.Code, w.Body)
	}
	r := getReport(t, reports)
	if r.ChargedTokens != 0 || r.UsageSource != "not_dispatched" {
		t.Fatal(r)
	}
	next := c.Clone()
	next.Models = nil
	if h.Reload(next) == nil {
		t.Fatal("removed occupied model")
	}
	close(release)
	<-done
	getReport(t, reports)
	// Reservation state is observable without adding a production debug endpoint.
	reservation, decision := h.quotas.ReserveForModel("alice", "local-llm", 395)
	if !decision.Allowed {
		t.Fatal("busy rejection leaked tokens", decision)
	}
	h.quotas.SettleTokens(reservation, 0)
	base, _ := registry.LocalURL(c.Models[0].Backend.BaseURL)
	if h.capacity.Occupied(base) {
		t.Fatal("completed request leaked capacity")
	}
}

func TestLifecycleCancellationWaitsForBackendIdle(t *testing.T) {
	var busy atomic.Bool
	started, cancelled := make(chan struct{}), make(chan struct{})
	c := lifecycleBackend(t, &busy, func(w http.ResponseWriter, r *http.Request) {
		busy.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunkFrame("hello", ""))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	h, reports := lifecycleHandler(t, c, DefaultProxyOptions())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &observedWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil && p != http.ErrAbortHandler {
				t.Errorf("panic: %v", p)
			}
		}()
		h.ServeHTTP(w, proxyRequest("local-llm", true).WithContext(ctx))
	}()
	<-started
	// Wait for the gateway to observe a backend event before cancelling.
	<-w.ready
	cancel()
	<-cancelled
	base, _ := registry.LocalURL(c.Models[0].Backend.BaseURL)
	if !h.capacity.Occupied(base) {
		t.Fatal("capacity released on HTTP disconnect")
	}
	<-done
	r := getReport(t, reports)
	if r.CapacityState != "quarantined_pending_idle" || r.ChargedTokens != 131 || r.UsageSource != "fallback" {
		t.Fatal(r)
	}
	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, proxyRequest("default", false))
	if probe.Code != 503 || !strings.Contains(probe.Body.String(), "capacity_unconfirmed") {
		t.Fatal(probe.Code, probe.Body)
	}
	getReport(t, reports)
	busy.Store(false)
	if !h.capacity.Recover(context.Background(), base, func(ctx context.Context) (bool, error) { return h.execution.Idle(ctx, c.Models[0]) }) {
		t.Fatal("idle runtime did not recover")
	}
	l, err := h.capacity.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}

type observedWriter struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (w *observedWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(b)
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func TestLifecycleUnknownDispatchDoesNotTrustIdle(t *testing.T) {
	var busy atomic.Bool
	c := lifecycleBackend(t, &busy, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	h, reports := lifecycleHandler(t, c, DefaultProxyOptions())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, proxyRequest("local-llm", false))
	r := getReport(t, reports)
	if r.CapacityState != "quarantined_unknown_dispatch" || r.ChargedTokens != 131 {
		t.Fatal(r)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, proxyRequest("local-llm", false))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "capacity_unconfirmed") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestLifecycleFinishExactlyOnce(t *testing.T) {
	h := registeredTest(t, registry.Default(), &fakeCounter{count: 3})
	reservation, d := h.quotas.ReserveForModel("alice", "local-llm", 131)
	if !d.Allowed {
		t.Fatal(d)
	}
	p, err := newPreparedRequest(registry.Default().Models[0], ChatRequest{Messages: []Message{}}, 128, h.quotas, reservation, 131)
	if err != nil {
		t.Fatal(err)
	}
	m := capacity.New(1)
	p.lease, _ = m.Acquire("backend")
	if err := p.finish(); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(); err != nil {
		t.Fatal(err)
	}
	l, err := m.Acquire("backend")
	if err != nil {
		t.Fatal(err)
	}
	p.finish()
	if _, err = m.Acquire("other"); err != capacity.ErrFull {
		t.Fatal("duplicate cleanup freed another request", err)
	}
	l.Release()
}
