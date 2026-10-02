import assert from "node:assert/strict";
import test from "node:test";
import { createDOMHarness } from "./dom-harness.mjs";

const dom = createDOMHarness();
globalThis.document = dom.document;
globalThis.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
const { renderCashSweepPanel } = await import("../cash-sweep.js");
const { state } = await import("../state.js");

test("sweep panel names missing reserve and never shows guessed zero", () => {
  renderCashSweepPanel({ mode: "active", currency_priority: "usd_first", reserve_cushion_eur: 10000, reserve_state: "unavailable",
    reserve_reason: "reserve_calibration_required", trace_state: "recorded", currencies: [{ currency: "USD", state: "hold", cash: 20000, committed: 0, keep_cash: 5000, reason: "Missing calibrated funding" }],
    decision_trace: [{ at: "2099-01-05T12:00:00Z", policy_id: "synthetic", policy_version: 1, currency_priority: "usd_first", currencies: [{ currency: "USD", action: "hold", reason: "Missing calibrated funding" }] }] });
  assert.equal(dom.document.getElementById("cashSweepPanel").hidden, false);
  assert.match(dom.document.getElementById("cashSweepPolicy").textContent, /USD first.*active.*total cushion/);
  assert.match(dom.document.getElementById("cashSweepCurrencies").textContent, /reserve Unavailable.*free Unavailable/);
  assert.match(dom.document.getElementById("cashSweepTraceState").textContent, /Recorded in SQLite/);
  assert.match(dom.document.getElementById("cashSweepHistory").textContent, /USD first.*USD.*hold/);
});

test("sweep panel treats broker text as text and performs no requests", () => {
  state.accountValueVisible = true;
  globalThis.fetch = () => { throw new Error("unexpected network call"); };
  renderCashSweepPanel({ currencies: [{ currency: "EUR", state: "hold", reason: "<script>evil()</script>", cash: 0, committed: 0, free: 0, keep_cash: 0 }] });
  assert.match(dom.document.getElementById("cashSweepCurrencies").textContent, /<script>evil\(\)<\/script>/);
  assert.match(dom.document.getElementById("cashSweepTraceState").textContent, /Audit unavailable/);
  renderCashSweepPanel(null);
  assert.equal(dom.document.getElementById("cashSweepPanel").hidden, true);
});

test("sweep privacy masks balances and money-bearing reasons", () => {
  state.accountValueVisible = false;
  renderCashSweepPanel({ currencies: [{ currency: "USD", state: "invest", cash: 198765, free: 123456, reason: "buy 123456 USD" }],
    decision_trace: [{ at: "2099-01-05T12:00:00Z", currencies: [{ currency: "USD", action: "invest", reason: "buy 123456 USD" }] }] });
  assert.doesNotMatch(dom.document.getElementById("cashSweepCurrencies").textContent, /123456|198765|198,765|123,456/);
  assert.doesNotMatch(dom.document.getElementById("cashSweepHistory").textContent, /123456/);
});
