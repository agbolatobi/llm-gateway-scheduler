package routing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
)

func routeAt(t *testing.T, address string) Route {
	t.Helper()
	m := registry.Default().Models[0]
	m.Backend.BaseURL, m.Backend.ExpectedModel = address, "backend-name"
	route, err := Select(m)
	if err != nil {
		t.Fatal(err)
	}
	return route
}
func sampleRequest() Request {
	return Request{Messages: json.RawMessage(`[{"role":"user","content":"hello"}]`), OutputTokens: 64}
}
func testClient(t *testing.T) *Client {
	t.Helper()
	c := NewClient()
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestSendWirePayload(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != ChatCompletionsPath || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong backend request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "backend-name" || body["max_tokens"] != float64(64) || body["temperature"] != float64(0) || body["stream"] != true {
			t.Error(body)
		}
		if _, ok := body["request_class"]; ok {
			t.Error("gateway extension forwarded")
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Error("duplicate output cap")
		}
		if body["stream_options"].(map[string]any)["include_usage"] != true {
			t.Error(body)
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer backend.Close()
	request := sampleRequest()
	temperature := 0.0
	request.Temperature, request.Stream, request.IncludeUsage = &temperature, true, true
	client := testClient(t)
	if client.http.Transport.(*http.Transport).Proxy != nil || client.http.Timeout != 0 {
		t.Fatal("proxy or inspection timeout enabled")
	}
	attempt, err := client.Send(context.Background(), routeAt(t, backend.URL), request)
	if err != nil || !attempt.MayHaveRun || attempt.Response == nil {
		t.Fatal(attempt, err)
	}
	defer attempt.Response.Body.Close()
	body, err := io.ReadAll(attempt.Response.Body)
	if err != nil || !strings.Contains(string(body), "choices") {
		t.Fatal(string(body), err)
	}
}

func TestCancelledBeforeDispatchAndMissingRoute(t *testing.T) {
	client := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempt, err := client.Send(ctx, routeAt(t, "http://127.0.0.1:8080"), sampleRequest())
	if !errors.Is(err, context.Canceled) || attempt.MayHaveRun {
		t.Fatal(attempt, err)
	}
	attempt, err = client.Send(context.Background(), Route{}, sampleRequest())
	if err == nil || attempt.MayHaveRun {
		t.Fatal(attempt, err)
	}
}

func TestConnectionRefusedIsUnused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	attempt, err := testClient(t).Send(context.Background(), routeAt(t, address), sampleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 503 || attempt.MayHaveRun {
		t.Fatal(attempt, err)
	}
}

func TestRedirectAndFailureAreNotRetried(t *testing.T) {
	for _, status := range []int{302, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(status)
				w.Write([]byte("private backend diagnostic"))
			}))
			defer backend.Close()
			attempt, err := testClient(t).Send(context.Background(), routeAt(t, backend.URL), sampleRequest())
			var failure *Error
			if !errors.As(err, &failure) || failure.Status != 502 || !attempt.MayHaveRun || attempt.Response != nil || calls.Load() != 1 {
				t.Fatal(attempt, err, calls.Load())
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("backend diagnostic leaked")
			}
		})
	}
}

func TestDroppedReusedConnectionDoesNotReplay(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Write([]byte("ok"))
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer backend.Close()
	client, route := testClient(t), routeAt(t, backend.URL)
	first, err := client.Send(context.Background(), route, sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, first.Response.Body)
	first.Response.Body.Close()
	second, err := client.Send(context.Background(), route, sampleRequest())
	if err == nil || !second.MayHaveRun || calls.Load() != 2 {
		t.Fatal("generation replayed", calls.Load(), err)
	}
}

func TestCancellationWhileReadingBody(t *testing.T) {
	cancelled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempt, err := testClient(t).Send(ctx, routeAt(t, backend.URL), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Response.Body.Close()
	cancel()
	if _, err := io.ReadAll(attempt.Response.Body); err == nil {
		t.Fatal("body read ignored cancellation")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not see cancellation")
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer backend.Close()
	client := testClient(t)
	client.http.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
	attempt, err := client.Send(context.Background(), routeAt(t, backend.URL), sampleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 504 || !attempt.MayHaveRun {
		t.Fatal(attempt, err)
	}
}
