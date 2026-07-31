# Historical upgrade fixtures

`internal/daemon/testdata/upgrades` contains immutable, synthetic artifacts
created by the real writers from the pinned `v1.7.1`, `v2.2.1`, `v2.3.0`, and `v2.5.4`
source tags. The v2.5.4 artifact isolates the already-current schema-v3 to
maintenance-only schema-v4 boundary.
Normal `go test` runs consume those committed files. They do not check out,
compile, or execute historical code and do not require network access.

Maintainers may intentionally rebuild the artifacts with:

```sh
scripts/upgrade-fixtures/refresh.sh
```

The refresh command verifies each tag's peeled commit, expands that commit into
an isolated temporary source tree, copies in a tag-compatible generator, and
runs only there. It never adds files to or builds inside the maintainer's
working tree. The v2.3.0 generator fixes both its clock and entropy so its
SQLite bytes are reproducible.

The generated manifest records the tag, peeled commit, generator, each file's
SHA-256 and private source/install modes, a digest over the complete artifact,
synthetic expected state, and whether current code must migrate it or fail
closed. Updating a tag pin, generator, fixture, expectation, or classification
is therefore a reviewable source change.
