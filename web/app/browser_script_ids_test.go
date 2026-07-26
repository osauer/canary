package appweb

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestBrowserScriptIDsMatchSPA is the static drift gate between the
// Playwright app scripts and the SPA DOM they assert against.
//
// Commit 0574bd3 (2026-06-09) removed the mitigation and order-review
// controls from index.html while scripts/app-browser-smoke.mjs kept asserting
// them: the browser smoke sat red for two days, was misdiagnosed as
// live-session flakiness, and v1.9.0 shipped anyway — the browser smokes run
// outside the check/test/release chains. This test closes the gap with
// no browser and no running app: every element id a browser script
// references must exist in index.html (or be created by the SPA modules through a
// statically visible literal), and every id the smoke deliberately asserts
// as REMOVED must stay gone from the SPA sources.

// browserScriptFiles are the Playwright scripts that hardcode SPA element
// ids. lib-app-browser.mjs references none today but is scanned so an id
// sneaking into the shared helpers is gated too.
var browserScriptFiles = []string{
	"app-browser-smoke.mjs",
	"app-screenshots.mjs",
	"lib-app-browser.mjs",
}

// removedSPAIDs are element ids the browser smoke asserts are ABSENT from
// the rendered DOM (removed product surfaces). Each entry is drift-guarded
// in both directions: the id must still be referenced by a browser script
// (else the entry is stale and must be deleted), and it must not reappear in
// index.html or the SPA modules (else the smoke's absence assert breaks at runtime).
// Values name the asserting function in scripts/app-browser-smoke.mjs.
var removedSPAIDs = map[string]string{
	"stressWarningsToggle":      "exerciseStressControlsRemoved",
	"stressChecksToggle":        "exerciseStressControlsRemoved",
	"stressInlineDetailPanel":   "exerciseStressControlsRemoved",
	"stressMitigationButton":    "exerciseStressControlsRemoved",
	"orderReviewPanel":          "exerciseStressControlsRemoved",
	"quickRiskPlanButton":       "exerciseStressControlsRemoved",
	"quickReviewBlockersButton": "exerciseStressControlsRemoved",
	"quickHeldActionsButton":    "exerciseStressControlsRemoved",
	"quickAlertsButton":         "exerciseStressControlsRemoved",
	"accountMenu":               "exerciseAccountPanel (account dropdown removed)",
	"accountMenuToggle":         "exerciseAccountPanel (account dropdown removed)",
	"marketPanel":               "exerciseMarketContext (old Market Context panel removed)",
	"toolsPanel":                "assertDebugToolsRemoved",
}

func TestBrowserScriptIDsMatchSPA(t *testing.T) {
	t.Parallel()
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	staticIDs := htmlElementIDs(string(htmlData))
	createdIDs := appJSCreatedIDs(embeddedSPASource(t))

	// id -> browser script files referencing it.
	referenced := map[string][]string{}
	for _, name := range browserScriptFiles {
		path := filepath.Join("..", "..", "scripts", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (moved or renamed? update browserScriptFiles): %v", path, err)
		}
		for id := range scriptElementIDs(string(data)) {
			referenced[id] = append(referenced[id], name)
		}
	}

	// Extraction tripwires: these ids are load-bearing in the smoke (it
	// cannot run without waiting on them), so their absence here means the
	// extraction broke, not that the scripts stopped using ids.
	for _, id := range []string{"dashboard", "connectionLine", "netLiquidation"} {
		if len(referenced[id]) == 0 {
			t.Fatalf("id extraction broke: %q is not among the ids referenced by %v", id, browserScriptFiles)
		}
	}
	if !staticIDs["dashboard"] {
		t.Fatalf("id extraction broke: index.html inventory misses id=%q", "dashboard")
	}

	for _, id := range slices.Sorted(maps.Keys(removedSPAIDs)) {
		assertedIn := removedSPAIDs[id]
		if len(referenced[id]) == 0 {
			t.Errorf("removedSPAIDs[%q] is stale: no browser script references it anymore (absence was asserted in %s); delete the entry", id, assertedIn)
		}
		if staticIDs[id] {
			t.Errorf("index.html re-adds id=%q, but %s asserts the surface stays removed; drop the element, or delete the smoke assert and this removedSPAIDs entry together", id, assertedIn)
		}
		if createdIDs[id] {
			t.Errorf("SPA modules create id=%q, but %s asserts the surface stays removed; drop the creation, or delete the smoke assert and this removedSPAIDs entry together", id, assertedIn)
		}
	}

	for _, id := range slices.Sorted(maps.Keys(referenced)) {
		if _, removed := removedSPAIDs[id]; removed {
			continue
		}
		if staticIDs[id] || createdIDs[id] {
			continue
		}
		t.Errorf("#%s is asserted by %s but index.html does not declare it and the SPA modules do not create it via a literal id pattern; removing an SPA surface must update the script assertions in the same change (for a deliberate absence assert, add the id to removedSPAIDs in this test; for a dynamically built element, give it a literal id= so this gate can see it)",
			id, strings.Join(referenced[id], ", "))
	}
}

// htmlIDAttrRE matches id attributes preceded by whitespace or a tag open,
// which keeps data-id= and similar suffixed attributes out.
var htmlIDAttrRE = regexp.MustCompile(`(?:^|[\s<])id=["']([^"']+)["']`)

func htmlElementIDs(html string) map[string]bool {
	ids := map[string]bool{}
	for _, m := range htmlIDAttrRE.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}
	return ids
}

// appJSCreatedIDs collects element ids app.js can mint at runtime, as far
// as that is statically visible: id attributes inside markup template
// strings, `.id = "..."` property assignments, and setAttribute("id", ...).
// Ids assembled from interpolation are invisible here — the gate's failure
// message tells the author to use a literal id instead.
var appJSCreateIDPatterns = []*regexp.Regexp{
	htmlIDAttrRE,
	regexp.MustCompile(`\.id\s*=\s*["'` + "`" + `]([A-Za-z_][A-Za-z0-9_-]*)["'` + "`" + `]`),
	regexp.MustCompile(`setAttribute\(\s*["']id["']\s*,\s*["'` + "`" + `]([A-Za-z_][A-Za-z0-9_-]*)["'` + "`" + `]`),
}

func appJSCreatedIDs(js string) map[string]bool {
	ids := map[string]bool{}
	for _, re := range appJSCreateIDPatterns {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			ids[m[1]] = true
		}
	}
	return ids
}

var (
	getElementByIDRE = regexp.MustCompile(`getElementById\(\s*["'` + "`" + `]([A-Za-z_][A-Za-z0-9_-]*)["'` + "`" + `]`)
	hashIDTokenRE    = regexp.MustCompile(`#([A-Za-z_][A-Za-z0-9_-]*)`)
	hexColorShapeRE  = regexp.MustCompile(`^(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
)

// scriptElementIDs extracts the element ids a browser script references:
// getElementById literals from the raw source (commented-out asserts still
// count — delete them instead), plus #id tokens from string literals
// (selectors for querySelector/locator/waitForSelector), with comments
// skipped so prose like docs/foo.md#anchor cannot register. Tokens shaped
// like 3/6-digit hex colors are dropped in case a script ever inlines CSS.
func scriptElementIDs(src string) map[string]bool {
	ids := map[string]bool{}
	for _, m := range getElementByIDRE.FindAllStringSubmatch(src, -1) {
		ids[m[1]] = true
	}
	for _, lit := range jsStringLiterals(src) {
		for _, m := range hashIDTokenRE.FindAllStringSubmatch(lit, -1) {
			if hexColorShapeRE.MatchString(m[1]) {
				continue
			}
			ids[m[1]] = true
		}
	}
	return ids
}

// jsStringLiterals returns the contents of '…', "…", and `…` literals,
// skipping // and /* */ comments so an apostrophe in prose cannot open a
// phantom string. It does not interpret ${} interpolation (treated as
// literal text) or JS regex literals — fine for the scripts gated here,
// and a mis-lex fails loud (bogus id) rather than passing silently.
func jsStringLiterals(src string) []string {
	var out []string
	for i := 0; i < len(src); i++ {
		switch c := src[i]; {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += 2 + end + 1
		case c == '"' || c == '\'' || c == '`':
			var b strings.Builder
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				b.WriteByte(src[j])
				j++
			}
			out = append(out, b.String())
			i = j
		}
	}
	return out
}
