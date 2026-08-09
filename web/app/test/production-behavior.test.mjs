import assert from "node:assert/strict";
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
const chrome = await import("../chrome.js");
const coverage = await import("../protection-coverage.js");
const lifecycle = await import("../lifecycle.js");
const marketEvents = await import("../market-events.js");
const orders = await import("../orders.js");
const portfolio = await import("../portfolio.js");
const protection = await import("../protection.js");
const settings = await import("../settings.js");
const shared = await import("../shared.js");
const shell = await import("../shell.js");
const stress = await import("../stress.js");
const underlyings = await import("../underlyings.js");
const alerts = await import("../alerts.js");
const brief = await import("../brief.js");

let renderCount = 0;
installRenderAll(() => { renderCount += 1; });

function response(body, status = 200) {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() { return typeof body === "string" ? JSON.parse(body) : body; },
    async text() { return text; },
  };
}

function reset() {
  storage.clear();
  dom.elements.clear();
  dom.document.body.dataset = {};
  renderCount = 0;
  state.snapshot = null;
  state.settings = null;
  state.authenticated = true;
  state.activeTab = "monitor";
  state.accountValueVisible = false;
  state.pairingRequired = false;
  state.connectionOK = false;
  state.connectionText = "Connecting";
  state.eventSource = null;
  state.portfolioDetailOpen = false;
  state.protectionOpen = false;
  state.protectionQtyOverrides = {};
  state.protectionQuoteTicks = {};
  state.protectionSnapshotBusy = false;
  state.protectionSnapshotLastAt = 0;
  state.protectionSnapshotNotice = "";
  state.protectionDerisk = { percent: 25, busy: "", result: null, submitted: null, requestRef: "", previewedAt: 0, abort: null };
  state.governance = null;
  state.governanceRefreshSucceeded = null;
  state.reconciliationCheck = { busy: false, state: "", error: false };
  state.safeNotificationTest = { busy: false, state: "", error: false };
  state.pushInspection = { state: "unsupported", busy: false };
  state.alertSettings = { mode: "watch_and_act" };
  state.alertsRefreshInFlight = null;
  if (state.alertsRefreshTimer) clearTimeout(state.alertsRefreshTimer);
  state.alertsRefreshTimer = null;
  state.alertsRefreshDueAt = 0;
  state.alertsLastRefreshAt = 0;
  globalThis.fetch = async () => response({});
}

function descendants(node) {
  if (!node || typeof node !== "object") return [];
  return [node, ...(node.children || []).flatMap(descendants)];
}

function byClass(node, className) {
  return descendants(node).filter((item) => item.classList?.contains(className));
}

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
    authority: {
      scope: { account_id: "SYNTHETIC-AUTHORITY", account_mode: "paper" },
      source: "account_summary_request",
      availability: "available",
      freshness: "current",
      as_of: now,
      fields: { base_currency: true, net_liquidation: true, buying_power: false, daily_pnl: true },
    },
  };
  state.snapshot = {
    account,
    positions: { portfolio: {}, authority: { scope: { account_id: "SYNTHETIC-AUTHORITY", account_mode: "paper" } } },
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
  const account = { authority: { scope: { account_id: "SYN-A", account_mode: "paper" }, availability: "unavailable" } };
  state.snapshot = {
    account,
    positions: { authority: { scope: { account_id: "SYN-B", account_mode: "paper" } } },
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
  const authority = {
    scope: { account_id: "SYNTHETIC", account_mode: "paper" },
    source: "portfolio_stream",
    availability: "unavailable",
    freshness: "stale",
    reason: "receipt_stale",
    as_of: staleAt,
  };
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
      lead: [{ text: "Desk ", role: "" }, { text: "€12K", role: "figure" }, { text: " <img src=x onerror=boom>", role: "watch" }],
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
