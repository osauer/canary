# Owner watchlist

Canary owns one ordered list of at most 20 distinct USD SMART stock underlyings.
It is a version-1 `watchlist` state document in `daemon.db`; preferences never
authorize a trade or change risk policy. No gateway connection, quote, historical
read, subscription or broker contract lookup is needed to read or edit the list.
Desk owns playbook settings; it consumes this list instead of keeping another
authoritative copy in Torok.

Each symbol has exactly `symbol`, `con_id`, `sec_type`, `currency`, `exchange`.
Symbols are trimmed and uppercased, then match `[A-Z0-9][A-Z0-9.]{0,31}`. Missing
type, currency and exchange default to `STK`, `USD`, `SMART`; other values fail.
IDs are 0 through MaxInt32. Zero is explicitly unresolved owner input; positive
IDs are preserved, never silently replaced. Symbols and positive IDs must be
distinct. Resolution and exact contract validation remain downstream read steps.
Adding an existing symbol with different identity fails; use explicit replacement
to change its identity.

## Durable ownership and retries

`watchlist.list` returns `{version:1,revision,symbols:[],as_of}` with an explicit
array and a fresh read clock. Revision 0 means pristine absence only. The first
accepted mutation creates revision 1 even when the list is empty; later clears
remain nonzero. Missing or corrupt/unaccepted storage returns an error, never
an apparently pristine list. No code reads or imports the retired
`~/.local/share/ibkr/watchlist.json`.

`watchlist.replace`, `.add` and `.remove` require `expected_revision` and immutable
`request_id`. A mutation atomically saves its document and receipt in the existing
SQLite state/event transaction. Later semantic noops append a receipt under a
revision fence without changing the document revision. Receipt terms include the
operation, normalized payload and expected revision. Reusing an ID for changed
terms fails with `watchlist_conflict`; replaying the original terms returns the
current list with `request_id`, original `saved_revision` and `replay:true`.
An old retry never reapplies an old list. Receipts use agent origin and confer no
human trading authority.

Desk migration may copy an explicitly saved legacy Desk list (including an empty
list) only with expected revision 0 and a stable migration request ID. Any competing accepted list, including an
intentional empty list, wins over migration. Revision 0 is readable but never
causes implicit seed/import or a hidden mutation.

## CLI and public adapter

```sh
canary watchlist list --json
canary watchlist add SYNTH --con-id 17 --json
canary watchlist remove SYNTH --expected-revision 1 --request-id owner-remove-1 --json
canary watchlist replace --spec - --json
```

Replace reads one strict JSON object from stdin (`-`) or a private file:

```json
{"symbols":[],"expected_revision":1,"request_id":"owner-clear-1"}
```

`list --json` exports the accepted snapshot. Import uses `replace --spec PATH|-`
with the desired `symbols`, an explicitly chosen revision fence and fresh request
ID; a full snapshot is not a replace request. Add/remove can read the current
revision once and generate an ID when omitted. They perform one CAS, never retry
a write silently. A failed command reports the ID and fence needed to retry
unchanged terms.

All errors exit nonzero. With `--json`, an actual daemon `watchlist_conflict`
also emits this fixed stdout envelope:

```json
{"error":{"code":"watchlist_conflict","message":"Watchlist terms or revision changed; refresh before saving"}}
```

Other errors emit no structured stdout claim. A timeout, connection loss or storage
failure has an unknown mutation outcome: retain the exact request and retry its ID.
A reread with a newer revision alone cannot prove conflict or non-application.

The typed public Go adapter exposes only `Client.Watchlist(ctx)`; no watchlist
mutation is added to MCP. The owner CLI uses local-preference guards. No dedicated
watchlist sidecar, second ledger, generic rule language or broker write is added.

## Verification boundary

Synthetic tests cover offline pristine and intentional empty ownership, immutable
receipts, current-state replay, fenced noops, competing CAS, restart persistence,
invalid/cancelled/unavailable state, lost acknowledgement after watermark failure,
strict CLI input and explicit conflict vs unknown error output. Installation and
private-list migration are separate owner-runtime steps after source acceptance.
