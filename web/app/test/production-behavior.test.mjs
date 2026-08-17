import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import test from "node:test";

import { FakeElement, createDOMHarness } from "./dom-harness.mjs";

const storage = new Map();
const dom = createDOMHarness();
globalThis.document = dom.document;
globalThis.localStorage = {
  getItem: (key) => storage.get(String(key)) ?? null,
  removeItem: (key) => storage.delete(String(key)),
  setItem: (key, value) => storage.set(String(key), String(value)),
};
globalThis.window = globalThis;
globalThis.Node = FakeElement;
globalThis.MutationObserver = undefined;
globalThis.requestAnimationFrame = (callback) => callback();
Object.defineProperty(globalThis, "navigator", {
  configurable: true,
  value: { serviceWorker: undefined, userAgent: "Synthetic browser" },
});
Object.defineProperty(globalThis, "Notification", { configurable: true, value: undefined });
Object.defineProperty(globalThis, "EventSource", { configurable: true, value: undefined });

const { normalizedPositionsSort, normalizedTab, state } = await import("../state.js");
const { installRenderAll } = await import("../render-runtime.js");
const moduleNames = [
  "alerts", "alert-inbox", "brief", "chrome", "lifecycle", "market-events", "opportunities", "orders",
  "portfolio", "protection", "protection-coverage", "settings", "shared", "shell", "strategies", "stress", "underlyings",
  "update",
];
const modules = Object.fromEntries(await Promise.all(moduleNames.map(async (name) => [name, await import(`../${name}.js`)])));
const { alerts, brief, chrome, lifecycle, opportunities, orders, portfolio, protection, settings, shared, shell, strategies, stress, underlyings, update } = modules;
const alertInbox = modules["alert-inbox"];
const coverage = modules["protection-coverage"];
const marketEvents = modules["market-events"];

let renderCount = 0;
installRenderAll(() => { renderCount += 1; });

function response(body, status = 200) {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => (typeof body === "string" ? JSON.parse(body) : body),
    text: async () => text,
  };
}

function reset() {
  storage.clear();
  dom.elements.clear();
  dom.document.body.dataset = {};
  renderCount = 0;
  if (state.alertsRefreshTimer) clearTimeout(state.alertsRefreshTimer);
  if (state.attentionRetryTimer) clearTimeout(state.attentionRetryTimer);
  if (state.updatePollTimer) clearTimeout(state.updatePollTimer);
  if (state.updateCompleteTimer) clearTimeout(state.updateCompleteTimer);
  Object.assign(state, {
    snapshot: null, settings: null, authenticated: true, activeTab: "monitor", accountValueVisible: false,
    pairingRequired: false, connectionOK: false, connectionText: "Connecting", eventSource: null,
    readOnlyPreview: false, updateStatus: null, updatePollTimer: null, updateCompleteTimer: null,
    portfolioDetailOpen: false, protectionOpen: false, protectionQtyOverrides: {}, protectionQuoteTicks: {},
    selectedUnderlying: "", positionsSort: "impact",
    protectionSnapshotBusy: false, protectionSnapshotLastAt: 0, protectionSnapshotNotice: "",
    protectionDerisk: { percent: 25, busy: "", result: null, submitted: null, requestRef: "", previewedAt: 0, abort: null },
    protectionStopRequestBusy: "", protectionStopRequests: {},
    opportunitiesOpen: false, opportunityPreviewBusy: "", opportunityPreviews: {}, opportunitySubmitBusy: "", opportunitySubmits: {},
    strategyDrafts: {}, strategyPreviewBusy: "", strategyPreviews: {}, strategySubmitBusy: "", strategySubmits: {},
    opportunitySnapshotBusy: false, opportunitySnapshotLastAt: 0, opportunitySnapshotNotice: "",
    governance: null, governanceRefreshSucceeded: null, reconciliationCheck: { busy: false, state: "", error: false },
    safeNotificationTest: { busy: false, state: "", error: false }, pushInspection: { state: "unsupported", busy: false },
    alertSettings: { mode: "watch_and_act" }, alertsRefreshInFlight: null, alertsRefreshTimer: null,
    alertEvidenceTarget: null, attentionEpoch: 0, attentionReadInFlight: null, attentionRetryTimer: null,
    attentionStatus: { state: "", error: false },
    alertsRefreshDueAt: 0, alertsLastRefreshAt: 0,
  });
  globalThis.fetch = async () => response({});
}

function descendants(node) {
  if (!node || typeof node !== "object") return [];
  return [node, ...(node.children || []).flatMap(descendants)];
}

const byClass = (node, className) => descendants(node).filter((item) => item.classList?.contains(className));
const accountScope = (accountId = "SYNTHETIC-AUTHORITY") => ({ account_id: accountId, account_mode: "paper" });
const sourceAuthority = (overrides = {}) => ({
  scope: accountScope(), source: "portfolio_stream", availability: "available", freshness: "current", ...overrides,
});

function assertSameNames(label, actual, expected) {
  const [left, right] = [actual, expected].map((values) => [...new Set(values)].sort());
  if (JSON.stringify(left) !== JSON.stringify(right)) throw new Error(`${label} mismatch`);
}

const embeddedJavaScriptNames = (source) => [...String(source).matchAll(/^\/\/go:embed\s+(.+)$/gm)]
  .flatMap((match) => match[1].trim().split(/\s+/)).filter((name) => name.endsWith(".js"));
const staticJavaScriptImports = (source) => [...String(source).matchAll(/^\s*import\s+(?:[^"']*?\s+from\s+)?["']\.\/([^"']+\.js)["']\s*;?\s*$/gm)]
  .map((match) => match[1]);

function validateEmbeddedAppAssetGraph({ diskNames, embeddedNames, sources }) {
  assertSameNames("embedded JavaScript set", embeddedNames, diskNames);
  const disk = new Set(diskNames);
  const reachable = new Set();
  const visit = (name) => {
    if (reachable.has(name)) return;
    if (!disk.has(name) || !sources.has(name)) throw new Error(`static import graph references missing module ${name}`);
    reachable.add(name);
    for (const imported of staticJavaScriptImports(sources.get(name))) visit(imported);
  };
  visit("app.js");
  assertSameNames("app.js static import graph", reachable, diskNames.filter((name) => name !== "service-worker.js"));
}

async function waitFor(check, message) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    if (check()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.fail(message);
}

test("embedded app asset graph pins disk, go:embed, and static imports with negative fixtures", async () => {
  const appRoot = new URL("../", import.meta.url);
  const diskNames = (await readdir(appRoot, { withFileTypes: true }))
    .filter((entry) => entry.isFile() && entry.name.endsWith(".js"))
    .map((entry) => entry.name)
    .sort();
  const goSource = await readFile(new URL("../assets.go", import.meta.url), "utf8");
  const embeddedNames = embeddedJavaScriptNames(goSource);
  const sources = new Map(await Promise.all(diskNames.map(async (name) => [name, await readFile(new URL(`../${name}`, import.meta.url), "utf8")])));

  validateEmbeddedAppAssetGraph({ diskNames, embeddedNames, sources });
  assert.throws(() => validateEmbeddedAppAssetGraph({ diskNames, embeddedNames: embeddedNames.filter((name) => name !== "settings.js"), sources }), /embedded JavaScript set mismatch/);
  const unreachable = new Map(sources);
  unreachable.set("app.js", sources.get("app.js").replace(/^import .*"\.\/settings\.js";\n/m, ""));
  assert.throws(() => validateEmbeddedAppAssetGraph({ diskNames, embeddedNames, sources: unreachable }), /app\.js static import graph mismatch/);
});

test("header market selector keeps the compact US options label and fixed narrow control", async () => {
	const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
	const css = await readFile(new URL("../styles.css", import.meta.url), "utf8");
	assert.match(html, /<option value="us-options">US opt\.<\/option>/);
	assert.match(css, /\.market-strip__selector select\s*\{[^}]*width:\s*50px;[^}]*max-width:\s*50px;/s);
});

test("primary navigation seats Positions and folds the Daily brief into Monitor", async () => {
  const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
  assert.equal(normalizedTab("positions"), "positions");
  assert.equal(normalizedTab("brief"), "monitor");
  assert.match(html, /id="tabPositions"[^>]*data-tab="positions"/);
  assert.match(html, /id="dashboard"[^>]*data-tab-panel="monitor"[\s\S]*id="briefPanel"/);
  assert.doesNotMatch(html, /id="tabBrief"|id="briefTab"|id="underlyingsSheet"/);
  assert.doesNotMatch(html, /id="attentionStatus"/);
});

test("Positions is performance-first while typed risk and guarded actions stay behind selection", async () => {
  reset();
  const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
  assert.match(html, /Watch what moved and how each position performed/);
  assert.match(html, />Portfolio trim</);
  assert.match(html, />Option groups</);
  assert.doesNotMatch(html, /Stock &amp; ETF risk|underlyingDetailToggle/);
  assert.equal(normalizedPositionsSort("exposure"), "exposure");
  assert.equal(normalizedPositionsSort("invented"), "impact");

  state.accountValueVisible = true;
  state.selectedUnderlying = "SYN";
  state.snapshot = {
    positions: {
      strategies: [{ id: "strategy-synthetic", underlying: "SYN", kind: "vertical", actionable: true }],
      strategy_issues: [],
      by_underlying: [],
      portfolio: { base_currency: "EUR" },
    },
    market_quotes: {}, market_events: {},
  };
  const group = {
    underlying: "SYN",
    stock: { symbol: "SYN", currency: "USD", mark: 100, quote_expectation: "none" },
    options: [{ symbol: "SYN", currency: "USD", daily_pnl_base: 5 }],
    group_daily_pnl_base: 12,
    group_unrealized_pnl_base: 45,
    group_market_value_base: 1200,
    group_market_value_pct_nlv: 6.5,
    group_effective_delta: 8.25,
    group_dollar_delta_base: 825,
  };
  state.snapshot.positions.by_underlying = [group];
  const row = underlyings.heldUnderlyingRows(state.snapshot.positions, "EUR")[0];
  assert.deepEqual({ daily: row.dailyPnl, open: row.openPnl, value: row.marketValue, delta: row.dollarDelta }, {
    daily: 12, open: 45, value: 1200, delta: 825,
  });
  const rendered = underlyings.underlyingBookRow(row, "EUR");
  assert.equal(byClass(rendered, "underlying-row__metric--pnl")[0].textContent.includes("Today"), true);
  assert.equal(byClass(rendered, "underlying-row__metric--open")[0].textContent.includes("Open"), true);
  assert.match(byClass(rendered, "position-inspector__distinction")[0].textContent, /Color shows direction, not an instruction/);
  assert.deepEqual(byClass(rendered, "position-inspector__action").map((button) => button.dataset.positionAction), ["strategy", "trim"]);
  assert.match(byClass(rendered, "position-inspector__scope")[0].textContent, /whole-book delta tool/);

  const rows = [
    { symbol: "A", dailyPnl: 10, marketValueBase: 100 },
    { symbol: "B", dailyPnl: -30, marketValueBase: 50 },
    { symbol: "C", dailyPnl: null, marketValueBase: 500 },
  ];
  assert.deepEqual([...rows].sort((a, b) => underlyings.compareUnderlyingRows(a, b, "impact")).map((item) => item.symbol), ["B", "A", "C"]);
  assert.deepEqual([...rows].sort((a, b) => underlyings.compareUnderlyingRows(a, b, "winners")).map((item) => item.symbol), ["A", "B", "C"]);
  assert.deepEqual([...rows].sort((a, b) => underlyings.compareUnderlyingRows(a, b, "exposure")).map((item) => item.symbol), ["C", "A", "B"]);
});

test("TestAppJSTradingStateUsesSnapshotCanWrite replacement exercises typed write and freeze-aware cancel gates", () => {
  reset();
  assert.equal(settings.tradingStatusSettingsLabel({}, { can_write: true }), "Write ready");
  assert.equal(settings.tradingStatusSettingsLabel({}, { can_preview: true }), "Preview ready");
  const order = { order_ref: "synthetic-order", open: true, order_type: "LMT", remaining: 2 };
  assert.equal(orders.orderModifyGate(order, { can_write: false }).ready, false);
  assert.equal(orders.orderModifyGate(order, { can_write: true }).ready, true);
	const frozen = { can_write: false, mode: "paper", account: "synthetic", write_blockers: [{ code: "trading_frozen" }] };
  assert.equal(orders.tradingCancelAllowed(frozen), true);
  assert.equal(orders.orderCancelGate(order, frozen).ready, true);
  assert.equal(orders.tradingCancelAllowed({ ...frozen, write_blockers: [{ code: "policy_blocked" }] }), false);
  assert.equal(orders.orderCancelGate(order, { ...frozen, account: "" }).ready, false);
});

test("strategy operations keep midpoint, credit, and debit terms explicit", () => {
  reset();
  assert.equal(strategies.strategyLimit({ priceMode: "midpoint", amount: "" }), undefined);
  assert.equal(strategies.strategyLimit({ priceMode: "credit", amount: "1.25" }), 1.25);
  assert.equal(strategies.strategyLimit({ priceMode: "debit", amount: "1.25" }), -1.25);
  assert.equal(strategies.strategyPriceTerms({ draft: { limit_price: 1.25 }, notional_currency: "USD" }), "Receive at least $1.25 per strategy");
  assert.equal(strategies.strategyPriceTerms({ draft: { limit_price: -0.8 }, notional_currency: "USD" }), "Pay up to $0.80 per strategy");
  assert.equal(strategies.strategyKind("vertical"), "Vertical spread");
  assert.match(strategies.formatExpiry("20260821"), /2026/);
});

test("TestAppJSSnapshotBannerClaimsLastGoodOnlyWhenPresent replacement keeps cold data distinct from retained data", () => {
  reset();
  const errors = [
    { source: "account", message: "unavailable" },
    { source: "positions", message: "unavailable" },
  ];
  assert.equal(shell.snapshotIssueSummary(errors, {}).text, "Account and positions unavailable.");
  assert.equal(shell.snapshotPayloadPresent({ market_calendar: { session: {} } }, "calendar"), true);
  assert.equal(shell.snapshotPayloadPresent({ market_calendar: {} }, "calendar"), false);
  assert.match(shell.snapshotIssueSummary(errors, { account: { as_of: "now" }, positions: { as_of: "now" } }).text, /showing last good snapshot/);
  const gatewayAndCold = shell.snapshotIssueSummary(errors, { status: { last_error: "Client id 7 already in use" } });
  assert.equal(gatewayAndCold.text, "Account and positions unavailable.");
});

test("sync strip combines app transport health with typed account-data authority", () => {
  reset();
  state.connectionOK = true;
  const now = new Date().toISOString();
  const current = { availability: "available", freshness: "current" };
  const snap = {
    updated_at: now,
    account: { authority: { ...current, source: "account_summary_request" } },
    positions: { authority: { ...current, source: "portfolio_stream" } },
    sources: { account: { state: "current" }, positions: { state: "current" } },
  };

  shell.renderSyncStrip(snap);
  assert.equal(dom.element("syncStatusLabel").textContent, "Snapshot");
  assert.equal(dom.element("syncStatusState").textContent, "Stream ok");
  assert.equal(dom.element("syncStrip").classList.contains("sync-strip--degraded"), false);

  snap.positions.authority = { availability: "unavailable", freshness: "unknown", reason: "unprimed" };
  shell.renderSyncStrip(snap);
  assert.equal(dom.element("syncStatusLabel").textContent, "Data gaps");
  assert.equal(dom.element("syncStatusState").textContent, "Stream ok");
  assert.equal(dom.element("syncStrip").classList.contains("sync-strip--degraded"), true);

  snap.positions.authority = current;
  snap.sources.positions = { state: "stale" };
  assert.equal(shell.snapshotHasDataGaps(snap), true);
});

test("app update footer offers only a verified target and proves the served version after reconnect", async () => {
  reset();
  update.applyUpdateStatus({ state: "development_build", current_version: "v3.0.1-39-gabcdef0", available: false });
  assert.equal(dom.element("updateAction").hidden, true);

  update.applyUpdateStatus({ state: "available", current_version: "v3.0.1", latest_version: "v3.0.2", available: true });
  assert.equal(dom.element("updateAction").hidden, false);
  assert.equal(dom.element("updateAction").textContent, "v3.0.2 available · Update");

  let requested = null;
  globalThis.fetch = async (_url, options) => {
    requested = JSON.parse(options.body);
    return response({ state: "updating", current_version: "v3.0.1", latest_version: "v3.0.2", target_version: "v3.0.2", available: false });
  };
  await update.requestUpdate();
  assert.deepEqual(requested, { target_version: "v3.0.2" });
  assert.equal(storage.get("canaryUpdateTarget"), "v3.0.2");
  assert.equal(dom.element("updateAction").textContent, "Updating to v3.0.2…");

  assert.equal(update.observeAppVersion("v3.0.2"), true);
  assert.equal(storage.has("canaryUpdateTarget"), false);
  assert.equal(dom.element("updateAction").textContent, "Updated to v3.0.2");
  clearTimeout(state.updateCompleteTimer);
  state.updateCompleteTimer = null;
});

test("TestAppJSAccountPrivacyMasksUnderlyingPnl replacement masks both summary and row until explicitly revealed", () => {
  reset();
  underlyings.setUnderlyingSummaryPnl("underlyingWinnerPnl", 12.5, "EUR");
  assert.equal(dom.element("underlyingWinnerPnl").textContent, "******");
  assert.equal(dom.element("underlyingWinnerPnl").classList.contains("is-private"), true);
  const hiddenRow = underlyings.underlyingBookRow({ symbol: "SYN", detail: "Synthetic", price: 10, change: 1, changePct: 2, pnl: 12.5, pnlCurrency: "EUR", marketFlags: [] }, "EUR");
  const hiddenPnl = byClass(hiddenRow, "underlying-row__metric--pnl")[0].children.find((child) => child.tagName === "B");
  assert.equal(hiddenPnl.textContent, "******");
  assert.equal(hiddenPnl.classList.contains("is-private"), true);
  chrome.syncAccountPrivacyState();
  assert.equal(dom.document.body.dataset.accountValues, "hidden");

  chrome.setAccountValueVisible(true);
  assert.equal(storage.get("canaryAccountValueVisible"), "true");
  assert.equal(renderCount, 1);
  underlyings.setUnderlyingSummaryPnl("underlyingWinnerPnl", 12.5, "EUR");
  assert.notEqual(dom.element("underlyingWinnerPnl").textContent, "******");
  assert.equal(shared.sensitiveMoneyHidden(12.5), false);
});

test("Settings masks the configured account everywhere until account values are revealed", () => {
  reset();
  const accountID = "SYNTHETIC-ACCOUNT-99";
  const scope = { account_id: accountID, account_mode: "paper" };
  state.snapshot = {
    account: { authority: { scope } }, positions: { authority: { scope } },
    status: { connected_account: accountID, account_mode: "paper" }, settings: {
      kind: "platform_settings", trading: { mode: { value: "paper" }, account: { value: accountID }, limits: {} },
      features: { stock_protection: { enabled: { value: true, access: "read" } } },
    },
  };
  settings.renderSettings();
  assert.deepEqual([dom.element("settingsPlateAccount").textContent, dom.element("settingsTradingMeta").textContent], [shared.maskAccountId(accountID), `paper / ${shared.maskAccountId(accountID)}`]);
  assert.equal(dom.element("settingsTradingMeta").classList.contains("is-private"), true);
  state.accountValueVisible = true;
  settings.renderSettings();
  assert.deepEqual([dom.element("settingsPlateAccount").textContent, dom.element("settingsTradingMeta").textContent], [accountID, `paper / ${accountID}`]);
  assert.equal(dom.element("settingsTradingMeta").classList.contains("is-private"), false);
});

test("Alerts uses current occurrences as the sole nudge representation", () => {
  reset();
  const now = "2026-08-09T00:00:00Z";
  state.snapshot = {
    sources: { nudges: { state: "stale", reason: "poll_stale", last_success_at: "2026-08-08T00:00:00Z" } },
    nudges: {
      as_of: now, source_health: { aggregate: "degraded" }, candidates: [{
        fingerprint: `sha256:${"a".repeat(64)}`, kind: "monthly_pulse", title: "Same process condition",
        body: "Review it", severity: "act", occurred_at: now,
      }],
    },
  };
  const active = [{
    display_id: "alert-synthetic-process", source: "governance", kind: "governance",
    presentation_code: "governance_monthly_pulse", title: "Same process condition", body: "Review it",
    severity: "act", last_seen_at: now, ended_at: null,
  }];
  const alerts = alertInbox.activeAlertItems(active);
  assert.equal(alerts.length, 1);
  assert.equal(alerts[0].alert.display_id, "alert-synthetic-process");
  assert.equal(alertInbox.activeAlertItems([]).length, 0, "a retained raw nudge is not current alert authority");
  state.snapshot.proposals = { as_of: now, proposals: [{ key: "protect", symbol: "SYN", bucket: "stock_stop" }] };
  state.snapshot.opportunities = { as_of: now, opportunities: [{ key: "exercise", symbol: "SYN", blockers: [] }] };
  assert.equal(alertInbox.activeAlertItems([]).length, 0, "non-alert actions stay on their dedicated Monitor surfaces");
});

test("alert taps resolve to exact evidence targets", () => {
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "rulebook_hedge_integrity" }), { kind: "rule", id: "hedge_integrity" });
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "rulebook_catalyst_coverage" }), { kind: "rule", id: "catalyst_coverage" });
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "regime_market_stress" }), { kind: "regime" });
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "data_health_regime" }), { kind: "regime" });
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "portfolio_stress" }), { kind: "stress" });
  assert.deepEqual(alertInbox.alertEvidenceTarget({ presentation_code: "order_integrity_mismatch" }), { kind: "orders" });

  const rule = stress.ruleChecklistRow({ id: "hedge_integrity", number: 12, title: "Protection assignment", status: "act", evidence: "Directional short." });
  const originalQuery = dom.document.querySelectorAll;
  dom.document.querySelectorAll = (selector) => selector === "[data-rule-id]" ? [rule] : selector === ".is-alert-evidence-target" && rule.classList.contains("is-alert-evidence-target") ? [rule] : [];
  alertInbox.openAlertEvidence({ presentation_code: "rulebook_hedge_integrity" });
  assert.equal(rule.classList.contains("is-alert-evidence-target"), true);
  assert.equal(rule.getAttribute("aria-current"), "location");
  const rerenderedRule = stress.ruleChecklistRow({ id: "hedge_integrity", number: 12, title: "Protection assignment", status: "act", evidence: "Directional short." });
  assert.equal(rerenderedRule.classList.contains("is-alert-evidence-target"), true, "selected Rulebook target survives a render");
  assert.equal(rerenderedRule.getAttribute("aria-current"), "location");
  dom.document.querySelectorAll = originalQuery;

  alertInbox.openAlertEvidence({ presentation_code: "regime_market_stress" });
  assert.equal(dom.element("regimeDetailPanel").classList.contains("is-alert-evidence-target"), true);
  assert.equal(dom.element("regimeDetailPanel").getAttribute("aria-current"), "location");
});

test("an alert touch preserves the card until its click opens evidence", () => {
  reset();
  state.activeTab = "alerts";
  dom.element("alertsTab").hidden = false;
  let fetches = 0;
  globalThis.fetch = async () => {
    fetches += 1;
    return response({}, 500);
  };
  const row = alertInbox.alertRowElement({
    display_id: "alert-touch-target",
    presentation_code: "rulebook_extrinsic_budget",
    title: "Option time value at risk",
    body: "Synthetic alert body.",
    severity: "act",
    first_seen_at: "2026-08-10T12:00:00Z",
    last_seen_at: "2026-08-10T12:00:00Z",
    state: "open",
    evidence_health: "current",
  });
  const title = byClass(row, "pd-alert__title")[0];
  let propagationStopped = false;

  row.dispatchEvent({
    type: "pointerdown",
    target: title,
    stopPropagation: () => { propagationStopped = true; },
  });
  assert.equal(propagationStopped, true);
  assert.equal(alertInbox.handleAttentionPointerDown({ target: title }), false);
  assert.equal(fetches, 0, "touching an alert must not start a refresh that can replace it");

  row.click();
  assert.deepEqual(state.alertEvidenceTarget, { kind: "rule", id: "extrinsic_budget" });
  assert.equal(fetches, 0);
});

test("alert rows separate affected positions and expose the authoritative review destination", () => {
  reset();
  state.snapshot = {
    rules: { rules: [{ id: "extrinsic_budget", evidence: "Paid option time value is 14.3% of NLV. The budget is 7.5%.", offenders: [{ symbol: "SYN", leg: "SYN 20261016 P 700" }, { symbol: "ALT", leg: "ALT 20261016 C 100" }, { symbol: "THR", leg: "THR extra leg" }] }] },
    brief: { ready: { capital: { consumed_pct: 93.4 }, latch: { consumed_pct_at_latch: 101.2, report_coverage_to: "2026-08-08T00:00:00Z" } } },
  };
  const optionRow = alertInbox.alertRowElement({
    display_id: "alert-option-facts", presentation_code: "rulebook_extrinsic_budget", title: "Option time value at risk",
    body: "The amount paid for time remaining in long options is above the Rulebook budget.", severity: "watch",
    first_seen_at: "2026-08-10T12:00:00Z", last_seen_at: "2026-08-10T12:00:00Z", state: "open", evidence_health: "current",
  });
  assert.equal(byClass(optionRow, "pd-alert__facts")[0].textContent, "Paid option time value is 14.3% of NLV. The budget is 7.5%.");
  const affected = byClass(optionRow, "alert-row__affected")[0];
  assert.equal(affected.children[0].textContent, "Affected positions · 3");
  assert.deepEqual(affected.children[1].children.map((item) => item.textContent), [
    "SYN 20261016 P 700", "ALT 20261016 C 100", "THR extra leg",
  ]);
  assert.equal(byClass(optionRow, "alert-row__action")[0].textContent, "Review rule details →");

  const drawdown = alertInbox.alertFactText({ presentation_code: "risk_policy_drawdown_latched" });
  assert.match(drawdown, /Current use 93\.4%/);
  assert.match(drawdown, /latched at 101\.2%/);
  assert.match(drawdown, /broker report through/);

  state.snapshot.stress = {
    primary_drivers: ["single_name_exposure_high"],
    portfolio: { largest_exposure: "SYN", largest_exposure_pct_nlv: 47.2 },
    market_indicators: [{ name: "Gamma", status: "n/a", comment: "Current options positioning is incomplete", as_of: "2026-08-10" }],
  };
  assert.equal(alertInbox.alertFactText({ presentation_code: "portfolio_stress" }), "SYN 47.2% of NLV");
  assert.equal(alertInbox.alertFactText({ presentation_code: "data_health_regime" }), "Gamma: Current options positioning is incomplete · as of 2026-08-10");
  state.snapshot.status = { data_quality: [{ surface: "regime", status: "partial", partial_clusters: ["credit"], as_of: "2026-08-10T15:58:00Z" }] };
  assert.match(alertInbox.alertFactText({ presentation_code: "data_health_regime" }), /^Credit inputs partial · as of /);
  state.snapshot.stress.market_confirmation = "partial";
  state.snapshot.stress.market_indicators = [{ name: "HYG vs SPY", status: "amber", reading: "HYG 79.51 · 0.24% below 50d 79.70", comment: "credit lagging; est 317011s; Provisional: confirmation starts at 0.25% below the 50-day average" }];
  const marketFact = alertInbox.alertFactText({ presentation_code: "regime_market_stress" });
  assert.equal(marketFact, "HYG vs SPY: HYG 79.51 · 0.24% below 50d 79.70 · Provisional: confirmation starts at 0.25% below the 50-day average · Not confirmed");
  assert.doesNotMatch(marketFact, /est \d+s/);
});

test("Rulebook rows expose stable alert destinations", () => {
  const row = stress.ruleChecklistRow({ id: "hedge_integrity", number: 12, title: "Hedge sized to the book", status: "act", evidence: "Directional short." });
  assert.equal(row.dataset.ruleId, "hedge_integrity");
  assert.equal(row.tabIndex, -1);
});

test("unconfirmed red market clusters render as provisional amber", () => {
  const credit = stress.REGIME_CLUSTERS.find((cluster) => cluster.key === "credit");
  const market = {
    unconfirmed_red_cluster_names: ["credit"],
    eligible_red_cluster_names: [],
    red_cluster_names: ["credit"],
  };
  const current = { market, market_indicators: [{ name: "HYG vs SPY", status: "amber" }] };
  assert.equal(stress.regimeClusterBand(credit, {}, current), "yellow");
  state.regimeDetailOpen = true;
  current.market_indicators[0].status = "red";
  stress.renderRegimeDetail(current.market_indicators, {}, current);
  assert.equal(byClass(dom.element("regimeIndicators"), "indicator-status")[0].classList.contains("amber"), true);
  state.regimeDetailOpen = false;
});

test("Monitor summaries distinguish alert findings, monitor-only findings, and data gaps", () => {
  reset();
  const rules = {
    rules: [
      { mode: "alert", status: "act" },
      { mode: "alert", status: "watch" },
      { mode: "track", status: "act" },
      { mode: "track", status: "watch" },
      { mode: "track", status: "info" },
      { mode: "track", status: "unknown" },
      { mode: "off", status: "act" },
    ],
  };
  assert.equal(stress.rulesTileFigure(rules), "2 alert-mode findings · 3 monitor-only · 1 data gap");
  const nonAlertRules = { rules: rules.rules.slice(2) };
  stress.renderRulesTileState(nonAlertRules, nonAlertRules.rules.map((_, index) => index));
  assert.equal(dom.element("stressRulesState").textContent, "Data gaps");
  assert.doesNotMatch(dom.element("stressRulesState").title, /every evaluated rule passes/i);
  assert.equal(dom.element("stressRulesInfoDot").hidden, false);
});

test("healthy lamp-test line hides and a served source fault reveals it", () => {
  reset();
  const snap = { updated_at: "2026-08-12T05:00:00Z", regime: { source_health: [{ source: "gamma", status: "ok" }] } };
  stress.renderLampTest(snap, { source_health: [{ source: "positions", status: "ok" }] });
  assert.equal(dom.element("lampTest").hidden, true);

  snap.regime.source_health[0].status = "stale";
  stress.renderLampTest(snap, { source_health: [{ source: "positions", status: "ok" }] });
  assert.equal(dom.element("lampTest").hidden, false);
  assert.match(dom.element("lampTestStamp").textContent, /gamma.*stale/i);
});

test("lamp test translates the internal alert-candidate feed into operator meaning", () => {
  const health = stress.lampTestSources({
    sources: { alert_candidates: { state: "unavailable", error: "producer unavailable" } },
  }, {});
  assert.deepEqual(health.faults, ["alert checking unavailable — current Alerts unconfirmed"]);
  assert.doesNotMatch(health.faults[0], /candidate/i);
});

test("market quote direction is literal for every symbol, including VIX", () => {
  assert.equal(stress.marketQuoteChangeClass("SPY", 1.2), "signed ok");
  assert.equal(stress.marketQuoteChangeClass("SPY", -1.2), "signed risk");
  assert.equal(stress.marketQuoteChangeClass("VIX", 1.2), "signed ok");
  assert.equal(stress.marketQuoteChangeClass("VIX", -1.2), "signed risk");
});

test("master subline separates an allowed portfolio rebalance from an unavailable market signal", () => {
  const evidence = ["breadth", "vol", "credit", "gamma", "funding", "fx"]
    .map((source) => ({ source, signal: "cluster", bucket: "green" }));
  const snap = {
    regime: {
      lifecycle: { stage: "data_quality", timing: "data_quality", evidence },
      posture: { tone: "data_quality", readiness: "blocked" },
    },
  };
  const current = {
    action: "rebalance",
    direction: "rebalance",
    severity: "watch",
    planner_mode_hint: "rebalance",
    planner_readiness: "ready",
    market: { stale_clusters: ["breadth", "funding"] },
  };
  assert.equal(
    stress.masterSubline(snap, current),
    "Rebalance based on portfolio risk · Market signal unavailable until Breadth and Funding recover",
  );

  current.action = "confirm_inputs";
  current.direction = "data_quality";
  current.planner_mode_hint = "confirm_data";
  current.planner_readiness = "blocked";
  assert.equal(
    stress.masterSubline(snap, current),
    "No market-stress action · Wait for Breadth and Funding to recover",
  );
});

test("a derived source row is named as inherited, not counted as an extra failure", () => {
  reset();
  const snap = {
    updated_at: "2026-08-13T19:00:00Z",
    regime: { source_health: [
      { source: "funding", status: "stale" },
      { source: "breadth", status: "stale" },
      { source: "vol", status: "ok" },
    ] },
  };
  const stressResult = { source_health: [
    { source: "positions", status: "ok" },
    { source: "regime", status: "stale", derived_from: ["funding", "breadth"], notes: ["stale clusters: breadth,funding"] },
  ] };

  const health = stress.lampTestSources(snap, stressResult);
  assert.equal(health.total, 4, "derived row must not join the denominator");
  assert.equal(health.ok, 2);
  assert.equal(health.faults.length, 2, "only leaf faults count");
  assert.equal(health.inherited.length, 1);
  assert.match(health.inherited[0], /inherits.*funding series/i);

  stress.renderLampTest(snap, stressResult);
  assert.match(dom.element("lampTestStamp").textContent, /2\/4 sources ok/);
  assert.match(dom.element("lampTestStamp").title, /inherits/i);

  const labels = stress.stressInputIssueLabels({ market: { stale_clusters: ["funding", "breadth"] }, ...stressResult }, snap);
  assert.ok(!labels.includes("regime snapshot"), `derived regime row must not add its own label, got ${labels.join(", ")}`);
  assert.ok(labels.includes("funding series") && labels.includes("breadth compute"), labels.join(", "));

  // A regime row without derived_from (older daemon payload) still counts
  // as its own source, exactly as before.
  const legacy = stress.lampTestSources(snap, { source_health: [
    { source: "positions", status: "ok" },
    { source: "regime", status: "stale", notes: ["stale clusters: breadth,funding"] },
  ] });
  assert.equal(legacy.total, 5);
  assert.equal(legacy.faults.length, 3);
});

test("Protection tile never presents zero actionable theta as portfolio theta", () => {
  reset();
  protection.renderProtectionTile({ counts: { actionable: 0 } }, [], { value: 0, currency: "EUR", title: "No theta-hygiene action is above policy threshold." });
  assert.equal(dom.element("protectionTileCounts").textContent, "0 actions");
  assert.doesNotMatch(dom.element("protectionTileCounts").textContent, /theta/i);

  protection.renderProtectionTile({ counts: { actionable: 1 } }, [{}], { value: 12, currency: "EUR" });
  assert.match(dom.element("protectionTileCounts").textContent, /theta action/i);
});

test("leaving Alerts for alert evidence cancels acknowledgement without a false error", async () => {
  reset();
  const now = "2026-08-09T12:00:00Z";
  const freshUntil = "2099-08-09T12:10:00Z";
  const attention = { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] };
  const current = {
    schema_version: "alerts-v1", version: "alert-delivery-v4", initialized: true, generation: 1,
    as_of: now, current_state: "clear",
    coverage: { state: "complete", freshness: "current", as_of: now, expected_sources: ["canary"], covered_sources: ["canary"] },
    sources: [{ source: "canary", status: "current", reason: "authoritative", evidence_health: "current", input_as_of: now, observed_at: now, evidence_as_of: now, fresh_until: freshUntil, covered: true }],
    occurrences: [], attention,
    delivery_health: { state: "healthy", class: "", updated_at: now, last_push_service_acceptance_at: null },
  };
  state.alerts = current;
  state.alertsFeedValid = true;
  state.activeTab = "alerts";
  state.attentionReadInFlight = null;
  state.attentionStatus = { state: "", error: false };
  dom.element("alertsTab").hidden = false;
  let releaseAttention;
  const attentionGate = new Promise((resolve) => { releaseAttention = resolve; });
  globalThis.fetch = async (url) => {
    if (String(url) === "/api/alerts/attention") {
      await attentionGate;
      return response(attention);
    }
    if (String(url) === "/api/alerts") return response(current);
    return response({ error: "unexpected request" }, 500);
  };
  const pending = alertInbox.acknowledgeAttention();
  state.activeTab = "monitor";
  dom.element("alertsTab").hidden = true;
  releaseAttention();
  assert.equal(await pending, false);
  assert.deepEqual(state.attentionStatus, { state: "", error: false });
});

test("Alerts recovery status explains retained truth and automatic retry", async () => {
	reset();
	globalThis.fetch = async () => response({ error: "synthetic unavailable" }, 503);
	assert.equal(await alertInbox.refreshAlerts(), false);
	assert.match(state.attentionStatus.state, /last verified list/i);
	assert.match(state.attentionStatus.state, /retry automatically/i);

	state.activeTab = "alerts";
	dom.element("alertsTab").hidden = false;
	state.attentionStatus = { state: "", error: false };
	assert.equal(await alertInbox.acknowledgeAttention({ retry: false }), false);
	assert.match(state.attentionStatus.state, /remain unread/i);
	assert.match(state.attentionStatus.state, /retry automatically/i);
});

test("Open Orders shows check age and refreshes from the typed read surface", async () => {
	reset();
	const originalNow = Date.now;
	try {
		Date.now = () => Date.parse("2026-08-13T12:20:00Z");
		state.ordersOpen = { as_of: "2026-08-13T12:00:00Z", orders: [] };
		orders.renderOpenOrders();
		assert.equal(dom.element("ordersAsOf").textContent, "checked 20m ago");
		assert.equal(dom.element("ordersAsOf").classList.contains("stale"), true);

		globalThis.fetch = async (url) => {
			assert.equal(String(url), "/api/orders/open");
			return response({ as_of: "2026-08-13T12:20:00Z", orders: [] });
		};
		assert.equal(await orders.refreshOpenOrders(), true);
		assert.equal(dom.element("ordersAsOf").textContent, "checked now");
		assert.equal(dom.element("ordersAsOf").classList.contains("stale"), false);
	} finally {
		Date.now = originalNow;
	}
});

test("option exercise renders blockers, stale previews, explicit confirmation, and exact-once submit", async () => {
  reset();
  state.opportunitiesOpen = true;
  const asOf = "2026-08-09T00:00:00Z";
  const account = "SYNTHETIC-CONFIRM-ACCOUNT";
  const base = { key: "exercise-eligible", revision: "rev-1", bucket: "option_exercise", symbol: "SYN", quantity: 2, contract: { right: "P", strike: 95, expiry: "2026-09-18" }, blockers: [] };
  state.snapshot = {
    trading: { mode: "paper", account, can_preview: true, can_write: true }, opportunities: {},
  };
  const render = (candidate) => {
    state.snapshot.opportunities = { as_of: asOf, counts: { total: 1 }, opportunities: [candidate] };
    opportunities.renderOpportunitiesPanel(state.snapshot.opportunities);
    return dom.element("opportunitiesRows").children[0];
  };
  let requests = [];
  const blocked = { ...base, blockers: [{ code: "funding_unavailable", message: "Funding evidence is unavailable" }] };
  let row = render(blocked);
  assert.match(byClass(row, "opportunity-row__blocker")[0].textContent, /Funding evidence is unavailable/);
  assert.equal(byClass(row, "opportunity-preview")[0].disabled, true);
  assert.equal(byClass(row, "opportunity-submit")[0].disabled, true);
  assert.equal(requests.length, 0);

  let releaseExercise;
  globalThis.fetch = async (url, init = {}) => {
    const request = { url: String(url), method: init.method || "GET", body: init.body ? JSON.parse(init.body) : null };
    requests.push(request);
    if (request.url === "/api/opportunities/preview-exercise") {
      return response({ opportunity: { key: base.key, revision: base.revision }, blockers: [], submit_eligible: true, preview_token: "synthetic-preview-token" });
    }
    if (request.url === "/api/opportunities/exercise") {
      return new Promise((resolve) => { releaseExercise = () => resolve(response({ accepted: true })); });
    }
    return response({ error: "unexpected synthetic request" }, 500);
  };

  row = render(base);
  byClass(row, "opportunity-preview")[0].click();
  await waitFor(() => requests.length === 1 && state.opportunityPreviewBusy === "", "exercise preview did not settle");
  assert.deepEqual(requests[0], {
    url: "/api/opportunities/preview-exercise",
    method: "POST",
    body: { key: base.key, revision: base.revision, quantity: 2, timeout_ms: 5000 },
  });
  assert.equal(requests.length, 1, "preview alone must not exercise");
  row = dom.element("opportunitiesRows").children[0];
  let confirm = byClass(row, "opportunity-submit")[0];
  assert.equal(confirm.disabled, false);
  assert.match(byClass(row, "opportunity-row__submit-state")[0].textContent, /Ready for explicit confirmation/);

  const stale = { ...base, revision: "rev-2" };
  state.opportunityPreviews[opportunities.opportunityPreviewStateKey(stale)] = { opportunity: { revision: base.revision }, blockers: [], submit_eligible: true, preview_token: "stale" };
  row = render(stale);
  assert.equal(byClass(row, "opportunity-submit")[0].disabled, true);
  assert.match(byClass(row, "opportunity-row__submit-state")[0].textContent, /Opportunity changed/);

  row = render(base);
  confirm = byClass(row, "opportunity-submit")[0];
  confirm.click();
  confirm.click();
  await waitFor(() => typeof releaseExercise === "function", "exercise confirmation did not reach the intercepted endpoint");
  assert.equal(requests.filter((request) => request.url === "/api/opportunities/exercise").length, 1, "double activation must submit once");
  assert.deepEqual(requests[1], {
    url: "/api/opportunities/exercise",
    method: "POST", body: { key: base.key, revision: base.revision, quantity: 2, preview_token: "synthetic-preview-token", timeout_ms: 5000, confirm_account: account, confirm_mode: "paper" },
  });
  row = dom.element("opportunitiesRows").children[0];
  assert.equal(byClass(row, "opportunity-submit")[0].disabled, true, "submission stays disabled while in flight");
  releaseExercise();
  await waitFor(() => state.opportunitySubmits[opportunities.opportunityPreviewStateKey(base)]?.accepted === true, "exercise submission did not settle");
  row = dom.element("opportunitiesRows").children[0];
  assert.equal(byClass(row, "opportunity-submit")[0].disabled, true, "an accepted preview cannot be resubmitted");
  assert.match(byClass(row, "opportunity-row__submit-state")[0].textContent, /Exercise instruction sent/);
  byClass(row, "opportunity-submit")[0].click();
  assert.equal(requests.filter((request) => request.url === "/api/opportunities/exercise").length, 1);
});

test("account authority distinguishes genuine zero from a missing money field", () => {
  reset();
  state.accountValueVisible = true;
  const now = new Date().toISOString();
  const account = {
    account_id: "SYNTHETIC-LEGACY-ID",
    base_currency: "EUR",
    net_liquidation: 0,
    buying_power: 0,
    daily_pnl: 0,
    daily_pnl_observation: { status: "ok", as_of: now },
    as_of: now,
    authority: sourceAuthority({
      source: "account_summary_request", as_of: now,
      fields: { base_currency: true, net_liquidation: true, buying_power: false, daily_pnl: true },
    }),
  };
  state.snapshot = {
    account,
    positions: { portfolio: {}, authority: { scope: accountScope() } },
    status: { connected_account: "SYNTHETIC-AUTHORITY", account_mode: "paper" },
    market_calendar: { session: { state: "regular", is_open: true } },
    sources: { account: { state: "current", last_success_at: now } },
  };

  underlyings.renderAccountPanel(account, state.snapshot.positions, {});
  assert.match(dom.element("netLiquidation").textContent, /0/);
  assert.match(dom.element("netLiquidation").textContent, /€|EUR/);
  assert.equal(dom.element("buyingPower").textContent, "--", "legacy zero must stay missing when the daemon says the field is absent");
  assert.match(dom.element("dailyPnl").textContent, /0/);
  assert.equal(dom.element("dailyPnlPct").textContent, "--", "a zero NLV cannot support a Daily P/L percentage");
  assert.equal(dom.element("accountLabel").textContent, "SYNTHETIC-AUTHORITY");
  assert.equal(shared.accountFieldValue(account, "net_liquidation"), 0);
  assert.equal(shared.accountFieldValue(account, "buying_power"), null);
  assert.equal(shared.accountBaseCurrency(account), "EUR");

  state.accountValueVisible = false;
  underlyings.renderAccountPanel(account, state.snapshot.positions, {});
  assert.equal(dom.element("netLiquidation").textContent, "******", "a genuine zero remains a present private value");
  assert.equal(dom.element("buyingPower").textContent, "--", "a missing zero must not become a private value");
  assert.equal(dom.element("accountLabel").textContent, shared.maskAccountId("SYNTHETIC-AUTHORITY"));
});

test("account identity refuses conflicting typed account scopes", () => {
  reset();
  const account = { authority: { scope: accountScope("SYN-A"), availability: "unavailable" } };
  state.snapshot = {
    account,
    positions: { authority: { scope: accountScope("SYN-B") } },
    status: { connected_account: "SYN-A", account_mode: "paper" },
  };
  assert.deepEqual(underlyings.currentAccountContext(account), {
    accountId: "",
    accountLabel: "Account mismatch",
    modeClass: "paper",
    modeLabel: "Paper",
    hasAccount: false,
  });
});

test("stale positions never render an empty clean book and retain nonempty rows only as context", () => {
  reset();
  const staleAt = new Date(Date.now() - 20 * 60_000).toISOString();
  const authority = sourceAuthority({ scope: accountScope("SYNTHETIC"), availability: "unavailable", freshness: "stale", reason: "receipt_stale", as_of: staleAt });
  const empty = { stocks: [], options: [], by_underlying: [], portfolio: {}, authority, as_of: "" };
  state.snapshot = { positions: empty, account: {}, sources: { positions: { state: "current", last_success_at: staleAt } } };
  underlyings.renderUnderlyings(empty, {});
  assert.equal(dom.element("underlyingBookCount").textContent, "Positions unavailable");
  assert.match(dom.element("underlyingBookStatus").textContent, /stale/i);
  assert.equal(dom.element("underlyingBookList").textContent, "Position data unavailable.");
  assert.match(dom.element("underlyingBookFreshness").textContent, /^Positions stale/);

  const retained = {
    ...empty,
    by_underlying: [{
      underlying: "SYN",
      stock: { symbol: "SYN", currency: "EUR", position: 1, quote_expectation: "none", daily_pnl_base: 0 },
      options: [],
      group_daily_pnl_base: 0,
    }],
  };
  state.snapshot.positions = retained;
  underlyings.renderUnderlyings(retained, {});
  assert.match(dom.element("underlyingBookCount").textContent, /^1 last known/);
  assert.match(dom.element("underlyingBookStatus").textContent, /visible for reference/i);
  assert.match(dom.element("underlyingBookList").textContent, /SYN/);
  assert.equal(dom.element("underlyingWinnerPnl").textContent, "--", "stale rows must not publish a clean or current P/L summary");
});

test("TestAppJSProtectionFastPathKeepsHardMarketEventBlocker replacement re-evaluates current active blockers at preview and submit", () => {
  reset();
  const proposal = { key: "synthetic-protection", symbol: "SYN", blockers: [] };
  const active = { by_symbol: { SYN: [{ id: "halt_regulatory_or_news", symbol: "SYN", status: "active", label: "Regulatory halt" }] } };
  const blocker = marketEvents.protectionMarketEventBlocker(proposal, active);
  assert.equal(blocker.code, "market_event_halt_regulatory_or_news");
  assert.match(blocker.message, /Regulatory halt is active/);
  assert.equal(marketEvents.protectionMarketEventBlocker(proposal, { by_symbol: { SYN: [{ id: "halt_regulatory_or_news", status: "recent" }] } }), null);
  state.snapshot = { trading: { can_preview: true, can_write: true }, proposals: {}, market_events: active };
  assert.equal(protection.protectionPreviewGate(proposal).ready, false);
  assert.equal(protection.protectionSubmitGate(proposal).ready, false);
  state.snapshot.market_events = { by_symbol: {} };
  assert.equal(protection.protectionPreviewGate(proposal).ready, true);
});

test("uncovered coverage rows stage a stop request that flows into the existing preview/submit gates", async () => {
  reset();
  state.protectionOpen = true;
  state.protectionSnapshotLastAt = Date.now();
  const coverageData = {
    status: "review",
    counts: { unprotected: 1, partial: 1, covered: 1 },
    by_underlying: [
      { underlying: "SYN", state: "unprotected", position_quantity: 12, unprotected_quantity: 12, unprotected_notional_base: 1200, unprotected_notional_base_currency: "EUR" },
      { underlying: "PART", state: "partial", position_quantity: 10, protected_quantity: 4, unprotected_quantity: 6 },
      { underlying: "COV", state: "covered", position_quantity: 5, protected_quantity: 5 },
      { underlying: "GONE", state: "orphaned_order" },
    ],
  };
  state.snapshot = {
    trading: { can_write: false, can_preview: true },
    positions: { stocks: [{ symbol: "SYN", con_id: 42, quantity: 12 }], options: [], protection_coverage: coverageData },
    proposals: {}, auto_trade: {}, market_events: {},
  };
  assert.deepEqual(protection.protectionRepairRows(coverageData).map((row) => row.underlying), ["SYN", "PART"],
    "repair rows must keep only unprotected/partial ledger rows");
  protection.renderProtectionCoverageRepair();
  const box = dom.element("protectionCoverageRepair");
  assert.equal(box.hidden, false);
  let buttons = byClass(box, "protection-repair__request");
  assert.equal(buttons.length, 2);
  assert.equal(buttons[0].disabled, true, "write-disabled trading must disable the request affordance");
  state.snapshot.trading = { can_write: true, can_preview: true, account: "DU111", mode: "paper" };
  protection.renderProtectionCoverageRepair();
  buttons = byClass(box, "protection-repair__request");
  assert.equal(buttons[0].disabled, false);
  let captured = null;
  const staged = {
    accepted: true, con_id: 42, symbol: "SYN", proposal_key: "trailing_stop:abcd", revision: "sha256:rev",
    ignore_cleared: true,
    snapshot: { revision: "sha256:rev", proposals: [{ key: "trailing_stop:abcd", revision: "sha256:rev", bucket: "trailing_stop", symbol: "SYN" }], counts: { total: 1, actionable: 1 } },
  };
  globalThis.fetch = async (url, init) => {
    captured = { url, body: JSON.parse(init.body) };
    return response(staged);
  };
  buttons[0].click();
  await waitFor(() => state.protectionStopRequests.SYN && !state.protectionStopRequests.SYN.pending, "stop request never settled");
  assert.equal(captured.url, "/api/proposals/request-stop");
  assert.equal(captured.body.con_id, 42, "unique held stock must resolve to con_id");
  assert.equal(captured.body.confirm_account, "DU111");
  assert.equal(captured.body.confirm_mode, "paper");
  assert.equal(state.snapshot.proposals.revision, "sha256:rev",
    "the returned snapshot must become the live proposals snapshot so preview uses the same revision");
  const note = protection.protectionStopRequestNote("SYN");
  assert.equal(note.blocked, false);
  assert.match(note.text, /Preview stop/);
  assert.match(note.text, /prior ignore cleared/);
});

test("option exits render the approved loss and profit-trail semantics without calling them coverage", () => {
  reset();
  const loss = {
    bucket: "option_loss_exit", action: "SELL", tif: "DAY",
    option_exit: { kind: "loss_exit", return_pct: -62, loss_exit_pct: 60, dte: 31 },
  };
  assert.equal(protection.protectionBucketLabel(loss), "Option loss exit");
  assert.equal(protection.protectionSideLabel(loss), "Sell to close");
  assert.equal(protection.protectionSubmitLabel(loss), "Preview exit");
  assert.equal(protection.protectionFinalSubmitLabel(loss), "Submit exit");
  assert.match(protection.protectionMetricText(loss), /premium −62\.0% · exit line −60\.0% · 31 DTE · DAY limit close/);
  assert.match(protection.protectionActionTitle(loss), /may remain unfilled while the loss worsens/i);

	const review = { bucket: "option_exit_review", option_exit: { kind: "review", dte: 31 } };
	assert.equal(protection.protectionBucketLabel(review), "Option exit review");
	assert.match(protection.protectionMetricText(review), /exact-contract evidence unavailable · 31 DTE · blocked/);

  const profit = {
    bucket: "trailing_stop", action: "SELL", tif: "DAY", sec_type: "OPT",
    option_exit: { kind: "profit_trail", return_pct: 55, profit_arm_gain_pct: 50, locked_gain_pct: 5, initial_locked_gain_pct: 7, dte: 31 },
		trail: { trailing_percent: 30, initial_stop_price: 1.1, limit_offset: 0.05 },
    trail_sizing: { chosen_pct: 30, selected_by: "policy_default" },
  };
  assert.equal(protection.protectionBucketLabel(profit), "Option profit trail");
  assert.equal(protection.protectionSideLabel(profit), "Sell profit trail");
  assert.equal(protection.protectionSubmitLabel(profit), "Preview trail");
  assert.equal(protection.protectionFinalSubmitLabel(profit), "Submit trail");
  assert.match(protection.protectionMetricText(profit), /premium \+55\.0% · armed at \+50\.0% · initial lock \+7\.0% · 31 DTE/);
	assert.match(protection.protectionMetricText(profit), /native 30\.0% premium trail/);
  assert.match(protection.protectionActionTitle(profit), /DAY TRAIL LIMIT close for the full exact-contract position/i);
});

test("TestAppJSMoneyFormattersNeverDefaultToUSD replacement preserves unknown, base, and contract currency semantics", () => {
  reset();
  assert.equal(shared.money(729.87, ""), "729.87");
  assert.equal(shared.money(729.87, "MIX"), "729.87 MIX");
  assert.match(shared.money(729.87, "EUR"), /729[,.]87/);
  assert.equal(shared.money(729.87, "").includes("$"), false);
  state.snapshot = { account: { base_currency: "EUR", authority: { availability: "available", fields: { base_currency: true } } } };
  assert.equal(protection.protectionLossCurrency(true, {}), "EUR");
  assert.equal(protection.protectionLossCurrency(false, { currency: "GBP" }), "GBP");
  const leg = protection.deriskLegRow({ action: "SELL", reduce_quantity: 1, symbol: "SYN", risk_contribution_cut: 200, notional_currency: "USD" }, false, "EUR");
  const legText = leg.textContent;
  assert.match(legText, /€200|EUR\s*200|200\s*€/);
  assert.equal(/\$200|USD\s*200/.test(legText), false);
  const position = protection.protectionPositionLine({ contract: { currency: "USD" }, position_day_change_money: 12.5, position_day_change_currency: "" });
  assert.equal(position.textContent.includes("$"), false);
  assert.equal(settings.tradingLimitSummary({ max_notional: { value: 5000 } }).includes("$"), false);
});

test("TestRegimeAuthorityHealthControlsVisibleDataQualityPosture replacement qualifies stale and unavailable authority", () => {
  reset();
  const snap = {
    regime: { authority_health: { status: "stale", failure_code: "refresh_timeout", last_success_at: "2026-07-01T10:00:00Z" } },
    sources: { regime: { state: "current" } },
  };
  const authority = stress.regimeAuthorityView(snap);
  assert.equal(authority.status, "stale");
  assert.equal(authority.degraded, true);
  assert.match(stress.regimeAuthorityLabel({ label: "Risk on" }, authority), /^Last known/);
  assert.equal(stress.regimePresentationPosture({ tone: "calm" }, authority).tone, "data_quality");
  stress.renderRegimeAuthorityTimestamp(snap, null);
  const timestamp = dom.element("regimeAsOf");
  assert.equal(timestamp.hidden, false);
  assert.equal(timestamp.classList.contains("stale"), true);
  assert.match(timestamp.title, /refresh timed out/);
  assert.equal(stress.regimeAuthorityView({ sources: { regime: { state: "not_observed" } } }).status, "unavailable");
  assert.equal(stress.regimeAuthorityView({ regime: { authority_health: { status: "fresh" } } }).degraded, false);
});

test("TestBriefCardStaticContract replacement renders production narrative, safe runs, and fallback sections", () => {
  reset();
  state.authenticated = true;
  state.activeTab = "monitor";
  const narrative = {
    as_of: "2026-07-01T08:30:00Z",
    brief_fingerprint: "sha256:synthetic-narrative",
    narrative: {
      lead: [{ text: "Desk ", role: "" }, { text: "€12K", role: "figure", account_sensitive: true }, { text: " 42%", role: "figure" }, { text: " <img src=x onerror=boom>", role: "watch" }],
      review: [{ runs: [{ text: "Review served movement.", role: "" }] }],
      ready: [{ runs: [{ text: "Act only on served evidence.", role: "act" }] }],
      coda: [{ text: "End of brief.", role: "" }],
    },
    review: { rules: { status: "ok", pass: 10, watch: 0, act: 0, unknown: 0 } },
    ready: { stress: { severity: "watch" } },
  };
  state.snapshot = { brief: narrative, sources: { brief: {} } };
  brief.renderBriefCard(state.snapshot);
  const sections = dom.element("briefSections");
  assert.equal(sections.classList.contains("brief-sections--narrative"), true);
  assert.equal(byClass(sections, "pd-placard").some((node) => node.textContent === "Review · since last close"), true);
  assert.equal(byClass(sections, "pd-placard").some((node) => node.textContent === "Ready · next open"), true);
  assert.deepEqual(byClass(sections, "pd-fig").map((node) => node.textContent), ["******", " 42%"]);
  state.accountValueVisible = true;
  brief.renderBriefCard(state.snapshot);
  assert.equal(byClass(sections, "pd-fig")[0].textContent, "€12K");
  assert.equal(byClass(sections, "pd-wtint")[0].textContent, " <img src=x onerror=boom>");
  assert.equal(descendants(sections).some((node) => node.tagName === "IMG"), false, "served runs must remain text, never markup");
  assert.equal(descendants(sections).some((node) => node.id === "briefSignoffButton"), false);

  const fallback = {
    as_of: "2026-07-01T08:30:00Z",
    brief_fingerprint: "sha256:synthetic-fallback",
    review: { status: "ok", rules: { status: "ok", pass: 10, watch: 0, act: 0, unknown: 0 }, session_pnl: { daily_pnl_base: 5, base_currency: "EUR" } },
    ready: { status: "ok", monthly_pulse: { status: "not_due", month: "2026-07" } },
  };
  state.snapshot = { brief: fallback, sources: {} };
  brief.renderBriefCard(state.snapshot);
  assert.equal(sections.classList.contains("brief-sections--narrative"), false);
  assert.deepEqual(sections.children.map((node) => byClass(node, "brief-section__head")[0]?.textContent), ["Reviewok", "Readyok"]);
  assert.equal(descendants(sections).some((node) => node.id === "briefSignoffButton"), false);
});

// Breadth is a specific trading session's close, and the daemon keeps serving
// The row rendered the percentages with nothing saying which day they came
