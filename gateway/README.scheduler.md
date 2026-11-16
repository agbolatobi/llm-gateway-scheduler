# Bounded scheduler

FIFO is the default; waiting state is process-local. Restart loses queued work.
No preemption, distributed queue or token batching.

## Configuration

Under `gateway` in `gateway.config.json`:

```json
"scheduler": {
  "policy": "fifo",
  "max_waiting_requests": 16,
  "max_waiting_per_runtime": 8,
  "max_queue_wait_seconds": 30,
  "aging_threshold_seconds": 5
}
```

Omitted fields use these defaults; `max_waiting_requests: 0` disables waiting.
Switch policy to `interactive_priority_aging` and run `./.runtime/gateway reload`.
Reload preserves arrivals/deadlines; lower limits do not evict existing jobs.
Referenced models cannot be removed/retargeted.

## Selection and cleanup

- **FIFO:** oldest request with available backend capacity.
- **Priority:** oldest eligible aged request, then interactive, then background.
- `request_class` accepts `interactive` (default) or `background`.
- Busy backends do not block other eligible work; aging cannot bypass capacity.

```text
Validate/reserve → wait/select → capacity + idle check → dispatch → settle
                     └─ overflow / cancellation / timeout
```

Waiting jobs reserve tokens, not execution slots; aliases share limits. The HTTP
request stays open. Pre-dispatch cancellation/expiry refunds tokens once, but
request counts remain charged. The earlier queue/total deadline wins. Shutdown
rejects/refunds waiting jobs before draining active work. [Settlement](README.lifecycle.md).

| Error | Meaning |
| --- | --- |
| 503 queue_full | Waiting limit reached |
| 504 queue_timeout / request_timeout | Deadline reached |
| 503 capacity_exceeded | Waiting disabled; runtime occupied |
| 503 capacity_unconfirmed | Backend quarantined |
| 503 scheduler_closed | Shutdown |

Logs expose `queue_wait_ms`, `scheduler_policy`, `dispatch_reason` and
`selection_sequence` (permit order, zero if none). Completion order can differ;
`dispatch_ms` is relative to that request's receipt. Code: `internal/scheduler/`.

## Tests

From `gateway/`: `go test -race -count=1 ./...`.
In the [Postman lab](README.lifecycle.md), reset → choose policy → hold backend →
start a stream → queue requests in other tabs. The third waiting request exceeds
the two-slot preset. Release with normal mode; cancel a waiting tab to test refunds.

Optional burst/cancellation test with the model running and idle:

```bash
cd gateway
GATEWAY_LIVE_CONFIG="$PWD/../gateway.config.json" go test -race -count=1 -run '^Test(QueueRunningModelBurst|LifecycleRunningModel)$' -v ./internal/api
```
