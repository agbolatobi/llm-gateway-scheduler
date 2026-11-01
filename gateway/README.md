# Gateway API skeleton

Go standard library only; Go 1.22+. No model calls or credentials are needed.
This implements a **text-only Chat Completions schema subset**, not the entire
OpenAI API. Authentication, key storage, quotas, routing, and scheduling remain planned.

## Run

From the project root in WSL, with Go installed:

```bash
cd gateway
go run ./cmd/server
```

Default: `http://127.0.0.1:8000`. Use `-listen 127.0.0.1:8001` to change it.
Keep it loopback-only: there is no authentication yet. Ctrl+C stops the gateway;
it does not start or stop llama.cpp on port 8080. No Python virtualenv is needed.

If using the Linux executable built for this workspace instead of installing Go:

```bash
# From the project root; rebuild after code changes.
./.runtime/gateway
```

## Endpoints

| Endpoint | Current behavior |
| --- | --- |
| `GET /health` | `200 {"status":"ok"}`; gateway liveness only, not model readiness |
| `POST /v1/chat/completions` | Validates the request, then returns `501 not_implemented` |
| `GET /v1/models` | Returns `501 not_implemented`; no registry yet |

```bash
curl http://127.0.0.1:8000/health

curl -i http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"local-llm","messages":[{"role":"user","content":"Explain prefill."}],"max_completion_tokens":64,"stream":false}'
```

The chat request above deliberately returns:

```json
{"error":{"message":"Chat inference is not implemented. This endpoint validates requests only.","type":"server_error","param":null,"code":"not_implemented"}}
```

Accepted fields:

- Required: non-empty `model`, non-empty `messages` with text `content` and a
  `system`, `developer`, `user`, or `assistant` role; optional message `name`.
- Optional: positive `max_completion_tokens` **or** legacy `max_tokens`,
  `temperature` (0–2), `stream`, and `stream_options.include_usage` when streaming.
- Gateway extension: `request_class` (`interactive` or `background`); not scheduled yet.

Unknown fields, tools, multimodal content, conflicting caps, malformed JSON,
and multiple JSON objects are rejected. Body limit: 1 MiB. Error responses are
JSON: `400` invalid input, `413` too large, `415` unsupported content type/encoding,
`404` unknown route, `405` wrong method. Even `stream: true` returns a JSON `501`,
not a fabricated SSE stream. Model IDs and context budgets are not checked yet.

## Code and tests

- `cmd/server/main.go`: startup, HTTP timeouts, graceful shutdown.
- `internal/api/schema.go`: request, completion, chunk, usage, and error types.
- `internal/api/server.go`: routes and validation; no backend connection.

```bash
cd gateway  # From the project root.
go test ./...
go vet ./...
go build -o ../.runtime/gateway ./cmd/server
```

Schema references: [Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
and [streaming chunks](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events).
Project plan: [gateway requirements](../README.gateway.md).
