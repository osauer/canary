import { $, calendarDate, hasNumericValue, money, sensitiveMoney, shortTimeWithZone } from "./shared.js";
import { state } from "./state.js";

const PRIORITIES = { usd_first: "USD first", balanced: "Balanced", eur_first: "EUR first" };

function cashSweepReason(reason) {
  if (String(reason || "").startsWith("reserve_calibration_required:")) return "Funding and stress reserve needs validation.";
  return String(reason || "").replace(/^reserve_[a-z_]+:\s*/, "");
}

function cashSweepMoney(value, currency) {
  return hasNumericValue(value) ? sensitiveMoney(value, currency) : "Unavailable";
}

// This panel renders daemon decisions; no UI calculation supplies authority.
function renderCashSweepPanel(sweep) {
  const panel = $("cashSweepPanel");
  if (!panel) return;
  panel.hidden = !sweep;
  if (!sweep) return;
  $("cashSweepPolicy").textContent = [PRIORITIES[sweep.currency_priority] || "Native cash", sweep.mode,
    hasNumericValue(sweep.reserve_cushion_eur) ? `${money(sweep.reserve_cushion_eur, "EUR")} total cushion` : ""].filter(Boolean).join(" · ");
  $("cashSweepReserve").textContent = cashSweepReason(sweep.reserve_reason || (state.accountValueVisible ? sweep.reason : ""));
  $("cashSweepReserve").hidden = !$("cashSweepReserve").textContent;
  const rows = (sweep.currencies || []).map((currency) => {
    const row = document.createElement("p");
    const summary = document.createElement("b");
    summary.textContent = `${currency.currency} · ${String(currency.state || "unavailable").replaceAll("_", " ")}`;
    const figures = document.createElement("span");
    const reserve = sweep.reserve_state === "unavailable" ? null : currency.effective_reserve ?? currency.keep_cash;
    figures.textContent = `Cash ${cashSweepMoney(currency.cash, currency.currency)} · committed ${cashSweepMoney(currency.committed, currency.currency)} · reserve ${cashSweepMoney(reserve, currency.currency)} · free ${cashSweepMoney(currency.free, currency.currency)}`;
    const reason = document.createElement("small");
    reason.textContent = state.accountValueVisible && currency.reason !== sweep.reserve_reason ? cashSweepReason(currency.reason) : "";
    row.append(summary, figures, reason);
    return row;
  });
  $("cashSweepCurrencies").replaceChildren(...rows);
  $("cashSweepTraceState").textContent = sweep.trace_state === "recorded" ? "Recorded in SQLite" : "Audit unavailable";
  const history = (sweep.decision_trace || []).slice(0, 5).flatMap((trace) => (trace.currencies || []).map((currency) => {
    const row = document.createElement("p");
    row.textContent = `${calendarDate(trace.at)} ${shortTimeWithZone(trace.at)} · ${PRIORITIES[trace.currency_priority] || "Native cash"} · ${currency.currency} · ${String(currency.action).replaceAll("_", " ")} · ${state.accountValueVisible ? cashSweepReason(currency.reason) : ""}`;
    row.title = `Policy ${trace.policy_id} v${trace.policy_version} · ${trace.policy_fingerprint}`;
    return row;
  }));
  $("cashSweepHistory").replaceChildren(...history);
}

export { renderCashSweepPanel };
