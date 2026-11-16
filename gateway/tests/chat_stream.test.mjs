import test from "node:test";
import assert from "node:assert/strict";
import { consumeChatStream } from "../internal/chatui/static/stream.mjs";

const event = (content, finishReason = null) =>
  `data: ${JSON.stringify({ choices: [{ delta: { content }, finish_reason: finishReason }] })}\n\n`;
const complete = event("", "stop") + "data: [DONE]\n\n";

function responseFor(text, chunkSize = 7) {
  const bytes = new TextEncoder().encode(text);
  let offset = 0;
  return new Response(
    new ReadableStream({
      pull(controller) {
        if (offset === bytes.length) {
          controller.close();
          return;
        }
        controller.enqueue(bytes.slice(offset, offset + chunkSize));
        offset = Math.min(offset + chunkSize, bytes.length);
      },
    }),
    { headers: { "Content-Type": "text/event-stream" } },
  );
}

test("streams split UTF-8 characters and CRLF frames without losing text", async () => {
  const updates = [];
  const response = responseFor(
    (": keepalive\n\n" + event("Hello 🌍") + event("!") + complete).replaceAll(
      "\n",
      "\r\n",
    ),
    1,
  );
  const result = await consumeChatStream(response, (text) =>
    updates.push(text),
  );
  assert.equal(result.text, "Hello 🌍!");
  assert.equal(result.finishReason, "stop");
  assert.ok(updates.includes("Hello 🌍"));
});

test("accepts usage-only events and multiple data lines", async () => {
  const source =
    'data: {"choices":\ndata: [{"delta":{"content":"answer"},"finish_reason":"length"}]}\n\n' +
    'data: {"choices":[],"usage":{"total_tokens":8}}\n\n' +
    "data: [DONE]\n\n";
  assert.deepEqual(await consumeChatStream(responseFor(source), () => {}), {
    text: "answer",
    finishReason: "length",
  });
});

test("rejects truncated streams and missing finish reasons", async () => {
  for (const source of [
    event("partial"),
    event("partial") + "data: [DONE]\n\n",
    event("partial") + complete.slice(0, -1),
  ]) {
    await assert.rejects(consumeChatStream(responseFor(source), () => {}));
  }
});

test("surfaces gateway errors instead of claiming a successful reply", async () => {
  const source =
    event("partial") +
    'event: error\ndata: {"error":{"message":"Runtime unavailable"}}\n\n';
  await assert.rejects(
    consumeChatStream(responseFor(source), () => {}),
    /Runtime unavailable/,
  );
});

test("rejects malformed JSON, oversized events and non-stream responses", async () => {
  await assert.rejects(
    consumeChatStream(responseFor("data: nope\n\n"), () => {}),
  );
  await assert.rejects(
    consumeChatStream(
      responseFor("data: " + "a".repeat(1024 * 1024 + 1), 65536),
      () => {},
    ),
    /size limit/,
  );
  await assert.rejects(
    consumeChatStream(new Response("{}"), () => {}),
    /event stream/,
  );
});

test("closes the reader on parser failure", async () => {
  let cancelled = false;
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode("data: invalid\n\n"));
    },
    cancel() {
      cancelled = true;
    },
  });
  await assert.rejects(
    consumeChatStream(
      new Response(body, { headers: { "Content-Type": "text/event-stream" } }),
      () => {},
    ),
  );
  assert.equal(cancelled, true);
});

test("waits for HTTP completion after DONE and propagates transport failures", async () => {
  let streamController;
  const body = new ReadableStream({
    start(controller) {
      streamController = controller;
      controller.enqueue(new TextEncoder().encode(event("answer") + complete));
    },
  });
  const completion = consumeChatStream(
    new Response(body, { headers: { "Content-Type": "text/event-stream" } }),
    () => {},
  );
  let settled = false;
  completion.then(
    () => {
      settled = true;
    },
    () => {
      settled = true;
    },
  );
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(settled, false);
  streamController.error(new Error("Connection reset"));
  await assert.rejects(completion, /Connection reset/);
});
