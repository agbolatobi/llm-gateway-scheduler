const apiKeyStorageName = "llm-gateway.chat.api-key";

// Storage may be disabled by browser settings. Chat still works in memory.
export function readSessionApiKey() {
  try {
    return window.sessionStorage.getItem(apiKeyStorageName) || "";
  } catch {
    return "";
  }
}

export function saveSessionApiKey(apiKey) {
  try {
    window.sessionStorage.setItem(apiKeyStorageName, apiKey);
    return true;
  } catch {
    return false;
  }
}

export function clearSessionApiKey() {
  try {
    window.sessionStorage.removeItem(apiKeyStorageName);
  } catch {
    // Unavailable storage must not prevent disconnecting the current page.
  }
}
