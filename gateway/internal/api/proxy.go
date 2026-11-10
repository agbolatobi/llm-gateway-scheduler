package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"llm-gateway-scheduler/gateway/internal/registry"
	"llm-gateway-scheduler/gateway/internal/routing"
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
func (o ProxyOptions) Validate() error {
	if o.RequestTimeout <= 0 || o.RequestTimeout > 30*time.Minute || o.IdleTimeout <= 0 || o.IdleTimeout > 5*time.Minute || o.WriteTimeout <= 0 || o.WriteTimeout > time.Minute {
		return fmt.Errorf("request timeout must be in (0,30m], backend idle timeout in (0,5m], client write timeout in (0,1m]")
	}
	return nil
}

// idleReader cancels the HTTP request to unblock a stalled body Read. There is
// no background body-reading goroutine or unbounded producer/consumer queue.
type idleReader struct {
	reader  io.Reader
	timeout time.Duration
	cancel  context.CancelCauseFunc
}

func (r idleReader) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.timeout, func() { r.cancel(errIdle) })
	defer timer.Stop()
	return r.reader.Read(p)
}

type limitedReader struct {
	reader    io.Reader
	remaining int64
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n > 0 {
			return 0, errLimit
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

type proxyOutput struct {
	w           http.ResponseWriter
	ctx         context.Context
	timeout     time.Duration
	contentType string
	started     bool
}

func (o *proxyOutput) Write(p []byte) (int, error) { return o.write(p, false) }
func (o *proxyOutput) write(p []byte, errorFrame bool) (int, error) {
	controller := http.NewResponseController(o.w)
	n := 0
	for len(p) > 0 {
		if !errorFrame && o.ctx.Err() != nil {
			return n, o.ctx.Err()
		}
		deadline := time.Now().Add(o.timeout)
		if errorFrame {
			deadline = time.Now().Add(min(o.timeout, time.Second))
		} else if limit, ok := o.ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return n, errors.Join(errClientWrite, err)
		}
		if !o.started {
			o.w.Header().Set("Content-Type", o.contentType)
			o.w.Header().Set("Cache-Control", "no-store")
			o.w.Header().Set("X-Accel-Buffering", "no")
			o.started = true
		}
		size := min(len(p), 16<<10)
		written, err := o.w.Write(p[:size])
		n += written
		if err != nil {
			return n, errors.Join(errClientWrite, err)
		}
		if written != size {
			return n, errors.Join(errClientWrite, io.ErrShortWrite)
		}
		p = p[size:]
		if err := controller.Flush(); err != nil {
			return n, errors.Join(errClientWrite, err)
		}
	}
	return n, nil
}

func (h *RegisteredHandler) proxy(w http.ResponseWriter, r *http.Request, p *preparedRequest) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	trace := traceFrom(ctx)
	base, _ := registry.LocalURL(p.model.Backend.BaseURL)
	p.idle = func(ctx context.Context) (bool, error) { return h.execution.Idle(ctx, p.model) }
	h.capacity.Recover(ctx, base, p.idle)
	lease, capacityErr := h.capacity.Acquire(base)
	if capacityErr != nil {
		code := capacityErr.Error()
		if trace != nil {
			trace.outcome = code
		}
		w.Header().Set("Retry-After", "1")
		writeError(w, 503, code, "Execution capacity is busy or awaiting confirmed backend cleanup.", "")
		return
	}
	p.lease = lease
	idle, probeErr := p.idle(ctx)
	if probeErr != nil || !idle {
		code := "capacity_exceeded"
		if probeErr != nil {
			code = "capacity_unavailable"
		}
		if trace != nil {
			trace.outcome = code
		}
		w.Header().Set("Retry-After", "1")
		writeError(w, 503, code, "The backend execution slot is busy or could not be verified.", "")
		return
	}
	// Collect final usage even when the caller does not want a usage-only event.
	if p.payload.Stream {
		p.payload.IncludeUsage = true
	}
	if trace != nil {
		trace.dispatch = time.Now()
		trace.outcome = "backend_error"
		p.payload.RequestID = trace.id
	}
	response, err := p.dispatch(ctx, h.client)
	output := &proxyOutput{w: w, ctx: ctx, timeout: h.options.WriteTimeout, contentType: "application/json"}
	if err == nil {
		contentType, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		expected := "application/json"
		if p.payload.Stream {
			expected = "text/event-stream"
		}
		if response.StatusCode != 200 || parseErr != nil || contentType != expected || (params["charset"] != "" && params["charset"] != "utf-8" && params["charset"] != "UTF-8") || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
			err = errProtocol
		} else {
			output.contentType = expected
			reader := idleReader{reader: response.Body, timeout: h.options.IdleTimeout, cancel: cancel}
			if p.payload.Stream {
				err = relaySSE(&limitedReader{reader: reader, remaining: maxStreamBytes}, output, trace, &p.facts)
			} else {
				err = relayJSON(&limitedReader{reader: reader, remaining: maxJSONBytes}, output, trace, &p.facts)
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
	if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(err, errClientWrite) {
		if output.started {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if !output.started {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(min(h.options.WriteTimeout, time.Second)))
		writeError(w, status, code, message, "")
		return
	}
	if !p.payload.Stream {
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
