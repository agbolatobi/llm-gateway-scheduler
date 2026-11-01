# Inference metrics

The benchmark currently records server token usage, first-text and completion
timings, request outcomes, sampled server memory, and native AIPerf metrics.
Dedicated experiments also record startup time and startup/idle memory.
Gateway/scheduler metrics remain planned.
See the [benchmark guide](benchmarks/README.md) for commands and output files.

## Concepts

Prefill processes input tokens; decode generates output tokens. The KV cache
reuses attention state at a memory cost. Quantization reduces numerical
precision and model size. Concurrency is outstanding requests; batching is how
the engine groups their work.

## Current measurements

| Metric | Definition | Why it matters |
| --- | --- | --- |
| Input/output tokens | Actual server usage, including input chat-template overhead. | Describes work performed; output caps are not actual lengths. |
| First-text TTFT, ms | Request send to first non-empty `delta.content`. | Measures when visible output begins. |
| Completion E2E, ms | Request send to the first SSE `[DONE]`. | Includes the full wait through the completion signal. |
| Backend prompt/decode time, ms | llama.cpp `prompt_ms` / `predicted_ms`, separately from client latency. | Separates input processing from generation. |
| New/cached input tokens | Backend `prompt_n` / `cache_n`; full input remains server usage. | Distinguishes full prefill from reused prefix work. |
| AIPerf request latency, ms | Request send to last content-bearing response. | Preserves the tool's separate timing boundary. |
| Native inter-token latency | AIPerf's derived decode pace using content timing and token counts. | Summarizes generation pace; not individual token timestamps. |
| Streaming gaps, ms | Gaps between content chunks in native metrics. | Reveals pauses; chunks may contain multiple tokens. |
| Output/request throughput | Successful output tokens or completions per run second. | Measures serving capacity. |
| Outcomes | Success, error, cancellation, incomplete stream and other unavailable-data states. | Keeps failures visible instead of improving results by dropping them. |
| Peak server RAM, MiB | Maximum sampled process RSS within a request window and across each case. | Shows resident memory demand, including shared/file-backed pages. |
| Peak server VRAM, MiB | Sampled per-PID NVIDIA used memory, summed across GPUs when supported. | Shows reported GPU memory demand without confusing it with device-wide totals. |
| Experiment startup, s | Fresh server launch through readiness; filesystem caches are not flushed. | Separates process startup from warm requests, without claiming cold-disk startup. |

Timing uses AIPerf's original monotonic `perf_counter_ns` values. Role metadata,
reasoning-only fields and usage events do not start first-text TTFT; non-empty
whitespace content does. A finish reason alone does not verify `[DONE]`.
Missing completion or failure gives a null E2E value with a reason; missing
usage is unavailable, never an estimated or zero token count.

Completion statistics include successful profiling requests only. Failed and
warm-up counts remain available. Native AIPerf statistics keep their own
definitions; do not assume all latency or throughput fields share a boundary.

The baseline uses a warm model with cross-request prompt reuse off. Successful
measured requests must report zero cached tokens and new + cached = actual input;
otherwise the experiment fails verification and retains its results. Optional
`--prompt-cache reuse` permits hits but does not guarantee them. This policy does
not disable the within-request KV cache. Backend counters have null/reason markers
when unavailable and their own successful-measured sample counts.

Memory polling defaults to 1 second; use `--memory-interval 0.1` for shorter
requests at higher overhead. Samples use the same host monotonic clock as request
timings. A request peak includes only samples collected wholly between send and
`[DONE]`; missing completion or no in-window sample means unavailable. Concurrent
requests share process observations: these are not attributable allocations.

Case peaks include AIPerf setup, warm-ups and teardown but exclude model startup.
The baseline is a pre-case observation, not guaranteed idle. RSS is approximate,
not virtual size or KV-cache allocation; VRAM is reported used memory, not
allocator-reserved memory. Sampling can miss spikes. Record the interval and keep
it fixed for comparisons.

Unavailable values are JSON `null`, blank CSV cells, and PDF N/A, with reasons
such as `cpu_only_server`, `wsl_process_vram_unsupported`,
`project_server_not_found`, or `no_samples_in_request_window`. Partial counter
coverage preserves observed peaks and failure counts. WSL process VRAM is marked
unsupported; device-wide GPU usage is never substituted. Other GPU vendors and
remote/unrecognized servers are unsupported by this initial collector.

## Planned measurements

| Metric | What to record | Why |
| --- | --- | --- |
| Completion-based TPOT, ms/token | `(E2E - TTFT) / (output tokens - 1)`; unavailable below two output tokens. | Gives an approximate client-observed generation pace. |
| Gateway queue wait | Admission to backend dispatch. | Isolates scheduler delay. |
| Queue depth / active requests | Waiting and dispatched requests over time. | Shows overload and admission behavior. |
| Per-class latency, completion and fairness | Interactive/background outcomes, oldest/max wait and unfinished counts. | Detects prioritization tradeoffs and starvation. |
| Goodput | Successful requests meeting fixed latency limits per run second, plus their fraction of attempts. | Measures useful capacity. |

Choose latency targets from the baseline, then freeze them for scheduler
comparisons. CPU/GPU utilization and detailed KV-cache occupancy remain optional
diagnostics. Server RSS is not KV-cache size.

## Fair comparisons

- Fix hardware/WSL limits, model/quantization, runtime, context, threads,
  batching, sampling, workload and cache policy. Separate cold starts.
- Vary input length, output cap and concurrency independently; compare actual
  lengths. Try concurrency 1, 2 and 4 within safe resource limits.
- Exclude warm-ups from measured summaries. Preserve every attempted outcome;
  report sample counts and failure rates alongside successful latency statistics.
- Repeat comparisons at least three times. Report median/p95/max, and use p99
  only with enough observations; three requests cannot establish tail behavior.
- Define run duration from first send to last terminal outcome, excluding warm-up.
  Divide total successful tokens by that duration; do not sum per-request rates.
- TTFT includes queueing and transport. TPOT and chunk gaps are approximations,
  not pure engine execution or exact per-token timings.
- Keep request IDs, timings, usage, outcomes and available finish reasons.
  Omit prompt/response bodies from compact reports.

Sources: [NVIDIA inference concepts](https://developer.nvidia.com/blog/mastering-llm-techniques-inference-optimization/),
[GenAI-Perf definitions](https://docs.nvidia.com/deeplearning/triton-inference-server/user-guide/docs/perf_benchmark/genai-perf-README.html#metrics),
[AIPerf metrics](https://github.com/ai-dynamo/aiperf/blob/main/docs/metrics-reference.md),
[vLLM monitoring](https://docs.vllm.ai/en/latest/design/metrics/), and
[llama.cpp monitoring](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md).
Memory sources: [Linux proc counters](https://www.kernel.org/doc/html/latest/filesystems/proc.html),
[NVIDIA SMI](https://docs.nvidia.com/deploy/nvidia-smi/index.html), and
[NVIDIA WSL limitations](https://docs.nvidia.com/cuda/wsl-user-guide/index.html#features-not-yet-supported).
