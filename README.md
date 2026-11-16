# Local LLM Gateway + Scheduler

llama.cpp inference, a Python CLI, a Go gateway with browser chat and request
queues, and AIPerf benchmarks. Run commands from the project root in Ubuntu WSL
with Python 3.12; the gateway also requires Go 1.22+.

## Setup

```bash
cd ~/ai-projects/llm-gateway-scheduler
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

All Python tools share `.venv`; activate it in later sessions. Install the runtime
and GGUF at the paths in `gateway.config.json`; see [models](README.models.md).

## Start and chat

```bash
python cli.py models start local-llm
python cli.py --stream "Explain the KV cache."
```

Direct chat uses **8080**, defaults to 128 output tokens, and starts a new
conversation per command. For browser/API chat on **8000**, follow
[gateway setup](gateway/README.md).

## Stop and clean up

```bash
python cli.py models stop local-llm
# For servers launched with the legacy --start-server flag:
python stop_server.py --dry-run
python stop_server.py
```

Use Ctrl+C for foreground servers. `deactivate` exits only the Python environment.
Keep `.runtime` and its locks; remove logs only after stopping their server.

## Guides

| Topic | Guide |
| --- | --- |
| Request flow and code map | [Architecture](README.gateway.md) |
| Register, start and stop models | [Models](README.models.md) |
| Browser chat, API keys and quotas | [Gateway](gateway/README.md) |
| Streaming / routing / queues | [Streaming](gateway/README.streaming.md) · [Routing](gateway/README.routing.md) · [Scheduler](gateway/README.scheduler.md) |
| Accounting and reusable Postman tests | [Lifecycle](gateway/README.lifecycle.md) |
| Benchmarks and measurements | [Commands](benchmarks/README.md) · [Metrics](README.metrics.md) |
| Terraform and Kubernetes | [Deployment](infra/README.md) |

Local requirements-page instructions: `README.local.md` (Git-ignored).

## Tests

```bash
python -m unittest discover -s tests -v
(cd gateway && go test -race -count=1 ./... && go vet ./...)
```

Live-model and deployment checks are opt-in; see their guides.
