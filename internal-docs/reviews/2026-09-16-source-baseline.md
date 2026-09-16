# Canary source baseline — 16 September 2026

This change preserves the complete pending source from the saved checkout at
`dc6a6dd4ed40742fc38f7f5d0117f23083e6cf38`. All 81 modified or untracked files
copied into the isolated worktree matched the saved source before editing.
Eighty were source or documentation; one was a synthetic browser screenshot.
The screenshot is private verification evidence, not a product asset.

## Scope and dependency decision

The candidate combines international exchange calendars, daemon-owned service
health, delayed quote recovery, chart-history continuity, app SSE/logging,
stress account binding, and standing option-purpose configuration. The related
CLI, RPC, MCP, SPA, generated references, and tests travel together.

The exact bid/ask repair consumes `MarketData.FeedType`. The connector, wire
mode switching, fallback implementation, display projection, and daemon quote
helpers are therefore required dependencies, not optional earlier work. A
ten-file subset does not form a compilable or reproducible baseline. One
integrated commit preserves the tested source rather than splitting these
cross-surface dependencies into intermediate broken states.

HyperServe v2.1.5 was the latest published stable release at review and matches
the concrete module and checksum pins. Ordinary builds do not discover or
upgrade dependencies. Strict JSON and SSE flush-error handling remain in place.

## Review corrections

- Read the connector's connection pointer under its existing mutex when
  classifying subscription data.
- Keep retained delayed prices and retry state when a live probe returns only
  statistics such as daily or yearly highs. Only usable quote-price fields
  complete recovery; non-finite values cannot establish it.
- Retain access-restricted source observations independently from another
  successful observation in the same data mode, until their own expiry.
- Include all supported markets in the calendar command's usage text.

Focused synthetic cases cover statistics-only recovery and same-mode health
restriction retention. Existing exact-quote tests cover request-owned bid/ask
times, unknown feed notices, delayed/frozen data, stale sides and cached sides.
Standing-purpose cases retain exact overrides, strategies, short-book
exceptions, complete-book evidence and close-only sizing.

## Authority and verification

Broker connectivity and producer observations remain daemon-owned. Health
reports describe services; they neither expose a holdings inventory nor certify
an individual instrument as tradable. Stress account identity is emitted only
when the account and positions authorities agree. Standing-purpose settings do
not grant order authority or bypass fresh exact-contract/risk evidence.

`make test` passed locally, including `make check`, modernization, vulnerability
and privacy checks, docs/SPA contracts, synthetic browser rendering, race tests
in both daemon build modes, hermetic CLI lifecycle tests, and four historical
regression witnesses. The witness reads committed HEAD; hosted CI must also
run it on the final committed candidate.

Additional component QA exercised the production data-health renderer with
synthetic reports at widths 1100 and 390, with explicit unknown source times,
restricted live access, no horizontal overflow and no script errors. Stock
Chromium was absent; Canary's standard helper used installed Chrome. This is
desktop synthetic proof, not installed-runtime or physical-phone proof.

No live broker smoke, installed daemon/app refresh, state/config changes,
paid model requests, or release publication is part of this baseline. Those
boundaries follow the task's source-only authorization. Private evidence and
the saved-checkout alignment record remain outside Git.

## Pending release notes

These reader-facing notes await the next authorized release. The existing
published changelog is not retroactively changed, and no release version is
assigned by this source cleanup.

### Added

- Service-health reports show what Canary is receiving, source restrictions,
  missing evidence and automatic retry state in the app, CLI and MCP.
- Exchange calendars include London, Tokyo and Hong Kong, with lunch breaks,
  early closes and explicit limits on published coverage.

### Changed

- Approved standing option-purpose configuration can classify ordinary long
  calls as directional and index puts as protection while preserving exact
  exceptions and escalating genuine conflicts.
- Stress assessments carry matching account and mode identity when current
  account and position sources establish it.

### Fixed

- Quotes retain usable delayed data while live access is unavailable, and
  displays preserve delayed labels and original source times.
- Current option bid/ask quotes no longer inherit an old last trade's time.
- Chart history retains its resolved instrument and completed-session context
  across closures and incremental refreshes.
- Healthy idle app streams remain open; stalled writes are bounded, and
  request logs omit query credentials.

Broker-write authority, risk thresholds and freeze controls are unchanged.
