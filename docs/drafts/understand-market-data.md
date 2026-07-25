# Market data and entitlements

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone confused about why a quote says delayed, or wondering
which IBKR subscriptions are worth paying for.

**Questions the page has to answer**

- Data arrives through the reader's own TWS or Gateway session, so entitlements
  match what they already see in TWS. Nothing is resold or proxied.
- What `live`, `frozen`, `delayed`, and `delayed-frozen` each mean, and which
  decisions each one is good enough for.
- Which surfaces need which entitlement. Options chains, SPX gamma, and breadth
  have different requirements from a stock quote.
- What happens outside a session, and how calendar context explains a quote that
  looks wrong.
- Where pacing limits show up, and why a first breadth build can take about an
  hour.
- Which parts work with no market-data subscription at all.

**Draw from**

- `docs/docs/understand/concepts.md`, "Market Calendars".
- `docs/docs/understand/sensors.md` for freshness and last-good semantics.
- The `session_context` field in the quote path.

**Boundaries to keep**

- Every claim about real-time data stays conditional on the reader's own
  subscriptions. The site-wide phrasing is real-time wherever their IBKR
  market-data subscriptions cover it, delayed where they do not.
