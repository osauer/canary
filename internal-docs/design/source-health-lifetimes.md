# Source health and acquisition lifetimes

`data.health` remains schema version 1. The additive dimensions are optional;
absence means an older or unsupported producer contract, not a healthy source.
`state` remains the conservative attention verdict for existing clients.

| Field | Evidence and meaning |
| --- | --- |
| `availability` | Acquisition/access observed by its producer: `available`, `limited`, `unavailable`, or `unknown`. A failed latest public-source fetch can coexist with retained prior data. This is not analytical usability or portfolio applicability. |
| `data_type` | Existing delivery mode, including delayed, frozen, delayed-frozen or mixed. Receipt of delayed/frozen data alone is not a source outage. Instrument clocks and execution suitability remain in the instrument DTO. |
| `cadence_state` | A producer's verified `not_due`, `pending`, or `overdue` schedule. Not-due does not clear a failed acquisition or a blocked analytical result. |
| `applicability` | `not_relevant` only when the owner verified that the current scope needs no evidence. For example, no exact held short stocks cannot prove the borrow-fee provider recovered. A not-relevant row keeps its availability, failure and real receipts but is not required and adds no concern. `required` remains the compatibility field. |
| `usability` | The analytical owner's quality verdict: `usable`, `limited`, `blocked`, or `unknown`. Currently populated for gamma, with bounded owner-authored `usability_reason`. Retained rankability alone cannot establish current usability when its publication or source is stale. |

The summary counts source concerns and unknown coverage. It does not count
independent incidents or individual missing instruments. Desk delivery health
remains a separate observation.

## Retained chronology

History dates state observations with the daemon's observer clock, independently
of provider last-attempt, receipt, publication or price clocks. `last_success`
uses actual producer receipt evidence; passive reads, not-due scheduling and
an irrelevant-scope verdict never advance it, and a producer that delivered
nothing reports no source clock. Records are versioned. An unversioned record
restores its `last_success`, except `events:borrow_fee`: an earlier recorder
counted its not-applicable verdict as success, so it keeps at most the newest
receipt the borrow-fee authorities prove, and none when they never recorded
one. Retention is seven days and at most 128
transitions per source, with a two MiB durable-document ceiling. Count/age pruning
sets `history_truncated`. Legacy six-row history is marked truncated when its
first-observed clock precedes its oldest retained transition. Restart adds an
explicit observation gap and restores no availability, entitlement or session.
There is no uptime or continuity claim across these gaps or truncations.

Immutable report revisions retain their existing two-minute page traversal
window. Each complete encoded page is at most 30 KiB, including concerns and
check metadata. `next_offset` advances by the rows actually emitted. A source
that cannot fit on its own returns an explicit error; no source or history is
silently removed to meet the wire budget.

## Acquisition boundaries

- Front-future resolutions retain only verified dated contract identities, never
  prices. Keys include full contract identity and broker socket/epoch. The owner
  retains a bounded wire flight after a short/canceled waiter so its late success
  can serve a later read. There are at most 64 keys/active flights, six hours of
  successful reuse capped at the next UTC date, and 15 seconds of failure backoff.
  Reconnect, expiry and failed refresh cannot return an old identity as usable.
- Shortable inventory retains actual tick receipts for two minutes within the
  same broker session. Releasing a subscription does not erase those receipts;
  reading them does not advance their clock. Negative probes are bounded to
  512 entries and 30 minutes. A fresh receipt supersedes a negative probe.
  Partial requested coverage remains partial; actual zero shares remains an
  observed zero. Restart restores no current receipt or negative authority.
- Market-event source health describes the held book. The daemon loops, the
  app and Stress derive one held-name scope (`rpc.MarketEventScope`). Only a
  read of exactly the scope the daemon derived within the last two minutes
  records `events:*` health, so an explicit-symbol read cannot overwrite it.
  Reg SHO and halt checks cover every held name. Shortable-inventory coverage
  does not expect a name no position expects market data for; the notes count
  it as not expected rather than missing.
- Borrow fee and shortable inventory take their applicability from the
  portfolio on every read, whatever the provider's schedule or backoff:
  `not_relevant` when a current, same-account portfolio stream holds no exact
  short stock among the held names. While the stream is not current (reconnect,
  resubscription, short download, quiet period) the last such verdict answers
  for the same account and names for up to 15 minutes; after that, or without
  one, the rows are required. A new name, another account or a held short stock
  ends it at once, and a restart restores none.
- Borrow-fee refreshes try IBKR's documented FTP host (ftp3) and IBKR's
  mirror (ftp2) in order within one attempt, starting with the host that served
  the retained file, and record the serving host as the source URL. Each control
  command has a 10-second deadline and the transfer a separate 45-second budget;
  a timeout at greeting or login reconnects once before failing over. When every
  host fails, the failure that progressed furthest is recorded, with the durable
  15-minute retry and the existing exact held-short TWS historical fallback.
  Cancellation while queued or during acquisition preserves prior failure/backoff
  evidence and closes sockets; the body limit is 16 MiB. Off-hours, `next_attempt`
  is the next regular US open. The file has no quoting: a quote in a name is text,
  a malformed row is skipped and counted in the source notes, and a published
  `#EOF` count must match. `>N` availability is kept as a lower bound, never a
  scarcity reading, and `NA` rates are kept as unpublished, never zero (state
  version 3; version 2 loads unchanged). Entitlement, scale validation and
  trading policy are unchanged.
- The New York Fed calendar is an independent, dated partial backup for key
  releases. It cannot repair a failed BLS source or establish complete BLS/event-free
  coverage. Ordinary access rejection remains visible. No alternate credentials,
  user-agent bypass, hardcoded release dates or invented calendar events are used.

## Gamma warning compatibility

Every accepted typed warning must survive JSON/cache rehydration without becoming
an unknown warning. Expiry-grid age retains the existing quality thresholds;
no-cache and rejected-cache results explicitly block. A genuinely unclassified
warning remains blocked. Previously polluted persisted snapshots cannot have their
unknown cause reconstructed safely: they remain blocked until a new valid
producer computation replaces them. The fix prevents recurrence, not retroactive
fabrication of successful evidence.

Replacement is newest-evidence, not best-quality: the first successful compute
of the next regular options session replaces the retained snapshot even when its
own quality gates block it. A failed refresh keeps the retained snapshot, adds
`refresh_failed:<token>` and retries under the escalating backoff that a broker
reconnect resets. While gamma is `not_due`, its health row's `next_attempt` is
the next regular U.S. listed-options open from the embedded market calendar,
the earliest time a scheduled refresh can run. It is absent outside calendar
coverage and is not a regime `next_due_at`.
