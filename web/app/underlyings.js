import { heldStressEvidence, heldStressItems, humanList, marketQuoteErrorLabel, quoteBySymbol, quoteChange, quoteChangePct, quotePrevClose, quotePrice, quoteTime } from "./stress.js";
import { marketEventFlagsForSymbol, marketFlagRow, renderMarketFlagRail, underlyingHeroMarketFlags } from "./market-events.js";
import { $, accountAuthority, accountBaseCurrency, accountFieldAvailable, accountFieldValue, ageLabel, cleanDetail, compactMoney, displayMoney, firstNumber, hasNumericValue, labelize, mergeCurrency, normalizeCurrency, normalizeSymbol, parseDate, pct, privacyMask, purgeRestoreSettingEnabled, quoteTimestamp, renderFreshnessTimestamp, renderSensitiveAccountId, renderSensitiveText, riskMoney, sensitiveDisplayMoney, sensitiveMoneyHidden, shortTime, signedClass, signedDisplayMoney, signedPct } from "./shared.js";
import { state } from "./state.js";

function renderAccountPanel(account = {}, positions = {}, stress = {}) {
  const detail = $("accountOverviewDetail");
  const detailToggle = $("accountOverviewToggle");
  detail.hidden = !state.accountOverviewOpen;
  detailToggle.textContent = state.accountOverviewOpen ? "Hide detail" : "Detail";
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
  // money values are the headline. An unresolved placeholder renders
  // plainly since it is not a sensitive id.
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
  renderDeltaTile(portfolio, baseCurrency);
}


// The Net $ Delta window is a flush readout: no lamp slot, because a delta is
// a reading, not an alarm. Every figure is a served portfolio field and
// stays behind the account privacy mask.
function renderDeltaTile(portfolio = {}, baseCurrency = "") {
  const lead = $("deltaTileLead");
  const sub = $("deltaTileSub");
  if (!lead || !sub) return;
  const delta = portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy;
  const theta = portfolio.daily_theta_base ?? portfolio.daily_theta_ccy;
  lead.textContent = [
    maskedRiskMoney(delta, portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency),
    `theta ${maskedRiskMoney(theta, portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency)}/d`,
  ].join(" · ");
  const largest = (portfolio.exposure_base || [])[0];
  const top = largest?.underlying
    ? `top ${largest.underlying}${typeof largest.market_value_pct_nlv === "number" ? ` ${pct(largest.market_value_pct_nlv)}` : ""}`
    : "top --";
  sub.textContent = [
    `FX1% ${maskedRiskMoney(portfolio.fx_sensitivity_per_pct, portfolio.fx_base_currency || baseCurrency)}`,
    top,
  ].join(" · ");
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
  const heldCount = rows.filter((row) => !row.virtual).length;
  const virtualCount = rows.length - heldCount;
  const count = $("underlyingBookCount");
  const status = $("underlyingBookStatus");
  const freshness = $("underlyingBookFreshness");
  const authorityView = positionsAuthorityView(positions, state.snapshot?.sources?.positions || {});
  const heldSymbols = rows.filter((row) => !row.virtual).slice(0, 3).map((row) => row.symbol);
  const heldLabel = heldSymbols.length > 0 ? ` · ${heldSymbols.join(", ")}${heldCount > heldSymbols.length ? ` +${heldCount - heldSymbols.length}` : ""}` : "";
  const quoteSummary = underlyingQuoteSummary(rows);
  renderUnderlyingPnlSummary(authorityView.available ? underlyingHeldDailyPnlTotals(rows, baseCurrency) : {});
  renderMovers(authorityView.available ? rows : [], baseCurrency);
  renderMarketFlagRail("underlyingFlagRail", underlyingHeroMarketFlags(rows, marketEvents));
  if (count) {
    count.textContent = !authorityView.available
      ? rows.length === 0 ? "Positions unavailable" : `${heldCount} last known / ${virtualCount} purged${heldLabel}`
      : rows.length === 0
      ? "No underlyings"
      : `${heldCount} held / ${virtualCount} purged${heldLabel}`;
  }
  if (status) {
    status.textContent = state.underlyingNotice
      || (!authorityView.available ? authorityView.detail : "")
      || quoteSummary
      || (virtualCount > 0 ? "Includes virtual purge-book records" : heldCount > 0 ? "Current held underlyings" : "Waiting for positions or purge book");
  }
  if (freshness) {
    renderPositionsFreshness(freshness, positions, state.snapshot?.sources?.positions || {});
  }
  const panel = $("underlyingPanel");
  if (panel && (state.underlyingBusy || state.underlyingNotice)) {
    state.underlyingDetailOpen = true;
  }
  renderUnderlyingBulkActions(rows);

  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "underlying-book__empty";
    empty.textContent = authorityView.available ? "No held or virtual underlyings." : "Position data unavailable.";
    list.replaceChildren(empty);
    renderUnderlyingExpansion();
    return;
  }

  list.replaceChildren(...rows.map((row) => underlyingBookRow(row, baseCurrency)));
  renderUnderlyingExpansion();
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

function renderUnderlyingBulkActions(rows) {
  const heldCount = rows.filter((row) => !row.virtual).length;
  const virtualCount = rows.length - heldCount;
  const trading = state.snapshot?.trading || {};
  setUnderlyingActionButtonState("buildAllUnderlyingsButton", false, underlyingWriteReason("Review restore", virtualCount > 0, trading));
  setUnderlyingActionButtonState("purgeAllUnderlyingsButton", false, underlyingWriteReason("Purge all held underlyings", heldCount > 0, trading));
  setUnderlyingActionButtonState("restoreAllUnderlyingsButton", false, underlyingWriteReason("Restore all purged rows", virtualCount > 0, trading));
  // Tooltips are invisible on touch; when every bulk action is disabled, say
  // why in one muted line instead of presenting three dead buttons.
  const bulkNote = $("underlyingBulkNote");
  if (bulkNote) {
    const allDisabled = ["buildAllUnderlyingsButton", "purgeAllUnderlyingsButton", "restoreAllUnderlyingsButton"]
      .every((id) => $(id)?.disabled);
    bulkNote.hidden = !allDisabled;
    if (allDisabled) {
      bulkNote.textContent = heldCount > 0
        ? underlyingWriteReason("Purge all held underlyings", true, trading)
        : "No held or purged underlyings to act on";
    }
  }
}

function setUnderlyingActionButtonState(id, enabled, reason) {
  const button = $(id);
  if (!button) return;
  button.disabled = !enabled;
  button.title = enabled ? reason : reason || "Unavailable";
}

function renderUnderlyingPnlSummary(totals) {
  setUnderlyingSummaryPnl("underlyingWinnerPnl", totals.winner, totals.winnerCurrency);
  setUnderlyingSummaryPnl("underlyingLoserPnl", totals.loser, totals.loserCurrency);
  // The winner/loser buckets and the brief's Movers row share one basis —
  // daily P/L attributed per underlying — and this line says so on screen,
  // with the session context, so the two surfaces reconcile by inspection.
  const basis = $("underlyingPnlBasis");
  if (basis) {
    const hasTotals = hasNumericValue(totals.winner) || hasNumericValue(totals.loser);
    basis.hidden = !hasTotals;
    basis.textContent = `Daily P/L by underlying · all held names${marketSessionClosed() ? " · since last close" : ""}`;
  }
}

// The movers row on the Monitor face: the same daily-P/L-by-underlying basis
// the Underlyings hero uses, largest movement first, with the remainder
// disclosed as one residual clause so the row never implies it is the whole
// book. Money keeps the 60% tints and the account privacy mask.
function renderMovers(rows, baseCurrency) {
  const placard = $("moversPlacard");
  const strip = $("moversRow");
  if (!placard || !strip) return;
  const movers = moverRows(rows);
  // The movers row is also the Underlyings sheet's opener: on a flat or
  // all-purged book the row would hide and strand the book behind no
  // control at all, so a held book keeps the placard with an honest
  // no-movement reading instead.
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
    .filter((row) => !row.virtual && typeof row.pnl === "number" && row.pnl !== 0)
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
// the close — it keeps moving on extended/overnight marks and rolls to the
// next trading day — so closed-session totals are labeled since-close, never
// as a completed-session result.
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
    if (row.virtual || typeof row.pnl !== "number" || row.pnl === 0) continue;
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

function setUnderlyingExpansion(open) {
  state.underlyingDetailOpen = Boolean(open);
  renderUnderlyingExpansion();
}

function renderUnderlyingExpansion() {
  const panel = $("underlyingPanel");
  const listPanel = $("underlyingBookListPanel");
  const button = $("underlyingDetailToggle");
  if (!panel || !listPanel || !button) return;
  panel.dataset.open = String(state.underlyingDetailOpen);
  listPanel.hidden = !state.underlyingDetailOpen;
  button.textContent = state.underlyingDetailOpen ? "Hide underlyings" : "Show underlyings";
  button.setAttribute("aria-expanded", String(state.underlyingDetailOpen));
}

function canWriteUnderlyings(trading = {}) {
	void trading;
	return false;
}

function underlyingWriteReason(action, hasRows, trading = {}) {
	void action;
	void trading;
  if (!hasRows) return "No matching underlying rows";
  if (!purgeRestoreSettingEnabled()) return "Purge/restore workflow is hidden in Settings";
  return "Purge/restore submission is unavailable; use TWS, then refresh and reconcile Canary";
}

function underlyingBookRows(positions, baseCurrency, marketEvents = {}) {
  const rows = new Map();
  for (const row of heldUnderlyingRows(positions, baseCurrency, marketEvents)) {
    rows.set(row.symbol, row);
  }
  for (const row of purgedUnderlyingRows(positions, baseCurrency, marketEvents)) {
    const existing = rows.get(row.symbol);
    if (existing) {
      existing.hasPurgeRecord = true;
      existing.purgeLabel = row.purgeLabel;
      continue;
    }
    rows.set(row.symbol, row);
  }
  return [...rows.values()].sort(compareUnderlyingRows);
}

function compareUnderlyingRows(a, b) {
  if (a.virtual !== b.virtual) return a.virtual ? 1 : -1;
  const aPnl = underlyingSortPnl(a);
  const bPnl = underlyingSortPnl(b);
  const aRank = underlyingPnlSortRank(aPnl);
  const bRank = underlyingPnlSortRank(bPnl);
  if (aRank !== bRank) return aRank - bRank;
  if (aRank === 0) return aPnl - bPnl || a.symbol.localeCompare(b.symbol);
  if (aRank === 1) return bPnl - aPnl || a.symbol.localeCompare(b.symbol);
  return a.symbol.localeCompare(b.symbol);
}

function underlyingSortPnl(row) {
  return row.virtual ? row.pnl : row.dailyPnl;
}

function underlyingPnlSortRank(value) {
  if (typeof value !== "number" || value === 0) return 2;
  return value < 0 ? 0 : 1;
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
      // even when its underlying stock is defunct.
      expectsQuote: optionCount > 0 || !group.stock || group.stock.quote_expectation !== "none",
      held: true,
      virtual: false,
      purged: false,
      stockCount,
      optionCount,
      detail: underlyingPositionDetail(stockCount, optionCount),
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

function purgedUnderlyingRows(positions, baseCurrency, marketEvents = {}) {
  const rows = new Map();
  for (const entry of purgeBookEntries(positions)) {
    const symbol = normalizeSymbol(entry.underlying || entry.symbol || entry.ticker || entry.contract?.symbol);
    if (!symbol) continue;
    const quoteState = underlyingMarketQuote(symbol);
    const row = rows.get(symbol) || {
      symbol,
      currency: "",
      price: null,
      priceSource: "",
      priceAt: "",
      change: null,
      changePct: null,
      pnl: null,
      pnlCurrency: "",
      pnlSource: "shadow P/L",
      quote: quoteState.quote,
      quoteError: quoteState.error,
      virtual: true,
      purged: true,
      held: false,
      legCount: 0,
      purgeIDs: new Set(),
      detail: "",
      marketFlags: marketEventFlagsForSymbol(symbol, marketEvents),
    };
    const currency = normalizeCurrency(entry.currency || entry.trading_currency || entry.contract?.currency || entry.base_currency);
    if (currency) {
      row.currency = mergeCurrency(row.currency, currency);
    }
    if (quoteState.quote) {
      row.quote = quoteState.quote;
      const marketPrice = quotePrice(quoteState.quote);
      if (typeof marketPrice === "number") {
        row.price = marketPrice;
        row.priceSource = quoteSourceLabel(quoteState.quote, "IBKR quote");
        row.priceAt = quoteTimestamp(quoteState.quote);
      }
      const quotePct = quoteChangePct(quoteState.quote);
      if (typeof quotePct === "number") {
        row.changePct = quotePct;
      }
      const marketChange = quoteChange(quoteState.quote);
      if (typeof marketChange === "number") {
        row.change = marketChange;
      }
      const quoteCurrency = normalizeCurrency(quoteState.quote.currency || quoteState.quote.contract?.currency);
      if (quoteCurrency) {
        row.currency = mergeCurrency(row.currency, quoteCurrency);
      }
    }
    if (quoteState.error) {
      row.quoteError = quoteState.error;
    }
    const price = firstNumber(entry.current_price, entry.quote_price, entry.price, entry.last_price, entry.mark, entry.underlying, entry.reference_price);
    if (typeof price === "number" && row.price === null) {
      row.price = price;
      row.priceSource = entry.current_price_source || entry.quote_price_source || entry.price_source || "purge book";
    }
    const change = firstNumber(entry.quote_change_pct, entry.change_pct, entry.day_change_pct, entry.regular_change_pct);
    if (typeof change === "number" && row.changePct === null) {
      row.changePct = change;
    }
    const absoluteChange = firstNumber(entry.quote_change, entry.change, entry.day_change, entry.regular_change);
    if (typeof absoluteChange === "number" && row.change === null) {
      row.change = absoluteChange;
    }
    const pnl = purgeEntryPnl(entry);
    if (typeof pnl.value === "number") {
      row.pnl = (row.pnl || 0) + pnl.value;
      row.pnlCurrency = mergeCurrency(row.pnlCurrency, pnl.currency || currency || baseCurrency);
      row.pnlSource = pnl.source;
    }
    if (entry.purge_id) row.purgeIDs.add(String(entry.purge_id));
    row.legCount += Number(entry.leg_count || 1);
    rows.set(symbol, row);
  }
  return [...rows.values()].map((row) => {
    const out = {
      ...row,
      currency: row.currency || row.pnlCurrency || baseCurrency,
      pnlCurrency: row.pnlCurrency || row.currency || baseCurrency,
      priceSource: row.priceSource || "purge book",
      purgeLabel: row.purgeIDs.size > 0 ? [...row.purgeIDs].slice(0, 2).join(", ") : "purge book",
      detail: `${row.legCount} purged ${row.legCount === 1 ? "leg" : "legs"}`,
    };
    out.marketFlags = marketEventFlagsForSymbol(out.symbol, marketEvents);
    out.quoteStatus = underlyingQuoteStatus(out);
    return out;
  });
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

function purgeBookEntries(positions = {}) {
  const out = [];
  const candidates = [
    state.snapshot?.purge_book,
    state.snapshot?.purge_books,
    state.snapshot?.purged_underlyings,
    state.snapshot?.purged_positions,
    state.latestPurgeStatus,
    positions.purge_book,
    positions.purge_books,
    positions.purged_underlyings,
    positions.purged_positions,
    readLocalPurgeBook(),
  ];
  for (const candidate of candidates) {
    collectPurgeEntries(candidate, out, {});
  }
  return out;
}

function collectPurgeEntries(candidate, out, context) {
  if (!candidate) return;
  if (Array.isArray(candidate)) {
    candidate.forEach((item) => collectPurgeEntries(item, out, context));
    return;
  }
  if (typeof candidate !== "object") return;

  const next = {
    purge_id: candidate.purge_id || context.purge_id,
    base_currency: candidate.base_currency || context.base_currency,
  };
  for (const key of ["books", "underlyings", "positions", "rows"]) {
    if (Array.isArray(candidate[key])) {
      candidate[key].forEach((item) => collectPurgeEntries(item, out, next));
    }
  }
  if (Array.isArray(candidate.legs)) {
    candidate.legs.forEach((leg) => out.push({ ...leg, ...next }));
  }
  if (candidate.symbol || candidate.underlying || candidate.ticker || candidate.contract?.symbol) {
    out.push({ ...candidate, ...next });
  }
}

function readLocalPurgeBook() {
  for (const key of ["ibkrPurgeBook", "ibkrPurgeBooks"]) {
    const raw = localStorage.getItem(key);
    if (!raw) continue;
    try {
      return JSON.parse(raw);
    } catch {
      continue;
    }
  }
  return null;
}

function purgeEntryPnl(entry) {
  const direct = firstNumber(entry.current_shadow_pnl, entry.shadow_pnl, entry.group_unrealized_pnl_base, entry.unrealized_pnl_base, entry.pnl_base, entry.pnl, entry.shadow_saved);
  const currency = normalizeCurrency(entry.pnl_currency || entry.base_currency || entry.currency || entry.contract?.currency);
  if (typeof direct === "number") {
    const shadow = typeof entry.current_shadow_pnl === "number" || typeof entry.shadow_pnl === "number" || typeof entry.shadow_saved === "number";
    return { value: direct, currency, source: shadow ? "shadow P/L" : "unrealized P/L" };
  }
  const restore = firstNumber(entry.current_restore_value, entry.estimated_value);
  const exit = firstNumber(entry.exit_value);
  if (typeof restore === "number" && typeof exit === "number") {
    return { value: exit - restore, currency, source: "shadow P/L" };
  }
  return { value: null, currency, source: "no P/L" };
}

function underlyingBookRow(row, baseCurrency) {
  const item = document.createElement("div");
  item.className = "underlying-row" + (row.virtual ? " underlying-row--virtual" : "") + (row.hasPurgeRecord ? " underlying-row--book" : "");
  if (row.quoteError) item.classList.add("underlying-row--quote-error");
  item.dataset.symbol = row.symbol;

  const identity = document.createElement("div");
  identity.className = "underlying-row__identity";
  const title = document.createElement("div");
  title.className = "underlying-row__title";
  const symbol = document.createElement("strong");
  symbol.textContent = row.symbol;
  title.append(symbol, ...underlyingMarkers(row));
  const detail = document.createElement("small");
  detail.textContent = row.detail;
  identity.append(title, detail);
  const flagRow = marketFlagRow(row.marketFlags || []);
  if (flagRow) identity.append(flagRow);

  const price = document.createElement("div");
  const quoteStatus = row.quoteStatus || underlyingQuoteStatus(row);
  price.className = "underlying-row__metric underlying-row__metric--quote quote-" + quoteStatus.tone;
  const priceValue = document.createElement("b");
  priceValue.textContent = displayMoney(row.price, row.currency);
  const priceNote = document.createElement("small");
  priceNote.className = "underlying-quote-status " + quoteStatus.tone;
  priceNote.textContent = quoteStatus.label;
  priceNote.title = quoteStatus.title;
  price.append(priceValue, priceNote);

  const change = document.createElement("div");
  change.className = "underlying-row__metric underlying-row__metric--change";
  const changeValue = document.createElement("b");
  const changeTone = typeof row.change === "number" ? row.change : row.changePct;
  changeValue.className = signedClass(changeTone);
  changeValue.textContent = signedDisplayMoney(row.change, row.currency);
  const changeNote = document.createElement("small");
  changeNote.className = signedClass(row.changePct);
  changeNote.textContent = typeof row.changePct === "number" ? `${signedPct(row.changePct)} day` : "% change";
  change.append(changeValue, changeNote);

  const pnl = document.createElement("div");
  pnl.className = "underlying-row__metric underlying-row__metric--pnl";
  const pnlValue = document.createElement("b");
  pnlValue.className = sensitiveMoneyHidden(row.pnl) ? "is-private" : signedClass(row.pnl);
  pnlValue.textContent = sensitiveDisplayMoney(row.pnl, row.pnlCurrency || baseCurrency);
  const pnlNote = document.createElement("small");
  pnlNote.textContent = row.pnlSource || "P/L";
  pnl.append(pnlValue, pnlNote);

  const actions = document.createElement("div");
  actions.className = "underlying-row__actions";
  actions.append(
    underlyingActionButton("Purge", !row.virtual, row, "purge"),
    underlyingActionButton("Restore", row.virtual, row, "restore"),
    underlyingActionButton("Build", row.virtual, row, "build"),
  );

  item.append(identity, price, change, pnl, actions);
  return item;
}

function underlyingMarkers(row) {
  const markers = [];
  if (row.virtual) {
    markers.push(underlyingMarker("Virtual", "virtual"));
    markers.push(underlyingMarker("Purged", "purged"));
  } else if (row.hasPurgeRecord) {
    markers.push(underlyingMarker("Book", "book"));
  }
  return markers;
}

function underlyingMarker(label, tone) {
  const marker = document.createElement("span");
  marker.className = "underlying-marker underlying-marker--" + tone;
  marker.textContent = label;
  return marker;
}

function underlyingActionButton(label, enabled, row, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "underlying-action underlying-action--" + action;
  button.textContent = label;
  const trading = state.snapshot?.trading || {};
  const writeAction = action === "purge" || action === "restore";
  const available = false;
  button.disabled = !available;
  const disabledReason = row.virtual
    ? "Already in the purge book; restore or build is available."
    : "Available after this underlying has been purged.";
  button.title = available
    ? underlyingActionTitle(label, row, action)
    : underlyingWriteReason(`${label} ${row.symbol}`, enabled, trading) || disabledReason;
  button.setAttribute("aria-label", `${label} ${row.symbol}`);
  if (available) {
    button.addEventListener("click", () => {
      runUnderlyingAction(action, { symbols: [row.symbol] });
    });
  }
  return button;
}

function underlyingActionTitle(label, row, action) {
	void label;
	void row;
	void action;
	return "Purge/restore submission is unavailable; use TWS, then refresh and reconcile Canary";
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
    // empty for an unresolved/mismatch placeholder, which is not sensitive.
    accountId: accountLabel,
    accountLabel: visibleAccountLabel,
    modeClass: String(modeLabel).toLowerCase().includes("paper") ? "paper" : String(modeLabel).toLowerCase().includes("live") ? "live" : "neutral",
    modeLabel,
    hasAccount: Boolean(accountLabel),
  };
}

// Operator decision (supersedes the earlier hide-in-live stance): the account
// strip ALWAYS states the mode. A plate that goes silent in live asks the
// reader to infer the loudest fact on the panel from an absence.
//
// LIVE reads in the plate's own engraved register — it is the normal state,
// and normal states do not shout. PAPER is the one deliberate identity
// exception: red field, white text, the TWS simulated-trading convention a
// trader already reads without thinking. It is a dedicated modifier, not a
// severity lamp: nothing is wrong when the desk is on paper, the numbers are
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

async function refreshPurgeStatus() {
  try {
    const res = await fetch("/api/purge/status", { credentials: "include" });
    if (!res.ok) return;
    state.latestPurgeStatus = await res.json();
    renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  } catch {
    // Purge status is secondary context; live positions and trading remain primary.
  }
}

async function runUnderlyingAction(action, target = {}) {
  const all = Boolean(target.all);
  const symbols = (target.symbols || []).map(normalizeSymbol).filter(Boolean);
  const label = underlyingActionLabel(action, all, symbols);
	state.underlyingNotice = `${label}: purge/restore submission is unavailable; use TWS, then refresh and reconcile Canary.`;
	renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
}

function underlyingActionEndpoint(action) {
  if (action === "build") return "/api/purge/restore/preview";
  if (action === "restore") return "/api/purge/restore/execute";
  return "/api/purge/execute";
}

function underlyingActionLabel(action, all, symbols) {
  const target = all ? "all" : symbols.join(", ") || "selection";
  if (action === "build") return `Build ${target}`;
  if (action === "restore") return `Restore ${target}`;
  return `Purge ${target}`;
}

function underlyingWriteConfirmation(action, label) {
	void action;
  const trading = state.snapshot?.trading || {};
  state.underlyingNotice = underlyingWriteReason(label, true, trading);
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  return null;
}

function purgeResultSummary(result = {}) {
  const status = result.status || "ok";
  const selected = Number(result.selected_legs || 0);
  const submitted = Number(result.submitted_legs || 0);
  const skipped = Number(result.skipped_legs || 0);
  const errors = Number(result.error_legs || 0);
  const message = result.message ? ` / ${result.message}` : "";
  const preview = result.kind === "ibkr.purge_restore_preview" ? "draft" : status;
  return `${preview}; ${selected} selected, ${submitted} submitted, ${skipped} skipped, ${errors} errors${message}`;
}

function renderUnderlyingActionResult(result = {}) {
  const panel = $("underlyingActionResult");
  if (!panel) return;
  panel.hidden = false;
  panel.className = "underlying-action-result " + (result.status === "error" || result.error_legs > 0 ? "risk" : "neutral");
  const lines = [];
  if (result.message) lines.push(result.message);
  if ((result.blockers || []).length > 0) {
    lines.push(...result.blockers.map((blocker) => blocker.message || blocker.code).filter(Boolean));
  }
  if ((result.skipped || []).length > 0) {
    lines.push(...result.skipped.slice(0, 3).map((row) => `${row.symbol || row.leg_id}: ${row.reason}`));
  }
  panel.textContent = lines.join(" / ") || purgeResultSummary(result);
}

export { accountAuthorityReason, accountDailyPnlPct, accountDailyPnlValue, canWriteUnderlyings, collectPurgeEntries, compareUnderlyingRows, currentAccountContext, exposureMetricRow, heldStressMetricRow, heldUnderlyingChange, heldUnderlyingChangePct, heldUnderlyingCurrency, heldUnderlyingDailyPnl, heldUnderlyingPrevClose, heldUnderlyingPrice, heldUnderlyingRows, maskedRiskMoney, moverCell, moverRows, positionsAuthorityView, purgeBookEntries, purgeEntryPnl, purgeResultSummary, purgedUnderlyingRows, quoteErrorBySymbol, quoteSourceLabel, readLocalPurgeBook, refreshPurgeStatus, renderAccountDailyPnlPct, renderAccountFreshness, renderAccountLargestExposure, renderAccountPanel, renderDeltaTile, renderMovers, renderPositionsFreshness, renderUnderlyingActionResult, renderUnderlyingBulkActions, renderUnderlyingExpansion, renderUnderlyingPnlSummary, renderUnderlyings, runUnderlyingAction, setUnderlyingActionButtonState, setUnderlyingExpansion, setUnderlyingSummaryPnl, underlyingActionButton, underlyingActionEndpoint, underlyingActionLabel, underlyingActionTitle, underlyingBookRow, underlyingBookRows, underlyingHeldDailyPnlTotals, underlyingMarker, underlyingMarkers, underlyingMarketQuote, underlyingPnlSortRank, underlyingPositionDetail, underlyingQuoteStatus, underlyingQuoteSummary, underlyingSortPnl, underlyingWriteConfirmation, underlyingWriteReason };
