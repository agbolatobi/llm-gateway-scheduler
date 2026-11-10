"""Manual test routes on the public lab port; private ports and keys stay internal."""
import json

import requests

import gate07_lab as fixtures


ROUTES = {("GET", "/v1/models"), ("POST", "/v1/chat/completions"),
          ("GET", "/__test/state"), ("POST", "/__test/control"),
          ("GET", "/tests/requests")}


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
            handler.reply(413 if size > 1 << 20 else 400, {"error": "JSON body must be between 1 byte and 1 MiB"})
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
        handler.reply(200, {"models": {name: state.snapshot()
                                     for name, state in zip(("local-llm", "second"), lab.states)}})
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
        if not isinstance(mode, str) or model not in ("local-llm", "second") or mode not in fixtures.MODES:
            handler.reply(400, {"error": "Choose model local-llm/second and a supported mode",
                                "modes": sorted(fixtures.MODES)})
            return
        backend = lab.servers[0 if model == "local-llm" else 1]
        with requests.post(f"http://127.0.0.1:{backend.server_port}/__test/control",
                           headers={"Authorization": "Bearer " + lab.key},
                           json={"mode": mode}, timeout=5) as reply:
            handler.reply(reply.status_code, reply.json())
        return

    user = handler.headers.get("X-Test-User", "actual")
    if user not in fixtures.USERS:
        handler.reply(400, {"error": "Unknown X-Test-User", "users": list(fixtures.USERS)})
        return
    with requests.request(handler.command, lab.gateway + handler.path,
                          headers={"Authorization": "Bearer " + lab.values[user + "_key"]},
                          json=body, stream=True, timeout=(3, 10)) as reply:
        # Forward gateway errors/status/request ID unchanged. Do not buffer SSE.
        handler.send_response(reply.status_code)
        for name in ("Content-Type", "X-Request-ID", "Retry-After"):
            if name in reply.headers:
                handler.send_header(name, reply.headers[name])
        handler.send_header("Cache-Control", "no-store")
        handler.send_header("Connection", "close")
        handler.end_headers()
        handler.close_connection = True
        try:
            for chunk in reply.iter_content(chunk_size=1):
                handler.wfile.write(chunk)
                handler.wfile.flush()
        except requests.RequestException:
            # Headers may already be sent: close the incomplete stream, without
            # fabricating a second HTTP response or a successful [DONE] event.
            return
