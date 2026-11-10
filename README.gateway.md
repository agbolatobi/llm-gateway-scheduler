# Gateway and scheduling plan

The [Go gateway](gateway/README.md) provides health, authentication, a model registry, tokenizer-based budgets, quotas, JSON/SSE forwarding, actual usage settlement, and execution limits. [Model commands](README.models.md) manage registered runtimes. Quota allowances scale with model registrations; execution capacity has explicit limits. Queueing remains planned.

[GATE-05 routing](gateway/README.routing.md) and [GATE-06 response delivery](gateway/README.streaming.md)
are connected to the public API. Failure and streaming tests use fake backends.

## Request flow

1. Authenticate its API key and validate the basic request.
2. Check user/global request limits and exhausted token budgets.
3. Look up the requested model; validate tokens and model quotas. Any valid API key can use any registered model.
4. Atomically reserve user/global/model budgets and admit the request.
5. Route to an eligible backend.
6. Acquire execution capacity or reject busy work, then proxy the response. Queueing comes next.
7. Settle usage and release resources safely on every exit path.

Exact token reservations require the model's tokenizer and chat template. Early quota checks still happen **before model lookup**.

## Gateway milestone

- **GATE-01:** Define a small OpenAI-shaped chat API and health endpoint.
- **GATE-02:** Issue, save, verify, and revoke user-linked API keys; the gateway stores only hashes.
- **GATE-03:** Check user/global quotas before model lookup.
- **GATE-04:** Share model/quota configuration; manage model processes explicitly; validate identity and token budgets; warn about unregistered servers without auto-registering them.
- **GATE-05:** Route each model to a healthy, configured backend.
- **GATE-06:** Proxy replies and streams; record timings and handle failures.
- **GATE-07:** Enforce active limits, cancel safely, settle usage, and log outcomes.

Busy work is rejected before dispatch. See [GATE-07 lifecycle and Postman tests](gateway/README.lifecycle.md).

### Local API-key reuse

Implemented: `credentials.json` in `~/.config/llm-gateway-scheduler/` saves client keys outside the repository (directory `0700`, file `0600`). The Python client loads the selected key only for its configured gateway origin. The server reads hashes from the separate `api-keys.json`; revocation takes effect on new requests immediately. See the [key commands and Postman setup](gateway/README.md#manage-saved-keys).

## Scheduling milestone

- **QUEUE-01:** Bound waiting work and global/per-runtime execution slots.
- **QUEUE-02:** Offer FIFO or interactive priority with aging; log dispatch decisions.
- **QUEUE-03:** Remove cancelled/expired jobs and settle reservations once.
- **QUEUE-04:** Replay ordering, capacity, and cleanup tests; smoke-test a real model.

The router chooses **where**; the scheduler chooses **when**. Waiting jobs reserve quota budgets, not execution slots. Model aliases sharing a runtime share its active limit. The backend owns token batching.

## Keep the scope small

Use local key administration, a configuration-based model registry, and an in-memory queue. Test two model routes with fake backends; keep scheduler benchmarks on the frozen single-model baseline. Restart drops waiting work. No distributed queue, dashboard, custom batching, or full OpenAI API implementation.

Completion criteria: [gateway tasks](project-requirements.html#gateway) and [scheduling tasks](project-requirements.html#schedule).
