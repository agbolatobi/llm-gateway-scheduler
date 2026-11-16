# Routing

Each canonical model and its aliases map to one configured loopback HTTP backend.
There is no load balancing, fallback, automatic startup or generation retry.
Accepted requests retain their configuration snapshot across reloads; registry
locks are released before generation.

Forwarded: messages, output cap, temperature, stream/usage options and X-Request-ID.
`max_completion_tokens` becomes llama.cpp's `max_tokens`. Gateway keys, cookies
and request class are not forwarded; proxies, redirects and body replay are disabled.

Dial timeout: **2s**; headers use the total request deadline. Cancellation ends
outbound HTTP but does not prove compute stopped. Each prepared request owns one
dispatch and cleanup; [settlement](README.lifecycle.md) verifies idle or quarantines.

Errors: route/dial **503**, timeout **504**, invalid backend response **502**.
[Streaming behavior](README.streaming.md).

From `gateway/`: `go test -race -count=1 ./internal/routing ./internal/api`.
