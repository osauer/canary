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
  state.protectionOpen = false;
  state.protectionQtyOverrides = {};
  state.protectionQuoteTicks = {};
  state.protectionSnapshotBusy = false;
  state.protectionSnapshotLastAt = 0;
  state.protectionSnapshotNotice = "";
  state.protectionDerisk = { percent: 25, busy: "", result: null, submitted: null, requestRef: "", previewedAt: 0, abort: null };
  state.governance = null;
  state.governanceRefreshSucceeded = null;
  state.governanceCutoverReceipt = null;
  state.governanceCutoverReview = { busy: false, state: "", error: false };
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

test("TestAppJSStalePairingURLFallsBackToDeviceLogin replacement executes bounded retry and definitive rejection", async () => {
  reset();
  const calls = [];
  globalThis.fetch = async (url) => {
    calls.push(url);
    if (url === "/api/bootstrap" && calls.filter((value) => value === url).length === 1) {
      return response("temporary relay outage", 503);
    }
    if (url === "/api/bootstrap") {
      return response({
        auth: { authenticated: true },
        alert_settings: { mode: "watch_and_act" },
        governance: null,
        alerts: null,
        snapshot: { account: {}, positions: { stocks: [], options: [], portfolio: {} }, stress: {}, sources: {} },
      });
    }
    if (url === "/api/orders/open") return response({ orders: [] });
    if (url === "/api/purge/status") return response({ entries: [] });
    return response({});
  };
  const delays = [];
  assert.equal(await lifecycle.bootstrapWithRetry({ sleep: async (delay) => delays.push(delay) }), true);
  assert.deepEqual(delays, [2000]);
  assert.equal(state.authenticated, true);
  assert.equal(state.pairingRequired, false);
  if (state.alertsRefreshTimer) clearTimeout(state.alertsRefreshTimer);
  state.alertsRefreshTimer = null;

  reset();
  globalThis.fetch = async () => response({}, 401);
  assert.equal(await lifecycle.fetchBootstrap(), null);
  assert.equal(state.pairingRequired, true);
  assert.match(dom.element("pairingText").textContent, /Scan a fresh QR code/);
});

test("TestAppJSTradingStateUsesSnapshotCanWrite replacement exercises typed write and freeze-aware cancel gates", () => {
  reset();
  assert.equal(settings.tradingStatusSettingsLabel({}, { can_write: true }), "Write ready");
  assert.equal(settings.tradingStatusSettingsLabel({}, { can_preview: true }), "Preview ready");
  const order = { order_ref: "synthetic-order", open: true, order_type: "LMT", remaining: 2 };
  assert.equal(orders.orderModifyGate(order, { can_write: false }).ready, false);
  assert.equal(orders.orderModifyGate(order, { can_write: true }).ready, true);
  assert.equal(underlyings.canWriteUnderlyings({ can_write: true }), false);
  assert.match(underlyings.underlyingWriteReason("purge", true, { can_write: true }), /submission is unavailable/);
  const frozen = { can_write: false, mode: "paper", account: "synthetic", write_blockers: [{ code: "trading_frozen" }] };
  assert.equal(orders.tradingCancelAllowed(frozen), true);
  assert.equal(orders.orderCancelGate(order, frozen).ready, true);
  assert.equal(orders.tradingCancelAllowed({ ...frozen, write_blockers: [{ code: "policy_blocked" }] }), false);
  assert.equal(orders.orderCancelGate(order, { ...frozen, account: "" }).ready, false);
});

test("TestAppJSQuoteErrorsRespectSnapshotMarketSession replacement distinguishes closed sessions from feed interruption", () => {
  reset();
  const closedCalendar = { session: { state: "closed", is_open: false } };
  assert.equal(stress.marketQuoteSessionClosed(closedCalendar), true);
  assert.equal(stress.marketQuoteSessionClosed({ session: { state: "unknown", is_open: false } }), false);
  assert.equal(stress.marketQuoteSessionClosed({ session: { state: "closed", is_open: true } }), false);
  const closed = stress.marketQuoteCell("SPY", null, {}, { errors: { SPY: "feed down" } }, closedCalendar);
  assert.equal(closed.children.at(-1).textContent, "Closed");
  assert.equal(closed.classList.contains("market-quote-cell--error"), false);
  assert.match(closed.children.at(-1).title, /session is closed/);
  const open = stress.marketQuoteCell("SPY", null, {}, { errors: { SPY: "feed down" } }, { session: { state: "regular", is_open: true } });
  assert.equal(open.children.at(-1).textContent, "Feed issue");
  assert.equal(open.classList.contains("market-quote-cell--error"), true);
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

test("TestAppJSStressDetailUsesSourceBackedEvidenceRows replacement ranks actual evidence and caps it at five", () => {
  reset();
  const rows = stress.stressDriverRows({ rows: [
    { title: "Portfolio stress", severity: "urgent" },
    { title: "Watch later", severity: "watch" },
    { title: "Urgent liquidity", severity: "urgent" },
    { title: "Data quality gap", direction: "data_quality" },
    { title: "Act now", severity: "act" },
    { title: "Concentration", direction: "defensive" },
    { title: "Sixth", severity: "watch" },
  ] });
  assert.equal(rows.length, 5);
  assert.deepEqual(rows.map((row) => row.title), ["Urgent liquidity", "Act now", "Data quality gap", "Concentration", "Watch later"]);
  assert.equal(stress.sourceHealthMentions({ source: "regime", notes: ["gamma cache unavailable"] }, "gamma"), true);
  assert.equal(stress.sourceHealthMentions({ source: "market_events", notes: [] }, "gamma"), false);
});

test("TestAppJSProtectionQuantityStepperAcceleratesAtTenBoundary replacement executes live click-time stepping", () => {
  reset();
  assert.deepEqual([
    protection.protectionQuantityStepDelta(9, 1),
    protection.protectionQuantityStepDelta(10, 1),
    protection.protectionQuantityStepDelta(10, -1),
    protection.protectionQuantityStepDelta(11, 1),
    protection.protectionQuantityStepDelta(20, -1),
  ], [1, 10, -1, 1, -10]);
  const proposal = { key: "synthetic-qty", revision: "r1", bucket: "risk_reduction", quantity: 10, max_quantity: 50, notional: 1000, contract: { currency: "EUR" } };
  const stepper = protection.protectionQuantityStepper(proposal);
  const [decrease, , increase] = stepper.children;
  assert.equal(decrease.getAttribute("aria-label"), "Decrease sell size");
  assert.equal(increase.getAttribute("aria-label"), "Increase sell size");
  increase.click();
  assert.equal(protection.protectionEffectiveQuantity(proposal), 20);
  increase.click();
  assert.equal(protection.protectionEffectiveQuantity(proposal), 30, "the second click must read the current override, not the original quantity");
});

test("TestUnderlyingWinnerLoserTotalsUseDailyPnl replacement ignores unrealized and quote-marked values", () => {
  reset();
  const group = {
    group_daily_pnl_base: 25,
    group_unrealized_pnl: 9999,
    stock: { daily_pnl_base: 10, unrealized_pnl_base: 8000 },
    options: [{ daily_pnl_base: 15, unrealized_pnl_base: 1999 }],
  };
  assert.deepEqual(underlyings.heldUnderlyingDailyPnl(group, "EUR", "USD"), { value: 25, currency: "EUR", source: "daily P/L" });
  assert.deepEqual(underlyings.heldUnderlyingDailyPnl({ stock: group.stock, options: group.options }, "EUR", "USD"), { value: 25, currency: "EUR", source: "daily P/L" });
  const totals = underlyings.underlyingHeldDailyPnlTotals([
    { pnl: 25, pnlCurrency: "EUR" },
    { pnl: -7, pnlCurrency: "EUR" },
    { pnl: 4000, pnlCurrency: "EUR", virtual: true },
  ], "EUR");
  assert.deepEqual(totals, { winner: 25, winnerCurrency: "EUR", loser: -7, loserCurrency: "EUR" });
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

test("TestAppJSProtectionSummaryUsesDataDrivenRiskTones replacement derives tone and no-stop value from served data", () => {
  reset();
  state.snapshot = { account: { base_currency: "EUR" }, positions: { stocks: [], options: [], portfolio: {}, protection_coverage: null }, stress: { portfolio: {} } };
  const proposals = {
    counts: {
      actionable: 2,
      base_currency: "EUR",
      theta_per_day_base: 42,
      risk_reduction: 1,
      risk_reduction_excess_notional_base: 1200,
    },
    proposals: [{ bucket: "trailing_stop", notional: 2500, contract: { currency: "EUR" } }],
  };
  protection.renderProtectionPanel(proposals, {}, {});
  assert.equal(dom.element("protectionTheta").classList.contains("metric-alert"), true);
  assert.equal(dom.element("protectionRiskExcess").classList.contains("metric-risk"), true);
  assert.equal(dom.element("protectionNoStopExposure").classList.contains("metric-alert"), true);
  assert.match(dom.element("protectionNoStopExposure").title, /sum of visible trailing-stop proposal notionals/);
  const noRows = coverage.protectionNoStopExposureSummary([], {}, null);
  assert.deepEqual(noRows, { text: "--", title: "No visible trailing-stop proposals without a matching open protective order.", risk: false });
});

test("TestAppJSRendersProtectionCoverageAndRiskTickets replacement executes risk-ticket, ladder, portfolio, and coverage renderers", () => {
  reset();
  state.snapshot = { account: { base_currency: "EUR" }, positions: { portfolio: {}, protection_coverage: { status: "ready", counts: { unprotected: 1 } } }, stress: { protection_coverage: { status: "unknown" } } };
  assert.equal(coverage.currentProtectionCoverage(), state.snapshot.positions.protection_coverage, "positions coverage is authoritative over stress fallback");
  const proposal = {
    bucket: "trailing_stop",
    execution_semantics: { reference_side: "bid", trigger_method_label: "last", price_guarantee: "stop_limit_can_leave_position_unfilled" },
    stop_risk: { estimated_loss_base: -100, base_currency: "EUR", gap_scenario: { gap_pct: 5, estimated_loss_base: -150 } },
    stop_ladder: [{ label: "current", stop_price: 90 }, { label: "proposed", stop_price: 95 }],
  };
  assert.deepEqual(protection.protectionRiskTicketParts(proposal, "trail 5%"), ["trail 5%", "trigger bid / last", "est. loss €100", "5.0% gap €150", "limit may not fill"]);
  const ticket = protection.protectionRiskTicket(proposal, "trail 5%");
  assert.equal(ticket.classList.contains("protection-row__risk-ticket"), true);
  assert.match(ticket.textContent, /trigger bid \/ last/);
  const ladder = protection.protectionStopLadder(proposal);
  assert.equal(ladder.getAttribute("aria-label"), "Stop ladder comparison");
  assert.equal(byClass(ladder, "protection-row__ladder-step").length > 0, true);

  const typedCoverage = {
    status: "ready",
    counts: { unprotected: 1, orphaned_order: 1 },
    by_underlying: [{ underlying: "AAA", state: "unprotected", unprotected_quantity: 2 }],
    orphaned_orders: [{ symbol: "BBB", remaining: 1, order_type: "STP", reconciliation_state: "orphaned_order" }],
  };
  assert.deepEqual(coverage.protectionCoverageDisplayRows(typedCoverage).map((row) => row.underlying), ["BBB", "AAA"]);
  assert.match(coverage.protectionCoverageDetailBody(typedCoverage, "EUR"), /Stale protective orders: BBB 1(?:\.00)? STP orphaned order/);
  const rows = portfolio.portfolioDetailRows({}, { stocks: [], options: [], protection_coverage: typedCoverage }, "EUR");
  assert.equal(rows.some((row) => row.label === "Protection coverage" && row.detail instanceof FakeElement), true);
  assert.match(stress.protectionCoverageStressLine({}, { account: { base_currency: "EUR" }, positions: { protection_coverage: typedCoverage } }), /Protection coverage:/);
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
  state.snapshot = { account: { base_currency: "EUR" } };
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

test("TestGovernanceRendererConsumesTypedAuthorities replacement renders only current allowlisted authority", () => {
  reset();
  state.snapshot = {
    sources: { nudges: { state: "current", reason: "authoritative", updated_at: "2026-07-01T12:00:00Z" } },
    nudges: {
      as_of: "2026-07-01T12:00:00Z",
      candidates: [{ title: "Synthetic current reminder", body: "Review synthetic evidence", severity: "watch", destination: "alerts" }],
      source_health: { aggregate: "ready" },
      confirmed_flow_coverage: { pre_cutover_flows_unreviewed: false },
    },
  };
  state.governance = { candidates: [], source_health: {}, poll_source: {}, occurrences: [], delivery_health: {}, diagnostic: {} };
  alerts.renderGovernance();
  assert.equal(dom.element("governanceCurrentState").textContent, "Needs you");
  assert.match(dom.element("governanceCurrentList").textContent, /Synthetic current reminder/);

  state.snapshot.sources.nudges = { state: "hostile-state", reason: "hostile-reason" };
  alerts.renderGovernance();
  assert.equal(dom.element("governanceCurrentState").textContent, "Unavailable");
  assert.equal(dom.element("governanceCurrentList").textContent.includes("Synthetic current reminder"), false);
  assert.equal(dom.element("governanceSourceHealth").textContent.includes("hostile"), false);
});

test("TestBriefCardStaticContract replacement renders production narrative, safe runs, fallback sections, and sign-off gating", () => {
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
    review: { one_tap: { report_id: "synthetic-report", signable: true, blockers: [] }, rules_delta: { status: "ok" } },
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
  const signoff = descendants(sections).find((node) => node.id === "briefSignoffButton");
  assert.ok(signoff);
  assert.match(signoff.title, /synthetic-report/);

  const fallback = {
    as_of: "2026-07-01T08:30:00Z",
    brief_fingerprint: "sha256:synthetic-fallback",
    review: { status: "ok", one_tap: { signable: false }, session_pnl: { daily_pnl_base: 5, base_currency: "EUR" } },
    ready: { status: "ok", monthly_pulse: { status: "not_due", month: "2026-07" }, artefacts: { rows: [] } },
  };
  state.snapshot = { brief: fallback, sources: {} };
  brief.renderBriefCard(state.snapshot);
  assert.equal(sections.classList.contains("brief-sections--narrative"), false);
  assert.deepEqual(sections.children.map((node) => byClass(node, "brief-section__head")[0]?.textContent), ["Reviewok", "Readyok"]);
  assert.equal(descendants(sections).some((node) => node.id === "briefSignoffButton"), false);
  assert.deepEqual(brief.briefAckBody({ stamp_target: "monthly", ready: { monthly_pulse: { month: "2026-07" } } }, "sha256:ack"), {
    kind: "monthly", brief_fingerprint: "sha256:ack", month: "2026-07", evidence: "render",
  });
});

// Breadth is a specific trading session's close, and the daemon keeps serving
// the last converged one when a refresh cannot reach its coverage threshold.
// The row rendered the percentages with nothing saying which day they came
// from, so a lane that had stopped producing looked current on the phone.
test("a refused market-data subscription is named on the app instead of reading as a vague quote fault", () => {
  reset();
  const snap = {
    status: {
      connected: true,
      subsystems: [{ name: "quote", status: "ready" }],
      market_data_access: [{ route_key: "SPX|IND|CBOE", symbol: "SPX", code: 354, reason: "not_subscribed" }],
    },
    market_quotes: { errors: { SPX: "quote.snapshot: symbol_inactive" } },
    sources: {},
  };

  // The cause is named, and the symbol's vaguer "quote unavailable" line is
  // dropped so one fault does not read as two.
  const labels = stress.marketSourceIssueLabels(snap);
  assert.deepEqual(labels, ["SPX not subscribed (IBKR 354)"]);

  // The gateway reading stays true about the link and stops being wrong
  // about the data.
  assert.equal(stress.gatewayDataStatus(snap), "Gateway live quotes OK; SPX not subscribed (IBKR 354)");

  // The Data quality remark surfaces it without any other source degrading.
  stress.renderRegimeQualityRemarks(snap, {});
  assert.equal(dom.element("regimeQualityRemarks").hidden, false);
  assert.equal(dom.element("regimeQualityText").textContent, "SPX not subscribed (IBKR 354)");

  // An unknown rejection code stays honest rather than claiming the
  // subscription is the cause.
  assert.equal(
    stress.marketAccessReasonLabel({ reason: "rejected", code: 322 }),
    "market data refused (IBKR 322)",
  );

  // A desk with nothing refused is unchanged: no label, no qualifier, and
  // an unrelated quote error still reports itself.
  const quiet = {
    status: { connected: true, subsystems: [{ name: "quote", status: "ready" }] },
    market_quotes: { errors: { SPY: "quote.snapshot: timeout" } },
    sources: {},
  };
  assert.deepEqual(stress.marketSourceIssueLabels(quiet), ["SPY quote timeout"]);
  assert.equal(stress.gatewayDataStatus(quiet), "Gateway live quotes OK");
  stress.renderRegimeQualityRemarks({ status: { connected: true }, sources: {} }, {});
  assert.equal(dom.element("regimeQualityRemarks").hidden, true);
});

test("brief breadth row shows the session it is reading and flags an overdue one", () => {
  reset();
  state.authenticated = true;
  state.activeTab = "monitor";
  const snapshot = {
    as_of: "2026-08-04T21:00:00Z",
    brief_fingerprint: "sha256:synthetic-breadth",
    review: { status: "ok", one_tap: { signable: false } },
    ready: {
      status: "ok",
      breadth: {
        status: "ok",
        detail: "S&P 500 constituent breadth · 2026-08-04 session",
        pct_above_50dma: 61.2, pct_above_200dma: 55, net_new_highs_pct: 0.8,
      },
    },
  };
  state.snapshot = { brief: snapshot, sources: {} };
  brief.renderBriefCard(state.snapshot);
  const breadthRow = () => byClass(dom.element("briefSections"), "brief-row")
    .find((node) => (byClass(node, "brief-row__head")[0]?.textContent || "").startsWith("Breadth"));

  let row = breadthRow();
  assert.ok(row, "breadth row must render");
  assert.match(byClass(row, "brief-row__detail")[0]?.textContent || "", /2026-08-04 session/);
  assert.match(byClass(row, "brief-row__head")[0]?.textContent || "", /ok$/);

  snapshot.ready.breadth = {
    status: "degraded",
    detail: "S&P 500 constituent breadth · 2026-08-03 session; a newer session is overdue",
    pct_above_50dma: 61.2, pct_above_200dma: 55, net_new_highs_pct: 0.8,
  };
  brief.renderBriefCard(state.snapshot);
  row = breadthRow();
  assert.match(byClass(row, "brief-row__detail")[0]?.textContent || "", /2026-08-03 session/);
  assert.match(byClass(row, "brief-row__detail")[0]?.textContent || "", /overdue/);
  assert.match(byClass(row, "brief-row__head")[0]?.textContent || "", /degraded$/);
  // An old close is still a real close: the badge changes, the reading stays.
  assert.match(byClass(row, "brief-row__value")[0]?.textContent || "", /61\.2/);
});

test("brief review row path serves the close capture and names a missing one", () => {
  reset();
  state.authenticated = true;
  state.activeTab = "monitor";
  const snapshot = {
    as_of: "2026-08-01T08:30:00Z",
    brief_fingerprint: "sha256:synthetic-capture",
    review: {
      status: "ok",
      one_tap: { signable: false },
      session_pnl: { daily_pnl_base: 5, base_currency: "EUR" },
      last_session: {
        status: "ok", detail: "close capture",
        session_date: "2026-07-31", daily_pnl_base: -433.7, base_currency: "EUR",
        session_close: "2026-07-31T20:00:00Z", captured_at: "2026-07-31T20:00:09Z",
      },
    },
    ready: { status: "ok", artefacts: { rows: [] } },
  };
  state.snapshot = { brief: snapshot, sources: {} };
  brief.renderBriefCard(state.snapshot);
  const captureRow = () => byClass(dom.element("briefSections"), "brief-row")
    .find((node) => (byClass(node, "brief-row__head")[0]?.textContent || "").startsWith("Last session close"));
  let row = captureRow();
  assert.ok(row, "close-capture row must render when the daemon serves it");
  const value = byClass(row, "brief-row__value")[0]?.textContent || "";
  assert.match(value, /2026-07-31/);
  assert.match(value, /Daily P\/L/);
  assert.match(value, /captured/);

  snapshot.review.last_session = { status: "unavailable", detail: "not captured", session_date: "2026-07-31" };
  brief.renderBriefCard(state.snapshot);
  row = captureRow();
  assert.match(byClass(row, "brief-row__value")[0]?.textContent || "", /not captured/);
  assert.doesNotMatch(byClass(row, "brief-row__value")[0]?.textContent || "", /Daily P\/L/);

  delete snapshot.review.last_session;
  brief.renderBriefCard(state.snapshot);
  assert.equal(captureRow(), undefined, "older daemons serve no last_session and must not grow an empty row");
});
