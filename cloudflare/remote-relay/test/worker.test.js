import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import worker, { RelaySession, __test } from "../src/worker.js";

const DAY_MS = 24 * 60 * 60 * 1000;
const VALID_ROUTE = "r_abcdefghijklmnop";

function fakeState(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    storage: {
      async get(key) {
        return data.get(key);
      },
      async put(key, value) {
        data.set(key, value);
      },
    },
  };
}

function fakeEnv() {
  const sessions = new Map();
  return {
    sessions,
    env: {
      RELAY_SESSION: {
        idFromName: (name) => name,
        get: (name) => {
          if (!sessions.has(name)) sessions.set(name, new RelaySession(fakeState(), {}));
          return sessions.get(name);
        },
      },
    },
  };
}

async function waitFor(predicate) {
  for (let i = 0; i < 100; i++) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.fail("condition was not reached");
}

function activeTomlStringArray(config, key) {
  const assignment = new RegExp(`^\\s*${key}\\s*=\\s*(\\[[^#]*\\])\\s*(?:#.*)?$`);
  const matches = config.split(/\r?\n/)
    .map((line) => assignment.exec(line))
    .filter(Boolean);
  assert.equal(matches.length, 1, `${key} must have one active TOML assignment`);
  const value = JSON.parse(matches[0][1]);
  assert.ok(value.every((item) => typeof item === "string"), `${key} must be a string array`);
  return value;
}

test("requestPath preserves path and query", () => {
  assert.equal(
    __test.requestPath("https://remote.osauer.dev/pair.html?remote=r1&pair=p1"),
    "/pair.html?remote=r1&pair=p1",
  );
});

test("route cookie round trips route id", () => {
  const cookie = __test.routeCookie(VALID_ROUTE);
  assert.match(cookie, /Secure/);
  assert.match(cookie, /HttpOnly/);
  assert.equal(__test.readCookie(cookie, "ibkr_remote_route"), VALID_ROUTE);
});

test("production config carries request cancellation through the Worker-to-DO hop", () => {
  const config = readFileSync(new URL("../wrangler.toml", import.meta.url), "utf8");
  assert.deepEqual(activeTomlStringArray(config, "compatibility_flags").sort(), [
    "enable_request_signal",
    "request_signal_passthrough",
  ]);
});

test("deployed relay continuity pins remain stable while package identity is Canary", () => {
  const config = readFileSync(new URL("../wrangler.toml", import.meta.url), "utf8");
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  assert.equal(pkg.name, "canary-remote-relay");
  assert.match(config, /^name = "ibkr-remote-relay"$/m);
  assert.match(config, /^name = "RELAY_SESSION"$/m);
  assert.match(config, /^class_name = "RelaySession"$/m);
  assert.match(config, /^new_sqlite_classes = \["RelaySession"\]$/m);
});

test("headerMap removes hop by hop headers", () => {
  const headers = new Headers({
    Connection: "upgrade",
    Cookie: "a=b",
    Upgrade: "websocket",
  });
  assert.deepEqual(__test.headerMap(headers), { cookie: ["a=b"] });
});

test("connect rejects a route with no registered token", async () => {
  const state = fakeState({ expires_at: new Date(Date.now() - 1000).toISOString() });
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/api/connect"));
  assert.equal(res.status, 401);
});

test("connect with the connector token revives an expired route", async () => {
  const state = fakeState({
    connector_token: "tok",
    expires_at: new Date(Date.now() - 1000).toISOString(),
  });
  const session = new RelaySession(state, {});
  const before = Date.now();
  // No websocket upgrade so the test stays inside Node: the token check and
  // TTL slide run before the upgrade requirement.
  const res = await session.fetch(
    new Request("https://relay.example/api/connect", {
      headers: { Authorization: "Bearer tok" },
    }),
  );
  assert.equal(res.status, 426);
  const next = Date.parse(state.data.get("expires_at"));
  assert.ok(next >= before + 7 * DAY_MS - 1000, `expired route was not revived: ${next}`);
});

test("connect does not slide the TTL for a bad connector token", async () => {
  const expiry = new Date(Date.now() + 60 * 1000).toISOString();
  const state = fakeState({ connector_token: "good", expires_at: expiry });
  const session = new RelaySession(state, {});
  const res = await session.fetch(
    new Request("https://relay.example/api/connect", {
      headers: { Upgrade: "websocket", Authorization: "Bearer bad" },
    }),
  );
  assert.equal(res.status, 401);
  assert.equal(state.data.get("expires_at"), expiry);
});

test("slideExpiry renews the route for a full TTL window", async () => {
  const stale = new Date(Date.now() + 60 * 1000).toISOString();
  const state = fakeState({ expires_at: stale });
  const session = new RelaySession(state, {});
  const before = Date.now();
  await session.slideExpiry();
  const next = Date.parse(state.data.get("expires_at"));
  assert.ok(next >= before + 7 * DAY_MS - 1000, `expires_at ${next} is not ~7 days out`);
  assert.ok(next > Date.parse(stale), "slideExpiry did not extend the window");
});

test("cookie-addressed requests refresh the route cookie", async () => {
  const env = {
    RELAY_SESSION: {
      idFromName: (name) => name,
      get: () => ({ fetch: async () => new Response("ok") }),
    },
  };
  const req = new Request("https://relay.example/api/bootstrap", {
    headers: { Cookie: `ibkr_remote_route=${VALID_ROUTE}` },
  });
  const res = await worker.fetch(req, env);
  assert.equal(res.status, 200);
  const setCookie = res.headers.get("Set-Cookie") || "";
  assert.match(setCookie, new RegExp(`ibkr_remote_route=${VALID_ROUTE}`));
  assert.match(setCookie, /Max-Age=34560000/);
});

test("invalid route selectors never address a Durable Object", async () => {
  const addressed = [];
  const env = {
    RELAY_SESSION: {
      idFromName: (name) => {
        addressed.push(name);
        return name;
      },
      get: () => ({ fetch: async () => new Response("unexpected") }),
    },
  };
  const requests = [
    new Request("https://relay.example/api/connect?route_id=bad"),
    new Request("https://relay.example/api/bootstrap?remote=bad"),
    new Request("https://relay.example/api/bootstrap", {
      headers: { Cookie: "ibkr_remote_route=bad" },
    }),
  ];
  for (const request of requests) {
    const response = await worker.fetch(request, env);
    assert.equal(response.status, 400);
  }
  assert.deepEqual(addressed, []);
});

test("client abort cancels one request without changing durable route state", async () => {
  const expiry = new Date(Date.now() + DAY_MS).toISOString();
  const state = fakeState({ connector_token: "tok", expires_at: expiry });
  const session = new RelaySession(state, {});
  const frames = [];
  let closeCalls = 0;
  const connector = {
    send(raw) {
      frames.push(JSON.parse(raw));
    },
    close() {
      closeCalls++;
    },
  };
  session.connector = connector;
  const before = [...state.data.entries()];
  const controller = new AbortController();
  const forwarded = session.fetch(new Request("https://relay.example/api/events", {
    signal: controller.signal,
  }));
  await waitFor(() => session.inflight.size === 1);
  const requestID = frames.find((frame) => frame.type === "request")?.id;
  controller.abort();
  const response = await forwarded;

  assert.equal(response.status, 499);
  assert.equal(session.inflight.size, 0);
  assert.deepEqual(frames.at(-1), { type: "request_cancel", id: requestID });
  assert.equal(session.connector, connector);
  assert.equal(closeCalls, 0);
  assert.deepEqual([...state.data.entries()], before);
});

test("stream abort leaves the connector usable for the next phone request", async () => {
  const state = fakeState({ expires_at: new Date(Date.now() + DAY_MS).toISOString() });
  const session = new RelaySession(state, {});
  const frames = [];
  const connector = {
    send(raw) {
      frames.push(JSON.parse(raw));
    },
    close() {
      assert.fail("request cancellation must not close the shared connector");
    },
  };
  session.connector = connector;

  const controller = new AbortController();
  const streaming = session.fetch(new Request("https://relay.example/api/events", {
    signal: controller.signal,
  }));
  await waitFor(() => session.inflight.size === 1);
  const streamID = frames.find((frame) => frame.type === "request").id;
  await session.handleConnectorMessage(JSON.stringify({
    type: "response_start",
    id: streamID,
    status: 200,
    headers: { "content-type": ["text/event-stream"] },
  }));
  const streamResponse = await streaming;
  const streamBody = streamResponse.text();
  controller.abort();
  await assert.rejects(streamBody);
  assert.equal(session.inflight.size, 0);
  await session.handleConnectorMessage(JSON.stringify({ type: "response_end", id: streamID }));

  const nextController = new AbortController();
  const next = session.fetch(new Request("https://relay.example/api/bootstrap", {
    signal: nextController.signal,
  }));
  await waitFor(() => session.inflight.size === 1);
  const nextID = frames.filter((frame) => frame.type === "request").at(-1).id;
  await session.handleConnectorMessage(JSON.stringify({
    type: "response_start",
    id: nextID,
    status: 200,
    headers: {},
  }));
  const nextResponse = await next;
  await session.handleConnectorMessage(JSON.stringify({ type: "response_end", id: nextID }));
  assert.equal(nextResponse.status, 200);
  assert.equal(session.inflight.size, 0);
  assert.equal(session.connector, connector);
  const frameCount = frames.length;
  nextController.abort();
  assert.equal(frames.length, frameCount, "completed request retained an abort listener");
});

test("register can resume an existing route with the connector token", async () => {
  const { env } = fakeEnv();
  const first = await worker.fetch(new Request("https://relay.example/api/register", { method: "POST" }), env);
  assert.equal(first.status, 200);
  const route = await first.json();
  const resumed = await worker.fetch(new Request("https://relay.example/api/register", {
    method: "POST",
    body: JSON.stringify({
      route_id: route.route_id,
      connector_token: route.connector_token,
    }),
  }), env);
  assert.equal(resumed.status, 200);
  const body = await resumed.json();
  assert.equal(body.route_id, route.route_id);
  assert.equal(body.connector_token, route.connector_token);
});

test("register rejects resume with the wrong connector token", async () => {
  const { env } = fakeEnv();
  const first = await worker.fetch(new Request("https://relay.example/api/register", { method: "POST" }), env);
  assert.equal(first.status, 200);
  const route = await first.json();
  const resumed = await worker.fetch(new Request("https://relay.example/api/register", {
    method: "POST",
    body: JSON.stringify({
      route_id: route.route_id,
      connector_token: "wrong",
    }),
  }), env);
  assert.equal(resumed.status, 401);
});

test("internal register revives an expired route on token-matched resume", async () => {
  const renewed = new Date(Date.now() + 7 * DAY_MS).toISOString();
  const state = fakeState({
    connector_token: "tok",
    expires_at: new Date(Date.now() - 1000).toISOString(),
  });
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/internal/register", {
    method: "POST",
    body: JSON.stringify({ token: "tok", expires_at: renewed, resume: true }),
  }));
  assert.equal(res.status, 200);
  assert.equal(state.data.get("expires_at"), renewed);
});

test("resume against empty storage adopts the presented token", async () => {
  const state = fakeState({});
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/internal/register", {
    method: "POST",
    body: JSON.stringify({
      token: "tok",
      expires_at: new Date(Date.now() + 7 * DAY_MS).toISOString(),
      resume: true,
    }),
  }));
  assert.equal(res.status, 200);
  assert.equal(state.data.get("connector_token"), "tok");
});

test("navigation without a route serves the localStorage recovery page", async () => {
  const { env } = fakeEnv();
  const res = await worker.fetch(new Request("https://relay.example/", {
    headers: { Accept: "text/html,application/xhtml+xml" },
  }), env);
  assert.equal(res.status, 200);
  assert.match(res.headers.get("Content-Type") || "", /text\/html/);
  const body = await res.text();
  assert.match(body, /ibkrRemoteRoute/);
  assert.match(body, /canary app pair/);
  assert.doesNotMatch(body, /ibkr app pair/);
});

test("API request without a route still gets JSON 400", async () => {
  const { env } = fakeEnv();
  const res = await worker.fetch(new Request("https://relay.example/api/bootstrap"), env);
  assert.equal(res.status, 400);
  assert.match(res.headers.get("Content-Type") || "", /application\/json/);
});

test("navigation to an offline route serves the auto-retry page with cookie refresh", async () => {
  const env = {
    RELAY_SESSION: {
      idFromName: (name) => name,
      get: () => ({
        fetch: async () => new Response(JSON.stringify({ error: "Mac relay connector offline" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        }),
      }),
    },
  };
  const res = await worker.fetch(new Request("https://relay.example/", {
    headers: { Accept: "text/html", Cookie: `ibkr_remote_route=${VALID_ROUTE}` },
  }), env);
  assert.equal(res.status, 503);
  assert.match(res.headers.get("Content-Type") || "", /text\/html/);
  const body = await res.text();
  assert.match(body, /Retrying automatically/);
  assert.match(body, /canary app --remote/);
  assert.doesNotMatch(body, /ibkr app --remote/);
  assert.match(res.headers.get("Set-Cookie") || "", new RegExp(`ibkr_remote_route=${VALID_ROUTE}`));
});
