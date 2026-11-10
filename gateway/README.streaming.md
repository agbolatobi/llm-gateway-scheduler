# Replies and streaming (GATE-06)

The gateway forwards text-only chat requests to a running registered backend.
Normal replies return JSON; `"stream": true` returns Server-Sent Events (SSE).
It does not start models, retry generation, or switch models after a failure.

## Run

From the project root in WSL, start the model using the [model commands](../README.models.md),
then start the gateway in another terminal:

```bash
./.runtime/gateway --config gateway.config.json
```

In Postman, send `POST http://127.0.0.1:8000/v1/chat/completions` with
**Authorization → Bearer Token**, the full saved API token, and **Body → raw → JSON**:

```json
{
  "model": "local-llm",
  "messages": [{"role": "user", "content": "Explain prefill and decode."}],
  "max_completion_tokens": 64,
  "stream": true,
  "stream_options": {"include_usage": true}
}
```

For normal JSON, set `stream` to `false` and remove `stream_options`.
The Python CLI also reuses your saved gateway key:

```bash
.venv/bin/python cli.py --base-url http://127.0.0.1:8000 --stream "Explain prefill."
```

Restart an already-running gateway after rebuilding; `reload` only reloads configuration.

## Delivery and errors

- Text events are validated and flushed in order as they arrive. Completion IDs,
  model metadata and optional usage remain unchanged. Heartbeats are not text.
- Final finish/usage events are held until a valid `[DONE]` arrives. EOF, malformed
  data, backend errors and timeouts never synthesize a successful completion.
- Before output starts, errors use the usual JSON envelope and HTTP status:
  `503` unavailable transport, `504` timeout, `502` bad response/backend failure.
- After SSE starts, HTTP stays `200`; failure sends `event: error` with the same
  error envelope, then closes without `[DONE]`. Clients must check error events
  and require `[DONE]` for success. A disconnected/unwritable client gets no error frame.
- Large normal JSON responses are forwarded incrementally with an 8 KiB tail
  retained for validation. If an already-started JSON reply fails, the HTTP
  response is aborted rather than appending another JSON document.

Buffering is bounded: 4 MiB per normal response, 16 MiB per stream, 256 KiB per
SSE event, and 512 KiB for pending terminal events. JSON parsing also limits
depth and collection sizes; one large JSON string may require memory up to the
response cap. There is no unbounded response queue. SSE follows
[standard event framing](https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation).

## Timeout controls

```bash
./.runtime/gateway --config gateway.config.json \
  --request-timeout 2m \
  --backend-idle-timeout 30s \
  --client-write-timeout 10s
```

These are the defaults. The request limit includes validation and generation.
The idle limit bounds each wait for backend body data; the write limit bounds
each client write. Transport has a 2s dial timeout; waiting for response headers
uses the overall request deadline because normal replies may generate first.
Client disconnects and failures cancel the outbound HTTP request. That does not
prove the backend stopped computing; [GATE-07 cleanup](README.lifecycle.md) confirms
inactivity before freeing capacity.

## Request IDs and timings

The gateway returns and forwards `X-Request-ID`. A caller ID of 1–64 letters,
digits, dots, underscores or hyphens is preserved; other values are replaced.
Backend completion IDs inside JSON/SSE are preserved separately.

One JSON timing record is written to the server log per chat request:

```json
{"event":"gateway_request","request_id":"example-1","outcome":"completed","dispatch_ms":8,"first_text_ms":92,"completion_ms":350,"ended_ms":351}
```

All durations start at gateway receipt (zero) and use Go's monotonic clock:
`dispatch_ms` precedes the outbound call, `first_text_ms` records the first
non-empty text observed, and `completion_ms` follows successful delivery.
Unreached milestones are `null`; `ended_ms` also exists on failure. Logs exclude
API tokens, prompts, generated text and raw backend errors. These are gateway
observations, not client end-to-end latency or proof that the client consumed data.
For normal JSON, first text is observable only when its content string is parsed;
use streaming to measure first-text latency during generation.

GATE-07 settles validated completed usage and refunds unused reserved tokens.
Missing/unreliable usage falls back to charging the full reservation. Streaming
usage is collected internally and forwarded when the client requests it.

## Replay tests

```bash
cd gateway
go test -race -count=1 ./...
go vet ./...
```

Fake-backend tests cover both routes, early delivery, ordered terminal events,
request IDs, monotonic timing, malformed/truncated responses, size limits,
idle/overall deadlines, failed client writes and cancellation. They load no model
weights and do not stop your running servers.

Optional smoke test against the first model in your configuration (two requests,
eight output tokens each; the model must already be running):

```bash
GATEWAY_LIVE_CONFIG="$PWD/../gateway.config.json" \
  go test -count=1 -run TestProxyRunningModel -v ./internal/api
```
