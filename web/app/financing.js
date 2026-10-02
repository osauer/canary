import { $, calendarDate, privacyMask, readJSONOrText, signedMoneyRead } from "./shared.js";
import { state } from "./state.js";

const currency = (value) => typeof value === "string" && /^[A-Z]{3}$/.test(value);
const number = (value) => typeof value === "number" && Number.isFinite(value);
const optionalNumber = (value) => value == null || number(value);
const count = (value) => Number.isSafeInteger(value) && value >= 0;
const day = (value) => {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T00:00:00Z$/.test(value)) return null;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) && new Date(parsed).toISOString().slice(0, 10) === value.slice(0, 10) ? parsed : null;
};
const rowCaches = new WeakMap();

function financingAccountScope() {
  const position = state.snapshot?.positions?.authority?.scope;
  const account = state.snapshot?.account?.authority?.scope;
  if (position?.account_id && account?.account_id && (position.account_id !== account.account_id || position.account_mode !== account.account_mode)) return "conflicting";
  const scope = position?.account_id ? position : account;
  return scope?.account_id && scope?.account_mode ? `${scope.account_id}|${scope.account_mode}` : "";
}

function validLendingPosition(value, stock) {
  return Boolean(value && stock && ["STOCK", "STK"].includes(stock.sec_type)
    && Number.isSafeInteger(stock.con_id) && stock.con_id > 0 && day(value.as_of) != null
    && ["reported", "stale"].includes(value.state) && currency(value.currency) && value.currency === stock.currency
    && number(value.quantity) && value.quantity > 0 && number(value.owned_quantity)
    && value.owned_quantity >= value.quantity && value.owned_quantity === stock.quantity
    && optionalNumber(value.net_rate_pct) && optionalNumber(value.collateral) && (value.collateral == null || value.collateral >= 0));
}

function validFinancingSummary(value) {
  if (!value || value.schema_version !== "financing.v1" || !["complete", "partial", "unavailable"].includes(value.state)
    || day(value.from) == null || day(value.to) == null || day(value.to) <= day(value.from)
    || day(value.to) - day(value.from) > 400 * 86400000 || !/^finance_[a-f0-9]{32}$/.test(value.fingerprint || "")
    || ![value.fee_count, value.covered_days, value.expected_days].every(count) || value.expected_days < 1
    || value.expected_days !== (day(value.to) - day(value.from)) / 86400000 || value.covered_days > value.expected_days
    || value.pnl_reconciliation !== "unproved" || value.payment_linkage !== "unavailable"
    || !optionalNumber(value.earned_base) || !optionalNumber(value.known_earned_base)
    || (value.base_currency != null && value.base_currency !== "" && !currency(value.base_currency))
    || !Array.isArray(value.native)) return false;
  if (value.earned_base != null && (value.state !== "complete" || !currency(value.base_currency) || value.covered_days !== value.expected_days)) return false;
  if (value.known_earned_base != null && !currency(value.base_currency)) return false;
  if (value.state === "unavailable" && (value.earned_base != null || value.known_earned_base != null)) return false;
  return new Set(value.native.map((row) => row?.currency)).size === value.native.length
    && value.native.every((row) => currency(row?.currency) && number(row?.amount));
}

function validFinancingFees(value) {
  if (!value || !validFinancingSummary(value.summary) || !Array.isArray(value.fees) || value.fees.length > 100
    || !count(value.filtered_count) || value.filtered_count < value.fees.length || value.filtered_count > value.summary.fee_count
    || !count(value.con_id || 0) || typeof (value.next_cursor || "") !== "string" || (value.next_cursor || "").length > 256) return false;
  const ids = new Set();
  return value.fees.every((row, i) => {
    if (!row || !/^fee_[a-f0-9]{32}$/.test(row.id || "") || ids.has(row.id) || !count(row.con_id) || row.con_id === 0
      || (value.con_id && row.con_id !== value.con_id) || typeof row.symbol !== "string" || !row.symbol.trim() || row.symbol.length > 80
      || !currency(row.currency) || day(row.value_date) == null || day(row.value_date) <= day(value.summary.from)
      || day(row.value_date) > day(value.summary.to) || (i > 0 && day(row.value_date) > day(value.fees[i - 1].value_date))
      || (row.start_date != null && (day(row.start_date) == null || day(row.start_date) > day(row.value_date)))
      || ![row.quantity, row.net_fee, row.net_rate_pct, row.collateral, row.fx_rate_to_base, row.base_amount].every(optionalNumber)
      || (row.quantity != null && row.quantity < 0) || (row.collateral != null && row.collateral < 0)
      || (row.fx_rate_to_base != null && row.fx_rate_to_base <= 0)
      || (row.base_amount != null && (row.net_fee == null || !currency(value.summary.base_currency) || row.fx_rate_to_base == null
        || Math.abs(row.base_amount - row.net_fee * row.fx_rate_to_base) > 1e-9 * Math.max(1, Math.abs(row.base_amount))))) return false;
    ids.add(row.id);
    return true;
  });
}

function feeMoney(value, unit) {
  if (!number(value)) return "Unavailable";
  return state.accountValueVisible ? signedMoneyRead(value, unit) : privacyMask();
}

function renderFinancing(summary) {
  const target = $("lendingIncome");
  if (!target) return;
  const valid = validFinancingSummary(summary);
  const fingerprint = valid ? summary.fingerprint : "";
  if (state.financing.fingerprint !== fingerprint) {
    const hadFocus = $("lendingFees")?.contains(document.activeElement);
    state.financing = { fingerprint, scope: financingAccountScope(), conID: 0, result: null, busy: false, error: "", requestID: state.financing.requestID + 1 };
    if (hadFocus && valid) $("lendingFeesRefresh")?.focus?.();
  }
  const available = state.authenticated && valid && (!state.financing.scope || state.financing.scope === financingAccountScope()) && financingAccountScope() !== "conflicting";
  target.hidden = !available;
  if (!available) {
    if (state.financing.result || state.financing.busy) state.financing.requestID += 1;
    state.financing.result = null; state.financing.busy = false;
    renderFeeRows([], summary); return;
  }
  $("lendingIncomeLabel").textContent = summary.state === "partial" ? "Known lending fees · partial period" : "Lending income earned";
  const value = summary.earned_base ?? summary.known_earned_base;
  $("lendingIncomeValue").textContent = number(value) ? feeMoney(value, summary.base_currency)
    : summary.native.length === 1 ? feeMoney(summary.native[0].amount, summary.native[0].currency)
      : summary.native.length > 1 ? "See native currencies" : "Unavailable";
  $("lendingIncomeValue").classList.toggle("is-private", !state.accountValueVisible && (number(value) || summary.native.length > 0));
  const period = `${calendarDate(summary.from)} → ${calendarDate(summary.to)} (opening date excluded)`;
  const evidence = summary.state === "unavailable"
    ? "Optional IBKR lending sections are missing. Add SYEP balances and fee details to the existing Flex query."
    : `${summary.covered_days}/${summary.expected_days} days covered · ${summary.fee_count} fee records${summary.reason === "net_fee_missing" ? " · some net fee amounts unavailable" : ""}`;
  const native = summary.native.length > 0 && value == null
    ? ` Native fees: ${summary.native.map((row) => feeMoney(row.amount, row.currency)).join("; ")}. Base conversion unavailable.` : "";
  $("lendingIncomeCoverage").textContent = `${period}. ${evidence}${native}`;
  renderFeeRows(state.financing.result?.fees || [], summary);
  renderFeesStatus();
}

function renderFeesStatus() {
  const ui = state.financing;
  $("lendingFeesTitle").textContent = ui.conID ? `Earned fee history · ${ui.result?.fees[0]?.symbol || "this position"}` : "Earned fee history";
  $("lendingFeesAll").hidden = !ui.conID;
  $("lendingFeesRefresh").disabled = ui.busy;
  $("lendingFeesMore").disabled = ui.busy;
  $("lendingFeesMore").hidden = !ui.result?.next_cursor;
  $("lendingFeesStatus").textContent = ui.busy ? "Reading reported fees…" : ui.error || (ui.result
    ? ui.result.filtered_count === 0 ? "No earned fee records in this reported period."
      : `${ui.result.fees.length} of ${ui.result.filtered_count} reported fees.` : "Expand to read reported fees.");
}

function renderFeeRows(rows, summary) {
  const container = $("lendingFees");
  if (!container) return;
  let cache = rowCaches.get(container);
  if (!cache) { cache = { nodes: new Map(), ids: "" }; rowCaches.set(container, cache); }
  const ids = rows.map((row) => row.id).join("|");
  const nodes = rows.map((row) => {
    let item = cache.nodes.get(row.id);
    if (!item) {
      const root = document.createElement("details");
      root.className = "lending-fee";
      const heading = document.createElement("summary");
      const identity = document.createElement("span");
      const amount = document.createElement("strong");
      const facts = document.createElement("p");
      heading.append(identity, amount); root.append(heading, facts);
      item = { root, identity, amount, facts }; cache.nodes.set(row.id, item);
    }
    item.identity.textContent = `${calendarDate(row.value_date)} · ${row.symbol}${row.net_fee < 0 ? " · correction" : ""}`;
    item.amount.textContent = feeMoney(row.net_fee, row.currency);
    item.amount.classList.toggle("is-private", !state.accountValueVisible && number(row.net_fee));
    item.facts.textContent = [
      number(row.quantity) ? `${row.quantity} shares on loan for this fee` : "Loan quantity unavailable",
      number(row.net_rate_pct) ? `customer net rate ${row.net_rate_pct.toFixed(2)}%` : "Customer net rate unavailable",
      row.start_date ? `loan started ${calendarDate(row.start_date)}` : "",
      number(row.collateral) ? `collateral ${feeMoney(row.collateral, row.currency)} (asset with repayment obligation)` : "Collateral unavailable",
      number(row.base_amount) ? `earned ${feeMoney(row.base_amount, summary.base_currency)} at statement FX ${row.fx_rate_to_base}` : "Base conversion unavailable",
      "Payment status unproved",
    ].filter(Boolean).join(" · ");
    return item.root;
  });
  // Keep native disclosures and keyboard focus stable on snapshot/privacy
  // refreshes. Appending a page also reuses every already visible row.
  if (ids !== cache.ids) {
    const oldIDs = cache.ids ? cache.ids.split("|") : [];
    const focus = container.contains(document.activeElement) ? document.activeElement : null;
    if (oldIDs.length <= rows.length && oldIDs.every((id, i) => id === rows[i].id)) container.append(...nodes.slice(oldIDs.length));
    else container.replaceChildren(...nodes);
    if (focus && container.contains(focus)) focus.focus?.({ preventScroll: true });
    cache.ids = ids;
    const retained = new Set(rows.map((row) => row.id));
    for (const id of cache.nodes.keys()) if (!retained.has(id)) cache.nodes.delete(id);
  }
}

async function refreshFinancingFees(append = false) {
  const summary = state.edgeResult?.account?.financing;
  if (!state.authenticated || !validFinancingSummary(summary)) return false;
  renderFinancing(summary);
  const ui = state.financing;
  if ($("lendingIncome").hidden) return false;
  if (ui.busy || (append && !ui.result?.next_cursor)) return false;
  const requestID = ++ui.requestID;
  ui.busy = true; ui.error = ""; renderFeesStatus();
  const params = new URLSearchParams({ from: summary.from.slice(0, 10), to: summary.to.slice(0, 10), fingerprint: summary.fingerprint, limit: "25" });
  if (ui.conID) params.set("con_id", String(ui.conID));
  if (append) params.set("cursor", ui.result.next_cursor);
  try {
    const response = await fetch(`/api/financing/fees?${params}`, { credentials: "include" });
    const body = await readJSONOrText(response);
    if (!response.ok || !validFinancingFees(body) || body.summary.fingerprint !== summary.fingerprint || (body.con_id || 0) !== ui.conID) throw new Error("invalid fee read");
    if (state.financing !== ui || ui.requestID !== requestID || !state.authenticated || (ui.scope && ui.scope !== financingAccountScope())) return false;
    const rows = append ? [...ui.result.fees, ...body.fees] : body.fees;
    if (new Set(rows.map((row) => row.id)).size !== rows.length) throw new Error("duplicate fee page");
    ui.result = { ...body, fees: rows };
    renderFeeRows(rows, summary);
    return true;
  } catch {
    if (state.financing !== ui || ui.requestID !== requestID) return false;
    ui.error = "Fees could not be refreshed. Refresh Edge and retry; retained rows are from the previous read.";
    return false;
  } finally {
    if (state.financing === ui && ui.requestID === requestID) { ui.busy = false; renderFeesStatus(); }
  }
}

async function showLendingFees(conID = 0) {
  if (!Number.isSafeInteger(conID) || conID < 0) return false;
  renderFinancing(state.edgeResult?.account?.financing);
  if (!state.financing.fingerprint) return false;
  const ui = state.financing;
  if (ui.conID !== conID) {
    ui.requestID += 1; ui.conID = conID; ui.result = null; ui.busy = false; ui.error = "";
  }
  $("lendingIncome").open = true;
  renderFinancing(state.edgeResult.account.financing);
  const loaded = ui.result != null || await refreshFinancingFees();
  $("lendingFeesTitle")?.scrollIntoView?.({ block: "nearest" });
  $("lendingFeesRefresh")?.focus?.({ preventScroll: true });
  return loaded;
}

function setupFinancing() {
  $("lendingIncome")?.addEventListener("toggle", () => {
    if ($("lendingIncome").open && !state.financing.result) void refreshFinancingFees();
  });
  $("lendingFeesRefresh")?.addEventListener("click", () => { void refreshFinancingFees(); });
  $("lendingFeesMore")?.addEventListener("click", () => { void refreshFinancingFees(true); });
  $("lendingFeesAll")?.addEventListener("click", () => { void showLendingFees(); });
}

export { refreshFinancingFees, renderFinancing, setupFinancing, showLendingFees, validFinancingFees, validFinancingSummary, validLendingPosition };
