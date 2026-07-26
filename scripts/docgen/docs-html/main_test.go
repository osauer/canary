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

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestManifestCoversTrackedTwins(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(tracked, true); err != nil {
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
		"../../../SECURITY.md#release-integrity":                 "https://github.com/osauer/canary/blob/main/SECURITY.md#release-integrity",
		"../../../internal-docs/design/internal-only.md#details": "https://github.com/osauer/canary/blob/main/internal-docs/design/internal-only.md#details",
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

// The navigation tree is generated once per page at a different directory
// depth, so every one of its links is a fresh relative path. This renders every
// page the site publishes and checks the tree on each: exactly one current
// item, every link resolving to a file that exists from that page's directory,
// and no planned page carrying a link.
func TestSideNavTree(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	renderer := newSiteRenderer(root, tracked)

	published, planned := 0, 0
	for _, page := range pages {
		if page.planned() {
			planned++
			continue
		}
		published++
	}

	rendered := map[string][]byte{}
	for _, page := range pages {
		if page.planned() {
			continue
		}
		document, err := renderer.render(page)
		if err != nil {
			t.Fatal(err)
		}
		rendered[filepath.ToSlash(page.output())] = document
	}
	hub, err := renderer.renderHub()
	if err != nil {
		t.Fatal(err)
	}
	rendered[hubOutput] = hub

	for output, document := range rendered {
		t.Run(output, func(t *testing.T) {
			tree, err := sideNavSection(string(document))
			if err != nil {
				t.Fatal(err)
			}

			if got := strings.Count(tree, `aria-current="page"`); got != 1 {
				t.Errorf("tree marks %d current items, want exactly 1", got)
			}

			// Overview plus every published page, and nothing else.
			hrefs := hrefPattern.FindAllStringSubmatch(tree, -1)
			if got, want := len(hrefs), published+1; got != want {
				t.Errorf("tree has %d links, want %d", got, want)
			}
			for _, href := range hrefs {
				target := filepath.Join(root, filepath.Dir(output), filepath.FromSlash(href[1]))
				if _, err := os.Stat(target); err != nil {
					t.Errorf("link %q does not resolve from %s: %v", href[1], output, err)
				}
			}

			if got := strings.Count(tree, `class="sidenav-planned"`); got != planned {
				t.Errorf("tree shows %d planned entries, want %d", got, planned)
			}
			for _, page := range pages {
				if !page.planned() {
					continue
				}
				label := html.EscapeString(page.NavTitle)
				if !strings.Contains(tree, `<span class="sidenav-planned">`+label+`</span>`) {
					t.Errorf("planned page %q is missing from the tree", page.NavTitle)
				}
				if strings.Contains(tree, filepath.Base(page.output())) {
					t.Errorf("planned page %q is linked; it has no published URL", page.NavTitle)
				}
			}

			for _, section := range sections {
				if !strings.Contains(tree, `<summary class="sidenav-heading">`+html.EscapeString(section.Title)+`</summary>`) {
					t.Errorf("tree is missing section %q", section.Title)
				}
			}

			// The section being read is the one that opens. The handbook
			// index belongs to no section, so it opens none.
			want := 1
			if output == hubOutput {
				want = 0
			}
			if got := strings.Count(tree, `<details class="sidenav-group" open>`); got != want {
				t.Errorf("tree opens %d sections, want %d", got, want)
			}
		})
	}
}

// The current page has to be the page being rendered, not merely some page.
func TestSideNavMarksThePageItRenders(t *testing.T) {
	root := repoRoot(t)
	tracked, err := trackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	renderer := newSiteRenderer(root, tracked)

	page := pageForSource(t, "docs/docs/understand/sensors.md")
	document, err := renderer.render(page)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sideNavSection(string(document))
	if err != nil {
		t.Fatal(err)
	}
	want := `<li><a href="sensors.html" class="is-current" aria-current="page">` + page.NavTitle + `</a></li>`
	if !strings.Contains(tree, want) {
		t.Errorf("tree does not mark the rendered page current; want %s", want)
	}
	// A sibling section resolves upward out of understand/.
	if !strings.Contains(tree, `<a href="../internals/architecture.html">Architecture</a>`) {
		t.Error("tree does not reach another section with a correct relative path")
	}

	hub, err := renderer.renderHub()
	if err != nil {
		t.Fatal(err)
	}
	tree, err = sideNavSection(string(hub))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tree, `<a class="sidenav-overview is-current" href="../docs/" aria-current="page">Overview</a>`) {
		t.Error("the handbook index does not mark its own tree entry current")
	}
}

var hrefPattern = regexp.MustCompile(`href="([^"]+)"`)

// sideNavSection isolates the tree so an assertion cannot be satisfied by the
// site header, the per-page section index, or a link in the prose.
func sideNavSection(document string) (string, error) {
	start := strings.Index(document, `<details class="sidenav">`)
	if start < 0 {
		return "", &sectionError{"page carries no navigation tree"}
	}
	// The tree nests a <details> per section, so it ends where the article
	// begins rather than at the first closing tag.
	end := strings.Index(document[start:], `<main class="doc">`)
	if end < 0 {
		return "", &sectionError{"navigation tree is not followed by the article"}
	}
	return document[start : start+end], nil
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
