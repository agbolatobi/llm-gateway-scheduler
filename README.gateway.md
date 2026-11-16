# Gateway architecture

```text
API key → schema validation → user/global quotas → model/token budget
→ backend selection → bounded queue → execution → response → settlement
```

Routing chooses **where**; scheduling chooses **when**. Waiting requests reserve
tokens, not execution slots. Aliases share quotas/capacity; llama.cpp owns batching.

| Code | Responsibility |
| --- | --- |
| `client/`, `cli.py` | Chat, saved keys, model management |
| `gateway/cmd/` | Server startup, key commands, reload, container utilities |
| `gateway/internal/api/` | Admission, forwarding, streaming, cleanup and logs |
| `gateway/internal/auth/`, `quota/` | Identity and budgets |
| `gateway/internal/registry/`, `routing/` | Models, tokenizer and destination |
| `gateway/internal/scheduler/`, `capacity/` | Waiting order and execution permits |
| `gateway/internal/chatui/` | Browser chat |
| `benchmarks/` | AIPerf runs and reports |
| `infra/`, `deploy/`, `docker/` | Terraform, Kubernetes and images |

Start reading `gateway/internal/api/admission.go`, `proxy.go`, then
`request_cleanup.go`. Queue handling lives in `gateway/internal/scheduler/`.
State is process-local: restart clears counters and waiting work.

Details: [gateway](gateway/README.md), [routing](gateway/README.routing.md),
[streaming](gateway/README.streaming.md), [cleanup](gateway/README.lifecycle.md),
[scheduler](gateway/README.scheduler.md). [Local requirements](project-requirements.html).
