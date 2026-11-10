# Routing and dispatch (GATE-05)

Implemented and connected to the public API through the
[GATE-06 response proxy](README.streaming.md). Valid requests generate normal
JSON replies or SSE streams using the selected running backend.

## Policy

- Resolve aliases and validate the model in GATE-04; route only after admission.
- One canonical model uses its one configured backend. No balancing, fallback,
  automatic model startup, redirects, or generation retries.
- Reuse admission's readiness/identity/context check for that exact model snapshot.
  A server can fail after the check; dispatch handles that failure, not a substitute model.
- Copy the validated model and serialized messages; reload affects later requests,
  not an in-flight destination. Registry locks end before outbound generation.

## Outbound request

`routing.Select` creates an immutable route. `Client.Send` posts to its configured
`/v1/chat/completions`, with the incoming context and an allowlisted JSON body:

```json
{
  "model": "the-backend-expected-model",
  "messages": [{"role": "user", "content": "hello"}],
  "max_tokens": 128
}
```

The effective output cap is always explicit, including when the user omitted it.
`max_completion_tokens` is normalized to llama.cpp's `max_tokens`. Temperature,
stream, and usage options are supported. A validated or generated `X-Request-ID`
is forwarded for correlation. Gateway keys, cookies, idempotency headers, and `request_class`
never go to the backend. Only configured IPv4 loopback HTTP destinations are accepted.

Connections have a 2-second dial timeout. The registered gateway uses its overall
request deadline while waiting for response headers, including normal generation.
Generation follows the caller's cancellation and the proxy's request, idle-read,
and client-write deadlines. The client disables proxies and redirects, removes request-body
replay support, and has no retry loop. HTTP cancellation is not proof that backend
compute has stopped; [GATE-07 cleanup](README.lifecycle.md) verifies inactivity before releasing capacity.

## Reservation and response ownership

The prepared request permits one dispatch. Its idempotent `finish()` closes any
response body and settles the reservation once, including error paths:

- Before obtaining a connection: release unused tokens (actual usage zero).
- After obtaining a connection: charge validated completed usage when available;
  otherwise conservatively charge the whole reservation.

The internal reply consumer must finish reading before returning; it cannot hand
the body to background work after cleanup. GATE-07 owns actual/fallback accounting
and confirmed execution-capacity release. Request counts
remain charged.

Missing/invalid routes and failed dials map to `503`; header/deadline timeouts to
`504`; redirects, backend non-2xx responses, and other transport failures to `502`.
Responses are sanitized. Client cancellation cancels the outbound call without
trying to send a fresh error response to a disconnected client.

## Replay the tests

From the project root in WSL:

```bash
cd gateway
/usr/local/go/bin/go test -race -count=1 ./internal/routing ./internal/api
```

Tests use two fake backends, isolated dispatch checks, and the live proxy. They cover routing,
effective caps, header isolation, invalid requests, aliases, cancellation, timeouts,
no retries/redirects, reload during a slow request, immutable snapshots, and token
cleanup. These tests load no real model weights. See the GATE-07 guide for
accounting/capacity tests; queueing belongs to the scheduling milestone.
