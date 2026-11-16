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

func routeAt(testContext *testing.T, address string) Route {
	testContext.Helper()
	model := registry.Default().Models[0]
	model.Backend.BaseURL, model.Backend.ExpectedModel = address, "backend-name"
	route, err := Select(model)
	if err != nil {
		testContext.Fatal(err)
	}
	return route
}
func sampleRequest() Request {
	return Request{Messages: json.RawMessage(`[{"role":"user","content":"hello"}]`), OutputTokens: 64}
}
func testClient(testContext *testing.T) *Client {
	testContext.Helper()
	client := NewClient()
	testContext.Cleanup(client.CloseIdleConnections)
	return client
}

func TestSendWirePayload(testContext *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
		if httpRequest.Method != "POST" || httpRequest.URL.Path != ChatCompletionsPath || httpRequest.Header.Get("Content-Type") != "application/json" {
			testContext.Error("wrong backend request")
		}
		var body map[string]any
		if err := json.NewDecoder(httpRequest.Body).Decode(&body); err != nil {
			testContext.Error(err)
		}
		if body["model"] != "backend-name" || body["max_tokens"] != float64(64) || body["temperature"] != float64(0) || body["stream"] != true {
			testContext.Error(body)
		}
		if _, ok := body["request_class"]; ok {
			testContext.Error("gateway extension forwarded")
		}
		if _, ok := body["max_completion_tokens"]; ok {
			testContext.Error("duplicate output cap")
		}
		if body["stream_options"].(map[string]any)["include_usage"] != true {
			testContext.Error(body)
		}
		responseWriter.Write([]byte(`{"choices":[]}`))
	}))
	defer backend.Close()
	request := sampleRequest()
	temperature := 0.0
	request.Temperature, request.Stream, request.IncludeUsage = &temperature, true, true
	client := testClient(testContext)
	if client.http.Transport.(*http.Transport).Proxy != nil || client.http.Timeout != 0 {
		testContext.Fatal("proxy or inspection timeout enabled")
	}
	attempt, err := client.Send(context.Background(), routeAt(testContext, backend.URL), request)
	if err != nil || !attempt.MayHaveRun || attempt.Response == nil {
		testContext.Fatal(attempt, err)
	}
	defer attempt.Response.Body.Close()
	body, err := io.ReadAll(attempt.Response.Body)
	if err != nil || !strings.Contains(string(body), "choices") {
		testContext.Fatal(string(body), err)
	}
}

func TestCancelledBeforeDispatchAndMissingRoute(testContext *testing.T) {
	client := testClient(testContext)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempt, err := client.Send(ctx, routeAt(testContext, "http://127.0.0.1:8080"), sampleRequest())
	if !errors.Is(err, context.Canceled) || attempt.MayHaveRun {
		testContext.Fatal(attempt, err)
	}
	attempt, err = client.Send(context.Background(), Route{}, sampleRequest())
	if err == nil || attempt.MayHaveRun {
		testContext.Fatal(attempt, err)
	}
}

func TestConnectionRefusedIsUnused(testContext *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		testContext.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	attempt, err := testClient(testContext).Send(context.Background(), routeAt(testContext, address), sampleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 503 || attempt.MayHaveRun {
		testContext.Fatal(attempt, err)
	}
}

func TestRedirectAndFailureAreNotRetried(testContext *testing.T) {
	for _, status := range []int{302, 429, 500, 503} {
		testContext.Run(http.StatusText(status), func(testContext *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, request.Body)
				responseWriter.Header().Set("Location", "/redirect-target")
				responseWriter.WriteHeader(status)
				responseWriter.Write([]byte("private backend diagnostic"))
			}))
			defer backend.Close()
			attempt, err := testClient(testContext).Send(context.Background(), routeAt(testContext, backend.URL), sampleRequest())
			var failure *Error
			if !errors.As(err, &failure) || failure.Status != 502 || !attempt.MayHaveRun || attempt.Response != nil || calls.Load() != 1 {
				testContext.Fatal(attempt, err, calls.Load())
			}
			if strings.Contains(err.Error(), "private") {
				testContext.Fatal("backend diagnostic leaked")
			}
		})
	}
}

func TestDroppedReusedConnectionDoesNotReplay(testContext *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		if calls.Add(1) == 1 {
			responseWriter.Write([]byte("ok"))
			return
		}
		conn, _, err := responseWriter.(http.Hijacker).Hijack()
		if err != nil {
			testContext.Error(err)
			return
		}
		conn.Close()
	}))
	defer backend.Close()
	client, route := testClient(testContext), routeAt(testContext, backend.URL)
	first, err := client.Send(context.Background(), route, sampleRequest())
	if err != nil {
		testContext.Fatal(err)
	}
	io.Copy(io.Discard, first.Response.Body)
	first.Response.Body.Close()
	second, err := client.Send(context.Background(), route, sampleRequest())
	if err == nil || !second.MayHaveRun || calls.Load() != 2 {
		testContext.Fatal("generation replayed", calls.Load(), err)
	}
}

func TestCancellationWhileReadingBody(testContext *testing.T) {
	cancelled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		responseWriter.WriteHeader(200)
		responseWriter.(http.Flusher).Flush()
		<-request.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempt, err := testClient(testContext).Send(ctx, routeAt(testContext, backend.URL), sampleRequest())
	if err != nil {
		testContext.Fatal(err)
	}
	defer attempt.Response.Body.Close()
	cancel()
	if _, err := io.ReadAll(attempt.Response.Body); err == nil {
		testContext.Fatal("body read ignored cancellation")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		testContext.Fatal("backend did not see cancellation")
	}
}

func TestResponseHeaderTimeout(testContext *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		<-request.Context().Done()
	}))
	defer backend.Close()
	client := testClient(testContext)
	client.http.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
	attempt, err := client.Send(context.Background(), routeAt(testContext, backend.URL), sampleRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 504 || !attempt.MayHaveRun {
		testContext.Fatal(attempt, err)
	}
}
