# Deploy on GCP

**Implemented and locally tested; not yet deployed/verified on GCP.**
One zonal GKE Standard cluster runs one Pod with gateway/UI + llama.cpp.
CPU is default; GPU is optional. Nodes, disks, NAT and storage are billable.

```text
Browser → port-forward → gateway :8000 → llama.cpp :8080 (loopback)
```

## 1. Prepare

From the project root in Linux/WSL: Python 3.12+, Terraform 1.13.5+, Google Cloud
CLI, `gke-gcloud-auth-plugin`, kubectl and Docker. Use a billed project with IAM
rights for APIs, GKE, networking, storage and service accounts. Check compute/GPU
quotas. Kubernetes requires 1.33+ and an fsGroup-capable StorageClass.

```bash
python3 deploy/scripts/setup.py
gcloud auth login
gcloud auth application-default login
gcloud auth application-default set-quota-project YOUR_PROJECT_ID
```

Fill **`infra/variables/`** (Git-ignored). `authorized_cidrs` is your public IP/VPN
range, usually `/32`. Credentials stay in `~/.config/gcloud/`, never Terraform inputs.

## 2. Create infrastructure

Enable Service Usage/Cloud Storage first. Review plans before applying.
Bootstrap creates the protected state bucket, then migrates its local state:

```bash
terraform -chdir=infra/bootstrap init
terraform -chdir=infra/bootstrap plan -var-file=../variables/bootstrap.tfvars -out=../variables/bootstrap.tfplan
terraform -chdir=infra/bootstrap apply ../variables/bootstrap.tfplan
cp infra/bootstrap/backend.tf.example infra/bootstrap/backend.tf
terraform -chdir=infra/bootstrap init -migrate-state -backend-config=../variables/bootstrap-backend.hcl

terraform -chdir=infra/gcp init -backend-config=../variables/backend.hcl
terraform -chdir=infra/gcp plan -var-file=../variables/test.tfvars -out=../variables/test.tfplan
terraform -chdir=infra/gcp apply ../variables/test.tfplan
terraform -chdir=infra/gcp output
```

Keep bootstrap/GKE state prefixes separate. Copy outputs into `workload.env`, run
the printed `connect_command`, and get `YOUR_CONTEXT` with `kubectl config current-context`.

## 3. Publish and deploy

Set `IMAGE_REPOSITORY` and `MODEL_URI` in `workload.env`. Upload only permitted
GGUF weights; use a unique object path per version.

```bash
gcloud auth configure-docker YOUR_REGION-docker.pkg.dev
python3 deploy/scripts/images.py --tag test-001 --push
# Copy the printed image digests into workload.env.
python3 deploy/scripts/upload_model.py /absolute/path/to/model.gguf
# Copy the printed MODEL_SHA256 into workload.env.
python3 deploy/scripts/render.py
# Review infra/variables/rendered/deployment.yaml.
python3 deploy/scripts/manage.py --context YOUR_CONTEXT deploy
python3 deploy/scripts/manage.py --context YOUR_CONTEXT port-forward
```

In another terminal:

```bash
python3 deploy/scripts/manage.py --context YOUR_CONTEXT keys create --user YOUR_NAME
```

Open the printed chat URL and paste the token. Model downloads use Workload
Identity and checksum verification; `infra/variables/rendered/release.json`
records image/model/manifest hashes.

If 8000 is occupied, use `port-forward --local-port 8001`, set
`CHAT_URL=http://127.0.0.1:8001/chat/`, and add `--base-url http://127.0.0.1:8001`
to smoke tests.

## 4. Verify and maintain

```bash
python3 deploy/scripts/smoke.py --context YOUR_CONTEXT
python3 deploy/scripts/manage.py --context YOUR_CONTEXT keys list
python3 deploy/scripts/manage.py --context YOUR_CONTEXT keys revoke --id KEY_ID
python3 deploy/scripts/manage.py --context YOUR_CONTEXT restart
```

Smoke tests create/revoke a temporary key; `--restart` also verifies persistence.
Rerun `deploy` for config changes. Keys persist on a private PVC; restart resets
quotas/queues. [Operations, key management and portability](OPERATIONS.md) · [Tests](TESTING.md).

## 5. Clean up

```bash
# Retain model/key disks:
python3 deploy/scripts/manage.py --context YOUR_CONTEXT cleanup
# Alternative: delete the dedicated namespace and its PVCs:
python3 deploy/scripts/manage.py --context YOUR_CONTEXT cleanup --delete-storage

terraform -chdir=infra/gcp plan -destroy -var-file=../variables/test.tfvars -out=../variables/destroy.tfplan
terraform -chdir=infra/gcp apply ../variables/destroy.tfplan
```

Back up keys before cluster deletion; retained PVCs are not backups. Check disk
reclaim policies. Empty the model bucket explicitly if destroying it; Terraform
will not empty it. If enabled, disable/apply cluster `deletion_protection` first.
The separate state bucket remains protected.

## Scope

Keep one replica: quotas/scheduling are process-local. Public HTTPS, budgets/alerts,
backups and HA remain follow-up work. There is no public model endpoint/load balancer.

References: [GKE Terraform](https://docs.cloud.google.com/kubernetes-engine/docs/terraform),
[Workload Identity](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity),
[GCS state](https://developer.hashicorp.com/terraform/language/backend/gcs),
[Kubernetes sidecars](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/).
