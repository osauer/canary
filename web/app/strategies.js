import { refreshOpenOrders } from "./orders.js";
import { $, labelize, money, protectionWriteConfirmation, readJSONOrText, renderFreshnessTimestamp } from "./shared.js";
import { state } from "./state.js";

function strategyKey(strategy = {}) {
  return `${strategy.id || ""}@${strategy.revision || ""}`;
}

function renderStrategies(positions = {}) {
  const list = $("strategiesList");
  if (!list) return;
  const strategies = positions.strategies || [];
  const issues = positions.strategy_issues || [];
  const current = strategies.filter((strategy) => strategy.status !== "closed");
  const count = $("strategiesCount");
  count.textContent = current.length === 0 ? "No groups" : current.length === 1 ? "1 group" : `${current.length} groups`;
  count.classList.toggle("is-zero", current.length === 0);
  renderFreshnessTimestamp("strategiesAsOf", positions.as_of, { staleMinutes: 15, fallback: "--" });
  const rows = [
    ...current.map(strategyRow),
    ...issues.map(strategyIssueRow),
  ];
  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No option strategies can be grouped safely from the current positions.";
    rows.push(empty);
  }
  list.replaceChildren(...rows);
}

function strategyRow(strategy) {
  const key = strategyKey(strategy);
  const draft = strategyDraft(strategy);
  const preview = state.strategyPreviews[key] || null;
  const submitted = state.strategySubmits[key] || null;
  const previewBusy = state.strategyPreviewBusy === key;
  const submitBusy = state.strategySubmitBusy === key;
  const row = document.createElement("section");
  row.className = `strategy-row pd-tile pd-order${strategy.actionable ? " pd-tile--watch" : ""}`;
  row.dataset.strategyId = strategy.id || "";
  if (strategy.actionable) {
    const bar = document.createElement("span");
    bar.className = "pd-tile__bar";
    bar.setAttribute("aria-hidden", "true");
    row.append(bar);
  }

  const head = document.createElement("div");
  head.className = "strategy-row__head";
  const identity = document.createElement("span");
  identity.className = "pd-tile__legend";
  identity.textContent = [strategy.underlying || "--", strategyKind(strategy.kind)].filter(Boolean).join(" · ");
  const units = document.createElement("b");
  units.className = "strategy-row__units";
  units.textContent = `${strategy.units || 0} ${strategy.units === 1 ? "unit" : "units"}`;
  const source = document.createElement("small");
  source.className = "strategy-row__source";
  source.textContent = strategySource(strategy.source);
  head.append(identity, units, source);

  const legs = document.createElement("div");
  legs.className = "strategy-row__legs";
  legs.replaceChildren(...(strategy.legs || []).map(strategyLeg));
  row.append(head, legs);

  if (!strategy.actionable) {
    const reason = document.createElement("p");
    reason.className = "strategy-row__reason";
    reason.textContent = strategy.reason || "Review the grouped legs before taking another action.";
    row.append(reason);
    return row;
  }

  const controls = strategyControls(strategy, draft, { previewBusy, submitBusy });
  row.append(controls);
  if (preview || submitted || previewBusy || submitBusy) {
    row.append(strategyPreviewCard(strategy, preview, submitted, { previewBusy, submitBusy }));
  }
  return row;
}

function strategyDraft(strategy) {
  const key = strategyKey(strategy);
  let draft = state.strategyDrafts[key];
  if (!draft) {
    draft = { operation: "close", units: 1, priceMode: "midpoint", amount: "" };
    state.strategyDrafts[key] = draft;
  }
  if ((strategy.units || 0) <= 1 && draft.operation === "reduce") draft.operation = "close";
  return draft;
}

function strategyControls(strategy, draft, busy) {
  const controls = document.createElement("div");
  controls.className = "strategy-row__controls";

  const operation = document.createElement("select");
  operation.setAttribute("aria-label", `Action for ${strategy.underlying || "strategy"}`);
  operation.append(strategySelectOption("Close all units", "close"));
  if ((strategy.units || 0) > 1) operation.append(strategySelectOption("Reduce position", "reduce"));
  operation.value = draft.operation;
  operation.disabled = busy.previewBusy || busy.submitBusy;
  operation.addEventListener("change", () => {
    draft.operation = operation.value;
    clearStrategyOutcome(strategy);
    renderStrategies(state.snapshot?.positions || {});
  });

  const units = document.createElement("input");
  units.type = "number";
  units.min = "1";
  units.max = String(Math.max(1, (strategy.units || 1) - 1));
  units.step = "1";
  units.value = String(Math.min(Number(draft.units) || 1, Number(units.max)));
  units.setAttribute("aria-label", `Units to reduce for ${strategy.underlying || "strategy"}`);
  units.hidden = draft.operation !== "reduce";
  units.disabled = busy.previewBusy || busy.submitBusy;
  units.addEventListener("change", () => {
    draft.units = Math.min(Number(units.max), Math.max(1, Math.trunc(Number(units.value) || 1)));
    clearStrategyOutcome(strategy);
    renderStrategies(state.snapshot?.positions || {});
  });

  const priceMode = document.createElement("select");
  priceMode.setAttribute("aria-label", `Limit price type for ${strategy.underlying || "strategy"}`);
  priceMode.append(
    strategySelectOption("Current midpoint", "midpoint"),
    strategySelectOption("Receive at least", "credit"),
    strategySelectOption("Pay up to", "debit"),
  );
  priceMode.value = draft.priceMode;
  priceMode.disabled = busy.previewBusy || busy.submitBusy;
  priceMode.addEventListener("change", () => {
    draft.priceMode = priceMode.value;
    clearStrategyOutcome(strategy);
    renderStrategies(state.snapshot?.positions || {});
  });

  const amount = document.createElement("input");
  amount.type = "number";
  amount.min = "0";
  amount.step = "0.01";
  amount.inputMode = "decimal";
  amount.placeholder = "0.00";
  amount.value = draft.amount;
  amount.hidden = draft.priceMode === "midpoint";
  amount.disabled = busy.previewBusy || busy.submitBusy;
  amount.setAttribute("aria-label", `Net limit per strategy for ${strategy.underlying || "strategy"}`);
  amount.addEventListener("change", () => {
    draft.amount = amount.value;
    clearStrategyOutcome(strategy);
    renderStrategies(state.snapshot?.positions || {});
  });

  const preview = document.createElement("button");
  preview.type = "button";
  preview.className = "strategy-preview";
  preview.textContent = busy.previewBusy ? "Checking broker" : draft.operation === "close" ? "Preview close" : "Preview reduction";
  preview.disabled = busy.previewBusy || busy.submitBusy || !strategyPreviewInputValid(draft);
  preview.title = preview.disabled && !busy.previewBusy
    ? "Enter a positive limit amount"
    : "Check current quotes, margin, and the complete combo order";
  preview.addEventListener("click", () => previewStrategy(strategy));

  controls.append(operation, units, priceMode, amount, preview);
  return controls;
}

function clearStrategyOutcome(strategy) {
  const key = strategyKey(strategy);
  delete state.strategyPreviews[key];
  delete state.strategySubmits[key];
}

function strategyPreviewInputValid(draft) {
  if (draft.priceMode === "midpoint") return true;
  const amount = Number(draft.amount);
  return Number.isFinite(amount) && amount > 0;
}

function strategySelectOption(label, value) {
  const option = document.createElement("option");
  option.value = value;
  option.textContent = label;
  return option;
}

function strategyPreviewCard(strategy, preview, submitted, busy) {
  const card = document.createElement("div");
  card.className = "strategy-preview-card";
  if (busy.previewBusy) {
    card.textContent = "Checking current quotes and broker margin.";
    return card;
  }
  if (!preview) {
    card.textContent = strategySubmitResultText(submitted, busy.submitBusy);
    return card;
  }
  if (preview.error) {
    card.classList.add("strategy-preview-card--blocked");
    card.textContent = preview.error;
    return card;
  }
  const group = preview.draft?.strategy_group || {};
  const title = document.createElement("b");
  title.textContent = strategyPreviewTitle(group);
  const terms = document.createElement("span");
  terms.className = "strategy-preview-card__terms";
  terms.textContent = strategyPriceTerms(preview);
  const broker = document.createElement("small");
  broker.textContent = strategyBrokerLine(preview);
  card.append(title, terms, broker);
  const legs = document.createElement("div");
  legs.className = "strategy-preview-card__legs";
  legs.replaceChildren(...(group.legs || []).map(strategyPreviewLeg));
  card.append(legs);
  const status = document.createElement("small");
  status.className = preview.submit_eligible ? "strategy-submit-state strategy-submit-state--ready" : "strategy-submit-state strategy-submit-state--blocked";
  status.textContent = submitted || busy.submitBusy
    ? strategySubmitResultText(submitted, busy.submitBusy)
    : preview.submit_eligible
      ? "Ready to send as one combo order."
      : strategyPreviewBlockedText(preview);
  card.append(status);
  if (preview.submit_eligible && preview.preview_token && !submitted?.accepted) {
    const submit = document.createElement("button");
    submit.type = "button";
    submit.className = "strategy-submit";
    submit.textContent = busy.submitBusy ? "Sending combo" : "Send combo order";
    submit.disabled = busy.submitBusy;
    submit.title = "Send every leg together using this broker-approved preview";
    submit.addEventListener("click", () => submitStrategy(strategy));
    card.append(submit);
  }
  return card;
}

function strategyPreviewTitle(group = {}) {
  const verb = group.operation === "close" ? "Close" : "Reduce";
  const units = group.units || 0;
  return `${verb} ${units} ${units === 1 ? "unit" : "units"} · ${group.units_after || 0} remaining`;
}

function strategyPriceTerms(preview = {}) {
  const value = Number(preview.draft?.limit_price || 0);
  const currency = preview.notional_currency || preview.draft?.contract?.currency || "USD";
  if (value > 0) return `Receive at least ${money(value, currency)} per strategy`;
  if (value < 0) return `Pay up to ${money(Math.abs(value), currency)} per strategy`;
  return "No net debit or credit";
}

function strategyBrokerLine(preview = {}) {
  const status = labelize(preview.what_if?.status || "unavailable");
  const commission = preview.what_if?.margin?.commission;
  const commissionCurrency = preview.what_if?.margin?.commission_currency || preview.notional_currency || "USD";
  const parts = [`Broker preview ${status}`];
  if (typeof commission === "number") parts.push(`estimated commission ${money(commission, commissionCurrency)}`);
  return parts.join(" · ");
}

function strategyPreviewBlockedText(preview = {}) {
  const message = preview.what_if?.message || (preview.warnings || []).map((warning) => warning.message || warning.code).filter(Boolean)[0];
  return message ? `Cannot send: ${message}` : "The broker preview did not approve this combo.";
}

function strategyPreviewLeg(leg = {}) {
  const row = document.createElement("span");
  row.textContent = `${labelize(leg.action || "")} ${leg.quantity || 0} · ${strategyContract(leg.contract)} · ${numberText(leg.before)} → ${numberText(leg.after)}`;
  return row;
}

function strategyLeg(leg = {}) {
  const row = document.createElement("span");
  const direction = Number(leg.ratio) < 0 ? "Short" : "Long";
  const ratio = Math.abs(Number(leg.ratio) || 0);
  row.textContent = `${direction} ${ratio} × ${strategyContract(leg.contract)} · ${numberText(Math.abs(Number(leg.quantity)))} held`;
  return row;
}

function strategyContract(contract = {}) {
  const right = String(contract.right || "").toUpperCase() === "C" ? "call" : String(contract.right || "").toUpperCase() === "P" ? "put" : labelize(contract.right || "option");
  return [formatExpiry(contract.expiry), formatStrike(contract.strike), right].filter(Boolean).join(" ");
}

function formatExpiry(value = "") {
  const raw = String(value).replaceAll("-", "");
  if (!/^\d{8}$/.test(raw)) return value;
  const date = new Date(`${raw.slice(0, 4)}-${raw.slice(4, 6)}-${raw.slice(6, 8)}T00:00:00Z`);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" }).format(date);
}

function formatStrike(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "";
  return Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, "").replace(/\.$/, "");
}

function numberText(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "--";
  return Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, "").replace(/\.$/, "");
}

function strategyKind(value = "") {
  const labels = {
    vertical: "Vertical spread",
    calendar: "Calendar spread",
    diagonal: "Diagonal spread",
    straddle: "Straddle",
    strangle: "Strangle",
    risk_reversal: "Risk reversal",
    two_leg: "Two-leg position",
  };
  return labels[value] || labelize(value || "Option strategy");
}

function strategySource(value = "") {
  const labels = {
    canary_lineage: "Recorded by Canary",
    broker_combo: "Reported as a broker combo",
    inferred: "Grouped from current positions",
    operator_confirmed: "Confirmed by the operator",
  };
  return labels[value] || "Grouped position";
}

function strategyIssueRow(issue = {}) {
  const row = document.createElement("div");
  row.className = "strategy-issue";
  const title = document.createElement("b");
  title.textContent = `${issue.underlying || "Option legs"} need review`;
  const reason = document.createElement("span");
  reason.textContent = issue.reason || "These legs cannot be grouped safely.";
  row.append(title, reason);
  return row;
}

function strategyLimit(draft) {
  if (draft.priceMode === "midpoint") return undefined;
  const amount = Number(draft.amount);
  return draft.priceMode === "debit" ? -amount : amount;
}

async function previewStrategy(strategy) {
  const key = strategyKey(strategy);
  const draft = strategyDraft(strategy);
  if (!strategyPreviewInputValid(draft) || state.strategyPreviewBusy || state.strategySubmitBusy) return;
  state.strategyPreviewBusy = key;
  delete state.strategySubmits[key];
  state.strategyPreviews[key] = null;
  renderStrategies(state.snapshot?.positions || {});
  const body = {
    strategy_id: strategy.id,
    expected_revision: strategy.revision,
    operation: draft.operation,
    units: draft.operation === "reduce" ? Number(draft.units) || 1 : 0,
    timeout_ms: 10000,
  };
  const limit = strategyLimit(draft);
  if (limit !== undefined) body.limit_price = limit;
  try {
    const res = await fetch("/api/strategies/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(body),
    });
    const result = await readJSONOrText(res);
    if (!res.ok) throw new Error(result.error || result.message || String(result));
    state.strategyPreviews[key] = result;
  } catch (err) {
    state.strategyPreviews[key] = { error: err.message };
  } finally {
    if (state.strategyPreviewBusy === key) state.strategyPreviewBusy = "";
    renderStrategies(state.snapshot?.positions || {});
  }
}

async function submitStrategy(strategy) {
  const key = strategyKey(strategy);
  const preview = state.strategyPreviews[key];
  if (!preview?.submit_eligible || !preview.preview_token || state.strategySubmitBusy || state.strategySubmits[key]?.accepted) return;
  const confirmation = protectionWriteConfirmation();
  if (!confirmation) {
    state.strategySubmits[key] = { error: "Trading account and mode are unavailable." };
    renderStrategies(state.snapshot?.positions || {});
    return;
  }
  state.strategySubmitBusy = key;
  renderStrategies(state.snapshot?.positions || {});
  try {
    const res = await fetch("/api/strategies/submit", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        preview_token: preview.preview_token,
        confirm_account: confirmation.account,
        confirm_mode: confirmation.mode,
      }),
    });
    const result = await readJSONOrText(res);
    if (!res.ok) throw new Error(result.error || result.message || String(result));
    state.strategySubmits[key] = result;
    if (result.accepted) await refreshOpenOrders();
  } catch (err) {
    state.strategySubmits[key] = { error: err.message };
  } finally {
    if (state.strategySubmitBusy === key) state.strategySubmitBusy = "";
    renderStrategies(state.snapshot?.positions || {});
  }
}

function strategySubmitResultText(result, busy) {
  if (busy) return "Sending the combo order. Check TWS before trying again.";
  if (!result) return "";
  if (result.error) return `Order not sent: ${result.error}`;
  if (result.accepted) return "Combo order sent. Check Open Orders and TWS for broker status.";
  return result.message ? `Order not sent: ${result.message}` : "The combo order was not accepted.";
}

export { formatExpiry, previewStrategy, renderStrategies, strategyContract, strategyKind, strategyLimit, strategyPreviewBlockedText, strategyPreviewInputValid, strategyPriceTerms, strategySource, submitStrategy };
