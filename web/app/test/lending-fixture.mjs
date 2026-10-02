// Fictional contracts, shares and fees only. No owner portfolio data.
function lendingFixture() {
  return {
    summary: {
      schema_version: "financing.v1", state: "complete", from: "2026-09-29T00:00:00Z", to: "2026-10-01T00:00:00Z",
      as_of: "2026-10-01T00:00:00Z", base_currency: "EUR", earned_base: 3.78, known_earned_base: 3.78,
      native: [{ currency: "USD", amount: 4.2 }], fee_count: 2, covered_days: 2, expected_days: 2,
      pnl_reconciliation: "unproved", payment_linkage: "unavailable", fingerprint: `finance_${"a".repeat(32)}`,
    },
    fees: [
      { id: `fee_${"a".repeat(32)}`, con_id: 900901, symbol: "SYNTH-LEND", value_date: "2026-10-01T00:00:00Z", start_date: "2026-09-30T00:00:00Z", currency: "USD", quantity: 60, net_fee: 2.1, net_rate_pct: 4.2, collateral: 18000, fx_rate_to_base: .9, base_amount: 1.89 },
      { id: `fee_${"b".repeat(32)}`, con_id: 900901, symbol: "SYNTH-LEND", value_date: "2026-09-30T00:00:00Z", start_date: "2026-09-30T00:00:00Z", currency: "USD", quantity: 60, net_fee: 2.1, net_rate_pct: 4.2, collateral: 18000, fx_rate_to_base: .9, base_amount: 1.89 },
    ], filtered_count: 2,
  };
}
const lendingPositionFixture = () => ({ as_of: "2026-10-01T00:00:00Z", state: "reported", quantity: 60, owned_quantity: 100, currency: "USD", net_rate_pct: 4.2, collateral: 18000 });
export { lendingFixture, lendingPositionFixture };
