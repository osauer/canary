import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const workerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");

function loadWorker({ clients = [], navigator, fetch } = {}) {
  const listeners = new Map();
  const notifications = [];
  const opened = [];
  const self = {
    addEventListener: (type, listener) => listeners.set(type, listener),
    skipWaiting: async () => {},
    registration: { showNotification: async (title, options) => notifications.push({ title, options }) },
    clients: {
      claim: async () => {},
      matchAll: async (matchOptions) => {
        assert.equal(matchOptions.type, "window");
        assert.equal(matchOptions.includeUncontrolled, true);
        return clients;
      },
      openWindow: async (route) => opened.push(route),
    },
  };
  if (navigator) self.navigator = navigator;
  if (fetch) self.fetch = fetch;
  const context = vm.createContext({ self, console });
  vm.runInContext(workerSource, context, { filename: "service-worker.js" });
  return { listeners, notifications, opened };
}

function badgeRecorder() {
  const calls = [];
  return {
    calls,
    navigator: {
      setAppBadge: async (count) => calls.push(["set", count]),
      clearAppBadge: async () => calls.push(["clear"]),
    },
  };
}

async function dispatch(listener, event) {
  let pending;
  listener({ ...event, waitUntil: (promise) => { pending = Promise.resolve(promise); } });
  await pending;
}

const push = (worker, json) => dispatch(worker.listeners.get("push"), { data: { json } });
const click = (worker, destination, close = () => {}) => dispatch(worker.listeners.get("notificationclick"), {
  notification: { data: { destination, url: "https://evil.example/private" }, close },
});

test("push payload uses only the active display id as its notification tag", async () => {
  const worker = loadWorker();
  await push(worker, () => ({
    title: "Safe title", body: "Safe body", kind: "policy_drift",
    display_id: "gov-1111111111111111", alert_id: "legacy-canary", destination: "alerts",
    url: "https://evil.example/private?token=sentinel",
  }));
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

test("malformed payload and unknown destination fail closed to monitor without an invented tag", async () => {
  const worker = loadWorker();
  for (const json of [
    () => { throw new Error("malformed"); },
    () => "https://evil.example",
    () => ({ destination: "javascript:alert(1)", url: "/admin" }),
  ]) await push(worker, json);
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
    navigate: async (route) => navigated.push(route),
    focus: async () => { focused++; },
  }] });
  let closed = 0;
  for (const destination of ["alerts", "brief", "javascript:alert(1)"]) {
    await click(worker, destination, () => { closed++; });
  }
  assert.deepEqual(navigated, ["/?tab=alerts", "/?tab=brief", "/?tab=monitor"]);
  assert.equal(focused, 3);
  assert.equal(closed, 3);
  assert.deepEqual(worker.opened, []);
});

test("notification click opens only fixed monitor and alerts routes when no client exists", async () => {
  const worker = loadWorker();
  for (const destination of ["alerts", "brief", "monitor", "https://evil.example", null]) await click(worker, destination);
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
  await push(worker, () => ({ title: "t", body: "b", destination: "alerts" }));
  assert.equal(worker.notifications.length, 1);
  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].url, "/api/alerts/attention");
  assert.equal(fetchCalls[0].init.credentials, "include");
  assert.deepEqual(badge.calls, [["set", 3]]);
});

test("a failed attention fetch leaves the icon badge untouched and still shows the notification", async () => {
  const badge = badgeRecorder();
  const worker = loadWorker({
    navigator: badge.navigator,
    fetch: async () => ({ ok: false, async json() { return {}; } }),
  });
  await push(worker, () => ({}));
  assert.equal(worker.notifications.length, 1);
  assert.deepEqual(badge.calls, []);
});
