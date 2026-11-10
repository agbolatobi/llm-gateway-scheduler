# Local LLM Inference Gateway + Smart Request Scheduler

Current features: a local llama.cpp server, Python chat CLI, and AIPerf
benchmarks with JSONL, CSV and PDF reports, including sampled server RAM/VRAM
where supported. The Go gateway supports API-key management, authentication,
chat validation, model registration, user/global/model budget checks, and JSON/SSE
inference forwarding. The Python CLI can manage separate registered model processes.
Actual usage settlement and execution limits are implemented; queueing remains planned.

Run commands from the project root in Ubuntu WSL using Python 3.12.

## Setup

The CLI and benchmarks share one root `.venv`. First-time setup:

```bash
cd ~/ai-projects/llm-gateway-scheduler
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

For later sessions, change to this directory and run `source .venv/bin/activate`.
Reinstall requirements when that file changes. Dependencies do not install the
runtime or model; the current launcher expects:

- Runtime: `runtime/llama-b11669/llama-server`
- Model: `models/qwen2.5-1.5b-instruct-q4_k_m.gguf`

Paths and launch settings are in [client/server.py](client/server.py). Defaults:
CPU inference, context 4,096, one generation slot, URL
`http://127.0.0.1:8080`, model alias `local-llm`.

## Start and chat

```bash
python cli.py --start-server --stream "Explain the KV cache."
```

The CLI reuses a ready server, waits for a loading model, or starts it when the
connection is refused. A shared startup lock prevents duplicate launches.
Other errors are reported. Startup failure stops only the process that attempt
launched; a ready server stays running after the CLI exits.

```bash
# Start or wait for readiness without sending a prompt
python cli.py --start-server --server-only

# Send a prompt to an already-running server
python cli.py --max-tokens 256 --temperature 0.7 "Explain prefill versus decode."
```

Each command is an independent conversation. Default output cap: 128 tokens;
temperature: 0. Use `--stream` for incremental output, `--startup-timeout 180`
to extend the default 120-second startup wait, or `--base-url` to change ports.
Automatic startup accepts local HTTP URLs using `127.0.0.1` or `localhost`.
For a new server, `--ctx-size`, `--parallel`, `--threads`, `--threads-batch`,
`--batch-size` and `--ubatch-size` override startup defaults. These flags require
`--start-server`; they do not reconfigure an already-running server.
`--model` selects the served alias, not a model file. See `python cli.py --help`.

### Manual startup and diagnostics

Alternatively, start the runtime in a separate terminal from the project root:

```bash
./runtime/llama-b11669/llama-server \
  --model models/qwen2.5-1.5b-instruct-q4_k_m.gguf \
  --alias local-llm --host 127.0.0.1 --port 8080 \
  --ctx-size 4096 --parallel 1 --device none
```

Keep that terminal open. Check readiness and automatic-startup logs with:

```bash
curl --fail http://127.0.0.1:8080/health
tail -n 50 .runtime/llama-server-8080.log
```

Ready health response: `{"status":"ok"}`. Log filenames use the server port.

## Stop and clean up

Finish active requests first. Ctrl+C in the chat CLI interrupts a request;
Ctrl+C in a manually started server terminal stops the server.

For either startup method:

```bash
python stop_server.py --dry-run
python stop_server.py
ss -ltnp 'sport = :8080'
```

The script selects a single matching project server by owner, executable,
working directory and explicit port. Ambiguous matches or timeouts are errors;
it does not force-kill. No match does not prove the port is free. For another
port or a longer timeout, use `python stop_server.py --port 8081 --timeout 30`.

Once there is no listener, optionally remove the log:

```bash
rm -i -- /root/ai-projects/llm-gateway-scheduler/.runtime/llama-server-8080.log
```

This permanently deletes that log after confirmation; save a copy if needed
and skip it if absent. Adjust port-specific commands together. Keep `.runtime`
and its lock files for startup coordination. `deactivate` exits the Python
environment; it does not stop the server.

## Gateway API

See [gateway/README.md](gateway/README.md) to run the Go server on port 8000.
Chat and model-list endpoints require a Bearer API key. Model listing works;
chat validates model/token budgets and forwards replies from a running backend.
Key creation, saved-key reuse, revocation, health, chat validation, and user/global
request quotas work. See the gateway README for configuration and reloads, and
[model management](README.models.md) for shared configuration and multi-model commands.
The CLI and benchmarks default to llama.cpp on port 8080. For gateway chat, use
`python cli.py --base-url http://127.0.0.1:8000 --stream "Explain prefill."`.
See [streaming and timeout controls](gateway/README.streaming.md).

## Benchmarks and metrics

- [Benchmark guide](benchmarks/README.md): run cases, concurrency/context experiments and export reports.
- [Metrics guide](README.metrics.md): definitions, limitations and planned measurements.
- [MEAS-02 results](benchmarks/MEAS-02.md): measured uncached prompt/output comparisons and runtime evidence.
- [MEAS-03 results](benchmarks/MEAS-03.md): tested concurrency ceiling and latency/throughput/memory tradeoff.
- [MEAS-04 results](benchmarks/MEAS-04.md): controlled context-memory comparison and [frozen scheduler baseline](benchmarks/baseline.json).

Machine-specific browser instructions are in `README.local.md` (local, ignored
by Git).

## Tests

```bash
python -m unittest discover -s tests -v
```

These tests do not load the model or stop an existing server. The optional
[AIPerf integration test](benchmarks/README.md#tests) uses a fake streaming server.
