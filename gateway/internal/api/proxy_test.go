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

func liveProxy(testContext *testing.T, config registry.Config, options ProxyOptions) (*RegisteredHandler, <-chan TimingRecord) {
	testContext.Helper()
	handler, err := NewRegisteredHandlerWithOptions(userVerifier{}, config, nil, options)
	if err != nil {
		testContext.Fatal(err)
	}
	reports := make(chan TimingRecord, 16)
	handler.report = func(request TimingRecord) { reports <- request }
	testContext.Cleanup(handler.Close)
	return handler, reports
}

func proxyRequest(model string, stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, model, stream)
	request := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer alice-one")
	request.Header.Set("X-Request-ID", "test-request-1")
	return request
}

func completion(text string) string {
	encodedResponse, _ := json.Marshal(map[string]any{"id": "chat-1", "object": "chat.completion", "created": 1, "model": "local-llm", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	return string(encodedResponse)
}

func chunkFrame(content, finish string) string {
	var reason any
	if finish != "" {
		reason = finish
	}
	encodedResponse, _ := json.Marshal(map[string]any{"id": "chat-1", "object": "chat.completion.chunk", "created": 1, "model": "local-llm", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": content}, "finish_reason": reason}}})
	return "data: " + string(encodedResponse) + "\n\n"
}

func getReport(testContext *testing.T, reports <-chan TimingRecord) TimingRecord {
	testContext.Helper()
	select {
	case timingRecord := <-reports:
		return timingRecord
	case <-time.After(3 * time.Second):
		testContext.Fatal("missing timing record")
		return TimingRecord{}
	}
}

func TestLiveProxyNormalAndRequestIdentity(testContext *testing.T) {
	config := fakeRoutes(testContext, func(index int, responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Request-ID") != "test-request-1" || request.Header.Get("Authorization") != "" {
			testContext.Error("incorrect forwarded headers")
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		io.WriteString(responseWriter, completion(fmt.Sprint(index)))
	})
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	for index, model := range []string{"default", "engineer"} {
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, proxyRequest(model, false))
		if responseRecorder.Code != 200 || responseRecorder.Body.String() != completion(fmt.Sprint(index)) || responseRecorder.Header().Get("X-Request-ID") != "test-request-1" {
			testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
		}
		timingRecord := getReport(testContext, reports)
		if timingRecord.Outcome != "completed" || timingRecord.DispatchMS == nil || timingRecord.FirstTextMS == nil || timingRecord.CompletionMS == nil || *timingRecord.DispatchMS > *timingRecord.FirstTextMS || *timingRecord.FirstTextMS > *timingRecord.CompletionMS || *timingRecord.CompletionMS > timingRecord.EndedMS {
			testContext.Fatalf("bad timing: %+v", timingRecord)
		}
	}
}

func TestLiveProxyStreamsBeforeBackendCompletes(testContext *testing.T) {
	release := make(chan struct{})
	config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(responseWriter, chunkFrame("hello", ""))
		responseWriter.(http.Flusher).Flush()
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		io.WriteString(responseWriter, chunkFrame("", "stop")+"data: [DONE]\n\n")
	})
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	testServer := httptest.NewServer(handler)
	defer testServer.Close()
	request := proxyRequest("local-llm", true)
	request.URL.Scheme = "http"
	request.URL.Host = strings.TrimPrefix(testServer.URL, "http://")
	request.RequestURI = ""
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		testContext.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "hello") {
		testContext.Fatal(line, err)
	}
	select {
	case <-reports:
		testContext.Fatal("request ended before backend completed")
	default:
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "[DONE]") || !strings.Contains(string(rest), `"finish_reason":"stop"`) {
		testContext.Fatal(string(rest), err)
	}
	if getReport(testContext, reports).Outcome != "completed" {
		testContext.Fatal("not completed")
	}
}

func TestLiveProxyBrokenStreams(testContext *testing.T) {
	for _, testCase := range []struct{ name, tail string }{
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
		testContext.Run(testCase.name, func(testContext *testing.T) {
			config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(responseWriter, chunkFrame("hello", "")+testCase.tail)
			})
			handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
			responseRecorder := httptest.NewRecorder()
			handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", true))
			body := responseRecorder.Body.String()
			if !strings.Contains(body, "hello") || !strings.Contains(body, "event: error") || strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "private secret") {
				testContext.Fatal(body)
			}
			timingRecord := getReport(testContext, reports)
			if timingRecord.CompletionMS != nil || timingRecord.Outcome == "completed" {
				testContext.Fatalf("false completion: %+v", timingRecord)
			}
		})
	}
}

func TestLiveProxyRejectsBadNormalResponses(testContext *testing.T) {
	for _, testCase := range []struct {
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
		testContext.Run(testCase.name, func(testContext *testing.T) {
			config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", testCase.contentType)
				responseWriter.WriteHeader(testCase.status)
				io.WriteString(responseWriter, testCase.body)
			})
			handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
			responseRecorder := httptest.NewRecorder()
			handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", false))
			if responseRecorder.Code != 502 || !json.Valid(responseRecorder.Body.Bytes()) || strings.Contains(responseRecorder.Body.String(), "private secret") {
				testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
			}
			if getReport(testContext, reports).CompletionMS != nil {
				testContext.Fatal("false completion")
			}
		})
	}
}

func TestLiveProxyTimeoutsCancelBackend(testContext *testing.T) {
	for _, testCase := range []struct {
		name        string
		stream      bool
		idle, total time.Duration
		code        string
	}{
		{"idle_before_output", false, 60 * time.Millisecond, 2 * time.Second, "backend_idle_timeout"},
		{"idle_after_output", true, 60 * time.Millisecond, 2 * time.Second, "backend_idle_timeout"},
		{"overall_after_output", true, 2 * time.Second, 150 * time.Millisecond, "request_timeout"},
	} {
		testContext.Run(testCase.name, func(testContext *testing.T) {
			cancelled := make(chan struct{}, 1)
			config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
				if testCase.stream {
					responseWriter.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(responseWriter, chunkFrame("hello", ""))
				} else {
					responseWriter.Header().Set("Content-Type", "application/json")
					responseWriter.WriteHeader(200)
				}
				responseWriter.(http.Flusher).Flush()
				<-request.Context().Done()
				cancelled <- struct{}{}
			})
			opts := DefaultProxyOptions()
			opts.IdleTimeout = testCase.idle
			opts.RequestTimeout = testCase.total
			handler, reports := liveProxy(testContext, config, opts)
			responseRecorder := httptest.NewRecorder()
			handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", testCase.stream))
			if !strings.Contains(responseRecorder.Body.String(), testCase.code) || strings.Contains(responseRecorder.Body.String(), "[DONE]") {
				testContext.Fatal(responseRecorder.Code, responseRecorder.Body)
			}
			if !testCase.stream && responseRecorder.Code != 504 {
				testContext.Fatal(responseRecorder.Code)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				testContext.Fatal("backend not cancelled")
			}
			timingRecord := getReport(testContext, reports)
			if timingRecord.Outcome != testCase.code || timingRecord.CompletionMS != nil {
				testContext.Fatalf("bad timeout timing: %+v", timingRecord)
			}
		})
	}
}

func TestLiveProxyClientCancellation(testContext *testing.T) {
	cancelled := make(chan struct{}, 1)
	config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(responseWriter, chunkFrame("hello", ""))
		responseWriter.(http.Flusher).Flush()
		<-request.Context().Done()
		cancelled <- struct{}{}
	})
	handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
	testServer := httptest.NewServer(handler)
	defer testServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := proxyRequest("local-llm", true).WithContext(ctx)
	request.URL.Scheme = "http"
	request.URL.Host = strings.TrimPrefix(testServer.URL, "http://")
	request.RequestURI = ""
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		testContext.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		testContext.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		testContext.Fatal("backend continued after client disconnected")
	}
	rpt := getReport(testContext, reports)
	if rpt.Outcome != "request_cancelled" || rpt.CompletionMS != nil {
		testContext.Fatalf("bad cancellation: %+v", rpt)
	}
}

func TestLiveProxyLargeNormalReplyIsIncremental(testContext *testing.T) {
	for _, valid := range []bool{true, false} {
		testContext.Run(fmt.Sprint(valid), func(testContext *testing.T) {
			release := make(chan struct{})
			body := completion(strings.Repeat("x", 100<<10))
			cut := len(body) - 100
			config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				io.WriteString(responseWriter, body[:cut])
				responseWriter.(http.Flusher).Flush()
				select {
				case <-release:
				case <-request.Context().Done():
					return
				}
				if valid {
					io.WriteString(responseWriter, body[cut:])
				}
			})
			handler, reports := liveProxy(testContext, config, DefaultProxyOptions())
			testServer := httptest.NewServer(handler)
			defer testServer.Close()
			request := proxyRequest("local-llm", false)
			request.URL.Scheme = "http"
			request.URL.Host = strings.TrimPrefix(testServer.URL, "http://")
			request.RequestURI = ""
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
			if err != nil {
				testContext.Fatal(err)
			}
			defer resp.Body.Close()
			first := make([]byte, 100)
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				testContext.Fatal(err)
			}
			close(release)
			rest, err := io.ReadAll(resp.Body)
			if valid {
				if err != nil || string(append(first, rest...)) != body {
					testContext.Fatal("normal reply changed", err)
				}
			} else if err == nil {
				testContext.Fatal("truncated reply got a clean HTTP completion")
			}
			rpt := getReport(testContext, reports)
			if valid != (rpt.CompletionMS != nil) {
				testContext.Fatalf("bad completion timing: %+v", rpt)
			}
		})
	}
}

func TestStreamFramingAndTiming(testContext *testing.T) {
	usage := `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"local-llm","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\n"
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		body := ": heartbeat\n\n" + chunkFrame("", "") + chunkFrame("hello", "") + chunkFrame("", "stop") + usage + "data: [DONE]\n\n"
		body = strings.ReplaceAll(body, "\n", newline)
		responseRecorder := httptest.NewRecorder()
		trace, _ := newRequestTrace("")
		out := &proxyOutput{responseWriter: responseRecorder, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
		if err := relaySSE(strings.NewReader(body), out, trace); err != nil {
			testContext.Fatal(err)
		}
		if trace.firstText.IsZero() || !strings.Contains(responseRecorder.Body.String(), "[DONE]") {
			testContext.Fatal("missing text/completion")
		}
	}
	responseRecorder := httptest.NewRecorder()
	trace, _ := newRequestTrace("")
	out := &proxyOutput{responseWriter: responseRecorder, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(chunkFrame("", "")+chunkFrame("", "stop")+"data: [DONE]\n\n"), out, trace); err != nil {
		testContext.Fatal(err)
	}
	if !trace.firstText.IsZero() {
		testContext.Fatal("metadata incorrectly counted as generated text")
	}
}

func TestProxyOptionsAndRequestIDs(testContext *testing.T) {
	for _, options := range []ProxyOptions{{}, {time.Hour, time.Second, time.Second}, {time.Second, 0, time.Second}, {time.Second, time.Second, 2 * time.Minute}} {
		if options.Validate() == nil {
			testContext.Fatal("accepted invalid limits", options)
		}
	}
	for _, id := range []string{"", "bad id", strings.Repeat("x", 65)} {
		trace, err := newRequestTrace(id)
		if err != nil || len(trace.id) != 32 || trace.id == id {
			testContext.Fatal("bad generated ID")
		}
	}
	trace, _ := newRequestTrace("safe-request_1.2")
	if trace.id != "safe-request_1.2" {
		testContext.Fatal("did not preserve safe ID")
	}
}

// A deterministic socket stand-in verifies that each write receives a deadline
// and that a failed flush cancels upstream work without a synthetic completion.
type stalledWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (writer *stalledWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	return nil
}
func (writer *stalledWriter) FlushError() error { return context.DeadlineExceeded }

func TestLiveProxyFailedClientWriteCancelsBackend(testContext *testing.T) {
	cancelled := make(chan struct{}, 1)
	config := fakeRoutes(testContext, func(_ int, responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(responseWriter, chunkFrame("hello", ""))
		responseWriter.(http.Flusher).Flush()
		<-request.Context().Done()
		cancelled <- struct{}{}
	})
	opts := DefaultProxyOptions()
	opts.WriteTimeout = 50 * time.Millisecond
	handler, reports := liveProxy(testContext, config, opts)
	responseRecorder := &stalledWriter{ResponseRecorder: httptest.NewRecorder()}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				testContext.Errorf("expected HTTP abort, got %v", got)
			}
		}()
		handler.ServeHTTP(responseRecorder, proxyRequest("local-llm", true))
	}()
	if responseRecorder.deadline.IsZero() || time.Until(responseRecorder.deadline) > opts.WriteTimeout {
		testContext.Fatal("missing write deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		testContext.Fatal("backend not cancelled")
	}
	timingRecord := getReport(testContext, reports)
	if timingRecord.Outcome != "client_write_failed" || timingRecord.CompletionMS != nil || strings.Contains(responseRecorder.Body.String(), "[DONE]") {
		testContext.Fatal(timingRecord, responseRecorder.Body)
	}
}

func TestBoundedResponseReaders(testContext *testing.T) {
	bodyReader := &limitedReader{reader: strings.NewReader("12345"), remaining: 4}
	responseBody, err := io.ReadAll(bodyReader)
	if string(responseBody) != "1234" || !errors.Is(err, errLimit) {
		testContext.Fatal(string(responseBody), err)
	}
	responseRecorder := httptest.NewRecorder()
	out := &proxyOutput{responseWriter: responseRecorder, ctx: context.Background(), timeout: time.Second, contentType: "application/json"}
	err = relayJSON(&limitedReader{reader: strings.NewReader(completion(strings.Repeat("x", maxJSONBytes))), remaining: maxJSONBytes}, out, nil)
	if !errors.Is(err, errLimit) || json.Valid(responseRecorder.Body.Bytes()) {
		testContext.Fatal("oversized normal response looked complete", err)
	}
}

func TestMultilineSSEData(testContext *testing.T) {
	body := strings.ReplaceAll(chunkFrame("hello", "")+chunkFrame("", "stop"), `,"created"`, ",\ndata: \"created\"") + "data: [DONE]\n\n"
	responseRecorder := httptest.NewRecorder()
	out := &proxyOutput{responseWriter: responseRecorder, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(body), out, nil); err != nil {
		testContext.Fatal(err)
	}
	if !strings.Contains(responseRecorder.Body.String(), "[DONE]") {
		testContext.Fatal("multiline event lost")
	}
}

func TestLlamaStreamAllowsPerEventTimestamps(testContext *testing.T) {
	body := chunkFrame("hello", "") + strings.Replace(chunkFrame(" world", ""), `"created":1`, `"created":2`, 1) + strings.Replace(chunkFrame("", "stop"), `"created":1`, `"created":3`, 1) + "data: [DONE]\n\n"
	responseRecorder := httptest.NewRecorder()
	out := &proxyOutput{responseWriter: responseRecorder, ctx: context.Background(), timeout: time.Second, contentType: "text/event-stream"}
	if err := relaySSE(strings.NewReader(body), out, nil); err != nil {
		testContext.Fatal(err)
	}
	if responseRecorder.Body.String() != body {
		testContext.Fatal("backend timestamps or event ordering changed")
	}
}
