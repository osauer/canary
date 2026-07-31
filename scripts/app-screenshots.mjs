#!/usr/bin/env node
// app-screenshots.mjs — regenerate the published Canary app screenshots
// (docs/social/canary-app-{mobile,desktop}.png) from the real PWA with
// synthetic account figures, so no real account data reaches the pixels.
//
// The page is served by a live `canary app` and paired exactly like
// app-browser-smoke.mjs (helpers shared via lib-app-browser.mjs); an init
// script then wraps fetch and EventSource and rewrites every
// account-bearing payload (bootstrap JSON, SSE account/snapshot/positions
// events) before the SPA sees it: account ids become the documentation
// placeholder DU1234567 and the summary figures become fixture values.
// By default market strip data (SPY/VIX/QQQ) stays real — it is public.
// `--synthetic` instead patches a complete embedded monitor snapshot after
// pairing, so every published panel is deterministic without a gateway.
//
// Usage: node scripts/app-screenshots.mjs [--synthetic]
//        [--base-url http://127.0.0.1:8765] [--out-dir docs/social]
//        [--browser chromium]

import { writeFile } from "node:fs/promises";
import path from "node:path";
import { createPairingSession, launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

const args = parseArgs(process.argv.slice(2));
const baseURL = (args["base-url"] || "http://127.0.0.1:8765").replace(/\/+$/, "");
const outDir = args["out-dir"] || "docs/social";
const browserName = args.browser || "chromium";
const synthetic = args.synthetic === "true";
if (!synthetic && path.resolve(outDir) === path.resolve("docs/social")) {
  throw new Error("refusing to write published screenshots without --synthetic");
}

// Documentation placeholder id (allowlisted by check-no-account-data) and
// deliberately tidy fixture figures — recognizable as demo data.
const FIXTURE = {
  accountID: "DU1234567",
  netLiquidation: 1_250_000,
  buyingPower: 4_800_000,
  dailyPnl: 2_340,
};

const SYNTHETIC_SNAPSHOT = buildSyntheticSnapshot();

const SHOTS = [
  {
    name: "canary-app-mobile.png",
    viewport: { width: 390, height: 844 },
    isMobile: true,
  },
  {
    name: "canary-app-desktop.png",
    viewport: { width: 1180, height: 760 },
    isMobile: false,
  },
];

const playwright = loadPlaywright("app-screenshots");
if (!playwright[browserName]) {
  console.error(`app-screenshots: unknown browser ${browserName}`);
  process.exit(2);
}

const { browser } = await launchBrowser(playwright[browserName], browserName, { headless: true });
const pendingSyntheticShots = [];
try {
  for (const shot of SHOTS) {
    // Pairing nonces are single-use; mint one per context.
    const pairing = await createPairingSession(baseURL, baseURL);
    const context = await browser.newContext({
      viewport: shot.viewport,
      deviceScaleFactor: 2,
      isMobile: shot.isMobile,
      hasTouch: shot.isMobile,
    });
    if (synthetic) {
      await context.addInitScript(() => {
        globalThis.__canarySmoke = {};
      });
    }
    await context.addInitScript(installFixtureRewrite, FIXTURE);
    const page = await context.newPage();
    await page.goto(pairing.url, { waitUntil: "domcontentloaded", timeout: 15000 });
    await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
    if (synthetic) {
      // The dashboard is revealed before the EventSource connection is made.
      // Let its mandatory initial snapshot land, then stop later live events
      // from replacing individual synthetic panels after the patch.
      await page.waitForFunction(
        () => globalThis.__canarySmoke?.initialSnapshotSeen === true,
        undefined,
        { timeout: 15000 },
      );
      const applied = await page.evaluate((snapshot) => {
        const apply = globalThis.__canarySmoke?.applySnapshotPatch;
        if (typeof apply !== "function") {
          throw new Error("synthetic snapshot patch hook is unavailable");
        }
        globalThis.__canarySmoke.freezeLiveEvents = true;
        return apply(snapshot);
      }, SYNTHETIC_SNAPSHOT);
      if (applied !== true) {
        throw new Error("synthetic snapshot patch hook did not confirm the patch");
      }
    }
    // Balances and the account id are privacy-masked by default (the id now
    // masks to U•••••NN under the same eye toggle); the published shots show the
    // (fixture) figures, so reveal them first — and the id-visible assertion
    // below depends on the id being unmasked.
    await page.locator("#accountPrivacyToggle").click();
    await page.waitForFunction(() => {
      const text = document.getElementById("netLiquidation")?.textContent?.trim();
      return Boolean(text) && text !== "******" && text !== "--";
    }, { timeout: 5000 });
    // With balances (and the id) revealed, assert the fixture actually took:
    // the fixture id is visible and no real id leaked.
    await page.waitForFunction(
      (id) => document.body.innerText.includes(id),
      FIXTURE.accountID,
      { timeout: 15000 },
    );
    if (synthetic) {
      await assertSyntheticRender(page);
      await page.evaluate(() => {
        document.querySelector(".shell")?.scrollTo(0, 0);
        globalThis.scrollTo(0, 0);
      });
      await page.waitForFunction(() => {
        const shell = document.querySelector(".shell");
        return (!shell || shell.scrollTop === 0) && globalThis.scrollY === 0;
      }, undefined, { timeout: 5000 });
    }
    const leaked = await page.evaluate(
      (id) => /\bD?U\d{6,9}\b/.test(document.body.innerText.replaceAll(id, "")),
      FIXTURE.accountID,
    );
    if (leaked) {
      throw new Error("an account-id-shaped string other than the fixture is visible; aborting");
    }
    const out = path.join(outDir, shot.name);
    if (synthetic) {
      pendingSyntheticShots.push({
        out,
        png: await page.screenshot(),
        viewport: shot.viewport,
      });
      // Brief-tab shot: the daily brief now lives on its own tab, so the
      // published gallery gains a second synthetic frame showing it.
      await page.locator("#tabBrief").click();
      // Either render is a valid frame: the row sections, or the daemon's
      // composed narrative when the fixture serves one.
      await page.waitForFunction(() => {
        const sections = document.getElementById("briefSections");
        return Boolean(document.getElementById("briefTab")?.hidden === false &&
          (sections?.querySelector(".brief-section") || sections?.querySelector(".pd-brf-lead")));
      }, { timeout: 5000 });
      pendingSyntheticShots.push({
        out: out.replace(/\.png$/i, "-brief.png"),
        png: await page.screenshot(),
        viewport: shot.viewport,
      });
      await page.locator("#tabMonitor").click();
      await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });
    } else {
      await page.screenshot({ path: out });
      console.log(`app-screenshots: wrote ${out} (${shot.viewport.width}x${shot.viewport.height}@2x)`);
    }
    await context.close();
  }
  for (const shot of pendingSyntheticShots) {
    await writeFile(shot.out, shot.png);
    console.log(`app-screenshots: wrote ${shot.out} (${shot.viewport.width}x${shot.viewport.height}@2x)`);
  }
} finally {
  await browser.close();
}

async function assertSyntheticRender(page) {
  const expectedHeadings = ["Review", "Ready"];
  const expectedNetLiquidation = compactFixtureMoney(FIXTURE.netLiquidation, "EUR");

  // The daily brief lives on its own bottom tab now: switch to it, assert the
  // two process movements render, then return to Monitor for the hero shot.
  await page.locator("#tabBrief").click();
  await page.waitForSelector("#briefTab:not([hidden])", { timeout: 5000 });
  await page.waitForSelector("#briefPanel:not([hidden])", { timeout: 5000 });
  // Both movements must render, in order, in whichever register the fixture
  // serves: row section headings, or the narrative's movement placards.
  await page.waitForFunction(
    (headings) => {
      const sections = [...document.querySelectorAll("#briefSections .brief-section__head h3")]
        .map((heading) => heading.textContent?.trim());
      if (sections.length === headings.length && sections.every((heading, index) => heading === headings[index])) return true;
      const placards = [...document.querySelectorAll("#briefSections .pd-placard")].map((placard) => placard.textContent?.trim() || "");
      return headings.every((heading) => placards.some((placard) => placard.startsWith(heading)));
    },
    expectedHeadings,
    { timeout: 5000 },
  );
  await page.locator("#tabMonitor").click();
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 5000 });

  await page.waitForFunction(
    (expected) => document.getElementById("netLiquidation")?.textContent?.trim() === expected,
    expectedNetLiquidation,
    { timeout: 5000 },
  );
  await page.waitForSelector("#marketQuoteStrip .market-quote-cell:not(.market-quote-cell--missing)", {
    state: "visible",
    timeout: 5000,
  });
  // The underlyings book moved into a tap-through sheet, so the Monitor face
  // carries only the movers row. Open the sheet from that row to prove the
  // synthetic book renders, then close it: the published frame is the
  // glance-only Monitor, not an overlay.
  await page.waitForSelector("#moversRow:not([hidden]) b", { state: "visible", timeout: 5000 });
  await page.locator("#moversRow").click();
  await page.waitForSelector("#underlyingsSheet[open] #underlyingBookListPanel:not([hidden]) .underlying-row", {
    state: "visible",
    timeout: 5000,
  });
  await page.locator("#underlyingsSheetClose").click();
  await page.waitForFunction(() => !document.getElementById("underlyingsSheet")?.open, undefined, { timeout: 5000 });
}

function compactFixtureMoney(value, currency) {
  const prefix = currency ? `${currency} ` : "";
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${prefix}${(value / 1_000_000).toFixed(abs >= 10_000_000 ? 1 : 2)}m`;
  if (abs >= 100_000) return `${prefix}${(value / 1_000).toFixed(0)}k`;
  if (abs >= 10_000) return `${prefix}${(value / 1_000).toFixed(1)}k`;
  return `${prefix}${new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value)}`;
}

function buildSyntheticSnapshot() {
  const second = 1000;
  const minute = 60 * second;
  const hour = 60 * minute;
  const day = 24 * hour;
  const anchor = Date.now() - 4 * second;
  const at = (offset = 0) => new Date(anchor + offset).toISOString();
  const dateAt = (offset = 0) => at(offset).slice(0, 10);
  const expiryAt = (offset) => dateAt(offset).replaceAll("-", "");
  const asOf = at();
  const quoteAt = at(-second);
  const baseCurrency = "EUR";
  const accountID = FIXTURE.accountID;
  const fx = 0.92;
  const fingerprint = (version, key) => ({ version, key });
  const sourceHealth = (source) => ({
    source,
    status: "ok",
    as_of: asOf,
    confidence: "high",
    fingerprint_stability: "semantic_buckets_only",
  });
  const compactSourceHealth = (source) => ({ source, status: "ok", as_of: asOf, confidence: "high" });

  const trading = {
    mode: "paper",
    endpoint: "synthetic",
    account: accountID,
    account_origin: "synthetic",
    mcp_trading: "disabled",
    can_preview: false,
    can_write: false,
    blocked: true,
    blockers: [{
      code: "synthetic_snapshot",
      message: "Broker actions are unavailable in screenshot mode.",
    }],
  };
  const policyStatus = {
    kind: "ibkr.protection_policy_status",
    status: "active",
    policy_id: "synthetic-protection",
    policy_version: 1,
    profile: "balanced",
    fingerprint: fingerprint("protection-policy-fp-v1", "synthetic-protection-quiet"),
    source: "embedded synthetic snapshot",
    loaded_at: asOf,
    last_checked_at: asOf,
    blockers: [],
  };
  const autoTrade = {
    kind: "ibkr.auto_trade_status",
    as_of: asOf,
    trading,
    proposals_enabled: true,
    fast_path_enabled: false,
    hot_reload: false,
    reload_interval: "5m",
    proposal_cadence: "5m",
    policy: policyStatus,
    blocked: false,
    blockers: [],
  };

  const aaplStock = {
    symbol: "AAPL",
    sec_type: "STOCK",
    con_id: 101,
    exchange: "SMART",
    currency: "USD",
    quantity: 300,
    multiplier: 1,
    avg_cost: 205,
    mark: 210,
    valuation_mark: 210,
    data_type: "live",
    price_source: "last",
    regular_close: 206.56,
    regular_close_at: quoteAt,
    quote_price: 210,
    quote_price_source: "last",
    quote_price_at: quoteAt,
    quote_price_as_of: "now",
    quote_change: 3.44,
    quote_change_pct: 1.67,
    prev_close: 206.56,
    bid: 209.95,
    ask: 210.05,
    day_change: 3.44,
    day_change_pct: 1.67,
    day_change_money: 1_032,
    price_at: quoteAt,
    price_as_of: "now",
    feed_type: "live",
    spread_pct: 0.05,
    quote_quality: "firm",
    market_value_ccy: 63_000,
    market_value_base: 57_960,
    fx_rate: fx,
    unrealized_pnl_ccy: 1_500,
    unrealized_pnl_base: 1_380,
    realized_pnl_ccy: 0,
    realized_pnl_base: 0,
    daily_pnl_ccy: 1_032,
    daily_pnl_base: 949.44,
  };
  const aaplCall = {
    symbol: "AAPL",
    sec_type: "OPTION",
    con_id: 102,
    exchange: "SMART",
    currency: "USD",
    quantity: -3,
    multiplier: 100,
    avg_cost: 525,
    mark: 4.2,
    valuation_mark: 4.2,
    data_type: "live",
    price_source: "mark",
    price_at: quoteAt,
    price_as_of: "now",
    quote_quality: "firm",
    market_value_ccy: -1_260,
    market_value_base: -1_159.2,
    fx_rate: fx,
    unrealized_pnl_ccy: 315,
    unrealized_pnl_base: 289.8,
    realized_pnl_ccy: 0,
    realized_pnl_base: 0,
    daily_pnl_ccy: -53.74,
    daily_pnl_base: -49.44,
    expiry: expiryAt(120 * day),
    strike: 220,
    right: "C",
    delta: 0.38,
    gamma: 0.012,
    theta: -0.06,
    vega: 0.32,
    underlying: 210,
  };
  const nvdaStock = {
    symbol: "NVDA",
    sec_type: "STOCK",
    con_id: 201,
    exchange: "SMART",
    currency: "USD",
    quantity: 250,
    multiplier: 1,
    avg_cost: 168,
    mark: 175,
    valuation_mark: 175,
    data_type: "live",
    price_source: "last",
    regular_close: 171.52,
    regular_close_at: quoteAt,
    quote_price: 175,
    quote_price_source: "last",
    quote_price_at: quoteAt,
    quote_price_as_of: "now",
    quote_change: 3.48,
    quote_change_pct: 2.03,
    prev_close: 171.52,
    bid: 174.95,
    ask: 175.05,
    day_change: 3.48,
    day_change_pct: 2.03,
    day_change_money: 870,
    price_at: quoteAt,
    price_as_of: "now",
    feed_type: "live",
    spread_pct: 0.06,
    quote_quality: "firm",
    market_value_ccy: 43_750,
    market_value_base: 40_250,
    fx_rate: fx,
    unrealized_pnl_ccy: 1_750,
    unrealized_pnl_base: 1_610,
    realized_pnl_ccy: 0,
    realized_pnl_base: 0,
    daily_pnl_ccy: 870,
    daily_pnl_base: 800.4,
  };
  const spyStock = {
    symbol: "SPY",
    sec_type: "STOCK",
    con_id: 301,
    exchange: "ARCA",
    currency: "USD",
    quantity: 100,
    multiplier: 1,
    avg_cost: 610,
    mark: 620,
    valuation_mark: 620,
    data_type: "live",
    price_source: "last",
    regular_close: 618,
    regular_close_at: quoteAt,
    quote_price: 620,
    quote_price_source: "last",
    quote_price_at: quoteAt,
    quote_price_as_of: "now",
    quote_change: 2,
    quote_change_pct: 0.32,
    prev_close: 618,
    bid: 619.98,
    ask: 620.02,
    day_change: 2,
    day_change_pct: 0.32,
    day_change_money: 200,
    price_at: quoteAt,
    price_as_of: "now",
    feed_type: "live",
    spread_pct: 0.01,
    quote_quality: "firm",
    market_value_ccy: 62_000,
    market_value_base: 57_040,
    fx_rate: fx,
    unrealized_pnl_ccy: 1_000,
    unrealized_pnl_base: 920,
    realized_pnl_ccy: 0,
    realized_pnl_base: 0,
    daily_pnl_ccy: 200,
    daily_pnl_base: 184,
  };
  const spyPut = {
    symbol: "SPY",
    sec_type: "OPTION",
    con_id: 302,
    exchange: "SMART",
    currency: "USD",
    quantity: 2,
    multiplier: 100,
    avg_cost: 900,
    mark: 8.5,
    valuation_mark: 8.5,
    data_type: "live",
    price_source: "mark",
    price_at: quoteAt,
    price_as_of: "now",
    quote_quality: "firm",
    market_value_ccy: 1_700,
    market_value_base: 1_564,
    fx_rate: fx,
    unrealized_pnl_ccy: -100,
    unrealized_pnl_base: -92,
    realized_pnl_ccy: 0,
    realized_pnl_base: 0,
    daily_pnl_ccy: 495.22,
    daily_pnl_base: 455.6,
    expiry: expiryAt(180 * day),
    strike: 580,
    right: "P",
    delta: -0.22,
    gamma: 0.005,
    theta: -0.1,
    vega: 0.55,
    underlying: 620,
  };

  const exposures = [{
    underlying: "SPY",
    market_value_base: 58_604,
    market_value_pct_nlv: 4.69,
    effective_delta: 56,
    dollar_delta_base: 31_942.4,
    unrealized_pnl_base: 828,
    daily_pnl_base: 639.6,
    base_currency: baseCurrency,
  }, {
    underlying: "AAPL",
    market_value_base: 56_800.8,
    market_value_pct_nlv: 4.54,
    effective_delta: 186,
    dollar_delta_base: 35_935.2,
    unrealized_pnl_base: 1_669.8,
    daily_pnl_base: 900,
    base_currency: baseCurrency,
  }, {
    underlying: "NVDA",
    market_value_base: 40_250,
    market_value_pct_nlv: 3.22,
    effective_delta: 250,
    dollar_delta_base: 40_250,
    unrealized_pnl_base: 1_610,
    daily_pnl_base: 800.4,
    base_currency: baseCurrency,
  }];

  const marketCalendar = {
    market: "us",
    label: "US equities",
    timezone: "America/New_York",
    as_of: asOf,
    coverage_start: dateAt(-day),
    coverage_end: dateAt(14 * day),
    source: "embedded synthetic calendar",
    source_url: "https://www.nyse.com/markets/hours-calendars",
    session: {
      market: "us",
      label: "US equities",
      date: dateAt(),
      timezone: "America/New_York",
      state: "regular",
      is_open: true,
      reason: "Regular cash session",
      open: at(-2 * hour),
      close: at(4 * hour),
      next_open: at(22 * hour),
      next_close: at(28 * hour),
      source: "embedded synthetic calendar",
      source_url: "https://www.nyse.com/markets/hours-calendars",
      coverage_start: dateAt(-day),
      coverage_end: dateAt(14 * day),
    },
    sessions: [],
  };

  const quote = (symbol, secType, price, previous, change, changePct, bid, ask) => ({
    symbol,
    contract: { symbol, sec_type: secType, exchange: secType === "IND" ? "CBOE" : "SMART", currency: "USD" },
    bid,
    ask,
    last: price,
    mark: price,
    price,
    price_source: "last",
    regular_close: previous,
    regular_close_at: quoteAt,
    prior_regular_close: previous,
    regular_change: change,
    regular_change_pct: changePct,
    quote_price: price,
    quote_price_source: "last",
    quote_price_at: quoteAt,
    quote_price_as_of: "now",
    quote_change: change,
    quote_change_pct: changePct,
    prev_close: previous,
    change,
    change_pct: changePct,
    iv: null,
    iv_status: "unavailable",
    data_type: "live",
    feed_type: "live",
    spread_pct: ((ask - bid) / price) * 100,
    quote_quality: "firm",
    indicative: false,
    price_at: quoteAt,
    price_as_of: "now",
    stale: false,
    as_of: asOf,
  });

  const rules = [
    ["single_name_exposure", "Per-name exposure cap"],
    ["option_line_premium", "Single option line premium"],
    ["cash_sell_only", "Negative cash sell-only mode"],
    ["extrinsic_budget", "Portfolio extrinsic budget"],
    ["expiry_runway", "Expiry runway"],
    ["catalyst_coverage", "Option outlives its catalyst"],
    ["overwrite_earnings", "Overwrite spans earnings"],
    ["earnings_size_freeze", "At size before earnings"],
    ["red_on_green", "Relative weakness on a green tape"],
    ["winner_trim", "Trim winners into strength"],
    ["green_day_action", "Green day is an execution day"],
    ["hedge_integrity", "Hedge sized to the book"],
    ["exit_discipline", "Exit the dead thesis"],
    ["fx_exposure", "Non-base currency exposure"],
  ].map(([id, title], index) => ({
    id,
    number: index + 1,
    title,
    status: index === 10 ? "info" : "pass",
    evidence: index === 10 ? "Positive tape noted; no required action." : "Synthetic inputs are inside the approved band.",
    offenders: [],
    exempt: [],
    notes: [],
  }));

  const briefState = (detail) => ({ status: "ok", detail });
  const proposal = {
    key: "synthetic-nvda-trail",
    revision: "synthetic-quiet-1",
    state: "generated",
    bucket: "trailing_stop",
    rank: 1,
    symbol: "NVDA",
    sec_type: "STK",
    action: "SELL",
    quantity: 50,
    max_quantity: 250,
    position_quantity: 250,
    position_effect: "reduce",
    order_type: "TRAIL",
    trail: {
      basis: "instrument_price",
      offset_type: "percent",
      trailing_percent: 8,
      initial_stop_price: 161,
    },
    trail_sizing: {
      method: "synthetic-balanced-trail",
      version: "v1",
      data_quality: "complete",
      selected_by: "policy",
      reference_price: 175,
      reference_source: "last",
      reference_as_of: quoteAt,
      policy_min_pct: 5,
      policy_default_pct: 8,
      policy_fallback_pct: 10,
      policy_max_pct: 15,
      chosen_pct: 8,
      initial_stop_price: 161,
      as_of: asOf,
    },
    execution_semantics: {
      reference_side: "bid",
      reference_price: 174.95,
      reference_as_of: quoteAt,
      trigger_method: 2,
      trigger_method_label: "last",
      trigger_source: "consolidated last",
      trigger_effect: "market_order_when_triggered",
      price_guarantee: "stop_price_is_not_execution_price",
    },
    stop_risk: {
      reference_price: 175,
      stop_price: 161,
      distance: 14,
      distance_pct: 8,
      quantity: 50,
      multiplier: 1,
      estimated_loss_ccy: 700,
      currency: "USD",
      estimated_loss_base: 644,
      base_currency: baseCurrency,
      estimated_loss_pct_nlv: 0.05,
      gap_scenario: {
        label: "12% gap",
        gap_pct: 12,
        assumed_execution_price: 154,
        estimated_loss_ccy: 1_050,
        estimated_loss_base: 966,
        estimated_loss_pct_nlv: 0.08,
      },
      warning_codes: [],
    },
    stop_ladder: [{
      label: "5%",
      kind: "fixed_5pct",
      percent: 5,
      stop_price: 166.25,
      estimated_loss_ccy: 437.5,
      estimated_loss_base: 402.5,
      estimated_loss_pct_nlv: 0.03,
      reference_price: 175,
    }, {
      label: "8% policy",
      kind: "policy_chosen",
      percent: 8,
      stop_price: 161,
      estimated_loss_ccy: 700,
      estimated_loss_base: 644,
      estimated_loss_pct_nlv: 0.05,
      reference_price: 175,
    }],
    trigger_method: 2,
    tif: "GTC",
    outside_rth: false,
    contract: { symbol: "NVDA", sec_type: "STK", exchange: "SMART", currency: "USD" },
    reason: "Add a modest trailing stop to a profitable stock line.",
    details: ["Reduces 20% of the held line if the broker trail triggers."],
    position_market_value: 43_750,
    market_value_pct_nlv: 3.22,
    position_day_change_money: 870,
    position_day_change_currency: "USD",
    position_day_change_pct: 2.03,
    policy_id: "synthetic-protection",
    policy_version: 1,
    policy_fingerprint: fingerprint("protection-policy-fp-v1", "synthetic-protection-quiet"),
    blockers: [],
    created_at: at(-2 * minute),
  };

  const marketEvents = {
    kind: "ibkr.market_events",
    schema_version: "market-events-v1",
    as_of: asOf,
    symbols: ["AAPL", "NVDA", "SPY"],
    flags: [],
    by_symbol: {},
    source_health: [sourceHealth("halt"), sourceHealth("reg_sho")],
    fingerprint: fingerprint("market-events-fp-v2", "synthetic-market-events-quiet"),
    warning_details: [],
    not_execution: "Context only; no orders are placed.",
  };

  return {
    status: {
      daemon_version: "synthetic",
      daemon_started: at(-hour),
      uptime_seconds: 3600,
      account: accountID,
      connected_account: accountID,
      account_mode: "paper",
      gateway_host: "synthetic",
      gateway_port: 0,
      gateway_tls: false,
      negotiated_tls: false,
      port_origin: "synthetic",
      tls_origin: "synthetic",
      alternates: [],
      client_id: 0,
      connected: true,
      data_type: "live",
      server_version: 0,
      background_tasks: [],
      subsystems: [],
      data_quality: [],
      data_farms: [],
      members: { source: "embedded", as_of: asOf, count: 503, refresh_state: "healthy" },
      trading,
    },
    market_calendar: marketCalendar,
    account: {
      account_id: accountID,
      account_type: "IB-MARGIN",
      base_currency: baseCurrency,
      net_liquidation: FIXTURE.netLiquidation,
      buying_power: FIXTURE.buyingPower,
      available_funds: 900_000,
      excess_liquidity: 950_000,
      total_cash: 1_094_345.2,
      maintenance_margin: 125_000,
      initial_margin: 150_000,
      gross_position_value: 157_973.2,
      unrealized_pnl: 4_107.8,
      realized_pnl: 0,
      cushion: 0.76,
      look_ahead_init_margin: 155_000,
      look_ahead_maint_margin: 130_000,
      look_ahead_available_funds: 895_000,
      look_ahead_excess_liquidity: 945_000,
      daily_pnl: FIXTURE.dailyPnl,
      pnl_unrealized_total: 4_107.8,
      pnl_realized_total: 0,
      currency_exposure: [{
        currency: "USD",
        net_liquidation_ccy: 169_190,
        cash_ccy: 0,
        stock_market_value_ccy: 168_750,
        option_market_value_ccy: 440,
        unrealized_pnl_ccy: 4_465,
        realized_pnl_ccy: 0,
        exchange_rate: fx,
        net_liquidation_base: 155_654.8,
      }],
      data_type: "live",
      as_of: asOf,
    },
    positions: {
      data_type: "live",
      as_of: asOf,
      stocks: [aaplStock, nvdaStock, spyStock],
      options: [aaplCall, spyPut],
      by_underlying: [{
        underlying: "AAPL",
        stock: aaplStock,
        options: [aaplCall],
        group_market_value_ccy: 61_740,
        group_market_value_base: 56_800.8,
        group_market_value_pct_nlv: 4.54,
        group_unrealized_pnl_ccy: 1_815,
        group_unrealized_pnl_base: 1_669.8,
        group_daily_pnl_base: 900,
        group_effective_delta: 186,
        group_dollar_delta_ccy: 39_060,
        group_dollar_delta_ccy_currency: "USD",
        group_dollar_delta_base: 35_935.2,
      }, {
        underlying: "NVDA",
        stock: nvdaStock,
        options: [],
        group_market_value_ccy: 43_750,
        group_market_value_base: 40_250,
        group_market_value_pct_nlv: 3.22,
        group_unrealized_pnl_ccy: 1_750,
        group_unrealized_pnl_base: 1_610,
        group_daily_pnl_base: 800.4,
        group_effective_delta: 250,
        group_dollar_delta_ccy: 43_750,
        group_dollar_delta_ccy_currency: "USD",
        group_dollar_delta_base: 40_250,
      }, {
        underlying: "SPY",
        stock: spyStock,
        options: [spyPut],
        group_market_value_ccy: 63_700,
        group_market_value_base: 58_604,
        group_market_value_pct_nlv: 4.69,
        group_unrealized_pnl_ccy: 900,
        group_unrealized_pnl_base: 828,
        group_daily_pnl_base: 639.6,
        group_effective_delta: 56,
        group_dollar_delta_ccy: 34_720,
        group_dollar_delta_ccy_currency: "USD",
        group_dollar_delta_base: 31_942.4,
      }],
      portfolio: {
        effective_delta: 492,
        dollar_delta_ccy: 117_530,
        dollar_delta_ccy_currency: "USD",
        dollar_delta_base: 108_127.6,
        dollar_delta_base_currency: baseCurrency,
        daily_theta_ccy: -2,
        daily_theta_ccy_currency: "USD",
        daily_theta_base: -1.84,
        daily_theta_base_currency: baseCurrency,
        gamma: -2.6,
        vega: 14,
        greeks_coverage: 2,
        greeks_total: 2,
        base_currency: baseCurrency,
        net_liquidation_base: FIXTURE.netLiquidation,
        exposure_base: exposures,
        fx_sensitivity_per_pct: 1_556.55,
        fx_base_currency: baseCurrency,
      },
      account_id: accountID,
    },
    market_quotes: {
      as_of: asOf,
      quotes: {
        SPY: quote("SPY", "STK", 620, 618, 2, 0.32, 619.98, 620.02),
        VIX: quote("VIX", "IND", 15.8, 15.95, -0.15, -0.94, 15.75, 15.85),
        QQQ: quote("QQQ", "STK", 530, 528.6, 1.4, 0.26, 529.96, 530.04),
        IWM: quote("IWM", "STK", 293.5, 292.88, 0.62, 0.21, 293.47, 293.53),
        HYG: quote("HYG", "STK", 79.7, 79.61, 0.09, 0.11, 79.68, 79.72),
        TLT: quote("TLT", "STK", 84.5, 84.78, -0.28, -0.33, 84.48, 84.52),
      },
      errors: {},
    },
    regime: {
      as_of: asOf,
      fingerprint: fingerprint("regime-fp-v2", "synthetic-regime-quiet"),
      lifecycle: {
        stage: "quiet",
        scope: "market",
        severity: "observe",
        readiness: "ready",
        timing: "contemporaneous",
        confidence: "high",
        evidence: [],
        confirmed_by: [],
        unconfirmed: [],
        suppressed: [],
        rejected_by: [],
        governors: [],
        fingerprint: fingerprint("regime-fp-v2", "synthetic-regime-quiet"),
        not_execution: "Regime read only; no orders are placed.",
      },
      summary: {
        label: "Normal regime",
        evidence: "4 green clusters / 0 yellow / 0 red",
        indicator_evidence: "4 green / 0 yellow / 0 red",
        punch_line: "Broad-market conditions are calm and well covered.",
        confidence: "high",
        dominant_risks: [],
        not_advice: "Regime read only; no orders are placed.",
      },
      posture: {
        label: "Normal regime",
        tone: "normal",
        stage: "quiet",
        severity: "observe",
        readiness: "ready",
        confidence: "high",
        evidence: "No stress cluster is active.",
      },
      composite: {
        verdict: "Normal regime",
        green_count: 4,
        yellow_count: 0,
        red_count: 0,
        ranked_count: 4,
        unranked_count: 0,
        cluster_green_count: 4,
        cluster_yellow_count: 0,
        cluster_red_count: 0,
        cluster_ranked_count: 4,
        cluster_unranked_count: 0,
        cluster_eligible_red_count: 0,
        cluster_provisional_red_count: 0,
      },
      warning_details: [],
      data_quality: [],
      source_health: [compactSourceHealth("breadth"), compactSourceHealth("volatility"), compactSourceHealth("credit"), compactSourceHealth("gamma")],
      indicators: [
        ["Breadth", "green", "62% above 50-DMA"],
        ["Volatility", "green", "VIX curve in contango"],
        ["Credit", "green", "Credit spreads calm"],
        ["Dealer gamma", "green", "Spot above zero gamma"],
      ].map(([name, band, reading]) => ({
        name,
        status: "ok",
        band,
        as_of: { label: "now", time: asOf, date: dateAt(), freshness: "live", source: "synthetic", age_seconds: 4 },
        reading,
        freshness_class: "fresh",
      })),
    },
    stress: {
      as_of: asOf,
      source_as_of: { account: asOf, positions: asOf, regime: asOf, market_events: asOf },
      fingerprint: fingerprint("stress-fp-v2", "synthetic-stress-quiet"),
      source_fingerprints: {},
      source_health: [sourceHealth("account"), sourceHealth("positions"), sourceHealth("regime"), sourceHealth("market_events")],
      policy: "synthetic-stress",
      policy_profile: "balanced",
      policy_version: "1",
      policy_fingerprint: fingerprint("stress-policy-fp-v1", "synthetic-stress-policy"),
      action: "stand_down",
      market_confirmation: "none",
      portfolio_fit: "low",
      input_health: "ok",
      direction: "constructive",
      severity: "observe",
      planner_mode_hint: "none",
      planner_readiness: "none",
      summary: "No defensive stress action is indicated.",
      primary_drivers: [],
      signals: [],
      rows: [{
        title: "Quiet desk",
        direction: "constructive",
        severity: "observe",
        guidance: "Maintain the current review cadence.",
        evidence: "Market and portfolio inputs are fresh and inside their calm bands.",
      }],
      portfolio: {
        base_currency: baseCurrency,
        net_liquidation: FIXTURE.netLiquidation,
        cushion_pct: 76,
        look_ahead_cushion_pct: 75.6,
        gross_exposure_pct_nlv: 12.45,
        net_delta_pct_nlv: 8.65,
        gross_delta_pct_nlv: 8.65,
        largest_exposure: "SPY",
        largest_exposure_pct_nlv: 4.69,
        largest_delta_exposure: "NVDA",
        largest_delta_exposure_pct_nlv: 3.22,
        daily_pnl_pct: 0.19,
        option_greeks: "complete",
        held_stress: [],
      },
      market: {
        regime_verdict: "Normal regime",
        regime_posture: {
          label: "Normal regime",
          tone: "normal",
          stage: "quiet",
          severity: "observe",
          readiness: "ready",
          confidence: "high",
          evidence: "No stress cluster is active.",
        },
        red_clusters: 0,
        eligible_red_clusters: 0,
        eligible_red_cluster_names: [],
        yellow_clusters: 0,
        ranked_clusters: 4,
        unranked_clusters: 0,
        red_cluster_names: [],
        yellow_cluster_names: [],
        unconfirmed_red_cluster_names: [],
        ambiguous_clusters: [],
        partial_clusters: [],
        computing_clusters: [],
        degraded_clusters: [],
        stale_clusters: [],
        spy_price: 620,
        spy_change_pct: 0.32,
        vix: 15.8,
        vix_change_pct: -0.94,
      },
      market_indicators: [{ name: "Breadth", status: "green", as_of: asOf, reading: "62% above 50-DMA", comment: "Broad participation" },
        { name: "Volatility", status: "green", as_of: asOf, reading: "VIX 15.8", comment: "Calm term structure" },
        { name: "Credit", status: "green", as_of: asOf, reading: "Spreads stable", comment: "No funding stress" },
        { name: "Dealer gamma", status: "green", as_of: asOf, reading: "Positive", comment: "Dampening posture" }],
      warnings: [],
      not_execution: "Advisory only; no orders are placed.",
    },
    rules: {
      as_of: asOf,
      enabled: true,
      status: "ok",
      rules,
      ranked: rules.map((_, index) => index),
      breach_counts: { pass: 13, info: 1, watch: 0, act: 0 },
      input_health: [sourceHealth("positions"), sourceHealth("account"), sourceHealth("tape"), sourceHealth("earnings")],
      earnings: [],
      policy_id: "synthetic-rulebook",
      policy_version: 1,
      policy_fingerprint: fingerprint("rulebook-fp-v3", "synthetic-rulebook-quiet"),
      base_currency: baseCurrency,
    },
    brief: {
      as_of: asOf,
      brief_fingerprint: "synthetic-brief-quiet",
      // Published frame: the fixture patch merges shallowly, so without this
      // block the daemon-composed narrative - prose over the OPERATOR's real
      // figures - would survive into a synthetic screenshot. The synthetic
      // narrative below mirrors the composer's quiet register over the
      // fixture's own numbers, so the published Brief frame shows the design
      // of record with no live prose underneath.
      narrative: {
        lead: [
          { text: "Stress reads stand down at observe severity. Regime stage quiet, verdict Normal regime. Nothing across Review and Ready needs a decision." },
        ],
        review: [
          { runs: [
            { text: "The session closed with Daily P/L " },
            { text: "EUR +2,340", role: "figure" },
            { text: " on equity of " },
            { text: "EUR 1,250,000", role: "figure" },
            { text: ". By name in EUR: AAPL +900.00, NVDA +800.40, SPY +639.60. 2 protection proposals were offered and 1 acted in the last recorded session, with no overrides, no rule transitions, and no capital events." },
          ] },
          { runs: [
            { text: "Admin is clean: reconcile clean and due in 5 days, auto-extend needs nothing, nothing waits for sign-off, and 1 order is working." },
          ] },
        ],
        ready: [
          { runs: [
            { text: "Breadth has " },
            { text: "62.0%", role: "figure" },
            { text: " above the 50-DMA and 58.0% above the 200-DMA, net new highs +1.4%. Dealer gamma is positive with spot 620.00 against zero gamma 607.00, a gap of +2.1%. The US session is regular." },
          ] },
          { runs: [
            { text: "Capital sits in the ok tier with " },
            { text: "2.0%", role: "figure" },
            { text: " of the drawdown budget consumed. The drawdown latch is open. Premium at risk " },
            { text: "EUR 1,564", role: "figure" },
            { text: " across 1 long option leg; hedge cost EUR -18.40 per day. Process folds clean: policy pins match, artefacts are current, the monthly pulse is not due, and no held-name events." },
          ] },
        ],
        coda: [{ text: "Nothing owed before the bell." }],
      },
      review: {
        ...briefState("Last session closed clean."),
        session_pnl: { ...briefState("Account snapshot is fresh."), equity_base: FIXTURE.netLiquidation, daily_pnl_base: FIXTURE.dailyPnl, base_currency: baseCurrency, as_of: asOf },
        attribution: { ...briefState("Position-level daily P/L is orderly."), rows: [{ symbol: "AAPL", daily_pnl_base: 900 }, { symbol: "NVDA", daily_pnl_base: 800.4 }, { symbol: "SPY", daily_pnl_base: 639.6 }] },
        rules_delta: { ...briefState("No rule transitions."), baseline_at: at(-day), transitions: [], added: [], removed: [], rulebook_fingerprint_changed: false, baseline_fingerprint: "synthetic-rulebook-quiet", current_fingerprint: "synthetic-rulebook-quiet" },
        proposals: { ...briefState("2 offered, 1 acted in the last recorded session."), day: "synthetic", offered: 2, acted: 1 },
        overrides: { ...briefState("No overrides used."), rows: [] },
        capital_events: { ...briefState("No capital events this session; adjusted-peak provenance shown."), latched: false, adjusted_peak_base: 1_275_000, peak_as_of: at(-day), base_currency: baseCurrency },
        reconcile: { ...briefState("Reconciliation is comfortably inside its window."), last_reconciled_at: at(-day), source: "synthetic clean report", deadline: at(5 * day), days_remaining: 5 },
        auto_extend: { ...briefState("No extension is needed."), report_id: "synthetic-clean", at: asOf },
        one_tap: { ...briefState("No exception sign-off is pending."), report_id: "", signable: false, blockers: [] },
        working_orders: { ...briefState("No working orders."), count: 0 },
      },
      ready: {
        ...briefState("Desk is ready for the open."),
        regime: { ...briefState("No stress lifecycle is active."), stage: "quiet", verdict: "Normal regime" },
        breadth: { ...briefState("Participation is broad."), pct_above_50dma: 62, pct_above_200dma: 58, net_new_highs_pct: 1.4, as_of: asOf, data_type: "live" },
        gamma: { ...briefState("Dealer positioning is dampening."), spot: 620, zero_gamma: 607, gap_pct: 2.14, gamma_sign: "positive", as_of: asOf },
        stress: { ...briefState("No defensive action."), action: "stand down", severity: "observe", summary: "Quiet desk" },
        session: { ...briefState("Regular session context."), market: "US", state: "regular", is_open: true, open: at(-2 * hour), close: at(4 * hour), next_open: at(22 * hour) },
        market_events: [{ ...briefState("No held-name market events."), kind: "earnings", count: 0, symbols: [] }],
        capital: { ...briefState("No drawdown warning."), tier: "normal", enforcement: "open", consumed_pct: 2, drawdown_base: 25_000, adjusted_peak_base: 1_275_000, base_currency: baseCurrency },
        latch: { ...briefState("Drawdown latch is open."), latched: false },
        premium_at_risk: { ...briefState("Option premium at risk is contained."), amount_base: 1_564, base_currency: baseCurrency, included_legs: 1, excluded_legs: 0 },
        hedge_cost: { ...briefState("Hedge carry is modest."), amount_base: -18.4, base_currency: baseCurrency, included_legs: 1, excluded_legs: 0 },
        policy_drift: { ...briefState("Policy pins match."), rows: [] },
        artefacts: { ...briefState("Scheduled artefacts are current."), rows: [{ ...briefState("Morning artefact complete."), kind: "morning", cadence: "daily", declared: true, completed: true, completed_at: asOf }] },
      },
    },
    proposals: {
      kind: "ibkr.trade_proposal_snapshot",
      schema_version: "trade-proposal-snapshot-v2",
      as_of: asOf,
      revision: "synthetic-quiet-1",
      account_id: accountID,
      account_mode: "paper",
      policy_id: "synthetic-protection",
      policy_version: 1,
      policy_fingerprint: fingerprint("protection-policy-fp-v1", "synthetic-protection-quiet"),
      policy_status: policyStatus,
      auto_trade: autoTrade,
      trading,
      source_fingerprints: {},
      market_events: marketEvents,
      proposals: [proposal],
      counts: {
        total: 1,
        actionable: 1,
        theta_hygiene: 0,
        risk_reduction: 0,
        trailing_stop: 1,
        market_flags: 0,
        theta_per_day: 0,
        theta_per_day_currency: "USD",
        theta_per_day_base: 0,
        risk_reduction_excess_notional: 0,
        risk_reduction_excess_notional_base: 0,
        base_currency: baseCurrency,
      },
      blockers: [],
      loaded_from_state: false,
    },
    market_events: marketEvents,
    trading,
    auto_trade: autoTrade,
    opportunities: {
      kind: "ibkr.opportunity_snapshot",
      schema_version: "opportunity-snapshot-v1",
      as_of: asOf,
      revision: "synthetic-empty-1",
      account_id: accountID,
      account_mode: "paper",
      policy_id: "synthetic-opportunities",
      policy_version: 1,
      policy_fingerprint: fingerprint("opportunity-policy-fp-v1", "synthetic-opportunities"),
      policy_status: { status: "active", policy_id: "synthetic-opportunities", policy_version: 1, fingerprint: fingerprint("opportunity-policy-fp-v1", "synthetic-opportunities"), loaded_at: asOf, last_checked_at: asOf, blockers: [] },
      status: { kind: "ibkr.opportunity_status", as_of: asOf, enabled: true, hot_reload: false, reload_interval: "5m", refresh_cadence: "5m", policy: { status: "active", policy_id: "synthetic-opportunities", policy_version: 1 }, trading, blocked: false, blockers: [] },
      trading,
      source_fingerprints: {},
      opportunities: [],
      counts: { total: 0, actionable: 0, blocked: 0, option_exercise: 0 },
      blockers: [],
      loaded_from_state: false,
    },
    settings: {},
    errors: [],
    sources: Object.fromEntries([
      "status", "market_calendar", "account", "positions", "market_quotes", "market_events", "regime", "stress", "rules", "brief", "trading", "auto_trade", "opportunities", "proposals", "settings",
    ].map((source) => [source, { updated_at: asOf }])),
    updated_at: asOf,
  };
}

// Runs inside the page before any app code. Rewrites account-bearing
// payloads from both transports. Generic by construction: every
// `account_id` string anywhere in a payload becomes the placeholder, and
// every object that carries `net_liquidation` gets the fixture summary,
// so SSE event shapes and bootstrap nesting can drift without leaking.
function installFixtureRewrite(fixture) {
  // The SPA reads the account id and mode from several alternates
  // (account.account_id, trading.account, status.connected_account,
  // status.account_mode, …), so the walker matches by value shape and
  // key family rather than enumerating paths.
  const ID_SHAPE = /^D?U\d{6,9}$/;
  const patch = (node) => {
    if (Array.isArray(node)) {
      node.forEach(patch);
      return node;
    }
    if (!node || typeof node !== "object") {
      return node;
    }
    for (const [key, value] of Object.entries(node)) {
      if (typeof value === "string") {
        if (ID_SHAPE.test(value.trim())) {
          node[key] = fixture.accountID;
        } else if (
          (key === "account_mode" || key === "environment" || key === "mode") &&
          /^live$/i.test(value.trim())
        ) {
          node[key] = "paper";
        }
        continue;
      }
      patch(value);
    }
    if (typeof node.net_liquidation === "number") {
      node.net_liquidation = fixture.netLiquidation;
      node.buying_power = fixture.buyingPower;
      node.daily_pnl = fixture.dailyPnl;
      if ("net_liquidation_start_of_day" in node) {
        node.net_liquidation_start_of_day = fixture.netLiquidation - fixture.dailyPnl;
      }
      if ("previous_net_liquidation" in node) {
        node.previous_net_liquidation = fixture.netLiquidation - fixture.dailyPnl;
      }
    }
    return node;
  };

  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (...fetchArgs) => {
    const res = await nativeFetch(...fetchArgs);
    const url = typeof fetchArgs[0] === "string" ? fetchArgs[0] : fetchArgs[0]?.url || "";
    if (!res.ok || !/\/api\/(bootstrap|account|positions)\b/.test(url)) {
      return res;
    }
    const body = patch(await res.clone().json());
    return new Response(JSON.stringify(body), {
      status: res.status,
      statusText: res.statusText,
      headers: res.headers,
    });
  };

  const NativeEventSource = globalThis.EventSource;
  globalThis.EventSource = function fixtureEventSource(url, options) {
    const es = new NativeEventSource(url, options);
    const nativeAdd = es.addEventListener.bind(es);
    es.addEventListener = (type, listener, ...rest) => {
      nativeAdd(
        type,
        (event) => {
          let patched = event;
          try {
            const data = JSON.stringify(patch(JSON.parse(event.data)));
            patched = new MessageEvent(event.type, { data, lastEventId: event.lastEventId });
          } catch {
            // Non-JSON events (heartbeats) pass through untouched.
          }
          const smoke = globalThis.__canarySmoke;
          if (smoke && type === "snapshot") smoke.initialSnapshotSeen = true;
          if (smoke?.freezeLiveEvents) return;
          if (typeof listener === "function") {
            listener.call(es, patched);
          } else {
            listener.handleEvent(patched);
          }
        },
        ...rest,
      );
    };
    return es;
  };
  globalThis.EventSource.prototype = NativeEventSource.prototype;
}
