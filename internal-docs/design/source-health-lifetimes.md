# Source health and acquisition lifetimes

`data.health` remains schema version 1. The additive dimensions are optional;
absence means an older or unsupported producer contract, not a healthy source.
`state` remains the conservative attention verdict for existing clients.

| Field | Evidence and meaning |
| --- | --- |
| `availability` | Acquisition/access observed by its producer: `available`, `limited`, `unavailable`, or `unknown`. A failed latest public-source fetch can coexist with retained prior data. This is not analytical usability or portfolio applicability. |
| `data_type` | Existing delivery mode, including delayed, frozen, delayed-frozen or mixed. Receipt of delayed/frozen data alone is not a source outage. Instrument clocks and execution suitability remain in the instrument DTO. |
| `cadence_state` | A producer's verified `not_due`, `pending`, or `overdue` schedule. Not-due does not clear a failed acquisition or a blocked analytical result. |
| `applicability` | `not_relevant` only when the owner verified that the current scope needs no evidence. For example, no exact held short stocks cannot prove the borrow-fee provider recovered. `required` remains the compatibility field. |
| `usability` | The analytical owner's quality verdict: `usable`, `limited`, `blocked`, or `unknown`. Currently populated for gamma, with bounded owner-authored `usability_reason`. Retained rankability alone cannot establish current usability when its publication or source is stale. |

The summary counts source concerns and unknown coverage. It does not count
independent incidents or individual missing instruments. Desk delivery health
remains a separate observation.

## Retained chronology

History dates state observations with the daemon's observer clock, independently
of provider last-attempt, receipt, publication or price clocks. `last_success`
uses actual producer receipt evidence; passive reads, not-due scheduling and
irrelevant scope never advance it. Retention is seven days and at most 128
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
- Borrow-fee refreshes use the existing official FTP source, durable 15-minute
  failure retry and existing exact held-short TWS historical fallback. Cancellation
  while queued or during acquisition preserves prior failure/backoff evidence.
  FTP cancellation closes sockets; the transfer has its existing time bound and
  a 16 MiB body limit. Entitlement, scale validation and trading policy are unchanged.
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
reconnect resets.
