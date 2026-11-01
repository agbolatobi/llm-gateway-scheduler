# Benchmarking with AIPerf

Run commands from the project root with the shared `.venv` active.
See [setup](../README.md#setup) for dependencies and
[server controls](../README.md#stop-and-clean-up) for shutdown.

## Run

```bash
# Preview inputs and commands without inference
python -m benchmarks.run --dry-run

# Main baseline: warm model, no cross-request prompt reuse
python -m benchmarks.run --pdf

# Also verify experimental controls and generate prompt/output comparisons
python -m benchmarks.run --compare --pdf

# Run one prompt/output-cap combination
python -m benchmarks.run --case engineering_short_64 --repetitions 5

# Optional cached-reuse comparison; hits are measured, not guaranteed
python -m benchmarks.run --prompt-cache reuse --case engineering_short_64 --pdf
```

The runner prepares inputs, starts or reuses the model through the existing CLI,
and runs AIPerf with streaming and temperature 0. Cases run sequentially; the
server stays running afterward. Omit `--pdf` for JSONL/CSV/JSON results only.
Default `--prompt-cache off` sends `cache_prompt: false` on warm-up and measured
requests. This prevents reuse across requests, not KV caching during generation.
One warm-up per case keeps initialization outside measured requests.

Useful flags:

| Flag | Purpose |
| --- | --- |
| `--input PATH` | Use another workload JSON file. |
| `--case ID` | Select a case; repeat for several. |
| `--repetitions N`, `--concurrency N` | Override workload settings; concurrency does not change server slots. |
| `--warmup-requests N` | Warm-ups per case; default 1, or 0 to disable. |
| `--prompt-cache off\|reuse` | Default off verifies uncached input processing; reuse permits prefix hits. |
| `--compare` | Require sequential warmed uncached controls; export comparison JSON/CSV/Markdown and check acceptance. |
| `--base-url URL`, `--model ALIAS` | Change the server address or served alias. |
| `--no-start-server` | Skip CLI readiness/startup; requires a running server. |
| `--startup-timeout N`, `--request-timeout N` | Timeouts in seconds; defaults 120 and 180. |
| `--output PATH` | Choose a new run directory; existing directories are refused. |
| `--memory-interval SECONDS` | Server memory polling, default 1; range 0.1-60. Shorter intervals add overhead. |

See `python -m benchmarks.run --help` for all options.

## Concurrency and context experiments

```bash
# Preview both experiments without starting a server or writing files
python -m benchmarks.experiments all --dry-run

# Run both; or replace all with concurrency or context
python -m benchmarks.experiments all
```

[experiments.json](experiments.json) controls the short case, eight measured
requests plus one warm-up per condition, CPU threads, batching and safety limits.
The concurrency sweep tests 1/2/4 outstanding requests with four fixed server
slots and total context 4,096. The context experiment tests total limits
4,096/8,192 at concurrency one; per-slot limits are verified separately.
Both use temperature zero and verified uncached inputs. Prompt token budgets
include the model's chat template, using its own tokenizer before inference.

Each condition starts a fresh server on dedicated port 8081 using the same
lifecycle helper as the CLI, then stops only that owned server. An occupied port
is refused; the normal server on 8080 is untouched. Startup and idle RSS samples,
backend allocation logs, runtime snapshots and normal AIPerf exports are retained.
An additional server needs free RAM; host headroom and process RSS are checked
while profiling. Failures/limits stop the sweep and preserve partial evidence.

Outputs are ignored under `benchmarks/artifacts/experiments-<id>/`:
`experiment.json`, `summary.{json,csv,md}`, per-condition evidence, and
`frozen_config.json` after both sweeps pass. Throughput uses total successful
output tokens divided by first-send-to-last-DONE wall time, excluding warm-ups.
The selected concurrency is only the highest tested within the declared limits
for this small CPU workload—not a general capacity guarantee.

Use `--config PATH`, `--input PATH`, `--output NEW_DIRECTORY`, or `--port N`
(not 8080). Edit limits before running, not after seeing results. Context tests
compare sampled used/resident RSS with separately logged KV allocation; these
are different counters. Similar RSS does not imply unchanged KV allocation.

Completed results: [concurrency](MEAS-03.md) and [context-memory](MEAS-04.md).
For later scheduler comparisons, keep [baseline.json](baseline.json) fixed.
Recheck its selected context with
`python -m benchmarks.experiments concurrency --config benchmarks/baseline.json`.
The ordinary CLI defaults are unchanged; the frozen profile is an explicit experiment configuration.

## Workload

[benchmark_prompts.json](../benchmark_prompts.json) contains three engineering
prompts with output caps of 64 and 256: six cases. Defaults are three repetitions
and concurrency one, producing 18 measured requests plus six warm-ups.

Targets of 128/512/2,048 input tokens are unverified; check actual backend counts
and the context budget. Output caps do not guarantee generated lengths.

Preparation preserves prompt text and writes AIPerf `mooncake_trace` JSONL plus
a case/request manifest. It runs automatically. For input inspection only,
`python -m benchmarks.prepare` writes to `benchmarks/generated/<id>/`.

## Results

Each run prints its directory under `benchmarks/artifacts/<run-id>/`.

| Location | Contents |
| --- | --- |
| `run.json` | Settings, commands, source hash and run/case status. |
| `runtime.json` | Actual local server properties, safe launch settings, model/runtime SHA-256 and hardware. |
| `<case-id>/runtime_snapshot.json` | Before/after configuration fingerprints and process identity checks. |
| `comparison.{json,csv,md}` | MEAS-02 acceptance checks, measured tables and observed comparison write-up when requested. |
| `inputs/` | Converted inputs and case/repetition mapping. |
| `<case-id>/request_metrics.{jsonl,csv}` | Compact per-request timings, status, server token usage, request-window memory peaks and unavailable reasons. |
| `<case-id>/latency_summary.json` | Successful measured timing/backend statistics, outcome counts and observed cache verification. |
| `<case-id>/memory_samples.jsonl` | Timestamped server RSS/VRAM samples, including unavailable counters. |
| `<case-id>/memory_summary.json` | Case baseline/peaks, PID, interval, scope, sample/error counts and counter sources. |
| `<case-id>/profile_export.jsonl` | Native per-request AIPerf metrics. |
| `<case-id>/profile_export_raw.jsonl` | Raw timestamped requests/responses. |
| `<case-id>/profile_export_aiperf.{json,csv}` | Native aggregate statistics. |
| `<case-id>/logs/` | AIPerf diagnostics. |
| `report.pdf` | Summary, when requested. |

Brace notation groups filenames; for example, `request_metrics.{jsonl,csv}`
means both `request_metrics.jsonl` and `request_metrics.csv`.

Select measured compact records with `benchmark_phase == "profiling"`;
native records use `metadata.benchmark_phase`. Warm-ups are `"warmup"`.
Records join by unique `x_request_id` (compact `request_id`); conversation IDs
map to cases/repetitions but may repeat across phases. File order is not stable.

A failed command, missing data or unsuccessful measured request fails the run
and retains available results. If AIPerf skips raw aggregation after failures,
the processor reads `raw_records/raw_records_*.jsonl`; absent raw data means
unverified completion and no successful E2E sample.

In `off` mode, a successful request also needs backend counters proving zero
cached tokens and `new + cached == actual input`. Cache hits, missing counters,
or inconsistent counts fail the experiment while retaining inference results.
`reuse` allows hits without requiring them; inspect `cache_verification`.
Legacy runs have an unspecified policy and are not relabeled or rewritten.

Generated files are ignored by Git. Raw exports include prompt/response bodies
and request headers and add storage/processing overhead; keep export settings
consistent across comparisons. Compact exports omit prompt/response bodies.

Memory monitoring starts after CLI readiness and stops after each AIPerf case,
including setup, warm-ups and teardown. It tracks the verified local project
server, not the client, child processes or whole host. Case baseline is the first
observation, not guaranteed idle; all peaks are sampled, so spikes may be missed.

Request fields `peak_ram_mib` / `peak_vram_mib` use samples wholly within send
through `[DONE]`. Short requests may have no samples; missing completion leaves
the window unavailable. Concurrent requests share process readings, not isolated
allocations. JSON null / blank CSV / PDF N/A always carry a reason. CPU-only
VRAM is `cpu_only_server`; WSL process VRAM is unsupported. Missing/ambiguous
local processes and remote servers are also marked unavailable, without failing
otherwise successful inference. Samples are retained on interruption or failure.

## PDF from a saved run

Replace `RUN_ID` with the printed run directory name:

```bash
python -m benchmarks.report benchmarks/artifacts/RUN_ID
```

This creates `report.pdf` without inference. Existing PDFs are refused; choose
a new filename with `--output benchmarks/artifacts/RUN_ID/report-copy.pdf`.

Reports show settings, coverage, errors, sampled memory and timing/token/throughput
statistics. Partial runs are labeled; missing values are N/A. Prepared/dry runs
cannot be exported. `--dry-run --pdf` creates no PDF.
The cross-case backend table shows P50 actual input/output counts, newly
processed/cached input counts, and backend prompt/decode times.

If automatic PDF export fails, benchmark data remains completed, the command
exits 1, and `run.json` records the PDF error. Retry the export command after
fixing the problem. Standalone export leaves the historical manifest unchanged.

## Timing and interpretation

Definitions and remaining work are in the [metrics guide](../README.metrics.md).
Project E2E timing verifies `[DONE]`; native AIPerf latency uses the last content
response. Failed requests have null E2E latency and a reason; missing token usage
also stays null. Completion percentiles exclude failures and warm-ups and use
linear interpolation at `(n - 1) * p`.

The default offline `builtin` tokenizer is internal plumbing; measured token
counts come from server usage. Older metrics-only runs cannot verify completion
retroactively. Three repetitions are a smoke workload, not reliable tail evidence.

Backend fields in compact JSONL/CSV come from llama.cpp's final SSE `timings`:

| Field | Backend counter |
| --- | --- |
| `backend_prompt_ms`, `backend_decode_ms` | `prompt_ms`, `predicted_ms` |
| `backend_prompt_tokens`, `cached_input_tokens` | `prompt_n`, `cache_n` |
| `backend_output_tokens` | `predicted_n` |

Missing, invalid or conflicting counters stay null with a reason. Phase summaries
exclude warm-ups and failed requests, and retain each metric's sample count.
Backend prompt time is not client TTFT; generation time is not full E2E.

## MEAS-02 procedure

Use the six-case default with concurrency 1, three or more repetitions, warm-ups
enabled, and `--prompt-cache off`. Compare short/medium/long at cap 64, then
64/256 at one fixed prompt. Check actual lengths and cache verification.
Keep runtime, sampling, context, threads, hardware and memory interval fixed.

Save a short comparison note with the run path, exact model/quantization,
runtime version and actual server configuration (especially when reusing a
server), cache policy, observations and limitations. Explain prompt versus
generation timings; do not label all TTFT as prefill. Existing cached runs remain
useful as a separately labeled baseline.

`--compare` generates this evidence automatically and requires complete backend
counters, successful warm-ups, at least three measured requests per case, both
controlled comparison groups, and unchanged verified runtime snapshots. Inference
may complete while acceptance remains incomplete; inspect `comparison.md` and
`run.json`. Snapshots cannot detect temporary changes between observations;
unexposed thread defaults are not guessed. Only allowlisted launch/environment
settings are recorded, not API keys or arbitrary environment variables.

For a saved run with captured runtime evidence, export comparisons without inference:
`python -m benchmarks.compare benchmarks/artifacts/RUN_ID`. Existing comparison
files are refused. Historical runs without snapshots remain incomplete; their
configuration is never retroactively invented.

The [initial baseline](MEAS-02.md) and [concurrency close-out](MEAS-03.md) record
measured results and interpretation; raw run artifacts remain ignored by Git.

## Tests

Use the [main test command](../README.md#tests) for unit tests. To exercise
installed AIPerf against a fake local server, including delayed/missing completion,
timeouts and PDF export:

```bash
RUN_AIPERF_SMOKE=1 python -m unittest discover -s tests -p test_aiperf_smoke.py -v
```

No model is loaded. Test artifacts are temporary.

Reference: [AIPerf 0.13.0 custom prompts](https://github.com/ai-dynamo/aiperf/blob/v0.13.0/docs/tutorials/custom-prompt-benchmarking.md)
and [CLI options](https://github.com/ai-dynamo/aiperf/blob/v0.13.0/docs/cli-options.md).
