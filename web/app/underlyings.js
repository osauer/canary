import { applyTileSeverity, heldStressEvidence, heldStressItems, humanList, marketQuoteErrorLabel, quoteBySymbol, quoteChange, quoteChangePct, quotePrevClose, quotePrice, quoteTime } from "./stress.js";
import { marketEventFlagsForSymbol, marketFlagRow, renderMarketFlagRail, underlyingHeroMarketFlags } from "./market-events.js";
import { $, accountAuthority, accountBaseCurrency, accountFieldAvailable, accountFieldValue, ageLabel, cleanDetail, compactMoney, displayMoney, firstNumber, hasNumericValue, labelize, mergeCurrency, normalizeCurrency, normalizeSymbol, numberRead, parseDate, pct, privacyMask, quoteTimestamp, renderFreshnessTimestamp, renderSensitiveAccountId, renderSensitiveText, riskMoney, sensitiveDisplayMoney, sensitiveMoneyHidden, shortTime, signedClass, signedDisplayMoney, signedPct } from "./shared.js";
import { normalizedPositionsSort, state } from "./state.js";

function renderAccountPanel(account = {}, positions = {}, stress = {}) {
  const detail = $("accountOverviewDetail");
  const detailToggle = $("accountOverviewToggle");
  detail.hidden = !state.accountOverviewOpen;
  detailToggle.textContent = state.accountOverviewOpen ? "Hide detail" : "Show detail";
  detailToggle.setAttribute("aria-expanded", String(state.accountOverviewOpen));
  $("accountPanel").dataset.open = String(state.accountOverviewOpen);

  const currency = accountBaseCurrency(account);
  const netLiquidation = accountFieldValue(account, "net_liquidation");
  const buyingPower = accountFieldValue(account, "buying_power");
  const dailyPnL = accountDailyPnlValue(account);
  const hasValue = hasNumericValue(netLiquidation);
  const accountContext = currentAccountContext(account);
  const value = $("netLiquidation");
  value.textContent = state.accountValueVisible || !hasValue
    ? compactMoney(netLiquidation, currency)
    : privacyMask();
  value.classList.toggle("is-private", !state.accountValueVisible && hasValue);
  renderSensitiveText("buyingPower", compactMoney(buyingPower, currency), hasNumericValue(buyingPower));
  renderSensitiveText("dailyPnl", signedDisplayMoney(dailyPnL, currency), hasNumericValue(dailyPnL));
  $("dailyPnl").className = hasNumericValue(dailyPnL)
    ? `${signedClass(dailyPnL)}${!state.accountValueVisible ? " is-private" : ""}`
    : "signed";
  renderAccountDailyPnlPct(account);
  // The account id is demoted to a quiet subtitle and masked by the eye toggle;
  renderSensitiveAccountId("accountLabel", accountContext.accountId, accountContext.accountLabel);
  renderTradingEnvPill(accountContext.modeClass);
  renderAccountFreshness(account, state.snapshot?.sources?.account || {});

  const button = $("accountPrivacyToggle");
  button.classList.toggle("is-visible", state.accountValueVisible);
  button.setAttribute("aria-pressed", String(state.accountValueVisible));
  const label = state.accountValueVisible ? "Hide account values" : "Show account values";
  button.setAttribute("aria-label", label);
  button.title = label;

  const positionsAvailable = positionsAuthorityView(positions, state.snapshot?.sources?.positions || {}).available;
  const portfolio = positionsAvailable ? positions.portfolio || {} : {};
  const baseCurrency = portfolio.base_currency || currency;
  renderSensitiveText("accountRiskDelta", riskMoney(
    portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
    portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy));
  renderSensitiveText("accountRiskTheta", riskMoney(
    portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
    portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy));
  renderSensitiveText("accountRiskFx", riskMoney(
    portfolio.fx_sensitivity_per_pct,
    portfolio.fx_base_currency || baseCurrency,
  ), hasNumericValue(portfolio.fx_sensitivity_per_pct));
  renderAccountLargestExposure(portfolio, stress, baseCurrency);
  renderDeltaTile(portfolio, stress, baseCurrency);
}


// The Net $ Delta window reads out net dollar delta, theta decay, FX
// sensitivity, and the largest single name. Values stay behind the account
// privacy mask; the tile takes the stress tint only when a delta-family
// driver is what the daemon ranks as the binding problem, so color here can
// never disagree with the Stress window beside it.
function renderDeltaTile(portfolio = {}, stress = {}, baseCurrency = "") {
  const lead = $("deltaTileLead");
  const sub = $("deltaTileSub");
  if (!lead || !sub) return;
  const delta = portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy;
  const theta = portfolio.daily_theta_base ?? portfolio.daily_theta_ccy;
  // The direction word is posture, and posture stays behind the mask like
  // every other signed tone. The delta renders compact ("-572K") so the
  // lead's two clauses survive a 375px tile without wrapping mid-figure;
  // the precise figure lives in the account Detail strip.
  const direction = state.accountValueVisible && typeof delta === "number" ? (delta > 0 ? "long" : delta < 0 ? "short" : "flat") : "";
  const deltaCurrency = portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency;
  const compactDelta = state.accountValueVisible && hasNumericValue(delta) ? compactMoney(delta, deltaCurrency) : maskedRiskMoney(delta, deltaCurrency);
  lead.textContent = [
    [direction, compactDelta].filter(Boolean).join(" "),
    `theta ${maskedRiskMoney(theta, portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency)}/d`,
  ].join(" · ");
  const largest = (portfolio.exposure_base || [])[0];
  const top = largest?.underlying
    ? `largest ${largest.underlying}${typeof largest.market_value_pct_nlv === "number" ? ` ${pct(largest.market_value_pct_nlv)} NLV` : ""}`
    : "largest --";
  sub.textContent = [
    `FX 1% ${maskedRiskMoney(portfolio.fx_sensitivity_per_pct, portfolio.fx_base_currency || baseCurrency)}`,
    top,
  ].join(" · ");
  const tile = $("deltaTile");
  if (tile) {
    const deltaDrivers = ["net_delta_high", "gross_delta_high", "single_name_delta_high", "single_name_exposure_high", "gross_exposure_high"];
    const driven = (stress.primary_drivers || []).some((id) => deltaDrivers.includes(String(id || "").trim().toLowerCase()));
    // This is a context readout, not an independently evaluated pass/fail
    // rule. Give measured context the design system's information lamp,
    // preserve a daemon-served binding severity, and gray missing evidence.
    const tone = !hasNumericValue(delta)
      ? "stale"
      : driven
        ? String(stress.severity || "").toLowerCase()
        : "info";
    applyTileSeverity(tile, tone);
    tile.title = "Net $ delta: delta-weighted market exposure across held underlyings — a 1% move in the underlyings shifts P/L by about 1% of this figure. "
      + "Theta: option time decay per day. FX 1%: P/L from a 1% move in non-base currencies. "
      + "Largest: the single name with the biggest market value, as a share of net liquidation.";
  }
}

function maskedRiskMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? riskMoney(value, currency) : privacyMask();
}

function renderAccountDailyPnlPct(account = {}) {
  const el = $("dailyPnlPct");
  if (!el) return;
  const value = accountDailyPnlPct(account);
  const observation = String(account.daily_pnl_observation?.status || "").toLowerCase();
  const closed = marketSessionClosed();
  el.className = "account-pnl-pct " + signedClass(value);
  if (typeof value === "number") {
    const suffix = observation === "stale" ? "stale" : closed || observation === "not_due" ? "since close" : "today";
    el.textContent = `${signedPct(value)} ${suffix}`;
  } else if (["missing", "invalid", "stale"].includes(observation)) {
    el.textContent = observation === "stale" ? "Daily P/L stale" : "Daily P/L unavailable";
  } else if (observation === "not_due") {
    el.textContent = "Daily P/L not due";
  } else {
    el.textContent = "--";
  }
  const frameAt = account.daily_pnl_observation?.as_of;
  el.title = closed
    ? "The broker's running daily P/L since its prior close, at off-session marks — not a completed-session result — as a percentage of estimated start-of-day net liquidation; the market is closed."
      + (frameAt ? ` P/L frame ${shortTime(frameAt)}.` : "")
    : "Daily P/L as a percentage of estimated start-of-day net liquidation";
}

function accountDailyPnlPct(account = {}) {
  const dailyPnL = accountDailyPnlValue(account);
  if (typeof dailyPnL !== "number") return null;
  const startOfDay = firstNumber(
    account.net_liquidation_start_of_day,
    account.previous_net_liquidation,
    accountFieldAvailable(account, "net_liquidation") && typeof account.net_liquidation === "number"
      ? account.net_liquidation - dailyPnL
      : null,
  );
  const denominator = typeof startOfDay === "number" && startOfDay > 0
    ? startOfDay
    : accountFieldValue(account, "net_liquidation");
  if (typeof denominator !== "number" || denominator <= 0) return null;
  return (dailyPnL / denominator) * 100;
}

function accountDailyPnlValue(account = {}) {
  const observation = String(account.daily_pnl_observation?.status || "").toLowerCase();
  if (!["ok", "stale", "not_due"].includes(observation)) return null;
  return accountFieldValue(account, "daily_pnl");
}

function accountAuthorityReason(reason = "") {
  switch (String(reason || "").toLowerCase()) {
    case "unstamped_cache": return "Canary has a cached account update, but cannot prove when it was observed.";
    case "scope_unresolved": return "No single account is selected.";
    case "scope_conflict": return "The account response conflicts with the selected account.";
    case "account_unbound": return "The account response could not be tied to the selected account.";
    case "account_mismatch": return "The account response names a different account.";
    case "unprimed": return "No account snapshot has arrived yet.";
    case "invalid_payload": return "The account response was incomplete or invalid.";
    case "clock_invalid": return "The account timestamp is ahead of this machine's clock.";
    case "receipt_stale": return "The account receipt is older than the current session.";
    case "session_changed": return "The broker session changed while the account snapshot was loading.";
    default: return "";
  }
}

function renderAccountFreshness(account = {}, source = {}) {
  const el = $("accountAsOf");
  if (!el) return;
  const authority = accountAuthority(account);
  const sourceState = String(source.state || "").toLowerCase();
  const lastSuccess = parseDate(source.last_success_at);
  if (el.dataset.freshnessLabel === undefined) el.dataset.freshnessLabel = "";
  el.hidden = false;
  el.classList.add("stale");

  if (sourceState === "unavailable") {
    el.textContent = lastSuccess ? `Account unavailable · last good ${shortTime(lastSuccess.toISOString())}` : "Account unavailable";
    el.title = "The app could not refresh account data. Values shown are the last snapshot received.";
    return;
  }
  if (!authority) {
    el.textContent = sourceState === "not_observed" ? "Account data pending" : "Account data cannot be verified";
    el.title = "Canary cannot tell whether account values are present, so it will not show them.";
    return;
  }

  const reason = accountAuthorityReason(authority.reason);
  if (authority.availability !== "available") {
    el.textContent = "Account unavailable";
    el.title = reason || "The daemon did not publish an available account snapshot.";
    return;
  }
  if (authority.freshness === "unknown") {
    el.textContent = authority.source === "account_updates_cache" ? "Cached · time unknown" : "Account time unknown";
    el.title = reason || "Canary cannot prove when this account snapshot was observed.";
    return;
  }
  if (authority.freshness === "stale") {
    const at = parseDate(authority.as_of || account.as_of);
    const minutes = at ? Math.max(0, Math.floor((Date.now() - at.getTime()) / 60000)) : null;
    el.textContent = minutes === null ? "Account data stale" : `Account data stale · ${ageLabel(minutes)}`;
    el.title = reason || "The daemon marked this account snapshot stale.";
    return;
  }

  el.classList.remove("stale");
  renderFreshnessTimestamp(el, authority.as_of || account.as_of, { staleMinutes: 15, quietWhenFresh: true, fallback: "Account time unavailable" });
}

function renderAccountLargestExposure(portfolio = {}, stress = {}, baseCurrency = "") {
  const panel = $("accountLargestExposurePanel");
  const button = $("accountLargestExposureToggle");
  const list = $("accountLargestExposureList");
  const exposures = (portfolio.exposure_base || []).slice(0, 5);
  const largest = exposures[0];
  const label = largest?.underlying
    ? `${largest.underlying}${typeof largest.market_value_pct_nlv === "number" ? ` ${pct(largest.market_value_pct_nlv)} of NLV` : ""}`
    : "--";
  $("accountLargestExposureLabel").textContent = label;
  panel.hidden = !state.accountExposureOpen;
  button.setAttribute("aria-expanded", String(state.accountExposureOpen));
  button.disabled = exposures.length === 0 && heldStressItems(stress).length === 0;
  button.title = button.disabled ? "No exposure rows in this snapshot" : "Show largest exposure detail";
  if (panel.hidden) return;

  const rows = exposures.map((exposure) => exposureMetricRow(exposure, baseCurrency));
  const heldStress = heldStressItems(stress).slice(0, 3);
  for (const item of heldStress) {
    rows.push(heldStressMetricRow(item));
  }
  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No exposure rows available for this snapshot.";
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...rows);
}

function exposureMetricRow(exposure, baseCurrency) {
  const row = document.createElement("div");
  row.className = "metric-row";
  const label = document.createElement("span");
  const pctText = typeof exposure.market_value_pct_nlv === "number" ? ` ${pct(exposure.market_value_pct_nlv)}` : "";
  label.textContent = `${exposure.underlying || "--"}${pctText}`;
  const value = document.createElement("b");
  value.textContent = sensitiveDisplayMoney(exposure.market_value_base, exposure.base_currency || baseCurrency);
  value.className = sensitiveMoneyHidden(exposure.market_value_base) ? "is-private" : "";
  row.append(label, value);
  return row;
}

function heldStressMetricRow(stress) {
  const row = document.createElement("div");
  row.className = "metric-row";
  const label = document.createElement("span");
  label.textContent = `${stress.underlying || "Held name"} stress`;
  const value = document.createElement("b");
  value.textContent = heldStressEvidence(stress);
  row.append(label, value);
  return row;
}

function renderUnderlyings(positions = {}, account = {}, marketEvents = state.snapshot?.market_events || {}) {
  const list = $("underlyingBookList");
  if (!list) return;

	const baseCurrency = normalizeCurrency(accountBaseCurrency(account) || positions.portfolio?.base_currency || "");
	const rows = underlyingBookRows(positions, baseCurrency, marketEvents);
	const heldCount = rows.length;
  const count = $("underlyingBookCount");
  const status = $("underlyingBookStatus");
  const freshness = $("underlyingBookFreshness");
  const sort = $("positionsSort");
  const authorityView = positionsAuthorityView(positions, state.snapshot?.sources?.positions || {});
  const legCount = rows.reduce((total, row) => total + row.stockCount + row.optionCount, 0);
  const quoteSummary = underlyingQuoteSummary(rows);
  renderUnderlyingPnlSummary(authorityView.available ? underlyingHeldDailyPnlTotals(rows, baseCurrency) : {});
  renderMovers(authorityView.available ? rows : [], baseCurrency);
  renderMarketFlagRail("underlyingFlagRail", underlyingHeroMarketFlags(rows, marketEvents));
	if (count) {
		count.textContent = !authorityView.available
			? rows.length === 0 ? "Positions unavailable" : `${heldCount} last known · ${legCount} ${legCount === 1 ? "leg" : "legs"}`
			: rows.length === 0
			? "No underlyings"
			: `${heldCount} held · ${legCount} ${legCount === 1 ? "leg" : "legs"}`;
	}
  if (status) {
    status.textContent = state.underlyingNotice
      || (!authorityView.available ? authorityView.detail : "")
      || quoteSummary
		|| (heldCount > 0 ? "Current held underlyings" : "Waiting for positions");
  }
  if (freshness) {
    renderPositionsFreshness(freshness, positions, state.snapshot?.sources?.positions || {});
  }
  if (sort) sort.value = normalizedPositionsSort(state.positionsSort);
  if (state.selectedUnderlying && !rows.some((row) => row.symbol === state.selectedUnderlying)) {
    state.selectedUnderlying = "";
  }

	if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "underlying-book__empty";
		empty.textContent = authorityView.available ? "No held underlyings." : "Position data unavailable.";
    list.replaceChildren(empty);
    return;
  }

  list.replaceChildren(...rows.map((row) => underlyingBookRow(row, baseCurrency)));
}

function positionsAuthorityView(positions = {}, source = {}) {
  const authority = positions.authority;
  const sourceState = String(source.state || "").toLowerCase();
  if (sourceState === "unavailable") {
    return { available: false, detail: "Position refresh unavailable; showing last good data." };
  }
  if (!authority || typeof authority !== "object") {
    return { available: false, detail: "Position data cannot be verified." };
  }
  if (authority.availability === "available") {
    return { available: true, detail: "Current held underlyings" };
  }
  const reason = String(authority.reason || "").toLowerCase();
  switch (reason) {
    case "scope_unresolved": return { available: false, detail: "Positions unavailable because no single account is selected." };
    case "scope_conflict":
    case "account_mismatch": return { available: false, detail: "Positions unavailable because the account does not match." };
    case "account_unbound": return { available: false, detail: "Positions could not be tied to the selected account." };
    case "unprimed": return { available: false, detail: "Positions are still loading; an empty result is not a clean book." };
    case "receipt_stale": return { available: false, detail: "Position data is stale; old rows remain visible for reference." };
    case "clock_invalid": return { available: false, detail: "Position data time could not be verified." };
    case "session_changed": return { available: false, detail: "The broker session changed while positions were loading." };
    default: return { available: false, detail: authority.freshness === "stale" ? "Position data is stale; old rows remain visible for reference." : "Position data unavailable." };
  }
}

function renderPositionsFreshness(el, positions = {}, source = {}) {
  if (!el) return;
  const authority = positions.authority;
  const sourceState = String(source.state || "").toLowerCase();
  const lastSuccess = parseDate(source.last_success_at);
  if (el.dataset.freshnessLabel === undefined) el.dataset.freshnessLabel = "";
  el.hidden = false;
  el.classList.add("stale");
  if (sourceState === "unavailable") {
    el.textContent = lastSuccess ? `Positions unavailable · last good ${shortTime(lastSuccess.toISOString())}` : "Positions unavailable";
    el.title = "The app could not refresh positions. Old rows remain visible for reference.";
    return;
  }
  if (!authority || authority.availability !== "available") {
    const at = parseDate(authority?.as_of || positions.as_of);
    const minutes = at ? Math.max(0, Math.floor((Date.now() - at.getTime()) / 60000)) : null;
    el.textContent = authority?.freshness === "stale"
      ? minutes === null ? "Positions stale" : `Positions stale · ${ageLabel(minutes)}`
      : "Positions unavailable";
    el.title = positionsAuthorityView(positions, source).detail;
    return;
  }
  el.classList.remove("stale");
  renderFreshnessTimestamp(el, authority.as_of || positions.as_of, { staleMinutes: 15, quietWhenFresh: true, fallback: "Position time unavailable" });
}

function renderUnderlyingPnlSummary(totals) {
  setUnderlyingSummaryPnl("underlyingWinnerPnl", totals.winner, totals.winnerCurrency);
  setUnderlyingSummaryPnl("underlyingLoserPnl", totals.loser, totals.loserCurrency);
  // The winner/loser buckets and the brief's Movers row share one basis —
  const basis = $("underlyingPnlBasis");
  if (basis) {
    const hasTotals = hasNumericValue(totals.winner) || hasNumericValue(totals.loser);
    basis.hidden = !hasTotals;
    basis.textContent = `Daily P/L by underlying · all held names${marketSessionClosed() ? " · since last close" : ""}`;
  }
}

// The movers row on the Monitor face: the same daily-P/L-by-underlying basis
// disclosed as one residual clause so the row never implies it is the whole
// book. Money keeps the 60% tints and the account privacy mask.
function renderMovers(rows, baseCurrency) {
  const placard = $("moversPlacard");
  const strip = $("moversRow");
  if (!placard || !strip) return;
  const movers = moverRows(rows);
  // The movers row is also the Underlyings sheet's opener: on a flat or
  const heldBook = Array.isArray(rows) && rows.length > 0;
  placard.hidden = movers.length === 0 && !heldBook;
  strip.hidden = movers.length === 0 && !heldBook;
  if (movers.length === 0) {
    if (heldBook) {
      placard.textContent = "Daily P/L by name";
      const quietCell = document.createElement("b");
      const label = document.createElement("span");
      label.textContent = "no daily movement";
      quietCell.append(label);
      strip.replaceChildren(quietCell);
    } else {
      strip.replaceChildren();
    }
    return;
  }
  const currency = normalizeCurrency(movers[0].pnlCurrency || baseCurrency);
  placard.textContent = `Daily P/L by name${currency ? ` · ${currency}` : ""}`;
  const shown = movers.slice(0, 3);
  const rest = movers.slice(shown.length);
  const cells = shown.map((mover) => moverCell(mover.symbol, mover.pnl, mover.pnlCurrency || baseCurrency));
  if (rest.length > 0) {
    const residual = rest.reduce((sum, mover) => sum + mover.pnl, 0);
    const residualCurrency = rest.reduce((ccy, mover) => mergeCurrency(ccy, mover.pnlCurrency || baseCurrency), "");
    cells.push(moverCell(`+${rest.length} others`, residual, residualCurrency || baseCurrency));
  }
  strip.replaceChildren(...cells);
}

function moverRows(rows) {
  return (rows || [])
		.filter((row) => typeof row.pnl === "number" && row.pnl !== 0)
    .sort((a, b) => Math.abs(b.pnl) - Math.abs(a.pnl) || a.symbol.localeCompare(b.symbol));
}

function moverCell(label, value, currency) {
  const cell = document.createElement("b");
  const name = document.createElement("span");
  name.textContent = label;
  const amount = document.createElement("i");
  amount.className = sensitiveMoneyHidden(value) ? "is-private" : signedClass(value);
  amount.textContent = sensitiveMoneyHidden(value) ? privacyMask() : displayMoney(value, currency);
  cell.append(name, amount);
  return cell;
}


// marketSessionClosed reads the served official us-equity calendar (never the
// market-strip selection override). The broker's daily P/L does NOT freeze at
// next trading day — so closed-session totals are labeled since-close, never
function marketSessionClosed() {
  const session = state.snapshot?.market_calendar?.session;
  return Boolean(session && session.is_open === false);
}

function setUnderlyingSummaryPnl(id, value, currency) {
  const el = $(id);
  if (!el) return;
  if (!hasNumericValue(value)) {
    el.className = "signed";
    el.textContent = "--";
    return;
  }
  if (sensitiveMoneyHidden(value)) {
    el.className = "signed is-private";
    el.textContent = privacyMask();
    return;
  }
  el.className = signedClass(value);
  el.textContent = displayMoney(value, currency);
}

function underlyingHeldDailyPnlTotals(rows, baseCurrency) {
  const totals = {
    winner: null,
    winnerCurrency: "",
    loser: null,
    loserCurrency: "",
  };
  for (const row of rows) {
		if (typeof row.pnl !== "number" || row.pnl === 0) continue;
    if (row.pnl > 0) {
      totals.winner = (totals.winner || 0) + row.pnl;
      totals.winnerCurrency = mergeCurrency(totals.winnerCurrency, row.pnlCurrency || baseCurrency);
    } else {
      totals.loser = (totals.loser || 0) + row.pnl;
      totals.loserCurrency = mergeCurrency(totals.loserCurrency, row.pnlCurrency || baseCurrency);
    }
  }
  return {
    ...totals,
    winnerCurrency: totals.winnerCurrency || baseCurrency,
    loserCurrency: totals.loserCurrency || baseCurrency,
  };
}

function underlyingBookRows(positions, baseCurrency, marketEvents = {}) {
	return heldUnderlyingRows(positions, baseCurrency, marketEvents)
    .sort((a, b) => compareUnderlyingRows(a, b, state.positionsSort));
}

function setPositionsSort(sort) {
  state.positionsSort = normalizedPositionsSort(sort);
  localStorage.setItem("canaryPositionsSort", state.positionsSort);
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {}, state.snapshot?.market_events || {});
}

function setSelectedUnderlying(symbol) {
  const selected = normalizeSymbol(symbol);
  state.selectedUnderlying = state.selectedUnderlying === selected ? "" : selected;
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {}, state.snapshot?.market_events || {});
}

function compareUnderlyingRows(a, b, sort = state.positionsSort) {
  const byName = () => String(a.symbol || "").localeCompare(String(b.symbol || ""));
  const numeric = (left, right, direction = -1, absolute = false) => {
    const leftNumber = typeof left === "number" ? (absolute ? Math.abs(left) : left) : null;
    const rightNumber = typeof right === "number" ? (absolute ? Math.abs(right) : right) : null;
    if (leftNumber === null && rightNumber === null) return byName();
    if (leftNumber === null) return 1;
    if (rightNumber === null) return -1;
    return (leftNumber - rightNumber) * direction || byName();
  };
  switch (normalizedPositionsSort(sort)) {
    case "winners": return numeric(a.dailyPnl, b.dailyPnl, -1);
    case "losers": return numeric(a.dailyPnl, b.dailyPnl, 1);
    case "exposure": return numeric(a.marketValueBase, b.marketValueBase, -1, true);
    case "name": return byName();
    default: return numeric(a.dailyPnl, b.dailyPnl, -1, true);
  }
}

function heldUnderlyingRows(positions, baseCurrency, marketEvents = {}) {
  return (positions.by_underlying || []).map((group) => {
    const symbol = normalizeSymbol(group.underlying || group.stock?.symbol || group.options?.[0]?.symbol);
    if (!symbol) return null;
    const quoteState = underlyingMarketQuote(symbol);
    const quote = quoteState.quote;
    const price = heldUnderlyingPrice(group, quote);
    const currency = heldUnderlyingCurrency(group, quote, baseCurrency);
    const pnl = heldUnderlyingDailyPnl(group, baseCurrency, currency);
    const stockCount = group.stock ? 1 : 0;
    const optionCount = (group.options || []).length;
    const groupStrategies = (positions.strategies || []).filter((strategy) => normalizeSymbol(strategy.underlying) === symbol);
    const groupIssues = (positions.strategy_issues || []).filter((issue) => normalizeSymbol(issue.underlying) === symbol);
    const marketValueBase = typeof group.group_market_value_base === "number" ? group.group_market_value_base : null;
    const openPnlBase = typeof group.group_unrealized_pnl_base === "number" ? group.group_unrealized_pnl_base : null;
    const dollarDeltaBase = typeof group.group_dollar_delta_base === "number" ? group.group_dollar_delta_base : null;
    const row = {
      symbol,
      currency,
      price: price.value,
      priceSource: price.source,
      priceAt: price.at,
      change: heldUnderlyingChange(group, quote, price.value),
      changePct: heldUnderlyingChangePct(group, quote, price.value),
      pnl: pnl.value,
      pnlCurrency: pnl.currency,
      pnlSource: pnl.source,
      dailyPnl: pnl.value,
      dailyPnlCurrency: pnl.currency,
      quote,
      quoteError: quoteState.error,
      // Mirrors rpc.ExpectsMarketDataGroup: an option leg still needs a quote
      expectsQuote: optionCount > 0 || !group.stock || group.stock.quote_expectation !== "none",
      held: true,
      stockCount,
      optionCount,
      detail: underlyingPositionDetail(stockCount, optionCount),
      marketValue: marketValueBase ?? (currency !== "MIX" && typeof group.group_market_value_ccy === "number" ? group.group_market_value_ccy : null),
      marketValueBase,
      marketValueCurrency: marketValueBase !== null ? baseCurrency : currency,
      marketValuePctNlv: typeof group.group_market_value_pct_nlv === "number" ? group.group_market_value_pct_nlv : null,
      openPnl: openPnlBase ?? (currency !== "MIX" && typeof group.group_unrealized_pnl_ccy === "number" ? group.group_unrealized_pnl_ccy : null),
      openPnlCurrency: openPnlBase !== null ? baseCurrency : currency,
      effectiveDelta: typeof group.group_effective_delta === "number" ? group.group_effective_delta : null,
      dollarDelta: dollarDeltaBase ?? (typeof group.group_dollar_delta_ccy === "number" ? group.group_dollar_delta_ccy : null),
      dollarDeltaCurrency: dollarDeltaBase !== null ? baseCurrency : normalizeCurrency(group.group_dollar_delta_ccy_currency || currency),
      strategies: groupStrategies,
      strategyIssues: groupIssues,
      marketFlags: marketEventFlagsForSymbol(symbol, marketEvents),
    };
    row.quoteStatus = underlyingQuoteStatus(row);
    return row;
  }).filter(Boolean);
}

function heldUnderlyingPrice(group, quote) {
  const marketPrice = quotePrice(quote);
  if (typeof marketPrice === "number") {
    return { value: marketPrice, source: quoteSourceLabel(quote, "IBKR quote"), at: quoteTimestamp(quote) };
  }
  const stockPrice = firstNumber(group.stock?.quote_price, group.stock?.mark, group.stock?.valuation_mark);
  if (typeof stockPrice === "number") {
    const source = typeof group.stock?.quote_price === "number" ? "stock quote" : "account mark";
    return { value: stockPrice, source, at: group.stock?.quote_price_at || group.stock?.price_at || "" };
  }
  const optionUnderlying = firstNumber(...(group.options || []).map((option) => option.underlying));
  if (typeof optionUnderlying === "number") {
    return { value: optionUnderlying, source: "option model spot", at: "" };
  }
  return { value: null, source: "no price" };
}

function heldUnderlyingChangePct(group, quote, price) {
  const marketChange = quoteChangePct(quote);
  if (typeof marketChange === "number") return marketChange;
  const stockChange = firstNumber(group.stock?.quote_change_pct, group.stock?.regular_change_pct, group.stock?.day_change_pct);
  if (typeof stockChange === "number") return stockChange;
  const prevClose = firstNumber(...(group.options || []).map((option) => option.prev_close));
  if (typeof price === "number" && typeof prevClose === "number" && prevClose !== 0) {
    return (price - prevClose) / prevClose * 100;
  }
  return null;
}

function heldUnderlyingChange(group, quote, price) {
  const marketChange = quoteChange(quote);
  if (typeof marketChange === "number") return marketChange;
  const stockChange = firstNumber(group.stock?.quote_change, group.stock?.regular_change, group.stock?.day_change);
  if (typeof stockChange === "number") return stockChange;
  const prevClose = heldUnderlyingPrevClose(group, quote);
  if (typeof price === "number" && typeof prevClose === "number") {
    return price - prevClose;
  }
  return null;
}

function heldUnderlyingPrevClose(group, quote) {
  const marketPrevClose = quotePrevClose(quote);
  if (typeof marketPrevClose === "number") return marketPrevClose;
  const stockPrevClose = firstNumber(group.stock?.prev_close, group.stock?.regular_close, group.stock?.prior_regular_close);
  if (typeof stockPrevClose === "number") return stockPrevClose;
  return firstNumber(...(group.options || []).map((option) => option.prev_close));
}

function heldUnderlyingCurrency(group, quote, baseCurrency) {
  const quoteCurrency = normalizeCurrency(quote?.currency || quote?.contract?.currency);
  if (quoteCurrency) return quoteCurrency;
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  const currencies = [...new Set(rows.map((row) => normalizeCurrency(row.currency)).filter(Boolean))];
  if (currencies.length === 1) return currencies[0];
  if (currencies.length > 1) return "MIX";
  return baseCurrency;
}

function heldUnderlyingDailyPnl(group, baseCurrency, currency) {
  if (typeof group.group_daily_pnl_base === "number") {
    return { value: group.group_daily_pnl_base, currency: baseCurrency, source: "daily P/L" };
  }
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  if (rows.length > 0 && rows.every((row) => typeof row.daily_pnl_base === "number")) {
    return { value: rows.reduce((sum, row) => sum + row.daily_pnl_base, 0), currency: baseCurrency, source: "daily P/L" };
  }
  if (rows.length > 0 && rows.every((row) => typeof row.daily_pnl_ccy === "number")) {
    return { value: rows.reduce((sum, row) => sum + row.daily_pnl_ccy, 0), currency, source: "daily P/L" };
  }
  return { value: null, currency: baseCurrency, source: "daily P/L pending" };
}

function underlyingMarketQuote(symbol) {
  const marketQuotes = state.snapshot?.market_quotes || {};
  return {
    quote: quoteBySymbol(marketQuotes.quotes || {}, symbol),
    error: quoteErrorBySymbol(marketQuotes.errors || {}, symbol),
    marketQuotes,
  };
}

function quoteErrorBySymbol(errors, symbol) {
  if (!errors) return "";
  const target = normalizeSymbol(symbol);
  if (!target) return "";
  for (const [key, value] of Object.entries(errors)) {
    if (normalizeSymbol(key) === target) return String(value || "");
  }
  return "";
}

function underlyingQuoteSummary(rows) {
  const quoteRows = rows.filter((row) => (row.held || row.quote) && row.expectsQuote !== false);
  const interrupted = quoteRows.filter((row) => row.quoteError).map((row) => row.symbol);
  if (interrupted.length > 0) {
    return `Quote feed interrupted for ${humanList(interrupted, 3)}; showing frozen values`;
  }
  const quoted = quoteRows.filter((row) => typeof quotePrice(row.quote) === "number").length;
  if (quoted > 0) {
    return `Quotes updating for ${quoted}/${quoteRows.length} rows`;
  }
  return "";
}

function underlyingQuoteStatus(row) {
  const quote = row.quote || null;
  const error = String(row.quoteError || "").trim();
  const at = quoteTimestamp(quote) || row.priceAt || "";
  const atLabel = at ? quoteTime(at) : "";
  const dataType = String(quote?.data_type || "").toLowerCase();
  const quality = String(quote?.quote_quality || "").toLowerCase();
  const hasQuotePrice = typeof quotePrice(quote) === "number";
  const source = row.priceSource || quoteSourceLabel(quote, "IBKR quote");
  const sourceDetail = [source, atLabel].filter(Boolean).join(" · ");
  const frozenLabel = atLabel ? `Frozen · ${atLabel}` : "Frozen";
  const showSource = sourceDetail || "last available value";

  if (error) {
    return {
      tone: "error",
      label: typeof row.price === "number"
        ? atLabel ? `Frozen · ${atLabel}` : "Frozen"
        : "Feed issue",
      title: `${marketQuoteErrorLabel(error)}; showing ${showSource}`,
    };
  }
  if (quote?.stale || quality === "stale" || quality === "missing") {
    return {
      tone: "warn",
      label: atLabel ? `Stale · ${atLabel}` : "Stale",
      title: `${cleanDetail(quote.stale_reason || quality || "stale quote")}; showing ${showSource}`,
    };
  }
  if (dataType.includes("frozen")) {
    return {
      tone: "warn",
      label: frozenLabel,
      title: `Gateway is in ${labelize(dataType)} mode; showing ${showSource}`,
    };
  }
  if (dataType.includes("delayed")) {
    return {
      tone: "warn",
      label: atLabel ? `Delayed · ${atLabel}` : "Delayed",
      title: `Delayed market-data feed; showing ${showSource}`,
    };
  }
  if (quality && quality !== "firm") {
    return {
      tone: "warn",
      label: atLabel ? `${labelize(quality)} · ${atLabel}` : labelize(quality),
      title: `Quote quality ${labelize(quality)}; showing ${showSource}`,
    };
  }
  if (quote && hasQuotePrice) {
    return {
      tone: "ok",
      label: atLabel ? `Live · ${atLabel}` : "Live",
      title: `IBKR quote feed; showing ${showSource}`,
    };
  }
  if (typeof row.price === "number") {
    return {
      tone: "fallback",
      label: cleanDetail(source || "Position mark"),
      title: quote ? "Underlying quote has no current price yet; showing the latest position mark." : "No live underlying quote yet; showing the latest position mark.",
    };
  }
  return {
    tone: "error",
    label: "No price",
    title: "No quote or position mark is available for this underlying.",
  };
}

function underlyingBookRow(row, baseCurrency) {
  const selected = state.selectedUnderlying === row.symbol;
	const item = document.createElement("section");
	item.className = `underlying-row${selected ? " is-selected" : ""}`;
  if (row.quoteError) item.classList.add("underlying-row--quote-error");
  item.dataset.symbol = row.symbol;

  const summary = document.createElement("button");
  summary.type = "button";
  summary.className = "underlying-row__summary";
  summary.dataset.positionSelect = row.symbol;
  summary.setAttribute("aria-expanded", String(selected));
  summary.setAttribute("aria-label", `${selected ? "Hide" : "Show"} position context for ${row.symbol}`);

  const identity = document.createElement("span");
  identity.className = "underlying-row__identity";
  const title = document.createElement("span");
  title.className = "underlying-row__title";
  const symbol = document.createElement("strong");
  symbol.textContent = row.symbol;
	title.append(symbol);
  const detail = document.createElement("small");
  detail.textContent = row.detail;
  identity.append(title, detail);
  const flagRow = marketFlagRow(row.marketFlags || []);
  if (flagRow) identity.append(flagRow);

  const price = document.createElement("span");
  const quoteStatus = row.quoteStatus || underlyingQuoteStatus(row);
  price.className = "underlying-row__metric underlying-row__metric--quote quote-" + quoteStatus.tone;
  const priceLabel = document.createElement("span");
  priceLabel.className = "underlying-row__metric-label";
  priceLabel.textContent = "Quote";
  const priceValue = document.createElement("b");
  priceValue.textContent = displayMoney(row.price, row.currency);
  const priceNote = document.createElement("small");
  const changeTone = typeof row.change === "number" ? row.change : row.changePct;
  priceNote.className = signedClass(changeTone);
  priceNote.textContent = underlyingDayMoveText(row);
  price.append(priceLabel, priceValue, priceNote);

  const pnl = document.createElement("span");
  pnl.className = "underlying-row__metric underlying-row__metric--pnl";
  const pnlLabel = document.createElement("span");
  pnlLabel.className = "underlying-row__metric-label";
  pnlLabel.textContent = "Today";
  const pnlValue = document.createElement("b");
  pnlValue.className = sensitiveMoneyHidden(row.pnl) ? "is-private" : signedClass(row.pnl);
  pnlValue.textContent = sensitiveDisplayMoney(row.pnl, row.pnlCurrency || baseCurrency);
  const pnlNote = document.createElement("small");
  pnlNote.textContent = row.pnlSource || "Daily P/L";
  pnl.append(pnlLabel, pnlValue, pnlNote);

  const openPnl = document.createElement("span");
  openPnl.className = "underlying-row__metric underlying-row__metric--open";
  const openLabel = document.createElement("span");
  openLabel.className = "underlying-row__metric-label";
  openLabel.textContent = "Open";
  const openValue = document.createElement("b");
  openValue.className = sensitiveMoneyHidden(row.openPnl) ? "is-private" : signedClass(row.openPnl);
  openValue.textContent = sensitiveDisplayMoney(row.openPnl, row.openPnlCurrency || baseCurrency);
  const openNote = document.createElement("small");
  openNote.textContent = "Unrealized P/L";
  openPnl.append(openLabel, openValue, openNote);

  const disclosure = document.createElement("span");
  disclosure.className = "underlying-row__disclosure";
  disclosure.setAttribute("aria-hidden", "true");

  summary.append(identity, price, pnl, openPnl, disclosure);
  item.append(summary);
  if (selected) item.append(positionInspector(row, baseCurrency, quoteStatus));
	return item;
}

function underlyingDayMoveText(row) {
  const parts = [];
  const changeTone = typeof row.change === "number" ? row.change : row.changePct;
  if (typeof row.change === "number") parts.push(signedDisplayMoney(row.change, row.currency));
  if (typeof row.changePct === "number") parts.push(`(${signedPct(row.changePct)})`);
  return parts.length > 0 ? parts.join(" ") : changeTone === 0 ? "Flat today" : "Move unavailable";
}

function positionInspector(row, baseCurrency, quoteStatus) {
  const inspector = document.createElement("div");
  inspector.className = "position-inspector";

  const head = document.createElement("div");
  head.className = "position-inspector__head";
  const heading = document.createElement("span");
  heading.textContent = "Position context";
  const quality = document.createElement("span");
  quality.className = `underlying-quote-status ${quoteStatus.tone}`;
  quality.textContent = quoteStatus.label;
  quality.title = quoteStatus.title;
  head.append(heading, quality);

  const facts = document.createElement("div");
  facts.className = "position-inspector__facts";
  facts.append(
    positionContextFact(
      "Market value",
      sensitiveDisplayMoney(row.marketValue, row.marketValueCurrency || baseCurrency),
      typeof row.marketValuePctNlv === "number"
        ? `${sensitivePct(row.marketValuePctNlv)} of NLV`
        : "Share of NLV unavailable",
      sensitiveMoneyHidden(row.marketValue),
    ),
    positionContextFact(
      "Dollar delta",
      sensitiveDisplayMoney(row.dollarDelta, row.dollarDeltaCurrency || baseCurrency),
      typeof row.effectiveDelta === "number"
        ? `Effective delta ${sensitiveNumber(row.effectiveDelta)}`
        : "Effective delta unavailable",
      sensitiveMoneyHidden(row.dollarDelta),
    ),
    positionContextFact(
      "Composition",
      row.detail,
      positionGroupingSummary(row),
      false,
    ),
  );

  const distinction = document.createElement("p");
  distinction.className = "position-inspector__distinction";
  distinction.textContent = "Today is broker Daily P/L; Quote is the underlying's market move; Open is unrealized P/L since entry. Color shows direction, not an instruction to trade.";

  const actionArea = document.createElement("div");
  actionArea.className = "position-inspector__actions";
  const actionCopy = document.createElement("div");
  const actionLabel = document.createElement("span");
  actionLabel.textContent = "Available paths";
  const actionDetail = document.createElement("small");
  actionDetail.textContent = positionActionSummary(row);
  actionCopy.append(actionLabel, actionDetail);
  const actionButtons = document.createElement("div");
  actionButtons.className = "position-inspector__buttons";

  for (const strategy of (row.strategies || [])) {
    const strategyButton = document.createElement("button");
    strategyButton.type = "button";
    strategyButton.className = `position-inspector__action${strategy.actionable ? " position-inspector__action--primary" : ""}`;
    strategyButton.dataset.positionAction = "strategy";
    strategyButton.dataset.strategyId = strategy.id || "";
    strategyButton.textContent = strategy.actionable
      ? `Review ${labelize(strategy.kind || "option")} group`
      : "Review group blocker";
    actionButtons.append(strategyButton);
  }
  if ((row.strategies || []).length === 0 && (row.strategyIssues || []).length > 0) {
    const issueButton = document.createElement("button");
    issueButton.type = "button";
    issueButton.className = "position-inspector__action";
    issueButton.dataset.positionAction = "option-groups";
    issueButton.textContent = "Review grouping issue";
    actionButtons.append(issueButton);
  }
  const trimButton = document.createElement("button");
  trimButton.type = "button";
  trimButton.className = "position-inspector__action";
  trimButton.dataset.positionAction = "trim";
  trimButton.textContent = "Review portfolio trim";
  actionButtons.append(trimButton);
  actionArea.append(actionCopy, actionButtons);

  const scope = document.createElement("small");
  scope.className = "position-inspector__scope";
  scope.textContent = `Portfolio trim is a whole-book delta tool, not a recommendation or a ${row.symbol}-only order. Every route still begins with guarded preview.`;

  inspector.append(head, facts, distinction, actionArea, scope);
  return inspector;
}

function positionContextFact(labelText, valueText, noteText, hiddenValue) {
  const fact = document.createElement("div");
  fact.className = "position-inspector__fact";
  const label = document.createElement("span");
  label.textContent = labelText;
  const value = document.createElement("b");
  value.textContent = valueText;
  if (hiddenValue) value.classList.add("is-private");
  const note = document.createElement("small");
  note.textContent = noteText;
  fact.append(label, value, note);
  return fact;
}

function positionGroupingSummary(row) {
  const actionable = (row.strategies || []).filter((strategy) => strategy.actionable).length;
  const review = (row.strategies || []).length - actionable;
  if (actionable > 0) return `${actionable} daemon-grouped ${actionable === 1 ? "option path" : "option paths"}`;
  if ((row.strategyIssues || []).length > 0) return row.strategyIssues[0].reason || "Option grouping needs review";
  if (review > 0) return row.strategies[0].reason || "Grouped option action needs review";
  return row.optionCount > 0 ? "No safe combo group is currently served" : "No option legs";
}

function positionActionSummary(row) {
  const actionable = (row.strategies || []).filter((strategy) => strategy.actionable).length;
  if (actionable > 0) return "An exact grouped option review is available; the portfolio trim remains global.";
  if ((row.strategyIssues || []).length > 0) return "Canary cannot safely group every option leg; review the typed reason before acting elsewhere.";
  if ((row.strategies || []).length > 0) return "A served option group is blocked; review its daemon-authored reason before acting elsewhere.";
  return "No position-specific reduction is inferred here. The portfolio trim is a separate whole-book review.";
}

function sensitivePct(value) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? pct(value) : privacyMask();
}

function sensitiveNumber(value) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? numberRead(value) : privacyMask();
}

function quoteSourceLabel(quote, fallback) {
  const dataType = String(quote?.data_type || "").trim();
  if (!dataType || dataType === "live") return fallback;
  return labelize(dataType) + " quote";
}

function underlyingPositionDetail(stockCount, optionCount) {
  const parts = [];
  if (stockCount > 0) parts.push(`${stockCount} stock ${stockCount === 1 ? "leg" : "legs"}`);
  if (optionCount > 0) parts.push(`${optionCount} option ${optionCount === 1 ? "leg" : "legs"}`);
  return parts.length ? parts.join(" / ") : "Held position";
}

function currentAccountContext(account = {}) {
  const trading = state.snapshot?.trading || {};
  const status = state.snapshot?.status || {};
  const positions = state.snapshot?.positions || {};
  const accountScope = accountAuthority(account)?.scope || {};
  const positionsScope = positions.authority?.scope || {};
  const concrete = (value) => {
    const text = String(value || "").trim();
    return text && text.toLowerCase() !== "all" ? text : "";
  };
  const scopedAccount = concrete(accountScope.account_id);
  const scopedPositionsAccount = concrete(positionsScope.account_id);
  const accountMode = String(accountScope.account_mode || "").trim().toLowerCase();
  const positionsMode = String(positionsScope.account_mode || "").trim().toLowerCase();
  const scopesConflict = Boolean(
    (scopedAccount && scopedPositionsAccount && scopedAccount.toLowerCase() !== scopedPositionsAccount.toLowerCase()) ||
    (accountMode && positionsMode && accountMode !== positionsMode),
  );
  const hasTypedScope = Boolean(accountAuthority(account) || positions.authority);
  const connectedAccount = hasTypedScope ? "" : concrete(status.connected_account);
  const accountLabel = scopesConflict ? "" : scopedAccount || scopedPositionsAccount || connectedAccount;
  const modeSource = [
    accountScope.account_mode,
    positionsScope.account_mode,
    status.account_mode,
    trading.mode,
    status.trading?.mode,
  ].map((value) => String(value || "").trim()).find((value) => /paper|live/i.test(value));
  const modeLabel = modeSource
    ? modeSource.toLowerCase().includes("paper") ? "Paper" : "Live"
    : "IBKR";
  const visibleAccountLabel = accountLabel || (scopesConflict ? "Account mismatch" : "Account unresolved");
  return {
    // accountId is the concrete broker id (masked by the eye toggle); it is
    accountId: accountLabel,
    accountLabel: visibleAccountLabel,
    modeClass: String(modeLabel).toLowerCase().includes("paper") ? "paper" : String(modeLabel).toLowerCase().includes("live") ? "live" : "neutral",
    modeLabel,
    hasAccount: Boolean(accountLabel),
  };
}

// Operator decision (supersedes the earlier hide-in-live stance): the account
// simply not real money. An unresolved mode renders a muted "mode?" — fail
// visible, never silently resemble live.
function renderTradingEnvPill(modeClass) {
  const pill = $("tradingEnvPill");
  if (!pill) return;
  pill.hidden = false;
  if (modeClass === "paper") {
    pill.textContent = "PAPER";
    pill.className = "trading-env-pill trading-env-pill--paper";
    pill.title = "Paper trading — portfolio data is not real money.";
    return;
  }
  if (modeClass === "live") {
    pill.textContent = "LIVE";
    pill.className = "trading-env-pill trading-env-pill--live";
    pill.title = "Live trading — these are real positions and real money.";
    return;
  }
  pill.textContent = "mode?";
  pill.className = "trading-env-pill trading-env-pill--unknown";
  pill.title = "Trading environment could not be resolved.";
}

export { accountAuthorityReason, accountDailyPnlPct, accountDailyPnlValue, compareUnderlyingRows, currentAccountContext, exposureMetricRow, heldStressMetricRow, heldUnderlyingChange, heldUnderlyingChangePct, heldUnderlyingCurrency, heldUnderlyingDailyPnl, heldUnderlyingPrevClose, heldUnderlyingPrice, heldUnderlyingRows, maskedRiskMoney, moverCell, moverRows, positionsAuthorityView, quoteErrorBySymbol, quoteSourceLabel, renderAccountDailyPnlPct, renderAccountFreshness, renderAccountLargestExposure, renderAccountPanel, renderDeltaTile, renderMovers, renderPositionsFreshness, renderUnderlyingPnlSummary, renderUnderlyings, setPositionsSort, setSelectedUnderlying, setUnderlyingSummaryPnl, underlyingBookRow, underlyingBookRows, underlyingHeldDailyPnlTotals, underlyingMarketQuote, underlyingPositionDetail, underlyingQuoteStatus, underlyingQuoteSummary };
