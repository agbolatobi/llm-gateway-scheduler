# Deployment details

| Location | Responsibility |
| --- | --- |
| `infra/bootstrap` | Protected GCS state bucket and operator access |
| `infra/gcp` | APIs, private nodes, VPC/NAT, bounded CPU/GPU pools, registry, model bucket and IAM |
| `docker` | Pinned gateway/UI and llama.cpp images; no model weights or keys |
| `deploy/base` | Portable resources; existing model volume required |
| `deploy/overlays/gke` | Workload Identity and checksum-verified GCS download |
| `deploy/scripts` | Personal setup, image/model publication, rendering, administration and smoke tests |

Terraform owns cloud resources. Kubectl/Kustomize owns the namespace and workload.
The scripts require an explicit Kubernetes context for changes. Personal inputs,
rendered manifests and release records live under ignored `infra/variables/`.

## Existing clusters

Use Kubernetes 1.33+, an fsGroup-capable StorageClass and a reachable registry.
Set `TARGET=base`, your own immutable image references and `STORAGE_CLASS` in the
input file. Supply `/models/model.gguf` on the `gateway-models` PVC before the
workload starts; the checksum verifier waits for a correct file. You can create
the namespace/PVCs from the generated base first, then populate that PVC using
an administrator-controlled copy Pod or volume snapshot. No Google credentials
or model downloader are required. Then run the same `render` and `manage deploy` commands.
Base GPU deployments require NVIDIA drivers/device plugin and suitable node placement.

## GPU mode

Set `enable_gpu=true` in Terraform inputs and apply the reviewed plan. This adds
one fixed GPU node while retaining the CPU pool for system workloads. Choose a
supported accelerator/machine/zone combination before applying.
Set `GPU=true` in workload inputs, rebuild/publish the runtime image, update its
digest and deploy. The GKE renderer selects the GPU pool, requests one GPU and
offloads model layers. The optional CUDA image is pinned in `docker/images.lock.json`.
The GPU path is rendered/schema-tested; it has not been run on GPU hardware.

## Startup and shutdown

Containers run as UID/GID 10001 with read-only roots and no Linux capabilities.
A volume initializer creates `/data/keys` as 0700; the gateway writes key files as
0600. Only administrators with Pod exec/storage access can manage the saved keys.
The reload socket is also inside this private directory. Back up this volume securely.

The model is a native sidecar. Model download and checksum verification happen
first; its startup probe waits for weights to load before the gateway starts.
Runtime readiness checks `/health`; its liveness probe checks the server process.
Gateway probes check `/health`. Pod readiness requires both containers to be ready.

`--listen 0.0.0.0:8000 --allow-remote-listen` exposes only the gateway socket.
Backend URLs remain restricted to loopback. Deployment shutdown gives the gateway
130 seconds to drain (request timeout: 120 seconds), then stops the runtime;
the Pod grace period is 160 seconds. Keep these values aligned when changing limits.

## Troubleshooting

```bash
kubectl --context YOUR_CONTEXT -n llm-gateway get pods,pvc
kubectl --context YOUR_CONTEXT -n llm-gateway describe pod POD_NAME
kubectl --context YOUR_CONTEXT -n llm-gateway logs deployment/gateway -c gateway
kubectl --context YOUR_CONTEXT -n llm-gateway logs deployment/gateway -c runtime
kubectl --context YOUR_CONTEXT -n llm-gateway logs deployment/gateway -c download-model
```

Pending: check node resources, storage and GPU quota. Download errors: check the
service-account annotation, IAM binding, URI and checksum. Forbidden control-plane
access: update your public CIDR and apply. Reconnect port-forward after Pod replacement.
A queue limit or token quota rejection is an application policy, not a Kubernetes failure.
