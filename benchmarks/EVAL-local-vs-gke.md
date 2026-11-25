# Local and GKE scheduling results

Benchmark session: November 23, 2026 (America/Edmonton). New GKE run compared with the saved local baseline. Same model checksum, benchmark worker, runtime build, prompts, output caps, arrival trace and scheduler/deadline settings. Three repeats per mode in each environment; **108 measured requests across 18 trials**.

## LOCAL

| Metric | Direct | FIFO | Priority + aging |
| --- | ---: | ---: | ---: |
| Interactive p95 first text (s) | 62.60 | 59.28 | 26.31 |
| Interactive p95 full reply (s) | 65.60 | 62.35 | 29.81 |
| Output throughput (tokens/s) | 14.75 | 15.38 | 14.86 |
| Completion (%) | 100.00 | 100.00 | 100.00 |
| Max background queue wait (s) | N/A | 39.73 | 52.25 |

Priority versus FIFO: observed interactive first-text p95 **55.6% lower**; full-reply p95 **52.2% lower**; throughput change **-3.4%**.

## GKE

| Metric | Direct | FIFO | Priority + aging |
| --- | ---: | ---: | ---: |
| Interactive p95 first text (s) | 98.76 | 92.21 | 37.92 |
| Interactive p95 full reply (s) | 103.96 | 97.35 | 43.10 |
| Output throughput (tokens/s) | 9.67 | 9.87 | 9.91 |
| Completion (%) | 100.00 | 100.00 | 100.00 |
| Max background queue wait (s) | N/A | 61.35 | 74.10 |

Priority versus FIFO: observed interactive first-text p95 **58.9% lower**; full-reply p95 **55.7% lower**; throughput change **+0.4%**.

## Interpretation

Local: 12 visible CPUs, 4 model threads. GKE: one e2-standard-4 node, 2 model threads, runtime CPU request 2 / limit 3. Cross-environment timing differences are not scheduler speedups.

These are small-sample, runtime-local measurements, not public-ingress or production SLO results. P95 uses nine interactive samples per mode. Queue deadline 180 s; gateway deadline 240 s; client deadline 250 s; aging threshold 60 s. Cache is erased and identically warmed before each trial; reuse is permitted within a trial. Earlier mislabeled runs are excluded.

## Evidence

- Local: `benchmarks/artifacts/eval-20261123T053957Z-1cdcf374`
- GKE: `benchmarks/artifacts/eval-20261123-gke-corrected`
- Session logs, comparison JSON, backup checksum and teardown inventory: `benchmarks/artifacts/cloud-20261123/`.
