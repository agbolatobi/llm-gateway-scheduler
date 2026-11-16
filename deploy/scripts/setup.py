"""Create ignored personal inputs without overwriting existing values."""
from common import ROOT, VARIABLES, executable

VARIABLES.mkdir(parents=True, exist_ok=True)
for example in (ROOT / "infra/examples").glob("*.example"):
    destination = VARIABLES / example.name.removesuffix(".example")
    if not destination.exists():
        destination.write_bytes(example.read_bytes())
        destination.chmod(0o600)
        print(f"Created {destination.relative_to(ROOT)}")
for tool in ("terraform", "gcloud", "kubectl", "docker"):
    try:
        print(f"{tool}: {executable(tool)}")
    except SystemExit as error:
        print(error)
print("Also install gke-gcloud-auth-plugin. Fill in infra/variables before deployment.")
