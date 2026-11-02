# Gateway API

Go 1.22+ and the standard library. Local key administration runs on Linux/WSL.
Health, API-key authentication, and chat validation work. Inference, model listing,
quotas, routing, and scheduling are still placeholders.

## Create a key and start

From the project root in WSL:

```bash
cd ~/ai-projects/llm-gateway-scheduler
./.runtime/gateway keys create --user tobia
./.runtime/gateway
```

If an older gateway is running on port 8000, stop it with Ctrl+C in its terminal
first. The rebuilt executable takes effect after restarting that process.
Default address: `http://127.0.0.1:8000`; change it with
`-listen 127.0.0.1:8001`. Keep this HTTP service on loopback.

With Go installed inside WSL, use `go run ./cmd/server` from `gateway/`
instead of `./.runtime/gateway`; the same arguments apply.

## Manage saved keys

```bash
./.runtime/gateway keys list
./.runtime/gateway keys show
./.runtime/gateway keys use --id KEY_ID
./.runtime/gateway keys revoke --id KEY_ID
```

- `create --user USER_ID` generates a random key, saves it, selects it, and prints
  the complete key on the `API token:` line for copying into Postman.
- `list` shows IDs, users, and status; it does not reveal keys or hashes.
- `show` prints the selected secret for copying into Postman. Use `--id KEY_ID`
  to show another saved, active key.
- `use` selects an existing key for automatic client reuse.
- `revoke` rejects that key on subsequent requests without restarting the gateway.
  It does not cancel requests already authenticated. Other keys for that user remain active.

Files live outside the repository in `~/.config/llm-gateway-scheduler/`:

| File | Contents |
| --- | --- |
| `api-keys.json` | Server verification hashes, key IDs, user IDs, creation dates, revocation status |
| `credentials.json` | Reusable client secrets, gateway origins, and selected key |

The directory is created with mode `0700`; files use `0600`. Client secrets
are not encrypted. The `create` and `show` commands print the full token. Server
verification never reads `credentials.json`. Writes are atomic and local
administration commands share a process lock.

For a different gateway port, create the key with
`--base-url http://127.0.0.1:8001`. All key commands and the server accept
`--config-dir PATH`; use the same directory for both.

## Test with Postman

Set `base_url` to `http://127.0.0.1:8000`. Copy the token from the `API token:`
line after creation (without the label), or use the output of `keys show`,
into a local secret variable named `api_key`. For both protected endpoints,
choose **Authorization → Bearer Token** and enter `{{api_key}}`.
The resulting header is `Authorization: Bearer <api_key>`.

| Endpoint | Authentication | Current response |
| --- | --- | --- |
| `GET /health` | None | `200 {"status":"ok"}`; gateway liveness only |
| `GET /v1/models` | Bearer key | `501 not_implemented` after authentication |
| `POST /v1/chat/completions` | Bearer key | Validates input, then returns `501 not_implemented` |

For chat, choose **Body → raw → JSON** and use `Content-Type: application/json`:

```json
{
  "model": "local-llm",
  "messages": [{"role": "user", "content": "Explain prefill."}],
  "max_completion_tokens": 64,
  "stream": false
}
```

Missing, invalid, and revoked keys receive `401`:

```json
{
  "error": {
    "message": "A valid API key is required. Use Authorization: Bearer <api_key>.",
    "type": "authentication_error",
    "param": null,
    "code": "invalid_api_key"
  }
}
```

A valid key reaches the existing `501` placeholder. Authentication happens
before reading the chat body. Query-string keys and duplicate Authorization
headers are rejected. A broken/unreadable key store returns `503` and never
grants access.

## Python client reuse

The existing client automatically loads the selected key for its saved gateway
origin; it does not send that key to llama.cpp on port 8080 or other servers.

```bash
.venv/bin/python cli.py --base-url http://127.0.0.1:8000 "Explain prefill."
```

This currently reports the gateway's `501` inference placeholder. Start the Go
gateway separately; `--start-server` is the llama.cpp launcher. For a custom
credentials location, set `LLM_GATEWAY_CREDENTIALS` to the credentials file.
Authenticated requests do not follow redirects.

## Supported chat fields

This is a text-only Chat Completions subset:

- Required: non-empty `model` and `messages`; each message needs a text
  `content` and role `system`, `developer`, `user`, or `assistant`.
  Message `name` is optional.
- Optional: positive `max_completion_tokens` **or** legacy `max_tokens`,
  `temperature` (0–2), `stream`, and `stream_options.include_usage` when streaming.
- Gateway extension: `request_class` (`interactive` or `background`).

Unknown fields, tools, multimodal content, conflicting caps, and malformed JSON
return `400`. Other errors: `413` body over 1 MiB, `415` unsupported content
type/encoding, `404` unknown route, `405` wrong method after authentication.
Even streaming requests return JSON `501` for now. Model IDs and context
budgets are not checked yet.

## Build and test

With Go installed inside WSL, from the project root:

```bash
cd gateway
go test ./...
go vet ./...
go build -o ../.runtime/gateway-next ./cmd/server
mv ../.runtime/gateway-next ../.runtime/gateway
```

Restart an existing gateway after rebuilding. Key files survive restarts.

- `cmd/server/`: HTTP startup and local key commands.
- `internal/api/`: schemas, authentication middleware, routes, validation.
- `internal/auth/`: key generation, persistence, verification, revocation.
- `../client/auth.py`: origin-scoped client key loading.

References: [Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create),
[streaming chunks](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events),
[project plan](../README.gateway.md).
