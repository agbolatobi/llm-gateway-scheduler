"""Deploy and administer one gateway in an explicitly selected Kubernetes context."""
import argparse
from common import DEFAULT_ENV, executable, read_settings, run
from render import render

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--env", default=DEFAULT_ENV)
parser.add_argument("--context", required=True, help="Exact kubectl context; prevents accidental use of another cluster")
commands = parser.add_subparsers(dest="command", required=True)
for action in ("deploy", "reload", "restart"):
    commands.add_parser(action)
forward = commands.add_parser("port-forward")
forward.add_argument("--local-port", type=int, default=8000)
keys = commands.add_parser("keys")
keys.add_argument("action", choices=("create", "list", "show", "revoke"))
keys.add_argument("--user")
keys.add_argument("--id")
cleanup = commands.add_parser("cleanup")
cleanup.add_argument("--delete-storage", action="store_true", help="Delete the dedicated namespace, including keys and models")
arguments = parser.parse_args()
settings = read_settings(arguments.env)
kubectl = [executable("kubectl"), "--context", arguments.context, "--namespace", "llm-gateway"]
if arguments.command == "deploy":
    manifest = render(settings)
    run([*kubectl, "apply", "-f", manifest])
    run([*kubectl, "rollout", "status", "deployment/gateway", "--timeout=20m"])
    print("Ready. Run port-forward, then keys create to obtain the chat URL and API key.")
elif arguments.command == "port-forward":
    if not 1 <= arguments.local_port <= 65535:
        parser.error("--local-port must be 1-65535")
    run([*kubectl, "port-forward", "--address=127.0.0.1", "service/gateway", f"{arguments.local_port}:8000"])
elif arguments.command == "restart":
    run([*kubectl, "rollout", "restart", "deployment/gateway"])
    run([*kubectl, "rollout", "status", "deployment/gateway", "--timeout=20m"])
elif arguments.command == "reload":
    run([*kubectl, "exec", "deployment/gateway", "-c", "gateway", "--", "/gateway", "reload", "--config-dir", "/data/keys"])
elif arguments.command == "keys":
    command = ["/gateway", "keys", arguments.action, "--config-dir", "/data/keys"]
    if arguments.action == "create":
        if not arguments.user:
            parser.error("keys create requires --user")
        command += ["--user", arguments.user, "--chat-url", settings.get("CHAT_URL", "http://127.0.0.1:8000/chat/")]
    if arguments.id:
        command += ["--id", arguments.id]
    if arguments.action == "revoke" and not arguments.id:
        parser.error("keys revoke requires --id")
    run([*kubectl, "exec", "deployment/gateway", "-c", "gateway", "--", *command])
elif arguments.command == "cleanup":
    if arguments.delete_storage:
        run([*kubectl, "delete", "namespace", "llm-gateway", "--ignore-not-found", "--wait=true"])
    else:
        run([*kubectl, "delete", "deployment", "gateway", "--ignore-not-found", "--wait=true"])
        run([*kubectl, "delete", "service", "gateway", "--ignore-not-found"])
        print("Workload stopped. Namespace, config, model volume and private key volume retained.")
