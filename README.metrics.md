# Metrics

Prefill processes input; decode generates output. KV caching reuses attention
state; quantization reduces model precision/size. Concurrency counts outstanding
requests; batching groups work inside the engine.

| Metric | Purpose |
| --- | --- |
| Actual input/output tokens | Work performed, including chat-template overhead |
| First-text TTFT | Send to first nonempty content delta: visible response delay |
| Completion E2E | Send to SSE `[DONE]`: full completion wait |
| AIPerf latency / ITL | Tool-defined content timing/decode pace, not exact token timestamps |
| Backend prompt/decode time | Engine work, separate from client latency |
| New/cached input tokens | Distinguish full prefill from prefix reuse |
| Throughput | Successful tokens or completions per elapsed run second |
| Outcomes / sample counts | Reliability and sample-size context |
| Sampled peak RAM/VRAM | Process memory demand and coverage |
| Startup time/memory | Fresh process to readiness; filesystem cache may remain warm |
| Queue wait / selection | Waiting duration, policy, reason and permit order |

## Reading results

- Timings use monotonic clocks. TTFT ignores role/usage events; nonempty whitespace
  counts. A finish reason is not `[DONE]`; incomplete E2E stays unavailable.
- Summaries exclude warm-ups but retain failure counts. Missing usage is not guessed.
  Throughput divides successful tokens by first-send-to-last-terminal time, not
  the sum of per-request rates. Native AIPerf boundaries can differ.
- Cross-request cache reuse defaults off and is verified; within-request KV caching remains.
- Memory sampling defaults to 1s and may miss spikes/short requests. Request samples
  must fall wholly within send-to-DONE; case peaks exclude startup. Concurrent
  requests share process readings, not attributable allocations.
- RSS is not KV allocation; process VRAM is not total device usage. WSL process
  VRAM is unsupported. Missing values: JSON null, blank CSV, PDF N/A with reasons;
  partial coverage retains sample/error counts.
- Compare fixed model, hardware, runtime, context, slots, cache policy and workload.
  Repeat at least three times and report failures/counts; smoke runs cannot establish tails.

Not yet evaluated: per-class fairness/oldest wait, queue-depth history, goodput,
and completion-based TPOT: `(E2E - TTFT) / (output_tokens - 1)` (unavailable below
2 output tokens). Detailed utilization/KV occupancy is optional.

[Benchmark commands](benchmarks/README.md) · [Gateway timing](gateway/README.streaming.md).
References: [AIPerf](https://github.com/ai-dynamo/aiperf/blob/main/docs/metrics-reference.md),
[llama.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md),
[Linux memory](https://www.kernel.org/doc/html/latest/filesystems/proc.html),
[NVIDIA WSL limitations](https://docs.nvidia.com/cuda/wsl-user-guide/index.html#features-not-yet-supported).
