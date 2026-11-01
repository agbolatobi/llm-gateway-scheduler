# MEAS-03: concurrency and memory tradeoff

Closed out 28 October 2026. All three saved concurrency conditions passed:
24/24 measured requests and 3/3 excluded warm-ups, with verified uncached input
and unchanged recorded settings within each condition.

## Controls

- CPU-only Qwen2.5 1.5B Instruct Q4_K_M; llama.cpp `b11669-620d2225c`.
- Ubuntu WSL2, Intel Core i5-1235U; six inference and six batch threads.
- Four fixed server slots, total context 4,096; verified context per slot 1,024.
- Logical batch 512, physical batch 128; continuous batching on, unified KV off.
- Exact `engineering_short_64` prompt: actual input 138 tokens and output 64.
- Eight measured requests and one warm-up per condition; temperature 0,
  cross-request prompt reuse off, memory sampling every 0.2 seconds.
- Fresh server per condition. Model/runtime hashes, sampling defaults, hardware
  and recorded launch settings match across conditions, excluding process identity.

## Results

TTFT and E2E below are P50 seconds. RSS is the maximum observed across the case,
including setup, warm-up and teardown but excluding model startup.

| Client concurrency | Observed overlap | Output tokens/s | TTFT, s | E2E, s | Peak RSS, MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 1 | 13.878 | 2.077 | 4.638 | 1,863.281 |
| 2 | 2 | 16.317 | 3.828 | 8.026 | 1,870.668 |
| 4 | 4 | 16.979 | 7.275 | 15.055 | 1,879.941 |

Throughput is total successful output tokens divided by elapsed time from the
first measured send to the last measured DONE. It excludes warm-ups, startup
and AIPerf setup; it is not a sum of per-request token rates.

## Decision and interpretation

Select **four as the highest tested reliable concurrency for this short CPU
workload**, under the limits declared before testing: maximum TTFT 20 seconds,
maximum E2E 30 seconds, and process RSS 3,072 MiB. The worst observed E2E at
concurrency four was 16.064 seconds. All conditions stayed within those limits.

This is a capacity ceiling, not the best interactive setting. Moving from one
to four increased throughput about 22%, but median E2E became about 3.25 times
longer. Concurrency one favors responsiveness; four trades longer per-request
waits for a modest aggregate throughput gain.

With server slots/context allocation held fixed rather than resized with client
concurrency, peak RSS increased only about 16.66 MiB. RSS measures
**used/resident process memory**, including shared and file-backed pages; it is
not allocator-reserved KV memory, exclusive physical memory, or a per-request
allocation. VRAM is unavailable with reason `cpu_only_server`, not zero.

Eight requests per condition and one fixed-order sweep are an exploratory
baseline, not reliable tail-latency or long-duration stability evidence. CPU
frequency, background load and workload changes can affect results. These
measurements do not establish GPU performance or a universal concurrency limit.

## Evidence and reproduction

```bash
python -m benchmarks.experiments concurrency
```

Saved evidence: [concurrency 1](artifacts/meas03-04-20261027-v2/concurrency-c1-ctx4096/),
[concurrency 2](artifacts/meas03-04-20261027-v2/concurrency-c2-ctx4096/), and
[concurrency 4](artifacts/meas03-04-20261027-v2/concurrency-c4-ctx4096/).
Each contains request JSONL/CSV, memory samples, backend counters and runtime snapshots.
The [combined summary](artifacts/meas03-04-20261027-v2/summary.json) still has its
last saved suite state because WSL stalled later during the context experiment.
That interruption does not invalidate these three completed concurrency stages.
Original artifacts are retained and Git-ignored; this close-out is in the codebase.
