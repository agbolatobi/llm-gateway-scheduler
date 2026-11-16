package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway-scheduler/gateway/internal/scheduler"
)

func queuedRequest(ctx context.Context, handler *RegisteredHandler, id, class string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body := fmt.Sprintf(`{"model":"default","messages":[{"role":"user","content":"hello"}],"max_tokens":8,"request_class":%q}`, class)
		request := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(body)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer alice-one")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Request-ID", id)
		responseRecorder := httptest.NewRecorder()
		handler.ServeHTTP(responseRecorder, request)
		done <- responseRecorder
	}()
	return done
}
func awaitWaiting(testContext *testing.T, handler *RegisteredHandler, count int) {
	testContext.Helper()
	end := time.Now().Add(2 * time.Second)
	for handler.scheduler.Waiting() != count && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if handler.scheduler.Waiting() != count {
		testContext.Fatalf("waiting=%d want=%d", handler.scheduler.Waiting(), count)
	}
}
func responseFrom(testContext *testing.T, ch <-chan *httptest.ResponseRecorder, code int) *httptest.ResponseRecorder {
	testContext.Helper()
	select {
	case responseRecorder := <-ch:
		if responseRecorder.Code != code {
			testContext.Fatalf("status=%d want=%d body=%s", responseRecorder.Code, code, responseRecorder.Body)
		}
		return responseRecorder
	case <-time.After(4 * time.Second):
		testContext.Fatal("request did not finish")
		return nil
	}
}

func TestQueueHTTPOrderingOverflowCancellationAndRefund(testContext *testing.T) {
	for _, policy := range []string{scheduler.FIFO, scheduler.Priority} {
		testContext.Run(policy, func(testContext *testing.T) {
			var busy atomic.Bool
			entered := make(chan string, 8)
			unblock := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(unblock) })
			config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) {
				busy.Store(true)
				defer busy.Store(false)
				id := request.Header.Get("X-Request-ID")
				entered <- id
				if id == "hold" {
					<-unblock
				}
				responseWriter.Header().Set("Content-Type", "application/json")
				io.WriteString(responseWriter, withUsage(completion("hello"), validUsage))
			})
			settings := scheduler.Default()
			settings.Policy = policy
			settings.MaxWaiting = 2
			settings.MaxPerRuntime = 2
			config.Gateway.Scheduler = &settings
			handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
			first := queuedRequest(context.Background(), handler, "hold", "interactive")
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				testContext.Fatal("backend not reached")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancelled := queuedRequest(ctx, handler, "cancelled", "background")
			awaitWaiting(testContext, handler, 1)
			background := queuedRequest(context.Background(), handler, "background", "background")
			awaitWaiting(testContext, handler, 2)
			overflow := responseFrom(testContext, queuedRequest(context.Background(), handler, "overflow", "interactive"), 503)
			if !strings.Contains(overflow.Body.String(), "queue_full") {
				testContext.Fatal(overflow.Body)
			}
			cancel()
			responseFrom(testContext, cancelled, 200) // Disconnected client receives no new response.
			awaitWaiting(testContext, handler, 1)
			interactive := queuedRequest(context.Background(), handler, "interactive", "interactive")
			awaitWaiting(testContext, handler, 2)
			once.Do(func() { close(unblock) })
			responseFrom(testContext, first, 200)
			responseFrom(testContext, background, 200)
			responseFrom(testContext, interactive, 200)
			order := []string{<-entered, <-entered}
			want := []string{"background", "interactive"}
			if policy == scheduler.Priority {
				want = []string{"interactive", "background"}
			}
			if fmt.Sprint(order) != fmt.Sprint(want) {
				testContext.Fatal(order, want)
			}
			seen := map[string]TimingRecord{}
			for index := 0; index < 5; index++ {
				timingRecord := getReport(testContext, reports)
				seen[timingRecord.RequestID] = timingRecord
			}
			for _, id := range []string{"cancelled", "overflow"} {
				if seen[id].ChargedTokens != 0 || seen[id].DispatchMS != nil {
					testContext.Fatalf("unused reservation leaked: %+v", seen[id])
				}
			}
			for _, id := range []string{"background", "interactive"} {
				request := seen[id]
				if request.ChargedTokens != 5 || request.QueueWaitMS <= 0 || request.SchedulerPolicy != policy {
					testContext.Fatal(request)
				}
			}
			if seen[want[0]].SelectionSequence == 0 || seen[want[0]].SelectionSequence >= seen[want[1]].SelectionSequence {
				testContext.Fatal("selection sequence does not explain policy order", seen)
			}
			if handler.scheduler.Waiting() != 0 {
				testContext.Fatal("waiting leak")
			}
		})
	}
}

func TestQueueExpiryRefreshesWriteDeadlineAndRefunds(testContext *testing.T) {
	var busy atomic.Bool
	config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
		testContext.Error("expired queued request reached backend")
	})
	settings := scheduler.Default()
	settings.MaxWaitSeconds = 1
	config.Gateway.Scheduler = &settings
	options := DefaultProxyOptions()
	options.WriteTimeout = 50 * time.Millisecond
	handler, reports := lifecycleHandler(testContext, config, options)
	lease, _ := handler.capacity.Acquire(config.Models[0].Backend.BaseURL)
	defer lease.Release()
	server := httptest.NewServer(handler)
	defer server.Close()
	request := proxyRequest("local-llm", false)
	request.URL.Scheme = "http"
	request.URL.Host = strings.TrimPrefix(server.URL, "http://")
	request.RequestURI = ""
	client := &http.Client{Timeout: 3 * time.Second}
	reply, err := client.Do(request)
	if err != nil {
		testContext.Fatal(err)
	}
	defer reply.Body.Close()
	body, err := io.ReadAll(reply.Body)
	if err != nil || reply.StatusCode != 504 || !strings.Contains(string(body), "queue_timeout") {
		testContext.Fatal(reply.StatusCode, string(body), err)
	}
	timingRecord := getReport(testContext, reports)
	if timingRecord.ChargedTokens != 0 || timingRecord.DispatchMS != nil || timingRecord.QueueWaitMS < 900 {
		testContext.Fatal(timingRecord)
	}
}

func TestQueueRoutePinAndShutdown(testContext *testing.T) {
	var busy atomic.Bool
	config := lifecycleBackend(testContext, &busy, func(responseWriter http.ResponseWriter, request *http.Request) {})
	settings := scheduler.Default()
	config.Gateway.Scheduler = &settings
	handler, reports := lifecycleHandler(testContext, config, DefaultProxyOptions())
	lease, _ := handler.capacity.Acquire(config.Models[0].Backend.BaseURL)
	defer lease.Release()
	waiting := queuedRequest(context.Background(), handler, "waiting", "background")
	awaitWaiting(testContext, handler, 1)
	// Release the capacity lease without waking: the admitted route reference
	// alone must protect removal/re-registration, even before dispatch.
	lease.Release()
	replacement := config.Clone()
	replacement.Models[0].ID = "replacement"
	if err := handler.Reload(replacement); err == nil {
		testContext.Fatal("queued route was removed")
	}
	handler.StopWaiting()
	responseRecorder := responseFrom(testContext, waiting, 503)
	if !strings.Contains(responseRecorder.Body.String(), "scheduler_closed") {
		testContext.Fatal(responseRecorder.Body)
	}
	timingRecord := getReport(testContext, reports)
	if timingRecord.ChargedTokens != 0 {
		testContext.Fatal(timingRecord)
	}
	if err := handler.Reload(replacement); err != nil {
		testContext.Fatal("route still pinned", err)
	}
}
