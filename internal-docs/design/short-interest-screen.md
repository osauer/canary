# Short-interest research

`canary short-interest screen --json` and MCP `canary_short_interest_screen`
adapt the daemon's `short_interest.screen` read-only contract. This is an
independent FINRA US equity universe, including listed funds and OTC; it is not
restricted to the highest-fee borrowing discoveries or the user's holdings.

The daemon reads the latest link on FINRA's official [files page](https://www.finra.org/finra-data/browse-catalog/equity-short-interest/files),
then its exact HTTPS `cdn.finra.org/equity/otcmarket/biweekly/shrtYYYYMMDD.csv`
publication. HTTP has fixed allowed origins, a 45-second overall bound, a 2 MiB
index limit, a 16 MiB file limit and a 30,000-row bound. One joined worker refreshes
on demand at most every six hours; failures retry after 30 minutes. RPC reads
do not download FINRA publications. The last good publication is atomically retained
in private daemon state, outside Git. A refresh failure preserves provenance and
marks retained rows stale. A receipt older than 48 hours or settlement older than
35 days is stale. This is twice-monthly data; receipt time is not settlement or
publication time, and no unverified publication date is invented.

Default ordering is reported short shares descending. Shared price, average
20-session dollar-volume filters and sorting happen in Canary before the output
limit (1–100). Source filters also support reporting-cycle average daily shares,
days to cover, listed-only (exclude OTC) and up to 100 excluded symbols. Missing
values sort last in either direction; symbol breaks ties. Source rankings cover
the full accepted publication; market rankings/filters use the covered subset.

Market enrichment shares the existing single background stock-context worker
and bounded per-family admission. Missing exact USD contract identity remains
unavailable, with explicit candidate/covered/pending/unavailable counts. No broad
ambiguous contract-resolution scan is performed. Repeated reads do not bypass
broker pacing or create individual subscriptions. Borrow fees carry their own
`fee_as_of` clock; they are borrower costs, not the FINRA settlement position.

FINRA's [glossary](https://www.finra.org/finra-data/browse-catalog/equity-short-interest/glossary)
defines average daily volume over the reporting interval, unlike the market
context's 20 completed trading sessions. FINRA floors days to cover at 1.00 and
zero-volume rows have no usable days-to-cover value. Split rows suppress the
change percentage; revision and split flags remain visible. Short interest is
not daily short-sale volume, borrower fees, lender yield or a trading signal.
No verified dated free-float feed is available: percentage of float stays absent,
and shares outstanding are never substituted. No paid feed, enrollment, order,
broker setting or risk policy is changed.
