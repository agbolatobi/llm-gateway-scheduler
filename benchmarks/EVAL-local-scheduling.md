# Local scheduling evaluation

Run: `eval-20261123T053957Z-1cdcf374`. Three repeats of the same six-request burst per mode: **54/54 completed**. Gateway class audit passed.

| Metric | Direct | FIFO | Priority + aging |
| --- | ---: | ---: | ---: |
| Interactive p95 first text (s) | 62.60 | 59.28 | 26.31 |
| Interactive p95 full reply (s) | 65.60 | 62.35 | 29.81 |
| Output throughput (tokens/s) | 14.75 | 15.38 | 14.86 |
| Completion (%) | 100.00 | 100.00 | 100.00 |
| Max background queue wait (s) | N/A | 39.73 | 52.25 |

Priority reduced observed interactive p95 first-text wait by **55.6%** and full-reply time by **52.2%** versus FIFO. Background wait increased, and throughput decreased **3.4%**.

Settings: aging 60 s; queue deadline 180 s; gateway deadline 240 s; client deadline 250 s. These override only temporary evaluation gateways. Live settings are unchanged. Longer deadlines enable completion; they do not make generation faster.

P95 pools nine successful interactive requests per mode. Throughput divides all successful output tokens by the sum of measured trial durations. Warmups are excluded. This is exploratory evidence from a short burst workload, not a stable tail-latency or starvation-freedom claim. No request needed aging promotion in this run.

The benchmark now sends JSON `request_class` and checks it against gateway logs. Previous reports used an ignored header and are not valid mixed-class priority comparisons.

```bash
python -m benchmarks.evaluate --target local --trace contended --repetitions 3 --aging-seconds 60 --queue-wait 180 --gateway-timeout 240 --timeout 250
```

[Report](artifacts/eval-20261123T053957Z-1cdcf374/report.html) · [Pooled results](artifacts/eval-20261123T053957Z-1cdcf374/policy-comparison.json). Raw events, configuration, source snapshot and checksums are retained in the ignored run directory.
