# Documentation drafts

Empty as of 2026-07-25: every planned page has been written, so the handbook is
32 published pages with nothing outstanding. Keep this file, and the directory,
for the next round.

Each file here is the brief for a handbook page that has been designed but not
written. The page appears on <https://osauer.dev/ibkr/docs/> with its scope and
a "not written yet" marker, and has no link and no URL until it exists.

Nothing in this directory is rendered, linked, or listed in the sitemap. It
sits outside `docs/`, which is the published site, so it is never served at all.

To publish one:

1. Write the real page as Markdown at `docs/docs/<section>/<page>.md`. That
   path is the published URL.
2. In `scripts/docgen/docs-html/main.go`, change that manifest entry from
   `statusPlanned` to `statusPublished`, replace `Draft` and `Page` with
   `Source`, and add `Description` plus a `Layout` if the page is long enough to
   want a table of contents.
3. Run `make docs-html-regen`, add the new URL to `docs/sitemap.xml`, and
   delete the brief. The generator writes a brand-new page's HTML without
   requiring it to be tracked first; `make docs-html-check` still demands
   both, because that is where an untracked output means a page that never
   reached the site.

The structure and the reasoning behind it are in
[`internal-docs/design/documentation-ia.md`](../design/documentation-ia.md).
