import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { once } from "node:events";
import test from "node:test";
import { deploy, main } from "../deploy.js";

const accountId = "0".repeat(32);
const token = "synthetic-private-token";
const root = `/client/v4/accounts/${accountId}/workers/scripts`;
const script = `${root}/ibkr-remote-relay`;
const initialSettings = {
  bindings: [
    { name: "RELAY_SESSION", type: "durable_object_namespace", class_name: "RelaySession", namespace_id: "existing-sqlite-namespace" },
    { name: "SECRET", type: "secret_text" },
    { name: "EXTRA", type: "plain_text", text: "synthetic-private-value" },
  ],
  compatibility_date: "2026-06-01",
  compatibility_flags: ["enable_request_signal", "request_signal_passthrough"],
  limits: { cpu_ms: 50 }, logpush: false, placement: { mode: "smart" },
  observability: { enabled: true }, tags: ["operator-owned"],
  tail_consumers: [{ service: "synthetic-log-consumer" }],
  future_setting: { preserve: true },
};

async function fixture(t, { exists = true, intercept } = {}) {
  const state = {
    exists, migration_tag: "v1", has_modules: true,
    settings: structuredClone(initialSettings), requests: [], uploads: [],
    rows: new Map([["paired-route", "durable-session-data"]]),
    routes: ["remote.example.test/*"], domains: ["remote.example.test"],
  };
  const server = createServer(async (req, res) => {
    try {
      assert.equal(req.headers.authorization, `Bearer ${token}`);
      state.requests.push([req.method, req.url]);
      const send = (result, status = 200) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ success: true, result }));
      };
      if (intercept && await intercept(req, res, state)) return;
      if (req.method === "GET" && req.url === root) {
        send(state.exists ? [{ id: "ibkr-remote-relay", migration_tag: state.migration_tag, has_modules: state.has_modules }] : []);
      } else if (req.method === "GET" && req.url === `${script}/settings`) {
        send(state.settings);
      } else if (req.method === "PUT" && req.url === `${script}/content`) {
        const request = new Request("http://fixture.invalid", { method: "PUT", headers: req.headers, body: req, duplex: "half" });
        const form = await request.formData();
        const metadataPart = form.get("metadata");
        const metadata = JSON.parse(typeof metadataPart === "string" ? metadataPart : await metadataPart.text());
        const source = form.get("worker.js");
        assert.equal(source.type, "application/javascript+module");
        assert.equal(await source.text(), await readFile(new URL("../src/worker.js", import.meta.url), "utf8"));
        assert.ok(state.exists, "a missing Worker cannot be created");
        assert.deepEqual(metadata, { main_module: "worker.js" }, "no settings or migrations may be uploaded");
        state.uploads.push(metadata);
        send({ migration_tag: state.migration_tag });
      } else {
        assert.fail(`unexpected endpoint: ${req.method} ${req.url}`);
      }
    } catch (error) {
      state.serverError = error;
      res.writeHead(500).end();
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => { server.closeAllConnections(); server.close(); assert.ifError(state.serverError); });
  const fetchImpl = (url, options) => {
    assert.equal(new URL(url).origin, "https://api.cloudflare.com");
    assert.equal(options.redirect, "error");
    assert.ok(options.signal instanceof AbortSignal);
    return fetch(`http://127.0.0.1:${server.address().port}${new URL(url).pathname}`, options);
  };
  return { state, run: (action = "--deploy") => deploy({ action, accountId, token, fetchImpl }), fetchImpl };
}

test("repeated deployments preserve the namespace, settings, session rows and routing", async (t) => {
  const { state, run } = await fixture(t);
  const before = structuredClone({ settings: state.settings, rows: state.rows, routes: state.routes, domains: state.domains });
  await run();
  await run();
  assert.equal(state.uploads.length, 2);
  assert.deepEqual({ settings: state.settings, rows: state.rows, routes: state.routes, domains: state.domains }, before);
  assert.deepEqual(state.requests.map(([method, path]) => `${method} ${path}`), Array(2).fill([
    `GET ${root}`, `GET ${script}/settings`, `PUT ${script}/content`, `GET ${script}/settings`,
  ]).flat());
});

test("missing Worker fails closed; check performs only reads", async (t) => {
  const { state, run } = await fixture(t, { exists: false });
  await assert.rejects(run(), /Worker is missing/);
  assert.equal(state.uploads.length, 0);
  await assert.rejects(run("--check"), /Worker is missing/);
  assert.deepEqual(state.requests, [["GET", root], ["GET", root]]);
  state.exists = true;
  await run("--check");
  assert.equal(state.uploads.length, 0);
  await run();
  assert.equal(state.uploads.length, 1);
});

test("migration, binding and compatibility drift fail before a write", async (t) => {
  for (const change of [
    (s) => { s.migration_tag = "v2"; },
    (s) => { delete s.migration_tag; },
    (s) => { s.has_modules = false; },
    (s) => { s.settings.bindings = []; },
    (s) => { s.settings.bindings[0].class_name = "DifferentClass"; },
    (s) => { s.settings.bindings[0].script_name = "different-worker"; },
    (s) => { s.settings.bindings[0].dispatch_namespace = "different-namespace"; },
    (s) => { delete s.settings.bindings[0].namespace_id; },
    (s) => { s.settings.bindings.push(s.settings.bindings[0]); },
    (s) => { s.settings.compatibility_date = "2026-06-02"; },
    (s) => { s.settings.compatibility_flags.pop(); },
  ]) {
    const { state, run } = await fixture(t);
    change(state);
    await assert.rejects(run());
    assert.equal(state.requests.filter(([method]) => method !== "GET").length, 0);
  }
});

test("API errors, invalid responses and redirects never imply a missing Worker", async (t) => {
  for (const [status, body, headers] of [
    [403, { success: false, errors: [{ code: 10000, message: token }] }],
    [404, { success: false, errors: [{ code: 10007, message: token }] }],
    [429, { success: false, errors: [{ code: 10001, message: token }] }],
    [200, { success: false, result: [] }],
    [200, { success: true, result: null }],
    [200, { success: true, result: {} }],
    [200, { success: true, result: [null] }],
    [502, `private response ${token}`],
    [302, "", { Location: `http://127.0.0.1:1/${token}` }],
  ]) {
    const { state, run } = await fixture(t, { intercept: (_req, res) => {
      res.writeHead(status, headers ?? {});
      res.end(typeof body === "string" ? body : JSON.stringify(body));
      return true;
    } });
    await assert.rejects(run(), (error) => !error.message.includes(token));
    assert.equal(state.requests.length, 1);
    assert.equal(state.requests[0][0], "GET");
  }
});

test("failed writes and failed verification never retry or claim success", async (t) => {
  for (const failure of ["write", "response", "settings", "readback"]) {
    const { state, run } = await fixture(t, { intercept: (req, res, s) => {
      if (failure === "write" && req.method === "PUT") {
        res.writeHead(500).end(JSON.stringify({ success: false, errors: [{ code: 10000, message: token }] }));
        return true;
      }
      if (req.method === "PUT" && failure === "response") s.migration_tag = "unexpected";
      if (req.method === "GET" && s.uploads.length && failure === "settings") s.settings.bindings[0].namespace_id = "changed-namespace";
      if (req.method === "GET" && s.uploads.length && failure === "readback") { res.writeHead(503).end(); return true; }
      return false;
    } });
    await assert.rejects(run(), (error) => /write may have applied/.test(error.message) && !error.message.includes(token));
    assert.equal(state.requests.filter(([method]) => method === "PUT").length, 1);
  }
});

test("CLI rejects missing action and credentials without network or credential disclosure", async () => {
  for (const [args, env] of [
    [[], {}], [["--deploy"], {}], [["--deploy"], { CLOUDFLARE_ACCOUNT_ID: token }],
    [[token], { CLOUDFLARE_ACCOUNT_ID: accountId, CLOUDFLARE_API_TOKEN: token }],
    [["--create"], { CLOUDFLARE_ACCOUNT_ID: accountId, CLOUDFLARE_API_TOKEN: token }],
    [["--deploy"], { CLOUDFLARE_ACCOUNT_ID: accountId, CLOUDFLARE_API_TOKEN: `${token}\n` }],
  ]) {
    const output = [];
    const code = await main(args, env, { stdout: (s) => output.push(s), stderr: (s) => output.push(s), fetchImpl: () => assert.fail("no request expected") });
    assert.equal(code, 1);
    assert.equal(output.length, 1);
    assert.ok(!output[0].includes(token));
  }
  const output = [];
  assert.equal(await main(["--deploy"], { CLOUDFLARE_ACCOUNT_ID: accountId, CLOUDFLARE_API_TOKEN: token }, {
    stdout: (s) => output.push(s), stderr: (s) => output.push(s),
    fetchImpl: () => { throw new Error(`transport failure ${token}`); },
  }), 1);
  assert.ok(!output.join().includes(token));
});
