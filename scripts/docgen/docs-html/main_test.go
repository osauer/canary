package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestManifestCoversTrackedTwins(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(tracked); err != nil {
		t.Fatal(err)
	}
	if got, want := len(pages), 32; got != want {
		t.Fatalf("manifest has %d pages, want %d", got, want)
	}
}

// The landing page is hand-written with its own inline CSS, so the only thing
// stopping its navigation from drifting away from the generated pages is this
// test. It checks the shared items appear in order; the landing may add its
// own entries around them, such as the Install anchor.
func TestLandingPageCarriesSharedNav(t *testing.T) {
	landing, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	rest := string(landing)
	for _, item := range navItems {
		want := fmt.Sprintf("<a href=%q>%s</a>", item.Href, item.Label)
		index := strings.Index(rest, want)
		if index < 0 {
			t.Fatalf("docs/index.html is missing the shared nav entry %s, or has it out of order", want)
		}
		rest = rest[index+len(want):]
	}
}

// Every published page has to be in the sitemap and every retired URL has to be
// out of it. Without this the sitemap silently rots each time a page moves.
func TestSitemapMatchesPublishedPages(t *testing.T) {
	sitemap, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "sitemap.xml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(sitemap)
	want := []string{publicBaseURL + hubHref}
	var forbidden []string
	for _, page := range pages {
		if page.planned() {
			continue
		}
		want = append(want, publicBaseURL+strings.TrimPrefix(filepath.ToSlash(page.output()), "docs/"))
		for _, legacy := range page.Legacy {
			forbidden = append(forbidden, publicBaseURL+strings.TrimPrefix(filepath.ToSlash(legacy), "docs/"))
		}
	}
	for _, loc := range want {
		if !strings.Contains(body, "<loc>"+loc+"</loc>") {
			t.Errorf("docs/sitemap.xml is missing %s", loc)
		}
	}
	for _, loc := range forbidden {
		if strings.Contains(body, "<loc>"+loc+"</loc>") {
			t.Errorf("docs/sitemap.xml still lists the retired URL %s", loc)
		}
	}
}

// Markdown sits beside the HTML it produces, so a correct source link is
// already a correct site link. Only the extension swap for another generated
// page is real work; assets under docs/ pass through untouched and anything
// else tracked in the repository becomes a GitHub blob link.
func TestRewriteDestination(t *testing.T) {
	renderer := newSiteRenderer(repoRoot(t), map[string]bool{
		"SECURITY.md":                           true,
		"docs/docs/reference/config.md":         true,
		"docs/docs/start/updating.md":           true,
		"docs/diagrams/example.svg":             true,
		"internal-docs/design/internal-only.md": true,
	})
	page := pageSpec{Source: "docs/docs/start/updating.md"}
	cases := map[string]string{
		"../reference/config.md?view=full#limits":                "../reference/config.html?view=full#limits",
		"../../../SECURITY.md#release-integrity":                 "https://github.com/osauer/ibkr/blob/main/SECURITY.md#release-integrity",
		"../../../internal-docs/design/internal-only.md#details": "https://github.com/osauer/ibkr/blob/main/internal-docs/design/internal-only.md#details",
		"../../diagrams/example.svg":                             "../../diagrams/example.svg",
		"../../../LOCAL.md":                                      "../../../LOCAL.md",
		"#reference":                                             "#reference",
		"https://example.com/a.md#x":                             "https://example.com/a.md#x",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := string(renderer.rewriteDestination(page, []byte(input)))
			if got != want {
				t.Fatalf("rewriteDestination(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestRenderIsDeterministicAndGeneratorOwned(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	renderer := newSiteRenderer(root, tracked)
	page := pageForSource(t, "docs/docs/understand/concepts.md")
	first, err := renderer.render(page)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderer.render(page)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("render output is not deterministic")
	}
	text := string(first)
	if strings.Contains(text, "md-source:") {
		t.Fatal("generated output still contains a human-stamp marker")
	}
	if !strings.Contains(text, "Generated from Markdown by scripts/docgen/docs-html. DO NOT EDIT.") {
		t.Fatal("generated output lacks generator ownership notice")
	}
}

func TestConfigRuntimeSettingsRowsMatchRegistry(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := newSiteRenderer(root, tracked).render(pageForSource(t, "docs/docs/reference/config.md"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractRuntimeSettingsRows(string(rendered))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]settingsRow, 0, len(rpc.SettingsKeys()))
	for _, spec := range rpc.SettingsKeys() {
		key := spec.Key
		grammar := ""
		switch spec.Kind {
		case rpc.SettingsKindBool:
			grammar = "true/false/null"
		case rpc.SettingsKindFloat:
			grammar = "number/null"
		case rpc.SettingsKindInt:
			grammar = "integer/null"
		case rpc.SettingsKindDateMap:
			key += ".<SYMBOL>"
			grammar = "YYYY-MM-DD[Tamc/Tbmo]/null"
		default:
			t.Fatalf("unhandled settings kind %q", spec.Kind)
		}
		want = append(want, settingsRow{Key: key, Grammar: grammar, Class: spec.Class, Description: spec.Doc})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public runtime-settings table does not match rpc.SettingsKeys()\n got: %#v\nwant: %#v", got, want)
	}
}

type settingsRow struct {
	Key         string
	Grammar     string
	Class       string
	Description string
}

var (
	settingsRowPattern = regexp.MustCompile(`(?s)<tr>\s*<td><code>(.*?)</code></td>\s*<td><code>(.*?)</code></td>\s*<td>(.*?)</td>\s*<td>(.*?)</td>\s*</tr>`)
	htmlTagPattern     = regexp.MustCompile(`<[^>]+>`)
)

func extractRuntimeSettingsRows(document string) ([]settingsRow, error) {
	start := strings.Index(document, `<h2 id="runtime-platform-settings">`)
	end := strings.Index(document, `<h2 id="environment-variables">`)
	if start < 0 || end < 0 || end <= start {
		return nil, &sectionError{"runtime platform settings section is missing"}
	}
	section := document[start:end]
	matches := settingsRowPattern.FindAllStringSubmatch(section, -1)
	rows := make([]settingsRow, 0, len(matches))
	for _, match := range matches {
		rows = append(rows, settingsRow{
			Key:         normalizedCell(match[1]),
			Grammar:     normalizedCell(match[2]),
			Class:       normalizedCell(match[3]),
			Description: normalizedCell(match[4]),
		})
	}
	return rows, nil
}

func normalizedCell(value string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTagPattern.ReplaceAllString(value, "")))
}

type sectionError struct{ message string }

func (e *sectionError) Error() string { return e.message }

func pageForSource(t *testing.T, source string) pageSpec {
	t.Helper()
	for _, page := range pages {
		if page.Source == source {
			return page
		}
	}
	t.Fatalf("page %s is not in manifest", source)
	return pageSpec{}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
