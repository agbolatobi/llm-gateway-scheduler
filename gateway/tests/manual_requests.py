"""Manual test routes on the public lab port; private ports and keys stay internal."""

import json
import http.client
import select
import socket
import threading
from urllib.parse import urlsplit

import requests

import gate07_lab as fixtures

ROUTES = {
    ("GET", "/v1/models"),
    ("POST", "/v1/chat/completions"),
    ("GET", "/__test/state"),
    ("POST", "/__test/control"),
    ("GET", "/tests/requests"),
}


def handle_manual(handler, lab):
    if (handler.command, handler.path) not in ROUTES:
        return False
    # Multiple manual requests may overlap to test capacity. A suite/reset must
    # not restart their gateway; controls remain usable to release held streams.
    if not lab.run_lock.acquire(blocking=False):
        handler.reply(409, {"error": "A test run or reset is already in progress"})
        return True
    try:
        lab.manual_requests += 1
    finally:
        lab.run_lock.release()
    try:
        dispatch(handler, lab)
    except (BrokenPipeError, ConnectionResetError):
        pass
    except requests.RequestException as error:
        handler.reply(502, lab.failure_info(error, "forward manual test request"))
    finally:
        with lab.run_lock:
            lab.manual_requests -= 1
    return True


def read_body(handler):
    if handler.headers.get("Transfer-Encoding"):
        handler.reply(400, {"error": "Use a JSON body with Content-Length"})
        return None
    try:
        size = int(handler.headers.get("Content-Length", "0"))
        if not 0 < size <= 1 << 20:
            handler.reply(
                413 if size > 1 << 20 else 400,
                {"error": "JSON body must be between 1 byte and 1 MiB"},
            )
            return None
        handler.connection.settimeout(5)
        body = json.loads(handler.rfile.read(size))
        if not isinstance(body, dict):
            raise ValueError("Expected JSON object")
        return body
    except (ValueError, OSError):
        handler.reply(400, {"error": "Provide a complete JSON object"})
        return None


def dispatch(handler, lab):
    if handler.path == "/__test/state":
        handler.reply(
            200,
            {
                "models": {
                    name: state.snapshot()
                    for name, state in zip(("local-llm", "second"), lab.states)
                }
            },
        )
        return
    if handler.path == "/tests/requests":
        records = []
        if lab.logs.exists():
            for line in lab.logs.read_text().splitlines():
                pos = line.find('{"event"')
                if pos >= 0:
                    try:
                        record = json.loads(line[pos:])
                    except ValueError:
                        continue
                    if record.get("event") == "gateway_request":
                        records.append(record)
        handler.reply(200, {"requests": records[-50:], "gateway_log": str(lab.logs)})
        return

    body = read_body(handler) if handler.command == "POST" else None
    if handler.command == "POST" and body is None:
        return
    if handler.path == "/__test/control":
        model, mode = body.get("model", "local-llm"), body.get("mode")
        if (
            not isinstance(mode, str)
            or model not in ("local-llm", "second")
            or mode not in fixtures.MODES
        ):
            handler.reply(
                400,
                {
                    "error": "Choose model local-llm/second and a supported mode",
                    "modes": sorted(fixtures.MODES),
                },
            )
            return
        backend = lab.servers[0 if model == "local-llm" else 1]
        with requests.post(
            f"http://127.0.0.1:{backend.server_port}/__test/control",
            headers={"Authorization": "Bearer " + lab.key},
            json={"mode": mode},
            timeout=5,
        ) as reply:
            handler.reply(reply.status_code, reply.json())
        return

    user = handler.headers.get("X-Test-User", "actual")
    if user not in fixtures.USERS:
        handler.reply(400, {"error": "Unknown X-Test-User", "users": list(fixtures.USERS)})
        return
    url = urlsplit(lab.gateway)
    connection = http.client.HTTPConnection(url.hostname, url.port, timeout=3)
    stop = threading.Event()
    started = False
    handler.close_connection = True
    watcher = None
    try:
        connection.connect()
        upstream_socket = connection.sock
        upstream_socket.settimeout(125)  # Gateway total timeout is 120s, including queue wait.

        def watch_disconnect():
            while not stop.wait(0.05):
                try:
                    readable, _, _ = select.select([handler.connection], [], [], 0)
                    if readable and handler.connection.recv(1, socket.MSG_PEEK) == b"":
                        upstream_socket.shutdown(socket.SHUT_RDWR)
                        return
                except OSError:
                    return

        watcher = threading.Thread(target=watch_disconnect, daemon=True)
        watcher.start()
        payload = json.dumps(body).encode() if body is not None else None
        connection.request(
            handler.command,
            handler.path,
            body=payload,
            headers={
                "Authorization": "Bearer " + lab.values[user + "_key"],
                "Content-Type": "application/json",
                "X-Request-ID": handler.headers.get("X-Request-ID", ""),
            },
        )
        reply = connection.getresponse()
        # Forward gateway errors/status/request ID unchanged. Do not buffer SSE.
        handler.send_response(reply.status)
        for name in ("Content-Type", "X-Request-ID", "Retry-After"):
            if reply.getheader(name):
                handler.send_header(name, reply.getheader(name))
        handler.send_header("Cache-Control", "no-store")
        handler.send_header("Connection", "close")
        handler.end_headers()
        started = True
        while chunk := reply.read1(4096):
            handler.wfile.write(chunk)
            handler.wfile.flush()
    except (OSError, http.client.HTTPException) as error:
        if not started:
            handler.reply(502, lab.failure_info(error, "forward manual test request"))
        # Never fabricate a successful [DONE] or a second response after headers.
    finally:
        stop.set()
        connection.close()
        if watcher is not None:
            watcher.join(timeout=0.2)
