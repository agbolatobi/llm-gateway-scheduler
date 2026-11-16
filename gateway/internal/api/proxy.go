package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
	"llm-gateway-scheduler/gateway/internal/scheduler"
)

const maxJSONBytes = 4 << 20
const maxStreamBytes = 16 << 20
const maxEventBytes = 256 << 10

var errIdle = errors.New("backend idle timeout")
var errProtocol = errors.New("invalid backend response")
var errLimit = errors.New("backend response limit")
var errClientWrite = errors.New("client write failed")

type ProxyOptions struct {
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
	WriteTimeout   time.Duration
}

func DefaultProxyOptions() ProxyOptions {
	return ProxyOptions{2 * time.Minute, 30 * time.Second, 10 * time.Second}
}
func (options ProxyOptions) Validate() error {
	if options.RequestTimeout <= 0 || options.RequestTimeout > 30*time.Minute || options.IdleTimeout <= 0 || options.IdleTimeout > 5*time.Minute || options.WriteTimeout <= 0 || options.WriteTimeout > time.Minute {
		return fmt.Errorf("request timeout must be in (0,30m], backend idle timeout in (0,5m], client write timeout in (0,1m]")
	}
	return nil
}

func (handler *RegisteredHandler) forwardBackendResponse(responseWriter http.ResponseWriter, request *http.Request, prepared *preparedRequest) {
	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	trace := requestTraceFromContext(ctx)
	base, _ := registry.LocalURL(prepared.model.Backend.BaseURL)
	prepared.idle = func(ctx context.Context) (bool, error) { return handler.execution.Idle(ctx, prepared.model) }
	handler.capacity.Recover(ctx, base, prepared.idle)

	class := "interactive"
	if trace != nil {
		class = trace.class
	}
	// Queue deadlines are independent of socket write deadlines. Keep the
	// receipt-to-completion context deadline while waiting, then reset writes.
	http.NewResponseController(responseWriter).SetWriteDeadline(time.Time{})
	result := handler.scheduler.Wait(ctx, base, class)
	http.NewResponseController(responseWriter).SetWriteDeadline(time.Now().Add(handler.options.WriteTimeout))
	if trace != nil {
		trace.queueWait = result.Wait
		trace.schedulerPolicy = result.Policy
		trace.dispatchReason = result.Reason
		trace.selectionSequence = result.SelectionSequence
	}
	if result.Err != nil {
		if errors.Is(result.Err, context.Canceled) {
			if trace != nil {
				trace.outcome = "request_cancelled"
			}
			return
		}
		code, status := result.Err.Error(), 503
		if errors.Is(result.Err, scheduler.ErrTimeout) {
			status = 504
		}
		if errors.Is(result.Err, context.DeadlineExceeded) {
			status, code = 504, "request_timeout"
		}
		if trace != nil {
			trace.outcome = code
		}
		if status == 503 {
			responseWriter.Header().Set("Retry-After", "1")
		}
		writeError(responseWriter, status, code, "The request could not obtain execution capacity within the queue limits.", "")
		return
	}
	prepared.lease = result.Lease
	if ctx.Err() != nil {
		return
	}

	idle, probeErr := prepared.idle(ctx)
	if probeErr != nil || !idle {
		code := "capacity_exceeded"
		if probeErr != nil {
			code = "capacity_unavailable"
		}
		if trace != nil {
			trace.outcome = code
		}
		responseWriter.Header().Set("Retry-After", "1")
		writeError(responseWriter, 503, code, "The backend execution slot is busy or could not be verified.", "")
		return
	}
	// Collect final usage even when the caller does not want a usage-only event.
	if prepared.payload.Stream {
		prepared.payload.IncludeUsage = true
	}
	if trace != nil {
		trace.dispatch = time.Now()
		trace.outcome = "backend_error"
		prepared.payload.RequestID = trace.id
	}
	response, err := prepared.sendToBackend(ctx, handler.client)
	output := &proxyOutput{responseWriter: responseWriter, ctx: ctx, timeout: handler.options.WriteTimeout, contentType: "application/json"}
	if err == nil {
		contentType, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		expected := "application/json"
		if prepared.payload.Stream {
			expected = "text/event-stream"
		}
		if response.StatusCode != 200 || parseErr != nil || contentType != expected || (params["charset"] != "" && params["charset"] != "utf-8" && params["charset"] != "UTF-8") || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
			err = errProtocol
		} else {
			output.contentType = expected
			reader := idleReader{reader: response.Body, timeout: handler.options.IdleTimeout, cancel: cancel}
			if prepared.payload.Stream {
				err = relaySSE(&limitedReader{reader: reader, remaining: maxStreamBytes}, output, trace, &prepared.facts)
			} else {
				err = relayJSON(&limitedReader{reader: reader, remaining: maxJSONBytes}, output, trace, &prepared.facts)
			}
		}
	}
	if err == nil {
		if trace != nil {
			trace.complete = time.Now()
			trace.outcome = "completed"
		}
		return
	}
	status, code, message := proxyFailure(ctx, err)
	if trace != nil {
		trace.outcome = code
	}
	cancel(err) // Stop backend work promptly before any error delivery to the client.
	if errors.Is(request.Context().Err(), context.Canceled) || errors.Is(err, errClientWrite) {
		if output.started {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if !output.started {
		http.NewResponseController(responseWriter).SetWriteDeadline(time.Now().Add(min(handler.options.WriteTimeout, time.Second)))
		writeError(responseWriter, status, code, message, "")
		return
	}
	if !prepared.payload.Stream {
		panic(http.ErrAbortHandler)
	} // No second JSON document or clean EOF on a broken reply.
	frame, _ := json.Marshal(ErrorResponse{Error: APIError{Type: "server_error", Code: code, Message: message}})
	if _, err := output.write(append(append([]byte("event: error\ndata: "), frame...), []byte("\n\n")...), true); err != nil {
		panic(http.ErrAbortHandler)
	}
	// Deliberately no finish_reason or [DONE] on failure.
}

func proxyFailure(ctx context.Context, err error) (int, string, string) {
	switch {
	case errors.Is(err, errClientWrite):
		return 502, "client_write_failed", "The client could not receive the response."
	case errors.Is(context.Cause(ctx), errIdle):
		return 504, "backend_idle_timeout", "The backend stopped sending data."
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return 504, "request_timeout", "The request exceeded its time limit."
	case errors.Is(ctx.Err(), context.Canceled):
		return 499, "request_cancelled", "The request was cancelled."
	case errors.Is(err, errLimit):
		return 502, "backend_response_too_large", "The backend response exceeded a safety limit."
	}
	var failure *routing.Error
	if errors.As(err, &failure) {
		return failure.Status, failure.Code, failure.Message
	}
	return 502, "backend_protocol_error", "The backend response was invalid or incomplete."
}
