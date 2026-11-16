import { consumeChatStream } from "./stream.mjs";
import { renderAssistantMessage } from "./markdown.mjs";
import {
  readSessionApiKey,
  saveSessionApiKey,
  clearSessionApiKey,
} from "./session.mjs";

const elements = Object.fromEntries(
  [
    "connect-form",
    "api-key",
    "connect-button",
    "connection-state",
    "model",
    "output-limit",
    "gateway-origin",
    "conversation-title",
    "new-chat",
    "disconnect",
    "messages",
    "welcome",
    "status",
    "chat-form",
    "prompt",
    "stop",
    "send",
  ].map((identifier) => [identifier, document.getElementById(identifier)]),
);

// Validated keys survive refreshes in this tab through sessionStorage.
// Conversation history stays in memory; credentials never enter URLs or cookies.
let apiKey = "";
let conversation = [];
let activeRequest = null;
let connecting = false;
elements["gateway-origin"].textContent = window.location.origin;

function setStatus(message, isError = false) {
  elements.status.textContent = message;
  elements.status.classList.toggle("error", isError);
}

function updateControls() {
  const connected = Boolean(apiKey) && !connecting;
  const busy = activeRequest !== null;
  elements["api-key"].disabled = connected || connecting;
  elements["connect-button"].disabled = connected || connecting;
  elements["connect-button"].textContent = connecting
    ? "Connecting…"
    : connected
      ? "Connected"
      : "Connect to gateway";
  elements["connection-state"].textContent = connected
    ? "Connected"
    : "Disconnected";
  elements["connection-state"].classList.toggle("connected", connected);
  elements.model.disabled = !connected || busy;
  elements["output-limit"].disabled = busy;
  elements.prompt.disabled = !connected || busy;
  elements.send.disabled = !connected || busy;
  elements.send.hidden = busy;
  elements.stop.hidden = !busy;
  elements["new-chat"].disabled = !connected || busy;
  elements.disconnect.disabled = !connected || busy;
  elements.messages.setAttribute("aria-busy", String(busy));
}

function clearConversation() {
  conversation = [];
  elements.messages
    .querySelectorAll(".message")
    .forEach((message) => message.remove());
  elements.welcome.hidden = false;
  elements.prompt.value = "";
}

function disconnect() {
  clearSessionApiKey();
  apiKey = "";
  elements["api-key"].value = "";
  elements.model.replaceChildren(new Option("Connect to load models", ""));
  elements["conversation-title"].textContent = "A place to think.";
  clearConversation();
  updateControls();
  setStatus("Disconnected. The key and conversation have been cleared.");
  elements["api-key"].focus();
}

async function gatewayRequest(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    mode: "same-origin",
    credentials: "omit",
    redirect: "error",
    cache: "no-store",
    headers: { Authorization: `Bearer ${apiKey}`, ...options.headers },
  });
  if (!response.ok) {
    let message = `The gateway returned HTTP ${response.status}.`;
    try {
      const body = await response.json();
      if (typeof body.error?.message === "string") message = body.error.message;
    } catch {
      /* Keep a readable fallback for non-JSON infrastructure errors. */
    }
    const retryAfter = response.headers.get("Retry-After");
    if (retryAfter) message += ` Retry after ${retryAfter} seconds.`;
    const failure = new Error(message);
    failure.status = response.status;
    throw failure;
  }
  return response;
}

async function connect(apiKeyCandidate) {
  if (connecting || activeRequest || apiKey) return;
  apiKey = apiKeyCandidate.trim();
  if (!apiKey) return;
  connecting = true;
  updateControls();
  setStatus("Checking your key and loading models…");
  try {
    const response = await gatewayRequest("/v1/models", {
      signal: AbortSignal.timeout(15000),
    });
    const listing = await response.json();
    if (
      !Array.isArray(listing.data) ||
      listing.data.length === 0 ||
      listing.data.some((model) => typeof model.id !== "string")
    ) {
      throw new Error("No registered models are available to select.");
    }
    elements.model.replaceChildren(
      ...listing.data.map((model) => new Option(model.id, model.id)),
    );
    clearConversation();
    elements["api-key"].value = "";
    elements["conversation-title"].textContent = elements.model.value;
    const savedForSession = saveSessionApiKey(apiKey);
    setStatus(
      savedForSession
        ? "Connected. Your key is saved for this tab's session."
        : "Connected. Session storage is unavailable; refresh will require your key again.",
    );
  } catch (error) {
    if (error.status === 401) clearSessionApiKey();
    apiKey = "";
    setStatus(error.message || "Could not connect to the gateway.", true);
  } finally {
    connecting = false;
    updateControls();
    if (apiKey) elements.prompt.focus();
  }
}

elements["connect-form"].addEventListener("submit", (event) => {
  event.preventDefault();
  void connect(elements["api-key"].value);
});

function appendMessage(role, text) {
  elements.welcome.hidden = true;
  const message = document.createElement("article");
  message.className = `message ${role}`;
  const label = document.createElement("p");
  label.className = "message-label";
  label.textContent = role === "user" ? "YOU" : elements.model.value;
  const body = document.createElement("div");
  body.className = "message-body";
  // Model output and prompts are untrusted text, never HTML.
  body.textContent = text;
  message.append(label, body);
  elements.messages.append(message);
  elements.messages.scrollTop = elements.messages.scrollHeight;
  return { message, body };
}

elements["chat-form"].addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!apiKey || connecting || activeRequest) return;
  const prompt = elements.prompt.value.trim();
  const outputTokens = Number(elements["output-limit"].value);
  if (!prompt || !elements.model.value) return;
  if (
    !Number.isInteger(outputTokens) ||
    outputTokens < 1 ||
    outputTokens > 65536
  ) {
    setStatus("Choose an output-token limit between 1 and 65,536.", true);
    return;
  }
  const pendingUserMessage = { role: "user", content: prompt };
  appendMessage("user", prompt);
  const assistant = appendMessage("assistant", "Waiting for the model…");
  elements.prompt.value = "";
  activeRequest = new AbortController();
  updateControls();
  setStatus("Waiting for the gateway. Your request may be queued…");
  let receivedText = false;
  let invalidKey = false;
  try {
    const response = await gatewayRequest("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      signal: activeRequest.signal,
      body: JSON.stringify({
        model: elements.model.value,
        messages: [...conversation, pendingUserMessage],
        max_completion_tokens: outputTokens,
        stream: true,
      }),
    });
    const result = await consumeChatStream(response, (text) => {
      const nearBottom =
        elements.messages.scrollHeight -
          elements.messages.scrollTop -
          elements.messages.clientHeight <
        100;
      receivedText = true;
      renderAssistantMessage(assistant.body, text);
      setStatus("Receiving reply…");
      if (nearBottom)
        elements.messages.scrollTop = elements.messages.scrollHeight;
    });
    renderAssistantMessage(
      assistant.body,
      result.text || "The model returned no text.",
    );
    // Only completed exchanges become context for the next request.
    if (result.text)
      conversation.push(pendingUserMessage, {
        role: "assistant",
        content: result.text,
      });
    setStatus(
      result.finishReason === "length"
        ? "Reply reached the output-token limit."
        : "Reply complete.",
    );
  } catch (error) {
    const stopped = activeRequest.signal.aborted;
    invalidKey = error.status === 401;
    if (!receivedText)
      assistant.body.textContent = stopped
        ? "Generation stopped."
        : "No reply received.";
    const note = document.createElement("p");
    note.className = "message-note";
    note.textContent = "This exchange was not added to conversation context.";
    assistant.message.append(note);
    setStatus(
      stopped
        ? "Stopped. The gateway is cleaning up the request."
        : error.message,
      !stopped,
    );
    elements.prompt.value = prompt;
  } finally {
    activeRequest = null;
    if (invalidKey) {
      clearSessionApiKey();
      apiKey = "";
      setStatus(
        "Your key is invalid or revoked. Paste a valid key to reconnect.",
        true,
      );
    }
    updateControls();
    if (apiKey) elements.prompt.focus();
  }
});

elements.stop.addEventListener("click", () => activeRequest?.abort());
elements.disconnect.addEventListener("click", disconnect);
elements["new-chat"].addEventListener("click", () => {
  clearConversation();
  setStatus("New conversation started.");
  elements.prompt.focus();
});
elements.model.addEventListener("change", () => {
  clearConversation();
  elements["conversation-title"].textContent = elements.model.value;
  setStatus("Model changed. A new conversation has started.");
});
elements.prompt.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    elements["chat-form"].requestSubmit();
  }
});
document.querySelectorAll("[data-prompt]").forEach((button) => {
  button.addEventListener("click", () => {
    if (!apiKey) {
      setStatus("Paste your API key and connect first.");
      elements["api-key"].focus();
      return;
    }
    elements.prompt.value = button.dataset.prompt;
    elements.prompt.focus();
  });
});
updateControls();

// Revalidate a saved key with the gateway before enabling chat after a refresh.
const savedApiKey = readSessionApiKey();
if (savedApiKey) void connect(savedApiKey);
