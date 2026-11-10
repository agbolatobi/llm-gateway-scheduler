import argparse
import math
import sys
from contextlib import closing

import requests

from client import LLMClient
from client.server import ensure_server_ready


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if len(argv) > 1 and argv[0] == "models" and argv[1] in {"list", "validate", "status", "check", "start", "stop", "--help", "-h"}:
        from client.models import main as models_main
        return models_main(argv[1:])
    parser = argparse.ArgumentParser(
        description="Chat with your local inference server."
    )
    parser.add_argument("prompt", nargs="?", help="The prompt to send")
    parser.add_argument("--stream", action="store_true")
    parser.add_argument("--max-tokens", type=int, default=128)
    parser.add_argument("--temperature", type=float, default=0)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--model", default="local-llm")
    parser.add_argument("--config", help="Shared config for registration warnings; does not change the direct chat URL")
    startup = parser.add_argument_group("New server settings (do not reconfigure a reused server)")
    for flag in ("ctx-size", "parallel", "threads", "threads-batch", "batch-size", "ubatch-size"):
        startup.add_argument("--" + flag, type=int)
    parser.add_argument(
        "--start-server", action="store_true",
        help="Start the local llama.cpp server if needed; leave it running afterward",
    )
    parser.add_argument(
        "--startup-timeout", type=float, default=120,
        help="Maximum seconds to wait for server readiness (default: 120)",
    )
    parser.add_argument(
        "--server-only", action="store_true",
        help="With --start-server, wait for readiness and exit without sending a prompt",
    )
    args = parser.parse_args(argv)
    server_options = {key: getattr(args, key) for key in
                      ("ctx_size", "parallel", "threads", "threads_batch", "batch_size", "ubatch_size")
                      if getattr(args, key) is not None}
    if server_options and not args.start_server:
        parser.error("server settings require --start-server")

    if args.server_only:
        if not args.start_server:
            parser.error("--server-only requires --start-server")
        if args.prompt is not None or args.stream:
            parser.error("--server-only cannot be combined with a prompt or --stream")
    elif args.prompt is None:
        parser.error("a prompt is required unless --start-server --server-only is used")

    if args.max_tokens < 1:
        parser.error("--max-tokens must be positive")
    if not 0 <= args.temperature <= 2:
        parser.error("--temperature must be between 0 and 2")
    if not math.isfinite(args.startup_timeout) or args.startup_timeout <= 0:
        parser.error("--startup-timeout must be a positive, finite number")

    client = LLMClient(
        base_url=args.base_url,
        model=args.model,
    )
    messages = [{"role": "user", "content": args.prompt}]
    options = {
        "max_tokens": args.max_tokens,
        "temperature": args.temperature,
    }

    try:
        from client.model_config import DEFAULT_CONFIG, ModelConfig
        from client.models import registration_warning
        config_path = args.config or DEFAULT_CONFIG
        if args.config or DEFAULT_CONFIG.exists():
            config = ModelConfig(config_path)
            warning = registration_warning(config, args.base_url, args.model)
            if warning:
                print(warning, file=sys.stderr)
        if args.start_server and args.config:
            raise ValueError("Use 'models start MODEL --config PATH' for configured model launches; legacy --start-server keeps the fixed default model")
        if args.start_server:
            print("Checking local model readiness...", file=sys.stderr)
            log_path = ensure_server_ready(
                args.base_url, model=args.model, timeout=args.startup_timeout,
                **({"server_options": server_options} if server_options else {}),
            )
            if log_path is not None:
                print(f"Server started. Logs: {log_path}", file=sys.stderr)

        if args.server_only:
            print("Local model is ready; no inference request was sent.")
            return 0

        if args.stream:
            # Close the HTTP response even if printing is interrupted.
            with closing(client.stream(messages, **options)) as events:
                for event in events:
                    for choice in event.get("choices", []):
                        text = choice.get("delta", {}).get("content")
                        if text:
                            print(text, end="", flush=True)
            print()
        else:
            result = client.chat(messages, **options)
            print(result["choices"][0]["message"]["content"])

        return 0

    except KeyboardInterrupt:
        print("\nClient disconnected.", file=sys.stderr)
        return 130
    except (
        requests.RequestException,
        OSError,
        ValueError,
        RuntimeError,
        KeyError,
        IndexError,
    ) as error:
        print(f"\nRequest failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
