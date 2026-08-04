import { protectionCoverageDetailFact } from "./protection-coverage.js";
import { $, accountBaseCurrency, accountFieldValue, cleanDetail, hasNumericValue, labelize, money, pct, renderSensitiveText, riskMoney, sensitiveDisplayMoney, sensitiveMoney, sensitiveMoneyHidden, signedClass, signedTone, wholePct } from "./shared.js";
import { greeksCoverage, greeksMeaning } from "./shell.js";
import { state } from "./state.js";
import { positionsAuthorityView } from "./underlyings.js";

function renderPortfolioRisk(positions, account) {
  const authority = positionsAuthorityView(positions, state.snapshot?.sources?.positions || {});
  if (!authority.available) {
    renderPortfolioUnavailable(authority);
    return;
  }
  const portfolio = positions.portfolio || {};
  const baseCurrency = portfolio.base_currency || accountBaseCurrency(account);
  renderPortfolioDeltaPosture(portfolio, account);
  // These two spans are screen-reader-only; without a spoken label the bare
  // money values read as unlabeled numbers, so the label travels inside.
  renderSensitiveText("portfolioDailyTheta", "Theta per day " + riskMoney(
    portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
    portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy));
  $("portfolioGreeksCoverage").textContent = greeksCoverage(portfolio, positions);
  $("portfolioGreeksMeaning").textContent = greeksMeaning(portfolio, positions);
  renderSensitiveText("portfolioFxSensitivity", "FX 1% sensitivity " + riskMoney(
    portfolio.fx_sensitivity_per_pct,
    portfolio.fx_base_currency || baseCurrency,
  ), hasNumericValue(portfolio.fx_sensitivity_per_pct));
  $("portfolioDetailSummary").textContent = portfolioDetailSummary(portfolio, positions);
  renderPortfolioDetail(portfolio, positions, baseCurrency);

  const exposures = (portfolio.exposure_base || []).slice(0, 3);
  renderExposureVisual(exposureComposition(positions, account, portfolio, baseCurrency));
  const list = $("portfolioExposureList");
  list.hidden = exposures.length === 0;
  list.replaceChildren(...exposures.map((exposure) => {
    const row = document.createElement("div");
    row.className = "metric-row";
    const label = document.createElement("span");
    const pctText = typeof exposure.market_value_pct_nlv === "number" ? ` ${pct(exposure.market_value_pct_nlv)}` : "";
    label.textContent = exposure.underlying + pctText;
    const value = document.createElement("b");
    value.textContent = sensitiveDisplayMoney(exposure.market_value_base, exposure.base_currency || baseCurrency);
    value.className = "exposure-value" + (sensitiveMoneyHidden(exposure.market_value_base) ? " is-private" : "");
    row.append(label, value);
    const pnl = exposure.daily_pnl_base ?? exposure.unrealized_pnl_base;
    if (state.accountValueVisible && typeof pnl === "number") {
      const detail = document.createElement("small");
      detail.className = signedClass(pnl);
      detail.textContent = "P/L " + money(pnl, exposure.base_currency || baseCurrency);
      value.append(detail);
    }
    return row;
  }));
}

function renderPortfolioUnavailable(authority = {}) {
  const detail = authority.detail || "Current portfolio data is unavailable.";
  const stale = /stale|last good|retained/i.test(detail);
  const value = $("portfolioDollarDelta");
  value.textContent = stale ? "Portfolio stale" : "Portfolio unavailable";
  value.className = "portfolio-delta-posture neutral";
  $("portfolioDeltaMeaning").textContent = detail;
  renderSensitiveText("portfolioDailyTheta", "--", false);
  renderSensitiveText("portfolioFxSensitivity", "--", false);
  $("portfolioGreeksCoverage").textContent = "--";
  $("portfolioGreeksMeaning").textContent = "Current portfolio Greeks are unavailable.";
  $("portfolioDetailSummary").textContent = stale ? "Portfolio data stale" : "Portfolio data unavailable";

  const visual = $("portfolioExposureVisual");
  visual.hidden = true;
  visual.replaceChildren();
  const exposures = $("portfolioExposureList");
  exposures.hidden = true;
  exposures.replaceChildren();

  const panel = $("portfolioDetailPanel");
  const button = $("portfolioDetailToggle");
  const wrapper = $("portfolioPanel");
  wrapper.dataset.open = String(state.portfolioDetailOpen);
  panel.hidden = !state.portfolioDetailOpen;
  button.setAttribute("aria-expanded", String(state.portfolioDetailOpen));
  button.textContent = state.portfolioDetailOpen ? "Hide detail" : "Detail";
  const list = $("portfolioDetailList");
  if (!state.portfolioDetailOpen) {
    list.replaceChildren();
    return;
  }
  const row = document.createElement("div");
  row.className = "empty-row";
  row.textContent = detail;
  list.replaceChildren(row);
}

function renderPortfolioDeltaPosture(portfolio, account) {
  const posture = portfolioDeltaPosture(portfolio, account);
  const value = $("portfolioDollarDelta");
  if (!value) return;
  value.textContent = posture.label;
  value.className = "portfolio-delta-posture " + posture.tone;
  const meaning = $("portfolioDeltaMeaning");
  if (meaning) {
    meaning.textContent = posture.detail;
  }
}

function portfolioDeltaPosture(portfolio = {}, account = {}) {
  const delta = portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy;
  const nlv = portfolio.net_liquidation_base ?? accountFieldValue(account, "net_liquidation");
  if (typeof delta !== "number") {
    return {
      label: "Delta unavailable",
      detail: "Waiting for portfolio Greeks or stock exposure.",
      tone: "neutral",
    };
  }
  const ratio = typeof nlv === "number" && nlv > 0 ? Math.abs(delta) / nlv : null;
  const direction = delta > 0 ? "Long-biased" : delta < 0 ? "Short-biased" : "Flat";
  if (ratio === null) {
    return {
      label: direction,
      detail: "Market sensitivity is available in detail.",
      tone: "neutral",
    };
  }
  if (ratio >= 1) {
    return {
      label: "High delta risk",
      detail: `${direction}; detail has the private estimate.`,
      tone: "risk",
    };
  }
  if (ratio >= 0.35) {
    return {
      label: "Moderate delta",
      detail: `${direction}; watch broad-market moves.`,
      tone: "warn",
    };
  }
  return {
    label: "Low delta",
    detail: `${direction}; broad-market sensitivity is contained.`,
    tone: "ok",
  };
}

function exposureComposition(positions, account, portfolio, baseCurrency) {
  const netLiquidation = portfolio.net_liquidation_base ?? accountFieldValue(account, "net_liquidation");
  const stocks = sumAbsBase(positions.stocks || [], baseCurrency);
  const options = sumAbsBase(positions.options || [], baseCurrency);
  const accountCash = accountFieldValue(account, "total_cash");
  const cash = typeof accountCash === "number" ? Math.max(0, accountCash) : 0;
  if (typeof netLiquidation === "number" && netLiquidation > 0) {
    const raw = [
      { label: "Equity", pct: stocks / netLiquidation * 100 },
      { label: "Options", pct: options / netLiquidation * 100 },
      { label: "Cash", pct: cash / netLiquidation * 100 },
    ].filter((item) => item.pct > 0.1);
    const used = raw.reduce((sum, item) => sum + item.pct, 0);
    if (used < 99) raw.push({ label: "Other", pct: 100 - used, other: true });
    return normalizeComposition(raw);
  }
  return normalizeComposition((portfolio.exposure_base || []).slice(0, 3).map((exposure) => ({
    label: exposure.underlying || "--",
    pct: Math.abs(Number(exposure.market_value_pct_nlv || 0)),
  })));
}

function sumAbsBase(rows, baseCurrency) {
  return rows.reduce((sum, row) => {
    if (typeof row.market_value_base === "number") return sum + Math.abs(row.market_value_base);
    if (row.currency === baseCurrency && typeof row.market_value_ccy === "number") return sum + Math.abs(row.market_value_ccy);
    return sum;
  }, 0);
}

function normalizeComposition(items) {
  const filtered = items.filter((item) => item.pct > 0);
  const total = filtered.reduce((sum, item) => sum + item.pct, 0);
  if (total <= 0) return [];
  if (total <= 100) return filtered;
  return filtered.map((item) => ({ ...item, pct: item.pct / total * 100 }));
}

function renderExposureVisual(exposures) {
  const visual = $("portfolioExposureVisual");
  if (!visual) return;
  if (exposures.length === 0) {
    visual.hidden = true;
    visual.replaceChildren();
    return;
  }

  const normalized = exposures.filter((exposure) => exposure.pct > 0);
  if (normalized.length === 0) {
    visual.hidden = true;
    visual.replaceChildren();
    return;
  }

  const totalShown = normalized.reduce((sum, exposure) => sum + exposure.pct, 0);
  // A sub-half-percent residue rounds to a "0%" legend chip, which reads as
  // a data bug; the bar simply absorbs it instead.
  let remainder = Math.max(0, 100 - totalShown);
  if (remainder < 0.5) remainder = 0;
  const trackBase = totalShown + remainder || totalShown;

  const track = document.createElement("div");
  track.className = "exposure-visual__track";
  for (const exposure of normalized) {
    const segment = document.createElement("div");
    segment.className = "exposure-visual__segment" + (exposure.other ? " exposure-visual__segment--other" : "");
    segment.style.width = `${(exposure.pct / trackBase) * 100}%`;
    segment.title = `${exposure.label} ${pct(exposure.pct)}`;
    track.append(segment);
  }
  if (remainder > 0) {
    const other = document.createElement("div");
    other.className = "exposure-visual__segment exposure-visual__segment--other";
    other.style.width = `${(remainder / trackBase) * 100}%`;
    other.title = `Other ${pct(remainder)}`;
    track.append(other);
  }

  const legend = document.createElement("div");
  legend.className = "exposure-visual__legend";
  legend.replaceChildren(...normalized.map((exposure) => exposureLegendItem(exposure.label, exposure.pct)));
  if (remainder > 0) {
    const otherItem = exposureLegendItem("Other", remainder);
    otherItem.classList.add("exposure-visual__item--other");
    legend.append(otherItem);
  }

  visual.hidden = false;
  visual.replaceChildren(track, legend);
}

function exposureLegendItem(label, value) {
  const item = document.createElement("div");
  item.className = "exposure-visual__item";
  const swatch = document.createElement("span");
  swatch.className = "exposure-visual__swatch";
  const itemLabel = document.createElement("span");
  itemLabel.className = "exposure-visual__label";
  itemLabel.textContent = label;
  const itemValue = document.createElement("span");
  itemValue.className = "exposure-visual__value";
  itemValue.textContent = wholePct(value);
  item.append(swatch, itemLabel, itemValue);
  return item;
}

function renderPortfolioDetail(portfolio, positions, baseCurrency) {
  const panel = $("portfolioDetailPanel");
  const button = $("portfolioDetailToggle");
  const wrapper = $("portfolioPanel");
  wrapper.dataset.open = String(state.portfolioDetailOpen);
  panel.hidden = !state.portfolioDetailOpen;
  button.setAttribute("aria-expanded", String(state.portfolioDetailOpen));
  button.textContent = state.portfolioDetailOpen ? "Hide detail" : "Detail";
  if (!state.portfolioDetailOpen) return;
  $("portfolioDetailList").replaceChildren(...portfolioDetailRows(portfolio, positions, baseCurrency).map(detailFact));
}

function setPortfolioExpansion(open) {
  state.portfolioDetailOpen = Boolean(open);
  renderPortfolioRisk(state.snapshot?.positions || {}, state.snapshot?.account || {});
}

function portfolioDetailRows(portfolio, positions, baseCurrency) {
  const total = portfolio.greeks_total || 0;
  const covered = portfolio.greeks_coverage || 0;
  const greeksTitle = total > 0 ? `${covered}/${total} option legs covered` : "No option legs";
  const greeksBody = total === 0
    ? "There are no option legs that need model Greeks in this snapshot."
    : covered === total
      ? "Delta, theta, gamma, and vega aggregates are complete for the current option legs."
      : "Some option legs are missing model Greeks; treat portfolio Greeks as partial.";
  const rows = [
    {
      label: "Greeks",
      title: greeksTitle,
      body: greeksBody,
      tone: total > 0 && covered < total ? "warn" : "ok",
    },
    {
      label: "Market risk (delta)",
      title: sensitiveMoney(
        portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
        portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
      ),
      body: state.accountValueVisible
        ? "Approximate portfolio move for a one-point move in the underlyings, converted to account base when possible."
        : "Hidden while account privacy is on. Dollar delta estimates how fast the held book moves with the market.",
      tone: "neutral",
    },
    {
      label: "Theta/day",
      title: sensitiveMoney(
        portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
        portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
      ),
      body: state.accountValueVisible
        ? "Estimated option time decay per day. Negative values mean expected decay cost."
        : "Hidden while account privacy is on. Theta/day estimates option time decay across the book.",
      tone: signedTone(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy, true),
    },
    {
      label: "FX 1%",
      title: sensitiveMoney(portfolio.fx_sensitivity_per_pct, portfolio.fx_base_currency || baseCurrency),
      body: state.accountValueVisible
        ? "Estimated base-currency P/L from a 1% move in non-base contract currencies."
        : "Hidden while account privacy is on. FX 1% estimates currency sensitivity across non-base exposures.",
      tone: "neutral",
    },
  ];
  const coverageFact = protectionCoverageDetailFact(positions.protection_coverage, baseCurrency);
  if (coverageFact) rows.push(coverageFact);
  if ((portfolio.exposure_base || []).length > 0) {
    rows.push({
      label: "Largest exposure",
      title: portfolio.exposure_base[0].underlying || "--",
      body: "The exposure rows below show dominant underlyings by absolute base-currency market value.",
      tone: "neutral",
    });
  } else if ((positions.stocks || []).length === 0 && (positions.options || []).length === 0) {
    rows.push({
      label: "Positions",
      title: "No open positions",
      body: "The portfolio-risk panel has no position rows to aggregate.",
      tone: "neutral",
    });
  }
  return rows;
}

function portfolioDetailSummary(portfolio, positions) {
  if (portfolio.greeks_total > 0) {
    return (portfolio.greeks_coverage || 0) >= portfolio.greeks_total ? "Greeks ready" : "Partial Greeks";
  }
  if ((positions.options || []).length === 0) {
    return "No option Greeks needed";
  }
  return "details";
}

function detailFact(fact) {
  const row = document.createElement("div");
  row.className = "detail-fact " + (fact.tone || "neutral");
  const label = document.createElement("span");
  label.textContent = labelize(fact.label);
  const title = document.createElement("b");
  title.textContent = cleanDetail(fact.title || "--");
  const body = document.createElement("p");
  body.textContent = cleanDetail(fact.body || "");
  row.append(label, title, body);
  if (fact.detail instanceof Node) row.append(fact.detail);
  return row;
}

export { detailFact, exposureComposition, exposureLegendItem, normalizeComposition, portfolioDeltaPosture, portfolioDetailRows, portfolioDetailSummary, renderExposureVisual, renderPortfolioDeltaPosture, renderPortfolioDetail, renderPortfolioRisk, renderPortfolioUnavailable, setPortfolioExpansion, sumAbsBase };
