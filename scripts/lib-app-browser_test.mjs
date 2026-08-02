import assert from "node:assert/strict";
import test from "node:test";

import { assertBrowserLaunchAllowed } from "./lib-app-browser.mjs";

test("browser launch fails fast inside the Codex macOS sandbox", () => {
  assert.throws(
    () => assertBrowserLaunchAllowed({ platform: "darwin", codexSandbox: "seatbelt" }),
    /cannot launch a macOS browser inside the Codex sandbox/,
  );
});

test("browser launch is allowed outside the Codex macOS sandbox", () => {
  assert.doesNotThrow(() => assertBrowserLaunchAllowed({ platform: "darwin", codexSandbox: "" }));
});

test("non-macOS launch behavior is unchanged", () => {
  assert.doesNotThrow(() => assertBrowserLaunchAllowed({ platform: "linux", codexSandbox: "seatbelt" }));
});
