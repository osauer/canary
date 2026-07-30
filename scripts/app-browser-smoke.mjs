#!/usr/bin/env node

import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import { extname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createPairingSession, launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

const args = parseArgs(process.argv.slice(2));
const baseURL = trimRight(args["base-url"] || "http://127.0.0.1:8765", "/");
const pairPublicURL = trimRight(args["pair-public-url"] || baseURL, "/");
const browserName = args.browser || "chromium";
const channel = args.channel || process.env.PLAYWRIGHT_CHANNEL || "";
const noNotification = args["no-notification"] !== "false";
const noWebCrypto = args["no-webcrypto"] === "true";
const lifecycle = args.lifecycle === "true";
const restartCommand = args["restart-command"] || "";
const stopRestartedApp = args["stop-restarted-app"] === "true";
const mobile = args.mobile !== "false";
const round4Synthetic = args["round4-synthetic"] === "true";
const rawGatewayCopyPattern = /gateway_unavailable|ibkr connection unavailable|quote\.snapshot|account\.summary|positions\.list/i;
const staleStressDomainCopyPattern = /\b(?:canary (?:snapshot|driver|drivers|trigger|market read|portfolio snapshot)|defensive canary action|canary as a market signal)\b/i;

const playwright = loadPlaywright("app-browser-smoke");

if (!playwright[browserName]) {
  console.error(`app-browser-smoke: unknown browser ${browserName}`);
  process.exit(2);
}

if (round4Synthetic) {
  await runRound4SyntheticSmoke();
  process.exit(0);
}

const pairing = await createPairingSession(baseURL, pairPublicURL);
const launchOptions = { headless: true };
if (channel) {
  launchOptions.channel = channel;
}

async function runRound4SyntheticSmoke() {
  const syntheticURL = "http://canary-synthetic.invalid/";
  const staticRoot = resolve(fileURLToPath(new URL("../web/app/", import.meta.url)));
  const staticTypes = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".json": "application/json", ".webmanifest": "application/manifest+json" };
  const launchedSynthetic = await launchBrowser(playwright[browserName], browserName, { headless: true, ...(channel ? { channel } : {}) });
  const browser = launchedSynthetic.browser;
  const mutationRequests = [];
  let attention = {
    unread_count: 2,
    high_water_seq: 4,
    read_through_seq: 2,
    unread_refs: [
      { display_id: "alert-0123456789abcdef", source: "canary", kind: "portfolio_risk" },
      { display_id: "alert-abcdef0123456789", source: "governance", kind: "governance" },
    ],
  };
  const now = new Date().toISOString();
  const freshUntil = new Date(Date.now() + 10 * 60_000).toISOString();
  const earlier = new Date(Date.now() - 60_000).toISOString();
  const alertSource = (source) => ({ source, status: "current", reason: "authoritative", evidence_health: "current", input_as_of: now, observed_at: now, evidence_as_of: now, fresh_until: freshUntil, covered: true });
  const alertOccurrence = (value) => ({
    display_id: value.display_id, source: value.source, kind: value.kind,
    presentation_code: value.presentation_code, title: value.title, body: value.body,
    state: value.state, severity: value.severity, evidence_health: "current", destination: "alerts",
    evidence_as_of: now, state_changed_at: now, first_seen_at: earlier, last_seen_at: now,
    ended_at: value.ended_at, end_reason: value.end_reason, attention_seq: value.attention_seq,
    disposition: "push_service_accepted",
  });
  let alerts = {
    schema_version: "alerts-v1", version: "alert-delivery-v4", initialized: true, generation: 1,
    as_of: now, current_state: "active",
    coverage: { state: "complete", freshness: "current", as_of: now, expected_sources: ["canary", "governance"], covered_sources: ["canary", "governance"] },
    sources: [alertSource("canary"), alertSource("governance")],
    occurrences: [
      alertOccurrence({ display_id: "alert-0123456789abcdef", source: "canary", kind: "portfolio_risk", presentation_code: "portfolio_stress", title: "Synthetic watch", body: "Review the current Canary alert.", state: "open", severity: "watch", ended_at: null, end_reason: null, attention_seq: 3 }),
      alertOccurrence({ display_id: "alert-abcdef0123456789", source: "governance", kind: "governance", presentation_code: "governance_monthly_pulse", title: "Synthetic process review", body: "Review the retained process alert.", state: "recovered", severity: "act", ended_at: now, end_reason: "recovered", attention_seq: 4 }),
    ],
    attention,
    delivery_health: { state: "healthy", class: "", updated_at: now, last_push_service_acceptance_at: now },
  };
  const readyInput = { status: "ok", as_of: now };
  const governance = {
    candidates: [],
    source_health: {},
    poll_source: {},
    occurrences: [{
      display_id: "gov-synthetic-4",
      title: "Synthetic process review",
      body: "Review the retained governance occurrence.",
      severity: "act",
      destination: "alerts",
      occurred_at: now,
    }],
    attempts: [],
    attempt_aggregate: {},
    health_aggregate: {},
    delivery_health: { state: "healthy", updated_at: now },
    diagnostic: { state: "push_service_accepted", at: now },
  };
  const bootstrap = {
    auth: { authenticated: true },
    alert_settings: { mode: "watch_and_act" },
    alerts,
    governance,
    settings: null,
    vapid_public_key: "",
    snapshot: {
      account: {},
      positions: { stocks: [], options: [], portfolio: {} },
      stress: { portfolio_fit: "low", portfolio: {}, fingerprint: { key: "synthetic-stress" } },
      trading: { mode: "disabled", can_preview: false, can_write: false },
      proposals: {},
      opportunities: {},
      sources: { nudges: { state: "current", updated_at: now, last_success_at: now } },
      nudges: {
        as_of: now,
        candidates: [{ title: "Synthetic process review", body: "Review the current process exception.", severity: "act", destination: "alerts" }],
        source_health: {
          aggregate: "degraded", policy: readyInput, reconciliation: readyInput, capital: readyInput,
          pins: readyInput, cadence: readyInput,
          confirmed_flow: { status: "unapproved", reason: "cutover_review_required", as_of: now },
        },
        context: { shadow: { count: 1 }, drawdown: { tier: "block", consumed_pct: 0 } },
        confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: true },
      },
    },
  };
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await context.addInitScript(() => {
    globalThis.__canarySmoke = { applySnapshotPatch: null };
    try { Object.defineProperty(globalThis, "Notification", { configurable: true, value: undefined }); } catch {}
    try { Object.defineProperty(globalThis, "EventSource", { configurable: true, value: undefined }); } catch {}
  });
  await context.route("http://canary-synthetic.invalid/**", async (route) => {
    const request = route.request();
    const requestURL = new URL(request.url());
    const requestPath = requestURL.pathname;
    const method = request.method();
    if (!['GET', 'HEAD'].includes(method)) {
      mutationRequests.push({ method, path: requestPath, body: request.postData() || "" });
    }
    const json = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (method === "GET" && requestPath === "/api/bootstrap") return json({ ...bootstrap, alerts });
    if (method === "GET" && requestPath === "/api/alerts/attention") return json(attention);
    if (method === "GET" && requestPath === "/api/alerts") return json(alerts);
    if (method === "GET" && requestPath === "/api/governance") return json(governance);
    if (method === "GET" && requestPath === "/api/orders/open") return json({ orders: [] });
    if (method === "GET" && requestPath === "/api/purge/status") return json({ entries: [] });
    if (method === "POST" && requestPath === "/api/alerts/attention/read") {
      const body = request.postDataJSON();
      if (Object.keys(body).length !== 1 || body.through_seq !== 4) return json({ error: "unexpected synthetic watermark" }, 400);
      attention = { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] };
      alerts = { ...alerts, generation: alerts.generation + 1, attention };
      return json(alerts);
    }
    if (!['GET', 'HEAD'].includes(method)) return json({ error: "synthetic mutation blocked" }, 503);
    if (requestPath.startsWith("/api/")) return json({});
    try {
      const relative = requestPath === "/" ? "index.html" : requestPath.slice(1);
      if (!/^[A-Za-z0-9._/-]+$/.test(relative) || relative.includes("..")) throw new Error("invalid path");
      const body = await readFile(resolve(staticRoot, relative));
      return route.fulfill({ status: 200, contentType: staticTypes[extname(relative)] || "application/octet-stream", body, headers: { "Cache-Control": "no-store" } });
    } catch {
      return route.fulfill({ status: 404, contentType: "text/plain", body: "not found" });
    }
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error?.message || error)));
  page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
  try {
    await page.goto(syntheticURL, { waitUntil: "domcontentloaded" });
    await page.waitForFunction(() => document.getElementById("dashboard")?.hidden === false, { timeout: 10000 });
    await assertVisibleRenameContract(page);
    try {
      await page.waitForFunction(() => document.getElementById("alertUnreadBadge")?.textContent === "2", { timeout: 5000 });
    } catch (error) {
      throw new Error(`synthetic unread did not render: ${errors.join(" | ") || error.message}`);
    }
    const monitor = await page.evaluate(() => ({
      active: document.getElementById("tabMonitor")?.classList.contains("active"),
      badge: document.getElementById("alertUnreadBadge")?.textContent || "",
      label: document.getElementById("tabAlerts")?.getAttribute("aria-label") || "",
    }));
    await page.locator("#tabAlerts").click();
    await page.waitForFunction(() => document.getElementById("alertUnreadBadge")?.hidden === true, { timeout: 5000 });
    const alertsView = await page.evaluate(() => ({
      detailsOpen: document.getElementById("governanceEvidenceDetails")?.open,
      cutoverVisible: document.getElementById("governanceCutoverReviewButton")?.hidden === false,
      coverage: document.getElementById("governanceCoverage")?.textContent || "",
      activeAlerts: document.getElementById("currentSignalList")?.textContent || "",
      endedAlerts: document.getElementById("alertHistoryList")?.textContent || "",
      authority: document.getElementById("alertAuthorityState")?.textContent || "",
      governanceHistory: document.getElementById("governanceHistoryList")?.textContent || "",
      litTiles: document.querySelectorAll("#currentSignalList .alert-row.pd-tile--watch").length,
      outTiles: document.querySelectorAll("#alertHistoryList .alert-row.pd-alert--out").length,
      authoritySeated: document.getElementById("lampTestDialog")?.contains(document.getElementById("alertAuthorityState")) === true,
    }));
    await page.locator("#tabSettings").click();
    const settings = await page.evaluate(() => ({
      modes: [...document.querySelectorAll("#alertSegments button")].map((button) => button.textContent.trim()),
      copy: document.querySelector(".settings-notification-card")?.textContent || "",
      pushState: document.getElementById("pushState")?.textContent || "",
    }));
    if (!monitor.active || monitor.badge !== "2" || monitor.label !== "Alerts, 2 unread") throw new Error(`synthetic unread monitor state failed: ${JSON.stringify(monitor)}`);
    if (alertsView.detailsOpen !== false || !alertsView.cutoverVisible || !alertsView.coverage.includes("Older payments need a one-time review") || !alertsView.activeAlerts.includes("Synthetic watch") || !alertsView.endedAlerts.includes("Synthetic process review") || alertsView.authority !== "Active" || !alertsView.governanceHistory.includes("Synthetic process review")) throw new Error(`synthetic Alerts state failed: ${JSON.stringify(alertsView)}`);
    // The log renders as annunciator tiles (watch lit, ended unlit) and the
    // alert authority now reports from inside the lamp-test detail.
    if (alertsView.litTiles !== 1 || alertsView.outTiles !== 1 || !alertsView.authoritySeated) throw new Error(`synthetic annunciator log failed: ${JSON.stringify(alertsView)}`);
    if (JSON.stringify(settings.modes) !== JSON.stringify(["Off", "Action required", "Watch + action"]) || !settings.copy.includes("global for this app host and all paired devices") || !settings.copy.includes("Off stops phone notifications while your in-app history remains") || !settings.copy.includes("Action required sends urgent items only") || !settings.copy.includes("Watch + action also sends review reminders") || !settings.copy.includes("not configured here") || !settings.copy.includes("shared across paired devices") || settings.pushState !== "unsupported") throw new Error(`synthetic Settings state failed: ${JSON.stringify(settings)}`);
    if (mutationRequests.length !== 1 || mutationRequests[0].method !== "POST" || mutationRequests[0].path !== "/api/alerts/attention/read" || JSON.parse(mutationRequests[0].body).through_seq !== 4) throw new Error(`unexpected synthetic mutations: ${JSON.stringify(mutationRequests)}`);
    if (errors.length > 0) throw new Error(`synthetic browser errors: ${errors.join("\n")}`);
    console.log(JSON.stringify({ ok: true, browser: browserName, mobile: true, isolated: true, monitor, alerts: alertsView, settings, intercepted_mutations: mutationRequests.map(({ method, path }) => ({ method, path })) }, null, 2));
  } finally {
    await browser.close();
  }
}
const launched = await launchBrowser(playwright[browserName], browserName, launchOptions);
const browser = launched.browser;
let cleanupPID = 0;
const context = await browser.newContext({
  viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
  isMobile: mobile,
  hasTouch: mobile,
});
// The operator's real unread attention is human-only evidence: this smoke
// drives the real shared host in a headless page that reports itself
// "visible", so opening the Alerts tab would POST /api/alerts/attention/read with
// the real high-water and silently mark the operator's unread as read (same
// hazard class as the guarded /api/brief/seen render stamp). Intercept the
// POST before any page interaction, never forward it, and answer with the
// shape the SPA expects so its state machine stays coherent.
// The SPA's service worker claims its clients immediately (skipWaiting +
// clients.claim), and WebKit never surfaces SW-controlled page fetches to
// Playwright's network routes — a context.route here silently lets the POST
// reach the real host. The primary guard therefore diverts the POST inside
// the page's wrapped fetch (init script below), before any network layer can
// see it; this route stays only as a second net for engines and windows
// where routing does observe the request.
let attentionReadIntercepted = 0;
let attentionReadRouted = 0;
await context.route(`${baseURL}/api/alerts/attention/read`, async (route) => {
  if (route.request().method() !== "POST") {
    await route.fallback();
    return;
  }
  let throughSeq = 0;
  try {
    const parsed = JSON.parse(route.request().postData() || "{}");
    throughSeq = Number.isFinite(Number(parsed.through_seq)) ? Number(parsed.through_seq) : 0;
  } catch {
    // Malformed body still must not reach the real host; answer neutrally.
  }
  attentionReadRouted += 1;
  await route.fulfill({
    status: 409,
    contentType: "application/json",
    body: JSON.stringify({ error: "browser smoke diverted attention read", through_seq: throughSeq }),
  });
});
// Second net for the render stamp (see the wrapped-fetch divert init script
// below): the primary guard is the page-level fetch wrapper because WebKit
// hides SW-controlled fetches from routing, so this route only fires on engines
// and windows where routing observes the request. It must never forward.
await context.route(`${baseURL}/api/brief/seen`, async (route) => {
  if (route.request().method() !== "POST") {
    await route.fallback();
    return;
  }
  let kind = "morning";
  try {
    const parsed = JSON.parse(route.request().postData() || "{}");
    if (typeof parsed.kind === "string" && parsed.kind) kind = parsed.kind;
  } catch {
    // Malformed body still must not reach the real host.
  }
  await route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify({ ok: true, kind, day: "2026-01-01", already_stamped: false, brief_fingerprint: "smoke-diverted" }),
  });
});
async function attentionReadInterceptedCount(page) {
  const diverted = await page.evaluate(() => globalThis.__canarySmoke?.attentionReadDiverted || 0);
  return diverted + attentionReadRouted;
}
if (noNotification) {
  await context.addInitScript(() => {
    try {
      Object.defineProperty(globalThis, "Notification", {
        configurable: true,
        value: undefined,
      });
    } catch {
      // Some engines make host globals non-configurable. The smoke still
      // catches ordinary browser errors through pageerror/console.
    }
  });
}
if (noWebCrypto) {
  await context.addInitScript(() => {
    try {
      const proto = Object.getPrototypeOf(globalThis.crypto);
      Object.defineProperty(proto, "subtle", {
        configurable: true,
        get() {
          return undefined;
        },
      });
    } catch {
      try {
        Object.defineProperty(globalThis.crypto, "subtle", {
          configurable: true,
          value: undefined,
        });
      } catch {
        // The final JSON reports whether the fallback path was used.
      }
    }
  });
}
await context.addInitScript(() => {
  globalThis.__canarySmoke = {
    eventCounts: {},
    fetches: [],
    openedEvents: 0,
    attentionReadDiverted: 0,
    briefSeenDiverted: 0,
  };
  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (...fetchArgs) => {
    const request = fetchArgs[0];
    const url = typeof request === "string" ? request : request?.url || "";
    const method = String((typeof request === "string" ? fetchArgs[1]?.method : request?.method || fetchArgs[1]?.method) || "GET").toUpperCase();
    if (method === "POST" && url.endsWith("/api/alerts/attention/read")) {
      // The QA page must never mark the operator's real unread as read.
      // Divert before any network layer (service-worker control hides this
      // request from Playwright routing in WebKit) and answer with the
      // current full DTO shape the SPA expects without advancing the cursor.
      let throughSeq = 0;
      try {
        const raw = typeof request === "string" ? fetchArgs[1]?.body : await request.clone().text();
        const parsed = JSON.parse(raw || "{}");
        if (Number.isFinite(Number(parsed.through_seq))) throughSeq = Number(parsed.through_seq);
      } catch {
        // Malformed body still must not reach the real host.
      }
      globalThis.__canarySmoke.attentionReadDiverted += 1;
      globalThis.__canarySmoke.fetches.push({ url, status: 200, diverted: true, at: Date.now() });
      const current = await nativeFetch("/api/alerts", { credentials: "include" });
      return new Response(
        await current.text(),
        { status: current.status, headers: { "Content-Type": "application/json", "X-Smoke-Through-Seq": String(throughSeq) } },
      );
    }
    if (method === "POST" && url.endsWith("/api/brief/seen")) {
      // The render-stamp is human-only evidence: a QA page that reports itself
      // visible would stamp the operator's real brief the instant the Brief tab
      // renders. Divert before any network layer (SW control hides this fetch
      // from Playwright routing in WebKit, exactly like /api/alerts/attention/read) and
      // answer with a receipt the render-stamp state machine accepts.
      let kind = "morning";
      try {
        const raw = typeof request === "string" ? fetchArgs[1]?.body : await request.clone().text();
        const parsed = JSON.parse(raw || "{}");
        if (typeof parsed.kind === "string" && parsed.kind) kind = parsed.kind;
      } catch {
        // Malformed body still must not reach the real host.
      }
      globalThis.__canarySmoke.briefSeenDiverted += 1;
      globalThis.__canarySmoke.fetches.push({ url, status: 200, diverted: true, at: Date.now() });
      return new Response(
        JSON.stringify({ ok: true, kind, day: "2026-01-01", already_stamped: false, brief_fingerprint: "smoke-diverted" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    try {
      const res = await nativeFetch(...fetchArgs);
      globalThis.__canarySmoke.fetches.push({ url, status: res.status, at: Date.now() });
      if (res.ok && url.endsWith("/api/bootstrap")) {
        res.clone().json().then((body) => {
          globalThis.__canarySmoke.latestStressHeldStress = body?.snapshot?.stress?.portfolio?.held_stress?.length || 0;
        }).catch(() => {});
      }
      return res;
    } catch (err) {
      globalThis.__canarySmoke.fetches.push({ url, error: String(err?.message || err), at: Date.now() });
      throw err;
    }
  };
  const NativeEventSource = globalThis.EventSource;
  globalThis.EventSource = function smokeEventSource(url, options) {
    const es = new NativeEventSource(url, options);
    globalThis.__canarySmoke.openedEvents++;
    // Fixture phases freeze the live stream for the APP's listeners only: a
    // real SSE snapshot landing between applySnapshotPatch and its assertion
    // repaints the operator's actual desk state over the fixture (the
    // governance monthly-pulse assertion caught exactly that). The smoke's
    // own counting listeners below register through nativeAdd and keep
    // observing the wire while frozen. Mirror of the app-screenshots.mjs guard.
    const nativeAdd = es.addEventListener.bind(es);
    es.addEventListener = (type, listener, ...rest) => {
      nativeAdd(type, (event) => {
        if (globalThis.__canarySmoke?.freezeLiveEvents && type !== "open") return;
        if (typeof listener === "function") listener.call(es, event);
        else listener.handleEvent(event);
      }, ...rest);
    };
    for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "market_quotes", "stress", "rules", "nudges", "heartbeat"]) {
      nativeAdd(type, (event) => {
        globalThis.__canarySmoke.eventCounts[type] = (globalThis.__canarySmoke.eventCounts[type] || 0) + 1;
        if (type === "snapshot" || type === "stress") {
          try {
            const data = JSON.parse(event.data);
            const stress = type === "snapshot" ? data?.stress : data;
            globalThis.__canarySmoke.latestStressHeldStress = stress?.portfolio?.held_stress?.length || 0;
          } catch {
            // Smoke assertions below stay DOM-based when payload inspection fails.
          }
        }
        if (type === "snapshot" || type === "rules") {
          try {
            const data = JSON.parse(event.data);
            const rules = type === "snapshot" ? data?.rules : data;
            globalThis.__canarySmoke.latestRulesCount = rules?.rules?.length || 0;
          } catch {
            // DOM assertions still cover the card when payload capture fails.
          }
        }
      });
    }
    return es;
  };
  globalThis.EventSource.prototype = NativeEventSource.prototype;
});
const page = await context.newPage();
const consoleMessages = [];
const pageErrors = [];
page.on("console", (msg) => {
  if (msg.type() === "error" || msg.type() === "warning") {
    const text = msg.text();
    if (/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(text)) {
      return;
    }
    consoleMessages.push(`${msg.type()}: ${text}`);
  }
});
page.on("pageerror", (err) => pageErrors.push(String(err?.message || err)));

try {
  await page.goto(pairing.url, { waitUntil: "domcontentloaded", timeout: 15000 });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await waitForSnapshotEvent(page, 0);
  const title = await page.title();
  const visibleIdentity = await assertVisibleRenameContract(page);
  const connection = await waitForHeader(page);
  const pushState = await page.locator("#pushState").textContent();
  const eventsBefore = await fetchEventsDiagnostics(page);
  const privacy = await exerciseAccountPrivacy(page);
  const accountPanel = await exerciseAccountPanel(page);
  const snapshotBanner = await assertSnapshotBannerCopy(page);
  const marketLayout = await exerciseMarketLayout(page);
  const viewportOverflow = await assertNoViewportOverflow(page);
  const stressControls = await exerciseStressControlsRemoved(page);
  const underlyingBookFixture = await exerciseUnderlyingPanelFixture(page);
  const stressDetail = await exerciseStressDetail(page);
  const rulesCard = await exerciseRulesCard(page);
  const marketContext = await exerciseMarketContext(page);
  const portfolioDetail = await exercisePortfolioDetail(page);
  const protectionRiskRendering = await exerciseProtectionRiskRendering(page);
  const alertHistory = await exerciseAlertHistory(page);
  const lampTestDetail = await exerciseLampTestDetail(page);
  const briefNarrative = await assertBriefNarrative(page);
  const governanceFixtures = await exerciseGovernanceFixtures(page);
  // Prove the attention-read guard was armed and effective: the alerts tab
  // was just exercised in a visible headless page, so the SPA must have
  // attempted the acknowledge POST, and every attempt must have been
  // intercepted rather than reaching the real host.
  const attentionGuardDeadline = Date.now() + 10000;
  attentionReadIntercepted = await attentionReadInterceptedCount(page);
  while (attentionReadIntercepted === 0 && Date.now() < attentionGuardDeadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    attentionReadIntercepted = await attentionReadInterceptedCount(page);
  }
  const attentionReadFetches = await page.evaluate(() => globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/attention/read")).length);
  // The ack deliberately skips its POST when nothing is unread, so on a
  // clean desk the guard proves itself differently: the ack path must have
  // read the attention DTO from the alerts view, and zero POSTs may have
  // reached the wire. Any POST that did fire must have been diverted.
  if (attentionReadIntercepted === 0) {
    const attentionState = await page.evaluate(() => ({
      aria: document.getElementById("tabAlerts")?.getAttribute("aria-label") || "",
      attentionGets: globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/attention")).length,
    }));
    if (!/no unread alerts/i.test(attentionState.aria)) {
      throw new Error(`attention read guard never fired with unread pending: ${JSON.stringify(attentionState)}`);
    }
    if (attentionState.attentionGets === 0) {
      throw new Error("attention ack path never ran: no /api/alerts/attention read from the alerts view");
    }
    console.log(`attention guard: clean desk (no unread), ack ran ${attentionState.attentionGets}x with zero cursor posts`);
  }
  if (attentionReadFetches !== attentionReadIntercepted) throw new Error(`attention read guard bypass suspected: page fetches=${attentionReadFetches} intercepted=${attentionReadIntercepted}`);
  const openOrders = await exerciseOpenOrders(page);
  const settingsTab = await exerciseSettingsTab(page);
  const debugTools = await assertDebugToolsRemoved(page, baseURL);
  if (noNotification && pushState !== "unsupported") {
    throw new Error(`expected unsupported with Notification removed, got ${JSON.stringify(pushState)}`);
  }
  if (pageErrors.length > 0 || consoleMessages.length > 0) {
    throw new Error(`browser errors:\n${[...pageErrors, ...consoleMessages].join("\n")}`);
  }
  let lifecycleResult = null;
  if (lifecycle) {
    lifecycleResult = await runLifecycleSmoke(page);
  }
  const smokeState = await page.evaluate(() => globalThis.__canarySmoke);
  const fallbackDeviceSecretStored = await page.evaluate(() => !!localStorage.getItem("ibkrDeviceSecret"));
  console.log(JSON.stringify({
    ok: true,
    browser: browserName,
    channel: launched.channel || null,
    base_url: baseURL,
    mobile,
    notification_removed: noNotification,
    webcrypto_removed: noWebCrypto,
    used_http_fallback: fallbackDeviceSecretStored,
    title,
    visible_identity: visibleIdentity,
    connection,
    push_state: pushState,
    privacy,
    account_panel: accountPanel,
    snapshot_banner: snapshotBanner,
    market_layout: marketLayout,
    viewport_overflow: viewportOverflow,
    stress_controls: stressControls,
    underlying_book_fixture: underlyingBookFixture,
    stress_detail: stressDetail,
    rules_card: rulesCard,
    market_context: marketContext,
    portfolio_detail: portfolioDetail,
    protection_risk_rendering: protectionRiskRendering,
    alert_history: alertHistory,
    lamp_test_detail: lampTestDetail,
    brief_narrative: briefNarrative,
    governance_fixtures: governanceFixtures,
    open_orders: openOrders,
    settings_tab: settingsTab,
    debug_tools: debugTools,
    events: {
      opened_event_streams: eventsBefore.opened_event_streams,
      event_counts: smokeState.eventCounts,
    },
    attention_read_intercepted: attentionReadIntercepted,
    lifecycle: lifecycleResult,
    pair_expires_at: pairing.expires_at,
  }, null, 2));
} finally {
  await browser.close();
  if (stopRestartedApp && cleanupPID) {
    try {
      process.kill(cleanupPID, "SIGTERM");
    } catch {
      // Best effort cleanup for isolated lifecycle smoke.
    }
  }
}

async function runLifecycleSmoke(page) {
  if (!restartCommand) {
    throw new Error("--restart-command is required with --lifecycle=true");
  }
  const before = await page.evaluate(() => ({
    snapshot: globalThis.__canarySmoke.eventCounts.snapshot || 0,
    authSessions: globalThis.__canarySmoke.fetches.filter((f) => f.url.endsWith("/api/auth/session") && f.status === 200).length,
  }));
  const connectionBeforeRestart = await waitForHeader(page);
  const restart = await runShellJSON(restartCommand);
  const snapshotAfter = await waitForSnapshotEvent(page, before.snapshot);
  const connectionAfterRestart = await waitForHeader(page);
  const eventsAfter = await fetchEventsDiagnostics(page);
  const after = await page.evaluate(() => ({
    authSessions: globalThis.__canarySmoke.fetches.filter((f) => f.url.endsWith("/api/auth/session") && f.status === 200).length,
  }));
  if (eventsAfter.opened_event_streams < 1) {
    throw new Error(`expected at least one SSE stream after restart, got ${eventsAfter.opened_event_streams}`);
  }
  return {
    connection_before_restart: connectionBeforeRestart,
    connection_after_restart: connectionAfterRestart,
    reauth_after_restart: after.authSessions > before.authSessions,
    snapshot_events_after_restart: snapshotAfter,
    restart,
    events: {
      opened_event_streams: eventsAfter.opened_event_streams,
      event_counts: eventsAfter.event_counts,
    },
  };
}

async function waitForSnapshotEvent(page, previousCount) {
  await page.waitForFunction((count) => {
    return (globalThis.__canarySmoke?.eventCounts?.snapshot || 0) > count;
  }, previousCount, { timeout: 20000 });
  return page.evaluate(() => globalThis.__canarySmoke.eventCounts.snapshot || 0);
}

async function waitForHeader(page) {
  await page.waitForFunction(() => {
    const text = document.getElementById("connectionLine")?.textContent?.trim() || "";
    const dot = document.getElementById("statusDot");
    return text && text !== "Connecting" && text !== "Market calendar loading" && dot?.classList.contains("ok");
  }, { timeout: 20000 });
  return page.locator("#connectionLine").textContent();
}

async function fetchEventsDiagnostics(page) {
  const events = await page.evaluate(() => ({
    opened_event_streams: globalThis.__canarySmoke?.openedEvents || 0,
    event_counts: { ...(globalThis.__canarySmoke?.eventCounts || {}) },
  }));
  if (events.opened_event_streams < 1) {
    throw new Error(`expected at least one SSE stream, got ${events.opened_event_streams}`);
  }
  return events;
}

async function exerciseAccountPrivacy(page) {
  await page.waitForSelector("#accountPanel:not([hidden])", { timeout: 5000 });
  const value = page.locator("#netLiquidation");
  const before = (await value.textContent())?.trim();
  if (before === "--") {
    return { masked_by_default: false, toggle_reveals: false, no_value: true };
  }
  if (before !== "******") {
    throw new Error(`net liquidation should be masked by default, got ${JSON.stringify(before)}`);
  }
  await page.locator("#accountPrivacyToggle").click();
  await page.waitForFunction(() => {
    const text = document.getElementById("netLiquidation")?.textContent?.trim();
    return text && text !== "******" && text !== "--";
  }, { timeout: 5000 });
  await page.locator("#accountPrivacyToggle").click();
  await page.waitForFunction(() => document.getElementById("netLiquidation")?.textContent?.trim() === "******", { timeout: 5000 });
  return { masked_by_default: true, toggle_reveals: true };
}

async function exerciseAccountPanel(page) {
  await page.waitForFunction(() => {
    const panel = document.getElementById("accountPanel");
    return panel && !panel.hidden && document.getElementById("accountLabel")?.textContent?.trim();
  }, { timeout: 5000 });
  const panel = await page.evaluate(() => ({
    accountMenuPresent: !!document.getElementById("accountMenu"),
    accountMenuTogglePresent: !!document.getElementById("accountMenuToggle"),
    accountLabel: document.getElementById("accountLabel")?.textContent?.trim() || "",
    pill: document.getElementById("tradingEnvPill")?.textContent?.trim() || "",
    pillHidden: !!document.getElementById("tradingEnvPill")?.hidden,
    freshness: document.getElementById("accountAsOf")?.textContent?.trim() || "",
    freshnessQuiet: !!document.getElementById("accountAsOf")?.hidden,
    dailyPnl: document.getElementById("dailyPnl")?.textContent?.trim() || "",
    dailyPnlPct: document.getElementById("dailyPnlPct")?.textContent?.trim() || "",
    riskValues: [
      document.getElementById("accountRiskDelta")?.textContent?.trim() || "",
      document.getElementById("accountRiskTheta")?.textContent?.trim() || "",
      document.getElementById("accountRiskFx")?.textContent?.trim() || "",
      document.getElementById("accountLargestExposureLabel")?.textContent?.trim() || "",
    ],
    accountHasUnderlyingBook: !!document.querySelector("#accountPanel #underlyingBookList"),
  }));
  if (panel.accountMenuPresent || panel.accountMenuTogglePresent) {
    throw new Error(`account dropdown DOM should be removed: ${JSON.stringify(panel)}`);
  }
  // Freshness runs quiet-when-fresh: a hidden empty badge is the healthy
  // state; visible text is required only when the badge is shown (stale or
  // missing timestamp).
  if (!panel.accountLabel || !panel.dailyPnlPct || panel.riskValues.some((value) => !value)) {
    throw new Error(`account panel is missing values: ${JSON.stringify(panel)}`);
  }
  // Operator decision: the trading-env pill renders nothing in live mode (a
  // hidden empty pill is correct), a loud PAPER in paper mode, and a muted
  // "mode?" when the environment is unresolved. Anything else is a bug.
  if (panel.pillHidden ? panel.pill : !["PAPER", "mode?"].includes(panel.pill)) {
    throw new Error(`unexpected trading-env pill state: ${JSON.stringify(panel)}`);
  }
  if (!panel.freshnessQuiet && !panel.freshness) {
    throw new Error(`account freshness badge is visible but empty: ${JSON.stringify(panel)}`);
  }
  if (panel.dailyPnl !== "--" && (panel.dailyPnlPct === "******" || !panel.dailyPnlPct.includes("%"))) {
    throw new Error(`account Daily P/L percent should stay visible in privacy mode: ${JSON.stringify(panel)}`);
  }
  if (/no concrete/i.test(panel.accountLabel)) {
    throw new Error(`account panel should not show no-concrete copy: ${JSON.stringify(panel)}`);
  }
  if (/^all accounts$/i.test(panel.accountLabel)) {
    throw new Error(`account panel should avoid vague All accounts copy: ${JSON.stringify(panel)}`);
  }
  if (panel.accountHasUnderlyingBook) {
    throw new Error("account panel should not contain the underlyings subledger");
  }
  const accountDetailInitiallyHidden = await page.locator("#accountOverviewDetail").evaluate((detail) => detail.hidden);
  if (!accountDetailInitiallyHidden) {
    throw new Error("account overview detail should start folded");
  }
  await page.locator("#accountOverviewToggle").click();
  await page.waitForFunction(() => {
    const toggle = document.getElementById("accountOverviewToggle");
    const detail = document.getElementById("accountOverviewDetail");
    return toggle?.getAttribute("aria-expanded") === "true" && detail && !detail.hidden;
  }, { timeout: 5000 });
  const exposureDisabled = await page.locator("#accountLargestExposureToggle").evaluate((button) => button.disabled);
  if (!exposureDisabled) {
    await page.locator("#accountLargestExposureToggle").click();
    await page.waitForFunction(() => {
      const toggle = document.getElementById("accountLargestExposureToggle");
      const detail = document.getElementById("accountLargestExposurePanel");
      return toggle?.getAttribute("aria-expanded") === "true" && detail && !detail.hidden;
    }, { timeout: 5000 });
  }
  return {
    visible: true,
    account: panel.accountLabel,
    mode: panel.pill,
    account_has_underlying_book: panel.accountHasUnderlyingBook,
    detail_initially_folded: accountDetailInitiallyHidden,
    exposure_detail_disabled: exposureDisabled,
  };
}

async function assertSnapshotBannerCopy(page) {
  const banner = await page.evaluate(() => {
    const el = document.getElementById("snapshotErrorBanner");
    const text = document.getElementById("snapshotErrorText");
    return {
      visible: !!el && !el.hidden,
      text: text?.textContent?.trim() || "",
      title: text?.getAttribute("title") || "",
    };
  });
  if (rawGatewayCopyPattern.test(banner.text)) {
    throw new Error(`snapshot banner leaks raw gateway error text: ${JSON.stringify(banner)}`);
  }
  return banner;
}

async function assertVisibleRenameContract(page) {
  const visible = await page.evaluate(() => {
    const wordmark = document.querySelector(".brand-copy h1");
    const stressNodes = [
      document.getElementById("stressHero"),
      document.getElementById("stressDetailPanel"),
      document.getElementById("marketQuoteStrip"),
      document.getElementById("protectionExposure"),
    ].filter(Boolean);
    return {
      title: document.title,
      wordmark: wordmark?.textContent?.replace(/\s+/g, " ").trim() || "",
      wordmarkLabel: wordmark?.getAttribute("aria-label") || "",
      stressText: stressNodes.map((node) => {
        const titled = [...node.querySelectorAll("[title]")].map((el) => el.getAttribute("title") || "");
        return [node.textContent || "", node.getAttribute("title") || "", ...titled].join(" ");
      }).join(" "),
      alertSourceText: [
        ...document.querySelectorAll(".alert-row__source"),
        ...document.querySelectorAll("#alertSourceList b"),
        document.getElementById("selectedAlertTime"),
      ].filter(Boolean).map((node) => node.textContent || "").join(" "),
    };
  });
  if (visible.title !== "Canary" || visible.wordmark !== "Canary" || visible.wordmarkLabel !== "Canary") {
    throw new Error(`visible product identity should be simply Canary: ${JSON.stringify(visible)}`);
  }
  const stale = visible.stressText.match(staleStressDomainCopyPattern);
  if (stale) {
    throw new Error(`stress surface retains stale domain copy ${JSON.stringify(stale[0])}: ${JSON.stringify(visible.stressText)}`);
  }
  if (/\bcanary\b/i.test(visible.alertSourceText)) {
    throw new Error(`alert source metadata exposes the retained backend sensor id: ${JSON.stringify(visible.alertSourceText)}`);
  }
  return { title: visible.title, wordmark: visible.wordmark, wordmark_label: visible.wordmarkLabel };
}

async function exerciseMarketLayout(page) {
  // Panel Dark session chip: "RTH · closes 3:59" while open, "opens 17:12"
  // otherwise — a minutes-precision countdown in both directions.
  await page.waitForFunction(() => {
    const text = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    return /\b(closes|opens) \d+d? ?\d*:\d{2}\b/i.test(text);
  }, { timeout: 10000 });
  const layout = await page.evaluate(() => {
    const regimeHalf = document.getElementById("regimeSummaryCard");
    const stressPanel = document.getElementById("stressHero");
    const signalPanel = document.getElementById("signalPanel");
    const underlyingPanel = document.getElementById("underlyingPanel");
    const marketStrip = document.querySelector(".market-strip");
    const accountPanel = document.getElementById("accountPanel");
    const regimeBeforeStress = !!(regimeHalf && stressPanel && (regimeHalf.compareDocumentPosition(stressPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const stressBeforeUnderlying = !!(signalPanel && underlyingPanel && (signalPanel.compareDocumentPosition(underlyingPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const accountAfterMarketStrip = !!(marketStrip && accountPanel && (marketStrip.compareDocumentPosition(accountPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const phase = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    const strip = document.querySelector(".market-strip");
    const stripStyle = strip ? getComputedStyle(strip) : null;
    const quoteStrip = document.getElementById("marketQuoteStrip");
    const quoteStripStyle = quoteStrip ? getComputedStyle(quoteStrip) : null;
    const firstQuote = document.querySelector("#marketQuoteStrip .market-quote-cell");
    const labelStyle = firstQuote?.querySelector("b") ? getComputedStyle(firstQuote.querySelector("b")) : null;
    const marketOpen = strip?.classList.contains("market-open") || false;
    const accountHasUnderlyingBook = !!document.querySelector("#accountPanel #underlyingBookList");
    const stressHasUnderlyingBook = !!document.querySelector("#stressHero #underlyingBookList");
    const standaloneHasUnderlyingBook = !!document.querySelector("#underlyingPanel #underlyingBookList");
    const quoteCells = document.querySelectorAll("#marketQuoteStrip .market-quote-cell").length;
    const underlyingOpen = !!underlyingPanel?.open;
    return {
      regimeBeforeStress,
      stressBeforeUnderlying,
      accountAfterMarketStrip,
      phase,
      marketOpen,
      accountHasUnderlyingBook,
      stressHasUnderlyingBook,
      standaloneHasUnderlyingBook,
      quoteCells,
      underlyingOpen,
      marketStyle: {
        stripShadow: stripStyle?.boxShadow || "",
        quoteBorder: quoteStripStyle?.borderTopWidth || "",
        quoteRadius: quoteStripStyle?.borderRadius || "",
        quoteBackground: quoteStripStyle?.backgroundColor || "",
        labelColor: labelStyle?.color || "",
        labelSize: labelStyle?.fontSize || "",
      },
    };
  });
  if (!layout.regimeBeforeStress) {
    throw new Error("Regime should appear before Portfolio stress in DOM order");
  }
  if (!layout.stressBeforeUnderlying) {
    throw new Error("Underlyings should appear after Portfolio stress in DOM order");
  }
  if (!layout.accountAfterMarketStrip) {
    throw new Error("Account panel should appear below the market countdown in DOM order");
  }
  if (layout.marketOpen && !/\bcloses \d/i.test(layout.phase)) {
    throw new Error(`open market chip should count down to the close: ${JSON.stringify(layout.phase)}`);
  }
  if (!layout.marketOpen && !/\bopens \d/i.test(layout.phase)) {
    throw new Error(`closed market chip should count down to the open: ${JSON.stringify(layout.phase)}`);
  }
  if (/:\d{2}:\d{2}\b/.test(layout.phase)) {
    throw new Error(`session countdown should stop at minutes, not tick seconds: ${JSON.stringify(layout.phase)}`);
  }
  if (layout.accountHasUnderlyingBook) {
    throw new Error("Account panel still contains the underlyings subledger");
  }
  if (layout.stressHasUnderlyingBook || !layout.standaloneHasUnderlyingBook) {
    throw new Error("Underlyings subledger should be standalone, not inside Portfolio stress");
  }
  if (layout.quoteCells !== 3) {
    throw new Error(`market strip should render three bounded quote cells, found ${layout.quoteCells}`);
  }
  if (layout.underlyingOpen) {
    throw new Error("Underlyings should be folded by default");
  }
  if (layout.marketStyle.stripShadow !== "none" || layout.marketStyle.quoteBorder !== "0px" || layout.marketStyle.quoteRadius !== "0px") {
    throw new Error(`market strip should be flat and unframed: ${JSON.stringify(layout.marketStyle)}`);
  }
  if (!/^(9|10|11)(\.\d+)?px$/.test(layout.marketStyle.labelSize)) {
    throw new Error(`market symbol labels should use compact engraved sizing: ${JSON.stringify(layout.marketStyle)}`);
  }
  if (/\b(Xetra|US market|US equities|US options)\b/i.test(layout.phase)) {
    throw new Error(`market line should not repeat the selected market label: ${JSON.stringify(layout.phase)}`);
  }
  return layout;
}

async function assertNoViewportOverflow(page) {
  const sizes = [
    { width: 390, height: 844 },
    { width: 547, height: 919 },
    { width: 900, height: 900 },
    { width: 1280, height: 900 },
  ];
  const results = [];
  for (const size of sizes) {
    await page.setViewportSize(size);
    await page.waitForTimeout(120);
    const info = await page.evaluate(() => {
      const clientWidth = document.documentElement.clientWidth;
      const pageScrollWidth = Math.max(document.documentElement.scrollWidth, document.body.scrollWidth);
      const signalPanel = document.getElementById("signalPanel")?.getBoundingClientRect();
      const dashboard = document.getElementById("dashboard")?.getBoundingClientRect();
      // A tile the SPA hides (no rules payload yet) measures 0x0; drop it so
      // the geometry assertions describe what is actually on the panel.
      const measured = (selector) => [...document.querySelectorAll(selector)]
        .map((tile) => tile.getBoundingClientRect())
        .filter((box) => box.width > 0);
      const regimeTiles = measured("#regimeSummaryCard > .pd-tile");
      const deskTiles = measured("#deskGrid > .pd-tile");
      const columns = (tiles) => new Set(tiles.map((tile) => Math.round(tile.left))).size;
      const master = document.getElementById("masterAnnunciator")?.getBoundingClientRect();
      const regimeGrid = document.getElementById("regimeSummaryCard")?.getBoundingClientRect();
      const stress = document.getElementById("stressHero")?.getBoundingClientRect();
      // Panel Dark: the master annunciator spans the panel above two fixed
      // 2x2 instrument grids (regime clusters, then the desk windows), and
      // signalPanel itself spans the full dashboard width.
      const signalLayout = regimeTiles.length > 0 ? {
        regimeTiles: regimeTiles.length,
        regimeColumns: columns(regimeTiles),
        deskTiles: deskTiles.length,
        deskColumns: columns(deskTiles),
        masterFullWidth: !!(master && regimeGrid) && Math.abs(master.width - regimeGrid.width) <= 4,
        masterAboveGrid: !!master && master.top < regimeTiles[0].top,
        regimeBeforeDesk: !!stress && regimeTiles[0].top < stress.top,
        signalPanelFullWidth: !!(signalPanel && dashboard) && Math.abs(signalPanel.width - dashboard.width) <= 4,
      } : null;
      const offenders = [...document.querySelectorAll("body *")]
        .filter((el) => {
          const style = getComputedStyle(el);
          if (style.display === "none" || style.visibility === "hidden") return false;
          if (style.overflowX === "auto" || style.overflowX === "scroll") return false;
          return el.scrollWidth > el.clientWidth + 1;
        })
        .slice(0, 8)
        .map((el) => ({
          tag: el.tagName.toLowerCase(),
          id: el.id || "",
          className: typeof el.className === "string" ? el.className : "",
          scrollWidth: el.scrollWidth,
          clientWidth: el.clientWidth,
        }));
      return { clientWidth, pageScrollWidth, offenders, signalLayout };
    });
    results.push({ ...size, ...info });
    if (info.pageScrollWidth > info.clientWidth + 1) {
      throw new Error(`page overflows at ${size.width}px: ${JSON.stringify(info)}`);
    }
    const layout = info.signalLayout;
    if (!layout || layout.regimeTiles !== 4 || layout.regimeColumns !== 2 || layout.deskTiles < 3 || layout.deskColumns !== 2) {
      throw new Error(`Regime should render a fixed 2x2 cluster grid and Desk a two-column window grid at ${size.width}px: ${JSON.stringify(layout)}`);
    }
    if (!layout.masterFullWidth || !layout.masterAboveGrid || !layout.regimeBeforeDesk || !layout.signalPanelFullWidth) {
      throw new Error(`Master annunciator should span a full-width combined panel above the regime grid, with the desk grid beneath, at ${size.width}px: ${JSON.stringify(layout)}`);
    }
  }
  return results;
}

async function exerciseStressControlsRemoved(page) {
  const counts = await page.evaluate(() => ({
    chipRows: document.querySelectorAll(".stress-chip-row").length,
    chips: document.querySelectorAll(".stress-chip").length,
    warningToggle: document.querySelectorAll("#stressWarningsToggle").length,
    checksToggle: document.querySelectorAll("#stressChecksToggle").length,
    inlineDetail: document.querySelectorAll("#stressInlineDetailPanel").length,
    mitigationButton: document.querySelectorAll("#stressMitigationButton").length,
    orderReviewPanel: document.querySelectorAll("#orderReviewPanel").length,
    riskPlanQuickAction: document.querySelectorAll("#quickRiskPlanButton").length,
    reviewBlockersButton: document.querySelectorAll("#quickReviewBlockersButton").length,
    heldActionsButton: document.querySelectorAll("#quickHeldActionsButton").length,
    alertsQuickButton: document.querySelectorAll("#quickAlertsButton").length,
  }));
  const total = Object.values(counts).reduce((sum, count) => sum + count, 0);
  if (total > 0) {
    throw new Error(`stress summary controls and risk-plan surfaces should be removed: ${JSON.stringify(counts)}`);
  }
  return counts;
}

async function exerciseUnderlyingPanelFixture(page) {
  await page.evaluate(() => {
    localStorage.setItem("ibkrPurgeBook", JSON.stringify({
      purge_id: "purge_ui_fixture",
      base_currency: "USD",
      legs: [{
        symbol: "SMOKE",
        sec_type: "STK",
        currency: "USD",
        current_price: 444.12,
        current_price_source: "fixture quote",
        quote_change_pct: -0.7,
        shadow_saved: 125.5,
        status: "priced",
      }],
    }));
  });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await page.waitForFunction(() => {
    return document.querySelector("#underlyingPanel #underlyingBookList .underlying-row");
  }, { timeout: 5000 });
  const info = await page.evaluate(() => ({
    count: document.getElementById("underlyingBookCount")?.textContent?.trim() || "",
    status: document.getElementById("underlyingBookStatus")?.textContent?.trim() || "",
    winner: document.getElementById("underlyingWinnerPnl")?.textContent?.trim() || "",
    loser: document.getElementById("underlyingLoserPnl")?.textContent?.trim() || "",
    accountHasUnderlyingBook: !!document.querySelector("#accountPanel #underlyingBookList"),
    stressHasUnderlyingBook: !!document.querySelector("#stressHero #underlyingBookList"),
    standaloneHasUnderlyingBook: !!document.querySelector("#underlyingPanel #underlyingBookList"),
    foldIcon: Boolean(document.querySelector("#underlyingPanel #underlyingDetailToggle.panel-chevron")),
    bulkButtons: [...document.querySelectorAll("#underlyingPanel .underlying-bulk-actions button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    })),
      rows: [...document.querySelectorAll("#underlyingPanel #underlyingBookList .underlying-row")].map((row) => ({
      symbol: row.dataset.symbol || "",
      virtual: row.classList.contains("underlying-row--virtual"),
      markers: [...row.querySelectorAll(".underlying-marker")].map((marker) => marker.textContent?.trim() || ""),
      quoteStatus: row.querySelector(".underlying-quote-status")?.textContent?.trim() || "",
      quoteStatusTitle: row.querySelector(".underlying-quote-status")?.getAttribute("title") || "",
      buttons: [...row.querySelectorAll("button")].map((button) => ({
        text: button.textContent?.trim() || "",
        disabled: button.disabled,
        title: button.title || "",
      })),
      text: row.textContent?.replace(/\s+/g, " ").trim() || "",
    })),
  }));
  if (info.accountHasUnderlyingBook || info.stressHasUnderlyingBook || !info.standaloneHasUnderlyingBook) {
    throw new Error(`underlyings subledger is in the wrong panel: ${JSON.stringify(info)}`);
  }
  if (!info.foldIcon) {
    throw new Error(`underlyings folded summary is missing its disclosure toggle: ${JSON.stringify(info)}`);
  }
  if (!info.winner || !info.loser) {
    throw new Error(`underlyings folded summary is missing winner/loser totals: ${JSON.stringify(info)}`);
  }
  const row = info.rows.find((item) => item.symbol === "SMOKE");
  if (!row || !row.virtual) {
    throw new Error(`virtual purge row is missing: ${JSON.stringify(info)}`);
  }
  if (!row.quoteStatus || !row.quoteStatusTitle) {
    throw new Error(`underlying row should include quote price status: ${JSON.stringify(row)}`);
  }
  if (rawGatewayCopyPattern.test(row.quoteStatus)) {
    throw new Error(`underlying row leaks raw gateway error text: ${JSON.stringify(row)}`);
  }
  for (const marker of ["Virtual", "Purged"]) {
    if (!row.markers.includes(marker)) {
      throw new Error(`virtual purge row lacks ${marker} marker: ${JSON.stringify(row)}`);
    }
  }
  const purge = row.buttons.find((button) => button.text === "Purge");
  const restore = row.buttons.find((button) => button.text === "Restore");
  const build = row.buttons.find((button) => button.text === "Build");
  if (!purge?.disabled || !purge.title) {
    throw new Error(`purged row should disable Purge with a reason: ${JSON.stringify(row.buttons)}`);
  }
  if (!restore || !build) {
    throw new Error(`purged row should render Restore and Build actions: ${JSON.stringify(row.buttons)}`);
  }
  if (row.buttons.some((button) => /placeholder|backend wiring/i.test(button.title))) {
    throw new Error(`underlying row still contains placeholder action copy: ${JSON.stringify(row.buttons)}`);
  }
  const bulkLabels = info.bulkButtons.map((item) => item.text);
  const expectedBulkLabels = ["Purge all", "Restore all", "Rebuild all"];
  if (JSON.stringify(bulkLabels) !== JSON.stringify(expectedBulkLabels)) {
    throw new Error(`bulk underlying controls should be ordered Purge all, Restore all, Rebuild all: ${JSON.stringify(info.bulkButtons)}`);
  }
  for (const label of expectedBulkLabels) {
    const button = info.bulkButtons.find((item) => item.text === label);
    if (button.disabled && !button.title) {
      throw new Error(`disabled bulk underlying control lacks a reason: ${JSON.stringify(button)}`);
    }
  }

  await page.evaluate(() => localStorage.removeItem("ibkrPurgeBook"));
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  // The reload resets __canarySmoke and the rendered stress/regime state; later
  // exercises assume the same arrived-snapshot barrier as the initial load.
  await waitForSnapshotEvent(page, 0);

  return {
    virtual_rows: info.rows.length,
    count: info.count,
    markers: row.markers,
    purge_disabled: purge.disabled,
    restore_disabled: restore.disabled,
    build_disabled: build.disabled,
    bulk_buttons: info.bulkButtons,
  };
}

async function exerciseStressDetail(page) {
  // Quiet-when-fresh blanks and hides the badge while the snapshot is fresh;
  // that is the healthy state, not a missing timestamp. Badge and hero are
  // sampled in one evaluate so the no-data decision cannot straddle the
  // re-render when a fresh instance's first stress poll lands mid-exercise.
  const readStressHead = () => page.evaluate(() => {
    const badge = document.getElementById("stressAsOf");
    const text = badge?.textContent?.trim() || "";
    return {
      quietFresh: !!badge && badge.hidden && !text,
      timestamp: text,
      hero: document.getElementById("stressHero")?.textContent || "",
    };
  });
  let head = await readStressHead();
  if (!head.quietFresh && stressTimestampMissing(head.timestamp)) {
    try {
      // Wait for either rendered outcome of the first stress poll: a visible
      // real timestamp, or the quiet-when-fresh blank+hidden badge that a
      // just-landed fresh snapshot renders.
      await page.waitForFunction(() => {
        const badge = document.getElementById("stressAsOf");
        if (!badge) return false;
        const text = badge.textContent?.trim() || "";
        if (badge.hidden && !text) return true;
        return text && text !== "no timestamp" && text !== "updated --" && text !== "--";
      }, { timeout: 30000 });
    } catch {
      // A first stress poll can legitimately outlast this wait (fresh app
      // instance against an off-hours live session); the pending-copy
      // assertion below still pins the rendered no-data contract.
    }
    head = await readStressHead();
  }
  const timestamp = head.timestamp;
  const timestampMissing = !head.quietFresh && stressTimestampMissing(timestamp);
  const initiallyOpen = await page.locator("#stressDetailPanel").evaluate((el) => !el.hidden);
  if (initiallyOpen) {
    throw new Error("Portfolio stress detail should be collapsed by default");
  }
  if (timestampMissing) {
    // Panel Dark's pending stress window states "cushion pending" (WP1); the
    // pre-instrument copy said "waiting for stress snapshot". Both are honest
    // no-data renders on a freshly restarted daemon.
    if (!/waiting for stress snapshot|cushion pending/i.test(head.hero)) {
      throw new Error(`stress timestamp is missing without pending copy: ${JSON.stringify({ timestamp, pending: head.hero })}`);
    }
    return { opens: false, initially_open: initiallyOpen, timestamp, no_value: true };
  }
  await page.locator("#stressDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("stressDetailPanel");
    return panel && !panel.hidden && document.getElementById("stressDetailGrid")?.children.length >= 2;
  }, { timeout: 5000 });
  await assertVisibleRenameContract(page);
  const counts = await page.evaluate(() => ({
    cards: document.getElementById("stressDetailGrid")?.children.length || 0,
    drivers: document.getElementById("stressDrivers")?.children.length || 0,
    held_stress: document.getElementById("heldStressList")?.children.length || 0,
    held_stress_payload: globalThis.__canarySmoke?.latestStressHeldStress || 0,
  }));
  if (counts.held_stress_payload > 0 && counts.held_stress === 0) {
    throw new Error("stress held_stress payload is present but detail panel did not render it");
  }
  await page.locator("#stressDetailToggle").click();
  await page.waitForFunction(() => {
    const stress = document.getElementById("stressDetailPanel");
    return stress?.hidden;
  }, { timeout: 5000 });
  return { opens: true, initially_open: initiallyOpen, timestamp, cards: counts.cards, drivers: counts.drivers, held_stress: counts.held_stress };
}

function stressTimestampMissing(value) {
  return !value || value === "--" || value === "updated --" || value === "no timestamp";
}

async function exerciseRulesCard(page) {
  // The rules card renders only once snapshot.rules arrives (stress
  // cadence); a fresh instance may legitimately not have it yet. Absent
  // card + no rules payload = pass with exercised:false, but a payload
  // without a card is a rendering bug.
  const hasPayload = await page.evaluate(() => (globalThis.__canarySmoke?.latestRulesCount || 0) > 0);
  const visible = await page.locator("#stressRulesCard").evaluate((el) => !el.hidden).catch(() => false);
  if (!visible) {
    if (hasPayload) {
      throw new Error("snapshot.rules payload present but #stressRulesCard is hidden");
    }
    return { exercised: false, reason: "no rules payload yet" };
  }
  const counts = (await page.locator("#stressRulesCounts").textContent())?.trim() || "";
  if (!counts || counts === "--") {
    throw new Error("rules card visible without a counts summary");
  }
  const initiallyOpen = await page.locator("#stressRulesDetailPanel").evaluate((el) => !el.hidden);
  if (initiallyOpen) {
    throw new Error("rules detail should be collapsed by default");
  }
  await page.locator("#stressRulesToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("stressRulesDetailPanel");
    return panel && !panel.hidden && (document.getElementById("stressRulesGrid")?.children.length || 0) >= 12;
  }, { timeout: 5000 });
  const grid = await page.evaluate(() => {
    const cards = [...(document.getElementById("stressRulesGrid")?.children || [])];
    return {
      cards: cards.length,
      unknown_as_pass: cards.some((c) => /unknown/i.test(c.textContent || "") && c.classList.contains("ok")),
    };
  });
  if (grid.unknown_as_pass) {
    throw new Error("a rules row renders unknown status with a pass tone — unknown must never read as pass");
  }
  await page.locator("#stressRulesToggle").click();
  await page.waitForFunction(() => document.getElementById("stressRulesDetailPanel")?.hidden, { timeout: 5000 });
  return { exercised: true, counts, cards: grid.cards };
}

async function exerciseMarketContext(page) {
  let before = await readMarketContext(page);
  if ((!before.regime || before.regime === "--") && !lifecycle) {
    try {
      await page.waitForFunction(() => {
        const text = document.getElementById("marketRegime")?.textContent?.trim() || "";
        return text && text !== "--";
      }, { timeout: 10000 });
      before = await readMarketContext(page);
    } catch {
      // Keep the no-value assertion below for app instances without live data.
    }
  }
  const expectedSymbols = ["SPY", "VIX", "QQQ"];
  if (before.quotes.length !== expectedSymbols.length) {
    throw new Error(`market strip should render ${expectedSymbols.length} quote cells: ${JSON.stringify(before.quotes)}`);
  }
  for (const symbol of expectedSymbols) {
    const quote = before.quotes.find((item) => item.symbol === symbol);
    if (!quote) {
      throw new Error(`market strip missing ${symbol}: ${JSON.stringify(before.quotes)}`);
    }
    if (quote.price !== "--" && !/^-?[\d.,]+[.,]\d{2}$/.test(quote.price)) {
      throw new Error(`${symbol} price should use two decimal places, got ${JSON.stringify(quote.price)}`);
    }
    if (quote.change && quote.change !== "--" && !/^[+-]?[\d.,]+[.,]\d{2}%$/.test(quote.change)) {
      throw new Error(`${symbol} change should use two decimal places, got ${JSON.stringify(quote.change)}`);
    }
    if (!quote.source) {
      throw new Error(`${symbol} quote cell should include source/as-of or error text`);
    }
    if (rawGatewayCopyPattern.test(quote.source)) {
      throw new Error(`${symbol} quote cell leaks raw gateway error text: ${JSON.stringify(quote.source)}`);
    }
  }
  if (before.marketContextPanelPresent) {
    throw new Error("old Market Context panel should be removed");
  }
  const instruments = await assertPanelDarkInstruments(page);
  if (!before.regime || before.regime === "--") {
    if (before.weather !== "weather-na") {
      throw new Error(`empty market regime should use weather-na, got ${JSON.stringify(before.weather)}`);
    }
    return {
      no_value: true,
      weather: before.weather,
      quote_cells: before.quotes.length,
      indicators: 0,
      instruments,
    };
  }
  if (!["weather-green", "weather-amber", "weather-red"].includes(before.weather)) {
    throw new Error(`market weather is not color coded, got ${JSON.stringify(before.weather)}`);
  }
  const stressInitiallyOpen = await page.locator("#stressDetailPanel").evaluate((el) => !el.hidden);
  const regimeInitiallyOpen = await page.locator("#regimeDetailPanel").evaluate((el) => !el.hidden);
  if (stressInitiallyOpen || regimeInitiallyOpen) {
    throw new Error(`Regime and stress details should both be collapsed by default: ${JSON.stringify({ stressInitiallyOpen, regimeInitiallyOpen })}`);
  }
  // Regime and stress detail now expand independently (no mutual exclusion):
  // opening regime must not touch stress, and opening stress afterward must
  // leave regime open too — both can be visible together in the shared deck.
  await page.locator("#regimeDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("regimeDetailPanel");
    const stress = document.getElementById("stressDetailPanel");
    return panel && !panel.hidden && stress?.hidden;
  }, { timeout: 5000 });
  const indicators = await page.evaluate(() => document.getElementById("regimeIndicators")?.children.length || 0);
  if (indicators === 0) {
    throw new Error("market regime detail is empty");
  }
  await page.locator("#stressDetailToggle").click();
  await page.waitForFunction(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const stress = document.getElementById("stressDetailPanel");
    return regime && !regime.hidden && stress && !stress.hidden;
  }, { timeout: 5000 });
  const bothOpen = await page.evaluate(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const stress = document.getElementById("stressDetailPanel");
    return !regime?.hidden && !stress?.hidden;
  });
  if (!bothOpen) {
    throw new Error("Regime and stress detail should be independently expandable — opening stress should not close regime");
  }
  await page.locator("#regimeDetailToggle").click();
  await page.locator("#stressDetailToggle").click();
  await page.waitForFunction(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const stress = document.getElementById("stressDetailPanel");
    return regime?.hidden && stress?.hidden;
  }, { timeout: 5000 });
  return {
    regime: before.regime,
    weather: before.weather,
    quote_cells: before.quotes.length,
    stress_initially_open: stressInitiallyOpen,
    regime_initially_open: regimeInitiallyOpen,
    both_independently_open: bothOpen,
    indicators,
    instruments,
  };
}


// Panel Dark instrument contract: fixed cluster windows that never reorder,
// a readout-class delta tile with no lamp slot, a lamp-test stamp that
// reports served source health, and the master law — a lit red window can
// never sit under a master that neither lamps nor discloses it.
async function assertPanelDarkInstruments(page) {
  // Declared inside the function: the smoke invokes itself through a
  // top-level await mid-file, so module-level consts below that line are
  // still in the temporal dead zone when assertions run.
  const REGIME_WINDOW_LEGENDS = ["Breadth", "Volatility", "Credit", "Dealer gamma"];
  const instruments = await page.evaluate(() => {
    const litClass = (el) => [...(el?.classList || [])].find((name) => name.startsWith("pd-tile--")) || "";
    const readTile = (el) => (el ? {
      lit: litClass(el),
      legend: el.querySelector(".pd-tile__legend")?.textContent?.trim() || "",
      cap: el.querySelector(".pd-tile__cap")?.textContent?.trim() || "",
      fig: el.querySelector(".pd-tile__fig")?.textContent?.trim() || "",
    } : null);
    return {
      master: {
        legend: document.getElementById("marketRegime")?.textContent?.trim() || "",
        sub: document.getElementById("marketRegimeSummary")?.textContent?.trim() || "",
        lit: litClass(document.getElementById("masterAnnunciator")),
      },
      lampTest: document.getElementById("lampTestStamp")?.textContent?.trim() || "",
      clusters: [...document.querySelectorAll("#regimeSummaryCard > .pd-tile")].map(readTile),
      stress: readTile(document.getElementById("stressHero")),
      protection: readTile(document.getElementById("protectionTile")),
      deltaReadout: document.getElementById("deltaTile")?.classList.contains("pd-tile--readout") || false,
      deltaHasLampBar: !!document.querySelector("#deltaTile .pd-tile__bar"),
      movers: [...document.querySelectorAll("#moversRow b")].map((cell) => cell.textContent?.trim() || ""),
    };
  });
  const legends = instruments.clusters.map((cluster) => cluster.legend);
  if (legends.join("|") !== REGIME_WINDOW_LEGENDS.join("|")) {
    throw new Error(`regime windows must keep fixed positions ${JSON.stringify(REGIME_WINDOW_LEGENDS)}, got ${JSON.stringify(legends)}`);
  }
  for (const cluster of instruments.clusters) {
    if (!cluster.cap || !cluster.fig) {
      throw new Error(`regime window is missing its served caption or figure: ${JSON.stringify(cluster)}`);
    }
  }
  if (!instruments.deltaReadout || instruments.deltaHasLampBar) {
    throw new Error(`Net $ Delta must be a flush readout tile with no lamp slot: ${JSON.stringify(instruments)}`);
  }
  if (!/\d+\/\d+ sources ok/i.test(instruments.lampTest)) {
    throw new Error(`lamp-test stamp should report served source health: ${JSON.stringify(instruments.lampTest)}`);
  }
  if (!instruments.master.sub) {
    throw new Error("master annunciator should carry an action subline");
  }
  const redWindows = instruments.clusters.filter((cluster) => cluster.lit === "pd-tile--act").length;
  if (redWindows > 0 && !instruments.master.lit && !/\b\d+ red:/i.test(instruments.master.sub)) {
    throw new Error(`master and panel disagree: ${redWindows} red window(s) under an unlit, undisclosed master: ${JSON.stringify(instruments.master)}`);
  }
  return instruments;
}

async function exercisePortfolioDetail(page) {
  const summary = (await page.locator("#portfolioDetailSummary").textContent())?.trim() || "";
  const hero = await page.evaluate(() => ({
    panelOpen: document.getElementById("portfolioPanel")?.dataset.open || "",
    delta: document.getElementById("portfolioDollarDelta")?.textContent?.trim() || "",
    meaning: document.getElementById("portfolioDeltaMeaning")?.textContent?.trim() || "",
  }));
  if (hero.panelOpen !== "false" || !hero.delta || !hero.meaning) {
    throw new Error(`portfolio folded hero is incomplete: ${JSON.stringify(hero)}`);
  }
  if (/[0-9$€£]|USD|EUR|GBP/.test(hero.delta)) {
    throw new Error(`portfolio folded delta should not expose the private numeric value: ${JSON.stringify(hero)}`);
  }
  await page.locator("#portfolioPanel .portfolio-layout").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("portfolioDetailPanel");
    return document.getElementById("portfolioPanel")?.dataset.open === "true" &&
      panel && !panel.hidden && document.getElementById("portfolioDetailList")?.children.length >= 4;
  }, { timeout: 5000 });
  const detail = await page.evaluate(() => ({
    rows: document.getElementById("portfolioDetailList")?.children.length || 0,
    text: document.getElementById("portfolioDetailList")?.textContent || "",
  }));
  if (!detail.text.includes("option legs") && !detail.text.includes("No option legs")) {
    throw new Error("portfolio detail does not explain Greeks coverage");
  }
  await page.locator("#portfolioDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("portfolioDetailPanel");
    return document.getElementById("portfolioPanel")?.dataset.open === "false" && panel && panel.hidden;
  }, { timeout: 5000 });
  return { opens: true, summary, rows: detail.rows, delta: hero.delta };
}

async function exerciseProtectionRiskRendering(page) {
  await page.evaluate(() => {
    const positionsCoverage = {
      status: "review",
      counts: { unprotected: 1, orphaned_order: 1 },
      unprotected_notional_base: 123,
      unprotected_notional_base_currency: "USD",
      by_underlying: [{
        underlying: "SMOKE",
        state: "unprotected",
        position_quantity: 10,
        unprotected_quantity: 10,
        unprotected_notional_base: 123,
        unprotected_notional_base_currency: "USD",
      }, {
        underlying: "PART",
        state: "partial",
        position_quantity: 40,
        protected_quantity: 25,
        unprotected_quantity: 15,
        unprotected_notional_base: 456,
        unprotected_notional_base_currency: "USD",
        orders: [{
          symbol: "PART",
          order_type: "TRAIL",
          tif: "GTC",
          stop_price: 31.5,
        }],
      }, {
        underlying: "COVER",
        state: "covered",
        position_quantity: 8,
        protected_quantity: 8,
        orders: [{
          symbol: "COVER",
          order_type: "TRAIL",
          tif: "GTC",
          stop_price: 50,
        }],
      }],
      largest_unprotected: [{
        underlying: "SMOKE",
        state: "unprotected",
        unprotected_notional_base: 123,
        unprotected_notional_base_currency: "USD",
      }],
      orphaned_orders: [{
        symbol: "OLD",
        order_type: "TRAIL",
        remaining: 100,
        reconciliation_state: "position_mismatch",
        last_message: "current position 0 no longer supports close-only protective order remaining 100; broker reconciliation required",
      }],
    };
    const stressCoverage = {
      status: "review",
      counts: { unprotected: 1 },
      unprotected_notional_base: 999,
      unprotected_notional_base_currency: "USD",
      largest_unprotected: [{
        underlying: "CANARY",
        state: "unprotected",
        unprotected_notional_base: 999,
        unprotected_notional_base_currency: "USD",
      }],
    };
    const apply = globalThis.__canarySmoke?.applySnapshotPatch;
    if (!apply) {
      throw new Error("smoke snapshot patch hook is unavailable");
    }
    apply({
      account: { base_currency: "USD" },
      positions: {
        portfolio: { base_currency: "USD" },
        protection_coverage: positionsCoverage,
      },
      stress: {
        portfolio_fit: "low",
        portfolio: { protection_coverage: stressCoverage },
        protection_coverage: stressCoverage,
      },
      proposals: {
        as_of: new Date().toISOString(),
        counts: { total: 1, actionable: 1, trailing_stop: 1 },
        proposals: [{
          key: "smoke-trail",
          revision: "smoke",
          bucket: "trailing_stop",
          state: "generated",
          symbol: "SMOKE",
          sec_type: "STK",
          action: "SELL",
          quantity: 10,
          max_quantity: 10,
          position_quantity: 10,
          position_effect: "close",
          order_type: "TRAIL",
          tif: "GTC",
          contract: { symbol: "SMOKE", sec_type: "STK", currency: "USD" },
          trail: { trailing_percent: 10, initial_stop_price: 90 },
          execution_semantics: {
            reference_side: "bid",
            trigger_method_label: "last",
            trigger_effect: "market_order_when_triggered",
            price_guarantee: "stop_price_is_not_execution_price",
          },
          stop_risk: {
            estimated_loss_base: 100,
            base_currency: "USD",
            estimated_loss_pct_nlv: 0.5,
            gap_scenario: {
              gap_pct: 7.5,
              estimated_loss_base: 145,
              estimated_loss_pct_nlv: 0.7,
            },
          },
          stop_ladder: [{
            label: "5%",
            kind: "fixed_5pct",
            percent: 5,
            stop_price: 95,
            estimated_loss_base: 50,
          }, {
            label: "10%",
            kind: "fixed_10pct",
            percent: 10,
            stop_price: 90,
            estimated_loss_base: 100,
          }, {
            label: "policy chosen",
            kind: "policy_chosen",
            percent: 10,
            stop_price: 90,
            estimated_loss_base: 100,
          }, {
            label: "ATR candidate",
            kind: "atr_candidate",
            percent: 12,
            stop_price: 88,
            estimated_loss_base: 120,
          }],
        }],
      },
    }, { protectionOpen: true, portfolioDetailOpen: true, stressDetailOpen: true });
  });
  await page.waitForFunction(() => {
    const portfolio = document.getElementById("portfolioDetailList")?.textContent?.toLowerCase() || "";
    const stress = document.getElementById("stressDetailGrid")?.textContent?.toLowerCase() || "";
    return document.querySelector(".protection-row__risk-ticket") &&
      document.querySelector(".protection-row__ladder") &&
      document.querySelector(".protection-coverage-ledger") &&
      portfolio.includes("protection coverage") &&
      stress.includes("protection coverage");
  }, { timeout: 5000 });
  const info = await page.evaluate(() => ({
    noStop: document.getElementById("protectionNoStopExposure")?.textContent?.trim() || "",
    riskTicket: document.querySelector(".protection-row__risk-ticket")?.textContent?.replace(/\s+/g, " ").trim() || "",
    ladder: document.querySelector(".protection-row__ladder")?.textContent?.replace(/\s+/g, " ").trim() || "",
    coverageLedger: document.querySelector(".protection-coverage-ledger")?.textContent?.replace(/\s+/g, " ").trim() || "",
    portfolioDetail: document.getElementById("portfolioDetailList")?.textContent?.replace(/\s+/g, " ").trim() || "",
    stressDetail: document.getElementById("stressDetailGrid")?.textContent?.replace(/\s+/g, " ").trim() || "",
  }));
  await assertVisibleRenameContract(page);
  if (!info.noStop.includes("123") || info.noStop.includes("999")) {
    throw new Error(`Protection no-stop exposure should use positions.protection_coverage, not stress context: ${JSON.stringify(info)}`);
  }
  for (const text of ["trigger bid / last", "est. loss", "7.5% gap", "trigger becomes market"]) {
    if (!info.riskTicket.includes(text)) {
      throw new Error(`Protection risk ticket missing ${JSON.stringify(text)}: ${JSON.stringify(info.riskTicket)}`);
    }
  }
  for (const text of ["Stop ladder", "5%", "10%", "Policy", "ATR"]) {
    if (!info.ladder.includes(text)) {
      throw new Error(`Protection ladder missing ${JSON.stringify(text)}: ${JSON.stringify(info.ladder)}`);
    }
  }
  const coverageLedgerLower = info.coverageLedger.toLowerCase();
  for (const text of ["smoke", "unprotected", "part", "partial", "cover", "covered", "old", "reconcile required"]) {
    if (!coverageLedgerLower.includes(text)) {
      throw new Error(`Protection coverage ledger missing ${JSON.stringify(text)}: ${JSON.stringify(info.coverageLedger)}`);
    }
  }
  const portfolioDetailLower = info.portfolioDetail.toLowerCase();
  for (const text of ["protection coverage", "largest unprotected", "stale protective orders"]) {
    if (!portfolioDetailLower.includes(text)) {
      throw new Error(`Portfolio protection coverage detail missing ${JSON.stringify(text)}: ${JSON.stringify(info.portfolioDetail)}`);
    }
  }
  if (!info.stressDetail.toLowerCase().includes("protection coverage")) {
    throw new Error(`Stress detail does not include protection coverage context: ${JSON.stringify(info.stressDetail)}`);
  }
  return info;
}

// Alerts is the annunciator log: lit tiles above, the extinguished register
// below, and nothing else. Every row must carry the engraved source placard,
// the served title, and a worded age line; act tiles must never sit under
// watch tiles; and an extinguished row must read unlit.
async function exerciseAlertHistory(page) {
  const SEVERITY_RANK = { act: 0, watch: 1, "": 2 };
  const initiallyOpen = await page.locator("#alertsPanel").evaluate((el) => !!el.open);
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
    await page.waitForFunction(() => document.getElementById("alertsPanel")?.open, { timeout: 5000 });
  }
  const info = await page.evaluate(() => {
    const readLog = (id) => [...document.querySelectorAll(`#${id} .alert-row`)].map((row) => ({
      placard: row.querySelector(".alert-row__source")?.textContent?.trim() || "",
      title: row.querySelector(".pd-alert__title")?.textContent?.trim() || "",
      age: row.querySelector(".pd-alert__age")?.textContent?.trim() || "",
      tint: row.classList.contains("pd-tile--act") ? "act" : row.classList.contains("pd-tile--watch") ? "watch" : "",
      out: row.classList.contains("pd-alert--out"),
    }));
    return {
      count: Number.parseInt(document.getElementById("alertCount")?.textContent || "0", 10) || 0,
      currentRows: document.querySelectorAll("#currentSignalList .alert-row").length,
      historyRows: document.querySelectorAll("#alertHistoryList .alert-row").length,
      currentCount: Number.parseInt(document.getElementById("currentSignalCount")?.textContent || "0", 10) || 0,
      historyCount: Number.parseInt(document.getElementById("alertHistoryCount")?.textContent || "0", 10) || 0,
      authority: document.getElementById("alertAuthorityState")?.textContent || "",
      coverage: document.getElementById("alertCoverageSummary")?.textContent || "",
      active: readLog("currentSignalList"),
      extinguished: readLog("alertHistoryList"),
      poster: document.querySelector("#currentSignalList .pd-poster__word")?.textContent?.trim() || "",
      quiet: document.querySelector("#currentSignalList .empty-row")?.textContent?.trim() || "",
      // A placard row carries its count or chip alongside the legend; the
      // legend is the first span, so read that rather than the whole row.
      placards: [...document.querySelectorAll("#alertsTab .pd-placard")].map((el) => (el.querySelector("span") || el).textContent?.trim() || ""),
      historyHidden: document.getElementById("alertsHistorySection")?.hidden !== false,
    };
  });
  let selected = false;
  const firstAlert = page.locator("#currentSignalList .alert-row:visible, #alertHistoryList .alert-row:visible").first();
  if ((await firstAlert.count()) > 0) {
    await firstAlert.click();
    await page.waitForFunction(() => {
      const panel = document.getElementById("selectedAlertPanel");
      return panel && !panel.hidden && document.getElementById("selectedAlertTitle")?.textContent?.trim();
    }, { timeout: 5000 });
    selected = true;
  }
  if (!info.authority || !info.coverage) throw new Error(`active alert authority did not render: ${JSON.stringify(info)}`);
  for (const row of [...info.active, ...info.extinguished]) {
    if (!row.placard || !row.title || !/^Lit /.test(row.age)) {
      throw new Error(`annunciator tile is incomplete: ${JSON.stringify(row)}`);
    }
  }
  const ranks = info.active.map((row) => SEVERITY_RANK[row.tint] ?? 9);
  if (ranks.some((rank, index) => index > 0 && rank < ranks[index - 1])) {
    throw new Error(`act tiles must sit above watch tiles: ${JSON.stringify(info.active.map((row) => row.tint))}`);
  }
  for (const row of info.extinguished) {
    if (!row.out || row.tint !== "" || !/, out /.test(row.age)) {
      throw new Error(`extinguished tile must read unlit with its burn window: ${JSON.stringify(row)}`);
    }
  }
  // A quiet log is either the engraved poster or the honest coverage
  // sentence — never an empty panel that could be mistaken for calm.
  if (info.active.length === 0 && info.poster !== "ALL DARK." && !/coverage is incomplete or stale/.test(info.quiet)) {
    throw new Error(`a quiet annunciator log must state why it is quiet: ${JSON.stringify({ poster: info.poster, quiet: info.quiet })}`);
  }
  if (!info.placards.includes("Active") || !info.placards.includes("Process evidence")) {
    throw new Error(`alerts placards are incomplete: ${JSON.stringify(info.placards)}`);
  }
  if (!info.historyHidden && !info.placards.some((placard) => /^Extinguished/.test(placard))) {
    throw new Error(`the extinguished register must carry its own placard: ${JSON.stringify(info.placards)}`);
  }
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
  }
  return {
    initially_open: initiallyOpen,
    opens: true,
    count: info.count,
    current_rows: info.currentRows,
    history_rows: info.historyRows,
    current_count: info.currentCount,
    history_count: info.historyCount,
    authority: info.authority,
    coverage: info.coverage,
    placards: info.placards,
    poster: info.poster,
    active_tints: info.active.map((row) => row.tint),
    first_age: info.active[0]?.age || info.extinguished[0]?.age || "",
    selected,
  };
}

// Source coverage, delivery evidence and the daily report check moved behind
// the Monitor's lamp-test stamp. Read-only QA: the report check's own button
// is asserted, never pressed.
async function exerciseLampTestDetail(page) {
  await page.locator("#tabMonitor").click();
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
  const closedBefore = await page.evaluate(() => document.getElementById("lampTestDialog")?.open === false);
  await page.locator("#lampTestButton").click();
  await page.waitForFunction(() => document.getElementById("lampTestDialog")?.open === true, { timeout: 5000 });
  const detail = await page.evaluate(() => {
    const dialog = document.getElementById("lampTestDialog");
    const alertsTab = document.getElementById("alertsTab");
    return {
      stamp: document.getElementById("lampTestStamp")?.textContent?.trim() || "",
      authority: document.getElementById("alertAuthorityState")?.textContent?.trim() || "",
      coverage: document.getElementById("alertCoverageSummary")?.textContent?.trim() || "",
      sourceRows: document.querySelectorAll("#alertSourceList .alert-source-row").length,
      delivery: document.getElementById("alertDeliveryHealth")?.textContent?.trim() || "",
      acceptance: document.getElementById("alertDeliveryAcceptance")?.textContent?.trim() || "",
      reportState: document.getElementById("reconciliationState")?.textContent?.trim() || "",
      reportButton: document.getElementById("reconciliationCheckButton")?.textContent?.trim() || "",
      seated: ["alertAuthoritySection", "alertSourceList", "alertDeliveryHealth", "reconciliationCard"]
        .filter((id) => dialog?.contains(document.getElementById(id))),
      strandedInAlerts: ["alertAuthoritySection", "alertSourceList", "alertDeliveryHealth", "reconciliationCard"]
        .filter((id) => alertsTab?.contains(document.getElementById(id))),
    };
  });
  await page.locator("#lampTestDialogClose").click();
  await page.waitForFunction(() => document.getElementById("lampTestDialog")?.open === false, { timeout: 5000 });
  if (closedBefore !== true) throw new Error("the lamp-test detail must start closed");
  if (detail.seated.length !== 4 || detail.strandedInAlerts.length > 0) {
    throw new Error(`lamp-test detail is not the seat for alert authority and the report check: ${JSON.stringify(detail)}`);
  }
  if (!detail.authority || !detail.coverage || !detail.delivery || !detail.acceptance || !detail.reportState) {
    throw new Error(`lamp-test detail did not render its served evidence: ${JSON.stringify(detail)}`);
  }
  if (detail.sourceRows === 0 && !/not initialized|rejected/i.test(detail.coverage)) {
    throw new Error(`lamp-test detail rendered no alert sources and did not say why: ${JSON.stringify(detail)}`);
  }
  if (detail.reportButton !== "Check again") {
    throw new Error(`the report check button lost its label: ${JSON.stringify(detail)}`);
  }
  return {
    opens: true,
    stamp: detail.stamp,
    authority: detail.authority,
    source_rows: detail.sourceRows,
    delivery: detail.delivery,
    report_state: detail.reportState,
  };
}

// Briefing contract: the daemon composes the prose, the SPA renders it
// verbatim. Either render is accepted (an older daemon serves no narrative and
// the row sections stay the surface), but when the narrative IS served the
// Panel Dark register must hold: movement placards, typed runs rendered as
// text rather than markup, and the reconcile sign-off reachable inside the
// Review movement exactly when the served row says it is signable.
async function assertBriefNarrative(page) {
  // Declared inside the function: the smoke invokes itself through a
  // top-level await mid-file, so module-level consts below that line are
  // still in the temporal dead zone when assertions run.
  const MOVEMENT_PLACARDS = ["Review \u00b7 last session", "Ready \u00b7 next open"];
  const SEVERITY_WORDS = ["observe", "watch", "act", "ok", "attention", "degraded", "unavailable"];
  const MARKUP_LEAKS = ["[f]", "[/f]", "[w]", "[/w]", "[a]", "[/a]", "<span", "<b>"];
  const FIXTURE_REPORT = "smoke-signoff-fixture";

  await page.locator("#tabBrief").click();
  await page.waitForFunction(() => document.getElementById("briefTab")?.hidden === false, { timeout: 5000 });
  await page.waitForSelector("#briefPanel:not([hidden])", { timeout: 5000 });
  // A brief source that is down renders its own empty state; report that as
  // what it is instead of timing out on a selector that cannot appear.
  const settled = await (await page.waitForFunction(() => {
    const sections = document.getElementById("briefSections");
    if (sections?.querySelector(".pd-brf-lead")) return "narrative";
    if (sections?.querySelector(".brief-section")) return "sections";
    if (sections?.querySelector(".brief-empty")) return "unavailable";
    return null;
  }, { timeout: 5000 })).jsonValue();
  if (settled === "unavailable") {
    await page.locator("#tabMonitor").click();
    await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
    return { mode: settled };
  }

  const served = await page.evaluate(async () => {
    const res = await fetch("/api/bootstrap", { credentials: "include" });
    const body = await res.json();
    const brief = body?.snapshot?.brief || {};
    return {
      narrative: Boolean(brief.narrative),
      review: brief.review || {},
      signable: brief.review?.one_tap?.signable === true && String(brief.review?.one_tap?.report_id || "") !== "",
    };
  });
  const rendered = await page.evaluate(() => {
    const sections = document.getElementById("briefSections");
    return {
      mode: sections?.classList.contains("brief-sections--narrative") ? "narrative" : "sections",
      placards: [...sections.querySelectorAll(".pd-placard")].map((el) => el.textContent?.trim() || ""),
      lead: sections.querySelector(".pd-brf-lead")?.textContent?.trim() || "",
      paragraphs: [...sections.querySelectorAll(".pd-brf-para")].map((el) => el.textContent?.trim() || ""),
      coda: sections.querySelector(".pd-brf-coda")?.textContent?.trim() || "",
      chip: sections.querySelector(".pd-chip")?.textContent?.trim() || "",
      roles: {
        figure: sections.querySelectorAll(".pd-fig").length,
        watch: sections.querySelectorAll(".pd-wtint").length,
        act: sections.querySelectorAll(".pd-atint").length,
      },
      text: sections.textContent || "",
      sectionHeadings: [...sections.querySelectorAll(".brief-section__head h3")].map((el) => el.textContent?.trim() || ""),
    };
  });
  if ((rendered.mode === "narrative") !== served.narrative) {
    throw new Error(`brief render mode disagrees with the served payload: ${JSON.stringify({ served: served.narrative, rendered: rendered.mode })}`);
  }
  if (rendered.mode !== "narrative") {
    // Older daemon: the row render must still be complete.
    if (rendered.sectionHeadings.join("|") !== "Review|Ready") {
      throw new Error(`brief row fallback is incomplete: ${JSON.stringify(rendered.sectionHeadings)}`);
    }
    await page.locator("#tabMonitor").click();
    await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
    return { mode: rendered.mode, headings: rendered.sectionHeadings };
  }

  if (!rendered.placards[0]?.startsWith("Briefing")) {
    throw new Error(`the briefing placard must stamp the brief: ${JSON.stringify(rendered.placards)}`);
  }
  for (const placard of MOVEMENT_PLACARDS) {
    if (!rendered.placards.some((text) => text.startsWith(placard))) {
      throw new Error(`briefing is missing the ${placard} placard: ${JSON.stringify(rendered.placards)}`);
    }
  }
  if (!rendered.lead || rendered.paragraphs.length === 0 || !rendered.coda) {
    throw new Error(`briefing must render a lead, paragraphs, and a coda: ${JSON.stringify(rendered)}`);
  }
  if (rendered.chip && !SEVERITY_WORDS.includes(rendered.chip.toLowerCase())) {
    throw new Error(`briefing severity chip must print the served vocabulary: ${JSON.stringify(rendered.chip)}`);
  }
  for (const leak of MARKUP_LEAKS) {
    if (rendered.text.includes(leak)) {
      throw new Error(`briefing prose leaked run markup ${JSON.stringify(leak)} into visible text`);
    }
  }

  // Sign-off reachability, both directions, against a frozen stream so a live
  // snapshot cannot repaint the fixture mid-assertion. Nothing is clicked:
  // /api/recon/signoff stays untouched.
  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = true; });
  const signoff = await page.evaluate(({ review, reportID }) => {
    const apply = globalThis.__canarySmoke?.applySnapshotPatch;
    if (!apply) throw new Error("smoke snapshot patch hook is unavailable");
    const read = () => {
      const button = document.getElementById("briefSignoffButton");
      return {
        present: Boolean(button),
        disabled: button?.disabled === true,
        title: button?.title || "",
        inReviewMovement: Boolean(button?.closest(".brief-signoff-seat")),
      };
    };
    apply({ brief: { review: { ...review, one_tap: { status: "ok", detail: "current report is signable", report_id: reportID, signable: true, blockers: [] } } } });
    const signable = read();
    apply({ brief: { review } });
    return { signable, restored: read() };
  }, { review: served.review, reportID: FIXTURE_REPORT });
  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = false; });
  if (!signoff.signable.present || signoff.signable.disabled || !signoff.signable.inReviewMovement || !signoff.signable.title.includes(FIXTURE_REPORT)) {
    throw new Error(`a signable report must seat the sign-off control in the Review movement: ${JSON.stringify(signoff.signable)}`);
  }
  if (signoff.restored.present !== served.signable) {
    throw new Error(`sign-off availability must follow the served row: ${JSON.stringify({ served: served.signable, restored: signoff.restored })}`);
  }

  await page.locator("#tabMonitor").click();
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
  return {
    mode: rendered.mode,
    placards: rendered.placards.length,
    paragraphs: rendered.paragraphs.length,
    roles: rendered.roles,
    chip: rendered.chip,
    signoff_seated: signoff.signable.inReviewMovement,
    signoff_served: served.signable,
  };
}

async function exerciseGovernanceFixtures(page) {
  const mutationPaths = ["/api/push/test", "/api/governance/cutover-review", "/api/recon/check", "/api/brief/seen"];
  const fetchesBefore = await page.evaluate((paths) => globalThis.__canarySmoke.fetches.filter((item) => paths.some((path) => item.url.endsWith(path))).length, mutationPaths);
  await page.locator("#tabAlerts").click();
  await page.waitForFunction(() => document.getElementById("alertsTab")?.hidden === false, { timeout: 5000 });
  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = true; });

  const renderFixture = (fixture) => page.evaluate((value) => {
    const apply = globalThis.__canarySmoke?.applySnapshotPatch;
    if (!apply) throw new Error("smoke snapshot patch hook is unavailable");
    apply(value.patch);
  }, fixture);
  const now = new Date();
  const asOf = now.toISOString();
  const earlier = new Date(now.getTime() - 60_000).toISOString();
  const later = new Date(now.getTime() + 3_600_000).toISOString();
  const readyInput = { status: "ok", as_of: asOf };
  const readyHealth = {
    aggregate: "ready", policy: readyInput, reconciliation: readyInput, capital: readyInput,
    pins: readyInput, cadence: readyInput, confirmed_flow: readyInput,
  };
  const baseGovernance = {
    candidates: [],
    source_health: {},
    poll_source: {},
    occurrences: [{
      display_id: "gov-1111111111111111", kind: "monthly_pulse", state: "due", severity: "watch",
      title: "Monthly risk pulse", body: "Monthly risk pulse is ready. Review the brief and policy pins.",
      destination: "monitor", occurred_at: earlier, first_seen_at: earlier, last_seen_at: asOf,
      fingerprint: "private-fingerprint-sentinel", target_ref: "private-target-sentinel", notes: "private-note-sentinel",
    }, {
      display_id: "gov-2222222222222222", kind: "policy_drift", state: "open", severity: "act",
      title: "Policy drift", body: "Approved policy identities changed. Review the drift table.",
      destination: "monitor", occurred_at: earlier, first_seen_at: earlier, last_seen_at: earlier, resolved_at: asOf,
    }, {
      display_id: "gov-3333333333333333", kind: "reconcile_due", state: "overdue", severity: "act",
      title: "Reconciliation overdue", body: "Reconciliation is overdue. Open IBKR for the current report.",
      destination: "monitor", occurred_at: earlier, first_seen_at: earlier, last_seen_at: earlier, expires_at: earlier,
    }],
    attempts: [{ class: "transport_retry", target_ref: "private-target-sentinel", raw_error: "private-error-sentinel" }],
    attempt_aggregate: { cumulative_attempts: 2, push_service_accepted: 1, retryable_failures: 1, rejected: 0, retry_pending: 1, missed: 0, suppressed: 0 },
    health_aggregate: { partial_episodes: 1, state_write_failures: 0, recoveries: 0, overflows: 0 },
    delivery_health: { state: "healthy", updated_at: asOf, last_push_service_acceptance_at: earlier },
    diagnostic: { state: "push_service_accepted", at: earlier },
    reconciliation: {
      report: { state: "current", reason: "none", coverage_to: asOf.slice(0, 10), last_completed_at: asOf, retry_automatic: true, can_check_now: true },
      evaluation: { state: "complete", reason: "none" },
    },
  };

  // The monthly-pulse assertions below read the brief ROW render, so every
  // brief fixture in this function pins narrative: null. Without it the live
  // daemon's composed prose survives the shallow patch merge and the SPA
  // renders the narrative instead, where these rows do not exist.
  await renderFixture({ patch: {
    sources: { nudges: { state: "current", updated_at: asOf, last_success_at: asOf } },
    nudges: { as_of: asOf, candidates: [], source_health: readyHealth, context: null, confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: false } },
    brief: { narrative: null, stamp_target: "", brief_fingerprint: "", ready: { monthly_pulse: { status: "not_due", month: "2099-01", due_at: later } } },
    governance: baseGovernance,
    governanceRefreshSucceeded: true,
  } });
  const notDue = await page.evaluate(() => [...document.querySelectorAll("#briefSections .brief-row")]
    .find((row) => row.querySelector(".brief-row__head b")?.textContent === "Monthly pulse")?.textContent || "");
  if (!notDue.includes("not due")) throw new Error(`governance not-due fixture is incomplete: ${JSON.stringify(notDue)}`);

  await renderFixture({ patch: {
    sources: { nudges: { state: "current", updated_at: asOf, last_success_at: asOf } },
    nudges: {
      as_of: asOf,
      candidates: [{
        fingerprint: "private-fingerprint-sentinel", kind: "monthly_pulse", state: "due", severity: "watch",
        title: "Monthly risk pulse", body: "Monthly risk pulse is ready. Review the brief and policy pins.",
        occurred_at: earlier, due_at: earlier, destination: "monitor", url: "https://evil.example/private",
      }],
      source_health: { ...readyHealth, aggregate: "degraded", confirmed_flow: { status: "unapproved", reason: "cutover_review_required", as_of: asOf } },
      context: { shadow: { count: 2 }, drawdown: { tier: "block", consumed_pct: 0 } },
      confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: true },
    },
    brief: { narrative: null, stamp_target: "", brief_fingerprint: "", ready: { monthly_pulse: { status: "due", month: "2099-01", due_at: earlier } } },
    governance: baseGovernance,
  } });
  await page.waitForFunction(() => document.getElementById("governanceCurrentList")?.textContent?.includes("Monthly risk pulse"), { timeout: 5000 });
  const due = await page.evaluate(() => ({
    ids: [
      "governanceCurrentState", "governanceCurrentCount", "governanceCurrentList", "governanceSourceHealth",
      "governanceContext", "governanceCoverage", "governanceCoverageDetail", "governanceEvidenceDetails", "governanceCutoverReviewButton", "governanceCutoverReviewStatus",
      "governanceHistoryCount", "governanceHistoryList", "safeNotificationTestButton", "safeNotificationTestStatus",
    ].filter((id) => !document.getElementById(id)),
    current: document.getElementById("governanceCurrentList")?.textContent || "",
    source: document.getElementById("governanceSourceHealth")?.textContent || "",
    context: document.getElementById("governanceContext")?.textContent || "",
    coverage: document.getElementById("governanceCoverage")?.textContent || "",
    coverageDetail: document.getElementById("governanceCoverageDetail")?.textContent || "",
    detailsOpen: document.getElementById("governanceEvidenceDetails")?.open,
    cutoverVisible: !document.getElementById("governanceCutoverReviewButton")?.hidden,
    history: document.getElementById("governanceHistoryList")?.textContent || "",
    monthly: [...document.querySelectorAll("#briefSections .brief-row")].find((row) => row.querySelector(".brief-row__head b")?.textContent === "Monthly pulse")?.textContent || "",
    visible: document.querySelector(".governance-section")?.textContent || "",
  }));
  if (due.ids.length > 0 || !due.current.includes("Monthly risk pulse") || !due.source.includes("Payment records: not enabled · one-time review needed") || !due.context.includes("Warning-only observations 2") || !due.context.includes("0.0% used") || !due.coverage.includes("Older payments need a one-time review") || !due.coverageDetail.includes("older payments need review") || due.detailsOpen !== false || !due.cutoverVisible || !["active", "resolved"].every((status) => due.history.includes(status)) || !due.monthly.includes("due")) {
    throw new Error(`governance due fixture is incomplete: ${JSON.stringify(due)}`);
  }
  for (const privateText of ["private-fingerprint-sentinel", "private-target-sentinel", "private-note-sentinel", "private-error-sentinel", "evil.example"]) {
    if (due.visible.includes(privateText)) throw new Error(`governance fixture leaked private text ${JSON.stringify(privateText)}`);
  }

  await renderFixture({ patch: {
    sources: { nudges: { state: "current", updated_at: asOf, last_success_at: asOf } },
    nudges: { as_of: asOf, candidates: [], source_health: readyHealth, context: null, confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: false } },
    governance: {
      ...baseGovernance,
      reconciliation: {
        report: { state: "retry_scheduled", reason: "report_not_ready", coverage_to: earlier.slice(0, 10), next_attempt_at: later, retry_automatic: true, can_check_now: true },
        evaluation: { state: "waiting", reason: "report_pending" },
      },
    },
  } });
  const reportRetry = await page.evaluate(() => ({
    state: document.getElementById("reconciliationState")?.textContent || "",
    heading: document.getElementById("reconciliationHeading")?.textContent || "",
    summary: document.getElementById("reconciliationSummary")?.textContent || "",
    meta: document.getElementById("reconciliationMeta")?.textContent || "",
  }));
  if (reportRetry.state !== "Retrying" || !reportRetry.heading.includes("recheck will retry") || !reportRetry.summary.includes("still has the report through") || !reportRetry.summary.includes("did not finish the re-read") || !reportRetry.meta.includes("Next automatic try")) {
    throw new Error(`governance report-retry fixture is incomplete: ${JSON.stringify(reportRetry)}`);
  }

  await renderFixture({ patch: {
    governance: {
      ...baseGovernance,
      reconciliation: {
        report: { state: "action_required", reason: "token_expired", coverage_to: earlier.slice(0, 10), retry_automatic: false, can_check_now: true },
        evaluation: { state: "waiting", reason: "report_pending" },
      },
    },
  } });
  const reportAction = await page.evaluate(() => ({
    state: document.getElementById("reconciliationState")?.textContent || "",
    heading: document.getElementById("reconciliationHeading")?.textContent || "",
    summary: document.getElementById("reconciliationSummary")?.textContent || "",
    button: document.getElementById("reconciliationCheckButton")?.textContent || "",
    buttonDisabled: document.getElementById("reconciliationCheckButton")?.disabled,
  }));
  if (reportAction.state !== "Needs you" || !reportAction.heading.includes("Fix the report connection") || !reportAction.summary.includes("Flex Web Service token") || !reportAction.summary.includes("~/.config/ibkr/flex-token") || reportAction.button !== "Check again" || reportAction.buttonDisabled !== false) {
    throw new Error(`governance report-action fixture is incomplete: ${JSON.stringify(reportAction)}`);
  }

  await renderFixture({ patch: {
    governance: {
      ...baseGovernance,
      reconciliation: {
        report: { state: "unavailable", reason: "authority_unavailable", coverage_to: earlier.slice(0, 10), retry_automatic: false, can_check_now: false },
        evaluation: { state: "waiting", reason: "report_pending" },
      },
    },
  } });
  const reportUnavailable = await page.evaluate(() => ({
    state: document.getElementById("reconciliationState")?.textContent || "",
    summary: document.getElementById("reconciliationSummary")?.textContent || "",
    buttonDisabled: document.getElementById("reconciliationCheckButton")?.disabled,
  }));
  if (reportUnavailable.state !== "Unavailable" || !reportUnavailable.summary.includes("Restart Canary") || !reportUnavailable.summary.includes("local Canary data store") || reportUnavailable.buttonDisabled !== true) {
    throw new Error(`governance report-unavailable fixture is incomplete: ${JSON.stringify(reportUnavailable)}`);
  }

  await renderFixture({ patch: {
    sources: { nudges: { state: "current", updated_at: asOf, last_success_at: asOf } },
    nudges: { candidates: [], source_health: { ...readyHealth, aggregate: "suppressed", pins: { status: "stale", reason: "evidence_stale", as_of: asOf } }, context: { drawdown: { tier: "block", consumed_pct: null } }, confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: false } },
    brief: { narrative: null, stamp_target: "", brief_fingerprint: "", ready: { monthly_pulse: { status: "blocked", month: "2099-01" } } },
    governance: baseGovernance,
  } });
  const blocked = await page.evaluate(() => ({ source: document.getElementById("governanceSourceHealth")?.textContent || "", context: document.getElementById("governanceContext")?.textContent || "", monthly: [...document.querySelectorAll("#briefSections .brief-row")].find((row) => row.querySelector(".brief-row__head b")?.textContent === "Monthly pulse")?.textContent || "" }));
  if (!blocked.source.includes("Saved approvals: out of date · information is out of date") || !blocked.context.includes("measurement unavailable") || !blocked.monthly.includes("blocked by policy evidence")) throw new Error(`governance blocked fixture is incomplete: ${JSON.stringify(blocked)}`);

  await renderFixture({ patch: {
    sources: { nudges: { state: "current", updated_at: asOf, last_success_at: asOf } },
    nudges: { candidates: [], source_health: readyHealth, context: null, confirmed_flow_coverage: { coverage_from: earlier, pre_cutover_flows_unreviewed: false } },
    brief: { narrative: null, stamp_target: "", brief_fingerprint: "", ready: { monthly_pulse: { status: "completed", month: "2099-01", completed_at: asOf } } },
  } });
  const completed = await page.evaluate(() => ({ current: document.getElementById("governanceCurrentList")?.textContent || "", monthly: [...document.querySelectorAll("#briefSections .brief-row")].find((row) => row.querySelector(".brief-row__head b")?.textContent === "Monthly pulse")?.textContent || "" }));
  if (!completed.current.includes("No current risk and process reminders") || !completed.monthly.includes("completed this month")) throw new Error(`governance completed fixture is incomplete: ${JSON.stringify(completed)}`);

  await renderFixture({ patch: {
    sources: { nudges: { state: "stale", reason: "poll_stale", updated_at: asOf, last_success_at: earlier } },
    nudges: { candidates: [{ title: "Stale retained candidate", body: "Retained", severity: "act", destination: "alerts" }] },
    governance: baseGovernance,
    governanceRefreshSucceeded: true,
  } });
  const stale = await page.evaluate(() => ({
    state: document.getElementById("governanceCurrentState")?.textContent || "",
    current: document.getElementById("governanceCurrentList")?.textContent || "",
    source: document.getElementById("governanceSourceHealth")?.textContent || "",
  }));
  if (stale.state !== "Unavailable" || stale.current.includes("Stale retained candidate") || !stale.current.includes("unavailable") || !stale.source.includes("out of date · latest update is late") || !stale.source.includes("updated") || !stale.source.includes("last successful")) {
    throw new Error(`governance stale fixture is incomplete: ${JSON.stringify(stale)}`);
  }

  await renderFixture({ patch: {
    sources: { nudges: { state: "not_observed", reason: "not_observed" } },
    nudges: { candidates: [{ title: "Unobserved retained candidate", body: "Retained", severity: "act", destination: "alerts" }] },
  } });
  const notObserved = await page.evaluate(() => ({
    state: document.getElementById("governanceCurrentState")?.textContent || "",
    current: document.getElementById("governanceCurrentList")?.textContent || "",
    source: document.getElementById("governanceSourceHealth")?.textContent || "",
  }));
  // The redesigned chip renders "waiting" for a not-yet-observed poll; the
  // raw enum only appears inside the source-health evidence line.
  if (notObserved.state !== "Waiting" || notObserved.current.includes("Unobserved retained candidate") || !notObserved.source.includes("waiting for first check · not checked yet")) {
    throw new Error(`governance not-observed fixture is incomplete: ${JSON.stringify(notObserved)}`);
  }

  await renderFixture({ patch: {
    sources: { nudges: { state: "unavailable", reason: "transport_unavailable", updated_at: asOf, last_success_at: earlier } },
    nudges: { candidates: [{ title: "Retained candidate must not win", body: "Retained", severity: "act", destination: "alerts" }] },
    governance: baseGovernance,
    governanceRefreshSucceeded: false,
  } });
  const unavailable = await page.evaluate(() => ({
    state: document.getElementById("governanceCurrentState")?.textContent || "",
    current: document.getElementById("governanceCurrentList")?.textContent || "",
    source: document.getElementById("governanceSourceHealth")?.textContent || "",
    history: document.getElementById("governanceHistoryList")?.textContent || "",
  }));
  if (unavailable.state !== "Unavailable" || !unavailable.current.includes("unavailable") || unavailable.current.includes("Retained candidate") || !unavailable.source.includes("the Mac could not reach the service") || !unavailable.source.includes("updated") || !unavailable.source.includes("last successful") || !unavailable.history.includes("Monthly risk pulse")) {
    throw new Error(`governance unavailable-with-history fixture is incomplete: ${JSON.stringify(unavailable)}`);
  }

  await new Promise((resolve) => setTimeout(resolve, 100));
  const fetchesAfter = await page.evaluate((paths) => globalThis.__canarySmoke.fetches.filter((item) => paths.some((path) => item.url.endsWith(path))).length, mutationPaths);
  if (fetchesAfter !== fetchesBefore) throw new Error(`governance fixture QA called a mutation endpoint: before=${fetchesBefore} after=${fetchesAfter}`);
  // Hold the Alerts tab until the SPA's dwell-gated acknowledge has fired
  // (and been diverted); leaving earlier would cancel the dwell and the
  // guard assertion downstream would see zero intercepts.
  const dwellDeadline = Date.now() + 10000;
  while ((await attentionReadInterceptedCount(page)) === 0 && Date.now() < dwellDeadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  await page.locator("#tabMonitor").click();
  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = false; });
  return { not_due: notDue, due, report_retry: reportRetry, report_action: reportAction, report_unavailable: reportUnavailable, blocked, completed, stale, not_observed: notObserved, unavailable_with_history: unavailable, mutation_fetches: 0 };
}

// Orders lives on its own bottom-nav tab (Monitor, Brief, Alerts, Orders,
// Settings) rather than an inline <details> panel — visibility is
// tab-driven, not a per-panel open/closed toggle, and the panel itself is
// always present once the tab is active (emptiness is signaled by the
// ordersOpenCount badge and an .empty-row message, not by hiding the panel).
async function exerciseOpenOrders(page) {
  await page.locator("#tabOrders").click();
  await page.waitForFunction(() => document.getElementById("ordersTab")?.hidden === false, { timeout: 5000 });
  const info = await page.evaluate(() => {
    const buttons = [...document.querySelectorAll("#ordersOpenList button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    }));
    return {
      panelPresent: !!document.getElementById("ordersPanel"),
      countText: document.getElementById("ordersOpenCount")?.textContent?.trim() || "",
      rows: document.querySelectorAll("#ordersOpenList .open-order-row").length,
      empty: document.getElementById("ordersOpenList")?.textContent?.includes("No open orders available for this view.") || false,
      buttons,
      oldLabels: buttons.map((button) => button.text).filter((label) => ["Modify", "Cancel", "Execute"].includes(label)),
    };
  });
  if (!info.panelPresent) {
    throw new Error("Orders panel should always be present once the Orders tab is active");
  }
  if (info.oldLabels.length > 0) {
    throw new Error(`open-order controls still use old labels: ${info.oldLabels.join(", ")}`);
  }
  if (info.rows === 0 && !info.empty) {
    throw new Error("open-order empty state is missing");
  }
  const expectedCount = info.rows === 1 ? "1 open" : `${info.rows} open`;
  if (info.countText !== expectedCount) {
    throw new Error(`orders open-count badge should read ${JSON.stringify(expectedCount)}, got ${JSON.stringify(info.countText)}`);
  }
  if (info.rows > 0) {
    for (const label of ["Preview change", "Apply change", "Cancel order"]) {
      if (!info.buttons.some((button) => button.text === label)) {
        throw new Error(`open-order control ${JSON.stringify(label)} is missing`);
      }
    }
    for (const button of info.buttons.filter((item) => item.disabled)) {
      if (!button.title) {
        throw new Error(`disabled open-order control lacks a reason: ${JSON.stringify(button.text)}`);
      }
    }
  }
  await page.locator("#tabMonitor").click();
  await page.waitForFunction(() => document.getElementById("dashboard")?.hidden === false, { timeout: 5000 });
  return {
    rows: info.rows,
    empty: info.empty,
    count_text: info.countText,
    buttons: info.buttons.map((button) => ({ text: button.text, disabled: button.disabled, has_reason: !!button.title })),
  };
}

async function exerciseSettingsTab(page) {
  const settingWritesBefore = await page.evaluate(() => globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/settings")).length);
  await page.locator("#tabSettings").click();
  await page.waitForFunction(() => document.getElementById("settingsTab")?.hidden === false, { timeout: 5000 });
  const selectors = [
    "#settingsTab",
    "#settingsAsOf",
    "#purgeRestoreToggle",
    "#stockProtectionToggle",
    "#settingsTradingStatus",
    "#settingsTradingMeta",
    "#settingsTradingLimits",
    "#settingsTradingLimitsMeta",
    "#settingsMarketDataStatus",
    "#settingsMarketDataMeta",
    "#settingsBuildStatus",
    "#settingsBuildMeta",
    "#settingsProtectionStatus",
    "#settingsProtectionMeta",
    "#settingsPolicyStatus",
    "#settingsPolicyMeta",
    "#alertSegments",
    "#pushState",
    "#enablePushButton",
    "#safeNotificationTestButton",
    "#safeNotificationTestStatus",
  ];
  const elements = await page.evaluate((expectedSelectors) => expectedSelectors.map((selector) => {
    const element = document.querySelector(selector);
    return {
      selector,
      present: Boolean(element),
      visible: Boolean(element && !element.closest("[hidden]") && element.getClientRects().length > 0),
    };
  }), selectors);
  for (const element of elements) {
    if (!element.present || !element.visible) {
      throw new Error(`Settings tab element ${element.selector} should be present and visible: ${JSON.stringify(element)}`);
    }
  }
  const notification = await page.evaluate(() => ({
    modes: [...document.querySelectorAll("#alertSegments button")].map((button) => button.textContent.trim()),
    copy: document.querySelector(".settings-notification-card")?.textContent || "",
  }));
  if (JSON.stringify(notification.modes) !== JSON.stringify(["Off", "Action required", "Watch + action"]) || !notification.copy.includes("global for this app host and all paired devices") || !notification.copy.includes("Off stops phone notifications while your in-app history remains") || !notification.copy.includes("Action required sends urgent items only") || !notification.copy.includes("Watch + action also sends review reminders") || !notification.copy.includes("not configured here") || !notification.copy.includes("shared across paired devices")) {
    throw new Error(`Settings notification card is incomplete: ${JSON.stringify(notification)}`);
  }
  const settingWritesAfter = await page.evaluate(() => globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/settings")).length);
  if (settingWritesAfter !== settingWritesBefore) throw new Error("rendered Settings smoke changed the alert delivery setting");
  await page.locator("#tabMonitor").click();
  await page.waitForFunction(() => document.getElementById("dashboard")?.hidden === false, { timeout: 5000 });
  return {
    elements: elements.map((element) => element.selector),
    notification,
  };
}

async function readMarketContext(page) {
  return page.evaluate(() => ({
    marketContextPanelPresent: !!document.getElementById("marketPanel"),
    quotes: [...document.querySelectorAll("#marketQuoteStrip .market-quote-cell")].map((cell) => ({
      symbol: cell.querySelector("b")?.textContent?.trim() || "",
      price: cell.querySelector("strong")?.textContent?.trim() || "",
      change: cell.querySelector(".market-change")?.textContent?.trim() || "",
      source: cell.querySelector("small")?.textContent?.trim() || "",
    })),
    regime: document.getElementById("marketRegime")?.textContent?.trim() || "",
    weather: [...(document.getElementById("regimeSummaryCard")?.classList || [])].find((name) => name.startsWith("weather-")) || "",
  }));
}

async function assertDebugToolsRemoved(page, baseURL) {
  const domInfo = await page.evaluate(() => ({
    panel_present: !!document.getElementById("toolsPanel"),
    tool_buttons: document.querySelectorAll("[data-tool]").length,
  }));
  const cookies = await page.context().cookies(baseURL);
  const cookieHeader = cookies.map((cookie) => `${cookie.name}=${cookie.value}`).join("; ");
  const endpoint = new URL("/api/tools/events", baseURL);
  const res = await fetch(endpoint, {
    method: "POST",
    headers: cookieHeader ? { Cookie: cookieHeader } : {},
  });
  const info = {
    ...domInfo,
    endpoint_status: res.status,
  };
  if (info.panel_present || info.tool_buttons > 0) {
    throw new Error(`debug tools still render: ${JSON.stringify(info)}`);
  }
  if (info.endpoint_status < 400) {
    throw new Error(`debug tools endpoint still responds successfully: ${JSON.stringify(info)}`);
  }
  return info;
}

async function runShellJSON(command) {
  const started = Date.now();
  const { stdout, stderr } = await execFilePromise("/bin/sh", ["-lc", command]);
  if (stderr.trim()) {
    console.error(stderr.trim());
  }
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch (err) {
    throw new Error(`restart command did not emit JSON: ${String(err?.message || err)}\n${stdout}`);
  }
  cleanupPID = parsed.new_pid || 0;
  return { ...parsed, smoke_elapsed_ms: Date.now() - started };
}

function execFilePromise(file, argv) {
  return new Promise((resolve, reject) => {
    execFile(file, argv, { timeout: 30000, maxBuffer: 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        reject(new Error(`${file} ${argv.join(" ")} failed: ${String(err?.message || err)}\n${stderr}`));
        return;
      }
      resolve({ stdout, stderr });
    });
  });
}

function trimRight(value, suffix) {
  while (value.endsWith(suffix)) {
    value = value.slice(0, -suffix.length);
  }
  return value;
}
