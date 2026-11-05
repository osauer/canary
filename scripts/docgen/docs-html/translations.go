package main

// The language layer. English is the master: every page is written in
// English first, and a translation is a second Markdown file that mirrors the
// English path under docs/docs/<slug>/ and records which revision of the
// English source it was made from. The generator discovers translations from
// the tracked tree, so adding a language is one entry in languages, one
// directory, and no manifest edit.

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

// langSpec is one language the site renders. Slug is the path segment under
// docs/docs/ (empty for English, whose pages sit at the root); Tag is the BCP
// 47 tag written into lang, hreflang and JSON-LD; Name is the language's own
// name, which is how a reader who does not read the current page's language
// finds the selector.
type langSpec struct {
	Tag  string
	Slug string
	Name string
	// OGLocale is the page's language in Open Graph's language_TERRITORY form.
	OGLocale string
	// SocialImage is the site-root relative link preview for pages without a
	// diagram of their own, with its words in this language.
	SocialImage string
	ui          uiStrings
	// derivedFrom names the language this one is generated from at build
	// time; its directory under docs/docs/ is never tracked.
	derivedFrom *langSpec
}

// uiStrings is the site chrome in one language: everything a generated page
// says that is not the Markdown body.
type uiStrings struct {
	// Documentation labels the hub and the sidenav; Overview is the hub link
	// inside the sidenav tree.
	Documentation string
	Overview      string
	OnThisPage    string
	NotWrittenYet string
	DimmedNote    string
	// HubLead opens the hub; HubDescription is its meta description.
	HubLead        string
	HubDescription string
	// Sections keys section slug to its title and blurb on the hub.
	Sections map[string]sectionSpec
	// Nav holds the header labels in navItems order.
	Nav []string
	// Footer labels and the fineprint under them.
	FooterDocs     string
	FooterPrivacy  string
	FooterSecurity string
	Fineprint      string
	// EnglishOnly marks, on a translated hub or sidenav, a page that exists
	// only in English; the link then leads to the English page.
	EnglishOnly string
	// Translated and Stale are the notes at the top of a translated page; the
	// link text follows. Stale is used when the English source moved on after
	// the translation was made.
	Translated  string
	Stale       string
	ReadEnglish string
}

var english = &langSpec{
	Tag:         "en",
	Slug:        "",
	Name:        "English",
	OGLocale:    "en_GB",
	SocialImage: "social/social-preview.png",
	ui: uiStrings{
		Documentation:  "Documentation",
		Overview:       "Overview",
		OnThisPage:     "On this page",
		NotWrittenYet:  "Not written yet",
		DimmedNote:     "Dimmed entries are not written yet.",
		HubLead:        "How to install, operate, and understand Canary against your own Interactive Brokers account. Start at the top if Canary is new to you; later sections move closer to the code.",
		HubDescription: hubDescription,
		Nav:            []string{"Documentation", "MCP tools", "Remote access", "Feedback", "GitHub"},
		FooterDocs:     "Documentation",
		FooterPrivacy:  "Privacy",
		FooterSecurity: "Security",
		Fineprint:      "Not financial advice. Canary is analysis software; nothing here is a recommendation to buy or sell any security.",
	},
}

// zhHant is Traditional Chinese in Hong Kong usage (帳戶, 私隱, 遙距, 甚麼).
// Simplified Chinese is derived from it, not written beside it: the
// Traditional-to-Simplified mapping is almost one-to-one, the reverse is not.
var zhHant = &langSpec{
	Tag:         "zh-Hant",
	Slug:        "zh-hant",
	Name:        "繁體中文",
	OGLocale:    "zh_HK",
	SocialImage: "social/social-preview-zh-hant.png",
	ui: uiStrings{
		Documentation:  "說明文件",
		Overview:       "總覽",
		OnThisPage:     "本頁內容",
		NotWrittenYet:  "尚未撰寫",
		DimmedNote:     "灰色項目尚未撰寫。",
		HubLead:        "如何安裝、操作及理解 Canary，並連接你自己的 Interactive Brokers 帳戶。初次接觸 Canary 請由頂部開始；後面的章節逐步貼近程式碼。尚未翻譯的頁面以英文顯示，並標示 EN。",
		HubDescription: "Canary 說明文件：安裝與首次運行、每日交易台流程、如何解讀每項量度、由程式碼產生的指令與工具參考，以及底層的系統設計。",
		Sections: map[string]sectionSpec{
			"start":      {Title: "開始使用", Blurb: "安裝 Canary，連接閘道與 MCP 主機，並確認交易台已上線。"},
			"operate":    {Title: "日常操作", Blurb: "每日流程：簡報、代理工作階段、配對應用程式、訂單預覽、保護與對帳。"},
			"understand": {Title: "理解數據", Blurb: "各項數字量度甚麼、何時可信，以及由誰設定衡量所用的上限。"},
			"reference":  {Title: "參考資料", Blurb: "指令、工具與設定的查閱資料，大部分由所記錄的程式碼產生。"},
			"internals":  {Title: "內部設計", Blurb: "給想了解設計的讀者：程序、型別化契約、儲存、線路協定與打包。"},
		},
		Nav:            []string{"說明文件", "MCP 工具", "遙距存取", "意見回饋", "GitHub"},
		FooterDocs:     "說明文件",
		FooterPrivacy:  "私隱",
		FooterSecurity: "安全",
		Fineprint:      "本站內容並非投資建議。Canary 是分析軟件；本站任何內容均不構成買賣任何證券的建議。",
		EnglishOnly:    "EN",
		Translated:     "本頁由 AI 模型譯自英文原文，尚未經母語人士審閱；兩者不一致時以英文版為準。",
		Stale:          "英文原文在本譯文之後已有更新，本頁內容可能落後。",
		ReadEnglish:    "閱讀英文版",
	},
}

// zhHans is Simplified Chinese, generated from zhHant by simplified.go: the
// glossary for words that differ by region, then OpenCC's hk2s profile for
// the script. Its chrome strings are derived the same way when the renderer
// starts.
var zhHans = &langSpec{
	Tag:         "zh-Hans",
	Slug:        "zh-hans",
	Name:        "简体中文",
	OGLocale:    "zh_CN",
	SocialImage: "social/social-preview-zh-hans.png",
	derivedFrom: zhHant,
}

// languages lists every language the site renders, English first. Adding an
// authored language here and a directory under docs/docs/ is the whole
// registration; a derived language names its source in derivedFrom.
var languages = []*langSpec{english, zhHant, zhHans}

func (l *langSpec) isEnglish() bool { return l.Slug == "" }

// hubOutput is the language's handbook index on disk; hubHref its site path.
func (l *langSpec) hubOutput() string {
	if l.isEnglish() {
		return hubOutput
	}
	return "docs/docs/" + l.Slug + "/index.html"
}

// homeHref is the language's landing page, from the site root.
func (l *langSpec) homeHref() string {
	if l.isEnglish() {
		return "index.html"
	}
	return l.Slug + "/index.html"
}

func (l *langSpec) hubHref() string {
	if l.isEnglish() {
		return hubHref
	}
	return "docs/" + l.Slug + "/"
}

// section returns the section's title and blurb in this language, falling
// back to English for a language that has not named a section.
func (l *langSpec) section(slug string) sectionSpec {
	if s, ok := l.ui.Sections[slug]; ok {
		return s
	}
	for _, s := range sections {
		if s.Slug == slug {
			return s
		}
	}
	return sectionSpec{Slug: slug}
}

// translation is one translated page: the Markdown file under
// docs/docs/<slug>/ that mirrors an English source.
type translation struct {
	lang *langSpec
	// source is the translation's own repository path; page is the English
	// page it translates.
	source string
	page   pageSpec
	// navTitle, summary and description replace the English manifest fields
	// on the translated hub, sidenav and page head.
	navTitle    string
	summary     string
	description string
	// sourceRevision is the git blob hash of the English source the
	// translation was made from; stale says the English source has changed
	// since.
	sourceRevision string
	stale          bool
	// derivedFrom is the authored translation a generated page came from,
	// nil for an authored one.
	derivedFrom *translation
	body        []byte
}

func (t *translation) output() string {
	return strings.TrimSuffix(t.source, ".md") + ".html"
}

// translationSource maps an English source path onto its translation path in
// a language: docs/docs/start/install.md -> docs/docs/zh-hant/start/install.md.
func translationSource(lang *langSpec, englishSource string) string {
	rest := strings.TrimPrefix(englishSource, "docs/docs/")
	return "docs/docs/" + lang.Slug + "/" + rest
}

// frontMatter is the block a translation opens with:
//
//	---
//	translation-of: docs/docs/start/install.md
//	source-revision: b2211b4bca2bd59bd138c40d6d0bb2ebb8a78893
//	nav-title: 安裝與首次運行
//	summary: ...
//	description: ...
//	---
//
// One key per line, no nesting, no quoting: a translator writes it by hand and
// the gate reads it without a YAML dependency.
type frontMatter struct {
	fields map[string]string
	body   []byte
}

func parseFrontMatter(source []byte) (frontMatter, error) {
	fm := frontMatter{fields: map[string]string{}}
	if !bytes.HasPrefix(source, []byte("---\n")) {
		return fm, fmt.Errorf("translation must open with a --- front matter block")
	}
	scanner := bufio.NewScanner(bytes.NewReader(source[4:]))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	offset := 4
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		offset += len(line) + 1
		if line == "---" {
			closed = true
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return fm, fmt.Errorf("front matter line %q is not key: value", line)
		}
		key = strings.TrimSpace(key)
		if _, dup := fm.fields[key]; dup {
			return fm, fmt.Errorf("front matter key %q repeats", key)
		}
		fm.fields[key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return fm, err
	}
	if !closed {
		return fm, fmt.Errorf("front matter block is not closed with ---")
	}
	if offset > len(source) {
		offset = len(source)
	}
	fm.body = source[offset:]
	return fm, nil
}

// gitBlobHash is the hash git assigns to a file's content, so a translation
// can record the exact English revision it was made from with
// `git hash-object <source>` and the gate can recompute it without git.
func gitBlobHash(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

var requiredFrontMatter = []string{"translation-of", "source-revision", "nav-title", "summary", "description"}

// loadTranslation reads one translation file and checks it against the
// manifest. english maps an English source path to its page spec.
func loadTranslation(lang *langSpec, source string, content []byte, english map[string]pageSpec, readEnglish func(string) ([]byte, error)) (*translation, error) {
	fm, err := parseFrontMatter(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	for _, key := range requiredFrontMatter {
		if fm.fields[key] == "" {
			return nil, fmt.Errorf("%s: front matter needs %s", source, key)
		}
	}
	for key := range fm.fields {
		known := false
		for _, want := range requiredFrontMatter {
			if key == want {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("%s: unknown front matter key %q", source, key)
		}
	}
	englishSource := path.Clean(fm.fields["translation-of"])
	page, ok := english[englishSource]
	if !ok {
		return nil, fmt.Errorf("%s: translation-of %s is not a published page in the manifest", source, englishSource)
	}
	if want := translationSource(lang, englishSource); want != source {
		return nil, fmt.Errorf("%s: a translation of %s must sit at %s so the source path stays the URL", source, englishSource, want)
	}
	revision := fm.fields["source-revision"]
	if len(revision) != 40 {
		return nil, fmt.Errorf("%s: source-revision %q is not a git blob hash (git hash-object %s)", source, revision, englishSource)
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return nil, fmt.Errorf("%s: source-revision %q is not a git blob hash (git hash-object %s)", source, revision, englishSource)
	}
	current, err := readEnglish(englishSource)
	if err != nil {
		return nil, fmt.Errorf("%s: read %s: %w", source, englishSource, err)
	}
	return &translation{
		lang:           lang,
		source:         source,
		page:           page,
		navTitle:       fm.fields["nav-title"],
		summary:        fm.fields["summary"],
		description:    fm.fields["description"],
		sourceRevision: revision,
		stale:          gitBlobHash(current) != revision,
		body:           fm.body,
	}, nil
}

// translationSet indexes every translation by language slug and English
// source.
type translationSet map[string]map[string]*translation

func (s translationSet) lookup(lang *langSpec, englishSource string) *translation {
	if lang == nil || lang.isEnglish() {
		return nil
	}
	return s[lang.Slug][englishSource]
}

// discoverTranslations reads every tracked Markdown file under a language
// directory. A Markdown file under docs/docs/ in a directory that is not a
// registered language fails: a new language is registered in languages first.
func discoverTranslations(tracked map[string]bool, english map[string]pageSpec, read func(string) ([]byte, error)) (translationSet, error) {
	set := translationSet{}
	bySlug := map[string]*langSpec{}
	for _, lang := range languages {
		if !lang.isEnglish() {
			bySlug[lang.Slug] = lang
			set[lang.Slug] = map[string]*translation{}
		}
	}
	// derive is the converter for generated languages; loaded only when one
	// is registered, which it is.
	var conv *converter
	for _, lang := range languages {
		if lang.derivedFrom != nil {
			c, err := newConverter()
			if err != nil {
				return nil, err
			}
			if err := deriveSimplifiedUI(c); err != nil {
				return nil, err
			}
			conv = c
		}
	}
	known := map[string]bool{}
	for _, section := range sections {
		known[section.Slug] = true
	}
	var files []string
	for file := range tracked {
		if strings.HasPrefix(file, "docs/docs/") && strings.HasSuffix(file, ".md") {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	for _, file := range files {
		rest := strings.TrimPrefix(file, "docs/docs/")
		first, _, ok := strings.Cut(rest, "/")
		if !ok || known[first] {
			continue
		}
		lang, ok := bySlug[first]
		if !ok {
			return nil, fmt.Errorf("%s sits under docs/docs/%s/, which is neither a section nor a registered language", file, first)
		}
		if lang.derivedFrom != nil {
			return nil, fmt.Errorf("%s is tracked, but %s pages are generated from %s at build time; edit the %s page instead", file, lang.Tag, lang.derivedFrom.Tag, lang.derivedFrom.Tag)
		}
		content, err := read(file)
		if err != nil {
			return nil, err
		}
		t, err := loadTranslation(lang, file, content, english, read)
		if err != nil {
			return nil, err
		}
		set[lang.Slug][path.Clean(t.page.Source)] = t
	}
	for _, lang := range languages {
		if lang.derivedFrom == nil {
			continue
		}
		for source, t := range set[lang.derivedFrom.Slug] {
			derived, err := deriveTranslation(conv, t)
			if err != nil {
				return nil, fmt.Errorf("%s: derive %s: %w", t.source, lang.Tag, err)
			}
			set[lang.Slug][source] = derived
		}
	}
	return set, nil
}

// report lists every translation with its state, for the docs-translations
// gate. It returns the number of translations behind their source.
func (s translationSet) report(out *strings.Builder) int {
	behind := 0
	total := 0
	derived := 0
	for _, lang := range languages {
		if lang.isEnglish() {
			continue
		}
		var sources []string
		for source := range s[lang.Slug] {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		for _, source := range sources {
			t := s[lang.Slug][source]
			if t.derivedFrom != nil {
				derived++
				state := "current"
				if t.stale {
					state = "behind"
				}
				fmt.Fprintf(out, "  derived  %s <- %s (%s, generated at build)\n", t.source, t.derivedFrom.source, state)
				continue
			}
			total++
			if t.stale {
				behind++
				fmt.Fprintf(out, "  behind   %s <- %s (translated at %s; re-translate, then set source-revision to `git hash-object %s`)\n",
					t.source, source, t.sourceRevision[:8], source)
			} else {
				fmt.Fprintf(out, "  current  %s <- %s\n", t.source, source)
			}
		}
	}
	fmt.Fprintf(out, "docs-translations: %d translation(s), %d derived, %d behind their English source\n", total, derived, behind)
	return behind
}
