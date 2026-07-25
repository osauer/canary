// Command docs-html renders the public documentation site from its Markdown
// sources. Markdown is the only prose authority; the checked-in HTML exists
// because the current GitHub Pages setup publishes docs/ as static files.
//
// Markdown sits beside the HTML it produces, so a source path is the published
// URL and there is nothing to keep in step. The manifest below adds only what
// the file system cannot say: which section a page belongs to, how it is
// described on the handbook index, and which retired URLs still redirect to it.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const (
	publicBaseURL = "https://osauer.dev/ibkr/"
	// hubOutput is the handbook index. The site root is docs/, so the public
	// /ibkr/docs/ prefix lands at docs/docs/ on disk.
	hubOutput = "docs/docs/index.html"
	hubHref   = "docs/"
)

// pageStatus separates pages that render from pages that are only announced on
// the handbook index. A planned page has no HTML output and never reaches the
// sitemap, so nothing thin is ever indexed.
type pageStatus string

const (
	statusPublished pageStatus = "published"
	statusPlanned   pageStatus = "planned"
)

type sectionSpec struct {
	Slug  string
	Title string
	Blurb string
}

// sections run from "get it running" to "how it is built". Order is the
// reading order on the handbook index.
var sections = []sectionSpec{
	{
		Slug:  "start",
		Title: "Start",
		Blurb: "Install ibkr, point it at a gateway and an MCP host, and confirm the desk is live.",
	},
	{
		Slug:  "operate",
		Title: "Operate",
		Blurb: "The daily loop: briefs, agent sessions, the paired app, order previews, protection, and reconciliation.",
	},
	{
		Slug:  "understand",
		Title: "Understand",
		Blurb: "What the numbers measure, when they can be trusted, and who sets the limits they are measured against.",
	},
	{
		Slug:  "reference",
		Title: "Reference",
		Blurb: "Command, tool, and configuration lookups. Most of this is generated from the code it documents.",
	},
	{
		Slug:  "internals",
		Title: "Under the hood",
		Blurb: "For readers who want the design: processes, typed contracts, storage, the wire protocol, and packaging.",
	},
}

type pageSpec struct {
	// Source is the Markdown authority. Empty for planned pages.
	Source string
	// Draft points at the repository stub that holds the scope of a planned
	// page. It is never rendered and never served. Page is where that draft
	// will publish once written; published pages derive it from Source.
	Draft string
	Page  string
	// Section keys into sections; NavTitle and Summary drive the handbook index
	// and the navigation tree. NavTitle normally equals the page's H1. The
	// Reference section is the deliberate exception: its nav labels drop the
	// word "reference" because the section heading right above them already
	// says it, while each H1 keeps it because a page title has to stand alone
	// in a browser tab and a search result.
	Section  string
	NavTitle string
	Summary  string
	// Description is the meta description of the rendered page.
	Description string
	Layout      string
	// SocialImage is site-root relative.
	SocialImage string
	Status      pageStatus
	// Legacy lists paths that used to serve this page. Each one is rendered as
	// a redirect stub so old links and search results keep working.
	Legacy []string
}

func (p pageSpec) planned() bool { return p.Status == statusPlanned }

// output is the published HTML path. Markdown sits beside the HTML it
// produces, so the source path is the URL and there is no mapping to keep
// honest. Planned pages declare Page directly because they have no source yet.
func (p pageSpec) output() string {
	if p.planned() {
		return p.Page
	}
	return strings.TrimSuffix(p.Source, ".md") + ".html"
}

var pages = []pageSpec{
	// ---- Start -------------------------------------------------------------
	{
		Section:  "start",
		Page:     "docs/docs/start/install.html",
		NavTitle: "Install and first run",
		Summary:  "Prerequisites, the four install paths, and the first commands that prove the gateway is reachable.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/start-install.md",
	},
	{
		Section:  "start",
		Page:     "docs/docs/start/hosts.html",
		NavTitle: "Connect an MCP host",
		Summary:  "Wiring Claude Desktop, Claude Code, Cursor, and Zed to the local server, and checking the connection.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/start-hosts.md",
	},
	{
		Section:  "start",
		Page:     "docs/docs/start/first-session.html",
		NavTitle: "Your first session",
		Summary:  "A guided half hour: read the account, quote a symbol, run a brief, and interpret what comes back.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/start-first-session.md",
	},
	{
		Source:      "docs/docs/start/updating.md",
		Section:     "start",
		NavTitle:    "Updating",
		Summary:     "Update the binary, the Desktop extension, S&P 500 membership, calendars, and local process state.",
		Description: "How to update the ibkr binary, Desktop extension, S&P 500 membership, calendars, and local process state.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/updating.html"},
	},
	{
		Section:  "start",
		Page:     "docs/docs/start/troubleshooting.html",
		NavTitle: "Troubleshooting",
		Summary:  "Gateway not reachable, stale quotes, a daemon that will not start, and the logs that answer each one.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/start-troubleshooting.md",
	},

	// ---- Operate -----------------------------------------------------------
	{
		Section:  "operate",
		Page:     "docs/docs/operate/daily-desk.html",
		NavTitle: "The daily desk",
		Summary:  "A working routine from the morning brief through regime, canary, and rules to the end-of-day read.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/operate-daily-desk.md",
	},
	{
		Source:      "docs/docs/operate/agents.md",
		Section:     "operate",
		NavTitle:    "Working with agents",
		Summary:     "Workflows, limits, and worked examples for driving ibkr from an MCP host.",
		Description: "Practical agentic workflows, limits, and examples for using ibkr through an MCP host.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/agentic-use.html"},
	},
	{
		Source:      "docs/docs/operate/app.md",
		Section:     "operate",
		NavTitle:    "The paired app",
		Summary:     "Running ibkr app, pairing a phone, the event stream, and opt-in canary push notifications.",
		Description: "How the ibkr app process serves the paired PWA, owns pairing, streams events, and sends opt-in canary notifications.",
		Status:      statusPublished,
	},
	{
		Section:  "operate",
		Page:     "docs/docs/operate/alerts.html",
		NavTitle: "Alerts and notifications",
		Summary:  "What raises an alert, where it is delivered, how delivery is proven, and how to tune the noise down.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/operate-alerts.md",
	},
	{
		Source:      "docs/docs/operate/orders.md",
		Section:     "operate",
		NavTitle:    "Order previews and the trading build",
		Summary:     "The read-only default, what an order preview does and does not do, and the separate opt-in trading build.",
		Description: "The read-only default in ibkr, what the order preview surface does, and how the separate experimental trading build is configured.",
		Status:      statusPublished,
	},
	{
		Section:  "operate",
		Page:     "docs/docs/operate/protection.html",
		NavTitle: "Protection and emergency exits",
		Summary:  "Trailing-stop and risk-reduction proposals, per-row blockers, and what purge and restore actually do.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/operate-protection.md",
	},
	{
		Section:  "operate",
		Page:     "docs/docs/operate/reconciliation.html",
		NavTitle: "Reconciliation",
		Summary:  "Matching broker statement flows against the declared capital ledger, and handling the lines that will not match.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/operate-reconciliation.md",
	},

	// ---- Understand --------------------------------------------------------
	{
		Source:      "docs/docs/understand/concepts.md",
		Section:     "understand",
		NavTitle:    "Concepts",
		Summary:     "Calendars, regime, canary, market events, protective stops, gamma, and breadth in one mental model.",
		Description: "What the load-bearing market, portfolio, and data-quality context surfaces measure, and how to read them without mis-acting on the output.",
		Layout:      "architecture",
		Status:      statusPublished,
		Legacy:      []string{"docs/concepts.html"},
	},
	{
		Source:      "docs/docs/understand/sensors.md",
		Section:     "understand",
		NavTitle:    "Sensors",
		Summary:     "How each measurement establishes authority and freshness, and how a gap fails closed instead of guessing.",
		Description: "How ibkr Gamma, Regime, Canary, Rulebook, and market-event sensors establish authority, freshness, last-good context, and fail-closed data quality.",
		Layout:      "architecture",
		SocialImage: "diagrams/sensor-authority-pipeline.png",
		Status:      statusPublished,
		Legacy:      []string{"docs/sensors.html"},
	},
	{
		Source:      "docs/docs/understand/policy.md",
		Section:     "understand",
		NavTitle:    "Trading policy",
		Summary:     "Who decides the risk boundaries, what the daemon evaluates, and what never becomes submit authority.",
		Description: "Who decides trading policy in ibkr, what the daemon does today, how controls change, and what remains an explicit human decision.",
		Layout:      "architecture",
		SocialImage: "diagrams/policy-lifecycle.png",
		Status:      statusPublished,
		Legacy:      []string{"docs/policies.html"},
	},
	{
		Section:  "understand",
		Page:     "docs/docs/understand/rulebook.html",
		NavTitle: "The rulebook",
		Summary:  "The fourteen advisory rules, what each one is protecting against, and how a breach is reported.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/understand-rulebook.md",
	},
	{
		Section:  "understand",
		Page:     "docs/docs/understand/risk-policy.html",
		NavTitle: "Writing a risk policy",
		Summary:  "Turning a personal risk mandate into the policy file: limits, drawdown ladder, overrides, and review.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/understand-risk-policy.md",
	},
	{
		Section:  "understand",
		Page:     "docs/docs/understand/market-data.html",
		NavTitle: "Market data and entitlements",
		Summary:  "Which subscriptions produce real-time data, what delayed and frozen mean, and how freshness is reported.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/understand-market-data.md",
	},
	{
		Section:  "understand",
		Page:     "docs/docs/understand/glossary.html",
		NavTitle: "Glossary",
		Summary:  "One place for the terms the other pages assume: NLV, R-multiple, zero gamma, last-good, fingerprint.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/understand-glossary.md",
	},

	// ---- Reference ---------------------------------------------------------
	{
		Source:      "docs/docs/reference/cli.md",
		Section:     "reference",
		NavTitle:    "CLI",
		Summary:     "Every ibkr subcommand with its flags and usage, generated from the command registry in the binary.",
		Description: "Generated reference for every ibkr subcommand, with its usage line, flags, subcommands, guard class, and MCP counterpart.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/reference/mcp-tools.md",
		Section:     "reference",
		NavTitle:    "MCP tools",
		Summary:     "Every tool exposed by ibkr mcp, with parameters and invocation guidance. Generated from the registry.",
		Description: "Generated reference for every tool exposed by ibkr mcp, including parameters and invocation guidance.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/mcp-tools.html"},
	},
	{
		Source:      "docs/docs/reference/mcp-resources.md",
		Section:     "reference",
		NavTitle:    "MCP resources",
		Summary:     "The non-tool resources, including the live quote subscription URI template.",
		Description: "Reference for the non-tool resources exposed by ibkr mcp, including live quote subscriptions.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/mcp-resources.html"},
	},
	{
		Source:      "docs/docs/reference/config.md",
		Section:     "reference",
		NavTitle:    "Configuration",
		Summary:     "TOML configuration, policy files, runtime platform settings, and environment variables.",
		Description: "Generated reference for ibkr TOML configuration, policy files, runtime platform settings, and environment variables.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/config.html"},
	},
	{
		Section:  "reference",
		Page:     "docs/docs/reference/releases.html",
		NavTitle: "Releases and support",
		Summary:  "Version scheme, what a release contains, how signatures are verified, and which versions are supported.",
		Status:   statusPlanned,
		Draft:    "internal-docs/drafts/reference-releases.md",
	},

	// ---- Under the hood ----------------------------------------------------
	{
		Source:      "docs/docs/internals/architecture.md",
		Section:     "internals",
		NavTitle:    "Architecture",
		Summary:     "Runtime processes, typed contracts, external data flows, state ownership, and deployment boundaries.",
		Description: "System architecture, protocols, external data flows, process boundaries, and local persistence for ibkr Canary.",
		Layout:      "architecture",
		SocialImage: "diagrams/system-architecture.png",
		Status:      statusPublished,
		Legacy:      []string{"docs/architecture.html"},
	},
	{
		Source:      "docs/docs/internals/storage.md",
		Section:     "internals",
		NavTitle:    "Storage",
		Summary:     "Why the daemon uses SQLite, how the data model follows from its job, and how state survives a restart.",
		Description: "How the ibkr daemon preserves state and evidence with SQLite, why the model exists, how data moves, and where the design must evolve.",
		Layout:      "architecture",
		SocialImage: "diagrams/storage-overview.png",
		Status:      statusPublished,
		Legacy:      []string{"docs/database.html"},
	},
	{
		Source:      "docs/docs/internals/protocol.md",
		Section:     "internals",
		NavTitle:    "TWS wire protocol",
		Summary:     "Coverage and semantic fingerprints for the clean-room Go implementation of the TWS protocol.",
		Description: "Coverage and semantic fingerprints for the clean-room Go implementation of the Interactive Brokers TWS wire protocol.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/protocol.html"},
	},
	{
		Source:      "docs/docs/internals/regime-dashboard.md",
		Section:     "internals",
		NavTitle:    "Regime dashboard contract",
		Summary:     "Source quality, cluster logic, lifecycle decisions, and the backtest that has to pass before a change ships.",
		Description: "Contract for the broad-market regime dashboard, source quality, cluster logic, lifecycle decisions, and backtesting.",
		Status:      statusPublished,
		Legacy:      []string{"docs/specs/risk-regime-dashboard.html"},
	},
	{
		Source:      "docs/docs/internals/regime-backtest.md",
		Section:     "internals",
		NavTitle:    "Regime and canary backtest runbook",
		Summary:     "How the regime and canary lifecycle is proven and tuned against point-in-time evidence.",
		Description: "Runbook for proving and tuning the ibkr regime and Canary lifecycle against point-in-time evidence.",
		Status:      statusPublished,
		Legacy:      []string{"docs/specs/regime-backtest-plan.html"},
	},
	{
		Source:      "docs/docs/internals/opportunity-research.md",
		Section:     "internals",
		NavTitle:    "Opportunity research harness",
		Summary:     "The diagnostic harness for testing candidate strategies against point-in-time evidence before trusting them.",
		Description: "The diagnostic harness that captures point-in-time evidence, scores later outcomes, and reports whether a candidate strategy is still too weak to trust.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/internals/gamma-cache.md",
		Section:     "internals",
		NavTitle:    "Gamma cache persistence",
		Summary:     "Design and invalidation semantics for the daemon's persistent dealer zero-gamma cache.",
		Description: "Design and invalidation semantics for the daemon's persistent dealer zero-gamma cache.",
		Status:      statusPublished,
		Legacy:      []string{"docs/design/gamma-zero-cache-persistence.html"},
	},
	{
		Source:      "docs/docs/internals/packaging.md",
		Section:     "internals",
		NavTitle:    "Packaging and distribution",
		Summary:     "What each published artifact contains, how it is signed, and the boundary every channel ships with.",
		Description: "How ibkr is packaged and distributed: the plugin, the Claude Desktop bundle, directory metadata, signing, and the read-only boundary each channel ships with.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/marketplace-readiness.html"},
	},
}

// navItems is the one definition of the site navigation. The generated pages
// render it directly; a test asserts the hand-written landing page carries the
// same items in the same order, so the two navigations cannot drift.
var navItems = []struct{ Label, Href string }{
	{"Documentation", hubHref},
	{"MCP tools", "docs/reference/mcp-tools.html"},
	{"Remote app beta", "canary-remote/"},
	{"Feedback", "feedback/"},
	{"GitHub", "https://github.com/osauer/ibkr"},
}

type headingInfo struct {
	Level int
	ID    string
	Text  string
}

type templateData struct {
	Title           string
	Description     string
	Canonical       string
	RootPrefix      string
	Layout          string
	GeneratorNotice template.HTML
	Nav             template.HTML
	SideNav         template.HTML
	Body            template.HTML
	JSONLD          template.JS
	SocialHead      template.HTML
}

var documentTemplate = template.Must(template.New("document").Parse(`<!doctype html>
{{.GeneratorNotice}}
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} | ibkr</title>
  <meta name="description" content="{{.Description}}">
  <link rel="canonical" href="{{.Canonical}}">
  <link rel="icon" type="image/png" href="{{.RootPrefix}}social/canary-icon.png">
{{.SocialHead}}
  <script type="application/ld+json">{{.JSONLD}}</script>
  <link rel="stylesheet" href="{{.RootPrefix}}shared.css">
</head>
<body class="docpage layout-{{.Layout}}">
  <div class="topline"></div>
  <header class="wrap nav" aria-label="Primary">
    <a class="brand" href="{{.RootPrefix}}index.html" aria-label="ibkr canary home"><img src="{{.RootPrefix}}social/canary-icon.png" width="192" height="192" alt="">ibkr canary</a>
{{.Nav}}  </header>
  <div class="wrap shell">
{{.SideNav}}    <main class="doc">
{{.Body}}    </main>
  </div>
  <footer>
    <div class="wrap"><a href="{{.RootPrefix}}index.html">ibkr</a><a href="{{.RootPrefix}}docs/">Documentation</a><a href="https://github.com/osauer/ibkr">GitHub</a><a href="https://github.com/osauer/ibkr/blob/main/PRIVACY.md">Privacy</a><a href="https://github.com/osauer/ibkr/blob/main/SECURITY.md">Security</a></div>
    <div class="wrap fineprint">Not financial advice. ibkr is analysis software; nothing here is a recommendation to buy or sell any security.</div>
  </footer>
</body>
</html>
`))

// The notice is passed as data rather than written inline: html/template
// strips comments that appear in the template text itself.
var redirectTemplate = template.Must(template.New("redirect").Parse(`<!doctype html>
{{.Notice}}
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <meta http-equiv="refresh" content="0; url={{.Target}}">
  <link rel="canonical" href="{{.Canonical}}">
  <title>{{.Title}} | ibkr</title>
</head>
<body>
  <p>This page has moved to <a href="{{.Target}}">{{.Title}}</a>.</p>
</body>
</html>
`))

type siteRenderer struct {
	root      string
	generated map[string]string
	tracked   map[string]bool
	markdown  goldmark.Markdown
	// unresolved collects relative links that point at nothing tracked, so the
	// run can fail with the whole list rather than one at a time.
	unresolved []string
}

func newSiteRenderer(root string, tracked map[string]bool) *siteRenderer {
	generated := make(map[string]string, len(pages))
	for _, page := range pages {
		if page.planned() {
			continue
		}
		generated[filepath.ToSlash(page.Source)] = filepath.ToSlash(page.output())
	}
	return &siteRenderer{
		root:      root,
		generated: generated,
		tracked:   tracked,
		markdown: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
	}
}

// navHTML renders the shared navigation for a page at the given root prefix.
func navHTML(rootPrefix string) string {
	var out strings.Builder
	out.WriteString("    <nav class=\"nav-links\" aria-label=\"Site\">\n")
	for _, item := range navItems {
		href := item.Href
		if !strings.Contains(href, "://") {
			href = rootPrefix + href
		}
		fmt.Fprintf(&out, "      <a href=\"%s\">%s</a>\n", template.HTMLEscapeString(href), template.HTMLEscapeString(item.Label))
	}
	out.WriteString("    </nav>\n")
	return out.String()
}

// sideNavHTML renders the between-page navigation tree that every generated
// page carries. It reads the same manifest as the handbook index, so a page
// added there reaches the tree with no second edit, and a planned page appears
// without a link exactly as the index shows it. current is the
// repository-relative output path of the page being rendered; hubOutput asks
// for the handbook index itself.
//
// The tree ships no JavaScript. <details> collapses it on a phone, and wide
// screens force it open through ::details-content; a browser without that
// pseudo-element keeps a collapsed control that still opens on click.
//
// Each section is its own <details>, open on the section being read. Fully
// expanded the tree runs about 1100px against a 720px laptop viewport, so a
// reader on the last section could not see their own position without
// scrolling the sidebar, which is the question the tree exists to answer.
func sideNavHTML(rootPrefix, current string) (string, error) {
	currentDir := filepath.Dir(current)

	here, hereTitle := "", ""
	for _, page := range pages {
		if !page.planned() && filepath.ToSlash(page.output()) == current {
			here = page.Section
		}
	}
	for _, section := range sections {
		if section.Slug == here {
			hereTitle = section.Title
		}
	}

	var out strings.Builder
	out.WriteString("    <details class=\"sidenav\">\n")
	out.WriteString("      <summary class=\"sidenav-summary\">Documentation")
	if hereTitle != "" {
		// The closed control on a phone says which section the reader is in,
		// so the tree does not have to be opened to answer "where am I".
		fmt.Fprintf(&out, "<span class=\"sidenav-here\">%s</span>", template.HTMLEscapeString(hereTitle))
	}
	out.WriteString("</summary>\n")
	out.WriteString("      <nav class=\"sidenav-tree\" aria-label=\"Documentation\">\n")

	overview, overviewMark := "sidenav-overview", ""
	if current == hubOutput {
		overview += " is-current"
		overviewMark = " aria-current=\"page\""
	}
	fmt.Fprintf(&out, "        <a class=\"%s\" href=\"%s\"%s>Overview</a>\n",
		overview, template.HTMLEscapeString(rootPrefix+hubHref), overviewMark)

	out.WriteString("        <ul class=\"sidenav-sections\">\n")
	for _, section := range sections {
		class, open := "sidenav-section", ""
		if section.Slug == here {
			class, open = "sidenav-section is-here", " open"
		}
		fmt.Fprintf(&out, "          <li class=\"%s\">\n", class)
		fmt.Fprintf(&out, "            <details class=\"sidenav-group\"%s>\n", open)
		fmt.Fprintf(&out, "              <summary class=\"sidenav-heading\">%s</summary>\n", template.HTMLEscapeString(section.Title))
		out.WriteString("              <ul>\n")
		for _, page := range pages {
			if page.Section != section.Slug {
				continue
			}
			title := template.HTMLEscapeString(page.NavTitle)
			if page.planned() {
				fmt.Fprintf(&out, "                <li><span class=\"sidenav-planned\">%s</span></li>\n", title)
				continue
			}
			output := filepath.ToSlash(page.output())
			href, err := filepath.Rel(currentDir, output)
			if err != nil {
				return "", err
			}
			mark := ""
			if output == current {
				mark = " class=\"is-current\" aria-current=\"page\""
			}
			fmt.Fprintf(&out, "                <li><a href=\"%s\"%s>%s</a></li>\n",
				template.HTMLEscapeString(filepath.ToSlash(href)), mark, title)
		}
		out.WriteString("              </ul>\n")
		out.WriteString("            </details>\n")
		out.WriteString("          </li>\n")
	}
	out.WriteString("        </ul>\n")
	// The key belongs only where something is dimmed. Nine pages open a
	// section that is fully written (the eight under "Under the hood", plus
	// the index, which opens nothing), and a legend with no referent is noise.
	for _, page := range pages {
		if page.Section == here && page.planned() {
			out.WriteString("        <p class=\"sidenav-note\">Dimmed entries are not written yet.</p>\n")
			break
		}
	}
	out.WriteString("      </nav>\n")
	out.WriteString("    </details>\n")
	return out.String(), nil
}

func (r *siteRenderer) render(page pageSpec) ([]byte, error) {
	sourcePath := filepath.Join(r.root, filepath.FromSlash(page.Source))
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}

	doc := r.markdown.Parser().Parse(text.NewReader(source))
	headings, err := r.transform(doc, source, page)
	if err != nil {
		return nil, err
	}
	if len(headings) == 0 || headings[0].Level != 1 || headings[0].Text == "" {
		return nil, fmt.Errorf("%s must start with one H1 title", page.Source)
	}
	for _, heading := range headings[1:] {
		if heading.Level == 1 {
			return nil, fmt.Errorf("%s has more than one H1", page.Source)
		}
	}

	var body bytes.Buffer
	if err := r.markdown.Renderer().Render(&body, source, doc); err != nil {
		return nil, err
	}
	bodyHTML := wrapTables(body.String())
	if page.Layout == "architecture" {
		bodyHTML = decorateArchitecture(bodyHTML, headings)
	}

	output := filepath.ToSlash(page.output())
	canonical := publicBaseURL + strings.TrimPrefix(output, "docs/")
	rootPrefix := relativeRootPrefix(output)

	jsonLD, err := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "TechArticle",
		"headline":    headings[0].Text,
		"description": page.Description,
		"url":         canonical,
		"author": map[string]string{
			"@type": "Person",
			"name":  "Oliver Sauer",
			"url":   "https://github.com/osauer",
		},
		"isPartOf": map[string]string{
			"@type": "SoftwareApplication",
			"name":  "ibkr",
			"url":   publicBaseURL,
		},
	})
	if err != nil {
		return nil, err
	}

	layout := page.Layout
	if layout == "" {
		layout = "standard"
	}
	sideNav, err := sideNavHTML(rootPrefix, output)
	if err != nil {
		return nil, err
	}
	data := templateData{
		Title:           headings[0].Text,
		Description:     page.Description,
		Canonical:       canonical,
		RootPrefix:      rootPrefix,
		Layout:          layout,
		GeneratorNotice: generatorNotice,
		Nav:             template.HTML(navHTML(rootPrefix)),
		SideNav:         template.HTML(sideNav),
		Body:            template.HTML(bodyHTML), // Goldmark escapes source HTML by default.
		JSONLD:          template.JS(jsonLD),     // json.Marshal produces valid script data.
	}
	if page.SocialImage != "" {
		data.SocialHead = template.HTML(fmt.Sprintf(
			"<meta property=\"og:type\" content=\"article\">\n  <meta property=\"og:title\" content=\"%s | ibkr\">\n  <meta property=\"og:description\" content=\"%s\">\n  <meta property=\"og:image\" content=\"%s\">\n  <meta name=\"twitter:card\" content=\"summary_large_image\">",
			template.HTMLEscapeString(data.Title), template.HTMLEscapeString(data.Description), template.HTMLEscapeString(publicBaseURL+page.SocialImage),
		))
	}

	var out bytes.Buffer
	if err := documentTemplate.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

const generatorNotice = template.HTML("<!-- Generated from Markdown by scripts/docgen/docs-html. DO NOT EDIT. -->")

const hubDescription = "The ibkr canary handbook: install and first run, the daily desk routine, how to read each measurement, generated command and tool references, and the system design underneath."

// renderHub builds the handbook index from the manifest. Planned pages appear
// with their scope but without a link, so a reader can see the whole shape
// while only finished pages are reachable and indexable.
func (r *siteRenderer) renderHub() ([]byte, error) {
	rootPrefix := relativeRootPrefix(hubOutput)
	hubDir := filepath.Dir(filepath.ToSlash(hubOutput))

	var body strings.Builder
	body.WriteString("<h1>Documentation</h1>\n")
	body.WriteString("<p class=\"hub-lead\">Everything written down about running an agentic trading desk on your own Interactive Brokers account. Start at the top if ibkr is new to you; the sections below get progressively closer to the code.</p>\n")

	for _, section := range sections {
		fmt.Fprintf(&body, "<section class=\"hub-section\" id=\"%s\">\n", section.Slug)
		fmt.Fprintf(&body, "<h2>%s</h2>\n", template.HTMLEscapeString(section.Title))
		fmt.Fprintf(&body, "<p class=\"hub-blurb\">%s</p>\n", template.HTMLEscapeString(section.Blurb))
		body.WriteString("<ul class=\"hub-list\">\n")
		for _, page := range pages {
			if page.Section != section.Slug {
				continue
			}
			title := template.HTMLEscapeString(page.NavTitle)
			summary := template.HTMLEscapeString(page.Summary)
			if page.planned() {
				fmt.Fprintf(&body, "<li class=\"hub-item is-planned\"><span class=\"hub-title\">%s</span><span class=\"hub-summary\">%s</span><span class=\"hub-flag\">Not written yet</span></li>\n", title, summary)
				continue
			}
			href, err := filepath.Rel(hubDir, filepath.ToSlash(page.output()))
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&body, "<li class=\"hub-item\"><a href=\"%s\"><span class=\"hub-title\">%s</span><span class=\"hub-summary\">%s</span></a></li>\n",
				template.HTMLEscapeString(filepath.ToSlash(href)), title, summary)
		}
		body.WriteString("</ul>\n</section>\n")
	}

	canonical := publicBaseURL + hubHref
	jsonLD, err := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CollectionPage",
		"name":        "ibkr documentation",
		"description": hubDescription,
		"url":         canonical,
		"isPartOf": map[string]string{
			"@type": "SoftwareApplication",
			"name":  "ibkr",
			"url":   publicBaseURL,
		},
	})
	if err != nil {
		return nil, err
	}

	sideNav, err := sideNavHTML(rootPrefix, hubOutput)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	err = documentTemplate.Execute(&out, templateData{
		Title:           "Documentation",
		Description:     hubDescription,
		Canonical:       canonical,
		RootPrefix:      rootPrefix,
		Layout:          "hub",
		GeneratorNotice: generatorNotice,
		Nav:             template.HTML(navHTML(rootPrefix)),
		SideNav:         template.HTML(sideNav),
		Body:            template.HTML(body.String()),
		JSONLD:          template.JS(jsonLD),
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// renderRedirect builds the stub that keeps a retired URL working.
func renderRedirect(from string, page pageSpec) ([]byte, error) {
	target, err := filepath.Rel(filepath.Dir(filepath.ToSlash(from)), filepath.ToSlash(page.output()))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = redirectTemplate.Execute(&out, struct {
		Target, Canonical, Title string
		Notice                   template.HTML
	}{
		Target:    filepath.ToSlash(target),
		Canonical: publicBaseURL + strings.TrimPrefix(filepath.ToSlash(page.output()), "docs/"),
		Title:     page.NavTitle,
		Notice:    generatorNotice,
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (r *siteRenderer) transform(doc ast.Node, source []byte, page pageSpec) ([]headingInfo, error) {
	var headings []headingInfo
	err := ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.Heading:
			idValue, ok := n.AttributeString("id")
			if !ok {
				return ast.WalkStop, fmt.Errorf("heading in %s has no generated id", page.Source)
			}
			id, ok := idValue.([]byte)
			if !ok {
				return ast.WalkStop, fmt.Errorf("heading id in %s has unexpected type %T", page.Source, idValue)
			}
			headings = append(headings, headingInfo{Level: n.Level, ID: string(id), Text: headingText(n, source)})
		case *ast.Link:
			n.Destination = r.rewriteDestination(page, n.Destination)
		case *ast.Image:
			n.Destination = r.rewriteDestination(page, n.Destination)
		}
		return ast.WalkContinue, nil
	})
	return headings, err
}

func headingText(heading *ast.Heading, source []byte) string {
	var out strings.Builder
	for i := range heading.Lines().Len() {
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
		segment := heading.Lines().At(i)
		out.Write(segment.Value(source))
	}
	return strings.TrimSpace(out.String())
}

// rewriteDestination maps a relative Markdown link onto the published site.
// Markdown sits beside the HTML it produces, so a link that is correct in the
// source is already correct on the site: a link to another generated page only
// swaps its extension, and a link to a tracked asset under docs/ is left
// alone. Anything else tracked in the repository becomes a GitHub blob link,
// because it ships in the repository rather than on the site.
func (r *siteRenderer) rewriteDestination(page pageSpec, destination []byte) []byte {
	raw := string(destination)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Path == "" || strings.HasPrefix(raw, "#") {
		return destination
	}

	sourceDir := filepath.Dir(filepath.ToSlash(page.Source))
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(sourceDir, filepath.FromSlash(parsed.Path))))

	if output, ok := r.generated[resolved]; ok {
		if rel, err := filepath.Rel(sourceDir, output); err == nil {
			parsed.Path = filepath.ToSlash(rel)
			return []byte(parsed.String())
		}
	}
	if strings.HasPrefix(resolved, "docs/") && !strings.HasSuffix(resolved, ".md") && r.tracked[resolved] {
		return destination
	}
	if r.tracked[resolved] {
		github := &url.URL{
			Scheme:   "https",
			Host:     "github.com",
			Path:     "/osauer/ibkr/blob/main/" + resolved,
			RawQuery: parsed.RawQuery,
			Fragment: parsed.Fragment,
		}
		return []byte(github.String())
	}
	// A relative link that resolves to nothing tracked is dead on the site.
	// Passing it through silently is how moving docs/design/ out of the web
	// root shipped 22 broken links: the page still rendered, and the link
	// still looked like a link.
	r.unresolved = append(r.unresolved, fmt.Sprintf("%s -> %s", page.Source, raw))
	return destination
}

func wrapTables(body string) string {
	body = strings.ReplaceAll(body, "<table>\n", "<div class=\"tblwrap\">\n<table>\n")
	body = strings.ReplaceAll(body, "</table>\n", "</table>\n</div>\n")
	return body
}

func decorateArchitecture(body string, headings []headingInfo) string {
	var toc strings.Builder
	toc.WriteString("<nav class=\"toc\" aria-label=\"On this page\">\n<span class=\"toc-label\">On this page</span>\n<ul>\n")
	section := 0
	for _, heading := range headings {
		if heading.Level != 2 {
			continue
		}
		section++
		fmt.Fprintf(&toc, "<li><a href=\"#%s\"><span class=\"n\">%02d</span>%s</a></li>\n",
			template.HTMLEscapeString(heading.ID), section, template.HTMLEscapeString(heading.Text))
	}
	toc.WriteString("</ul>\n</nav>\n")

	firstH2 := strings.Index(body, "<h2 ")
	if firstH2 >= 0 {
		body = body[:firstH2] + toc.String() + body[firstH2:]
	}
	section = 0
	for _, heading := range headings {
		if heading.Level != 2 {
			continue
		}
		section++
		open := fmt.Sprintf("<h2 id=\"%s\">", heading.ID)
		replacement := fmt.Sprintf("<h2 id=\"%s\"><span class=\"secno\" aria-hidden=\"true\">%02d</span>", heading.ID, section)
		body = strings.Replace(body, open, replacement, 1)
	}
	return body
}

func relativeRootPrefix(output string) string {
	rel, err := filepath.Rel(filepath.Dir(output), "docs")
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel) + "/"
}

func trackedFiles(root string) (map[string]bool, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	tracked := map[string]bool{}
	for item := range bytes.SplitSeq(out, []byte{0}) {
		if len(item) > 0 {
			tracked[filepath.ToSlash(string(item))] = true
		}
	}
	return tracked, nil
}

func validateManifest(tracked map[string]bool) error {
	known := map[string]bool{}
	for _, section := range sections {
		known[section.Slug] = true
	}

	declaredSource := map[string]bool{}
	declaredOutput := map[string]bool{hubOutput: true}
	for _, page := range pages {
		if !known[page.Section] {
			return fmt.Errorf("page %s has unknown section %q", page.output(), page.Section)
		}
		if page.NavTitle == "" || page.Summary == "" {
			return fmt.Errorf("page %s needs a NavTitle and a Summary for the handbook index", page.output())
		}
		output := filepath.ToSlash(page.output())
		if declaredOutput[output] {
			return fmt.Errorf("duplicate manifest output %s", output)
		}
		declaredOutput[output] = true

		if page.planned() {
			if page.Source != "" {
				return fmt.Errorf("planned page %s must not declare a Source", output)
			}
			if !tracked[filepath.ToSlash(page.Draft)] {
				return fmt.Errorf("planned page %s has no tracked draft at %s", output, page.Draft)
			}
			if tracked[output] {
				return fmt.Errorf("planned page %s must not have a published file at %s", output, output)
			}
			continue
		}

		source := filepath.ToSlash(page.Source)
		if declaredSource[source] {
			return fmt.Errorf("duplicate manifest source %s", source)
		}
		declaredSource[source] = true
		if !tracked[source] {
			return fmt.Errorf("manifest source is not tracked: %s", source)
		}
		if !tracked[output] {
			return fmt.Errorf("manifest output is not tracked: %s", output)
		}
		if page.Description == "" {
			return fmt.Errorf("published page %s needs a Description", output)
		}
		for _, legacy := range page.Legacy {
			legacy = filepath.ToSlash(legacy)
			if declaredOutput[legacy] {
				return fmt.Errorf("duplicate legacy path %s", legacy)
			}
			declaredOutput[legacy] = true
			if !strings.HasPrefix(legacy, "docs/") {
				return fmt.Errorf("legacy redirect %s is outside the published site tree", legacy)
			}
			if !tracked[legacy] {
				return fmt.Errorf("legacy redirect is not tracked: %s", legacy)
			}
		}
	}

	var undeclared []string
	for path := range tracked {
		if !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
			continue
		}
		if tracked[strings.TrimSuffix(path, ".md")+".html"] && !declaredSource[path] {
			undeclared = append(undeclared, path)
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		return fmt.Errorf("tracked Markdown/HTML twins missing from manifest: %s", strings.Join(undeclared, ", "))
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// artifact pairs a repository-relative output path with its rendered bytes.
type artifact struct {
	path string
	data []byte
}

func (r *siteRenderer) artifacts() ([]artifact, error) {
	out := make([]artifact, 0, len(pages)+8)
	for _, page := range pages {
		if page.planned() {
			continue
		}
		data, err := r.render(page)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", page.Source, err)
		}
		out = append(out, artifact{filepath.ToSlash(page.output()), data})
		for _, legacy := range page.Legacy {
			stub, err := renderRedirect(legacy, page)
			if err != nil {
				return nil, fmt.Errorf("redirect %s: %w", legacy, err)
			}
			out = append(out, artifact{filepath.ToSlash(legacy), stub})
		}
	}
	hub, err := r.renderHub()
	if err != nil {
		return nil, fmt.Errorf("render hub: %w", err)
	}
	if len(r.unresolved) > 0 {
		sort.Strings(r.unresolved)
		return nil, fmt.Errorf("relative links resolve to nothing tracked:\n  %s", strings.Join(r.unresolved, "\n  "))
	}
	return append(out, artifact{hubOutput, hub}), nil
}

func run(root string, check bool) (int, error) {
	tracked, err := trackedFiles(root)
	if err != nil {
		return 0, err
	}
	if err := validateManifest(tracked); err != nil {
		return 0, err
	}
	renderer := newSiteRenderer(root, tracked)
	items, err := renderer.artifacts()
	if err != nil {
		return 0, err
	}
	var stale []string
	for _, item := range items {
		target := filepath.Join(root, filepath.FromSlash(item.path))
		if check {
			got, err := os.ReadFile(target)
			if err != nil {
				return 0, err
			}
			if !bytes.Equal(got, item.data) {
				stale = append(stale, item.path)
			}
			continue
		}
		if err := writeAtomic(target, item.data); err != nil {
			return 0, fmt.Errorf("write %s: %w", item.path, err)
		}
	}
	if len(stale) > 0 {
		return 0, fmt.Errorf("generated HTML is stale: %s; run make docs-html-regen", strings.Join(stale, ", "))
	}
	return len(items), nil
}

func main() {
	check := flag.Bool("check", false, "verify generated HTML without writing files")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}
	count, err := run(absRoot, *check)
	if err != nil {
		fatal(err)
	}
	if *check {
		fmt.Printf("docs-html-check: %d generated file(s) match Markdown sources\n", count)
	} else {
		fmt.Printf("docs-html-regen: generated %d file(s)\n", count)
	}
}

func fatal(err error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
