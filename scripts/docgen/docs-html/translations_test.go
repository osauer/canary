package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseFrontMatter(t *testing.T) {
	fm, err := parseFrontMatter([]byte("---\ntranslation-of: docs/docs/start/install.md\nnav-title: 安裝: 與首次運行\n---\n# Title\n\nBody.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fm.fields["translation-of"]; got != "docs/docs/start/install.md" {
		t.Errorf("translation-of = %q", got)
	}
	// Only the first colon separates key and value.
	if got := fm.fields["nav-title"]; got != "安裝: 與首次運行" {
		t.Errorf("nav-title = %q", got)
	}
	if string(fm.body) != "# Title\n\nBody.\n" {
		t.Errorf("body = %q", fm.body)
	}

	for _, bad := range []string{
		"# No front matter\n",
		"---\nkey: value\n",
		"---\nno separator\n---\n",
		"---\nkey: a\nkey: b\n---\n",
	} {
		if _, err := parseFrontMatter([]byte(bad)); err == nil {
			t.Errorf("parseFrontMatter(%q) accepted", bad)
		}
	}
}

// The recorded revision is what `git hash-object` prints, so a translator can
// stamp a translation with git and the build recomputes it without git.
func TestGitBlobHash(t *testing.T) {
	if got := gitBlobHash([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("gitBlobHash = %s", got)
	}
}

func TestLoadTranslationStaleness(t *testing.T) {
	english := map[string]pageSpec{
		"docs/docs/start/install.md": {Source: "docs/docs/start/install.md", Section: "start", NavTitle: "Install", Summary: "s", Status: statusPublished},
	}
	source := []byte("# Install\n\nOriginal.\n")
	read := func(string) ([]byte, error) { return source, nil }
	header := "---\ntranslation-of: docs/docs/start/install.md\nsource-revision: %s\nnav-title: 安裝\nsummary: 摘要\ndescription: 描述\n---\n# 安裝 {#install}\n"

	current, err := loadTranslation(zhHant, "docs/docs/zh-hant/start/install.md", []byte(strings.Replace(header, "%s", gitBlobHash(source), 1)), english, read)
	if err != nil {
		t.Fatal(err)
	}
	if current.stale {
		t.Error("translation of the current source reported stale")
	}
	if current.output() != "docs/docs/zh-hant/start/install.html" {
		t.Errorf("output = %s", current.output())
	}

	behind, err := loadTranslation(zhHant, "docs/docs/zh-hant/start/install.md", []byte(strings.Replace(header, "%s", gitBlobHash([]byte("older\n")), 1)), english, read)
	if err != nil {
		t.Fatal(err)
	}
	if !behind.stale {
		t.Error("translation of an older source not reported stale")
	}

	for name, file := range map[string]string{
		"wrong directory": "docs/docs/zh-hant/operate/install.md",
		"wrong name":      "docs/docs/zh-hant/start/setup.md",
	} {
		if _, err := loadTranslation(zhHant, file, []byte(strings.Replace(header, "%s", gitBlobHash(source), 1)), english, read); err == nil {
			t.Errorf("%s: accepted %s", name, file)
		}
	}
	for name, broken := range map[string]string{
		"unknown source":  strings.Replace(strings.Replace(header, "%s", gitBlobHash(source), 1), "start/install.md", "start/missing.md", 1),
		"short revision":  strings.Replace(header, "%s", "abc123", 1),
		"missing summary": strings.Replace(strings.Replace(header, "%s", gitBlobHash(source), 1), "summary: 摘要\n", "", 1),
		"unknown key":     strings.Replace(strings.Replace(header, "%s", gitBlobHash(source), 1), "summary: 摘要\n", "summary: 摘要\nextra: x\n", 1),
	} {
		if _, err := loadTranslation(zhHant, "docs/docs/zh-hant/start/install.md", []byte(broken), english, read); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Markdown under docs/docs/ in a directory that is neither a section nor a
// registered language is a mistake, not a new language.
func TestDiscoverTranslationsRejectsUnknownLanguage(t *testing.T) {
	tracked := map[string]bool{"docs/docs/zh-hans/start/install.md": true}
	read := func(string) ([]byte, error) { return []byte("---\n---\n"), nil }
	if _, err := discoverTranslations(tracked, map[string]pageSpec{}, read); err == nil {
		t.Error("unregistered language directory accepted")
	}
}

// renderSite renders the tracked repository into a temporary directory and
// returns its root. Everything the translations change is checked against
// the real tree, so a test here fails the same way the Pages build would.
func renderSite(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if _, err := run(root, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readSite(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var headingIDs = regexp.MustCompile(`<h[1-6] id="([^"]*)"`)

func TestTranslatedPagesRender(t *testing.T) {
	site := renderSite(t)
	zh := readSite(t, site, "docs/zh-hant/start/ibkr-hong-kong-singapore.html")
	en := readSite(t, site, "docs/start/ibkr-hong-kong-singapore.html")

	for _, want := range []string{
		`<html lang="zh-Hant">`,
		`<link rel="alternate" hreflang="en" href="https://osauer.dev/canary/docs/start/ibkr-hong-kong-singapore.html">`,
		`<link rel="alternate" hreflang="zh-Hant" href="https://osauer.dev/canary/docs/zh-hant/start/ibkr-hong-kong-singapore.html">`,
		`<link rel="alternate" hreflang="x-default" href="https://osauer.dev/canary/docs/start/ibkr-hong-kong-singapore.html">`,
		`<link rel="canonical" href="https://osauer.dev/canary/docs/zh-hant/start/ibkr-hong-kong-singapore.html">`,
		`<script src="../../../lang.js"></script>`,
		`class="translation-note"`,
		`href="../../start/ibkr-hong-kong-singapore.html" hreflang="en" data-lang="en"`,
		`"inLanguage":"zh-Hant"`,
		`"translationOfWork"`,
		// Links written against the English source reach the translated
		// neighbour where one exists and the English page otherwise.
		`<a href="install.html">`,
		`<a href="../../understand/market-data.html">`,
	} {
		if !strings.Contains(zh, want) {
			t.Errorf("translated page lacks %s", want)
		}
	}
	for _, want := range []string{
		`<html lang="en">`,
		`<link rel="alternate" hreflang="zh-Hant" href="https://osauer.dev/canary/docs/zh-hant/start/ibkr-hong-kong-singapore.html">`,
		`<a href="../zh-hant/start/ibkr-hong-kong-singapore.html" hreflang="zh-Hant" lang="zh-Hant" data-lang="zh-Hant">繁體中文</a>`,
	} {
		if !strings.Contains(en, want) {
			t.Errorf("English page lacks %s", want)
		}
	}
	if strings.Contains(en, "translation-note") {
		t.Error("English page carries a translation note")
	}

	// A translation keeps the English anchors, so every #fragment link into
	// the page works in both languages.
	enIDs := headingIDs.FindAllStringSubmatch(en, -1)
	zhIDs := headingIDs.FindAllStringSubmatch(zh, -1)
	if len(enIDs) != len(zhIDs) {
		t.Fatalf("heading count differs: en %d, zh %d", len(enIDs), len(zhIDs))
	}
	for i := range enIDs {
		if enIDs[i][1] != zhIDs[i][1] {
			t.Errorf("heading %d: en id %q, zh id %q", i, enIDs[i][1], zhIDs[i][1])
		}
	}

	// A page with no translation still offers the selector, pointing at the
	// translated hub, and carries no hreflang set. The generated CLI
	// reference is rebuilt from code and will never carry a translation.
	cli := readSite(t, site, "docs/reference/cli.html")
	if strings.Contains(cli, `hreflang="x-default"`) {
		t.Error("untranslated page declares alternates")
	}
	if !strings.Contains(cli, `<a href="../zh-hant/" hreflang="zh-Hant" lang="zh-Hant" data-lang="zh-Hant">繁體中文</a>`) {
		t.Error("untranslated page lacks the selector link to the translated hub")
	}
}

// The notice under a translated title is the reader-facing half of the
// stale check: a translation behind its source says so, in its language.
func TestTranslationNoteHTML(t *testing.T) {
	current, err := translationNoteHTML(&translation{lang: zhHant}, "docs/docs/zh-hant/start/install.html", "docs/docs/start/install.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current, `class="translation-note"`) || !strings.Contains(current, zhHant.ui.Translated) || !strings.Contains(current, `href="../../start/install.html" hreflang="en" data-lang="en"`) {
		t.Errorf("current note = %s", current)
	}
	stale, err := translationNoteHTML(&translation{lang: zhHant, stale: true}, "docs/docs/zh-hant/start/install.html", "docs/docs/start/install.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stale, `class="translation-note is-stale"`) || !strings.Contains(stale, zhHant.ui.Stale) {
		t.Errorf("stale note = %s", stale)
	}
}

func TestTranslatedHubRenders(t *testing.T) {
	site := renderSite(t)
	hub := readSite(t, site, "docs/zh-hant/index.html")
	for _, want := range []string{
		`<html lang="zh-Hant">`,
		`<h1>說明文件</h1>`,
		`<link rel="alternate" hreflang="en" href="https://osauer.dev/canary/docs/">`,
		`<link rel="alternate" hreflang="zh-Hant" href="https://osauer.dev/canary/docs/zh-hant/">`,
		// Translated pages under their own title, English-only pages marked.
		`<a href="start/ibkr-hong-kong-singapore.html"><span class="hub-title">香港及新加坡帳戶</span>`,
		`<a href="../reference/cli.html"><span class="hub-title">CLI<span class="hub-lang" lang="en">EN</span></span>`,
	} {
		if !strings.Contains(hub, want) {
			t.Errorf("translated hub lacks %s", want)
		}
	}
	english := readSite(t, site, "docs/index.html")
	if strings.Contains(english, "hub-lang") {
		t.Error("English hub marks pages as English")
	}
}
