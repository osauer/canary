import { protectionEffectiveBlockers } from "./market-events.js";
import { cleanDetail, compactWholeMoney, firstNumber, hasNumericValue, labelize, normalizeCurrency, normalizeSymbol, numberRead, sensitiveDisplayMoney } from "./shared.js";
import { state } from "./state.js";

function protectionVisibleRows(rows = [], marketEvents = {}) {
  return rows.filter((proposal) => !protectionCoveredByExistingOrder(proposal, marketEvents));
}

function protectionNoStopExposureSummary(rows = [], marketEvents = {}, coverage = null) {
  if (protectionCoverageHasData(coverage)) {
    return protectionCoverageNoStopSummary(coverage);
  }
  const visible = protectionVisibleRows(rows, marketEvents)
    .filter((proposal) => proposal.bucket === "trailing_stop");
  if (visible.length === 0) {
    return {
      text: "--",
      title: "No visible trailing-stop proposals without a matching open protective order.",
      risk: false,
    };
  }
  const amounts = visible
    .map((proposal) => protectionProposalNotional(proposal))
    .filter(({ value }) => hasNumericValue(value) && value > 0);
  const currencies = [...new Set(amounts.map(({ currency }) => currency).filter(Boolean))];
  const countText = `${visible.length} ${visible.length === 1 ? "position" : "positions"}`;
  if (amounts.length === visible.length && currencies.length === 1) {
    const total = amounts.reduce((sum, row) => sum + row.value, 0);
    return {
      text: compactWholeMoney(total, currencies[0]),
      title: `${countText} without a matching open broker stop; sum of visible trailing-stop proposal notionals.`,
      risk: total > 0,
    };
  }
  return {
    text: countText,
    title: "Visible trailing-stop proposals without a matching open broker stop; dollar sum is unavailable or mixed-currency.",
    risk: true,
  };
}

function protectionProposalNotional(proposal = {}) {
  const value = firstNumber(proposal.notional);
  const currency = normalizeCurrency(proposal.contract?.currency || proposal.currency || "");
  return { value, currency };
}

function currentProtectionCoverage() {
  return protectionCoverageFromPositions(state.snapshot || {});
}

function protectionCoverageFromPositions(snap = state.snapshot || {}) {
  const coverage = snap.positions?.protection_coverage;
  return protectionCoverageHasData(coverage) ? coverage : null;
}

function canaryProtectionCoverageFor(snap = state.snapshot || {}, canary = snap.canary || {}) {
  const candidates = [
    protectionCoverageFromPositions(snap),
    canary.portfolio?.protection_coverage,
    canary.protection_coverage,
  ];
  return candidates.find(protectionCoverageHasData) || null;
}

function protectionCoverageHasData(coverage = null) {
  if (!coverage || typeof coverage !== "object") return false;
  if (coverage.status || coverage.message) return true;
  if (hasNumericValue(coverage.unprotected_notional_base)) return true;
  if ((coverage.by_underlying || []).length > 0) return true;
  if ((coverage.largest_unprotected || []).length > 0) return true;
  if ((coverage.orphaned_orders || []).length > 0) return true;
  if ((coverage.reconcile_required_orders || []).length > 0) return true;
  return Object.values(coverage.counts || {}).some((value) => Number(value || 0) > 0);
}


// protectionCoverageBaseCurrency resolves the currency labeling coverage
// base-notional amounts. Empty means genuinely unknown — callers render the
// bare number rather than coercing to USD.
function protectionCoverageBaseCurrency(coverage = {}, fallback = "") {
  return normalizeCurrency(
    coverage.unprotected_notional_base_currency ||
    fallback ||
    state.snapshot?.positions?.portfolio?.base_currency ||
    state.snapshot?.account?.base_currency ||
    "",
  );
}

function protectionCoverageCounts(coverage = {}) {
  const counts = coverage.counts || {};
  const orphaned = Number(counts.orphaned_order || (coverage.orphaned_orders || []).length || 0);
  const reconcile = Number(counts.reconcile_required || (coverage.reconcile_required_orders || []).length || 0);
  return {
    covered: Number(counts.covered || 0),
    partial: Number(counts.partial || 0),
    unprotected: Number(counts.unprotected || 0),
    orphaned,
    reconcile,
    unknown: Number(counts.unknown || 0),
    stale: orphaned + reconcile,
  };
}

function protectionCoverageNoStopSummary(coverage = {}) {
  const counts = protectionCoverageCounts(coverage);
  const baseCurrency = protectionCoverageBaseCurrency(coverage);
  const issueCount = counts.unprotected + counts.partial;
  const status = String(coverage.status || "").toLowerCase();
  const stale = protectionCoverageStaleText(coverage);
  if (status === "unknown" || counts.unknown > 0) {
    return {
      text: "Unknown",
      title: [coverage.message || "Protection coverage is unavailable for this snapshot.", stale].filter(Boolean).join(" "),
      risk: true,
    };
  }
  if (hasNumericValue(coverage.unprotected_notional_base)) {
    // A zero notional with uncovered rows means the rows could not be valued
    // (stale or delisted marks), not that nothing is exposed — "€0" there is
    // false comfort, so the row count leads instead.
    if (coverage.unprotected_notional_base <= 0 && issueCount > 0) {
      return {
        text: `${issueCount} ${issueCount === 1 ? "row" : "rows"}`,
        title: [
          `${issueCount} uncovered stock/ETF ${issueCount === 1 ? "row" : "rows"} whose notional is zero or unavailable (stale or delisted marks); a euro amount would be false comfort.`,
          stale,
        ].filter(Boolean).join(" "),
        risk: true,
      };
    }
    const text = compactWholeMoney(coverage.unprotected_notional_base, baseCurrency);
    const label = issueCount > 0
      ? `${issueCount} ${issueCount === 1 ? "stock/ETF row has" : "stock/ETF rows have"} uncovered quantity`
      : "No uncovered stock/ETF quantity in the coverage ledger";
    const currencyNote = baseCurrency ? "" : "Base currency is unavailable in this snapshot; the amount is shown without a currency label.";
    return {
      text,
      title: [label, currencyNote, stale].filter(Boolean).join(" "),
      risk: coverage.unprotected_notional_base > 0 || issueCount > 0 || counts.stale > 0,
    };
  }
  if (issueCount > 0) {
    return {
      text: `${issueCount} ${issueCount === 1 ? "row" : "rows"}`,
      title: [`Coverage ledger found uncovered stock/ETF quantity; base notional is unavailable.`, stale].filter(Boolean).join(" "),
      risk: true,
    };
  }
  return {
    text: counts.stale > 0 ? "Review" : "--",
    title: [counts.stale > 0 ? "Stale protective orders need reconciliation." : "No uncovered stock/ETF quantity in the coverage ledger.", stale].filter(Boolean).join(" "),
    risk: counts.stale > 0,
  };
}

// A defunct holding is excluded from no-stop exposure because nothing can stop
// it out. Silently dropping it would be the mirror of the "€0" false comfort
// the exposure metric already guards against, so it stays as visible text.
function protectionNotProtectableText(coverage) {
  const rows = (coverage?.by_underlying || []).filter((row) => row.state === "not_protectable");
  if (rows.length === 0) return "";
  const names = rows.map((row) => row.underlying).filter(Boolean).slice(0, 3).join(", ");
  const subject = rows.length === 1 ? "defunct holding has" : "defunct holdings have";
  const named = names ? ` (${names})` : "";
  return `${rows.length} ${subject} no mark to protect${named} — excluded from no-stop exposure; reconcile with broker records.`;
}

function protectionCoverageStaleText(coverage = {}) {
  const counts = protectionCoverageCounts(coverage);
  const parts = [];
  if (counts.orphaned > 0) parts.push(`${counts.orphaned} orphaned`);
  if (counts.reconcile > 0) parts.push(`${counts.reconcile} reconcile-required`);
  return parts.length > 0 ? `Stale stops: ${parts.join(", ")}.` : "";
}

function protectionCoverageLargestRows(coverage = {}) {
  const largest = Array.isArray(coverage.largest_unprotected) ? coverage.largest_unprotected : [];
  if (largest.length > 0) return largest;
  return (coverage.by_underlying || [])
    .filter((row) => ["partial", "unprotected"].includes(String(row.state || "").toLowerCase()))
    .slice()
    .sort((a, b) => Math.abs(firstNumber(b.unprotected_notional_base, 0)) - Math.abs(firstNumber(a.unprotected_notional_base, 0)));
}

function protectionCoverageLargestText(coverage = {}, baseCurrency = "", { sensitive = false } = {}) {
  const rows = protectionCoverageLargestRows(coverage).slice(0, 3);
  if (rows.length === 0) return "";
  return rows.map((row) => {
    const symbol = normalizeSymbol(row.underlying || row.symbol || "");
    const value = firstNumber(row.unprotected_notional_base, row.market_value_base);
    const ccy = row.unprotected_notional_base_currency || baseCurrency || protectionCoverageBaseCurrency(coverage);
    const amount = hasNumericValue(value)
      ? sensitive
        ? sensitiveDisplayMoney(value, ccy)
        : compactWholeMoney(value, ccy)
      : "";
    return [symbol || "Unknown", amount].filter(Boolean).join(" ");
  }).join(", ");
}

function protectionCoverageStaleOrderText(coverage = {}, limit = 2) {
  const orders = [
    ...(coverage.orphaned_orders || []),
    ...(coverage.reconcile_required_orders || []),
  ].slice(0, limit);
  if (orders.length === 0) return "";
  return orders.map((order) => {
    const symbol = normalizeSymbol(order.symbol || "");
    const qty = hasNumericValue(order.remaining) && order.remaining > 0 ? `${numberRead(order.remaining)} ` : "";
    const type = String(order.order_type || "").toUpperCase();
    const state = String(order.reconciliation_state || "").replaceAll("_", " ");
    return [symbol, `${qty}${type}`.trim(), state].filter(Boolean).join(" ");
  }).join(", ");
}

function protectionCoverageDisplayRows(coverage = {}) {
  const rows = [];
  const seen = new Set();
  for (const row of coverage.by_underlying || []) {
    const key = normalizeSymbol(row.underlying || row.symbol || "");
    if (key) seen.add(key);
    rows.push(row);
  }
  for (const order of [...(coverage.orphaned_orders || []), ...(coverage.reconcile_required_orders || [])]) {
    const symbol = normalizeSymbol(order.symbol || order.underlying || "");
    if (!symbol || seen.has(symbol)) continue;
    seen.add(symbol);
    rows.push({
      underlying: symbol,
      state: order.reconciliation_state ? "reconcile_required" : "orphaned_order",
      orders: [order],
      warning_codes: [order.reconciliation_state || "orphaned_order"],
      message: order.last_message || "Protective order needs broker reconciliation.",
    });
  }
  return rows.slice().sort((a, b) => protectionCoverageRowPriority(a) - protectionCoverageRowPriority(b) ||
    normalizeSymbol(a.underlying || a.symbol || "").localeCompare(normalizeSymbol(b.underlying || b.symbol || "")));
}

function protectionCoverageRowPriority(row = {}) {
  const state = String(row.state || "").toLowerCase();
  if (["orphaned_order", "reconcile_required"].includes(state)) return 0;
  if (state === "partial") return 1;
  if (state === "unprotected") return 2;
  if (state === "unknown") return 3;
  if (state === "covered") return 4;
  return 5;
}

function protectionCoverageRowState(row = {}) {
  const state = String(row.state || "").toLowerCase();
  if (state === "orphaned_order") return "orphaned order";
  if (state === "reconcile_required") return "reconcile required";
  return labelize(state || "unknown");
}

function protectionCoverageRowClass(row = {}) {
  const state = String(row.state || "").toLowerCase();
  if (["orphaned_order", "reconcile_required", "partial", "unprotected"].includes(state)) return state;
  if (state === "covered") return "covered";
  return "unknown";
}

function protectionCoverageQuantityText(row = {}) {
  const protectedQty = firstNumber(row.protected_quantity);
  const unprotectedQty = firstNumber(row.unprotected_quantity);
  const positionQty = firstNumber(row.position_quantity);
  const state = String(row.state || "").toLowerCase();
  if (["orphaned_order", "reconcile_required"].includes(state)) {
    const order = (row.orders || [])[0] || {};
    const remaining = firstNumber(order.remaining, order.quantity);
    return hasNumericValue(remaining) ? `${numberRead(Math.abs(remaining))} remaining` : "open order";
  }
  if (state === "covered" && hasNumericValue(protectedQty)) return `${numberRead(Math.abs(protectedQty))} covered`;
  if (state === "partial") {
    const parts = [];
    if (hasNumericValue(protectedQty)) parts.push(`${numberRead(Math.abs(protectedQty))} covered`);
    if (hasNumericValue(unprotectedQty)) parts.push(`${numberRead(Math.abs(unprotectedQty))} uncovered`);
    if (parts.length > 0) return parts.join(" / ");
  }
  if (state === "unprotected" && hasNumericValue(unprotectedQty)) return `${numberRead(Math.abs(unprotectedQty))} uncovered`;
  if (hasNumericValue(positionQty)) return `${numberRead(Math.abs(positionQty))} position`;
  return "";
}

function protectionCoverageNotionalText(row = {}, baseCurrency = "", { sensitive = true } = {}) {
  const value = firstNumber(row.unprotected_notional_base, row.market_value_base);
  if (!hasNumericValue(value)) return "";
  const ccy = row.unprotected_notional_base_currency || row.base_currency || baseCurrency || protectionCoverageBaseCurrency();
  const amount = sensitive ? sensitiveDisplayMoney(Math.abs(value), ccy) : compactWholeMoney(Math.abs(value), ccy);
  const state = String(row.state || "").toLowerCase();
  if (state === "covered") return "";
  return `${amount} unprotected notional`;
}

function protectionCoverageOrderText(row = {}) {
  const orders = row.orders || [];
  if (orders.length === 0) return "";
  return orders.slice(0, 2).map((order) => {
    const type = String(order.order_type || "").toUpperCase();
    const tif = String(order.tif || "").toUpperCase();
    const stop = firstNumber(order.stop_price, order.trail?.initial_stop_price);
    const limit = firstNumber(order.limit_price);
    const parts = [type, tif].filter(Boolean);
    if (hasNumericValue(stop)) parts.push(`stop ${numberRead(stop)}`);
    if (hasNumericValue(limit) && type.includes("LIMIT")) parts.push(`limit ${numberRead(limit)}`);
    return parts.join(" ");
  }).filter(Boolean).join(", ");
}

function protectionCoverageLedger(coverage = {}, baseCurrency = "") {
  const rows = protectionCoverageDisplayRows(coverage);
  if (rows.length === 0) return null;
  const visible = rows.slice(0, 6);
  const ledger = document.createElement("div");
  ledger.className = "protection-coverage-ledger";
  ledger.setAttribute("aria-label", "Per-underlying protection coverage");
  for (const row of visible) {
    const item = document.createElement("div");
    item.className = `protection-coverage-ledger__row protection-coverage-ledger__row--${protectionCoverageRowClass(row)}`;
    const head = document.createElement("div");
    head.className = "protection-coverage-ledger__head";
    const symbol = document.createElement("span");
    symbol.className = "protection-coverage-ledger__symbol";
    symbol.textContent = normalizeSymbol(row.underlying || row.symbol || "") || "Unknown";
    const state = document.createElement("span");
    state.className = "protection-coverage-ledger__state";
    state.textContent = protectionCoverageRowState(row);
    head.append(symbol, state);
    const meta = document.createElement("div");
    meta.className = "protection-coverage-ledger__meta";
    const message = row.message ? cleanDetail(row.message) : "";
    const parts = [
      protectionCoverageQuantityText(row),
      protectionCoverageNotionalText(row, baseCurrency, { sensitive: true }),
      protectionCoverageOrderText(row),
      message,
    ].filter(Boolean);
    meta.textContent = parts.join(" · ");
    item.append(head, meta);
    ledger.append(item);
  }
  if (rows.length > visible.length) {
    const more = document.createElement("div");
    more.className = "protection-coverage-ledger__more";
    more.textContent = `${rows.length - visible.length} more coverage ${rows.length - visible.length === 1 ? "row" : "rows"}`;
    ledger.append(more);
  }
  return ledger;
}

function protectionCoverageTone(coverage = {}) {
  const counts = protectionCoverageCounts(coverage);
  const status = String(coverage.status || "").toLowerCase();
  if (status === "unknown" || counts.unknown > 0) return "warn";
  if (counts.unprotected > 0 || counts.partial > 0 || counts.stale > 0 || Number(coverage.unprotected_notional_base || 0) > 0) {
    return "risk";
  }
  return "ok";
}

function protectionHiddenRowsText(rows = [], marketEvents = {}) {
  const covered = rows.filter((proposal) => protectionCoveredByExistingOrder(proposal, marketEvents)).length;
  if (covered === 0) return "";
  const parts = [];
  if (covered > 0) parts.push(`${covered} already-covered ${covered === 1 ? "proposal" : "proposals"} hidden`);
  return parts.join(" · ");
}

function protectionCoveredByExistingOrder(proposal = {}, marketEvents = {}) {
  if (proposal.bucket !== "trailing_stop") return false;
  const blockers = protectionEffectiveBlockers(proposal, marketEvents);
  return blockers.length > 0 && blockers.every(protectionBlockerIsExistingOrder);
}

function protectionBlockerIsExistingOrder(blocker = {}) {
  return String(blocker.code || "") === "existing_protective_order";
}

function protectionEmptyRow(message) {
  const empty = document.createElement("div");
  empty.className = "empty-row";
  empty.textContent = message;
  return empty;
}

function applyProtectionSnapshot(proposals = {}) {
  state.snapshot = {
    ...(state.snapshot || {}),
    proposals,
    auto_trade: proposals.auto_trade || state.snapshot?.auto_trade,
    trading: proposals.trading || state.snapshot?.trading,
    market_events: proposals.market_events || state.snapshot?.market_events,
  };
}

function protectionCoverageDetailFact(coverage = null, baseCurrency = "") {
  if (!protectionCoverageHasData(coverage)) return null;
  const ccy = protectionCoverageBaseCurrency(coverage, baseCurrency);
  return {
    label: "Protection coverage",
    title: protectionCoverageHeadline(coverage, ccy, { sensitive: true }),
    body: protectionCoverageDetailBody(coverage, ccy),
    tone: protectionCoverageTone(coverage),
    detail: protectionCoverageLedger(coverage, ccy),
  };
}

function protectionCoverageHeadline(coverage = {}, baseCurrency = "", { sensitive = false } = {}) {
  const counts = protectionCoverageCounts(coverage);
  const status = String(coverage.status || "").toLowerCase();
  const ccy = protectionCoverageBaseCurrency(coverage, baseCurrency);
  if (status === "unknown" || counts.unknown > 0) return "Coverage unknown";
  if (hasNumericValue(coverage.unprotected_notional_base)) {
    const amount = sensitive
      ? sensitiveDisplayMoney(coverage.unprotected_notional_base, ccy)
      : compactWholeMoney(coverage.unprotected_notional_base, ccy);
    const stale = counts.stale > 0 ? ` · ${counts.stale} stale` : "";
    return `${amount} unprotected${stale}`;
  }
  const uncovered = counts.unprotected + counts.partial;
  if (uncovered > 0) return `${uncovered} ${uncovered === 1 ? "position" : "positions"} unprotected`;
  if (counts.stale > 0) return `${counts.stale} stale ${counts.stale === 1 ? "stop" : "stops"}`;
  if (counts.covered > 0) return `${counts.covered} ${counts.covered === 1 ? "position" : "positions"} covered`;
  return "Coverage ready";
}

function protectionCoverageDetailBody(coverage = {}, baseCurrency = "") {
  const counts = protectionCoverageCounts(coverage);
  const largest = protectionCoverageLargestText(coverage, baseCurrency, { sensitive: true });
  const staleOrders = protectionCoverageStaleOrderText(coverage, 3);
  const parts = [];
  if (largest) {
    parts.push(`Largest unprotected: ${largest}.`);
  } else if (counts.unprotected === 0 && counts.partial === 0) {
    parts.push("No unprotected stock/ETF notional in this snapshot.");
  }
  if (staleOrders) parts.push(`Stale protective orders: ${staleOrders}.`);
  if (coverage.message) parts.push(cleanDetail(coverage.message));
  if (parts.length === 0) {
    parts.push("Coverage ledger compares stock/ETF positions to observed open stop orders.");
  }
  return parts.join(" ");
}

export { applyProtectionSnapshot, canaryProtectionCoverageFor, currentProtectionCoverage, protectionBlockerIsExistingOrder, protectionCoverageBaseCurrency, protectionCoverageCounts, protectionCoverageDetailBody, protectionCoverageDetailFact, protectionCoverageDisplayRows, protectionCoverageFromPositions, protectionCoverageHasData, protectionCoverageHeadline, protectionCoverageLargestRows, protectionCoverageLargestText, protectionCoverageLedger, protectionCoverageNoStopSummary, protectionCoverageNotionalText, protectionCoverageOrderText, protectionCoverageQuantityText, protectionCoverageRowClass, protectionCoverageRowPriority, protectionCoverageRowState, protectionCoverageStaleOrderText, protectionCoverageStaleText, protectionCoverageTone, protectionCoveredByExistingOrder, protectionEmptyRow, protectionHiddenRowsText, protectionNoStopExposureSummary, protectionNotProtectableText, protectionProposalNotional, protectionVisibleRows };
