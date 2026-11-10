# Model registration and management

Run from the project root in Ubuntu WSL. `gateway.config.json` is shared by the
Go gateway and Python CLI. It registers the existing `local-llm` model only;
no second model is downloaded, registered, or started automatically.

## Commands

```bash
.venv/bin/python cli.py models validate
.venv/bin/python cli.py models list
.venv/bin/python cli.py models status
.venv/bin/python cli.py models start local-llm
.venv/bin/python cli.py models stop local-llm
./.runtime/gateway --config gateway.config.json
```

Each model command accepts `--config PATH`; start/stop also accept `--timeout`.
`status MODEL` checks one registration. `list` and `validate` make no network calls.
Start verifies identity and per-request context before reuse. New processes keep
running after the CLI exits. Stop sends SIGTERM only to a recorded project-owned
process after checking its executable, PID start time, command fingerprint, and
configuration. Reused external processes are not adopted or stopped.

## Add a second model manually

1. Obtain a suitable second GGUF file yourself, or start an external llama.cpp server.
2. Add a `models` entry to `gateway.config.json` with a unique canonical `id`,
   aliases, `runtime_id`, and free backend port (for example 8082; check conflicts).
3. Set its expected backend model name, context/output limits, and model allowances.
4. For a project-managed server, set `launch.model_file` and the other launch fields.
   Paths resolve from the configuration file's directory. Omit `launch` entirely
   for externally managed servers. Managed runtimes currently require `parallel: 1`.
5. Validate, explicitly start the new model if needed, and check status:

```bash
.venv/bin/python cli.py models validate
.venv/bin/python cli.py models start YOUR_MODEL_ID
.venv/bin/python cli.py models status
```

To apply edited configuration to an already-running gateway started with
`--config`, run `./.runtime/gateway reload` from another WSL terminal. The command
confirms success or reports rejection. If the server uses `--config-dir PATH`,
pass the same flag to `reload`. Restart an older gateway once to enable this command.
Reload is atomic with quota settings and preserves usage. Invalid reloads keep the old settings.
Listen address, existing backend identity/context/launch settings, and quota-window
changes require a gateway restart; runtime setting changes also require explicitly
stopping/restarting the affected owned model. Reload never starts/stops a model.

Two model processes need enough combined RAM/VRAM. Quota ceilings do not add
hardware capacity. The checked-in CPU-only settings are learning defaults.

## Unregistered models

Check an address you explicitly supply (there is no port scanning):

```bash
.venv/bin/python cli.py models check --base-url http://127.0.0.1:8082
```

An unregistered address produces a warning, not a configuration edit. The server
is left alone; direct requests remain possible. A registered address serving a
different model reports `mismatch`. The gateway rejects unknown model IDs with
`404 model_not_found`. There is no automatic or interactive registration command.

Legacy direct chat remains available:

```bash
.venv/bin/python cli.py --base-url http://127.0.0.1:8082 --model YOUR_BACKEND_MODEL "Hello"
```

Direct chat warns about unregistered local model/address pairs but does not block
them. `--config PATH` on a chat command selects the warning configuration, not the
chat URL. Legacy `--start-server` still uses the old fixed model file; use
`models start` to select weights from the registry. Benchmarks keep their existing
direct-server lifecycle and frozen baseline; registration does not rewrite it.

## Gateway validation

Any valid gateway API key can list and request every registered model. There are
no per-model user allowlists. `/v1/models` lists canonical registrations, not
guaranteed-ready runtimes. Aliases share one model quota; user/global quotas still
apply. Remove the obsolete `allowed_users` field from older configurations;
configuration validation rejects it rather than silently ignoring it.

Chat processing is: authentication → basic schema → user/global request quotas →
canonical model/output-cap checks → runtime identity/context checks →
`/apply-template` and `/tokenize` → atomic user/global/model reservation.
Configured managed models also check the runtime-reported model file path. External
registrations rely on reported identity, not a cryptographic proof of weights.

Successful validation routes to the registered backend and returns JSON or an
SSE stream. Valid completed usage settles token budgets; missing/unreliable usage
charges input plus the output cap. Accepted request counts remain charged.
Global and per-runtime execution limits default to one; the current proxy requires
one physical slot per runtime. See [capacity and cleanup](gateway/README.lifecycle.md).
Missing/invalid API keys return 401, unknown models 404, oversized output/context 400,
quota exhaustion 429, and unavailable/mismatched runtime metadata 503.
Token counts include chat-template and special-token overhead; unavailable
tokenizers fail closed rather than using estimates.

## Tests

```bash
.venv/bin/python -m unittest discover -s tests -p test_models.py -v
cd gateway
/usr/local/go/bin/go test -race ./...
```

Tests use two fake model backends and temporary owned processes, not a second
loaded LLM. Real two-model memory/performance validation depends on your hardware
and installing the second model.
