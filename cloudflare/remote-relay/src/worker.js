const ROUTE_COOKIE = "ibkr_remote_route";
// These route/storage identifiers and the deployed Worker/DO names are
// not part of Canary's source-level product rename.
const CONNECTOR_TOKEN_KEY = "connector_token";
const EXPIRES_AT_KEY = "expires_at";
const ROUTE_TTL_MS = 7 * 24 * 60 * 60 * 1000;
// The route cookie only names the route; it grants nothing without the Mac
const ROUTE_COOKIE_MAX_AGE_S = 400 * 24 * 60 * 60;
const ROUTE_ID_RE = /^r_[A-Za-z0-9_-]{16,128}$/;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return json({ ok: true, service: "ibkr-remote-relay" });
    }
    if (url.pathname === "/api/register" && request.method === "POST") {
      return registerRoute(request, env);
    }
    if (url.pathname === "/api/connect") {
      const routeID = url.searchParams.get("route_id") || "";
      if (!routeID) return json({ error: "route_id required" }, 400);
      if (!ROUTE_ID_RE.test(routeID)) return json({ error: "invalid route_id" }, 400);
      return routeStub(env, routeID).fetch(request);
    }

    const routeID = url.searchParams.get("remote") || readCookie(request.headers.get("Cookie") || "", ROUTE_COOKIE);
    if (!routeID) {
      // A navigation without a route cookie is an installed PWA whose
      // a JSON dead end that reads as "you must re-pair".
      if (wantsHTML(request)) return recoveryPage();
      return json({ error: "remote route required" }, 400);
    }
    if (!ROUTE_ID_RE.test(routeID)) return json({ error: "invalid remote route" }, 400);
    let response = await routeStub(env, routeID).fetch(request);
    // While the Mac connector is down or the route aged out, a navigation
    // must keep polling instead of stranding the user on raw JSON: the Mac
    if (wantsHTML(request) && (response.status === 503 || response.status === 410)) {
      response = retryPage(response.status);
    }
    // Re-set the route cookie on every addressed request, cookie- or
    // relay by cookie only, so each visit must renew the cookie's clock.
    const routed = new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers: response.headers,
    });
    routed.headers.append("Set-Cookie", routeCookie(routeID));
    return routed;
  },
};

function wantsHTML(request) {
  if (request.method !== "GET") return false;
  return (request.headers.get("Accept") || "").includes("text/html");
}

function htmlPage(body, status = 200) {
  return new Response(body, {
    status,
    headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" },
  });
}

function recoveryPage() {
  return htmlPage(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Canary · reconnect</title>
<body style="font-family:-apple-system,sans-serif;padding:2rem;text-align:center">
<p id="msg">Reconnecting to your Mac…</p>
<script>
  const route = localStorage.getItem("ibkrRemoteRoute");
  if (route) {
    location.replace("/?remote=" + encodeURIComponent(route));
  } else {
    document.getElementById("msg").textContent =
      "This phone has no saved remote route. Scan a pairing QR code from \`canary app pair\` on the Mac.";
  }
</script>
</body>`);
}

function retryPage(status) {
  const why = status === 410
    ? "The Mac has been offline for a while; waiting for it to come back."
    : "The Mac relay connector is offline; waiting for it to come back.";
  return htmlPage(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Canary · waiting for Mac</title>
<body style="font-family:-apple-system,sans-serif;padding:2rem;text-align:center">
<p>${why}</p>
<p style="color:#888">Retrying automatically. If this never recovers, restart \`canary app --remote\` on the Mac.</p>
<script>setTimeout(() => location.reload(), 5000);</script>
</body>`, status);
}

export class RelaySession {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.connector = null;
    this.inflight = new Map();
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === "/internal/register" && request.method === "POST") {
      const body = await request.json();
      const existing = await this.state.storage.get(CONNECTOR_TOKEN_KEY);
      if (body.resume && existing && existing !== body.token) {
        return json({ error: "unauthorized route resume" }, 401);
      }
      // A token-matched resume always revives the route, even past its
      // storage adopts the presented token so a relay redeploy or DO
      // migration cannot permanently orphan the paired fleet.
      await this.state.storage.put(CONNECTOR_TOKEN_KEY, body.token);
      await this.state.storage.put(EXPIRES_AT_KEY, body.expires_at);
      return json({ ok: true });
    }
    if (url.pathname === "/api/connect") {
      return this.connect(request);
    }
    return this.forward(request);
  }

  async connect(request) {
    const expected = await this.state.storage.get(CONNECTOR_TOKEN_KEY);
    const token = connectorToken(request);
    if (!expected || token !== expected) {
      return json({ error: "unauthorized" }, 401);
    }
    // Slide the route TTL on every authenticated connector (re)connection —
    // even one past its expiry: route identity must survive any Mac
    // only gates phone-side forwards while the Mac is away. The Go
    await this.slideExpiry();
    if (request.headers.get("Upgrade") !== "websocket") {
      return json({ error: "websocket upgrade required" }, 426);
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    server.accept();
    if (this.connector) this.connector.close(1012, "connector replaced");
    this.connector = server;
    server.addEventListener("message", (event) => this.handleConnectorMessage(event.data));
    server.addEventListener("close", () => this.connectorClosed(server));
    server.addEventListener("error", () => this.connectorClosed(server));
    return new Response(null, { status: 101, webSocket: client });
  }

  async forward(request) {
    if (await this.expired()) return json({ error: "route expired" }, 410);
    if (!this.connector) return json({ error: "Mac relay connector offline" }, 503);
    if (request.signal?.aborted) return json({ error: "client disconnected" }, 499);
    const id = crypto.randomUUID();
    const body = await request.arrayBuffer();
    if (request.signal?.aborted) return json({ error: "client disconnected" }, 499);
    const { readable, writable } = new TransformStream();
    const writer = writable.getWriter();
    const connector = this.connector;
    const start = new Promise((resolve) => {
      const abortHandler = () => {
        this.failInflight(id, 499, "client disconnected", true);
      };
      this.inflight.set(id, {
        resolve,
        readable,
        writer,
        started: false,
        sent: false,
        connector,
        signal: request.signal,
        abortHandler,
      });
      request.signal?.addEventListener("abort", abortHandler, { once: true });
      if (request.signal?.aborted) abortHandler();
    });
    try {
      connector.send(JSON.stringify({
        type: "request",
        id,
        method: request.method,
        path: requestPath(request.url),
        headers: headerMap(request.headers),
        body: bytesToBase64(new Uint8Array(body)),
      }));
      const pending = this.inflight.get(id);
      if (pending) pending.sent = true;
    } catch {
      this.failInflight(id, 503, "Mac relay connector unavailable", false);
    }
    return start;
  }

  async handleConnectorMessage(raw) {
    let frame;
    try {
      frame = JSON.parse(raw);
    } catch {
      return;
    }
    const pending = this.inflight.get(frame.id);
    if (!pending) return;
    if (frame.type === "response_start") {
      if (pending.started) return;
      const headers = new Headers();
      for (const [key, values] of Object.entries(frame.headers || {})) {
        for (const value of values) headers.append(key, value);
      }
      pending.started = true;
      pending.resolve(new Response(pending.readable, {
        status: frame.status || 200,
        headers,
      }));
      return;
    }
    if (frame.type === "response_chunk") {
      if (!pending.started) return;
      try {
        await pending.writer.write(base64ToBytes(frame.body || ""));
      } catch {
        // The phone may have disconnected while this write was pending.
      }
      return;
    }
    if (frame.type === "response_end") {
      const finished = this.takeInflight(frame.id);
      if (!finished) return;
      try {
        await finished.writer.close();
      } catch {
        // A concurrent phone disconnect already aborted the stream.
      }
      return;
    }
    if (frame.type === "response_error") {
      const failed = this.takeInflight(frame.id);
      if (!failed) return;
      const status = frame.status || 502;
      const body = JSON.stringify({ error: frame.error || "relay error" });
      if (!failed.started) {
        failed.resolve(new Response(body, {
          status,
          headers: { "Content-Type": "application/json" },
        }));
      } else {
        try {
          await failed.writer.abort(frame.error || "relay error");
        } catch {
          // A concurrent phone disconnect already aborted the stream.
        }
      }
    }
  }

  connectorClosed(connector) {
    if (this.connector === connector) this.connector = null;
    for (const [id, pending] of [...this.inflight.entries()]) {
      if (pending.connector !== connector) continue;
      this.failInflight(id, 503, "Mac relay connector disconnected", false);
    }
  }

  takeInflight(id) {
    const pending = this.inflight.get(id);
    if (!pending) return null;
    this.inflight.delete(id);
    pending.signal?.removeEventListener("abort", pending.abortHandler);
    return pending;
  }

  failInflight(id, status, error, notifyConnector) {
    const pending = this.takeInflight(id);
    if (!pending) return;
    if (notifyConnector && pending.sent) {
      try {
        pending.connector.send(JSON.stringify({ type: "request_cancel", id }));
      } catch {
        // Connector failure is handled by its close/error listener.
      }
    }
    if (!pending.started) {
      pending.resolve(json({ error }, status));
    } else {
      pending.writer.abort(error).catch(() => {});
    }
  }

  async expired() {
    const raw = await this.state.storage.get(EXPIRES_AT_KEY);
    return !!raw && Date.parse(raw) <= Date.now();
  }

  async slideExpiry() {
    await this.state.storage.put(EXPIRES_AT_KEY, new Date(Date.now() + ROUTE_TTL_MS).toISOString());
  }
}

async function registerRoute(request, env) {
  const body = await registerRequestBody(request);
  const wantsResume = !!(body.route_id || body.connector_token);
  if (wantsResume && (!body.route_id || !body.connector_token)) {
    return json({ error: "route_id and connector_token are both required to resume a route" }, 400);
  }
  if (body.route_id && !ROUTE_ID_RE.test(body.route_id)) {
    return json({ error: "invalid route_id" }, 400);
  }
  const routeID = body.route_id || `r_${randomToken(18)}`;
  const token = body.connector_token || randomToken(32);
  const expiresAt = new Date(Date.now() + ROUTE_TTL_MS).toISOString();
  const origin = new URL(request.url).origin;
  const stub = routeStub(env, routeID);
  const register = await stub.fetch(new Request(`${origin}/internal/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token, expires_at: expiresAt, resume: wantsResume }),
  }));
  if (!register.ok) {
    let err = { error: register.statusText || "register route failed" };
    try {
      err = await register.json();
    } catch {
      // Keep the generic body.
    }
    return json(err, register.status);
  }
  return json({
    route_id: routeID,
    public_url: origin,
    connector_url: `${origin.replace("https://", "wss://").replace("http://", "ws://")}/api/connect?route_id=${encodeURIComponent(routeID)}&token=${encodeURIComponent(token)}`,
    connector_token: token,
    expires_at: expiresAt,
  });
}

async function registerRequestBody(request) {
  const raw = await request.text();
  if (!raw.trim()) return {};
  try {
    return JSON.parse(raw);
  } catch {
    return {};
  }
}

function routeStub(env, routeID) {
  return env.RELAY_SESSION.get(env.RELAY_SESSION.idFromName(routeID));
}

function json(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function requestPath(rawURL) {
  const url = new URL(rawURL);
  return `${url.pathname}${url.search}`;
}

function headerMap(headers) {
  const out = {};
  for (const [key, value] of headers) {
    if (skipHeader(key)) continue;
    if (!out[key]) out[key] = [];
    out[key].push(value);
  }
  return out;
}

function skipHeader(key) {
  return ["connection", "upgrade", "transfer-encoding"].includes(key.toLowerCase());
}

function connectorToken(request) {
  const auth = request.headers.get("Authorization") || "";
  if (auth.startsWith("Bearer ")) return auth.slice("Bearer ".length).trim();
  return new URL(request.url).searchParams.get("token") || "";
}

function routeCookie(routeID) {
  return `${ROUTE_COOKIE}=${encodeURIComponent(routeID)}; Path=/; Max-Age=${ROUTE_COOKIE_MAX_AGE_S}; Secure; HttpOnly; SameSite=Lax`;
}

function readCookie(raw, name) {
  for (const part of raw.split(";")) {
    const [key, value] = part.trim().split("=");
    if (key === name) return decodeURIComponent(value || "");
  }
  return "";
}

function randomToken(byteLength) {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  return bytesToBase64URL(bytes);
}

function bytesToBase64URL(bytes) {
  return bytesToBase64(bytes).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function bytesToBase64(bytes) {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}

function base64ToBytes(raw) {
  const binary = atob(raw);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

export const __test = {
  requestPath,
  headerMap,
  readCookie,
  routeCookie,
};
