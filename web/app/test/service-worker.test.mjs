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

function receiptRecorder({ fail = false } = {}) {
  const receipts = [];
  return {
    receipts,
    fetch: async (url, init) => {
      if (url === "/api/push/ack") {
        receipts.push({ init, body: JSON.parse(init.body) });
        if (fail) throw new Error("offline");
        return { ok: true, async json() { return {}; } };
      }
      return { ok: false, async json() { return {}; } };
    },
  };
}

test("a displayed notification posts a displayed receipt for its notice id", async () => {
  const recorder = receiptRecorder();
  const worker = loadWorker({ fetch: recorder.fetch });
  await push(worker, () => ({ title: "t", body: "b", destination: "alerts", display_id: "alert-0123456789abcdef", notice_id: "alert-0123456789abcdef" }));
  assert.equal(worker.notifications.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(worker.notifications[0].options.data)), { destination: "alerts", notice_id: "alert-0123456789abcdef" });
  assert.equal(recorder.receipts.length, 1);
  assert.equal(recorder.receipts[0].init.method, "POST");
  assert.equal(recorder.receipts[0].init.credentials, "include");
  assert.equal(recorder.receipts[0].body.notice_id, "alert-0123456789abcdef");
  assert.equal(recorder.receipts[0].body.event, "displayed");
  assert.ok(Number.isFinite(Date.parse(recorder.receipts[0].body.at)));
});

test("a hostile notice id is dropped: no receipt and no routed id", async () => {
  const recorder = receiptRecorder();
  const worker = loadWorker({ fetch: recorder.fetch });
  for (const noticeID of ["../api/devices", "https://evil.example", "UPPER-case", 42]) {
    await push(worker, () => ({ title: "t", body: "b", destination: "alerts", notice_id: noticeID }));
  }
  assert.equal(recorder.receipts.length, 0);
  for (const notification of worker.notifications) assert.deepEqual(Object.keys(notification.options.data), ["destination"]);
  await dispatch(worker.listeners.get("notificationclick"), { notification: { data: { destination: "alerts", notice_id: "../x" }, close() {} } });
  assert.equal(recorder.receipts.length, 0);
  assert.deepEqual(worker.opened, ["/?tab=alerts"]);
});

test("a tap posts an opened receipt and carries the notice id on the fixed route", async () => {
  const recorder = receiptRecorder();
  const worker = loadWorker({ fetch: recorder.fetch });
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "alerts", notice_id: "diagnostic-0123456789abcdef" }, close() {} },
  });
  assert.equal(recorder.receipts.length, 1);
  assert.equal(recorder.receipts[0].body.event, "opened");
  assert.equal(recorder.receipts[0].body.notice_id, "diagnostic-0123456789abcdef");
  assert.deepEqual(worker.opened, ["/?tab=alerts&notice=diagnostic-0123456789abcdef"]);
});

test("a failed receipt never blocks the notification or the navigation", async () => {
  const recorder = receiptRecorder({ fail: true });
  const worker = loadWorker({ fetch: recorder.fetch });
  await push(worker, () => ({ title: "t", body: "b", notice_id: "alert-0123456789abcdef" }));
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "monitor", notice_id: "alert-0123456789abcdef" }, close() {} },
  });
  assert.equal(worker.notifications.length, 1);
  assert.equal(recorder.receipts.length, 2);
  assert.deepEqual(worker.opened, ["/?tab=monitor&notice=alert-0123456789abcdef"]);
});
