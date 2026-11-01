# MEAS-02: uncached inference baseline

Completed 27 October 2026. The controlled comparison passed with 18/18 measured
requests and 6/6 warm-ups successful, zero measured cache hits, and matching
runtime fingerprints before/after all six cases.

## Method and runtime

- CPU-only llama.cpp `b11669-620d2225c`; Qwen2.5 1.5B Instruct, Q4_K_M.
- Ubuntu WSL2; Intel Core i5-1235U, 12 visible logical CPUs, approximately
  7,761 MiB visible RAM.
- Context 4,096; one server slot; concurrency 1; temperature 0.
- Three measured repetitions and one excluded warm-up per case. The model
  remained loaded; every request used `cache_prompt: false`.
- Inference/batch threads used runtime defaults: no explicit thread launch or
  selected environment override was present. The resolved count was not exposed
  by `/props`; it is not guessed. The same process/configuration was retained.
- Sampling defaults, request overrides, chat-template hash, model/runtime file
  hashes, CPU affinity and process identity are recorded in the runtime evidence.
  The server's default temperature was overridden to 0 by benchmark requests.

Model SHA-256:
`6a1a2eb6d15622bf3c96857206351ba97e1af16c30d7a74ee38970e434e9407e`.

Runtime binary SHA-256:
`3f2b53013c2eb596c8aca87e4740240a41f95bcebf72e61084d6d5ba0db761cc`.

These identify observed local files; no unverified upstream model revision is claimed.

## Results

All times below are P50 seconds from three successful measured requests per case.
Input/output counts are actual server usage. Every input token was newly
processed; every cached-input count was zero.

| Prompt / cap | Input | Output | Backend prompt | Backend generation | TTFT | E2E |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Short / 64 | 138 | 64 | 2.070 | 2.527 | 2.093 | 4.633 |
| Short / 256 | 138 | 256 | 2.124 | 10.508 | 2.159 | 12.667 |
| Medium / 64 | 436 | 64 | 6.605 | 2.682 | 6.615 | 9.240 |
| Medium / 256 | 436 | 256 | 5.071 | 9.269 | 5.081 | 14.781 |
| Long / 64 | 1,593 | 64 | 24.867 | 3.126 | 24.887 | 28.013 |
| Long / 256 | 1,593 | 256 | 30.195 | 16.332 | 30.225 | 47.569 |

## Interpretation

At a fixed cap of 64, increasing input from 138 to 1,593 tokens increased backend
prompt processing from 2.070 to 24.867 seconds. Output remained 64 tokens, so
output-length differences do not explain that prompt-time increase. Prompt
processing accounted for most of the long/64 request duration.

For the exact same short prompt, raising the cap from 64 to 256 produced 64 and
256 actual output tokens. Backend generation rose from 2.527 to 10.508 seconds,
while prompt processing remained similar at 2.070 and 2.124 seconds. The larger
generation workload explains most of the observed E2E increase in that comparison.

Client TTFT is not pure prefill: it also includes transport, queueing, first-token
work and other overhead. Backend phase times are reported separately, not
calculated by subtracting client timings. Independently calculated medians need
not sum to the E2E median.

The medium/long prompt times also vary between caps despite unchanged input
lengths. This small fixed-order experiment cannot isolate CPU frequency,
background activity, cache hierarchy or other sources of variation. Decode time
also changes with prompt size at fixed output; do not attribute all E2E changes
to prefill. No precise linear-scaling or causal-isolation claim is made.

Three repetitions satisfy this initial task, but do not establish reliable tail
latency. They are per-case repetitions, not three independent full-run trials.
Prompt content varies with input length, and these CPU results do not describe GPU
serving. Model warm-up is separate from cross-request prefix reuse.

## Evidence and reproduction

Run from the project root with the shared environment active:

```bash
python -m benchmarks.run --compare --pdf
```

Saved evidence under [meas02-20261027-uncached](artifacts/meas02-20261027-uncached/):

- [Generated comparison and acceptance checks](artifacts/meas02-20261027-uncached/comparison.md)
- [Machine-readable comparison](artifacts/meas02-20261027-uncached/comparison.json)
- [Runtime configuration](artifacts/meas02-20261027-uncached/runtime.json)
- [PDF report, readable copy](artifacts/meas02-20261027-uncached/report-readable.pdf)
- Per-case request JSONL/CSV, native/raw AIPerf exports, and runtime snapshots.

Raw/generated artifacts remain Git-ignored; this concise write-up is in the
codebase. Runtime snapshots cannot detect temporary changes between observations.
The original automatic PDF is retained; the readable copy changes formatting only.
