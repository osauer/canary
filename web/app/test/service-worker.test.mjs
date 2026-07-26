import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const workerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");

function loadWorker(options = {}) {
  const listeners = new Map();
  const notifications = [];
  const opened = [];
  const clients = options.clients || [];
  const self = {
    addEventListener(type, listener) {
      listeners.set(type, listener);
    },
    skipWaiting: async () => {},
    registration: {
      async showNotification(title, notificationOptions) {
        notifications.push({ title, options: notificationOptions });
      },
    },
    clients: {
      claim: async () => {},
      async matchAll(matchOptions) {
        assert.equal(matchOptions.type, "window");
        assert.equal(matchOptions.includeUncontrolled, true);
        return clients;
      },
      async openWindow(route) {
        opened.push(route);
      },
    },
  };
  if (options.navigator) self.navigator = options.navigator;
  if (options.fetch) self.fetch = options.fetch;
  const context = vm.createContext({ self, console });
  vm.runInContext(workerSource, context, { filename: "service-worker.js" });
  return { listeners, notifications, opened };
}

function badgeRecorder() {
  const calls = [];
  return {
    calls,
    navigator: {
      setAppBadge: async (count) => { calls.push(["set", count]); },
      clearAppBadge: async () => { calls.push(["clear"]); },
    },
  };
}

async function dispatch(listener, event) {
  let pending;
  listener({
    ...event,
    waitUntil(promise) {
      pending = Promise.resolve(promise);
    },
  });
  await pending;
}

test("push payload uses only the active display id as its notification tag", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), {
    data: { json: () => ({
      title: "Safe title", body: "Safe body", kind: "policy_drift",
      display_id: "gov-1111111111111111", alert_id: "legacy-canary", destination: "alerts",
      url: "https://evil.example/private?token=sentinel",
    }) },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(worker.notifications)), [{
    title: "Safe title",
    options: {
      body: "Safe body",
      data: { destination: "alerts" },
      tag: "gov-1111111111111111",
      badge: "/favicon-64.png",
      icon: "/icon-192.png",
    },
  }]);
  assert.equal(JSON.stringify(worker.notifications).includes("evil.example"), false);
});

test("a brief-destined push shows on the Brief tab", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), {
    data: { json: () => ({
      title: "Monthly risk pulse due", body: "Review the monthly risk brief and policy pins.",
      kind: "monthly_pulse", display_id: "gov-2222222222222222", destination: "brief",
    }) },
  });
  assert.equal(worker.notifications[0].options.data.destination, "brief");
  assert.equal(worker.notifications[0].options.tag, "gov-2222222222222222");
});

test("distinct display ids remain distinct and a legacy alert id is ignored", async () => {
  const worker = loadWorker();
  for (const display_id of ["gov-aaaaaaaaaaaaaaaa", "gov-bbbbbbbbbbbbbbbb"]) {
    await dispatch(worker.listeners.get("push"), { data: { json: () => ({ display_id, kind: "monthly_pulse" }) } });
  }
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({ alert_id: "canary-stable" }) } });
  assert.deepEqual(worker.notifications.map((item) => item.options.tag), [
    "gov-aaaaaaaaaaaaaaaa", "gov-bbbbbbbbbbbbbbbb", undefined,
  ]);
});

test("push fallback identity is Canary without changing delivery semantics", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({}) } });
  assert.equal(worker.notifications[0].title, "Canary");
  assert.equal(worker.notifications[0].options.body, "Open Canary for details.");
  assert.deepEqual(JSON.parse(JSON.stringify(worker.notifications[0].options.data)), { destination: "monitor" });
  assert.equal("tag" in worker.notifications[0].options, false);
});

test("malformed payload and unknown destination fail closed to monitor without an invented tag", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), { data: { json: () => { throw new Error("malformed"); } } });
  await dispatch(worker.listeners.get("push"), { data: { json: () => "https://evil.example" } });
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({ destination: "javascript:alert(1)", url: "/admin" }) } });
  for (const notification of worker.notifications) {
    assert.equal(notification.options.data.destination, "monitor");
    assert.deepEqual(Object.keys(notification.options.data), ["destination"]);
    assert.equal("tag" in notification.options, false);
    assert.equal(JSON.stringify(notification).includes("evil.example"), false);
    assert.equal(JSON.stringify(notification).includes("/admin"), false);
  }
});

test("notification click navigates and focuses an existing client", async () => {
  const navigated = [];
  let focused = 0;
  const worker = loadWorker({ clients: [{
    async navigate(route) { navigated.push(route); },
    async focus() { focused++; },
  }] });
  let closed = 0;
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "alerts", url: "https://evil.example" }, close() { closed++; } },
  });
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "brief", url: "https://evil.example/private" }, close() { closed++; } },
  });
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "javascript:alert(1)", url: "https://evil.example/private" }, close() { closed++; } },
  });
  assert.deepEqual(navigated, ["/?tab=alerts", "/?tab=brief", "/?tab=monitor"]);
  assert.equal(focused, 3);
  assert.equal(closed, 3);
  assert.deepEqual(worker.opened, []);
});

test("notification click opens only fixed monitor and alerts routes when no client exists", async () => {
  const worker = loadWorker();
  for (const destination of ["alerts", "brief", "monitor", "https://evil.example", null]) {
    await dispatch(worker.listeners.get("notificationclick"), {
      notification: { data: { destination, url: "file:///private/sentinel" }, close() {} },
    });
  }
  assert.deepEqual(worker.opened, ["/?tab=alerts", "/?tab=brief", "/?tab=monitor", "/?tab=monitor", "/?tab=monitor"]);
  assert.equal(worker.opened.some((route) => /evil|file:|private/.test(route)), false);
});

test("push mirrors the server unread count onto the app icon badge", async () => {
  const badge = badgeRecorder();
  const fetchCalls = [];
  const worker = loadWorker({
    navigator: badge.navigator,
    fetch: async (url, init) => {
      fetchCalls.push({ url, init });
      return { ok: true, async json() { return { unread_count: 3, high_water_seq: 9, read_through_seq: 6, unread_refs: [] }; } };
    },
  });
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({ title: "t", body: "b", destination: "alerts" }) } });
  assert.equal(worker.notifications.length, 1);
  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].url, "/api/alerts/attention");
  assert.equal(fetchCalls[0].init.credentials, "include");
  assert.deepEqual(badge.calls, [["set", 3]]);
});

test("push clears the icon badge when the server reports zero unread", async () => {
  const badge = badgeRecorder();
  const worker = loadWorker({
    navigator: badge.navigator,
    fetch: async () => ({ ok: true, async json() { return { unread_count: 0 }; } }),
  });
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({}) } });
  assert.deepEqual(badge.calls, [["clear"]]);
});

test("a failed attention fetch leaves the icon badge untouched and still shows the notification", async () => {
  const badge = badgeRecorder();
  const worker = loadWorker({
    navigator: badge.navigator,
    fetch: async () => ({ ok: false, async json() { return {}; } }),
  });
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({}) } });
  assert.equal(worker.notifications.length, 1);
  assert.deepEqual(badge.calls, []);
});

test("a badge-less runtime still shows the notification", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({}) } });
  assert.equal(worker.notifications.length, 1);
});

test("notification click navigates without fetching or touching the icon badge", async () => {
  const badge = badgeRecorder();
  const worker = loadWorker({
    navigator: badge.navigator,
    fetch: async () => { throw new Error("notificationclick must not fetch"); },
  });
  await dispatch(worker.listeners.get("notificationclick"), { notification: { close() {}, data: { destination: "alerts" } } });
  assert.deepEqual(worker.opened, ["/?tab=alerts"]);
  assert.deepEqual(badge.calls, []);
});
