// Synthetic market values, never an account or captured broker response.
export function marketTapeFixture() {
  const dates = ["08-26", "08-27", "08-28", "08-31", "09-01", "09-02", "09-03", "09-04", "09-08", "09-09", "09-10", "09-11", "09-14", "09-15", "09-16", "09-17", "09-18", "09-21", "09-22", "09-23"];
  const price = (i, index) => ({ close: 100 + i, change_pct: i === 19 ? 0 : 1, window_change_pct: i, volume: index ? null : i === 3 ? 0 : 1e6 + i * 1e5, relative_volume_20: index ? null : i === 3 ? 0 : 1.2 });
  return {
    schema_version: "market-tape-v1", as_of: "2026-09-24T10:00:00Z", timezone: "America/New_York", latest_session: "2026-09-23", coverage_status: "partial", historical_availability: "unknown", not_predictive: true,
    sessions: dates.map((date, i) => ({ date: `2026-${date}`, spx: i === 5 ? null : price(i, true), qqq: price(i, false), breadth: i === 4 ? null : { pct_above_50dma: 65 - i, pct_above_200dma: 75 - i, change_50_pp: i === 5 ? null : -1, new_highs: 0, new_lows: 3, member_count: 100, coverage_50: 90, coverage_200: 85, coverage_highs_lows: 85 }, reading: { headline: i === 19 ? "Price held; trend participation narrowed" : "Synthetic session reading", summary: "The index held while fewer measured stocks stood above their 50-day average.", evidence: [{key: "advance_decline", label: "Daily participation", value: "Not collected for this session", meaning: "Trend breadth cannot reconstruct daily advancers."}], watch_for: ["Compare price and participation in the next completed session."], limits: ["Descriptive history; no forecast."] } })),
    sources: ["spx", "qqq", "breadth"].map((key) => ({ key, source: "Synthetic observations", status: key === "qqq" ? "available" : "partial", as_of: "2026-09-23T22:00:00Z", covered_through: "2026-09-23", missing_sessions: key === "qqq" ? 0 : 1, detail: "Original historical availability unknown" })),
    notes: ["Retrospective observations; no predictive claim.", "QQQ is ETF volume, not signed market flow."],
  };
}
