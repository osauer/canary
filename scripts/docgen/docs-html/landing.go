package main

// The landing page is hand-written HTML, so it has no Markdown to translate.
// English stays the one page: its layout, figures, links and release stamps
// live only in docs/index.html. A translation is a catalogue of English and
// Chinese strings applied to that page at build, so a release that moves a
// version number or a style needs no second edit, and an English sentence that
// changes shows in English on the Chinese page until its pair is updated.
//
// Replacement is whole element content or a whole reader-text attribute, never
// a substring: the key "Main" can only match >Main< or aria-label="Main", so it
// cannot touch a URL, a class name or a longer sentence.

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	landingSource          = "docs/index.html"
	landingCatalogueSource = "scripts/docgen/docs-html/landing-zh-hant.txt"
)

type landingPair struct{ en, zh string }

type landingCatalogue struct {
	pairs []landingPair
	keep  map[string]bool
}

// parseLandingCatalogue reads "en:" and "zh:" lines in pairs and "keep:" lines
// for strings that stay English on purpose. A literal \n in a value is a line
// break, for a terminal block.
func parseLandingCatalogue(data []byte) (*landingCatalogue, error) {
	c := &landingCatalogue{keep: map[string]bool{}}
	seen := map[string]bool{}
	pending := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		field, value, ok := strings.Cut(line, ": ")
		if !ok {
			return nil, fmt.Errorf("line %d: want en:, zh: or keep:", number)
		}
		value = strings.ReplaceAll(value, `\n`, "\n")
		switch field {
		case "en":
			if pending != "" {
				return nil, fmt.Errorf("line %d: the string before it has no zh line", number)
			}
			if seen[value] {
				return nil, fmt.Errorf("line %d: duplicate string %q", number, value)
			}
			seen[value] = true
			pending = value
		case "zh":
			if pending == "" {
				return nil, fmt.Errorf("line %d: zh without en", number)
			}
			c.pairs = append(c.pairs, landingPair{pending, value})
			pending = ""
		case "keep":
			c.keep[value] = true
		default:
			return nil, fmt.Errorf("line %d: want en:, zh: or keep:", number)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if pending != "" {
		return nil, fmt.Errorf("the last string has no zh line")
	}
	return c, nil
}

// derive converts every Chinese value, for the Simplified page.
func (c *landingCatalogue) derive(conv *converter) (*landingCatalogue, error) {
	out := &landingCatalogue{keep: c.keep, pairs: make([]landingPair, len(c.pairs))}
	for i, pair := range c.pairs {
		zh, err := conv.convert(pair.zh)
		if err != nil {
			return nil, err
		}
		out.pairs[i] = landingPair{pair.en, zh}
	}
	return out, nil
}

// landingTextAttrs are the attributes, and the JSON-LD value, that hold reader
// text. A class, an id or a form value never translates, whatever it spells.
var landingTextAttrs = []string{`alt=`, `aria-label=`, `content=`, `: `}

// apply translates a page, and returns the catalogue's English strings the page
// no longer contains.
func (c *landingCatalogue) apply(page string) (string, []string) {
	args := make([]string, 0, len(c.pairs)*2*(len(landingTextAttrs)+1))
	var unused []string
	for _, pair := range c.pairs {
		found := false
		add := func(en, zh string) {
			found = found || strings.Contains(page, en)
			args = append(args, en, zh)
		}
		add(">"+pair.en+"<", ">"+pair.zh+"<")
		for _, attr := range landingTextAttrs {
			add(attr+`"`+pair.en+`"`, attr+`"`+pair.zh+`"`)
		}
		if !found {
			unused = append(unused, pair.en)
		}
	}
	return strings.NewReplacer(args...).Replace(page), unused
}

var (
	landingCodeBlocks = regexp.MustCompile(`(?s)<(style|script|pre|code)\b.*?</(style|script|pre|code)>`)
	landingTextNode   = regexp.MustCompile(`>([^<>]+)<`)
	landingLabelAttr  = regexp.MustCompile(`\b(?:alt|aria-label)="([^"]+)"`)
	landingVersionTag = regexp.MustCompile(`^v[0-9]`)
)

// untranslated lists the strings of a translated page still in English: those
// with a Latin letter and no Chinese, outside code, unless kept on purpose.
func (c *landingCatalogue) untranslated(page string) []string {
	var out []string
	body := landingCodeBlocks.ReplaceAllString(page, "<x>")
	check := func(matches [][]string) {
		for _, match := range matches {
			text := strings.TrimSpace(match[1])
			hasLetter, hasHan := false, false
			for _, r := range text {
				hasLetter = hasLetter || unicode.IsLetter(r) && r < unicode.MaxASCII
				hasHan = hasHan || unicode.Is(unicode.Han, r)
			}
			if !hasLetter || hasHan || c.keep[text] || landingVersionTag.MatchString(text) {
				continue
			}
			if !slices.Contains(out, text) {
				out = append(out, text)
			}
		}
	}
	check(landingTextNode.FindAllStringSubmatch(body, -1))
	check(landingLabelAttr.FindAllStringSubmatch(body, -1))
	return out
}

// loadLanding reads the Traditional catalogue and derives the Simplified one.
func (r *siteRenderer) loadLanding() error {
	data, err := r.read(landingCatalogueSource)
	if err != nil {
		return err
	}
	traditional, err := parseLandingCatalogue(data)
	if err != nil {
		return fmt.Errorf("%s: %w", landingCatalogueSource, err)
	}
	conv, err := newConverter()
	if err != nil {
		return err
	}
	simplified, err := traditional.derive(conv)
	if err != nil {
		return err
	}
	r.landing = map[string]*landingCatalogue{zhHant.Slug: traditional, zhHans.Slug: simplified}
	return nil
}

// landingReport lists, for the docs-translations gate, each string of the
// English landing page the Traditional catalogue has not caught up with. The
// Simplified page derives from the same catalogue, so one list covers both.
func (r *siteRenderer) landingReport(out *strings.Builder) (int, error) {
	behind, err := r.landingBehind(zhHant)
	if err != nil {
		return 0, err
	}
	for _, text := range behind {
		fmt.Fprintf(out, "behind    landing page: %s\n", strings.ReplaceAll(text, "\n", " "))
	}
	fmt.Fprintf(out, "docs-translations: landing page, %d string(s), %d behind its English source\n", len(r.landing[zhHant.Slug].pairs), len(behind))
	return len(behind), nil
}

func landingOutput(lang *langSpec) string {
	if lang.isEnglish() {
		return landingSource
	}
	return "docs/" + lang.Slug + "/index.html"
}

func landingVersions() map[string]string {
	versions := map[string]string{}
	for _, lang := range languages {
		versions[lang.Tag] = landingOutput(lang)
	}
	return versions
}

// landingBehind counts what a language's landing page has fallen behind by.
func (r *siteRenderer) landingBehind(lang *langSpec) ([]string, error) {
	page, err := r.read(landingSource)
	if err != nil {
		return nil, err
	}
	catalogue := r.landing[lang.Slug]
	translated, unused := catalogue.apply(string(page))
	return append(catalogue.untranslated(translated), unused...), nil
}

// renderLanding builds the landing page in a language. Every language gets the
// hreflang links, the language script and the selector; a translation also gets
// its text, its links re-based one directory down and its own canonical URL.
func (r *siteRenderer) renderLanding(lang *langSpec) ([]byte, error) {
	data, err := r.read(landingSource)
	if err != nil {
		return nil, err
	}
	page, output := string(data), landingOutput(lang)
	note := ""
	if !lang.isEnglish() {
		catalogue := r.landing[lang.Slug]
		page, _ = catalogue.apply(page)
		behind, err := r.landingBehind(lang)
		if err != nil {
			return nil, err
		}
		page = r.rebaseLandingLinks(page, lang)
		own := siteURL(output)
		page = strings.ReplaceAll(page, `"`+publicBaseURL+`"`, `"`+own+`"`)
		page = strings.ReplaceAll(page, publicBaseURL+english.SocialImage, publicBaseURL+lang.SocialImage)
		page = strings.ReplaceAll(page, `href="/canary/"`, `href="/canary/`+lang.Slug+`/"`)
		page, err = replaceOnce(page, `<html lang="en">`, fmt.Sprintf(`<html lang="%s">`, lang.Tag))
		if err != nil {
			return nil, err
		}
		page, err = replaceOnce(page, `"@type": "SoftwareApplication",`, fmt.Sprintf(`"@type": "SoftwareApplication",
        "inLanguage": %q,`, lang.Tag))
		if err != nil {
			return nil, err
		}
		class, text := "translation-note", lang.ui.Translated
		if len(behind) > 0 {
			class, text = "translation-note is-stale", lang.ui.Stale
		}
		note = fmt.Sprintf("    <p class=\"%s\">%s<a href=\"../\" hreflang=\"en\" data-lang=\"en\">%s</a></p>\n    ",
			class, template.HTMLEscapeString(text), template.HTMLEscapeString(lang.ui.ReadEnglish))
	}
	selector, err := langLinksHTML(lang, filepath.Dir(output), landingVersions())
	if err != nil {
		return nil, err
	}
	localeTag := `  <meta property="og:locale" content="en_GB">` + "\n"
	if page, err = replaceOnce(page, localeTag, localeTagsHTML(lang, landingVersions())); err != nil {
		return nil, err
	}
	head := alternatesHTML(landingVersions()) +
		fmt.Sprintf("  <script src=\"%slang.js\"></script>\n", relativeRootPrefix(output))
	if page, err = replaceOnce(page, "</head>", head+"</head>"); err != nil {
		return nil, err
	}
	if page, err = replaceOnce(page, "</header>", selector+"  </header>"); err != nil {
		return nil, err
	}
	if note != "" {
		if page, err = replaceOnce(page, `<div class="footer-bottom">`, strings.TrimLeft(note, " ")+`<div class="footer-bottom">`); err != nil {
			return nil, err
		}
	}
	return []byte(page), nil
}

func replaceOnce(page, old, replacement string) (string, error) {
	if count := strings.Count(page, old); count != 1 {
		return "", fmt.Errorf("%s: want one %q, found %d", landingSource, old, count)
	}
	return strings.Replace(page, old, replacement, 1), nil
}

var (
	landingAnchorTag = regexp.MustCompile(`<a\b[^>]*>`)
	landingHref      = regexp.MustCompile(`\bhref="([^"]*)"`)
	landingResource  = regexp.MustCompile(`\b(href|src|poster)="([^"]*)"`)
)

func landingRelative(target string) bool {
	return target != "" && !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "#") &&
		!strings.HasPrefix(target, "../") && !strings.Contains(target, ":")
}

// rebaseLandingLinks moves every relative link one directory down. A link to a
// page that exists in this language goes to that version; a link to a page that
// exists in English only says so with hreflang, which the page's style marks.
func (r *siteRenderer) rebaseLandingLinks(page string, lang *langSpec) string {
	translated := map[string]string{"docs/": strings.TrimPrefix(lang.hubOutput(), "docs/")}
	for _, t := range r.translations[lang.Slug] {
		translated[strings.TrimPrefix(t.page.output(), "docs/")] = strings.TrimPrefix(t.output(), "docs/")
	}
	page = landingAnchorTag.ReplaceAllStringFunc(page, func(tag string) string {
		match := landingHref.FindStringSubmatch(tag)
		if match == nil || !landingRelative(match[1]) {
			return tag
		}
		parsed, err := url.Parse(match[1])
		if err != nil {
			return tag
		}
		target, ok := translated[parsed.Path]
		if ok {
			parsed.Path = "../" + strings.TrimSuffix(target, "index.html")
			return strings.Replace(tag, match[0], `href="`+parsed.String()+`"`, 1)
		}
		replacement := `href="../` + match[1] + `"`
		if strings.HasSuffix(parsed.Path, "/") || strings.HasSuffix(parsed.Path, ".html") {
			replacement += ` hreflang="en"`
		}
		return strings.Replace(tag, match[0], replacement, 1)
	})
	return landingResource.ReplaceAllStringFunc(page, func(attr string) string {
		match := landingResource.FindStringSubmatch(attr)
		if !landingRelative(match[2]) {
			return attr
		}
		return match[1] + `="../` + match[2] + `"`
	})
}
