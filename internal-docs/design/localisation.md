# Localisation

Updated: 2026-11-04 12:40 CET

What it takes to let Canary itself speak a second language: the CLI's
human-readable output, `--help`, error text, the paired app and the MCP
server. The documentation site already renders translations (the first
Traditional Chinese pages shipped with this note; the mechanism is in
`scripts/docgen/docs-html/translations.go`). This note is an assessment, not
a build: it sizes the job, names the design that fits the code as it stands,
and ends with the decisions that are the owner's.

## What is already true

- English is the master everywhere. Documentation translations record the git
  blob of the English source they were made from, and a translation behind
  its source renders with a notice and is listed by
  `make docs-translations-check`.
- The paired app formats money and dates through `Intl` with the browser's
  locale (`web/app/shared.js`), so numbers already follow the reader. Its
  labels, headings and status words are hard-coded English in `index.html`
  (about 210 text nodes) and in template literals across the thirty
  JavaScript modules (about 150 `textContent` assignments, plus the strings
  built inside them).
- The CLI renders through hand-written formatters in `internal/cli` (77
  non-test files, 22,000 lines). Human-readable output is a product contract
  pinned by 132 golden cases in `internal/cli/testdata/golden`.
- The MCP registry (`internal/mcp/tools.go`) carries 44 tool descriptions,
  about 45 KB of English prose written for a model.
- There is no message catalogue and no `golang.org/x/text` dependency in the
  product module. The docs generator's module carries OpenCC for the
  Simplified derivation; the product module does not. `scripts/check-go-dependencies.sh` keeps that module
  minimal, so any new dependency needs the one-sentence justification the
  engineering rules ask for.

## Which Chinese

Author in Traditional Chinese with Hong Kong usage (帳戶, 私隱, 遙距, 甚麼,
港交所, 新交所, 滬深港通) and derive Simplified from it. The reason is the
conversion direction, not the reader count: Traditional to Simplified is
almost one-to-one, Simplified to Traditional is not (发 is 發 or 髮, 后 is
後 or 后, 干 is 乾, 幹 or 干). Hong Kong and mainland vocabulary are also
closer to each other in software terms (軟件, 數據) than either is to
Taiwan's (軟體, 資料), so one glossary covers the derived variant:

| Traditional, Hong Kong | Simplified, Singapore and mainland | English |
| --- | --- | --- |
| 持倉 | 持仓 | positions |
| 淨清算價值 | 净清算价值 | net liquidation value |
| 未實現盈虧 | 未实现盈亏 | unrealised P&L |
| 基礎貨幣 | 基础货币 | base currency |
| 期權鏈 | 期权链 | option chain |
| 保證金 | 保证金 | margin |
| 追蹤止損 | 追踪止损 | trailing stop |
| 結單 | 结单 | statement (IBKR uses 结单 in Simplified too) |
| 沽空 | 卖空 | short selling (vocabulary differs) |
| 業績 | 财报 | earnings (vocabulary differs) |

The derivation is a build step in the docs generator
(`scripts/docgen/docs-html/simplified.go`): the glossary above and its
software rows are applied to the Traditional Markdown first, then OpenCC's
`hk2s` profile converts the script. OpenCC is the pure-Go port
`github.com/longbridgeapp/opencc` v0.3.13, pinned in the generator's own
`go.mod` with its dictionaries embedded, so `make check` needs no binary on
PATH. Simplified pages are build output, never tracked; a tracked file under
`docs/docs/zh-hans/` fails the build. They carry the Traditional page's
English revision, so the stale gate judges both by the same source. The
glossary is machine-written like the pages and needs the same native read;
a Singapore reader is the one to judge its mainland-leaning choices (开盘
for 開市, 财报 for 業績, 做市商 for 莊家). Owner decision 2026-11-04 12:34
CET: ship Simplified from the first publication.

## Where the language setting lives

One setting, `display.language`, beside `display.date_format` in the
daemon's platform settings (`internal/rpc/settings.go` registry, owner
class, `internal/daemon/platform_settings.go` validation and storage). Values
are closed: `en`, `zh-Hant`, later `zh-Hans`; `null` clears the override.
Every adapter reads it from the daemon:

- The app sets `<html lang>` and its `Intl` locale from it and renders its
  catalogue in it. A reader changes it on the Settings screen, under
  Display, as they change the date format today.
- The CLI asks the daemon's settings on each run, which it already does for
  the trading status it prints. Output written before the daemon answers,
  which is the `OFFLINE` and `STARTING` paths of `canary status` and the
  `daemon socket did not appear` error, stays English: those lines are read
  when nothing else works, and the English text is what the troubleshooting
  page quotes.
- No environment variable and no `config.toml` key. `LANG` and `LC_ALL` are
  the machine's locale, not the owner's choice for this desk, and a second
  source means two answers to one question. The engineering rules say a knob
  means the default was chosen wrong; the default here is English, chosen
  once, and the setting is the one place it changes.

## Message catalogue

A plain catalogue, not `golang.org/x/text/message`:

- `internal/i18n` with one embedded file per language
  (`catalog/zh-Hant.json`), keyed by the English string. `i18n.T(lang, "Net
  liquidation")` returns the translation or the key. A missing key is a
  test failure, not a runtime fallback: a gate walks every catalogue and
  fails on a key the code no longer uses or a used key a language lacks.
- `x/text/message` would bring plural rules, number and currency formatting
  and the `gotext` extraction tool. Canary formats money and dates itself,
  by the broker's conventions (`HKD 1,842,310.55`, `¥ 15,000,000`), and must
  keep doing so: the figures are part of the golden contract and a locale
  formatter would change them. Plural handling is one `if` in the few
  places it occurs. The extraction tool wants `message.Printf` at every call
  site, which is the same edit as `i18n.T` with a dependency on top. A JSON
  catalogue is also what a native reader can review without a Go toolchain.

Column width is the hidden cost. Twenty-five renderer files pad with
`%-Ns`, which counts bytes, and the alignment helpers count runes. A Chinese
label is two terminal cells per rune, so every table misaligns the moment a
header is translated. The fix is one width-aware padder used by every
renderer, measuring East Asian width (a small dependency,
`golang.org/x/text/width` or `go-runewidth`, or a 200-line table of the
ranges that matter), landed before the first translated label. Terminal
fonts also vary in whether they render ambiguous-width characters as one
cell or two; the renderers should treat them as two and accept a ragged
edge on fonts that disagree.

## Golden render tests per language

`TestRenderGolden` runs each case once in English. Per language it runs the
same cases with the catalogue applied and compares to
`testdata/golden/<lang>/<case>.txt`; `-update` rewrites every language from
the current renderer output. The synthetic inputs stay shared, so a change to
what a renderer prints shows up once per language in the same diff, and a
language whose golden was not reviewed fails rather than drifts. The English
goldens stay byte-identical to today's, which is the proof that adding a
language changed nothing for the English reader.

Width-sensitive goldens (the tabular renderers) need the padder first, or
every Chinese golden is a picture of misalignment that a reviewer then has
to re-approve after the fix.

## What stays English

- Identifiers the reader types or greps: command and flag names, config
  keys and section names, settings keys, JSON field names, enum values and
  status badges (`READY`, `delayed`, `port_rejecting`), file and socket
  paths, environment variables.
- Log lines in `ibkr-daemon.log`: a diagnostic stream read in bug reports,
  by the maintainer, in one language.
- Error text that travels over RPC from the daemon: the CLI maps the error
  codes it already recognises (`gateway_unavailable`, the `start:` integrity
  failures) to a translated hint and prints the daemon's English text under
  it, so a redacted `canary feedback` report quotes what the daemon said.
- The MCP server end to end. Tool descriptions and parameter docs are read
  by a model, which answers the user in whatever language the user wrote;
  results are typed JSON whose keys are the contract. Translating the
  descriptions would make a worse prompt and the results need no labels
  that a host model cannot supply itself. The one MCP touchpoint is
  `canary_settings`, which reports `display.language` like any other key.
- `canary --help`: translated. It is the reader's first contact with the
  CLI and reads as prose. The usage lines (`canary brief [--details]`) stay
  as they are; the summary sentences come from the catalogue.

## Size of the job

| Surface | Strings | Files | Note |
| --- | --- | --- | --- |
| CLI renderers | about 580 literals of twelve characters or more, 130 error constructions | 77 in `internal/cli` | Plus the width padder and 132 goldens per language |
| `--help` | 44 command summaries and their flag docs | `internal/cli/cli.go`, `catalog.go` | Generated `docs/reference/cli.md` stays English |
| Paired app | about 210 static text nodes, about 150 built in JavaScript | `web/app/index.html` and 30 modules | A `t()` helper and one catalogue file; the app has no string table today |
| Daemon setting | one key, one kind | `internal/rpc/settings.go`, `internal/daemon/platform_settings.go` | Same path as `display.date_format` |
| MCP | 0 | | Stays English by design |

In order: the setting and the app's Settings screen (small, proves the
path end to end), then the width padder with English goldens unchanged,
then the catalogue with the brief and status renderers first since they are
what a reader meets every day, then `--help`, then the rest of the app.
Each step leaves the English product byte-identical, which the goldens and
`make app-check` prove.

## Decisions for the owner

1. Decided 2026-11-04 12:34 CET: Traditional Chinese in Hong Kong usage is
   the authored master, Simplified is generated from it and ships with it.
2. `display.language` as the one source for the app and the CLI, English
   before the daemon answers: confirm.
3. MCP stays English end to end: confirm.
4. The width padder's dependency: `golang.org/x/text/width`, `go-runewidth`,
   or a hand-kept table. Each is a product-module dependency decision.
5. Whether a documentation translation that is behind its English source
   should fail `make check`. Today it is listed and rendered with a notice;
   failing would make every English edit wait for a native reader.
6. Who reads the Chinese: every translated page is machine-translated by a
   model and needs a native reader's cold read before it is published.
