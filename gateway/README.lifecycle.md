# Accounting and Postman tests

One llama.cpp slot per runtime (`parallel: 1`); global active capacity defaults
to one. Aliases share permits. Direct backend calls bypass gateway limits.

## Accounting and cleanup

| Outcome | Token charge | Capacity |
| --- | --- | --- |
| Not dispatched / failed dial | Refund reservation | Release unused permit |
| Complete reply, valid usage | Actual input + output | Verify idle |
| Missing/invalid/partial usage | Full reservation | Verify idle or quarantine |
| Ambiguous connected dispatch | Full reservation | Quarantine; idle alone is insufficient |

Usage must contain integer input/output/total counts consistent with tokenized
input, cap and sum. SSE requires `[DONE]`. Cached input remains logically charged;
accepted request counts are not refunded.

`preparedRequest.finish()` cleans up once, polling `/slots` for up to 2s independently
of client cancellation. Pending-idle quarantine can recover on later probes.
Unknown-dispatch quarantine requires restarting the backend, then the gateway
with all backends idle. Reload does not clear quarantine; restart resets counters.
Logs record IDs, timing, charges and capacity, excluding keys and conversation text.
[Waiting/overflow behavior](README.scheduler.md).

## Reusable Postman lab

```bash
.venv/bin/python gateway/tests/gate07_lab.py
```

Uses **127.0.0.1:8005** and `.runtime/gate07-lab/`. Import its
`postman.collection.json` and `postman.environment.json`; select
**Gateway tests — reusable**. Startup prints file links/Windows paths.
Missing exports are recreated; unchanged environments are retained. Reimport only
when URLs/keys or saved requests change, or after deleting them in Postman.
Local files do not sync automatically. Exports contain test secrets; keep them local.

| Endpoint | Purpose |
| --- | --- |
| GET `/tests/status` | Export links, settings, latest report |
| POST `/tests/run` | Lifecycle and Go queue checks |
| POST `/tests/reset` | Reset state; keep keys/scheduler settings |
| GET/POST `/tests/scheduler` | Read/change queue policy |
| GET `/v1/models`, POST `/v1/chat/completions` | Manual requests |
| POST `/__test/control`, GET `/__test/state` | Fake backend modes/state |
| GET `/tests/requests` | Latest 50 settlement records |

All require the saved Bearer key. Manual `X-Test-User` values: actual (default),
stream, fallback, busy, cancel, timeout. Use separate tabs for held streams and
competing requests; release with normal mode. Stubborn tests quarantine; stall
tests timeout. Disable waiting for legacy busy-rejection checks.

Run/reset returns 409 while manual requests are active; controls stay usable.
Automatic tests temporarily disable waiting, then restore settings. Failures
return 500 with expected/actual evidence and redacted keys; results persist in
`latest-report.json`. The lab owns only its private gateway/fake backends.

## Replay

```bash
.venv/bin/python -m unittest discover -s gateway/tests -p 'test_lab_*.py' -v
.venv/bin/python gateway/tests/gate07_lab.py --verify
```

Verification uses disposable state; queue checks require Go. Restart the lab for code changes.
