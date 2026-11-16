import assert from "node:assert/strict";
import test from "node:test";
import {
  readSessionApiKey,
  saveSessionApiKey,
  clearSessionApiKey,
} from "../internal/chatui/static/session.mjs";

test("session key survives another read and disconnect removes only that key", () => {
  const entries = new Map([["unrelated", "keep"]]);
  globalThis.window = {
    sessionStorage: {
      getItem: (name) => entries.get(name) ?? null,
      setItem: (name, value) => entries.set(name, value),
      removeItem: (name) => entries.delete(name),
    },
  };
  assert.equal(readSessionApiKey(), "");
  assert.equal(saveSessionApiKey("test-key"), true);
  assert.equal(readSessionApiKey(), "test-key");
  clearSessionApiKey();
  assert.equal(readSessionApiKey(), "");
  assert.equal(entries.get("unrelated"), "keep");
  delete globalThis.window;
});

test("blocked session storage does not break chat", () => {
  globalThis.window = {
    get sessionStorage() {
      throw new Error("Storage disabled");
    },
  };
  assert.equal(readSessionApiKey(), "");
  assert.equal(saveSessionApiKey("test-key"), false);
  assert.doesNotThrow(clearSessionApiKey);
  delete globalThis.window;
});
