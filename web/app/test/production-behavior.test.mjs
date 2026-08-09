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

const { state } = await import("../state.js");
const { installRenderAll } = await import("../render-runtime.js");
const moduleNames = [
  "alerts", "alert-inbox", "brief", "chrome", "lifecycle", "market-events", "opportunities", "orders",
  "portfolio", "protection", "protection-coverage", "settings", "shared", "shell", "stress", "underlyings",
];
const modules = Object.fromEntries(await Promise.all(moduleNames.map(async (name) => [name, await import(`../${name}.js`)])));
const { alerts, brief, chrome, lifecycle, opportunities, orders, portfolio, protection, settings, shared, shell, stress, underlyings } = modules;
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
  Object.assign(state, {
    snapshot: null, settings: null, authenticated: true, activeTab: "monitor", accountValueVisible: false,
    pairingRequired: false, connectionOK: false, connectionText: "Connecting", eventSource: null,
    portfolioDetailOpen: false, protectionOpen: false, protectionQtyOverrides: {}, protectionQuoteTicks: {},
    protectionSnapshotBusy: false, protectionSnapshotLastAt: 0, protectionSnapshotNotice: "",
    protectionDerisk: { percent: 25, busy: "", result: null, submitted: null, requestRef: "", previewedAt: 0, abort: null },
    opportunitiesOpen: false, opportunityPreviewBusy: "", opportunityPreviews: {}, opportunitySubmitBusy: "", opportunitySubmits: {},
    opportunitySnapshotBusy: false, opportunitySnapshotLastAt: 0, opportunitySnapshotNotice: "",
    governance: null, governanceRefreshSucceeded: null, reconciliationCheck: { busy: false, state: "", error: false },
    safeNotificationTest: { busy: false, state: "", error: false }, pushInspection: { state: "unsupported", busy: false },
    alertSettings: { mode: "watch_and_act" }, alertsRefreshInFlight: null, alertsRefreshTimer: null,
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
  assert.equal(dom.element("syncStatusState").textContent, "Live");
  assert.equal(dom.element("syncStrip").classList.contains("sync-strip--degraded"), false);

  snap.positions.authority = { availability: "unavailable", freshness: "unknown", reason: "unprimed" };
  shell.renderSyncStrip(snap);
  assert.equal(dom.element("syncStatusLabel").textContent, "Data gaps");
  assert.equal(dom.element("syncStatusState").textContent, "Degraded");
  assert.equal(dom.element("syncStrip").classList.contains("sync-strip--degraded"), true);

  snap.positions.authority = current;
  snap.sources.positions = { state: "stale" };
  assert.equal(shell.snapshotHasDataGaps(snap), true);
});

test("TestAppJSAccountPrivacyMasksUnderlyingPnl replacement masks both summary and row until explicitly revealed", () => {
  reset();
  underlyings.setUnderlyingSummaryPnl("underlyingWinnerPnl", 12.5, "EUR");
  assert.equal(dom.element("underlyingWinnerPnl").textContent, "******");
  assert.equal(dom.element("underlyingWinnerPnl").classList.contains("is-private"), true);
  const hiddenRow = underlyings.underlyingBookRow({ symbol: "SYN", detail: "Synthetic", price: 10, change: 1, changePct: 2, pnl: 12.5, pnlCurrency: "EUR", marketFlags: [] }, "EUR");
  const hiddenPnl = byClass(hiddenRow, "underlying-row__metric--pnl")[0].children[0];
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

test("Action Queue uses alert occurrences as the sole nudge representation", () => {
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
  const queue = alertInbox.actionQueueItems(active);
  assert.deepEqual(queue.map((item) => item.kind), ["alert"]);
  assert.equal(alertInbox.actionQueueItems([]).length, 0, "a retained raw nudge is not current queue authority");
  state.snapshot.proposals = { as_of: now, proposals: [{ key: "protect", symbol: "SYN", bucket: "stock_stop" }] };
  state.snapshot.opportunities = { as_of: now, opportunities: [{ key: "exercise", symbol: "SYN", blockers: [] }] };
  assert.deepEqual(alertInbox.actionQueueItems([]).map((item) => item.kind).sort(), ["exercise", "protection"]);
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
