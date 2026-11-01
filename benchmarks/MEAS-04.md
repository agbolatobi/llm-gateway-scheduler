# MEAS-04: context and memory

Completed 28 October 2026. The fresh context-only retry passed both conditions:
16/16 measured requests and 2/2 warm-ups, with unchanged recorded controls
apart from the intended context limit and process identity. A separate
concurrency-four confirmation passed another 8/8 requests and one warm-up.

## Method

CPU-only Qwen2.5 1.5B Instruct Q4_K_M and llama.cpp `b11669-620d2225c`.
Both context conditions used the exact short prompt (138 input tokens), cap 64
(64 actual output tokens), concurrency one, four server slots, six inference
and six batch threads, logical/physical batches 512/128, continuous batching on,
unified KV off, temperature zero, and verified cross-request cache reuse off.

Each condition used a fresh owned server. Process RSS was sampled every 0.2
seconds during startup, a two-second idle interval, and the benchmark case.
Startup allocation lines, raw samples and before/after runtime snapshots are saved.

## Measurements

Memory is MiB. Idle RSS is the first post-readiness idle observation; case peak
includes setup, warm-up and teardown but excludes model startup. All peaks are sampled.

| Total context | Context/slot | Logged KV allocation | Logged compute buffer | Idle RSS | Startup peak RSS | Case peak RSS |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 4,096 | 1,024 | 112.00 | 17.13 | 1,841.961 | 1,841.961 | 1,863.164 |
| 8,192 | 2,048 | 224.00 | 17.38 | 1,954.004 | 1,953.996 | 1,975.293 |

Doubling total context doubled the backend-reported KV buffer allocation, even
though the input/output workload stayed short. Idle RSS rose about 112.04 MiB
and case peak about 112.13 MiB. This is consistent with larger startup KV
storage in this configuration, rather than memory depending only on tokens in
the current request. It does not establish behavior for every backend or mode.

Logged allocated buffer sizes and measured used/resident RSS are different
counters. RSS includes model mappings, shared/file-backed pages and work buffers;
it is not a direct KV-size measurement or exclusive physical-memory total.
VRAM remains null with `cpu_only_server`, not zero. Sampling may miss peaks and
the earliest startup allocations. Idle and startup peaks can differ slightly
because they are sampled in separate windows.

P50 E2E was 4.371 versus 5.503 seconds. A single fixed-order comparison cannot
attribute that difference solely to context size: CPU frequency, background
load and cache state were not isolated. Launch-to-readiness was 9.761 versus
1.679 seconds; filesystem caches were not flushed, so this is not evidence that
the larger context starts faster. Eight requests do not establish reliable tails.

## Frozen configuration

Use [baseline.json](baseline.json) for later scheduler comparison experiments:
total context **8,192**, four slots (**2,048 per slot**), selected maximum client
concurrency **four**, and the fixed CPU/batch/cache settings above.

The larger limit accommodates all existing cases: the model's chat-template and
tokenizer report a maximum input of 1,593 tokens, so the largest cap requires
1,593 + 256 = 1,849 tokens, below 2,048. Token-budget compatibility does not prove
latency or reliability for every case under contention.

The selected profile was separately checked at concurrency four on the short
case: eight successes, 17.847 output tokens/s, P50 E2E 14.322 seconds, maximum
E2E 14.635 seconds, and peak RSS 1,992.906 MiB. All declared limits passed.
This is an initial CPU baseline, not a production or mixed-workload guarantee.

Keep this configuration fixed across future policy comparisons. If changing
model, runtime, context, threads, batching or cache policy, establish a new
baseline rather than mixing results. The JSON includes observed identity hashes;
check them against runtime evidence. It does not change normal CLI startup defaults.

## Reproduce and inspect

```bash
python -m benchmarks.experiments context
# Recheck concurrency with the chosen larger-context configuration
python -m benchmarks.experiments concurrency --config benchmarks/baseline.json
```

- [Context retry: JSON/CSV/Markdown summary and raw evidence](artifacts/meas04-20261028-retry/)
- [Joint profile confirmation](artifacts/meas04-20261028-confirmation/confirmation.json)
- [All six tokenizer budgets](artifacts/meas04-20261028-confirmation/workload_budgets.json)

The earlier WSL-interrupted context attempt is retained but excluded. This retry
uses corrected startup/idle scope labels; its raw sample values were not edited.
Generated evidence remains Git-ignored; this note and frozen profile are in the codebase.
