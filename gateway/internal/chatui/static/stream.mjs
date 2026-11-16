// fetch is used instead of EventSource so the API key stays in an Authorization
// header. Read to HTTP completion: a partial stream is never saved as a reply.
export async function consumeChatStream(response, onText) {
  if (
    !response.body ||
    !response.headers.get("content-type")?.includes("text/event-stream")
  ) {
    throw new Error("The gateway did not return an event stream.");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let pendingText = "";
  let dataLines = [];
  let eventName = "";
  let eventSize = 0;
  let completed = false;
  let finishReason = null;
  let reply = "";

  function dispatchEvent() {
    if (dataLines.length === 0) {
      eventName = "";
      eventSize = 0;
      return;
    }
    const payload = dataLines.join("\n");
    dataLines = [];
    eventSize = 0;
    const currentEvent = eventName;
    eventName = "";
    if (completed) throw new Error("Unexpected data after stream completion.");
    if (payload === "[DONE]") {
      if (!finishReason)
        throw new Error("The response ended without a finish reason.");
      completed = true;
      return;
    }
    const chunk = JSON.parse(payload);
    if (currentEvent === "error" || chunk.error) {
      throw new Error(
        chunk.error?.message || "The gateway interrupted the response.",
      );
    }
    if (currentEvent && currentEvent !== "message") {
      throw new Error("Unexpected gateway event.");
    }
    const choice = chunk.choices?.[0];
    if (typeof choice?.delta?.content === "string") {
      reply += choice.delta.content;
      onText(reply);
    }
    if (typeof choice?.finish_reason === "string")
      finishReason = choice.finish_reason;
  }

  function consumeLine(line) {
    if (line.endsWith("\r")) line = line.slice(0, -1);
    if (line === "") {
      dispatchEvent();
      return;
    }
    eventSize += line.length;
    if (eventSize > 1024 * 1024)
      throw new Error("A gateway event exceeded the size limit.");
    if (line.startsWith(":")) return;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "data") dataLines.push(value);
    if (field === "event") eventName = value;
  }

  try {
    while (true) {
      const { value, done } = await reader.read();
      pendingText += done
        ? decoder.decode()
        : decoder.decode(value, { stream: true });
      let lineEnd = pendingText.indexOf("\n");
      while (lineEnd >= 0) {
        consumeLine(pendingText.slice(0, lineEnd));
        pendingText = pendingText.slice(lineEnd + 1);
        lineEnd = pendingText.indexOf("\n");
      }
      if (pendingText.length > 1024 * 1024)
        throw new Error("A gateway event exceeded the size limit.");
      if (done) break;
    }
    if (!completed || pendingText.trim() || dataLines.length) {
      throw new Error(
        "The connection ended before the reply completed. Please try again.",
      );
    }
    return { text: reply, finishReason };
  } finally {
    // Parsing errors must close the request too, allowing gateway cancellation
    // and quarantine logic to release the model safely.
    try {
      await reader.cancel();
    } catch {
      /* The transport may already be closed. */
    }
    reader.releaseLock();
  }
}
