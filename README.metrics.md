# Evaluation metrics

The final evaluation uses **five headline metrics**. Repeat the same workload
against direct access, FIFO, and priority with aging, separately in local WSL
and GKE. Run the [cross-environment evaluator](benchmarks/README.evaluation.md)
with `python -m benchmarks.evaluate --target local|gke`.

| Metric | Definition | Why it matters |
| --- | --- | --- |
| p95 first-text TTFT | Client send to first nonempty content delta, for successful streams | Shows how long slower requests wait before users see a reply. |
| p95 end-to-end latency | Client send to SSE `[DONE]`, for successful streams | Shows how long users wait for a complete answer, including queueing and generation. |
| Output throughput (tokens/s) | Actual output tokens from successful requests / full measured run seconds | Measures useful capacity; reveals when lower latency costs total output. |
| Completion rate (%) | Successfully completed requests / all offered requests × 100 | Prevents rejection or failure from making latency look artificially good. |
| Maximum observed background queue wait | Largest gateway queue wait among background jobs that entered the queue | Shows the cost of interactive priority for background jobs; finite observations cannot prove no starvation. |

p95 means 95% of the measured values are at or below this value. Show TTFT,
end-to-end latency, and completion rate separately for interactive/background
requests. Show throughput for the whole workload. Outcome/sample counts are
supporting evidence, not additional headline metrics.

## Measurement rules

- Replay quiet and contended timed traces; arrivals must not wait for previous
  replies. Repeat each case at least three times. Exclude warmups and flag small
  samples; a short smoke test is not reliable tail-latency evidence.
- Use monotonic client timestamps. Role/usage events do not start TTFT; nonempty
  whitespace does. Success requires a valid completed stream, including `[DONE]`
  and clean transport completion. Failures are excluded from latency, never from
  offered-request counts. A finish reason alone is not success.
- Throughput uses first scheduled arrival to the last measured request's terminal
  outcome, including rejection/failure time. Use actual usage, never output caps
  or summed per-request rates. Missing usage makes token throughput unavailable.
- Count rejected, failed, cancelled, timed-out, unfinished, and unsent offered work
  explicitly. Unfinished work prevents a final throughput claim until resolved.
- Join gateway `queue_wait_ms` logs by request ID. Include cancelled/timed-out
  background jobs that entered the queue. A still-waiting job has a lower-bound
  wait; missing logs mean N/A, not zero. No background work means N/A. Direct
  llama.cpp access has no gateway queue: report this metric N/A for that baseline.

## Local and GKE comparisons

Keep model/quantization, prompts, caps, warmup, cache policy, quotas, context,
slots, and arrival trace fixed within each environment. Record hardware, runtime,
policy, and client location with the results. Claim scheduler effects from the
within-environment comparisons, not from differences between local and cloud hardware.

Locally, use the same WSL client for backend and gateway paths. On GKE, run both
paths from the same Pod network context; the model stays bound to loopback.
Port-forward/browser runs are a separate client-experience check because their
network/tunnel latency differs. Collect logs locally or with `kubectl logs`.

## Existing diagnostic benchmarks

[Benchmark tooling](benchmarks/README.md) also captures input tokens, backend
prefill/decode timings, cache counters, AIPerf latency/ITL, startup, and sampled
RAM/VRAM. Keep these for troubleshooting, outside the final five-row summary.
The AIPerf runner targets llama.cpp; the separate evaluation command compares
direct access and temporary FIFO/priority gateways locally or inside a GKE Pod.

Prefill processes input; decode generates output. Token usage includes chat-template
overhead. Cross-request cache reuse defaults off in existing benchmarks, while
within-request KV caching remains. AIPerf content timing differs from `[DONE]` E2E.

Memory samples may miss peaks; concurrent requests share process readings.
RSS is not KV allocation, and process VRAM is not device-wide usage. WSL process
VRAM is unsupported. Missing data stays JSON null, blank CSV or PDF N/A with a reason.

[Gateway timings](gateway/README.streaming.md) · [Scheduling](gateway/README.scheduler.md).
References: [AIPerf](https://github.com/ai-dynamo/aiperf/blob/main/docs/metrics-reference.md),
[llama.cpp](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md).
