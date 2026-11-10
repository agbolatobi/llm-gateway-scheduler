# Gateway API

Go 1.22+ and the standard library. Local key administration runs on Linux/WSL.
Health, authentication, model listing, tokenized request validation, and
user/global/model quotas, and normal/streamed inference forwarding work.
Actual token settlement and execution limits work; queueing remains planned.
See [model management](../README.models.md).

See [routing policy](README.routing.md) and [streaming, timeouts, and timing logs](README.streaming.md).
See [capacity, cleanup, and Postman tests](README.lifecycle.md) for GATE-07.

## Create a key and start

From the project root in WSL:

```bash
cd ~/ai-projects/llm-gateway-scheduler
./.runtime/gateway keys create --user tobia
./.runtime/gateway --config gateway.config.json
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
| `GET /v1/models` | Bearer key | `200` list of all canonical registrations |
| `POST /v1/chat/completions` | Bearer key | Validates schema, quotas and model budget; returns `200` JSON or SSE |

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

A valid key proceeds to validation; a ready registered model and sufficient
budgets reach inference. Use `"stream": true` for incremental SSE events. Authentication happens
before reading the chat body. Query-string keys and duplicate Authorization
headers are rejected. A broken/unreadable key store returns `503` and never
grants access.

## Python client reuse

The existing client automatically loads the selected key for its saved gateway
origin; it does not send that key to llama.cpp on port 8080 or other servers.

```bash
.venv/bin/python cli.py --base-url http://127.0.0.1:8000 "Explain prefill."
```

Add `--stream` for incremental output. Start the Go
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
Unknown models return 404, oversized context/output 400, and unavailable
or mismatched runtime metadata 503. Tokenization includes the running chat template.

Any valid API key can list and request every registered model. Missing or invalid
keys return 401. There are no per-model user allowlists; quotas still apply.

## User and global quotas (GATE-03)

`--config PATH` loads shared model/quota settings. Without it, built-in settings
register `local-llm` at port 8080 with these defaults (the file is not auto-loaded):

| Allowance | Per user | Effective global, one model | Global ceiling |
| --- | ---: | ---: | ---: |
| Requests / 60 seconds | 20 | 30 | 60 |
| Tokens / hour | 40,000 | 100,000 | 200,000 |

Global limits are `min(sum of canonical model allowances, ceiling)`, calculated
at configuration load, not by looking up the requested model. Each model in
`gateway.config.json` contributes once; aliases contribute nothing. Adding a
second model with the same allowance raises the effective global limits to the
ceiling. These are learning defaults, not measured GPU capacity. Models sharing
hardware still need backend admission limits. Per-model request/token limits
are checked after model validation, using canonical IDs.

Flow: authenticate → validate request shape → check/count user/global quotas → model
lookup/tokenization/reservation. Only chat POSTs consume inference allowances. All keys for a
user share counters. Health and model-list requests do not consume them.
Once a request passes the quota check, it counts even if a later stage returns
an error. A rejected quota check changes
neither request counter; invalid authentication/schema/methods do not count.

Exhaustion returns `429`, `type: rate_limit_error`, and one of
`user_request_limit`, `global_request_limit`, `user_token_limit`, or
`global_token_limit`, `model_request_limit`, or `model_token_limit`.
`Retry-After` is seconds rounded up to the relevant reset;
it is advice, not guaranteed availability. Missing quota enforcement fails closed
with `503`. Errors never expose other users' counters.

To reload edited settings without resetting usage, run from another WSL terminal:

```bash
./.runtime/gateway reload
# If the server uses a custom key/control directory:
./.runtime/gateway reload --config-dir /path/to/private-directory
```

The command waits for success or rejection and exits nonzero on failure. It uses
a private local Unix socket, not a public HTTP endpoint or inference API key.
The server and command must run as the same OS user with the same `--config-dir`.
Each running gateway needs its own directory. `reload.sock` is removed on clean
shutdown; a stale socket after a crash is recovered at the next startup under a lock.
Restart an older gateway once after rebuilding to enable the command.

The command rereads the file selected at startup (`--config`, or legacy
`--quota-config`); it does not accept a replacement configuration path. Built-in
defaults have no file to reload. `--timeout 10s` controls the confirmation wait;
if it times out, check the server log because the reload may still finish.
Reload does not update executable code or start/stop model processes.
SIGHUP to the exact gateway PID remains supported as a manual alternative.

Invalid reloads retain the
previous settings. Lowering a limit below current usage blocks new work; it does
not cancel accepted requests. Changing window lengths requires restart.

Windows are fixed and anchored at process startup, not rolling windows. They
reset lazily on the next quota operation; boundary bursts are possible. All
state is in memory for one process: restart resets it, and multiple processes
do not share a global allowance. Idle user entries disappear at window rollover.

**Token accounting boundary:** available tokens are `limit - spent - reserved`.
GATE-04 atomically reserves user/global/model budgets for tokenized input plus
the output cap. Before a backend connection is obtained, failures release unused
tokens. Completed replies charge validated actual usage; missing/unreliable usage
charges the full reservation. See [settlement and capacity](README.lifecycle.md).
Reservations retain their original admission-hour bucket across resets.

`--quota-config` remains a legacy, quota-only mode for existing commands; it has
no model registry or model validation, and `/v1/models` remains a placeholder in
that mode; chat remains `501`. Do not combine it with `--config`; use the shared file for inference.

## Build and test

With Go installed inside WSL, from the project root:

```bash
cd gateway
go test ./...
go test -race ./...
go vet ./...
go build -o ../.runtime/gateway-next ./cmd/server
mv ../.runtime/gateway-next ../.runtime/gateway
```

Restart an existing gateway after rebuilding. Key files survive restarts.

- `cmd/server/`: HTTP startup and local key commands.
- `internal/api/`: schemas, authentication, validation, JSON/SSE proxy, timing.
- `internal/routing/`: immutable backend selection and outbound HTTP transport.
- `internal/auth/`: key generation, persistence, verification, revocation.
- `internal/quota/`: configuration, atomic user/global counters, token ledger.
- `internal/registry/`: shared model configuration, readiness, templating/tokenization.
- `../client/auth.py`: origin-scoped client key loading.

References: [Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create),
[streaming chunks](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events),
[project plan](../README.gateway.md).
