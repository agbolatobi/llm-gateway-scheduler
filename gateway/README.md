# Gateway API and chat

From the project root, with Go 1.22+ and the [shared venv](../README.md#setup):

```bash
mkdir -p .runtime
(cd gateway && go build -o ../.runtime/gateway-next ./cmd/server)
mv .runtime/gateway-next .runtime/gateway
python cli.py models start local-llm
./.runtime/gateway keys create --user YOUR_NAME
./.runtime/gateway --config gateway.config.json
```

Stop any older gateway first. Gateway: **8000**; model: **8080**.
Without `--config`, built-in settings apply; the file is not loaded automatically.

## Browser chat

Open **http://127.0.0.1:8000/chat/**, paste the full token and connect.
Markdown replies stream; **Stop** cancels, **New chat** clears context.
Response settings default to 1,024 output tokens; the configured model allows
2,048 within a 4,096-token context. Token quotas still apply.

The key survives refresh in tab `sessionStorage`; disconnect/rejected keys clear
it. History clears on refresh. Blocked storage requires re-entering the key.
Raw HTML is escaped and remote images are not loaded.

## Keys

```bash
./.runtime/gateway keys list
./.runtime/gateway keys show
./.runtime/gateway keys use --id KEY_ID
./.runtime/gateway keys revoke --id KEY_ID
```

Create prints/saves the full token; list shows IDs. Any valid key can use any model.
Revocation blocks new requests. The key store is `~/.config/llm-gateway-scheduler/`:
`api-keys.json` holds hashes; `credentials.json` holds reusable secrets (0700 directory,
0600 files). Use matching `--config-dir PATH` for custom stores. Python reuses keys
only for their saved origin.

For deployment, `keys create --user YOUR_NAME --chat-url https://HOST/chat/`
changes the printed link only. It neither configures HTTPS nor changes the saved
client origin. Serve `/chat/` and `/v1/` together; use the serving gateway's key store.

## API / Postman

Use **Bearer Token** with the full token, not its ID.

| Endpoint | Auth | Purpose |
| --- | --- | --- |
| GET `/health` | None | Liveness |
| GET `/v1/models` | Bearer | Registered models, not readiness |
| POST `/v1/chat/completions` | Bearer | JSON or streaming chat |

Body → raw → JSON:

```json
{"model":"local-llm","messages":[{"role":"user","content":"Explain prefill."}],"max_completion_tokens":64,"stream":false}
```

Text roles: system/developer/user/assistant. Optional: temperature 0–2, stream/usage
options and `request_class: interactive|background`. Use `max_completion_tokens`
or `max_tokens`, not both. Unsupported fields/tools/multimodal content return 400;
body limit: 1 MiB. Invalid keys return 401. [Streaming](README.streaming.md).

## Quotas

Defaults: user **20 requests/min + 40,000 tokens/hour**; each model contributes
**30/min + 100,000/hour** globally, capped at **60/min + 200,000/hour**.
Aliases add no allowance; keys for one user share counters. Exhaustion returns
429 and Retry-After. Windows/counters are process-local and reset on restart.
[Charging and refunds](README.lifecycle.md).

## Reload

```bash
./.runtime/gateway reload
```

Use the same OS user/key directory. Reload preserves usage and rejects invalid
changes; timeout leaves the outcome unconfirmed. Code, quota windows, listen
address and existing model identity/context/launch changes need restart.
Lower limits affect new admissions; reload never starts models.
Legacy `--quota-config` is validation-only (chat 501), incompatible with `--config`.

## Tests and deployment

```bash
(cd gateway && go test -race -count=1 ./... && go vet ./...)
node --test gateway/tests/chat_*.test.mjs
```

Node tests require Node 18+, no npm install. [Reusable Postman lab](README.lifecycle.md)
uses port **8005**. [Kubernetes deployment](../infra/README.md) opts into container
binding with `--listen 0.0.0.0:8000 --allow-remote-listen`; model backends remain
loopback-only. Shutdown drain defaults to 5s; deployment uses `--shutdown-timeout 130s`.

[Architecture](../README.gateway.md) · [Models](../README.models.md) ·
[Routing](README.routing.md) · [Scheduler](README.scheduler.md).
