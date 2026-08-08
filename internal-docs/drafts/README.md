# Documentation drafts

Empty as of 2026-07-25: every planned page has been written, so the handbook is
32 published pages with nothing outstanding. Keep this file, and the directory,
for the next round.

Each file here is the brief for a handbook page that has been designed but not
written. The page appears on <https://osauer.dev/canary/docs/> with its scope and
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
3. Run `make pages-build`, add the new URL to `docs/sitemap.xml`, inspect the
   generated artifact under `dist/pages`, and delete the brief. The generator
   refuses tracked HTML twins; `make docs-html-check` builds and validates the
   exact artifact that Pages deploys.

The structure and the reasoning behind it are in
[`internal-docs/design/documentation-ia.md`](../design/documentation-ia.md).
