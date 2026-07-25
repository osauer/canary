---
paths:
  - "internal/app/**"
  - "internal/cli/**"
  - "internal/config/**"
  - "internal/dial/**"
  - "internal/update/**"
  - "pkg/ibkr/**"
  - "scripts/docgen/config-ref/**"
---

# Adding or removing IBKR_* env vars

Every production read of an `IBKR_*` environment variable must be flagged with a `// docgen:env NAME | description` comment next to the read or its named constant. `scripts/docgen/config-ref` AST-checks literal/constant `os.Getenv` and `os.LookupEnv` calls against those comments, then emits `docs/docs/reference/config.md`; `make check` fails for an undocumented read or generated-doc drift. New env var → add the read, add the comment, run `make docs-regen`, and commit the source plus generated references together.
