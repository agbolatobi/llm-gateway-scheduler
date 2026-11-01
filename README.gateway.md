# Gateway and scheduling plan

Gateway and scheduling plan. The [Go API skeleton](gateway/README.md) now provides health, chat validation, and placeholder endpoints. Everything else below remains planned: authentication, quota, registry, router, scheduler, and reverse-proxy modules within one service.

## Request flow

1. Validate the basic request and authenticate its API key.
2. Check user/global request limits and exhausted token budgets.
3. Look up the requested model; validate access, tokens, and model quotas.
4. Atomically reserve user/global/model budgets and admit the request.
5. Route to an eligible backend.
6. Queue until execution slots are available, then proxy the response.
7. Settle usage and release resources safely on every exit path.

Exact token reservations require the model's tokenizer and chat template. Early quota checks still happen **before model lookup**.

## Gateway milestone

- **GATE-01:** Define a small OpenAI-shaped chat API and health endpoint.
- **GATE-02:** Issue, save, verify, and revoke user-linked API keys; the gateway stores only hashes.
- **GATE-03:** Check user/global quotas before model lookup.
- **GATE-04:** Register models, validate requests, and reserve quota budgets.
- **GATE-05:** Route each model to a healthy, configured backend.
- **GATE-06:** Proxy replies and streams; record timings and handle failures.
- **GATE-07:** Enforce active limits, cancel safely, settle usage, and log outcomes.

Before the scheduler exists, reject work exceeding active limits.

### Local API-key reuse

Save the client key in `~/.config/llm-gateway-scheduler/credentials.json`, outside the repository. Create the directory with mode `0700` and the file with mode `0600`; the client loads the key automatically across sessions. This file contains the secret, not a hash, and is not encrypted. Never log it. The gateway keeps only a verification hash; revoking a key stops it working even if the client still has it saved.

## Scheduling milestone

- **QUEUE-01:** Bound waiting work and global/per-runtime execution slots.
- **QUEUE-02:** Offer FIFO or interactive priority with aging; log dispatch decisions.
- **QUEUE-03:** Remove cancelled/expired jobs and settle reservations once.
- **QUEUE-04:** Replay ordering, capacity, and cleanup tests; smoke-test a real model.

The router chooses **where**; the scheduler chooses **when**. Waiting jobs reserve quota budgets, not execution slots. Model aliases sharing a runtime share its active limit. The backend owns token batching.

## Keep the scope small

Use local key administration, a configuration-based model registry, and an in-memory queue. Test two model routes with fake backends; keep scheduler benchmarks on the frozen single-model baseline. Restart drops waiting work. No distributed queue, dashboard, custom batching, or full OpenAI API implementation.

Completion criteria: [gateway tasks](project-requirements.html#gateway) and [scheduling tasks](project-requirements.html#schedule).
