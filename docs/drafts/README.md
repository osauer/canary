# Documentation drafts

Each file here is the brief for a handbook page that has been designed but not
written. The page appears on <https://osauer.dev/ibkr/docs/> with its scope and
a "not written yet" marker, and has no link and no URL until it exists.

Nothing in this directory is rendered, linked, or listed in the sitemap, and
`robots.txt` keeps it out of search results.

To publish one:

1. Write the real page as Markdown under `docs/`, in the directory that matches
   its subject rather than its URL. Source layout and public URL are decoupled
   on purpose.
2. In `scripts/docgen/docs-html/main.go`, change that manifest entry from
   `statusPlanned` to `statusPublished`, drop `Draft`, and add `Source`,
   `Description`, and a `Layout` if the page is long enough to want a table of
   contents.
3. Run `make docs-html-regen`, add the new URL to `docs/sitemap.xml`, and
   delete the brief.

The structure and the reasoning behind it are in
[`docs/design/documentation-ia.md`](../design/documentation-ia.md).
