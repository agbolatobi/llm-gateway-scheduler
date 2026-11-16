# AIPerf benchmarks

Run from the project root with the [shared venv](../README.md#setup) active.

## Run

```bash
python -m benchmarks.run --dry-run
python -m benchmarks.run --pdf
python -m benchmarks.run --compare --pdf
python -m benchmarks.run --case engineering_short_64 --repetitions 5
```

Run prepares inputs, starts/reuses llama.cpp through the CLI, runs streaming
AIPerf at temperature 0 and leaves the server running. Defaults: three engineering
prompts × caps 64/256, three measured repetitions and one warm-up per case.
Prompt targets are not actual token counts.

| Flag | Purpose |
| --- | --- |
| `--input PATH`, `--case ID` | Select workload/cases |
| `--repetitions N`, `--concurrency N` | Load; does not change server slots |
| `--prompt-cache off\|reuse` | Default off verifies no cross-request reuse |
| `--compare` | Controlled comparisons and acceptance checks |
| `--base-url URL`, `--model ALIAS` | Runtime destination |
| `--no-start-server` | Require a running server |
| `--memory-interval SECONDS` | Default 1; range 0.1–60 |
| `--output NEW_DIRECTORY` | New run location; existing paths refused |

Use `--help` for other options. `python -m benchmarks.prepare` only inspects/prepares inputs.

## Results

Runs print their directory under ignored `benchmarks/artifacts/`.

| Files | Contents |
| --- | --- |
| `run.json`, `runtime.json`, `inputs/` | Settings, runtime evidence and workload mapping |
| `CASE/request_metrics.jsonl`, `.csv` | Per-request metrics and unavailable reasons |
| `CASE/latency_summary.json` | Timing, outcomes and cache statistics |
| `CASE/memory_samples.jsonl`, `memory_summary.json` | Samples, peaks and coverage |
| `CASE/runtime_snapshot.json`, `profile_export*`, `logs/` | Runtime fingerprints and native evidence |
| `comparison.json/.csv/.md`, `report.pdf` | Optional comparison/PDF |

Analyze `benchmark_phase == "profiling"`; join by request ID. Raw exports can
contain text/headers; compact reports omit them. Failures preserve partial data;
missing measurements stay unavailable. E2E requires `[DONE]`, unlike native
AIPerf content timing. [Metric definitions](../README.metrics.md).

Generate reports from saved runs:

```bash
python -m benchmarks.report benchmarks/artifacts/RUN_ID
python -m benchmarks.compare benchmarks/artifacts/RUN_ID
```

Existing outputs are refused; PDF supports `--output NEW_PATH`. PDF failure keeps
data but returns failure. Dry/prepared runs cannot produce PDFs; comparisons
require captured runtime evidence.

MEAS-02 requires concurrency 1, warm-ups, ≥3 measured repeats, cache off and fixed
runtime/sampling. Compare input lengths at one cap, then caps at one prompt.
`--compare` verifies samples, backend counters/cache and unchanged snapshots;
a successful inference run can still fail comparison acceptance.

## Experiments and tests

```bash
python -m benchmarks.experiments all --dry-run
python -m benchmarks.experiments all
```

Use `concurrency` or `context` instead of `all` to select a sweep. Each condition
starts/stops its own server on **8081**, refuses occupied ports and checks memory
headroom. Normal 8080 is untouched; extra model memory is needed.

[Settings](experiments.json): concurrency 1/2/4 with four slots; total contexts
4096/8192, with per-slot limits verified. RSS differs from logged KV allocation.
Keep [baseline.json](baseline.json) frozen; it does not change CLI defaults.
Results: [MEAS-02](MEAS-02.md), [MEAS-03](MEAS-03.md), [MEAS-04](MEAS-04.md).

[Unit tests](../README.md#tests). Optional installed-AIPerf smoke test, without a model:

```bash
RUN_AIPERF_SMOKE=1 python -m unittest discover -s tests -p test_aiperf_smoke.py -v
```

[AIPerf CLI reference](https://github.com/ai-dynamo/aiperf/blob/v0.13.0/docs/cli-options.md).
