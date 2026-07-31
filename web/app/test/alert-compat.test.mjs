import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

import { createDOMHarness, FakeElement } from "./dom-harness.mjs";

const alertsSource = await readFile(new URL("../alerts.js", import.meta.url), "utf8");
const serviceWorkerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");

function loadAlerts({ visibility = "visible", alertsPanelHidden = false } = {}) {
  const modeButtons = ["none", "act_only", "watch_and_act"].map((mode) => {
    const button = new FakeElement();
    button.dataset.mode = mode;
    return button;
  });
  const { document, element, elements } = createDOMHarness({
    visibilityState: visibility,
    querySelectorAll(selector) {
      if (selector === "#alertSegments button") return modeButtons;
      return [];
    },
  });
  element("alertsTab").hidden = alertsPanelHidden;
  const storageWrites = [];
  const badgeCalls = [];
  const state = {
    activeTab: "monitor",
    alertFilter: "all",
    alertSettings: { mode: "watch_and_act" },
    alertSettingsUpdate: { busy: false, state: "", error: false },
    alerts: [],
    attention: null,
    attentionEpoch: 0,
    attentionReadInFlight: null,
    attentionRetryTimer: null,
    attentionStatus: { state: "", error: false },
    authenticated: true,
    clearedAlertFingerprint: "",
    governance: null,
    governanceCutoverReceipt: null,
    governanceCutoverReview: { busy: false, state: "", error: false },
    governanceRefreshAfterFlight: false,
    governanceRefreshDueAt: 0,
    governanceRefreshInFlight: null,
    governanceRefreshSucceeded: null,
    governanceRefreshTimer: null,
    governanceRefreshTimerEnsureTrailing: false,
    governanceLastRefreshAt: 0,
    pushInspection: { state: "status unavailable", busy: false },
    safeNotificationTest: { busy: false, state: "", error: false },
    selectedAlertID: null,
    snapshot: {
      stress: { fingerprint: { key: "stress-current" }, portfolio_fit: "low", portfolio_alert_relevant: false, portfolio: {} },
      sources: { nudges: { state: "current" } },
      nudges: {
        as_of: "2026-07-02T00:00:00Z",
        candidates: [],
        source_health: { aggregate: "ready" },
        confirmed_flow_coverage: { coverage_from: "2026-07-01T00:00:00Z", pre_cutover_flows_unreviewed: false },
      },
    },
    vapidPublicKey: "",
  };
  const registration = { pushManager: { async getSubscription() { return null; } } };
  function Notification() {}
  Notification.permission = "default";
  const context = vm.createContext({
    console,
    Date,
    JSON,
    clearTimeout,
    setTimeout,
    document,
    state,
    $: element,
    b64urlToBytes: () => new Uint8Array(),
    heldStressItems: () => [],
    heldStressSummary: () => "",
    labelize: (value) => String(value),
    shortTime: () => "12:00",
    localStorage: {
      getItem: () => "",
      setItem(key, value) { storageWrites.push([key, value]); },
    },
    navigator: {
      serviceWorker: { ready: Promise.resolve(registration) },
      setAppBadge(count) { badgeCalls.push(["set", count]); return Promise.resolve(); },
      clearAppBadge() { badgeCalls.push(["clear"]); return Promise.resolve(); },
    },
    Notification,
    PushManager: function PushManager() {},
  });
  const executable = alertsSource
    .replace(/^import .*;\n/gm, "")
    .replace(/export \{([^}]+)\};\s*$/m, "globalThis.__exports = {$1};");
  vm.runInContext(executable, context, { filename: "alerts.js" });
  return { badgeCalls, context, document, elements, exports: context.__exports, modeButtons, registration, state, storageWrites };
}

function response(body, ok = true) {
  return { ok, async json() { return body; }, async text() { return JSON.stringify(body); } };
}

function delayedJSONResponse() {
  let markStarted;
  let releaseBody;
  const started = new Promise((resolve) => { markStarted = resolve; });
  const body = new Promise((resolve) => { releaseBody = resolve; });
  return {
    response: {
      ok: true,
      async json() {
        markStarted();
        return body;
      },
    },
    started,
    release: releaseBody,
  };
}

function attention(unreadCount, highWaterSeq, readThroughSeq, unreadRefs) {
  return { unread_count: unreadCount, high_water_seq: highWaterSeq, read_through_seq: readThroughSeq, unread_refs: unreadRefs };
}

function governanceDTO(overrides = {}) {
  return {
    candidates: [],
    source_health: {},
    poll_source: {},
    occurrences: [],
    delivery_health: {},
    diagnostic: {},
    ...overrides,
  };
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

function visibleText(element) {
  return [element?.textContent || "", ...(element?.children || []).map(visibleText)].join(" ").trim();
}

function assertExactJSONCall(call, { method, url, body }) {
  assert.equal(call.url, url);
  assert.equal(call.init.method, method);
  assert.equal(call.init.credentials, "include");
  assert.deepEqual(Object.keys(call.init.headers), ["Content-Type"]);
  assert.equal(call.init.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(call.init.body), body);
}

test("each notification level makes one exact app-owned PUT and strict failures roll back with safe copy", async () => {
  for (const mode of ["none", "act_only", "watch_and_act"]) {
    const harness = loadAlerts();
    const calls = [];
    harness.context.fetch = async (url, init = {}) => {
      calls.push({ url, init });
      return response({ mode });
    };
    assert.equal(await harness.exports.setAlertMode(mode), true);
    assert.equal(calls.length, 1);
    assertExactJSONCall(calls[0], { method: "PUT", url: "/api/alerts/settings", body: { mode } });
    assert.equal(calls.some((call) => call.url === "/api/settings"), false);
    assert.equal(harness.state.alertSettings.mode, mode);
    assert.match(harness.elements.get("alertSettingsStatus").textContent, /saved/i);
  }

  for (const fixture of [response({}, false), response({ mode: "hostile" }), response({ mode: "none", private: "receipt" })]) {
    const harness = loadAlerts();
    harness.state.alertSettings = { mode: "act_only" };
    harness.context.fetch = async (url) => {
      assert.equal(url, "/api/alerts/settings");
      return fixture;
    };
    assert.equal(await harness.exports.setAlertMode("none"), false);
    assert.equal(harness.state.alertSettings.mode, "act_only");
    assert.equal(harness.elements.get("alertSettingsStatus").textContent, "Delivery level was not changed.");
    assert.equal(harness.elements.get("alertSettingsStatus").textContent.includes("private"), false);
  }
});
test("device status distinguishes permission from an actual browser subscription", async () => {
  const harness = loadAlerts();
  assert.equal(await harness.exports.refreshPushState(), "permission not granted");
  harness.context.Notification.permission = "denied";
  assert.equal(await harness.exports.refreshPushState(), "permission blocked");
  harness.context.Notification.permission = "granted";
  assert.equal(await harness.exports.refreshPushState(), "permission granted but not subscribed");
  harness.registration.pushManager.getSubscription = async () => ({ endpoint: "private-endpoint" });
  assert.equal(await harness.exports.refreshPushState(), "browser subscribed");
  assert.equal(harness.elements.get("pushState").textContent, "browser subscribed");
  assert.equal(harness.elements.get("pushState").textContent.includes("private-endpoint"), false);
});
