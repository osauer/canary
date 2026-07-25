# Documentation structure

Updated: 2026-07-25 07:35 CEST

How the public documentation at <https://osauer.dev/ibkr/docs/> is organised,
why it is organised that way, and what still has to be written.

## The problem this replaced

Before this change the site published fifteen documentation pages with no index
and no navigation entry. Nine of them had no inbound link from the landing page
or the header, so they were reachable only through the sitemap and through
cross-references buried in other pages. The landing page and the generated
pages carried two different navigation lists with nothing keeping them in step.
Trader guides, generated reference, architecture deep-dives, and internal specs
all sat at one flat level, so a reader arriving at any of them had no way to
tell who the page was written for or what to read next.

## What the structure is

Five sections, ordered by how far the reader has travelled: get it running,
run it, understand what it says, look things up, see how it is built. The first
four are written for someone trading their own account. The fifth is for
someone reading the code.

| Section | URL | For |
| --- | --- | --- |
| Start | `/ibkr/docs/start/` | Installing, connecting a host, the first session, updates, and what to do when it breaks. |
| Operate | `/ibkr/docs/operate/` | The daily loop: briefs, agents, the paired app, order previews, protection, reconciliation. |
| Understand | `/ibkr/docs/understand/` | What each measurement means and who sets the limits it is measured against. |
| Reference | `/ibkr/docs/reference/` | Command, tool, and configuration lookups. Generated from the code wherever possible. |
| Under the hood | `/ibkr/docs/internals/` | Architecture, storage, the wire protocol, published design notes, packaging. |

`docs/` is the published site and holds nothing else. Design records and
developer guides live in `internal-docs/`; the task contracts and hygiene notes
an agent is told to load live in `.agents/docs/`. The line between those two is
whether a document instructs the agent or describes the system. Three former
`docs/design/` and `docs/specs/` documents had genuine public value and moved
into Under the hood instead.

## Decisions worth recording

**Markdown sits beside the HTML it produces.** `docs/docs/internals/storage.md`
publishes to `/ibkr/docs/internals/storage.html`, and the generator derives the
second from the first. There is no mapping table to keep honest, a link that is
correct in the source is already correct on the site, and the Markdown reads
correctly on GitHub too. The manifest adds only what the file system cannot
say: section, index copy, and retired URLs.

**The site root is `docs/`, so `/ibkr/docs/` lands at `docs/docs/` on disk.**
Mildly surprising in a diff, and worth it: a "Documentation" navigation entry
should lead to a URL that says `docs`.

**Every retired URL redirects.** The fifteen pages that moved kept their old
paths as generated `noindex` redirect stubs pointing at the canonical URL. The
generator owns those stubs, so `make docs-html-check` protects them the same
way it protects the pages.

**Planned pages appear without a page.** A planned entry carries a title and a
one-line scope and renders on the index with a "not written yet" marker and no
link. It produces no HTML, no URL, and no sitemap entry. A reader can see the
whole shape of the handbook; a search engine only ever sees finished work. The
brief for each one lives in `internal-docs/drafts/`, outside the site tree, so
it is never served.

**Three checks stop the structure from rotting.** The generator refuses a
manifest entry without a section, title, and summary. One test asserts the
hand-written landing page carries the same navigation items in the same order
as the generated pages. Another asserts every published page is in the sitemap
and no retired URL is still listed.

## What is published

Eighteen pages. Fifteen were already public. Three were written but never
published: the paired-app guide, the order-preview and trading-build guide, and
the opportunity research harness.

## What is missing

Fourteen pages, in the order they should be written. Each one has a brief in
`internal-docs/drafts/`.

**First, because their absence is the most visible.**

1. `reference/cli` — 36 subcommands, and no reference for any of them on the
   site. This should be generated from the registry in `internal/cli`, the same
   way `mcp-tools` and `config` are generated. Build the generator, then flip
   the manifest entry. Roughly a day, and then it maintains itself.
2. `start/install` — the install story lives in the README and in five separate
   setup pages. The handbook has no entry point of its own.
3. `start/troubleshooting` — the README section is the only version, and it is
   the page a frustrated user goes looking for first.

**Then the daily-use gaps.**

4. `operate/daily-desk`
5. `understand/rulebook` — fourteen rules on the landing page, no public page
   explaining them. `internal-docs/design/trading-rulebook.md` is the authority to
   distil from; it stays in the repository.
6. `understand/risk-policy`
7. `operate/protection`
8. `understand/market-data`

**Then the rest.**

9. `start/hosts`
10. `start/first-session`
11. `operate/alerts`
12. `operate/reconciliation`
13. `understand/glossary` — once it exists, the local glossaries at the end of
    `understand/policy.md` and `internals/storage.md` can link to it instead.
14. `reference/releases`

## Page-level work, separate from structure

Three pages are long enough to be worth splitting, and none of it is urgent.
`understand/sensors.md` runs 3,400 words across five sensor families and would
read better as one page per family. `internals/architecture.md` runs 3,600 words
but is a reference-style read with a section index, so it can stay whole.
`understand/concepts.md` overlaps `sensors.md` in several places; once the
glossary and market-data pages exist, some of that overlap should collapse.

## Adding a page

1. Write the Markdown at `docs/docs/<section>/<page>.md`. That path is the URL.
2. Add or update the manifest entry in `scripts/docgen/docs-html/main.go`:
   `Source`, `Section`, `NavTitle`, `Summary`, `Description`, and
   `Layout: "architecture"` if it is long enough to want a section index. The
   output path is derived from `Source`.
3. `make docs-html-regen`.
4. Add the URL to `docs/sitemap.xml` with an honest `lastmod`. The test will
   tell you if you forget.
5. If it is a page an agent should find, add it to `llms.txt` and
   `llms-full.txt` and move their `Updated:` stamps.
