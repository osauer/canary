import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import test from "node:test";
import { withEdgeLearning } from "./edge-learning-fixture.mjs";
import { marketTapeFixture } from "./market-tape-fixture.mjs";

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
  "alerts", "alert-inbox", "brief", "chrome", "edge", "lifecycle", "market-events", "market-tape", "opportunities", "orders",
  "portfolio", "protection", "protection-coverage", "settings", "shared", "shell", "strategies", "stress", "underlyings",
  "update",
];
const modules = Object.fromEntries(await Promise.all(moduleNames.map(async (name) => [name, await import(`../${name}.js`)])));
const { alerts, brief, chrome, edge, lifecycle, opportunities, orders, portfolio, protection, settings, shared, shell, strategies, stress, underlyings, update } = modules;
const alertInbox = modules["alert-inbox"];
const coverage = modules["protection-coverage"];
const marketEvents = modules["market-events"];
const marketTape = modules["market-tape"];

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
    edgeResult: null, edgeBusy: false, edgeError: "", edgeRequestID: 0,
    marketTapeResult: null, marketTapeBusy: false, marketTapeError: "", marketTapeRequestID: 0, marketTapeIndex: 0,
    pairingRequired: false, connectionOK: false, connectionText: "Connecting", eventSource: null,
    readOnlyPreview: false, updateStatus: null, updatePollTimer: null, updateCompleteTimer: null,
    portfolioDetailOpen: false, protectionOpen: false, protectionQtyOverrides: {}, protectionQuoteTicks: {},
    protectionReviewOpen: {}, protectionCalculationsOpen: {},
    protectionPreviewBusy: "", protectionPreviews: {}, protectionSubmitBusy: "", protectionSubmits: {},
    proposalMarketCalendars: {}, proposalMarketCalendarBusy: {},
    selectedUnderlying: "", positionsSort: "impact",
    protectionSnapshotBusy: false, protectionSnapshotLastAt: 0, protectionSnapshotNotice: "",
    protectionDerisk: { percent: 25, busy: "", result: null, submitted: null, requestRef: "", previewedAt: 0, abort: null },
    protectionStopRequestBusy: "", protectionStopRequests: {},
    opportunitiesOpen: false, opportunityPreviewBusy: "", opportunityPreviews: {}, opportunitySubmitBusy: "", opportunitySubmits: {},
    strategyDrafts: {}, strategyPreviewBusy: "", strategyPreviews: {}, strategySubmitBusy: "", strategySubmits: {},
    opportunitySnapshotBusy: false, opportunitySnapshotLastAt: 0, opportunitySnapshotNotice: "",
    governance: null, governanceRefreshSucceeded: null, reconciliationCheck: { busy: false, state: "", error: false },
    safeNotificationTest: { busy: false, state: "", error: false }, pushInspection: { state: "unsupported", busy: false },
    dateFormatUpdate: { busy: false, state: "", error: false },
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

test("market tape rejects malformed evidence and never joins gaps", () => {
  reset();
  const good = marketTapeFixture();
  assert.equal(marketTape.validMarketTape(good), true);
  for (const mutate of [
    (r) => { r.not_predictive = false; },
    (r) => { r.sessions[0].date = "2026-99-99"; },
    (r) => { r.sessions[1].date = r.sessions[0].date; },
    (r) => { r.sessions[0].qqq.volume = "0"; },
    (r) => { r.sessions[0].spx.volume = 12; },
    (r) => { r.sessions[0].breadth.coverage_50 = 0; },
    (r) => { r.sources[0].key = "__proto__"; },
    (r) => { r.sessions[0].reading.evidence = "untyped advice"; },
  ]) {
    const bad = structuredClone(good); mutate(bad);
    assert.equal(marketTape.validMarketTape(bad), false);
  }
  assert.equal(marketTape.tapePath([1, 2, null, 0, 3], (i) => i, (v) => v), "M0.00,1.00 L1.00,2.00 M3.00,0.00 L4.00,3.00");
});

test("market tape retains zero, source clocks and safe text in the rendered session", async () => {
  reset();
  const result = marketTapeFixture();
  result.notes.push("<img src=x onerror=alert(1)>");
  result.sessions[3].reading.headline = "<img src=x onerror=alert(1)>";
  const requests = [];
  globalThis.fetch = async (path, options) => { requests.push({path, options}); return response(result); };
  assert.equal(await marketTape.refreshMarketTape(), true);
  assert.equal(requests[0].path, "/api/market-tape?sessions=20");
  assert.equal(requests[0].options.method, undefined);
  state.marketTapeIndex = 3;
  marketTape.renderMarketTape();
  assert.match(dom.element("marketTapeValues").textContent, /0\.00×/);
  assert.match(dom.element("marketTapeValues").textContent, /0 reported shares/);
  assert.equal(await marketTape.refreshMarketTape(), true);
  assert.equal(state.marketTapeIndex, 3, "refresh lost the selected session");
  assert.match(dom.element("marketTapeHeadline").textContent, /<img/);
  assert.equal(descendants(dom.element("marketTapeHeadline")).some((n) => n.tagName === "IMG"), false);
  state.marketTapeIndex = 4;
  marketTape.renderMarketTape();
  assert.match(dom.element("marketTapeValues").textContent, /Above 50-day—/);
  assert.ok(dom.element("marketTapeSources").textContent.includes(shared.calendarDateTime(result.sources[0].as_of, { timeZoneName: "short" })));
  assert.ok(dom.element("marketTapeReadAt").textContent.includes(shared.calendarDateTime(result.as_of, { timeZoneName: "short" })));
  assert.match(dom.element("marketTapeNotes").textContent, /<img/);
  assert.equal(descendants(dom.element("marketTapeNotes")).some((n) => n.tagName === "IMG"), false);
  assert.equal(byClass(dom.element("marketTapePlots"), "market-tape__plot").length, 4);
});

test("market tape ignores late replies and labels retained data after refresh failure", async () => {
  reset();
  const pending = [];
  globalThis.fetch = () => new Promise((resolve) => pending.push(resolve));
  const older = marketTape.refreshMarketTape();
  const newer = marketTape.refreshMarketTape();
  const latest = marketTapeFixture(); latest.as_of = "2026-09-24T11:00:00Z";
  pending[1](response(latest));
  assert.equal(await newer, true);
  pending[0](response(marketTapeFixture()));
  assert.equal(await older, false);
  assert.equal(state.marketTapeResult.as_of, latest.as_of);
  globalThis.fetch = async () => response("PRIVATE BROKER ERROR", 503);
  assert.equal(await marketTape.refreshMarketTape(), false);
  assert.match(dom.element("marketTapeReadAt").textContent, /showing the previous read/);
  assert.doesNotMatch(dom.element("marketTapeStatus").textContent, /PRIVATE/);
  state.authenticated = false;
  globalThis.fetch = () => assert.fail("unauthenticated tape acquisition");
  assert.equal(await marketTape.refreshMarketTape(), false);
});
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

test("primary navigation seats Edge and keeps Settings behind the header gear", async () => {
  const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
  assert.equal(normalizedTab("positions"), "positions");
  assert.equal(normalizedTab("edge"), "edge");
  assert.equal(normalizedTab("brief"), "monitor");
  assert.match(html, /id="tabPositions"[^>]*data-tab="positions"/);
  assert.match(html, /id="tabEdge"[^>]*data-tab="edge"/);
  assert.match(html, /id="settingsButton"[^>]*aria-label="Open Settings"/);
  assert.match(html, /id="dashboard"[^>]*data-tab-panel="monitor"[\s\S]*id="briefPanel"/);
  assert.doesNotMatch(html, /id="tabBrief"|id="briefTab"|id="tabSettings"|id="underlyingsSheet"|id="edgeWindow"|id="edgeHorizon"/);
  assert.doesNotMatch(html, /id="attentionStatus"/);
  assert.match(html, /One year of trading decisions/);
  assert.match(html, /Price outcomes after decisions/);
  assert.ok(html.indexOf('class="panel edge-impact"') < html.indexOf('class="panel edge-account"'), "decision insight must lead account P/L");
});

test("primary workspaces have a clear title and keep explanatory prose out of navigation", async () => {
  const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
  const headings = [
    ["dashboard", "monitorWorkspaceTitle", "Monitor"],
    ["positionsTab", "positionsGuideTitle", "Positions"],
    ["edgeTab", "edgeWorkspaceTitle", "Edge"],
    ["alertsTab", "alertsWorkspaceTitle", "Alerts"],
    ["ordersTab", "ordersWorkspaceTitle", "Orders"],
    ["settingsTab", "settingsWorkspaceTitle", "Settings"],
  ];
  for (const [panelID, titleID, title] of headings) {
    const panel = html.slice(html.indexOf(`id="${panelID}"`));
    const header = panel.slice(0, panel.indexOf("</header>"));
    assert.ok(header.includes(`aria-labelledby="${titleID}"`));
    assert.ok(header.includes(`<h2 id="${titleID}">${title}</h2>`));
  }
  assert.equal((html.match(/class="workspace-heading(?: workspace-heading--tab| positions-workspace__mast)(?: sr-only)?"/g) || []).length, 6);
  assert.match(html, /<details class="brief-disclosure"[^>]*>/);
  assert.doesNotMatch(html, /<details class="brief-disclosure"[^>]* open/);
  assert.ok(html.indexOf('id="briefSourceBanner"') < html.indexOf('class="brief-disclosure"'), "brief source warnings remain outside the disclosure");
});

test("Edge opens as an automatic one-year review and explains findings without trading controls", async () => {
  reset();
  const result = {
    schema_version: "canary-edge-v3",
    state: "current",
    as_of: "2026-08-24T12:00:00Z",
    window: "365d",
    horizon_sessions: 20,
    automatic_horizon: true,
    horizon_selection: { mode: "automatic", reason: "longest_adequately_covered", eligible_changes: 4, scored_changes: 4, coverage_pct: 100, largest_action_sample: 3, minimum_sample: 3, minimum_coverage_pct: 25, adequate: true },
    headline: "Adds had +EUR 125.00 observed impact at 20 sessions.",
    market_context: [
      { key: "spy", label: "S&P 500 proxy (SPY)", kind: "market_proxy", sample_count: 3, median_change_pct: 2.1 },
      { key: "qqq", label: "Nasdaq-100 proxy (QQQ)", kind: "market_proxy", sample_count: 3, median_change_pct: 3.2 },
      { key: "dia", label: "Dow proxy (DIA)", kind: "market_proxy", sample_count: 3, median_change_pct: 1.4 },
      { key: "vix", label: "CBOE VIX", kind: "volatility_index", sample_count: 3, median_change_pct: -8.5, median_change_points: -1.7 },
    ],
    market_context_missing: [],
    account: {
      base_currency: "EUR", requested_from: "2026-05-26T00:00:00Z", actual_from: "2026-05-27T00:00:00Z", actual_to: "2026-08-24T00:00:00Z",
      starting_equity_base: 1000, ending_equity_base: 1150, external_flows_base: 50, profit_loss_base: 100,
      definition: "Ending equity − starting equity − statement-confirmed external flows.",
    },
    action_rollups: ["open", "add", "trim", "exit"].map((action) => ({
      action,
      horizons: [1, 5, 20].map((sessions) => ({ sessions, sample_count: action === "add" ? 3 : 1, total_base: sessions, median_base: sessions })),
    })),
    findings: [{
      change_id: "change_safe", symbol: "SYN", action: "add", direction: "long", executed_at: "2026-07-01T14:00:00Z", horizon_sessions: 20,
      decision_notional_base: 2500, decision_impact_base: 125, decision_impact_pct: 5,
      market_context: [{ key: "spy", label: "S&P 500 proxy (SPY)", kind: "market_proxy", start_day: "2026-06-30T00:00:00Z", end_day: "2026-07-29T00:00:00Z", start_close: 600, end_close: 612, change_pct: 2 }],
    }],
    options: {
      coverage: { execution_episodes: 2, opening_episodes: 0, opening_only_zero_episodes: 0, closing_episodes: 2, mixed_episodes: 0, unknown_episodes: 0, event_episodes: 0 },
      realized: {
        known_pnl_base: 113, positive_count: 1, negative_count: 1, flat_count: 0, complete_count: 2, partial_count: 0, unavailable_count: 0, total_count: 2, truncated: false,
        episodes: [
          { id: "option_gain", grouping: "exact_order", lifecycle: "closing", underlying: "SYN", activity_from: "2026-06-15T14:00:00Z", activity_to: "2026-06-15T14:01:00Z", realized_pnl_base: 125, pnl_status: "complete", missing_evidence: [], legs: [{ symbol: "SYN CALL", underlying: "SYN", expiry: "2026-09-18", strike: 100, put_call: "call" }] },
          { id: "option_loss", grouping: "unlinked_execution", lifecycle: "closing", underlying: "SYN", activity_from: "2026-07-15T14:00:00Z", activity_to: "2026-07-15T14:00:00Z", realized_pnl_base: -12, pnl_status: "complete", missing_evidence: [], legs: [{ symbol: "SYN PUT", underlying: "SYN", expiry: "2026-10-16", strike: 90, put_call: "put" }] },
        ],
      },
      open: {
        snapshot_date: "2026-08-24T00:00:00Z", known_pnl_base: -25, positive_count: 0, negative_count: 1, flat_count: 0, complete_count: 1, unavailable_count: 0, total_count: 1, truncated: false,
        positions: [{ id: "option_open", symbol: "SYN CALL", underlying: "SYN", snapshot_date: "2026-08-24T00:00:00Z", expiry: "2026-11-20", strike: 105, put_call: "call", open_pnl_base: -25, pnl_status: "complete", missing_evidence: [] }],
      },
    },
    coverage: { trade_changes: 4, eligible_changes: 4, scored_by_horizon: { 1: 4, 5: 4, 20: 4 }, reason_counts: {}, present_sections: ["trades"], missing_sections: [] },
    method: {
      metric: "Decision price impact", counterfactual: "Leave the pre-trade position unchanged.", horizon_definition: "Available IBKR closes.",
      headline_selection: "Most clean observations at the selected horizon.", finding_ranking: "Absolute percentage, then absolute dollars, then opaque ID.",
      materiality_gate: "Account-relative finding gates.", automatic_horizon: "Longest adequately covered horizon.", market_context: "Informational benchmark paths only.",
      account_definition: "Ending equity − starting equity − external flows.", exclusions: "Distributions and financing.", options_method: "Broker-reported realized episodes and the dated open snapshot remain separate.",
      no_causal_claim: true, no_predictive_claim: true, not_investment_advice: true,
    },
    fingerprint: "edge_safe", not_execution: true,
  };
  withEdgeLearning(result);
  const change = {
    id: "change_safe", symbol: "SYN", asset_class: "stock", currency: "EUR", action: "add", direction: "long",
    executed_at: "2026-07-01T14:00:00Z", delta_quantity: 10, position_before: 20, position_after: 30,
    execution_vwap: 250, multiplier: 1, direct_costs_base: 2,
    scores: [
      { sessions: 1, horizon_day: "2026-07-02T00:00:00Z", horizon_close: 251, horizon_fx: 1, decision_notional_base: 2500, decision_impact_base: 8, decision_impact_pct: 0.32 },
      { sessions: 5, reason: "intervening_change" },
      { sessions: 20, horizon_day: "2026-07-29T00:00:00Z", horizon_close: 262.7, horizon_fx: 1, decision_notional_base: 2500, decision_impact_base: 125, decision_impact_pct: 5, market_context: [{ key: "spy", label: "S&P 500 proxy (SPY)", kind: "market_proxy", start_day: "2026-06-30T00:00:00Z", end_day: "2026-07-29T00:00:00Z", start_close: 600, end_close: 612, change_pct: 2 }] },
    ],
  };
  const option = {
    id: "option_loss", kind: "realized_episode",
    episode: {
      id: "option_loss", grouping: "unlinked_execution", lifecycle: "closing", underlying: "SYN", activity_from: "2026-07-15T14:00:00Z", activity_to: "2026-07-15T14:00:00Z",
      realized_pnl_base: -12, pnl_status: "complete", missing_evidence: [],
      legs: [{ id: "option-leg_loss", symbol: "SYN PUT", underlying: "SYN", expiry: "2026-10-16", strike: 90, put_call: "put", multiplier: 100, side: "sell", open_close: "closing", quantity: 2, execution_price: 3.25, currency: "USD", realized_pnl_base: -12, direct_costs_base: 1.5, missing_evidence: [] }],
    },
  };
  assert.equal(edge.validEdgeResult(result), true);
  assert.equal(edge.validEdgeResult({ ...result, patterns: [{ ...result.patterns[0], eligible_changes: -1 }] }), false);
  assert.equal(edge.validEdgeResult({ ...result, patterns: [{ ...result.patterns[0], horizons: [] }] }), false);
  assert.equal(edge.validEdgeResult({ ...result, change }), true);
  assert.equal(edge.validEdgeResult({ ...result, option }), true);
  assert.equal(edge.validEdgeResult({ ...result, change: { ...change, id: "broker-order" } }), false);
  assert.equal(edge.validEdgeResult({ ...result, options: { ...result.options, open: { ...result.options.open, known_pnl_base: 0, complete_count: 0, unavailable_count: 1, positions: [{ ...result.options.open.positions[0], open_pnl_base: null, pnl_status: "unavailable", missing_evidence: [] }] } } }), false);
  assert.equal(edge.validEdgeResult({ ...result, not_execution: false }), false);
  const requests = [];
  globalThis.fetch = async (url) => {
    requests.push(String(url));
    if (String(url).includes("change=change_safe")) return response({ ...result, change });
    if (String(url).includes("option=option_loss")) return response({ ...result, option });
    return response(result);
  };
  state.authenticated = true;
  assert.equal(await edge.refreshEdge(), true);
  assert.match(dom.element("edgeLearning").textContent, /Same decisions/);
  assert.match(dom.element("edgeLearning").textContent, /Monthly results/);
  assert.match(dom.element("edgeLearning").textContent, /Long adds/);
  assert.match(dom.element("edgeLearning").textContent, /not proof of skill/);
  assert.match(dom.element("edgeOptionCycles").textContent, /Completed option positions/);
  assert.match(dom.element("edgeOptionCycles").textContent, /SYN CALL/);
  assert.match(dom.element("edgeOptionCycles").children[4].children[0].textContent, /Partial/);
  assert.deepEqual(requests, ["/api/edge"]);
  assert.equal(dom.element("edgeImpactLens").textContent, "After 20 sessions");
  assert.match(dom.element("edgeMarketContext").textContent, /S&P 500 proxy \(SPY\) \+2\.10%/);
  state.edgeResult = { ...result, market_context: result.market_context.filter((row) => row.key !== "vix"), market_context_missing: ["vix"] };
  edge.renderEdge();
  assert.match(dom.element("edgeMarketContext").textContent, /CBOE VIX unavailable/);
  state.edgeResult = result;
  edge.renderEdge();
  assert.equal(dom.element("edgeAccountValue").textContent, "******");
  assert.equal(dom.element("edgeHeadline").textContent, "Account values hidden");
  const finding = byClass(dom.element("edgeFindings"), "edge-finding")[0];
  assert.equal(finding.tagName, "BUTTON");
  assert.equal(finding.getAttribute("aria-expanded"), "false");
  assert.equal(finding.textContent.includes("******"), true);
  finding.click();
  await waitFor(() => requests.length === 2 && !state.edgeBusy, "finding detail did not load");
  assert.equal(requests[1], "/api/edge?change=change_safe");
  assert.equal(dom.element("edgeChangePanel").hidden, false);
  assert.match(dom.element("edgeChangeSummary").textContent, /20 → 30/);
  assert.equal(byClass(dom.element("edgeChangeScores"), "edge-change-score").length, 3);
  assert.match(dom.element("edgeChangeScores").textContent, /Intervening Change/);
  assert.match(dom.element("edgeChangeScores").textContent, /\*\*\*\*\*\*/);
  state.accountValueVisible = true;
  edge.renderEdge();
  assert.match(dom.element("edgeAccountValue").textContent, /100/);
  assert.equal(dom.element("edgeHeadline").textContent, result.headline);
  assert.match(dom.element("edgeChangeSummary").textContent, /€250\.00/);
  assert.match(dom.element("edgeChangeScores").textContent, /\+€125\.00/);
  assert.match(dom.element("edgeChangeScores").textContent, /S&P 500 proxy \(SPY\) \+2\.00%/);
  const resultButtons = descendants(dom.element("edgeFindings")).filter((node) => node.tagName === "BUTTON");
  assert.equal(resultButtons.length, 1);
  assert.equal(resultButtons.every((node) => node.classList.contains("edge-finding") && node.type === "button"), true, "Edge may expose explanation buttons only");
  byClass(dom.element("edgeFindings"), "edge-finding")[0].click();
  assert.equal(dom.element("edgeChangePanel").hidden, true, "tapping the expanded finding collapses its explanation");
  assert.equal(requests.length, 2, "collapsing a finding should not issue another read");
  assert.equal(byClass(dom.element("edgeOptionRealizedList"), "edge-option-row").length, 2);
  assert.equal(byClass(dom.element("edgeOptionOpenList"), "edge-option-row").length, 1);
  assert.match(dom.element("edgeOptionRealizedSummary").textContent, /known broker realized P\/L/);
  assert.match(dom.element("edgeOptionOpenSummary").textContent, /known broker open P\/L/);
  const optionRow = byClass(dom.element("edgeOptionRealizedList"), "edge-option-row")[1];
  assert.equal(optionRow.tagName, "BUTTON");
  optionRow.click();
  await waitFor(() => requests.length === 3 && !state.edgeBusy, "option detail did not load");
  assert.equal(requests[2], "/api/edge?option=option_loss");
  assert.equal(dom.element("edgeOptionPanel").hidden, false);
  assert.match(dom.element("edgeOptionDetailSummary").textContent, /Broker realized P\/L-€12\.00/);
  assert.match(dom.element("edgeOptionDetailLegs").textContent, /qty 2/);
  assert.match(dom.element("edgeOptionDetailLegs").textContent, /at \$3\.25/);
  assert.match(dom.element("edgeOptionDetailLegs").textContent, /Costs \+€1\.50/);
  optionRow.click();
  assert.equal(dom.element("edgeOptionPanel").hidden, true, "tapping the expanded option collapses its evidence");
  assert.equal(requests.length, 3, "collapsing option evidence should not issue another read");
  const unavailableOpen = {
    ...result,
    options: {
      ...result.options,
      open: { snapshot_date: result.options.open.snapshot_date, positive_count: 0, negative_count: 0, flat_count: 0, complete_count: 0, unavailable_count: 1, total_count: 1, truncated: true, positions: [] },
    },
  };
  assert.equal(edge.validEdgeResult(unavailableOpen), true);
  state.edgeResult = unavailableOpen;
  edge.renderEdge();
  assert.match(dom.element("edgeOptionOpenSummary").textContent, /No numeric broker open P\/L/);
  assert.match(dom.element("edgeOptionOpenSummary").textContent, /1 unavailable/);
  assert.doesNotMatch(dom.element("edgeOptionOpenSummary").textContent, /[+$€]0\.00/);
  assert.match(dom.element("edgeOptionOpenList").textContent, /unavailable positions remain counted/);

  const confirmedEmptyOpen = {
    ...result,
    options: {
      ...result.options,
      open: { snapshot_date: result.options.open.snapshot_date, positive_count: 0, negative_count: 0, flat_count: 0, complete_count: 0, unavailable_count: 0, total_count: 0, truncated: false, positions: [] },
    },
  };
  assert.equal(edge.validEdgeResult(confirmedEmptyOpen), true);
  state.edgeResult = confirmedEmptyOpen;
  edge.renderEdge();
  assert.notEqual(dom.element("edgeOptionOpenAsOf").textContent, "No dated snapshot");
  assert.match(dom.element("edgeOptionOpenList").textContent, /0 open options as of/);

  const missingOpenSnapshot = {
    ...confirmedEmptyOpen,
    options: { ...confirmedEmptyOpen.options, open: { ...confirmedEmptyOpen.options.open, snapshot_date: undefined } },
  };
  assert.equal(edge.validEdgeResult(missingOpenSnapshot), true);
  state.edgeResult = missingOpenSnapshot;
  edge.renderEdge();
  assert.equal(dom.element("edgeOptionOpenAsOf").textContent, "No dated snapshot");
  assert.match(dom.element("edgeOptionOpenList").textContent, /No dated Flex open-position snapshot is available/);
});

test("Edge renders every authority state without turning missing evidence into results", () => {
  reset();
  const base = {
    schema_version: "canary-edge-v3",
    state: "current",
    window: "90d",
    horizon_sessions: 20,
    automatic_horizon: true,
    horizon_selection: { mode: "automatic", reason: "snapshot_unavailable", eligible_changes: 0, scored_changes: 0, coverage_pct: 0, largest_action_sample: 0, minimum_sample: 3, minimum_coverage_pct: 25, adequate: false },
    market_context: [],
    market_context_missing: [],
    action_rollups: [],
    findings: [],
    options: {
      coverage: { execution_episodes: 0, opening_episodes: 0, opening_only_zero_episodes: 0, closing_episodes: 0, mixed_episodes: 0, unknown_episodes: 0, event_episodes: 0 },
      realized: { positive_count: 0, negative_count: 0, flat_count: 0, complete_count: 0, partial_count: 0, unavailable_count: 0, total_count: 0, truncated: false, episodes: [] },
      open: { positive_count: 0, negative_count: 0, flat_count: 0, complete_count: 0, unavailable_count: 0, total_count: 0, truncated: false, positions: [] },
    },
    coverage: { trade_changes: 0, eligible_changes: 0, scored_by_horizon: {}, reason_counts: {} },
    method: { metric: "Decision price impact", account_definition: "Ending equity − starting equity − external flows.", no_causal_claim: true, no_predictive_claim: true, not_investment_advice: true },
    not_execution: true,
  };

  state.edgeResult = {
    ...base,
    state: "action_required",
    reason: "query_field_missing",
    setup: { manifest_version: "edge-flex-v1", steps: ["Create query.", "Add fields.", "Save credentials."], sections: ["trades"], missing_requirements: ["trades.ibOrderID", "open_positions.markPrice"] },
  };
  edge.renderEdge();
  assert.equal(dom.element("edgeSetup").hidden, false);
  assert.equal(dom.element("edgeResults").hidden, true);
  assert.equal(dom.element("edgeSetupSteps").children.length, 3);
  assert.match(dom.element("edgeSetupMissing").textContent, /trades\.ibOrderID/);
  assert.equal(dom.element("edgeSetupMissing").hidden, false);
  assert.match(dom.element("edgeStatus").textContent, /Flex evidence setup is required/);

  state.edgeResult = { ...base, state: "backfilling", reason: "statement_backfill_paced" };
  edge.renderEdge();
  assert.equal(dom.element("edgeSetup").hidden, true);
  assert.equal(dom.element("edgeResults").hidden, true);
  assert.match(dom.element("edgeStatus").textContent, /Backfill is running/);

  state.edgeResult = { ...base, state: "degraded", reason: "newer_evidence_pending", fingerprint: "edge_prior_snapshot" };
  edge.renderEdge();
  assert.equal(dom.element("edgeResults").hidden, false, "a degraded last-good publication remains visible");
  assert.match(dom.element("edgeStatus").textContent, /prior snapshot is visible/);
  assert.equal(dom.element("edgeStatus").classList.contains("edge-status--risk"), true);

  state.edgeResult = {
    ...base,
    state: "insufficient_evidence",
    reason: "trade_history_unproved",
    fingerprint: "edge_account_only",
    account: { requested_from: "2025-08-24", actual_from: "2025-08-25", actual_to: "2026-08-24", profit_loss_base: 10, external_flows_base: 0, definition: "Ending equity minus starting equity minus external flows." },
    setup: { manifest_version: "canary-reporting-flex-v1", steps: ["Open the saved query.", "Confirm Trades.", "Validate the corrected query; no Edge parameters or debug export."], sections: [{ key: "trades", label: "Trades", fields: ["tradePrice"] }] },
  };
  edge.renderEdge();
  assert.equal(dom.element("edgeSetup").hidden, false, "the existing setup panel explains the terminal evidence gap");
  assert.equal(dom.element("edgeSetupTitle").textContent, "Trade history was not returned");
  assert.equal(dom.element("edgeResults").hidden, false, "proved account evidence remains visible");
  assert.match(dom.element("edgeStatus").textContent, /completed one-year report returned no Trades section/);
  assert.match(dom.element("edgeStatus").textContent, /not a backfill/);
  assert.doesNotMatch(dom.element("edgeStatus").textContent, /waiting|retry automatically/);
  assert.doesNotMatch(dom.element("edgeStatus").textContent, /Trade History Unproved/);
  assert.match(dom.element("edgeSetupReason").textContent, /finished the one-year report and is not waiting/);
  assert.match(dom.element("edgeSetupReason").textContent, /Trades is selected at execution detail/);
  assert.match(dom.element("edgeSetupSteps").textContent, /no Edge parameters or debug export/);
  assert.equal(dom.element("edgeStatus").classList.contains("edge-status--risk"), true);

  state.edgeResult = { ...base, state: "unavailable", reason: "snapshot_authority_unavailable" };
  edge.renderEdge();
  assert.equal(dom.element("edgeResults").hidden, true);
  assert.match(dom.element("edgeStatus").textContent, /No sound Edge result is currently available/);
});

test("display date format offers US and European dates with optional weekdays", () => {
  reset();
  const expected = {
    us: "Aug 18, 2026",
    eu: "18 Aug 2026",
    us_weekday: "Tuesday, Aug 18, 2026",
    eu_weekday: "Tuesday, 18 Aug 2026",
  };
  for (const [mode, label] of Object.entries(expected)) {
    state.settings = { kind: "ibkr.platform_settings", display: { date_format: { value: mode, access: "write", source: "runtime" } } };
    assert.equal(shared.dateFormatMode(), mode);
    assert.equal(shared.calendarDate("2026-08-18"), label);
  }
  state.settings = null;
  assert.equal(shared.dateFormatMode(), "us");
  assert.equal(shared.calendarDate("2026-08-18"), expected.us);
});

test("Settings date format uses the typed platform-settings patch and repaints", async () => {
  reset();
  const served = (value) => ({
    kind: "ibkr.platform_settings",
    as_of: "2026-08-18T12:00:00Z",
    display: { date_format: { value, access: "write", source: "runtime" } },
    features: { stock_protection: { enabled: { value: true, access: "write", source: "runtime" } } },
  });
  state.settings = served("us");
  let request = null;
  globalThis.fetch = async (url, init = {}) => {
    request = { url: String(url), method: init.method, body: JSON.parse(init.body) };
    return response(served("eu_weekday"));
  };
  assert.equal(await settings.setDateFormat("eu_weekday"), true);
  assert.deepEqual(request, { url: "/api/settings", method: "PATCH", body: { display: { date_format: "eu_weekday" } } });
  assert.equal(state.settings.display.date_format.value, "eu_weekday");
  assert.equal(state.dateFormatUpdate.state, "Date format saved.");
  assert.equal(renderCount > 0, true);

  reset();
  state.readOnlyPreview = true;
  state.settings = served("us");
  globalThis.fetch = async () => { throw new Error("read-only preview must not write"); };
  assert.equal(await settings.setDateFormat("eu"), true);
  assert.equal(state.settings.display.date_format.value, "eu");
  assert.equal(state.dateFormatUpdate.state, "Preview only · not saved.");
});

test("Positions is performance-first while typed risk and guarded actions stay behind selection", async () => {
  reset();
  const html = await readFile(new URL("../index.html", import.meta.url), "utf8");
  assert.match(html, /<h2 id="positionsGuideTitle">Positions<\/h2>/);
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
  assert.equal(byClass(rendered, "underlying-row__metric--pnl")[0].textContent.includes("Daily P/L"), true);
  assert.equal(byClass(rendered, "underlying-row__metric--open")[0].textContent.includes("Unrealized P/L"), true);
  assert.match(byClass(rendered, "position-inspector__distinction")[0].textContent, /Color shows direction, not an instruction/);
  assert.deepEqual(byClass(rendered, "position-inspector__action").map((button) => button.dataset.positionAction), ["strategy", "trim"]);
  const mark = underlyings.underlyingBookRow({ ...row, price: 0, priceSource: "account mark" }, "EUR");
  assert.match(byClass(mark, "underlying-row__metric--quote")[0].textContent, /Account mark/);
  assert.match(byClass(mark, "underlying-row__metric--quote")[0].textContent, /0[.,]00/);
  const current = { ...state.snapshot.positions, authority: { availability: "available", freshness: "current", as_of: new Date().toISOString() } };
  underlyings.renderUnderlyings(current, {});
  assert.equal(dom.element("underlyingLoserPnl").textContent, "None");
  underlyings.renderUnderlyings({ ...current, by_underlying: [{ underlying: "MISSING", stock: { currency: "USD" }, options: [] }] }, {});
  assert.equal(dom.element("underlyingLoserPnl").textContent, "Unavailable");

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
  const ruleGroup = new FakeElement("details");
  ruleGroup.append(rule);
  const originalQuery = dom.document.querySelectorAll;
  dom.document.querySelectorAll = (selector) => selector === "[data-rule-id]" ? [rule] : selector === ".is-alert-evidence-target" && rule.classList.contains("is-alert-evidence-target") ? [rule] : [];
  alertInbox.openAlertEvidence({ presentation_code: "rulebook_hedge_integrity" });
  assert.equal(rule.classList.contains("is-alert-evidence-target"), true);
  assert.equal(rule.getAttribute("aria-current"), "location");
  assert.equal(rule.open, true, "an alert opens the selected rule's evidence");
  assert.equal(ruleGroup.open, true, "an alert opens a collapsed result group");
  const rerenderedRule = stress.ruleChecklistRow({ id: "hedge_integrity", number: 12, title: "Protection assignment", status: "act", evidence: "Directional short." });
  assert.equal(rerenderedRule.classList.contains("is-alert-evidence-target"), true, "selected Rulebook target survives a render");
  assert.equal(rerenderedRule.getAttribute("aria-current"), "location");
  dom.document.querySelectorAll = originalQuery;

  alertInbox.openAlertEvidence({ presentation_code: "regime_market_stress" });
  assert.equal(dom.element("regimeDetailPanel").classList.contains("is-alert-evidence-target"), true);
  assert.equal(dom.element("regimeDetailPanel").getAttribute("aria-current"), "location");
  dom.element("briefDisclosure").open = false;
  alertInbox.openAlertEvidence({ presentation_code: "risk_policy_drawdown_latched" });
  assert.equal(dom.element("briefDisclosure").open, true, "alert evidence must open the collapsed brief");
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

test("Rules keep configuration, applicability, and incomplete measurements distinct", () => {
  reset();
  assert.equal(stress.ruleGroupKey({ mode: "alert", status: "watch" }), "attention");
  assert.equal(stress.ruleGroupKey({ mode: "track", status: "act" }), "monitor");
  assert.equal(stress.ruleGroupKey({ mode: "track", status: "unknown" }), "unknown");
  assert.equal(stress.ruleGroupKey({ status: "not_evaluated", reason: "earnings_not_applicable" }), "not_applicable");
  assert.equal(stress.ruleGroupKey({ status: "not_evaluated", reason: "pnl_unavailable" }), "not_evaluated");
  assert.equal(stress.ruleGroupKey({ mode: "off", status: "not_evaluated", reason: "rule_off" }), "off");
  assert.equal(stress.ruleGroupKey({ mode: "alert", status: "act", reason: "rule_off" }), "attention", "a conflicting reason cannot hide an active result");
  const monitoring = stress.ruleChecklistRow({ id: "monitor", title: "Cash reserve", mode: "track", status: "act", evidence: "Reserve is below its threshold." });
  assert.equal(monitoring.classList.contains("neutral"), true);
  assert.equal(byClass(monitoring, "rules-row__status")[0].textContent, "Act level");
  assert.equal(byClass(monitoring, "rules-row__mode")[0].textContent, "Monitor only");
  const unknown = stress.ruleChecklistRow({ id: "partial", title: "Exposure", mode: "alert", status: "unknown", observed: 17.3, threshold: 40, unit: "% NLV", evidence: "Exposure is incomplete because delta is missing." });
  assert.equal(unknown.classList.contains("ok"), false);
  assert.equal(byClass(unknown, "rules-row__status")[0].textContent, "Unknown");
  assert.equal(byClass(unknown, "rules-row__meter").length, 0, "an incomplete result must not imply a valid progress-to-limit comparison");
  const minimum = stress.ruleChecklistRow({
    id: "minimum", title: "Exposure", mode: "alert", status: "act", observed: 42, observed_is_lower_bound: true,
    threshold: 40, unit: "% NLV", evidence: "At least 42% of NLV; some deltas are missing.",
    offenders: [{ symbol: "SYN" }, { symbol: "ALT" }, { symbol: "THR" }, { symbol: "FOUR" }],
    exempt: [{ symbol: "EXEMPT", note: "Exact-contract exemption." }], notes: ["Partial inputs remain incomplete."],
  });
  assert.match(minimum.textContent, /Observed minimum≥ 42(?:\.0+)?% NLV/);
  assert.match(minimum.textContent, /FOUR/);
  assert.match(minimum.textContent, /Exact-contract exemption/);
  assert.match(minimum.textContent, /Partial inputs remain incomplete/);
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
  const snap = { updated_at: "2026-08-12T05:00:00Z", status: {data_health:{schema_version:1,summary:{state:"current",label:"Required data meets its stated use",current:2,required:2}}}, regime: { source_health: [{ source: "gamma", status: "ok" }] } };
  stress.renderLampTest(snap, { source_health: [{ source: "positions", status: "ok" }] });
  assert.equal(dom.element("lampTest").hidden, true);

  stress.renderLampTest(snap, { source_health: [{ source: "positions", status: "ok" }] }, "Dealer gamma assessment unavailable");
  assert.equal(dom.element("lampTest").hidden, false, "an unavailable assessment remains visible even when source counters are healthy");
  assert.match(dom.element("lampTestStamp").textContent, /Dealer gamma assessment unavailable/);

  snap.status.data_health.summary = {state:"limited",label:"Gamma stale",current:1,required:2};
  stress.renderLampTest(snap, { source_health: [{ source: "positions", status: "ok" }] });
  assert.equal(dom.element("lampTest").hidden, false);
  assert.match(dom.element("lampTestStamp").textContent, /gamma.*stale/i);
});

test("lamp test translates the internal alert-candidate feed into operator meaning", () => {
  const health = stress.lampTestSources({
    sources: { alert_candidates: { state: "unavailable", error: "producer unavailable" } },
  }, {});
  assert.deepEqual(health.faults, ["Canary data health unverified", "alert checking unavailable — current Alerts unconfirmed"]);
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

test("source health uses Canary's counts and retains missing authority as unknown", () => {
  reset();
  const snap = { updated_at: "2026-08-13T19:00:00Z", status: { data_health: {
    schema_version: 1, summary: { state: "limited", label: "2 data problems · 1 unverified", current: 2, required: 5, problems: 2, unverified: 1 },
  } }, regime: { source_health: [{ source: "unknown", status: "" }] } };
  const health = stress.lampTestSources(snap, { source_health: [{ source: "other", status: "ok" }] });
  assert.equal(health.total, 5);
  assert.equal(health.ok, 2);
  assert.deepEqual(health.faults, ["2 data problems · 1 unverified"]);
  stress.renderLampTest(snap, {});
  assert.match(dom.element("lampTestStamp").textContent, /2\/5 sources ok/);
  const legacy = stress.lampTestSources({ regime: { source_health: [{ source: "missing", status: "" }] } }, {});
  assert.equal(legacy.ok, 0);
  assert.deepEqual(legacy.faults, ["Canary data health unverified"]);
});

test("data-health pages preserve delayed evidence and reject incomplete coverage", async () => {
  reset();
  const health = await import("../data-health.js");
  const originalFetch = globalThis.fetch;
  const source = {id:"ibkr:quotes",name:"IBKR quote feed",state:"limited",receiving:"Delayed · last session",data_type:"delayed-frozen",source_time_kind:"unknown",access:{code:354,reason:"not_subscribed",retry_at:"2026-09-15T08:00:00Z"}};
  const report = {schema_version:1,revision:"synthetic",as_of:"2026-09-15T06:00:00Z",valid_until:"2026-09-15T06:01:00Z",summary:{total:1,label:"1 data problem · 0 unverified"},sources:[source],offset:0,complete:true};
  try {
    globalThis.fetch = async () => response(report);
    await health.refreshDataHealth();
    assert.match(dom.element("dataHealthSources").textContent,/IBKR quote feed — Delayed · last session/);
    assert.match(dom.element("dataHealthSources").textContent,/Source time: unknown/);
    assert.match(dom.element("dataHealthReceipt").textContent,/last Canary assessment/);
    globalThis.fetch = async () => response({...report,summary:{...report.summary,total:2}});
    await health.refreshDataHealth();
    assert.match(dom.element("dataHealthSources").textContent,/IBKR quote feed — Delayed · last session/);
    assert.match(dom.element("dataHealthReceipt").textContent,/current health is unconfirmed/);
  } finally { globalThis.fetch=originalFetch; }
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
    push_delivery: {
      last_sent: null, last_alert_sent_at: null, silent_since: null, last_displayed: null, last_opened: null,
      witnessed: false, intake_rejected_since: null, subscription_expired_at: null, active_subscriptions: 0,
    },
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
  assert.equal(dom.element("underlyingWinnerPnl").textContent, "Unavailable", "stale rows must not publish a clean or current P/L summary");
});

function protectionActionFixture() {
  const proposal = {
    key: "synthetic-protection", revision: "synthetic-revision", state: "generated",
    symbol: "SYN", bucket: "trailing_stop", action: "SELL", quantity: 2, max_quantity: 4,
    contract: { sec_type: "STK", currency: "USD" },
  };
  state.protectionOpen = true;
  state.proposalMarketCalendars.us = { market: "us", session: { is_open: true } };
  state.snapshot = {
    trading: { can_preview: true, can_write: true, account: "SYNTHETIC-PAPER", mode: "paper" },
    positions: {
      stocks: [{ symbol: "SYN", con_id: 42, quantity: 4 }], options: [],
      portfolio: { dollar_delta_base: 400 },
      protection_coverage: { by_underlying: [{ underlying: "SYN", state: "unprotected" }] },
    },
    proposals: { revision: "synthetic-revision", proposals: [proposal], counts: { total: 1, actionable: 1 } },
    auto_trade: {}, market_events: {},
  };
  return proposal;
}

test("pre-authorised rows show the countdown and a veto that follows the daemon record", () => {
  reset();
  const proposal = protectionActionFixture();
  proposal.automatic = { pre_authorised: true, bucket: "trailing_stop", state: "pending", submit_at: "2026-09-21T14:30:00Z", veto_window: "30m0s" };
  protection.renderProtectionPanel(state.snapshot.proposals);
  const rows = dom.element("protectionRows");
  const [veto] = byClass(rows, "protection-veto");
  assert.ok(veto, "a pending automatic record offers a veto");
  assert.equal(veto.disabled, false);
  assert.match(veto.title, /hold lasts until the proposal changes/);
  const [automatic] = byClass(rows, "protection-row__automatic");
  assert.match(automatic.textContent, /Canary places this itself at .* unless you veto/);
  assert.equal(protection.protectionVetoAvailable(proposal), true);

  // Latched brake: no window, the copy says so, the veto still shows while pending.
  proposal.automatic.latch_skipped_window = true;
  assert.match(protection.protectionAutomaticText(proposal), /placing this now.*brake is latched/);

  // Held by the settling rule: still pending, the copy names the hand order,
  // and the veto stays available.
  proposal.automatic = { pre_authorised: true, state: "pending", submit_at: "2026-09-21T14:30:00Z", held_at: "2026-09-21T14:30:30Z" };
  assert.match(protection.protectionAutomaticText(proposal), /^Held while an order placed or modified outside Canary since this proposal appeared/);
  assert.equal(protection.protectionVetoAvailable(proposal), true);

  // Deferred by the freeze: not failed, resubmits once lifted, veto available.
  proposal.automatic = { pre_authorised: true, state: "deferred", deferred_at: "2026-09-21T14:30:00Z", resubmit_at: "2026-09-21T14:31:00Z" };
  assert.match(protection.protectionAutomaticText(proposal), /^Deferred by the trading freeze/);
  assert.equal(protection.protectionVetoAvailable(proposal), true);

  // Terminal states name the outcome and offer no veto.
  for (const [state_, pattern] of [["vetoed", /^Vetoed at/], ["submitted", /^Placed by Canary at/], ["failed", /did not place this: trading_frozen/], ["superseded", /superseded/]]) {
    proposal.automatic = { pre_authorised: true, state: state_, vetoed_at: "2026-09-21T14:10:00Z", submitted_at: "2026-09-21T14:30:00Z", reason: "trading_frozen: frozen" };
    assert.match(protection.protectionAutomaticText(proposal), pattern, state_);
    assert.equal(protection.protectionVetoAvailable(proposal), false, state_);
  }
  // A pre-authorised row without a record is told what will happen; an
  // ordinary row says nothing about automation.
  proposal.automatic = { pre_authorised: true, bucket: "trailing_stop" };
  assert.match(protection.protectionAutomaticText(proposal), /^Pre-authorised: Canary places this itself once the row is unblocked/);
  assert.equal(protection.protectionVetoAvailable(proposal), false);
  proposal.automatic = { pre_authorised: false, bucket: "trailing_stop" };
  assert.equal(protection.protectionAutomaticText(proposal), "");
  delete proposal.automatic;
  protection.renderProtectionPanel(state.snapshot.proposals);
  assert.equal(byClass(dom.element("protectionRows"), "protection-veto").length, 0, "no record, no veto button");
});

test("read-only preview disables the veto like every other protection action", () => {
  reset();
  const proposal = protectionActionFixture();
  proposal.automatic = { pre_authorised: true, state: "pending", submit_at: "2026-09-21T14:30:00Z" };
  state.readOnlyPreview = true;
  protection.renderProtectionPanel(state.snapshot.proposals);
  const [veto] = byClass(dom.element("protectionRows"), "protection-veto");
  assert.ok(veto);
  assert.equal(veto.disabled, true);
  assert.match(veto.title, /Read-only preview/);
});

test("read-only protection explains and disables stop, repair, ignore, and portfolio trim actions", () => {
  reset();
  const proposal = protectionActionFixture();
  state.readOnlyPreview = true;
  state.protectionPreviews[protection.protectionPreviewStateKey(proposal)] = { submit_eligible: true };
  state.protectionDerisk.result = { eligible_count: 1, legs: [{ symbol: "SYN", action: "SELL", reduce_quantity: 1, submit_eligible: true }] };
  state.protectionDerisk.previewedAt = Date.now();
  try {
    protection.renderProtectionPanel(state.snapshot.proposals);
    assert.equal(dom.element("protectionReadOnlyBadge").hidden, false);
    const row = dom.element("protectionRows");
    for (const className of ["protection-preview", "protection-submit", "protection-ignore"]) {
      const [button] = byClass(row, className);
      assert.ok(button, `${className} must remain visible`);
      assert.equal(button.disabled, true, `${className} must respect browser permissions even when trading is ready`);
      assert.match(button.title, /Read-only preview.*paired Canary app/);
    }
    const [repair] = byClass(dom.element("protectionCoverageRepair"), "protection-repair__request");
    assert.equal(repair.disabled, true);
    assert.match(repair.title, /Read-only preview/);
    assert.equal(dom.element("protectionDeriskPercent").disabled, true);
    assert.equal(dom.element("protectionDeriskPreview").disabled, true);
    assert.match(dom.element("protectionDeriskPreview").title, /Read-only preview/);
    const [submit] = byClass(dom.element("protectionDeriskBasket"), "protection-derisk__submit");
    assert.equal(submit.disabled, true, "retained eligible baskets cannot enable submission in read-only mode");
    assert.match(submit.title, /Read-only preview/);
    assert.match(dom.element("protectionDeriskState").textContent, /Read-only preview.*paired Canary app/);

    for (const positions of [{ stocks: [] }, { stocks: [{ con_id: 42, quantity: 4 }] }]) {
      state.snapshot.positions = positions;
      protection.renderProtectionDerisk();
      assert.match(dom.element("protectionDeriskState").textContent, /Read-only preview/,
        "the browser restriction must stay clear when holdings or delta are unavailable");
    }
  } finally {
    protection.cancelProtectionDerisk();
  }
});

test("protection context distinguishes missing theta inputs, closed-session context, and no reduction proposals", () => {
  reset();
  protectionActionFixture();
  const proposals = state.snapshot.proposals;
  Object.assign(proposals.counts, { theta_hygiene: 0, risk_reduction: 0 });
  Object.assign(state.snapshot.positions.portfolio, { greeks_total: 1, greeks_coverage: 0 });
  state.snapshot.positions.options = [{ warning_details: [{ code: "options_closed" }] }];
  protection.renderProtectionPanel(proposals);
  assert.equal(dom.element("protectionTheta").textContent, "Unavailable");
  assert.match(dom.element("protectionThetaNote").textContent, /Options market closed/);
  assert.equal(dom.element("protectionRiskExcess").textContent, "No risk reduction proposed");

  state.snapshot.positions.options[0].warning_details = [];
  delete proposals.counts.risk_reduction;
  protection.renderProtectionPanel(proposals);
  assert.doesNotMatch(dom.element("protectionThetaNote").textContent, /market closed/,
    "a missing input alone must not imply that the options session is closed");
  assert.equal(dom.element("protectionRiskExcess").textContent, "Unavailable",
    "an absent proposal count must not imply that no reduction was proposed");

  Object.assign(proposals.counts, { risk_reduction: 1, theta_per_day_base: 12, base_currency: "EUR" });
  protection.renderProtectionPanel(proposals);
  assert.equal(dom.element("protectionRiskExcess").textContent, "Review");
  assert.match(dom.element("protectionTheta").textContent, /12/);
  assert.doesNotMatch(dom.element("protectionThetaNote").textContent, /unavailable|closed/i,
    "a served theta total takes priority over unavailable per-leg detail");
  proposals.counts.risk_reduction_excess_notional_base = 0;
  protection.renderProtectionPanel(proposals);
  assert.match(dom.element("protectionRiskExcess").textContent, /0/,
    "a served zero remains a measured amount rather than missing data");
});

test("protection consolidates a staged stop only with current, unambiguous held-contract and account evidence", () => {
  reset();
  const proposal = protectionActionFixture();
  proposal.contract.con_id = 42;
  const positions = state.snapshot.positions;
  positions.authority = { availability: "available", freshness: "current", scope: { account_id: "SYNTHETIC-PAPER", account_mode: "paper" } };
  Object.assign(state.snapshot.proposals, { account_id: "SYNTHETIC-PAPER", account_mode: "paper" });
  const row = positions.protection_coverage.by_underlying[0];
  assert.equal(protection.protectionRepairProposal(row), proposal);
  protection.renderProtectionPanel(state.snapshot.proposals);
  assert.equal(dom.element("protectionCoverageRepair").hidden, true);
  assert.match(byClass(dom.element("protectionRows"), "protection-row__status")[0].textContent, /No active stop-loss · Proposal staged/);

  for (const [field, value] of [["account_id", "SYNTHETIC-OTHER"], ["account_mode", "live"]]) {
    const saved = state.snapshot.proposals[field];
    state.snapshot.proposals[field] = value;
    assert.equal(protection.protectionRepairProposal(row), null, "cross-account evidence must not collapse a coverage row");
    state.snapshot.proposals[field] = saved;
  }
  positions.authority.freshness = "stale";
  assert.equal(protection.protectionRepairProposal(row), null);
  positions.authority.freshness = "current";
  positions.stocks.push({ symbol: "SYN", con_id: 43, quantity: 1 });
  assert.equal(protection.protectionRepairProposal(row), null, "symbol ambiguity must remain visible");
  positions.stocks.pop();
  proposal.contract.con_id = 43;
  assert.equal(protection.protectionRepairProposal(row), null);
  proposal.contract.con_id = 42;
  proposal.contract.sec_type = "OPT";
  assert.equal(protection.protectionRepairProposal(row), null, "an option stop cannot repair stock coverage");
  protection.renderProtectionCoverageRepair();
  assert.equal(dom.element("protectionCoverageRepair").hidden, false);

  // Consolidated rows must not consume the visible repair limit and hide the
  // next holding that still needs a proposal.
  const staged = Array.from({ length: 6 }, (_, index) => ({
    ...proposal, key: `staged-${index}`, symbol: `SYN${index}`,
    contract: { con_id: index + 1, sec_type: "STK", currency: "USD" },
  }));
  state.snapshot.proposals.proposals = staged;
  positions.stocks = staged.map((p) => ({ symbol: p.symbol, con_id: p.contract.con_id, quantity: 1 }));
  positions.protection_coverage.by_underlying = [
    ...staged.map((p) => ({ underlying: p.symbol, state: "unprotected" })),
    { underlying: "NEEDS", state: "unprotected" },
  ];
  protection.renderProtectionCoverageRepair();
  assert.match(dom.element("protectionCoverageRepair").textContent, /NEEDS/);
  assert.equal(byClass(dom.element("protectionCoverageRepair"), "protection-repair__row").length, 1);
});

test("stop review disclosure retains execution risk and fallback evidence without requesting a broker preview", () => {
  reset();
  const proposal = protectionActionFixture();
  Object.assign(proposal, {
    tif: "GTC", trail: { initial_stop_price: 90, trailing_percent: 10 },
    trail_sizing: { chosen_pct: 10, selected_by: "policy_default", fallback: true },
    execution_semantics: { reference_side: "bid", trigger_method_label: "last", price_guarantee: "stop_price_is_not_execution_price" },
    stop_risk: { estimated_loss_base: 20, base_currency: "EUR", gap_scenario: { gap_pct: 5, estimated_loss_base: 30 } },
    stop_ladder: [{ kind: "fixed_5pct", stop_price: 95, estimated_loss_base: 10 }, { kind: "policy_chosen", stop_price: 90, estimated_loss_base: 20 }],
  });
  const requests = [];
  globalThis.fetch = async (...args) => { requests.push(args); return response({}); };
  const review = protection.protectionRow(proposal);
  assert.equal(review.tagName, "DETAILS");
  assert.equal(review.open, false);
  assert.match(byClass(review, "protection-row__summary")[0].textContent, /Stop 90.00 USD/);
  assert.match(byClass(review, "protection-row__summary")[0].textContent, /Preset stop distance/);
  const details = byClass(review, "protection-row__review")[0];
  assert.match(byClass(details, "protection-review__execution")[0].textContent, /Trigger: bid \/ last.*fill price can differ/);
  assert.match(byClass(details, "protection-review__facts")[0].textContent, /Estimated loss at stop.*€20.*5.0% gap.*€30/);
  const calculations = byClass(details, "protection-row__calculations")[0];
  assert.equal(calculations.open, false);
  assert.match(calculations.textContent, /Volatility data unavailable; using the configured fallback/);
  assert.match(protection.protectionCompactMetric({ ...proposal, trail: { initial_stop_price: 90, trailing_amount: 10 } }), /10.0% initial distance/,
    "a currency trail sized from a percentage is not a native percentage trail");
  assert.equal(byClass(calculations, "protection-row__ladder")[0].tagName, "TABLE");
  review.open = true;
  review.dispatchEvent({ type: "toggle" });
  calculations.open = true;
  calculations.dispatchEvent({ type: "toggle" });
  const refreshed = protection.protectionRow(proposal);
  assert.equal(refreshed.open, true);
  assert.equal(byClass(refreshed, "protection-row__calculations")[0].open, true);
  assert.deepEqual(requests, [], "reading a proposal must not call the order path");
  proposal.blockers = [{ code: "synthetic_blocker", message: "Synthetic hard blocker" }];
  const blocked = protection.protectionRow(proposal);
  assert.match(byClass(blocked, "protection-row__summary")[0].textContent, /Synthetic hard blocker/);
  assert.equal(byClass(blocked, "protection-preview")[0].disabled, true);
});

test("portfolio trim opens its own sheet and keeps submission dependent on a basket preview", () => {
  reset();
  protectionActionFixture();
  chrome.setProtectionSheetOpen(true);
  assert.equal(dom.element("protectionSheet").open, true);
  chrome.setPortfolioTrimSheetOpen(true);
  assert.equal(dom.element("protectionSheet").open, false);
  assert.equal(dom.element("portfolioTrimSheet").open, true);
  assert.equal(byClass(dom.element("protectionDeriskBasket"), "protection-derisk__submit").length, 0);
  chrome.setProtectionSheetOpen(true);
  assert.equal(dom.element("portfolioTrimSheet").open, false);
  assert.equal(dom.element("protectionSheet").open, true);
});

test("protection handlers reject stale action controls after switching to read-only and keep snapshot reads available", async () => {
  reset();
  const proposal = protectionActionFixture();
  const previewKey = protection.protectionPreviewStateKey(proposal);
  state.protectionPreviews[previewKey] = { submit_eligible: true };
  state.protectionDerisk.result = { eligible_count: 1 };
  assert.equal(protection.protectionPreviewSubmitGate(proposal, state.protectionPreviews[previewKey]).ready, true);
  const requests = [];
  globalThis.fetch = async (url, init = {}) => {
    requests.push(`${init.method || "GET"} ${url}`);
    return response(state.snapshot.proposals);
  };
  state.readOnlyPreview = true;
  await protection.previewProtectionProposal(proposal);
  await protection.submitProtectionProposal(proposal);
  await protection.requestProtectionStop({ underlying: "SYN" });
  await protection.ignoreProtectionProposal(proposal);
  await protection.previewProtectionDerisk();
  await protection.submitProtectionDerisk();
  assert.deepEqual(requests, [], "read-only action handlers must return before making any request");
  assert.equal(state.protectionPreviewBusy, "");
  assert.equal(state.protectionSubmitBusy, "");
  assert.equal(state.protectionDerisk.busy, "");
  await protection.refreshProtectionProposals();
  assert.deepEqual(requests, ["GET /api/proposals"], "read-only refresh uses the snapshot read route");
});

test("paired protection retains broker preview and submit eligibility gates", async () => {
  reset();
  const proposal = protectionActionFixture();
  assert.equal(protection.protectionPreviewGate(proposal).ready, true);
  assert.equal(protection.protectionPreviewSubmitGate(proposal).ready, false, "submission still requires a preview");
  const requests = [];
  globalThis.fetch = async (url, init = {}) => {
    requests.push({ url, method: init.method, body: JSON.parse(init.body) });
    return response({ submit_eligible: true, preview: { what_if: { status: "accepted" } } });
  };
  await protection.previewProtectionProposal(proposal);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "/api/proposals/preview");
  assert.equal(requests[0].method, "POST");
  assert.equal(requests[0].body.key, proposal.key);
  assert.equal(requests[0].body.revision, proposal.revision);
  assert.equal(requests[0].body.quantity, proposal.quantity);
  const [submit] = byClass(dom.element("protectionRows"), "protection-submit");
  assert.equal(submit.disabled, false, "a paired, eligible preview still exposes submission");
  state.snapshot.trading.can_write = false;
  protection.renderProtectionPanel(state.snapshot.proposals);
  assert.equal(byClass(dom.element("protectionRows"), "protection-submit")[0].disabled, true);
  state.snapshot.trading.can_preview = false;
  assert.equal(protection.protectionPreviewGate(proposal).ready, false);
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
    review: { last_session: { session_date: "2026-06-26" }, rules: { status: "ok", pass: 10, watch: 0, act: 0, unknown: 0 } },
    ready: { stress: { severity: "watch" } },
  };
  state.snapshot = { brief: narrative, sources: { brief: {} } };
  brief.renderBriefCard(state.snapshot);
  const sections = dom.element("briefSections");
  assert.equal(sections.classList.contains("brief-sections--narrative"), true);
  assert.deepEqual(byClass(sections, "pd-placard").map((node) => node.textContent), ["Friday's close → next openwatch", "Review", "Ready"]);
  assert.match(dom.element("briefAsOf").textContent, /^Jul 1, 2026 · /);
  state.settings = { kind: "ibkr.platform_settings", display: { date_format: { value: "eu_weekday" } } };
  brief.renderBriefCard(state.snapshot);
  assert.match(dom.element("briefAsOf").textContent, /^Wednesday, 1 Jul 2026 · /);
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


test("Regime option context preserves quality, horizon differences and safe prose", () => {
  reset();
  const cards = stress.regimeGammaDetails([{
    underlying: "SPX", rankability: "blocked", data_type: "frozen", rankability_reason: "prior session",
    interpretation: "Modeled hedging amplifies moves <img src=x onerror=boom>",
    skew_interpretation: "Put-minus-call IV +3.0 vol points; not a directional forecast.",
    horizons: [{ horizon: "0dte", regime: "short_gamma" }, { horizon: "term", regime: "long_gamma" }],
    directional_inference: "Bullish/bearish positioning is unknown.",
  }]);
  assert.equal(cards.length, 1);
  const text = cards[0].textContent;
  assert.match(text, /blocked.*frozen/);
  assert.match(text, /0dte: short gamma.*term: long gamma/);
  assert.match(text, /Bullish\/bearish positioning is unknown/);
  assert.ok(text.includes("<img src=x onerror=boom>"));
  const hasImage = (node) => node.tagName === "IMG" || (node.children || []).some(hasImage);
  assert.equal(hasImage(cards[0]), false);
  const retained = stress.regimeGammaDetails([{ underlying: "SPX", rankability: "rankable" }], { degraded: true, status: "stale" });
  assert.match(retained[0].textContent, /Last-known SPX.*Retained observation; regime authority stale/);
});

test("Brief overview preserves served priority, privacy, coverage and expandable evidence", () => {
  reset();
  state.authenticated = true;
  state.accountValueVisible = false;
  state.snapshot = { brief: {
    narrative: {
      lead: [{ text: "Full explanation" }],
      overview: {
        assessment: [{ text: "Assessment incomplete." }],
        attention: [{ runs: [{ text: "Capital", role: "watch", topic: "capital" }, { text: " secret amount", account_sensitive: true }] }],
        context: [{ runs: [{ text: "Breadth observed yesterday <img src=x>" }] }],
        coverage: [{ runs: [{ text: "Portfolio unavailable; VVIX source degraded" }] }],
      },
    },
    review: { rules: { status: "degraded", unknown: 1 } },
    ready: { market_events: [{ kind: "halt", status: "ok", count: 0 }] },
  }, sources: { positions: { error: "positions unavailable", state: "unavailable" } } };
  brief.renderBriefCard(state.snapshot);
  const sections = dom.element("briefSections");
  assert.deepEqual(byClass(sections, "pd-placard").map(n => n.textContent), ["Needs review", "Context", "Coverage gaps"]);
  assert.match(sections.textContent, /Assessment incomplete/);
  assert.match(sections.textContent, /VVIX source degraded/);
  assert.doesNotMatch(sections.textContent, /secret amount/);
  assert.equal(descendants(sections).some(n => n.tagName === "IMG"), false);
  const details = byClass(sections, "brief-full-details")[0];
  assert.equal(details.tagName, "DETAILS");
  assert.notEqual(details.open, true);
  assert.match(details.textContent, /Held-name events require an available positions snapshot/);
  assert.equal(byClass(sections, "brief-topic-link")[0].textContent, "Capital");
  details.open = true;
  brief.renderBriefCard(state.snapshot);
  assert.equal(byClass(sections, "brief-full-details")[0].open, true, "snapshot refresh must preserve expanded evidence");
});

test("delayed quotes retain origin and never use receipt time as quote time", () => {
  reset();
  const quote = { price: 100, price_source: "prev_close", data_type: "prev_close", feed_type: "delayed-frozen", as_of: "2026-09-15T05:34:00Z" };
  assert.equal(shared.quoteTimestamp(quote), "");
  const row = { price: 100, priceAt: quote.as_of, quote };
  assert.equal(underlyings.underlyingQuoteStatus(row).label, "Delayed · last session");
  const rendered = underlyings.underlyingBookRow({ ...row, symbol: "SYNTH", marketFlags: [] }, "USD");
  assert.match(byClass(rendered, "underlying-row__metric--quote")[0].textContent, /Delayed · last session/);
  assert.match(underlyings.underlyingQuoteStatus(row).title, /quote time unknown/);
  assert.equal(underlyings.underlyingQuoteStatus({ ...row, quote: { ...quote, stale: true } }).label, "Delayed · stale");
  assert.match(stress.marketQuoteSourceLine(quote, { as_of: quote.as_of }), /Delayed · last session.*quote time unknown/);
  assert.doesNotMatch(stress.marketQuoteSourceLine(quote, { as_of: quote.as_of }), /05:34/);
  assert.match(stress.marketAccessReasonLabel({ code: 354, reason: "not_subscribed", fallback_data_type: "delayed-frozen" }), /live access unavailable; delayed last-session data in use/);
});

test("push delivery proof validates device receipts and names the silence", () => {
  reset();
  const now = "2026-09-26T07:00:00Z";
  const freshUntil = "2099-09-26T07:10:00Z";
  const silentSince = "2026-08-11T13:42:47Z";
  const push = {
    last_sent: { at: silentSince, kind: "alert", class: "push_service_accepted", http_status: 0, accepted: true },
    last_alert_sent_at: silentSince, silent_since: silentSince, last_displayed: null, last_opened: null,
    witnessed: false, intake_rejected_since: "2026-08-15T01:53:52Z", subscription_expired_at: null, active_subscriptions: 3,
  };
  const feed = {
    schema_version: "alerts-v1", version: "alert-delivery-v4", initialized: true, generation: 1,
    as_of: now, current_state: "clear",
    coverage: { state: "complete", freshness: "current", as_of: now, expected_sources: ["canary"], covered_sources: ["canary"] },
    sources: [{ source: "canary", status: "current", reason: "authoritative", evidence_health: "current", input_as_of: now, observed_at: now, evidence_as_of: now, fresh_until: freshUntil, covered: true }],
    occurrences: [], attention: { unread_count: 0, high_water_seq: 0, read_through_seq: 0, unread_refs: [] },
    delivery_health: { state: "healthy", class: "", updated_at: now, last_push_service_acceptance_at: silentSince },
    push_delivery: push,
  };
  assert.equal(alertInbox.validateAlerts(feed), feed);
  for (const [name, mutate] of [
    ["acceptance counted as a witness", (value) => { value.push_delivery.witnessed = true; }],
    ["unknown notice kind", (value) => { value.push_delivery.last_sent.kind = "reminder"; }],
    ["impossible HTTP status", (value) => { value.push_delivery.last_sent.http_status = 42; }],
    ["extra key", (value) => { value.push_delivery.endpoint = "https://web.push.apple.com/private"; }],
    ["missing proof", (value) => { delete value.push_delivery; }],
  ]) {
    const value = JSON.parse(JSON.stringify(feed));
    mutate(value);
    assert.throws(() => alertInbox.validateAlerts(value), alertInbox.AlertContractError, name);
  }

  state.alerts = feed;
  state.alertsFeedValid = true;
  alertInbox.renderAlerts();
  const unwitnessed = dom.element("alertDeliveryAcceptance").textContent;
  assert.match(unwitnessed, /does not prove the phone displayed it/);
  assert.match(unwitnessed, /No device has confirmed a push yet/);
  assert.match(unwitnessed, /No alert push since/);
  assert.match(unwitnessed, /Alert intake has been refused since/);

  const opened = { at: now, kind: "diagnostic", device: "iPhone" };
  state.alerts = { ...feed, push_delivery: { ...push, last_opened: opened, witnessed: true, intake_rejected_since: null } };
  assert.equal(alertInbox.validateAlerts(state.alerts), state.alerts);
  alertInbox.renderAlerts();
  const witnessed = dom.element("alertDeliveryAcceptance").textContent;
  assert.match(witnessed, /Last device receipt: opened on iPhone at .* \(test\)\./);
  assert.doesNotMatch(witnessed, /No device has confirmed/);
});

test("a notification launch repeats only a well-formed opened receipt", async () => {
  reset();
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, init) => { calls.push({ url: String(url), init }); return response({ recorded: false }); };
  try {
    assert.equal(await alerts.acknowledgeNoticeOpened("diagnostic-0123456789abcdef"), true);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, "/api/push/ack");
    assert.equal(calls[0].init.method, "POST");
    assert.equal(calls[0].init.credentials, "include");
    const body = JSON.parse(calls[0].init.body);
    assert.equal(body.notice_id, "diagnostic-0123456789abcdef");
    assert.equal(body.event, "opened");
    assert.ok(Number.isFinite(Date.parse(body.at)));
    for (const hostile of ["../api/devices", "https://evil.example", "", null, "A-UPPER"]) {
      assert.equal(await alerts.acknowledgeNoticeOpened(hostile), false);
    }
    assert.equal(calls.length, 1);
  } finally { globalThis.fetch = originalFetch; }
});

test("a safe test with no subscription on this device says what to do", async () => {
  reset();
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => response({ state: "no_subscription", push_service_accepted: false });
    assert.equal(await alerts.sendSafeNotificationTest(), false);
    assert.match(state.safeNotificationTest.state, /not subscribed to push/);
    assert.equal(state.safeNotificationTest.error, true);
    globalThis.fetch = async () => response({ state: "push_service_accepted", push_service_accepted: true, notice_id: "diagnostic-0123456789abcdef" });
    assert.equal(await alerts.sendSafeNotificationTest(), true);
    assert.match(state.safeNotificationTest.state, /Tap the notification/);
    assert.equal(state.safeNotificationTest.error, false);
  } finally { globalThis.fetch = originalFetch; }
});
