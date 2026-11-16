# Streaming and timings

[Start the gateway](README.md), then POST to
`http://127.0.0.1:8000/v1/chat/completions` with Bearer auth:

```json
{"model":"local-llm","messages":[{"role":"user","content":"Explain prefill."}],"max_completion_tokens":64,"stream":true,"stream_options":{"include_usage":true}}
```

For JSON replies, set `stream:false` and remove `stream_options`.
CLI: `python cli.py --base-url http://127.0.0.1:8000 --stream "Hello"`.

## Delivery and errors

SSE events are validated/flushed in order; terminal events wait for `[DONE]`.
Before output, failures return JSON errors. After streaming starts, HTTP stays
200 but errors close without `[DONE]`: **require `[DONE]` for success**.
Broken normal JSON is aborted without appending another document.

Bounds: normal response 4 MiB, stream 16 MiB, event 256 KiB, pending terminal
events 512 KiB; JSON validation retains an 8 KiB tail.

## Timeouts and logs

Defaults: `--request-timeout 2m` (includes queue wait),
`--backend-idle-timeout 30s` per read, `--client-write-timeout 10s` per write.
Cancellation alone does not release capacity; [cleanup](README.lifecycle.md) checks idle.

One `gateway_request` log per chat. Valid X-Request-ID values (1–64 letters,
digits, dots, underscores or hyphens) are preserved; others are generated.
Payload completion IDs are separate.

`dispatch_ms`, `first_text_ms` and `completion_ms` measure monotonic time from
receipt; unreached milestones are null. `ended_ms` includes cleanup. These are
gateway observations, not client E2E or proof of delivery. Logs exclude keys,
prompts, output and raw backend errors. [Queue fields](README.scheduler.md).

Tests: from `gateway/`, `go test -race -count=1 ./...`.
