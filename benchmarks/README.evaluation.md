# Local and GKE evaluation

Run from the project root. Requires Python 3.11+, Go 1.22+ and an **idle,
single-slot llama.cpp runtime** with `/slots` enabled. GKE also needs `kubectl`
access to read Pods/ConfigMaps and exec into the `runtime` container.

```bash
# Inspect the workload without inference.
python -m benchmarks.evaluate --dry-run

# Local: start your registered model with the existing CLI first, then run in WSL.
LLAMA_ARG_SLOT_SAVE_PATH=/tmp python cli.py models start local-llm
python -m benchmarks.evaluate --target local

# GKE: use an existing deployment; choose a Pod if several are Ready.
python -m benchmarks.evaluate --target gke --context YOUR_CONTEXT

# Scheduling comparison: longer deadlines isolate scheduling from timeout loss.
python -m benchmarks.evaluate --target local --trace contended --aging-seconds 60 --queue-wait 180 --gateway-timeout 240 --timeout 250

# Short check; not enough samples for performance claims.
python -m benchmarks.evaluate --trace contended --repetitions 1

# Verify saved evidence and regenerate reports offline.
python -m benchmarks.evaluate --report-only benchmarks/artifacts/eval-RUN_ID
```

Use `--config PATH`, `--model ID`, `--workload PATH`, `--prompts PATH`,
`--trace NAME`, `--mode direct|fifo|interactive_priority_aging`, `--output NEW_DIR`
or `--help`. GKE reads the selected Pod's ConfigMap unless `--config` overrides it.
It requires explicit `--context`; namespace defaults to `llm-gateway`.

`--aging-seconds`, `--queue-wait` and `--gateway-timeout` affect only temporary
evaluation gateways and are saved in the plan. Keep queue wait < gateway timeout
< client `--timeout`. Live configuration remains unchanged. A longer deadline
improves completion opportunity, not generation speed.

Gateway classes are sent in JSON `request_class`. Reports verify each sent job's
class against gateway evidence; mismatches invalidate policy comparisons and
return a failing exit status. Older runs created with `X-Request-Class` treated
all jobs as interactive and must not support mixed-class priority claims.

Cache erasure also requires llama.cpp's `--slot-save-path` option. The local
command above enables it through the environment; stop an already running model
first with `python cli.py models stop local-llm`, then start it again. The deployment
manifest enables it with `/tmp`; redeploy existing GKE workloads to pick it up.

## What runs

The command builds one Go worker. Locally it runs beside llama.cpp; on GKE it
runs in the runtime container, then removes its temporary remote files. It tests
direct access and temporary FIFO/priority gateways using the real gateway code
from this checkout. Live gateway settings, keys and quota counters are unchanged.
No API key import is needed: each temporary gateway uses an unexported random key.

[evaluation.json](evaluation.json) defines fixed arrivals using cases from
`benchmark_prompts.json`. All modes receive the same jobs, with three repeats
and rotated mode order. Gateway quotas remain enabled and reset per trial; all
jobs share one evaluation user. Rejections count as offered work.

Keep other traffic off the model throughout the run. The worker checks idle
state but cannot lock out external clients. It verifies model identity, counts
templated input tokens, erases the slot cache, and runs an identical 8-token
warmup before each trial. Cache reuse **within** a trial remains enabled by the
runtime; this is not the existing AIPerf runner's uncached baseline.

The default quiet trace has 30-second spacing; contended arrivals are 0.1 seconds
apart. Reports flag unexpected queueing/no contention. Adjust the saved workload
to suit your hardware, then repeat **all modes**. Defaults are exploratory;
increase jobs/repeats before drawing p95 conclusions.

## Report and audit

Each run creates a new ignored `benchmarks/artifacts/eval-*` directory:

| File | Purpose |
| --- | --- |
| `report.html` | Readable five-metric report; browser Print → PDF |
| `summary.json`, `summary.csv`, `requests.csv` | Per-trial/class summaries and request results |
| `events.jsonl` | Monotonic timings, raw SSE, gateway queue records, warmups and runtime snapshots |
| `plan.json`, `workload.json`, `prompts.json` | Exact inputs and effective configuration |
| `provenance.json`, `source.zip`, `worker`, logs | Source/build hashes, worker binary, environment and failures |
| `manifest.json` | SHA-256 integrity checks for saved files |

The five metrics and reasons are in [README.metrics.md](../README.metrics.md).
Failures stay in completion counts; missing usage/queue records become N/A.
Aborted runs retain partial reports. Hashes detect edits, not malicious rewriting
of both evidence and manifest. Keep the folder intact; raw streams contain text.

These are **runtime-local scheduler measurements**. They exclude ingress/TLS,
browser latency and the deployed gateway image. GKE runs use the local checkout's
gateway code. Compare policies within each environment; hardware differences
prevent attributing local-versus-GKE differences to scheduling. The small finite
trace does not prove starvation freedom. AIPerf diagnostics remain separate.

## Tests

```bash
python -m unittest discover -s tests -p 'test_evaluation*.py' -v
RUN_EVALUATION_SMOKE=1 python -m unittest discover -s tests -p test_evaluation_smoke.py -v
(cd gateway && go test ./internal/evaluation ./internal/api)
```

The smoke test uses a simulated runtime and real gateway code. It checks complete
and failed runs plus offline report regeneration; it is not a live-model/GKE result.
