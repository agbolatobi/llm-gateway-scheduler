# Infrastructure checks

Run from the project root. These checks do not create GCP resources:

```bash
terraform fmt -check -recursive infra
terraform -chdir=infra/bootstrap init -backend=false
terraform -chdir=infra/bootstrap validate
terraform -chdir=infra/gcp init -backend=false
terraform -chdir=infra/gcp validate
terraform -chdir=infra/gcp test
python3 -m unittest discover -s deploy/tests -p 'test_*.py' -v
(cd gateway && go test -race ./... && go vet ./...)
```

Offline Terraform validation is intended before backend setup. For a configured
working directory, keep its existing backend; do not use test initialization to
replace it. Terraform tests use a mocked provider and do not validate real IAM,
quota availability or GKE provisioning.

Optional schema validation with kubeconform:

```bash
kubectl kustomize deploy/base | kubeconform -strict -summary -kubernetes-version 1.34.1
kubectl kustomize deploy/overlays/gke | kubeconform -strict -summary -kubernetes-version 1.34.1
```

Real CPU model tests use isolated containers and a temporary loopback port:

```bash
docker build -f docker/gateway.Dockerfile -t llm-gateway-infra-test:local .
docker build -f docker/runtime.Dockerfile -t llm-runtime-infra-test:local .
python3 deploy/tests/containers.py --model /absolute/path/to/model.gguf
```

The container test requires Linux root for temporary-volume ownership (run in
this project's root WSL environment, or use sudo for the test command). It checks
UI/auth, JSON/SSE, cancellation, queue overflow, persisted keys, revocation and
both shutdown orders, then removes its own containers and temporary files.
It does not stop existing project services. Tests assume a small CPU GGUF that
can finish 64 tokens within 20 seconds and 256 tokens within the request timeout.

Validated locally: both Terraform roots, three mocked plan scenarios, CPU/GPU/base
rendering, Kubernetes 1.34 schemas, Go race tests/vet, both image builds, and the
real Qwen2.5 1.5B Q4 container tests. GCP apply, Workload Identity downloads on GKE,
GPU execution and cluster recovery still require a real deployment.
