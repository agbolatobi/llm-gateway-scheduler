package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// Request contains only backend-supported fields. Model and effective output
// cap are set by admission, never by copying the incoming HTTP request/headers.
type Request struct {
	Messages     json.RawMessage
	OutputTokens int
	Temperature  *float64
	Stream       bool
	IncludeUsage bool
	RequestID    string
}

// Attempt transfers an open response body to its caller on success. MayHaveRun
// becomes true when a connection is obtained: conservative, not proof of usage.
// A failed dial or rejection before Do leaves it false.
type Attempt struct {
	Response   *http.Response
	MayHaveRun bool
}

type Client struct{ http *http.Client }

func NewClient() *Client {
	return NewClientWithHeaderTimeout(10 * time.Second)
}

// A normal completion may not send headers until all generation has finished.
// The registered gateway supplies its overall request limit here; the request
// context still bounds the full exchange, including prior validation.
func NewClientWithHeaderTimeout(headerTimeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout:  headerTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           16, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second,
		DisableCompression: true,
	}
	return &Client{http: &http.Client{Transport: transport,
		// No five-second inspection timeout on generation/body consumption.
		// Its lifetime follows the caller's context; GATE-06 adds stream policy.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (client *Client) CloseIdleConnections() { client.http.CloseIdleConnections() }

func (client *Client) Send(ctx context.Context, route Route, request Request) (Attempt, error) {
	var attempt Attempt
	if route.baseURL == "" || route.backendModel == "" {
		return attempt, failure(503, "route_unavailable", "No eligible configured backend is available.", nil)
	}
	if request.OutputTokens < 1 || request.OutputTokens > route.maxOutputTokens || !json.Valid(request.Messages) {
		return attempt, failure(500, "dispatch_invalid", "The prepared backend request is invalid.", nil)
	}
	if err := ctx.Err(); err != nil {
		return attempt, transportError(err)
	}
	// Separate wire schema: request_class, Authorization, cookies and client
	// idempotency headers cannot leak into the backend request.
	payload := struct {
		Model         string          `json:"model"`
		Messages      json.RawMessage `json:"messages"`
		MaxTokens     int             `json:"max_tokens"`
		Temperature   *float64        `json:"temperature,omitempty"`
		Stream        bool            `json:"stream,omitempty"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options,omitempty"`
	}{Model: route.backendModel, Messages: request.Messages, MaxTokens: request.OutputTokens,
		Temperature: request.Temperature, Stream: request.Stream}
	if request.Stream && request.IncludeUsage {
		payload.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return attempt, failure(500, "dispatch_invalid", "The prepared backend request is invalid.", err)
	}
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, route.baseURL+ChatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return attempt, failure(500, "dispatch_invalid", "The prepared backend request is invalid.", err)
	}
	outbound.Header.Set("Content-Type", "application/json")
	if request.Stream {
		outbound.Header.Set("Accept", "text/event-stream")
	} else {
		outbound.Header.Set("Accept", "application/json")
	}
	if request.RequestID != "" {
		outbound.Header.Set("X-Request-ID", request.RequestID)
	}
	// A non-empty POST without GetBody or idempotency headers is not replayable
	// by net/http, including when a reused connection fails before a write.
	outbound.GetBody = nil
	response, err := client.http.Do(outbound)
	attempt.MayHaveRun = connected.Load()
	if err != nil {
		return attempt, transportError(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return attempt, failure(502, "backend_redirect", "The backend returned an unsupported redirect.", nil)
		}
		return attempt, failure(502, "backend_error", "The backend rejected or failed the request.", nil)
	}
	attempt.Response = response // Caller owns and must close the body.
	return attempt, nil
}

func transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return failure(502, "request_cancelled", "The request was cancelled.", err)
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return failure(504, "backend_timeout", "The backend did not respond before the deadline.", err)
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" {
		return failure(503, "backend_unavailable", "The configured backend is unavailable.", err)
	}
	return failure(502, "backend_transport_error", "The backend connection failed.", err)
}
