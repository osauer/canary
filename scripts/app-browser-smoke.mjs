#!/usr/bin/env node

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
  const syntheticOrigin = "http://canary-synthetic.invalid";
  const syntheticURL = "http://canary-synthetic.invalid/?pair=expired-synthetic&nonce=synthetic-nonce&remote=synthetic-route";
  const staticRoot = resolve(fileURLToPath(new URL("../web/app/", import.meta.url)));
  const staticTypes = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".json": "application/json", ".webmanifest": "application/manifest+json" };
  const launchedSynthetic = await launchBrowser(playwright[browserName], browserName, { headless: true, ...(channel ? { channel } : {}) });
  const browser = launchedSynthetic.browser;
  const mutationRequests = [];
  let bootstrapRequests = 0;
  let deviceCookieRecoveries = 0;
  let deviceRecoveryRequired = false;
  let successfulPairings = 0;
  let pairingAttempts = 0;
  const externalRequests = [];
  let attention = {
    unread_count: 1,
    high_water_seq: 4,
    read_through_seq: 2,
    unread_refs: [
      { display_id: "alert-0123456789abcdef", source: "canary", kind: "portfolio_risk" },
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
    ],
    attention,
    delivery_health: { state: "healthy", class: "", updated_at: now, last_push_service_acceptance_at: now },
  };
  const readyInput = { status: "ok", as_of: now };
  const bootstrap = {
    auth: { authenticated: true },
    alert_settings: { mode: "watch_and_act" },
    alerts,
    settings: null,
    vapid_public_key: "",
    snapshot: {
      account: {},
      positions: {
        stocks: [], options: [], portfolio: {}, as_of: now,
        strategies: [{
          id: "strategy-synthetic-vertical", revision: 2, underlying: "SYN", kind: "vertical",
          source: "inferred", status: "current", units: 2, guaranteed_combo: true, actionable: true,
          position_fingerprint: "synthetic-position-fingerprint",
          legs: [
            { contract: { conid: 1101, symbol: "SYN", sec_type: "OPT", currency: "USD", expiry: "20260918", strike: 100, right: "C", trading_class: "SYN" }, quantity: 2, ratio: 1 },
            { contract: { conid: 1102, symbol: "SYN", sec_type: "OPT", currency: "USD", expiry: "20260918", strike: 105, right: "C", trading_class: "SYN" }, quantity: -2, ratio: -1 },
          ],
        }],
      },
      stress: { portfolio_fit: "low", portfolio: {}, fingerprint: { key: "synthetic-stress" } },
      brief: {
        as_of: now,
        brief_fingerprint: "sha256:synthetic-render",
        narrative: {
          lead: [{ text: "Synthetic desk ready.", role: "figure" }],
          review: [{ runs: [{ text: "Review synthetic process evidence.", role: "watch" }] }],
          ready: [{ runs: [{ text: "Act only on served synthetic evidence.", role: "act" }] }],
          coda: [{ text: "No account-derived data was loaded.", role: "" }],
        },
        review: { rules: { status: "ok", pass: 10, watch: 0, act: 0, unknown: 0 } },
        ready: { stress: { severity: "watch" } },
      },
      trading: { mode: "paper", account: "SYNTHETIC-PAPER", can_preview: true, can_write: true },
      proposals: {},
      opportunities: {},
      sources: { nudges: { state: "current", updated_at: now, last_success_at: now } },
      nudges: {
        as_of: now,
        candidates: [{ title: "Synthetic process review", body: "Review the current process exception.", severity: "act", destination: "alerts" }],
        source_health: {
          aggregate: "degraded", policy: readyInput, reconciliation: readyInput, capital: readyInput,
          pins: readyInput, cadence: readyInput,
          confirmed_flow: { status: "ok", as_of: now },
        },
        context: { shadow: { count: 1 }, drawdown: { tier: "block", consumed_pct: 0 } },
        confirmed_flow_coverage: { coverage_from: earlier },
      },
    },
  };
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await context.addInitScript(() => {
    globalThis.__canarySmoke = { applySnapshotPatch: null };
    try { Object.defineProperty(globalThis, "Notification", { configurable: true, value: undefined }); } catch {}
    try { Object.defineProperty(globalThis, "EventSource", { configurable: true, value: undefined }); } catch {}
    try { Object.defineProperty(globalThis.crypto, "subtle", { configurable: true, value: undefined }); } catch {}
  });
  await context.route("**/*", async (route) => {
    const request = route.request();
    const requestURL = new URL(request.url());
    if (requestURL.origin !== syntheticOrigin) {
      externalRequests.push({ method: request.method(), origin: requestURL.origin, path: requestURL.pathname });
      return route.abort("blockedbyclient");
    }
    const requestPath = requestURL.pathname;
    const method = request.method();
    if (!['GET', 'HEAD'].includes(method)) {
      mutationRequests.push({ method, path: requestPath, body: request.postData() || "" });
    }
    const json = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (method === "GET" && requestPath === "/api/bootstrap") {
      bootstrapRequests += 1;
      if (deviceRecoveryRequired) {
        const cookie = request.headers().cookie || "";
        if (!cookie.includes("ibkr_app_device=synthetic-device-cookie") || cookie.includes("ibkr_app_session=")) {
          return json({ error: "synthetic device-cookie recovery unavailable" }, 401);
        }
        await context.addCookies([{
          name: "ibkr_app_session",
          value: "synthetic-recovered-session",
          url: syntheticOrigin,
          httpOnly: true,
          sameSite: "Lax",
        }]);
        deviceCookieRecoveries += 1;
        deviceRecoveryRequired = false;
      }
      return json({ ...bootstrap, alerts });
    }
    if (method === "POST" && requestPath === "/api/pairing/complete") {
      pairingAttempts += 1;
      const body = request.postDataJSON();
      if (body.pairing_id === "expired-synthetic") {
        return json({ error: "synthetic pairing link expired" }, 410);
      }
      if (body.pairing_id === "fresh-synthetic" && body.nonce === "fresh-synthetic-nonce") {
        successfulPairings += 1;
        await context.addCookies([
          { name: "ibkr_app_device", value: "synthetic-device-cookie", url: syntheticOrigin, httpOnly: true, sameSite: "Lax" },
          { name: "ibkr_app_session", value: "synthetic-session", url: syntheticOrigin, httpOnly: true, sameSite: "Lax" },
        ]);
        return json({ device_id: "synthetic-device" });
      }
      return json({ error: "unexpected synthetic pairing request" }, 400);
    }
    if (method === "GET" && requestPath === "/api/alerts/attention") return json(attention);
    if (method === "GET" && requestPath === "/api/alerts") return json(alerts);
    if (method === "GET" && requestPath === "/api/orders/open") return json({
      as_of: now,
      orders: [{
        order_ref: "synthetic-order",
        symbol: "SYN",
        action: "SELL",
        quantity: 1,
        remaining: 1,
        order_type: "LMT",
        limit_price: 123.45,
        tif: "DAY",
        open: true,
        modify_eligible: false,
        cancel_eligible: false,
        lifecycle_state: "working",
      }],
    });
    if (method === "POST" && requestPath === "/api/strategies/preview") {
      const body = request.postDataJSON();
      if (body.strategy_id !== "strategy-synthetic-vertical" || body.expected_revision !== 2 || body.operation !== "close") {
        return json({ error: "unexpected synthetic strategy preview" }, 400);
      }
      return json({
        preview_token: "synthetic-strategy-preview-token",
        preview_token_scope: "strategy",
        token_minted: true,
        submit_eligible: true,
        executable: true,
        mode: "paper",
        account: "SYNTHETIC-PAPER",
        draft: {
          action: "SELL", order_type: "LMT", limit_price: 1.2, tif: "DAY", strategy: "group-close",
          contract: { symbol: "SYN", sec_type: "BAG", currency: "USD" },
          strategy_group: {
            strategy_id: "strategy-synthetic-vertical", strategy_revision: 2, operation: "close",
            units: 2, units_before: 2, units_after: 0, guaranteed_combo: true,
            legs: [
              { contract: { expiry: "20260918", strike: 100, right: "C" }, action: "SELL", quantity: 2, before: 2, after: 0 },
              { contract: { expiry: "20260918", strike: 105, right: "C" }, action: "BUY", quantity: 2, before: -2, after: 0 },
            ],
          },
        },
        notional_currency: "USD",
        what_if: { status: "accepted", available: true, margin: { commission: 1.15, commission_currency: "USD" } },
        as_of: now,
      });
    }
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
  const expectedPairingErrors = [];
  page.on("pageerror", (error) => errors.push(String(error?.message || error)));
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    const text = message.text();
    const sourceURL = message.location()?.url || "";
    if (/Failed to load resource: the server responded with a status of 410/.test(text) && sourceURL.endsWith("/api/pairing/complete")) {
      expectedPairingErrors.push({ text, sourceURL });
      return;
    }
    errors.push(text);
  });
  try {
    await page.goto(syntheticURL, { waitUntil: "domcontentloaded" });
    await waitForAuthenticatedApp(page);
    await assertVisibleRenameContract(page);
    try {
      await page.waitForFunction(() => document.getElementById("alertUnreadBadge")?.textContent === "1", { timeout: 5000 });
    } catch (error) {
      throw new Error(`synthetic unread did not render: ${errors.join(" | ") || error.message}`);
    }
    const monitor = await page.evaluate(() => ({
      active: document.getElementById("tabMonitor")?.classList.contains("active"),
      badge: document.getElementById("alertUnreadBadge")?.textContent || "",
      label: document.getElementById("tabAlerts")?.getAttribute("aria-label") || "",
      route: location.pathname + location.search,
      remote: localStorage.getItem("ibkrRemoteRoute") || "",
    }));
    const attentionRead = page.waitForResponse((response) => (
      response.request().method() === "POST"
        && new URL(response.url()).pathname === "/api/alerts/attention/read"
    ), { timeout: 5000 });
    await page.locator("#tabAlerts").click();
    await attentionRead;
    await page.waitForFunction(() => (
      document.getElementById("alertsTab")?.hidden === false
        && document.getElementById("alertUnreadBadge")?.textContent === "1"
        && document.getElementById("alertUnreadBadge")?.hidden === false
    ), { timeout: 5000 });
    const alertsView = await page.evaluate(() => ({
      activeAlerts: document.getElementById("currentSignalList")?.textContent || "",
      authority: document.getElementById("alertAuthorityState")?.textContent || "",
      litTiles: document.querySelectorAll("#currentSignalList .alert-row.pd-tile--watch").length,
      authoritySeated: document.getElementById("lampTestDialog")?.contains(document.getElementById("alertAuthorityState")) === true,
    }));
    await page.locator("#tabSettings").click();
    const settings = await page.evaluate(() => ({
      modes: [...document.querySelectorAll("#alertSegments button")].map((button) => button.textContent.trim()),
      copy: document.querySelector(".settings-notification-card")?.textContent || "",
      pushState: document.getElementById("pushState")?.textContent || "",
    }));
    await page.locator("#tabBrief").click();
    await page.waitForFunction(() => document.getElementById("briefTab")?.hidden === false, { timeout: 5000 });
    const briefView = await page.evaluate(() => ({
      narrative: document.getElementById("briefSections")?.classList.contains("brief-sections--narrative") === true,
      text: document.getElementById("briefSections")?.textContent || "",
      accountText: document.getElementById("accountLabel")?.textContent || "",
    }));
    await page.setViewportSize({ width: 591, height: 844 });
    await page.locator("#tabOrders").click();
    await page.waitForFunction(() => document.getElementById("ordersOpenCount")?.textContent === "1 open", { timeout: 5000 });
    const ordersView = await page.evaluate(() => ({
      count: document.getElementById("ordersOpenCount")?.textContent || "",
      text: document.getElementById("ordersOpenList")?.textContent || "",
      active: document.getElementById("ordersTab")?.hidden === false,
      layout: (() => {
        const row = document.querySelector("#ordersOpenList .open-order-row");
        const identity = row?.querySelector(".open-order-row__main");
        const rowRect = row?.getBoundingClientRect();
        const identityRect = identity?.getBoundingClientRect();
        return {
          viewport_width: window.innerWidth,
          grid_columns: row ? getComputedStyle(row).gridTemplateColumns.trim().split(/\s+/).filter(Boolean) : [],
          row_width: rowRect?.width || 0,
          identity_width: identityRect?.width || 0,
          horizontal_overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        };
      })(),
    }));
    await page.waitForFunction(() => document.getElementById("strategiesCount")?.textContent === "1 group", { timeout: 5000 });
    const strategyBefore = await page.evaluate(() => ({
      count: document.getElementById("strategiesCount")?.textContent || "",
      text: document.getElementById("strategiesList")?.textContent || "",
      previewButtons: document.querySelectorAll("#strategiesList .strategy-preview").length,
    }));
    await page.locator("#strategiesList .strategy-preview").click();
    await page.waitForFunction(() => document.querySelector("#strategiesList .strategy-submit")?.textContent === "Send combo order", { timeout: 5000 });
    const strategyAfter = await page.evaluate(() => ({
      text: document.getElementById("strategiesList")?.textContent || "",
      submitEnabled: document.querySelector("#strategiesList .strategy-submit")?.disabled === false,
      submitRequests: globalThis.__canarySmoke?.fetches?.filter((item) => item.url.endsWith("/api/strategies/submit")).length || 0,
    }));
    if (!monitor.active || monitor.badge !== "1" || monitor.label !== "Alerts, 1 open" || monitor.route !== "/" || monitor.remote !== "synthetic-route") throw new Error(`synthetic unread/pairing recovery state failed: ${JSON.stringify(monitor)}`);
    if (!alertsView.activeAlerts.includes("Synthetic watch") || alertsView.authority !== "Active") throw new Error(`synthetic Alerts state failed: ${JSON.stringify(alertsView)}`);
    if (alertsView.litTiles !== 1 || !alertsView.authoritySeated) throw new Error(`synthetic annunciator log failed: ${JSON.stringify(alertsView)}`);
    if (JSON.stringify(settings.modes) !== JSON.stringify(["Off", "Action required", "Watch + action"]) || !settings.copy.includes("global for this app host and all paired devices") || !settings.copy.includes("Off stops phone notifications while current alerts remain visible") || !settings.copy.includes("Action required sends urgent items only") || !settings.copy.includes("Watch + action also sends review reminders") || !settings.copy.includes("not configured here") || !settings.copy.includes("shared across paired devices") || settings.pushState !== "unsupported") throw new Error(`synthetic Settings state failed: ${JSON.stringify(settings)}`);
    if (!briefView.narrative || !briefView.text.includes("Synthetic desk ready.") || !briefView.text.includes("No account-derived data was loaded.") || briefView.accountText !== "Account unresolved") throw new Error(`synthetic Brief state failed: ${JSON.stringify(briefView)}`);
    if (!ordersView.active || ordersView.count !== "1 open" || !ordersView.text.includes("SYN")) throw new Error(`synthetic Orders state failed: ${JSON.stringify(ordersView)}`);
    if (strategyBefore.count !== "1 group" || !strategyBefore.text.includes("SYN · Vertical spread") || !strategyBefore.text.includes("2 units") || strategyBefore.previewButtons !== 1) throw new Error(`synthetic strategy group failed: ${JSON.stringify(strategyBefore)}`);
    if (!strategyAfter.text.includes("$1.20 per strategy") || !strategyAfter.text.includes("Broker preview Accepted") || !strategyAfter.text.includes("0 remaining") || !strategyAfter.submitEnabled || strategyAfter.submitRequests !== 0) throw new Error(`synthetic strategy preview failed: ${JSON.stringify(strategyAfter)}`);
    if (ordersView.layout.viewport_width !== 591 || ordersView.layout.grid_columns.length !== 1 || ordersView.layout.identity_width < ordersView.layout.row_width * 0.8 || ordersView.layout.horizontal_overflow) {
      throw new Error(`synthetic Orders compact layout collapsed or overflowed: ${JSON.stringify(ordersView.layout)}`);
    }
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator("#tabMonitor").click();
    const desktopLayout = await page.evaluate(() => ({
      viewport_width: window.innerWidth,
      active: document.getElementById("dashboard")?.hidden === false,
      nav_buttons: [...document.querySelectorAll("#bottomTabs button")].filter((button) => getComputedStyle(button).visibility === "visible").length,
      account_masked: document.getElementById("accountLabel")?.textContent === "Account unresolved",
      horizontal_overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
    }));
    if (desktopLayout.viewport_width !== 1280 || !desktopLayout.active || desktopLayout.nav_buttons !== 5 || !desktopLayout.account_masked || desktopLayout.horizontal_overflow) {
      throw new Error(`synthetic desktop layout failed: ${JSON.stringify(desktopLayout)}`);
    }
    await page.setViewportSize({ width: 390, height: 844 });
    const mutationPaths = mutationRequests.map(({ method, path }) => `${method} ${path}`);
    if (JSON.stringify(mutationPaths) !== JSON.stringify(["POST /api/pairing/complete", "POST /api/alerts/attention/read", "POST /api/strategies/preview"]) || JSON.parse(mutationRequests[1].body).through_seq !== 4) throw new Error(`unexpected synthetic mutations: ${JSON.stringify(mutationRequests)}`);
    if (pairingAttempts !== 1 || expectedPairingErrors.length !== 1 || bootstrapRequests < 1) throw new Error(`synthetic pairing recovery did not execute exactly once: ${JSON.stringify({ pairingAttempts, expectedPairingErrors, bootstrapRequests })}`);

    await page.evaluate(() => localStorage.setItem("canaryActiveTab", "monitor"));
    const bootstrapBeforeReload = bootstrapRequests;
    await page.reload({ waitUntil: "domcontentloaded" });
    await waitForAuthenticatedApp(page);
    const reload = await page.evaluate(() => ({
      active: document.getElementById("tabMonitor")?.classList.contains("active") === true,
      route: location.pathname + location.search,
      remote: localStorage.getItem("ibkrRemoteRoute") || "",
      pairingHidden: document.getElementById("pairingPanel")?.hidden === true,
    }));
    if (!reload.active || reload.route !== "/" || reload.remote !== "synthetic-route" || !reload.pairingHidden || pairingAttempts !== 1 || bootstrapRequests <= bootstrapBeforeReload) throw new Error(`synthetic reload/auth continuity failed: ${JSON.stringify({ reload, pairingAttempts, bootstrapRequests, bootstrapBeforeReload })}`);

    await context.clearCookies();
    await page.evaluate(() => localStorage.clear());
    await page.goto(`${syntheticOrigin}/?pair=fresh-synthetic&nonce=fresh-synthetic-nonce`, { waitUntil: "domcontentloaded" });
    await waitForAuthenticatedApp(page);
    const freshPairing = await page.evaluate(() => ({
      deviceStored: localStorage.getItem("ibkrDeviceID") === "synthetic-device",
      route: location.pathname + location.search,
      pairingHidden: document.getElementById("pairingPanel")?.hidden === true,
    }));
    const pairedCookieNames = new Set((await context.cookies(syntheticOrigin)).map((cookie) => cookie.name));
    if (!freshPairing.deviceStored || freshPairing.route !== "/" || !freshPairing.pairingHidden || successfulPairings !== 1 || !pairedCookieNames.has("ibkr_app_device") || !pairedCookieNames.has("ibkr_app_session")) {
      throw new Error(`synthetic fresh pairing failed: ${JSON.stringify({ freshPairing, successfulPairings, cookieCount: pairedCookieNames.size })}`);
    }

    await context.clearCookies({ name: "ibkr_app_session" });
    const continuityCookies = new Set((await context.cookies(syntheticOrigin)).map((cookie) => cookie.name));
    if (!continuityCookies.has("ibkr_app_device") || continuityCookies.has("ibkr_app_session")) {
      throw new Error(`synthetic session clear did not preserve only the device credential: ${JSON.stringify({ cookieCount: continuityCookies.size })}`);
    }
    deviceRecoveryRequired = true;
    const recoveryBootstrapBefore = bootstrapRequests;
    await page.reload({ waitUntil: "domcontentloaded" });
    await waitForAuthenticatedApp(page);
    const recoveredCookieNames = new Set((await context.cookies(syntheticOrigin)).map((cookie) => cookie.name));
    const authRecovery = await page.evaluate(() => ({
      deviceStored: localStorage.getItem("ibkrDeviceID") === "synthetic-device",
      route: location.pathname + location.search,
      pairingHidden: document.getElementById("pairingPanel")?.hidden === true,
    }));
    if (!authRecovery.deviceStored || authRecovery.route !== "/" || !authRecovery.pairingHidden || deviceRecoveryRequired || deviceCookieRecoveries !== 1 || bootstrapRequests <= recoveryBootstrapBefore || !recoveredCookieNames.has("ibkr_app_session")) {
      throw new Error(`synthetic device-cookie auth recovery failed: ${JSON.stringify({ authRecovery, deviceRecoveryRequired, deviceCookieRecoveries, bootstrapRequests, recoveryBootstrapBefore, cookieCount: recoveredCookieNames.size })}`);
    }
    const finalMutationPaths = mutationRequests.map(({ method, path }) => `${method} ${path}`);
    if (JSON.stringify(finalMutationPaths) !== JSON.stringify(["POST /api/pairing/complete", "POST /api/alerts/attention/read", "POST /api/strategies/preview", "POST /api/pairing/complete"])) {
      throw new Error(`unexpected synthetic mutations after fresh pairing: ${JSON.stringify(mutationRequests)}`);
    }
    if (externalRequests.length > 0) throw new Error(`synthetic browser attempted external requests: ${JSON.stringify(externalRequests)}`);
    if (errors.length > 0) throw new Error(`synthetic browser errors: ${errors.join("\n")}`);
    console.log(JSON.stringify({ ok: true, browser: browserName, mobile: true, isolated: true, synthetic_only: true, external_requests: 0, pairing: { expired_fallback: true, fresh_pairing: true, attempts: pairingAttempts }, monitor, brief: briefView, alerts: alertsView, orders: ordersView, strategies: { grouped: strategyBefore, preview: strategyAfter, submit_clicked: false }, desktop_layout: desktopLayout, settings, reload, auth_recovery: { device_cookie: true, session_reissued: true }, bootstrap_requests: bootstrapRequests, intercepted_mutations: mutationRequests.map(({ method, path }) => ({ method, path })) }, null, 2));
  } finally {
    await browser.close();
  }
}

async function waitForAuthenticatedApp(page) {
  await page.waitForFunction(() => (
    document.getElementById("pairingPanel")?.hidden === true
      && document.getElementById("tabPanels")?.hidden === false
      && document.getElementById("bottomTabs")?.hidden === false
  ), { timeout: 10000 });
}

const launched = await launchBrowser(playwright[browserName], browserName, launchOptions);
const browser = launched.browser;
const context = await browser.newContext({
  viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
  isMobile: mobile,
  hasTouch: mobile,
});
// The operator's real unread attention is human-only evidence: this smoke
// POST before any page interaction, never forward it, and answer with the
// clients.claim), and WebKit never surfaces SW-controlled page fetches to
// see it; this route stays only as a second net for engines and windows
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
    alertCursors: [],
    openedEvents: 0,
    attentionReadDiverted: 0,
  };
  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (...fetchArgs) => {
    const request = fetchArgs[0];
    const url = typeof request === "string" ? request : request?.url || "";
    const method = String((typeof request === "string" ? fetchArgs[1]?.method : request?.method || fetchArgs[1]?.method) || "GET").toUpperCase();
    if (method === "POST" && url.endsWith("/api/alerts/attention/read")) {
      // The QA page must never mark the operator's real unread as read.
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
    try {
      const res = await nativeFetch(...fetchArgs);
      globalThis.__canarySmoke.fetches.push({ url, status: res.status, at: Date.now() });
      if (res.ok && method === "GET" && (url.endsWith("/api/alerts") || url.endsWith("/api/alerts/attention"))) {
        res.clone().json().then((body) => {
          const attention = url.endsWith("/api/alerts/attention") ? body : body?.attention;
          globalThis.__canarySmoke.alertCursors.push({
            path: url.endsWith("/api/alerts/attention") ? "/api/alerts/attention" : "/api/alerts",
            unread_count: attention?.unread_count,
            high_water_seq: attention?.high_water_seq,
            read_through_seq: attention?.read_through_seq,
            unread_refs: Array.isArray(attention?.unread_refs)
              ? attention.unread_refs.map((ref) => `${ref.source}/${ref.kind}/${ref.display_id}`)
              : [],
          });
          if (globalThis.__canarySmoke.alertCursors.length > 8) globalThis.__canarySmoke.alertCursors.shift();
        }).catch(() => {});
      }
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
    // governance monthly-pulse assertion caught exactly that). The smoke's
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
  try {
    await page.waitForSelector("#bottomTabs:not([hidden])", { timeout: 15000 });
  } catch (error) {
    const pairingState = await page.evaluate(() => ({
      pairing_hidden: document.getElementById("pairingPanel")?.hidden !== false,
      pairing_text: document.getElementById("pairingStatus")?.textContent?.trim() || "",
      retry_visible: document.getElementById("retryAuthButton")?.hidden === false,
      path: location.pathname,
    }));
    throw new Error(`paired app did not authenticate: ${JSON.stringify(pairingState)}; ${error.message}`);
  }
  if (await page.locator("#dashboard").getAttribute("hidden") !== null) {
    await page.locator("#tabMonitor").click();
  }
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await waitForSnapshotEvent(page, 0);
  const title = await page.title();
  const visibleIdentity = await assertVisibleRenameContract(page);
  const connection = await waitForHeader(page);
  const pushState = await page.locator("#pushState").textContent();
  const eventsBefore = await fetchEventsDiagnostics(page);
  const privacy = await exerciseAccountPrivacy(page);
  const accountPanel = await exerciseAccountPanel(page);
  const accountAuthority = await exerciseAccountAuthorityFixtures(page);
  const snapshotBanner = await assertSnapshotBannerCopy(page);
  const marketLayout = await exerciseMarketLayout(page);
  const viewportOverflow = await assertNoViewportOverflow(page);
  const stressControls = await exerciseStressControlsRemoved(page);
  const underlyingBookFixture = await exerciseUnderlyingPanelFixture(page);
  const stressDetail = await exerciseStressDetail(page);
  const rulesCard = await exerciseRulesCard(page);
  const sheetLayer = await exerciseSheetLayer(page);
  const marketContext = await exerciseMarketContext(page);
  const portfolioDetail = await exercisePortfolioDetail(page);
  const protectionRiskRendering = await exerciseProtectionRiskRendering(page);
  const alertSurface = await exerciseAlerts(page);
  const lampTestDetail = await exerciseLampTestDetail(page);
  const briefNarrative = await assertBriefNarrative(page);
  // Prove the attention-read guard was armed and effective: the alerts tab
  // was just exercised in a visible headless page, so the SPA must have
  // attempted the acknowledge POST, and every attempt must have been
  const attentionGuardDeadline = Date.now() + 10000;
  attentionReadIntercepted = await attentionReadInterceptedCount(page);
  while (attentionReadIntercepted === 0 && Date.now() < attentionGuardDeadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    attentionReadIntercepted = await attentionReadInterceptedCount(page);
  }
  const attentionReadFetches = await page.evaluate(() => globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/attention/read")).length);
  // The ack deliberately skips its POST when nothing is unread, so on a
  // clean desk the guard proves itself differently: the ack path must have
  // reached the wire. Any POST that did fire must have been diverted.
  if (attentionReadIntercepted === 0) {
    const attentionState = await page.evaluate(() => ({
      aria: document.getElementById("tabAlerts")?.getAttribute("aria-label") || "",
      attentionGets: globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/attention")).length,
      unread: globalThis.__canarySmoke.alertCursors?.at(-1)?.unread_count,
    }));
    if (attentionState.unread !== 0) {
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
  const smokeState = await page.evaluate(() => globalThis.__canarySmoke);
  // The crypto-less pairing path used to mint a readable bearer secret here.
  // It no longer does — the HttpOnly device cookie is that path's only
  const plaintextDeviceSecret = await page.evaluate(() => !!localStorage.getItem("ibkrDeviceSecret"));
  if (plaintextDeviceSecret) {
    throw new Error("ibkrDeviceSecret is set: the plaintext device-secret fallback was reintroduced");
  }
  console.log(JSON.stringify({
    ok: true,
    browser: browserName,
    channel: launched.channel || null,
    base_url: baseURL,
    mobile,
    notification_removed: noNotification,
    webcrypto_removed: noWebCrypto,
    plaintext_device_secret: plaintextDeviceSecret,
    title,
    visible_identity: visibleIdentity,
    connection,
    push_state: pushState,
    privacy,
    account_panel: accountPanel,
    account_authority: accountAuthority,
    snapshot_banner: snapshotBanner,
    market_layout: marketLayout,
    viewport_overflow: viewportOverflow,
    stress_controls: stressControls,
    underlying_book_fixture: underlyingBookFixture,
    stress_detail: stressDetail,
    rules_card: rulesCard,
    sheet_layer: sheetLayer,
    market_context: marketContext,
    portfolio_detail: portfolioDetail,
    protection_risk_rendering: protectionRiskRendering,
    alerts: alertSurface,
    lamp_test_detail: lampTestDetail,
    brief_narrative: briefNarrative,
    open_orders: openOrders,
    settings_tab: settingsTab,
    debug_tools: debugTools,
    events: {
      opened_event_streams: eventsBefore.opened_event_streams,
      event_counts: smokeState.eventCounts,
    },
    attention_read_intercepted: attentionReadIntercepted,
    pair_expires_at: pairing.expires_at,
  }, null, 2));
} finally {
  await browser.close();
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
  if (!panel.accountLabel || !panel.dailyPnlPct || panel.riskValues.some((value) => !value)) {
    throw new Error(`account panel is missing values: ${JSON.stringify(panel)}`);
  }
  // Operator decision: the plate ALWAYS states the mode — LIVE in the plate's
  if (panel.pillHidden || !["LIVE", "PAPER", "mode?"].includes(panel.pill)) {
    throw new Error(`unexpected trading-env plate state: ${JSON.stringify(panel)}`);
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

async function exerciseAccountAuthorityFixtures(page) {
  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = true; });
  const now = new Date().toISOString();
  const result = await page.evaluate((asOf) => {
    const apply = globalThis.__canarySmoke?.applySnapshotPatch;
    if (!apply) throw new Error("smoke snapshot patch hook is unavailable");
    const read = () => ({
      account: document.getElementById("accountLabel")?.textContent?.trim() || "",
      net: document.getElementById("netLiquidation")?.textContent?.trim() || "",
      buyingPower: document.getElementById("buyingPower")?.textContent?.trim() || "",
      daily: document.getElementById("dailyPnl")?.textContent?.trim() || "",
      freshness: document.getElementById("accountAsOf")?.textContent?.trim() || "",
      freshnessHidden: document.getElementById("accountAsOf")?.hidden === true,
      positionsUnavailable: document.getElementById("underlyingBookCount")?.textContent?.trim() === "Positions unavailable"
        && /no single account is selected/i.test(document.getElementById("underlyingBookStatus")?.textContent || "")
        && /Position data unavailable/i.test(document.getElementById("underlyingBookList")?.textContent || ""),
		positionsClaimClean: /No underlyings|No held underlyings/i.test([
        document.getElementById("underlyingBookCount")?.textContent || "",
        document.getElementById("underlyingBookList")?.textContent || "",
      ].join(" ")),
      syncLabel: document.getElementById("syncStatusLabel")?.textContent?.trim() || "",
      syncState: document.getElementById("syncStatusState")?.textContent?.trim() || "",
    });
    const authority = {
      scope: { account_id: "SYNTHETIC-AUTHORITY", account_mode: "paper" },
      source: "account_summary_request",
      availability: "available",
      freshness: "current",
      as_of: asOf,
      fields: { base_currency: true, net_liquidation: true, buying_power: false, daily_pnl: true },
    };
    apply({
      status: { connected_account: "SYNTHETIC-AUTHORITY", account_mode: "paper" },
      account: {
        account_id: "IGNORED-LEGACY-ID", base_currency: "EUR", net_liquidation: 0, buying_power: 0, daily_pnl: 0,
        daily_pnl_observation: { status: "ok", as_of: asOf }, as_of: asOf, authority,
      },
      positions: { authority: { ...authority, source: "portfolio_stream", fields: undefined } },
      sources: { account: { state: "current", last_success_at: asOf } },
    });
    const maskedZero = read();
    document.getElementById("accountPrivacyToggle")?.click();
    const revealedZero = read();

    apply({ account: { authority: { ...authority, source: "account_updates_cache", freshness: "unknown", reason: "unstamped_cache", as_of: "" } } });
    const cached = read();

    apply({
      status: { connected_account: "", account_mode: "unknown" },
      account: {
        account_id: "STALE-LEGACY-ID", base_currency: "USD", net_liquidation: 0, buying_power: 0, daily_pnl: null,
        authority: {
          scope: { account_id: "", account_mode: "unknown" }, source: "account_summary_request",
          availability: "unavailable", freshness: "unknown", reason: "scope_unresolved",
          fields: { base_currency: false, net_liquidation: false, buying_power: false, daily_pnl: false },
        },
      },
      positions: {
        stocks: [], options: [], by_underlying: [],
        authority: { scope: { account_id: "", account_mode: "unknown" }, source: "portfolio_stream", availability: "unavailable", freshness: "unknown", reason: "scope_unresolved" },
      },
      sources: { account: { state: "current", last_success_at: asOf } },
    });
    const unavailable = read();
    document.getElementById("accountPrivacyToggle")?.click();
    return { maskedZero, revealedZero, cached, unavailable };
  }, now);

  if (result.maskedZero.net !== "******" || result.maskedZero.buyingPower !== "--" || result.maskedZero.daily !== "******") {
    throw new Error(`account authority must distinguish a private real zero from a missing zero: ${JSON.stringify(result.maskedZero)}`);
  }
  if (!/0/.test(result.revealedZero.net) || /\$|USD/.test(result.revealedZero.net) || !/€|EUR/.test(result.revealedZero.net) || result.revealedZero.buyingPower !== "--") {
    throw new Error(`account authority must reveal the genuine EUR zero without inventing USD: ${JSON.stringify(result.revealedZero)}`);
  }
  if (result.cached.freshness !== "Cached · time unknown") {
    throw new Error(`cached account context must name its unknown time: ${JSON.stringify(result.cached)}`);
  }
  if (result.unavailable.net !== "--" || result.unavailable.buyingPower !== "--" || result.unavailable.daily !== "--" || result.unavailable.account !== "Account unresolved" || result.unavailable.freshness !== "Account unavailable") {
    throw new Error(`unavailable account data must not render legacy zeros or a guessed account: ${JSON.stringify(result.unavailable)}`);
  }
  if (!result.unavailable.positionsUnavailable || result.unavailable.positionsClaimClean) {
    throw new Error(`an unavailable empty positions result must not read as a clean book: ${JSON.stringify(result.unavailable)}`);
  }
  if (result.unavailable.syncLabel !== "Data gaps" || result.unavailable.syncState !== "Degraded") {
    throw new Error(`unavailable account data must degrade the global sync plate: ${JSON.stringify(result.unavailable)}`);
  }

  await page.evaluate(() => { globalThis.__canarySmoke.freezeLiveEvents = false; });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await waitForSnapshotEvent(page, 0);
  return { genuine_zero: true, missing_zero: true, cached_named: true, unresolved_refused: true };
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
  // Panel Dark session chip: "RTH · closes 3:59:04" while open, and the
  await page.waitForFunction(() => {
    const text = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    return /\b(closes|opens in) \d+d? ?\d*:\d{2}:\d{2}\b/i.test(text);
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
    // The market dot's title is the other rendering of the same served
    const sessionDotTitle = document.getElementById("marketStateDot")?.getAttribute("title") || "";
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
      sessionDotTitle,
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
  if (!layout.marketOpen && !/\bopens in \d/i.test(layout.phase)) {
    throw new Error(`closed market chip should count down to the open: ${JSON.stringify(layout.phase)}`);
  }
  if (!/:\d{2}:\d{2}\b/.test(layout.phase)) {
    throw new Error(`session countdown should tick seconds: ${JSON.stringify(layout.phase)}`);
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
  // A closed market states WHY in chip text, not only in the title: a
  if (/closed for (the weekend|a holiday)/i.test(layout.sessionDotTitle) && /^opens\b/i.test(layout.phase)) {
    throw new Error(`closed session chip should lead with its served closure word: ${JSON.stringify(layout)}`);
  }
  if (/closed for the weekend/i.test(layout.sessionDotTitle) && !/^weekend\b/i.test(layout.phase)) {
    throw new Error(`weekend chip should print the served weekend word: ${JSON.stringify(layout)}`);
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
      const measured = (selector) => [...document.querySelectorAll(selector)]
        .map((tile) => tile.getBoundingClientRect())
        .filter((box) => box.width > 0);
      const regimeTiles = measured("#regimeSummaryCard > .pd-tile");
      const deskTiles = measured("#deskGrid > .pd-tile");
      const columns = (tiles) => new Set(tiles.map((tile) => Math.round(tile.left))).size;
		  const master = document.getElementById("masterAnnunciator")?.getBoundingClientRect();
		  const regimeGrid = document.getElementById("regimeSummaryCard")?.getBoundingClientRect();
		  const stress = document.getElementById("stressHero")?.getBoundingClientRect();
		  const marketSelect = document.getElementById("marketSelect");
      // Panel Dark: the master annunciator spans the panel above two fixed
      const signalLayout = regimeTiles.length > 0 ? {
        regimeTiles: regimeTiles.length,
        regimeColumns: columns(regimeTiles),
        deskTiles: deskTiles.length,
        deskColumns: columns(deskTiles),
        masterFullWidth: !!(master && regimeGrid) && Math.abs(master.width - regimeGrid.width) <= 4,
        masterHeight: master ? Math.round(master.height) : 0,
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
		  return {
			clientWidth,
			pageScrollWidth,
			offenders,
			signalLayout,
			marketSelectWidth: marketSelect ? Math.round(marketSelect.getBoundingClientRect().width) : 0,
			usOptionsLabel: [...(marketSelect?.options || [])].find((option) => option.value === "us-options")?.textContent?.trim() || "",
		  };
    });
    results.push({ ...size, ...info });
    if (info.pageScrollWidth > info.clientWidth + 1) {
      throw new Error(`page overflows at ${size.width}px: ${JSON.stringify(info)}`);
    }
    const layout = info.signalLayout;
    if (!layout || layout.regimeTiles !== 6 || layout.regimeColumns !== 3 || layout.deskTiles < 3 || layout.deskColumns !== 2) {
      throw new Error(`Regime should render a fixed 3x2 cluster grid (one window per daemon cluster) and Desk a two-column window grid at ${size.width}px: ${JSON.stringify(layout)}`);
    }
    if (!layout.masterFullWidth || !layout.masterAboveGrid || !layout.regimeBeforeDesk || !layout.signalPanelFullWidth) {
      throw new Error(`Master annunciator should span a full-width combined panel above the regime grid, with the desk grid beneath, at ${size.width}px: ${JSON.stringify(layout)}`);
    }
		if (layout.masterHeight > 96) {
		  throw new Error(`Master annunciator should remain a compact hierarchy signal at ${size.width}px: ${JSON.stringify(layout)}`);
		}
		if (info.marketSelectWidth > 52 || info.usOptionsLabel !== "US opt.") {
		  throw new Error(`Market selector should stay narrow with the abbreviated options label at ${size.width}px: ${JSON.stringify(info)}`);
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
		const now = new Date().toISOString();
		globalThis.__canarySmoke.applySnapshotPatch({
			positions: {
				authority: { availability: "available", freshness: "current", as_of: now },
				by_underlying: [{
					underlying: "SMOKE",
					group_daily_pnl_base: 125.5,
					stock: { symbol: "SMOKE", currency: "USD", mark: 444.12, quote_expectation: "none" },
				}],
				portfolio: { base_currency: "USD" },
			},
			sources: { positions: { state: "current", last_success_at: now } },
		});
	});
	await page.waitForFunction(() => document.querySelector('#underlyingBookList [data-symbol="SMOKE"]'), { timeout: 5000 });
	const info = await page.evaluate(() => {
		const row = document.querySelector('#underlyingBookList [data-symbol="SMOKE"]');
		return {
			count: document.getElementById("underlyingBookCount")?.textContent?.trim() || "",
			status: document.getElementById("underlyingBookStatus")?.textContent?.trim() || "",
			winner: document.getElementById("underlyingWinnerPnl")?.textContent?.trim() || "",
			accountHasUnderlyingBook: Boolean(document.querySelector("#accountPanel #underlyingBookList")),
			stressHasUnderlyingBook: Boolean(document.querySelector("#stressHero #underlyingBookList")),
			standaloneHasUnderlyingBook: Boolean(document.querySelector("#underlyingPanel #underlyingBookList")),
			foldIcon: Boolean(document.querySelector("#underlyingPanel #underlyingDetailToggle.panel-chevron")),
			rowText: row?.textContent?.replace(/\s+/g, " ").trim() || "",
			actions: row?.querySelectorAll("button").length || 0,
		};
	});
	if (info.accountHasUnderlyingBook || info.stressHasUnderlyingBook || !info.standaloneHasUnderlyingBook || !info.foldIcon) {
		throw new Error(`underlyings book is in the wrong panel or lacks its disclosure: ${JSON.stringify(info)}`);
	}
	if (!/1 held/.test(info.count) || !/SMOKE/.test(info.rowText) || !info.winner) {
		throw new Error(`held underlying did not render with its P\/L summary: ${JSON.stringify(info)}`);
	}
	if (info.actions !== 0) {
		throw new Error(`monitoring rows must not expose retired write actions: ${JSON.stringify(info)}`);
	}

	await page.reload({ waitUntil: "domcontentloaded" });
	await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
	await waitForSnapshotEvent(page, 0);
	return info;
}

async function exerciseStressDetail(page) {
  // Quiet-when-fresh blanks and hides the badge while the snapshot is fresh;
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
      await page.waitForFunction(() => {
        const badge = document.getElementById("stressAsOf");
        if (!badge) return false;
        const text = badge.textContent?.trim() || "";
        if (badge.hidden && !text) return true;
        return text && text !== "no timestamp" && text !== "updated --" && text !== "--";
      }, { timeout: 30000 });
    } catch {
      // A first stress poll can legitimately outlast this wait (fresh app
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
  // The checklist lives in a tap-through sheet now: the Monitor Rules window
  await page.locator("#stressRulesCard").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("stressRulesDetailPanel");
    return Boolean(document.getElementById("rulesSheet")?.open) && panel && !panel.hidden &&
      (document.getElementById("stressRulesGrid")?.children.length || 0) >= 12;
  }, { timeout: 5000 });
  const grid = await page.evaluate(() => {
    const cards = [...(document.getElementById("stressRulesGrid")?.children || [])];
    return {
      cards: cards.length,
      tally: document.getElementById("rulesSheetTally")?.textContent?.trim() || "",
      leaders: cards.filter((c) => c.querySelector(".rules-row__leader")).length,
      unknown_as_pass: cards.some((c) => /unknown/i.test(c.textContent || "") && c.classList.contains("ok")),
    };
  });
  if (grid.unknown_as_pass) {
    throw new Error("a rules row renders unknown status with a pass tone — unknown must never read as pass");
  }
  if (grid.leaders !== grid.cards) {
    throw new Error(`every rules row should render as a dot-leader checklist line: ${JSON.stringify(grid)}`);
  }
  if (!grid.tally || grid.tally === "--") {
    throw new Error(`rules sheet should open on the served breach tally: ${JSON.stringify(grid)}`);
  }
  await page.locator("#stressRulesToggle").click();
  await page.waitForFunction(() => document.getElementById("stressRulesDetailPanel")?.hidden, { timeout: 5000 });
  await page.locator("#rulesSheetClose").click();
  await page.waitForFunction(() => !document.getElementById("rulesSheet")?.open, { timeout: 5000 });
  return { exercised: true, counts, cards: grid.cards, tally: grid.tally };
}


// Panel Dark tap-through: the Monitor is glance-only, so depth must live in
// sheets opened from the instruments — and Opportunities must be
async function exerciseSheetLayer(page) {
  const before = await page.evaluate(() => ({
    protectionSheetOpen: Boolean(document.getElementById("protectionSheet")?.open),
    underlyingsSheetOpen: Boolean(document.getElementById("underlyingsSheet")?.open),
    // A hidden ancestor means no boxes at all, which is exactly how a closed
    protectionOnMonitor: Boolean(document.getElementById("protectionPanel")?.offsetParent),
    underlyingsOnMonitor: Boolean(document.getElementById("underlyingPanel")?.offsetParent),
    opportunitiesHidden: Boolean(document.getElementById("opportunitiesPanel")?.hidden),
    opportunitiesCount: document.getElementById("opportunitiesCount")?.textContent?.trim() || "",
  }));
  if (before.protectionSheetOpen || before.underlyingsSheetOpen) {
    throw new Error(`sheets should start closed: ${JSON.stringify(before)}`);
  }
  if (before.protectionOnMonitor || before.underlyingsOnMonitor) {
    throw new Error(`Protection and Underlyings depth should live in sheets, not on the Monitor face: ${JSON.stringify(before)}`);
  }
  const opportunityCount = Number.parseInt(before.opportunitiesCount, 10) || 0;
  if (opportunityCount === 0 && !before.opportunitiesHidden) {
    throw new Error(`Opportunities is exception-shaped: nothing may render at count 0: ${JSON.stringify(before)}`);
  }
  await page.locator("#protectionTile").click();
  await page.waitForFunction(() => {
    const sheet = document.getElementById("protectionSheet");
    return Boolean(sheet?.open) && Boolean(document.getElementById("protectionPanel")?.offsetParent);
  }, { timeout: 5000 });
  const protectionSheet = await page.evaluate(() => ({
    title: document.getElementById("protectionSheetTitle")?.textContent?.trim() || "",
    // The trim control keeps its own hidden gate (it needs reduce-eligible
    // that it is offered on this account.
    deriskSeated: Boolean(document.querySelector("#protectionSheet #protectionDerisk")),
    previewSeated: Boolean(document.querySelector("#protectionSheet #protectionDeriskPreview")),
    rowsSeated: Boolean(document.querySelector("#protectionSheet #protectionRows")),
    opportunitiesSeated: Boolean(document.querySelector("#protectionSheet #opportunitiesPanel")),
    submitButtons: document.querySelectorAll("#protectionSheet #protectionDeriskSubmit").length,
  }));
  for (const key of ["deriskSeated", "previewSeated", "rowsSeated", "opportunitiesSeated"]) {
    if (!protectionSheet[key]) {
      throw new Error(`Protection sheet is missing a reseated surface (${key}): ${JSON.stringify(protectionSheet)}`);
    }
  }
  if (protectionSheet.title !== "Protection") {
    throw new Error(`Protection sheet should carry its engraved placard name: ${JSON.stringify(protectionSheet)}`);
  }
  // Reseating must not have loosened the two-gesture trim: Submit is minted
  // by a preview that surfaced eligible legs, never by opening the sheet.
  if (protectionSheet.submitButtons !== 0) {
    throw new Error(`the trim basket must not offer Submit before a preview: ${JSON.stringify(protectionSheet)}`);
  }
  await page.locator("#protectionSheetClose").click();
  await page.waitForFunction(() => !document.getElementById("protectionSheet")?.open, { timeout: 5000 });

  const moversVisible = await page.locator("#moversRow").evaluate((el) => !el.hidden).catch(() => false);
  let underlyingsSheet = { exercised: false, reason: "no movers row on this book" };
  if (moversVisible) {
    await page.locator("#moversRow").click();
    await page.waitForFunction(() => {
      const sheet = document.getElementById("underlyingsSheet");
      return Boolean(sheet?.open) && Boolean(document.getElementById("underlyingPanel")?.offsetParent);
    }, { timeout: 5000 });
		underlyingsSheet = await page.evaluate(() => ({
			exercised: true,
			title: document.getElementById("underlyingsSheetTitle")?.textContent?.trim() || "",
			bookExpanded: !document.getElementById("underlyingBookListPanel")?.hidden,
			writeActions: document.querySelectorAll("#underlyingsSheet .underlying-action").length,
		}));
		if (underlyingsSheet.writeActions !== 0) {
			throw new Error(`Underlyings sheet must not retain retired write actions: ${JSON.stringify(underlyingsSheet)}`);
    }
    if (!underlyingsSheet.bookExpanded || underlyingsSheet.title !== "Underlyings") {
      throw new Error(`Underlyings sheet should open on the expanded book: ${JSON.stringify(underlyingsSheet)}`);
    }
    await page.locator("#underlyingsSheetClose").click();
    await page.waitForFunction(() => !document.getElementById("underlyingsSheet")?.open, { timeout: 5000 });
  }
  return { monitor: before, protection_sheet: protectionSheet, underlyings_sheet: underlyingsSheet };
}

async function exerciseMarketContext(page) {
  let before = await readMarketContext(page);
  if (!before.regime || before.regime === "--") {
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
// reports served source health, and the master law — a lit red window can
// never sit under a master that neither lamps nor discloses it.
async function assertPanelDarkInstruments(page) {
  // Declared inside the function: the smoke invokes itself through a
  const REGIME_WINDOW_LEGENDS = ["Breadth", "Volatility", "Credit", "Dealer gamma", "Funding", "FX"];
  const instruments = await page.evaluate(() => {
    const litClass = (el) => [...(el?.classList || [])].find((name) => name.startsWith("pd-tile--")) || "";
    const readTile = (el) => (el ? {
      lit: litClass(el),
      legend: el.querySelector(".pd-tile__legend")?.textContent?.trim() || "",
      cap: el.querySelector(".pd-tile__cap")?.textContent?.trim() || "",
      fig: el.querySelector(".pd-tile__fig")?.textContent?.trim() || "",
      trip: el.querySelector(".pd-tile__trip")?.textContent?.trim() || "",
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
  // Trip anchors are SERVED or absent. A window may legitimately carry none
  // anchor must never be an empty stub the reader has to interpret.
  const blankTrip = instruments.clusters.find((cluster) => cluster.trip === "--");
  if (blankTrip) {
    throw new Error(`regime window renders a placeholder trip anchor instead of omitting it: ${JSON.stringify(blankTrip)}`);
  }
	if (instruments.deltaReadout || !instruments.deltaHasLampBar) {
		throw new Error(`Net $ Delta must use the standard tile face and lamp slot: ${JSON.stringify(instruments)}`);
  }
  if (!/\d+\/\d+ sources ok/i.test(instruments.lampTest)) {
    throw new Error(`lamp-test stamp should report served source health: ${JSON.stringify(instruments.lampTest)}`);
  }
  if (!instruments.master.sub) {
    throw new Error("master annunciator should carry an action subline");
  }
  // Master law, first half: an act-lit window under an unlit master must
  const redWindows = instruments.clusters.filter((cluster) => cluster.lit === "pd-tile--act").length;
  if (redWindows > 0 && !instruments.master.lit && !/severity held|\b\d+ red:/i.test(instruments.master.sub)) {
    throw new Error(`master and panel disagree: ${redWindows} red window(s) under an unlit, undisclosed master: ${JSON.stringify(instruments.master)}`);
  }
  // Second half, after every daemon cluster got a window of its own: a lit red
  const namedReds = /\b\d+ red: ([^·]+)/i.exec(instruments.master.sub);
  if (namedReds) {
    const legendsLower = legends.map((legend) => legend.toLowerCase());
    const onPanel = namedReds[1].split(",")
      .map((name) => name.trim().toLowerCase())
      .filter((name) => name && legendsLower.some((legend) => legend.includes(name)));
    if (onPanel.length > 0) {
      throw new Error(`master subline names reds the panel already tiles (${onPanel.join(", ")}): ${JSON.stringify(instruments.master.sub)}`);
    }
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
        counts: { total: 3, actionable: 3, trailing_stop: 2, option_loss_exit: 1 },
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
        }, {
          key: "smoke-option-loss",
          revision: "smoke",
          bucket: "option_loss_exit",
          state: "generated",
          symbol: "SYNLOSS",
          sec_type: "OPT",
          action: "SELL",
          quantity: 2,
          max_quantity: 2,
          position_quantity: 2,
          position_effect: "close",
          order_type: "LMT",
          tif: "DAY",
          contract: { con_id: 9001, symbol: "SYNLOSS", sec_type: "OPT", currency: "USD", expiry: "20260918", strike: 100, right: "C" },
          option_exit: { kind: "loss_exit", intent: "directional", economic_role: "directional", dte: 31, return_pct: -62, loss_exit_pct: 60 },
        }, {
          key: "smoke-option-profit",
          revision: "smoke",
          bucket: "trailing_stop",
          state: "generated",
          symbol: "SYNPROFIT",
          sec_type: "OPT",
          action: "SELL",
          quantity: 1,
          max_quantity: 1,
          position_quantity: 1,
          position_effect: "close",
          order_type: "TRAIL LIMIT",
          tif: "DAY",
          contract: { con_id: 9002, symbol: "SYNPROFIT", sec_type: "OPT", currency: "USD", expiry: "20260918", strike: 100, right: "C" },
          option_exit: { kind: "profit_trail", intent: "directional", economic_role: "directional", dte: 31, return_pct: 55, profit_arm_gain_pct: 50, locked_gain_pct: 5, initial_locked_gain_pct: 7 },
		  trail: { offset_type: "percent", trailing_percent: 30, initial_stop_price: 1.1, limit_offset: 0.05 },
          trail_sizing: { method: "option-profit-lock-v1", chosen_pct: 30, selected_by: "policy_default", policy_min_pct: 20, policy_max_pct: 50 },
          execution_semantics: { reference_side: "bid", trigger_method_label: "broker default", trigger_effect: "limit_order_when_triggered", price_guarantee: "stop_limit_can_leave_position_unfilled" },
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
    optionRows: [...document.querySelectorAll(".protection-row")]
      .map((row) => row.textContent?.replace(/\s+/g, " ").trim() || "")
      .filter((text) => text.includes("Option loss exit") || text.includes("Option profit trail")),
	optionRiskTickets: [...document.querySelectorAll(".protection-row")]
		.filter((row) => /Option (loss exit|profit trail)/.test(row.textContent || ""))
		.filter((row) => row.querySelector(".protection-row__risk-ticket, .protection-row__ladder")).length,
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
  if (info.optionRows.length !== 2 || !info.optionRows.some((text) => text.includes("Option loss exit") && text.includes("DAY limit close") && text.includes("Preview exit")) ||
		!info.optionRows.some((text) => text.includes("Option profit trail") && text.includes("armed at +50.0%") && text.includes("native 30.0% premium trail") && text.includes("Preview trail"))) {
    throw new Error(`Option exit rows did not render approved semantics: ${JSON.stringify(info.optionRows)}`);
  }
	if (info.optionRiskTickets !== 0) {
		throw new Error(`Option exit rows must not inherit stock-stop loss ladders: ${JSON.stringify(info)}`);
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

// Alerts is the current annunciator log. Terminal delivery evidence stays
// private and must not reappear as a user-facing history register.
async function exerciseAlerts(page) {
  const SEVERITY_RANK = { act: 0, watch: 1, "": 2 };
  await page.locator("#tabAlerts").click();
  await page.waitForFunction(() => document.getElementById("alertsTab")?.hidden === false, { timeout: 5000 });
  await page.waitForTimeout(2200);
  try {
    await page.waitForFunction(() => (
      (globalThis.__canarySmoke?.attentionReadDiverted || 0) > 0
        || globalThis.__canarySmoke?.alertCursors?.at(-1)?.unread_count === 0
    ), { timeout: 10000 });
  } catch (error) {
    const attentionState = await page.evaluate(() => ({
      tab_visible: document.getElementById("alertsTab")?.hidden === false,
      document_visibility: document.visibilityState,
      aria: document.getElementById("tabAlerts")?.getAttribute("aria-label") || "",
      rendered_rows: document.querySelectorAll("#currentSignalList .alert-row").length,
      status: document.getElementById("attentionStatus")?.textContent?.trim() || "",
      fetches: (globalThis.__canarySmoke?.fetches || [])
        .filter((item) => String(item.url || "").includes("/api/alerts"))
        .map((item) => ({ path: new URL(item.url, location.origin).pathname, status: item.status, diverted: item.diverted === true })),
      cursors: globalThis.__canarySmoke?.alertCursors || [],
    }));
    throw new Error(`attention acknowledgement did not settle: ${JSON.stringify(attentionState)}; ${error.message}`);
  }
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
      currentCount: Number.parseInt(document.getElementById("currentSignalCount")?.textContent || "0", 10) || 0,
      authority: document.getElementById("alertAuthorityState")?.textContent || "",
      coverage: document.getElementById("alertCoverageSummary")?.textContent || "",
      active: readLog("currentSignalList"),
      poster: document.querySelector("#currentSignalList .pd-poster__word")?.textContent?.trim() || "",
      quiet: document.querySelector("#currentSignalList .empty-row")?.textContent?.trim() || "",
      // A placard row carries its count or chip alongside the legend; the
      placards: [...document.querySelectorAll("#alertsTab .pd-placard")].map((el) => (el.querySelector("span") || el).textContent?.trim() || ""),
      terminalSectionPresent: document.getElementById("alertsHistorySection") !== null,
      activeLegendHidden: document.getElementById("currentSignalPlacard")?.hidden !== false,
    };
  });
  if (!info.authority || !info.coverage) throw new Error(`active alert authority did not render: ${JSON.stringify(info)}`);
  for (const row of info.active) {
    if (!row.placard || !row.title || !/^Lit /.test(row.age)) {
      throw new Error(`annunciator tile is incomplete: ${JSON.stringify(row)}`);
    }
  }
  const ranks = info.active.map((row) => SEVERITY_RANK[row.tint] ?? 9);
  if (ranks.some((rank, index) => index > 0 && rank < ranks[index - 1])) {
    throw new Error(`act tiles must sit above watch tiles: ${JSON.stringify(info.active.map((row) => row.tint))}`);
  }
  // A quiet log is either the engraved poster or the honest coverage
  // sentence — never an empty panel that could be mistaken for calm.
  if (info.active.length === 0 && info.poster !== "ALL DARK." && !/coverage is incomplete or stale/.test(info.quiet)) {
    throw new Error(`a quiet annunciator log must state why it is quiet: ${JSON.stringify({ poster: info.poster, quiet: info.quiet })}`);
  }
  // Process evidence lives on the Settings back panel since WP5; the log
  // carries only its own placards, and the old one reappearing here would
  if (!info.placards.includes("Open") || info.placards.includes("Process evidence")) {
    throw new Error(`alerts placards are incomplete or carry relocated sections: ${JSON.stringify(info.placards)}`);
  }
  // The ALL DARK poster is the count; every other state keeps the legend.
  if (info.activeLegendHidden !== (info.poster === "ALL DARK.")) {
    throw new Error(`the Active legend must yield to the poster and only to the poster: ${JSON.stringify({ poster: info.poster, activeLegendHidden: info.activeLegendHidden })}`);
  }
  if (info.terminalSectionPresent) throw new Error("terminal alert history must not be rendered in v3");
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
  }
  return {
    initially_open: initiallyOpen,
    opens: true,
    count: info.count,
    current_rows: info.currentRows,
    current_count: info.currentCount,
    authority: info.authority,
    coverage: info.coverage,
    placards: info.placards,
    poster: info.poster,
    active_legend_hidden: info.activeLegendHidden,
    active_tints: info.active.map((row) => row.tint),
    first_age: info.active[0]?.age || info.extinguished[0]?.age || "",
    terminal_history_present: info.terminalSectionPresent,
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
// Panel Dark register must hold: movement placards, typed runs rendered as
// Review movement exactly when the served row says it is signable.
async function assertBriefNarrative(page) {
  // Declared inside the function: the smoke invokes itself through a
  const MOVEMENT_PLACARDS = ["Review \u00b7 since last close", "Ready \u00b7 next open"];
  const SEVERITY_WORDS = ["observe", "watch", "act", "ok", "attention", "degraded", "unavailable"];
  const MARKUP_LEAKS = ["[f]", "[/f]", "[w]", "[/w]", "[a]", "[/a]", "<span", "<b>"];
  const FIXTURE_REPORT = "smoke-signoff-fixture";

  await page.locator("#tabBrief").click();
  await page.waitForFunction(() => document.getElementById("briefTab")?.hidden === false, { timeout: 5000 });
  await page.waitForSelector("#briefPanel:not([hidden])", { timeout: 5000 });
  // A brief source that is down renders its own empty state; report that as
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
    // A freshly restarted app host can fail the first fetch at the network
    // layer (WebKit reports a bare "Load failed"); one transient miss must
    let body = null;
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        const res = await fetch("/api/bootstrap", { credentials: "include" });
        body = await res.json();
        break;
      } catch (err) {
        if (attempt === 2) throw err;
        await new Promise((resolve) => setTimeout(resolve, 1500));
      }
    }
    const brief = body?.snapshot?.brief || {};
    return {
      narrative: Boolean(brief.narrative),
      review: brief.review || {},
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

  await page.locator("#tabMonitor").click();
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
  return {
    mode: rendered.mode,
    placards: rendered.placards.length,
    paragraphs: rendered.paragraphs.length,
    roles: rendered.roles,
    chip: rendered.chip,
  };
}


// Orders lives on its own bottom-nav tab (Monitor, Brief, Alerts, Orders,
async function exerciseOpenOrders(page) {
  await page.locator("#tabOrders").click();
  await page.waitForFunction(() => document.getElementById("ordersTab")?.hidden === false, { timeout: 5000 });
  await page.waitForFunction(() => document.getElementById("ordersAsOf")?.textContent?.trim().startsWith("checked "), { timeout: 5000 });
  const info = await page.evaluate(() => {
    const buttons = [...document.querySelectorAll("#ordersOpenList button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    }));
    return {
      panelPresent: !!document.getElementById("ordersPanel"),
      checkedCopy: document.getElementById("ordersAsOf")?.textContent?.trim() || "",
      countText: document.getElementById("ordersOpenCount")?.textContent?.trim() || "",
      rows: document.querySelectorAll("#ordersOpenList .open-order-row").length,
      empty: document.getElementById("ordersOpenList")?.textContent?.includes("None working.") || false,
      // Panel Dark order bars: every row is a machined tile carrying an
      bars: document.querySelectorAll("#ordersOpenList .open-order-row.pd-tile.pd-order").length,
      legends: [...document.querySelectorAll("#ordersOpenList .open-order-row .pd-tile__legend")].map((el) => el.textContent?.trim() || ""),
      readings: [...document.querySelectorAll("#ordersOpenList .open-order-row .pd-tile__reading")].map((el) => el.textContent?.trim() || ""),
      foot: document.querySelector("#ordersPanel .orders-foot")?.textContent?.trim() || "",
      buttons,
      oldLabels: buttons.map((button) => button.text).filter((label) => ["Modify", "Cancel", "Execute"].includes(label)),
    };
  });
  if (!info.panelPresent) {
    throw new Error("Orders panel should always be present once the Orders tab is active");
  }
  if (!/^checked (now|\d+[mh](?: \d+m)? ago)$/.test(info.checkedCopy)) {
    throw new Error(`open-order timestamp should describe when the broker view was checked: ${JSON.stringify(info.checkedCopy)}`);
  }
  if (!info.foot.includes("Order journal") || !info.foot.includes("current broker") || !info.foot.includes("local order state")) {
    throw new Error(`orders journal must state its broker and local-state authority at the foot: ${JSON.stringify(info.foot)}`);
  }
  if (info.bars !== info.rows || info.legends.length !== info.rows || info.legends.some((legend) => !legend) || info.readings.length !== info.rows || info.readings.some((reading) => !reading)) {
    throw new Error(`open orders must render as engraved order bars: ${JSON.stringify({ rows: info.rows, bars: info.bars, legends: info.legends, readings: info.readings })}`);
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
    checked_copy: info.checkedCopy,
    legends: info.legends,
    readings: info.readings,
    foot: info.foot,
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
    // The stamped type plate at the foot of the back panel.
    "#settingsPlateAccount",
    "#settingsPlateMode",
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
  if (JSON.stringify(notification.modes) !== JSON.stringify(["Off", "Action required", "Watch + action"]) || !notification.copy.includes("global for this app host and all paired devices") || !notification.copy.includes("Off stops phone notifications while current alerts remain visible") || !notification.copy.includes("Action required sends urgent items only") || !notification.copy.includes("Watch + action also sends review reminders") || !notification.copy.includes("not configured here") || !notification.copy.includes("shared across paired devices")) {
    throw new Error(`Settings notification card is incomplete: ${JSON.stringify(notification)}`);
  }
  // The back panel: engraved banks, slide switches that print their state,
  // and the stamped type plate. Presentation only — no control is exercised.
  const backPanel = await page.evaluate(() => ({
    banks: [...document.querySelectorAll("#settingsTab .pd-bank")].length,
    placards: [...document.querySelectorAll("#settingsTab > .settings-panel .pd-placard")].map((el) => el.textContent?.trim() || ""),
    switches: [...document.querySelectorAll("#settingsTab .pd-sw input")].length,
    statusCells: [...document.querySelectorAll("#settingsTab .pd-grid--status .pd-tile--cell .pd-tile__legend")].map((el) => el.textContent?.trim() || ""),
    plate: document.querySelector("#settingsTab .pd-plate")?.textContent?.trim() || "",
    plateMode: document.getElementById("settingsPlateMode")?.textContent?.trim() || "",
  }));
  for (const placard of ["Notifications", "Workflows", "Status"]) {
    if (!backPanel.placards.includes(placard)) {
      throw new Error(`Settings back panel is missing the ${JSON.stringify(placard)} bank: ${JSON.stringify(backPanel.placards)}`);
    }
  }
  if (backPanel.switches !== 1 || JSON.stringify(backPanel.statusCells) !== JSON.stringify(["Trading", "Limits", "Market data", "Build", "Protection", "Policy"])) {
    throw new Error(`Settings back panel banks are incomplete: ${JSON.stringify(backPanel)}`);
  }
  if (!backPanel.plate.startsWith("CANARY") || !backPanel.plate.endsWith("MADE FOR ONE DESK") || !backPanel.plateMode) {
    throw new Error(`Settings type plate is incomplete: ${JSON.stringify(backPanel.plate)}`);
  }
  const settingWritesAfter = await page.evaluate(() => globalThis.__canarySmoke.fetches.filter((item) => item.url.endsWith("/api/alerts/settings")).length);
  if (settingWritesAfter !== settingWritesBefore) throw new Error("rendered Settings smoke changed the alert delivery setting");
  await page.locator("#tabMonitor").click();
  await page.waitForFunction(() => document.getElementById("dashboard")?.hidden === false, { timeout: 5000 });
  return {
    elements: elements.map((element) => element.selector),
    notification,
    back_panel: backPanel,
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

function trimRight(value, suffix) {
  while (value.endsWith(suffix)) {
    value = value.slice(0, -suffix.length);
  }
  return value;
}
