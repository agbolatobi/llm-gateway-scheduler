package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func liveProxy(t *testing.T, c registry.Config, options ProxyOptions) (*RegisteredHandler, <-chan TimingRecord) {
	t.Helper()
	h, err := NewRegisteredHandlerWithOptions(userVerifier{}, c, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan TimingRecord, 16)
	h.report = func(r TimingRecord) { reports <- r }
	t.Cleanup(h.Close)
	return h, reports
}

func proxyRequest(model string, stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, model, stream)
	r := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer alice-one")
	r.Header.Set("X-Request-ID", "test-request-1")
	return r
}

func completion(text string) string {
	b, _ := json.Marshal(map[string]any{"id": "chat-1", "object": "chat.completion", "created": 1, "model": "local-llm", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	return string(b)
}

func chunkFrame(content, finish string) string {
	var reason any
	if finish != "" {
		reason = finish
	}
	b, _ := json.Marshal(map[string]any{"id": "chat-1", "object": "chat.completion.chunk", "created": 1, "model": "local-llm", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": content}, "finish_reason": reason}}})
	return "data: " + string(b) + "\n\n"
}

func getReport(t *testing.T, reports <-chan TimingRecord) TimingRecord {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("missing timing record")
		return TimingRecord{}
	}
}

func TestLiveProxyNormalAndRequestIdentity(t *testing.T) {
	c := fakeRoutes(t, func(i int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-ID") != "test-request-1" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect forwarded headers")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completion(fmt.Sprint(i)))
	})
	h, reports := liveProxy(t, c, DefaultProxyOptions())
	for i, model := range []string{"default", "engineer"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, proxyRequest(model, false))
		if w.Code != 200 || w.Body.String() != completion(fmt.Sprint(i)) || w.Header().Get("X-Request-ID") != "test-request-1" {
			t.Fatal(w.Code, w.Body)
		}
		r := getReport(t, reports)
		if r.Outcome != "completed" || r.DispatchMS == nil || r.FirstTextMS == nil || r.CompletionMS == nil || *r.DispatchMS > *r.FirstTextMS || *r.FirstTextMS > *r.CompletionMS || *r.CompletionMS > r.EndedMS {
			t.Fatalf("bad timing: %+v", r)
		}
	}
}

func TestLiveProxyStreamsBeforeBackendCompletes(t *testing.T) {
	release := make(chan struct{})
	c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunkFrame("hello", ""))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, chunkFrame("", "stop")+"data: [DONE]\n\n")
	})
	h, reports := liveProxy(t, c, DefaultProxyOptions())
	s := httptest.NewServer(h)
	defer s.Close()
	r := proxyRequest("local-llm", true)
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(s.URL, "http://")
	r.RequestURI = ""
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "hello") {
		t.Fatal(line, err)
	}
	select {
	case <-reports:
		t.Fatal("request ended before backend completed")
	default:
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "[DONE]") || !strings.Contains(string(rest), `"finish_reason":"stop"`) {
		t.Fatal(string(rest), err)
	}
	if getReport(t, reports).Outcome != "completed" {
		t.Fatal("not completed")
	}
}

func TestLiveProxyBrokenStreams(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"eof", ""},
		{"finish_without_done", chunkFrame("", "stop")},
		{"early_done", "data: [DONE]\n\n"},
		{"malformed", "data: {bad}\n\n"},
		{"missing_created", strings.Replace(chunkFrame("other", ""), `"created":1,`, "", 1)},
		{"missing_index", strings.Replace(chunkFrame("other", ""), `,"index":0`, "", 1)},
		{"error", "event: error\ndata: {\"error\":\"private secret\"}\n\n"},
		{"identity_change", strings.Replace(chunkFrame("other", ""), "chat-1", "chat-2", 1)},
		{"oversized", "data: " + strings.Repeat("x", maxEventBytes) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, chunkFrame("hello", "")+tc.tail)
			})
			h, reports := liveProxy(t, c, DefaultProxyOptions())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, proxyRequest("local-llm", true))
			body := w.Body.String()
			if !strings.Contains(body, "hello") || !strings.Contains(body, "event: error") || strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "private secret") {
				t.Fatal(body)
			}
			r := getReport(t, reports)
			if r.CompletionMS != nil || r.Outcome == "completed" {
				t.Fatalf("false completion: %+v", r)
			}
		})
	}
}

func TestLiveProxyRejectsBadNormalResponses(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"malformed", "application/json", `{"id":`, 200},
		{"missing_fields", "application/json", `{}`, 200},
		{"wrong_type", "text/html", "private secret", 200},
		{"backend_error", "application/json", `{"error":"private secret"}`, 500},
		{"wrong_status", "application/json", completion("hello"), 201},
		{"duplicate_key", "application/json", strings.Replace(completion("hello"), `"id":"chat-1"`, `"id":"chat-1","id":"chat-2"`, 1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			h, reports := liveProxy(t, c, DefaultProxyOptions())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, proxyRequest("local-llm", false))
			if w.Code != 502 || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "private secret") {
				t.Fatal(w.Code, w.Body)
			}
			if getReport(t, reports).CompletionMS != nil {
				t.Fatal("false completion")
			}
		})
	}
}

func TestLiveProxyTimeoutsCancelBackend(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stream      bool
		idle, total time.Duration
		code        string
	}{
		{"idle_before_output", false, 60 * time.Millisecond, 2 * time.Second, "backend_idle_timeout"},
		{"idle_after_output", true, 60 * time.Millisecond, 2 * time.Second, "backend_idle_timeout"},
		{"overall_after_output", true, 2 * time.Second, 150 * time.Millisecond, "request_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := make(chan struct{}, 1)
			c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, chunkFrame("hello", ""))
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				cancelled <- struct{}{}
			})
			opts := DefaultProxyOptions()
			opts.IdleTimeout = tc.idle
			opts.RequestTimeout = tc.total
			h, reports := liveProxy(t, c, opts)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, proxyRequest("local-llm", tc.stream))
			if !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatal(w.Code, w.Body)
			}
			if !tc.stream && w.Code != 504 {
				t.Fatal(w.Code)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("backend not cancelled")
			}
			r := getReport(t, reports)
			if r.Outcome != tc.code || r.CompletionMS != nil {
				t.Fatalf("bad timeout timing: %+v", r)
			}
		})
	}
}

func TestLiveProxyClientCancellation(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunkFrame("hello", ""))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
	h, reports := liveProxy(t, c, DefaultProxyOptions())
	s := httptest.NewServer(h)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := proxyRequest("local-llm", true).WithContext(ctx)
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(s.URL, "http://")
	r.RequestURI = ""
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("backend continued after client disconnected")
	}
	rpt := getReport(t, reports)
	if rpt.Outcome != "request_cancelled" || rpt.CompletionMS != nil {
		t.Fatalf("bad cancellation: %+v", rpt)
	}
}

func TestLiveProxyLargeNormalReplyIsIncremental(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			release := make(chan struct{})
			body := completion(strings.Repeat("x", 100<<10))
			cut := len(body) - 100
			c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body[:cut])
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if valid {
					io.WriteString(w, body[cut:])
				}
			})
			h, reports := liveProxy(t, c, DefaultProxyOptions())
			s := httptest.NewServer(h)
			defer s.Close()
			r := proxyRequest("local-llm", false)
			r.URL.Scheme = "http"
			r.URL.Host = strings.TrimPrefix(s.URL, "http://")
			r.RequestURI = ""
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			first := make([]byte, 100)
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Fatal(err)
			}
			close(release)
			rest, err := io.ReadAll(resp.Body)
			if valid {
				if err != nil || string(append(first, rest...)) != body {
					t.Fatal("normal reply changed", err)
				}
			} else if err == nil {
				t.Fatal("truncated reply got a clean HTTP completion")
			}
			rpt := getReport(t, reports)
			if valid != (rpt.CompletionMS != nil) {
				t.Fatalf("bad completion timing: %+v", rpt)
			}
		})
	}
}

func TestStreamFramingAndTiming(t *testing.T) {
	usage := `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\n"
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		body := ": heartbeat\n\n" + chunkFrame("", "") + chunkFrame("hello", "") + chunkFrame("", "stop") + usage + "data: [DONE]\n\n"
		body = strings.ReplaceAll(body, "\n", newline)
		w := httptest.NewRecorder()
		trace, _ := newTrace("")
		out := &proxyOutput{w: w, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
		if err := relaySSE(strings.NewReader(body), out, trace); err != nil {
			t.Fatal(err)
		}
		if trace.firstText.IsZero() || !strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatal("missing text/completion")
		}
	}
	w := httptest.NewRecorder()
	trace, _ := newTrace("")
	out := &proxyOutput{w: w, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(chunkFrame("", "")+chunkFrame("", "stop")+"data: [DONE]\n\n"), out, trace); err != nil {
		t.Fatal(err)
	}
	if !trace.firstText.IsZero() {
		t.Fatal("metadata incorrectly counted as generated text")
	}
}

func TestProxyOptionsAndRequestIDs(t *testing.T) {
	for _, o := range []ProxyOptions{{}, {time.Hour, time.Second, time.Second}, {time.Second, 0, time.Second}, {time.Second, time.Second, 2 * time.Minute}} {
		if o.Validate() == nil {
			t.Fatal("accepted invalid limits", o)
		}
	}
	for _, id := range []string{"", "bad id", strings.Repeat("x", 65)} {
		trace, err := newTrace(id)
		if err != nil || len(trace.id) != 32 || trace.id == id {
			t.Fatal("bad generated ID")
		}
	}
	trace, _ := newTrace("safe-request_1.2")
	if trace.id != "safe-request_1.2" {
		t.Fatal("did not preserve safe ID")
	}
}

// A deterministic socket stand-in verifies that each write receives a deadline
// and that a failed flush cancels upstream work without a synthetic completion.
type stalledWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *stalledWriter) SetWriteDeadline(d time.Time) error { w.deadline = d; return nil }
func (w *stalledWriter) FlushError() error                  { return context.DeadlineExceeded }

func TestLiveProxyFailedClientWriteCancelsBackend(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	c := fakeRoutes(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, chunkFrame("hello", ""))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
	opts := DefaultProxyOptions()
	opts.WriteTimeout = 50 * time.Millisecond
	h, reports := liveProxy(t, c, opts)
	w := &stalledWriter{ResponseRecorder: httptest.NewRecorder()}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("expected HTTP abort, got %v", got)
			}
		}()
		h.ServeHTTP(w, proxyRequest("local-llm", true))
	}()
	if w.deadline.IsZero() || time.Until(w.deadline) > opts.WriteTimeout {
		t.Fatal("missing write deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("backend not cancelled")
	}
	r := getReport(t, reports)
	if r.Outcome != "client_write_failed" || r.CompletionMS != nil || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal(r, w.Body)
	}
}

func TestBoundedResponseReaders(t *testing.T) {
	r := &limitedReader{reader: strings.NewReader("12345"), remaining: 4}
	b, err := io.ReadAll(r)
	if string(b) != "1234" || !errors.Is(err, errLimit) {
		t.Fatal(string(b), err)
	}
	w := httptest.NewRecorder()
	out := &proxyOutput{w: w, ctx: context.Background(), timeout: time.Second, contentType: "application/json"}
	err = relayJSON(&limitedReader{reader: strings.NewReader(completion(strings.Repeat("x", maxJSONBytes))), remaining: maxJSONBytes}, out, nil)
	if !errors.Is(err, errLimit) || json.Valid(w.Body.Bytes()) {
		t.Fatal("oversized normal response looked complete", err)
	}
}

func TestMultilineSSEData(t *testing.T) {
	body := strings.ReplaceAll(chunkFrame("hello", "")+chunkFrame("", "stop"), `,"created"`, ",\ndata: \"created\"") + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	out := &proxyOutput{w: w, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(body), out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("multiline event lost")
	}
}

func TestLlamaStreamAllowsPerEventTimestamps(t *testing.T) {
	body := chunkFrame("hello", "") + strings.Replace(chunkFrame(" world", ""), `"created":1`, `"created":2`, 1) + strings.Replace(chunkFrame("", "stop"), `"created":1`, `"created":3`, 1) + "data: [DONE]\n\n"
	w := httptest.NewRecorder()
	out := &proxyOutput{w: w, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(body), out, nil); err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != body {
		t.Fatal("backend timestamps or event ordering changed")
	}
}
