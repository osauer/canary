# Glossary

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Any reader who hits a term the other pages assume they know.

**Shape.** Alphabetical, one short paragraph each, every entry linking to the
page where the term does real work. No entry longer than three sentences.

**Terms to cover, at minimum**

- Broker and account: NLV, buying power, managed account, paper and live.
- Risk: risk unit, R-multiple, fixed-fractional sizing, drawdown ladder.
- Options: Greeks, open interest, implied move, theta hygiene, zero gamma.
- Market state: regime stage, breadth, LULD, Reg SHO, borrow stress.
- System: daemon, sensor, last-good, freshness, semantic fingerprint, preview
  token, journal, capital event, agent origin.

**Draw from**

The glossaries already sitting at the end of `docs/docs/understand/policy.md` and
`docs/docs/internals/storage.md`, plus the definitions scattered through `docs/docs/understand/concepts.md`
and `docs/docs/understand/sensors.md`. Consolidating them here means those pages can drop
their local glossaries and link instead.

**Boundaries to keep**

- Definitions describe what the term means in this system. Where the industry
  uses a term differently, say so rather than quietly redefining it.
