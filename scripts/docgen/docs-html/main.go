// Command docs-html renders the public documentation site from its Markdown
// sources into a build directory. Markdown is the only prose authority;
// generated HTML is never tracked.
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
	publicBaseURL = "https://osauer.dev/canary/"
	// hubOutput is the handbook index. The site root is docs/, so the public
	// /canary/docs/ prefix lands at docs/docs/ on disk.
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
		Blurb: "Install Canary, point it at a gateway and an MCP host, and confirm the desk is live.",
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
		Source:      "docs/docs/start/install.md",
		Section:     "start",
		NavTitle:    "Install and first run",
		Summary:     "Prerequisites, the install paths, and the first commands that prove the gateway is reachable.",
		Description: "What has to be running before Canary connects, which install path to choose on macOS or Linux, how to read the status health check, and where the binary, config, state, and logs are written.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/hosts.md",
		Section:     "start",
		NavTitle:    "Connect an MCP host",
		Summary:     "Wiring Claude Desktop, Claude Code, Cursor, and Zed to the local server, and checking the connection.",
		Description: "How to point Claude Desktop, Claude Code, Cursor, Zed, or any stdio MCP host at the local Canary server, confirm the connection, and see what the agent gets.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/your-llm.md",
		Section:     "start",
		NavTitle:    "Connect the LLM you already use",
		Summary:     "Connect an Interactive Brokers account to Claude, Cursor or another MCP host, and where the data goes.",
		Description: "How to connect an Interactive Brokers account to the LLM you already use: Claude Desktop, Claude Code, Cursor, Zed or another MCP host, where your account data goes, and what to ask.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/agentic-trading.md",
		Section:     "start",
		NavTitle:    "Agentic trading",
		Summary:     "What an AI agent can do on an Interactive Brokers account, and what it takes to send an order.",
		Description: "How to connect an AI agent to an Interactive Brokers account with Canary, what the agent can read and watch, and why every order stays your decision.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/first-session.md",
		Section:     "start",
		NavTitle:    "Your first session",
		Summary:     "A guided half hour: read the account, run the brief, inspect the rulebook, and interpret what comes back.",
		Description: "A seven-step walkthrough of a first Canary session: gateway health, account, positions, the daily brief, rulebook and policy reads, named-symbol technicals, and the same reads through an agent.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/ibkr-hong-kong-singapore.md",
		Section:     "start",
		NavTitle:    "Hong Kong and Singapore accounts",
		Summary:     "The same gateway steps, base currency in HKD, SGD or yen, the Asian exchange calendars, and when to run the brief.",
		Description: "What is different when an Interactive Brokers account sits in Hong Kong, Singapore, Japan or Australia: identical IB Gateway steps, how HKD, SGD and yen figures render, the SEHK, SGX, Tokyo, ASX and Stock Connect calendars, and the brief's session at an Asian morning.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/start/updating.md",
		Section:     "start",
		NavTitle:    "Updating",
		Summary:     "Update the binary, the Desktop extension, S&P 500 membership, calendars, and local process state.",
		Description: "How to update the Canary binary, Desktop extension, S&P 500 membership, calendars, and local process state.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/updating.html"},
	},
	{
		Source:      "docs/docs/start/troubleshooting.md",
		Section:     "start",
		NavTitle:    "Troubleshooting",
		Summary:     "Gateway not reachable, stale quotes, a daemon that will not start, and the logs that answer each one.",
		Description: "Symptom-first fixes for Canary: gateway discovery and handshake errors, a daemon that will not start, delayed or frozen quotes, breadth and gamma states, MCP host wiring, and where the logs live.",
		Status:      statusPublished,
	},

	// ---- Operate -----------------------------------------------------------
	{
		Source:      "docs/docs/operate/daily-desk.md",
		Section:     "operate",
		NavTitle:    "The daily desk",
		Summary:     "A working routine from the morning brief through discipline and protection to the after-close read.",
		Description: "The recurring Canary trading-day loop in time order: which command to run before the open, how to read the brief's row statuses, what to leave watching during the session, and which checks the daemon already absorbs.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/agents.md",
		Section:     "operate",
		NavTitle:    "Working with agents",
		Summary:     "Workflows, limits, and worked examples for driving Canary from an MCP host.",
		Description: "Practical workflows, limits, and examples for using Canary through an MCP host.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/agentic-use.html"},
	},
	{
		Source:      "docs/docs/operate/app.md",
		Section:     "operate",
		NavTitle:    "The paired app",
		Summary:     "Running canary app, pairing a phone, the event stream, and opt-in push notifications.",
		Description: "How the canary app process serves the paired PWA, owns pairing, streams events, and sends opt-in push notifications.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/alerts.md",
		Section:     "operate",
		NavTitle:    "Alerts and notifications",
		Summary:     "What raises an alert, where it is delivered, how delivery is proven, and how to tune the noise down.",
		Description: "The nine daemon producers that can raise a Canary alert, the single paired-app inbox they share, the three notification modes and the per-source delivery gate, and what a push-service acceptance does and does not prove.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/orders.md",
		Section:     "operate",
		NavTitle:    "Constrained orders and the trading build",
		Summary:     "The read-only default, the constrained order lifecycle that remains, and the separate opt-in trading build.",
		Description: "The read-only default in Canary, which constrained order surfaces remain, and how the separate experimental trading build is configured.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/protection.md",
		Section:     "operate",
		NavTitle:    "Protection and risk reduction",
		Summary:     "Trailing-stop and risk-reduction proposals, per-row blockers, and how a proposal becomes an order.",
		Description: "How Canary generates protection proposals, why a row blocks, and how to fix a protective stop that no longer matches its position.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/cash.md",
		Section:     "operate",
		NavTitle:    "Cash management",
		Summary:     "The cash sweep and currency leveling: their own policy section, their own authority, and how each order is bounded.",
		Description: "How Canary's cash sweep buys same-currency bills with idle cash and how currency leveling repays a borrowed currency, under the policy's own [cash] section and authority.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/operate/reconciliation.md",
		Section:     "operate",
		NavTitle:    "Reconciliation",
		Summary:     "Matching broker statement flows against the declared capital ledger, and handling the lines that will not match.",
		Description: "How canary recon matches IBKR Flex statement flows against declared capital events, how to read each exception category, and which parts of the loop run without a person.",
		Status:      statusPublished,
	},

	// ---- Understand --------------------------------------------------------
	{
		Source:      "docs/docs/understand/concepts.md",
		Section:     "understand",
		NavTitle:    "Concepts",
		Summary:     "Calendars, regime, stress, market events, protective stops, gamma, and breadth in one mental model.",
		Description: "What the load-bearing market, portfolio, and data-quality context surfaces measure, and how to read them without mis-acting on the output.",
		Layout:      "architecture",
		Status:      statusPublished,
		Legacy:      []string{"docs/concepts.html"},
	},
	{
		Source:      "docs/docs/understand/edge.md",
		Section:     "understand",
		NavTitle:    "Canary Edge",
		Summary:     "Broker-truth account P/L, fixed-horizon decision price impact, option actuals, and the coverage that bounds every result.",
		Description: "How Canary Edge reconstructs IBKR portfolio history, calculates account P/L and stock or ETF decision price impact, reports option actuals, and fails closed when evidence is incomplete.",
		Layout:      "architecture",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/understand/sensors.md",
		Section:     "understand",
		NavTitle:    "Sensors",
		Summary:     "How each measurement establishes authority and freshness, and how a gap fails closed instead of guessing.",
		Description: "How Canary Gamma, Regime, Stress, Rulebook, and market-event sensors establish authority, freshness, last-good context, and fail-closed data quality.",
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
		Description: "Who decides trading policy in Canary, what the daemon does today, how controls change, and what remains an explicit human decision.",
		Layout:      "architecture",
		SocialImage: "diagrams/policy-lifecycle.png",
		Status:      statusPublished,
		Legacy:      []string{"docs/policies.html"},
	},
	{
		Source:      "docs/docs/understand/rulebook.md",
		Section:     "understand",
		NavTitle:    "The rulebook",
		Summary:     "The advisory rules, what each one is protecting against, and how a breach is reported.",
		Description: "What each advisory rulebook check protects against, why a rule that cannot get clean data never passes, and why a clean run is not permission to trade.",
		Layout:      "architecture",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/understand/risk-policy.md",
		Section:     "understand",
		NavTitle:    "Writing a risk policy",
		Summary:     "Turning a personal risk mandate into the policy file: limits, drawdown ladder, overrides, and review.",
		Description: "How to write the Canary personal risk policy file: where it lives, what each section governs, what the schema refuses, and how a revision becomes effective.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/understand/market-data.md",
		Section:     "understand",
		NavTitle:    "Market data and entitlements",
		Summary:     "Which subscriptions produce real-time data, what delayed and frozen mean, and how freshness is reported.",
		Description: "How market data reaches Canary through your own TWS or IB Gateway session, what each data type supports, which surfaces need which entitlement, and what works with none.",
		Layout:      "architecture",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/understand/glossary.md",
		Section:     "understand",
		NavTitle:    "Glossary",
		Summary:     "One place for the terms the other pages assume: NLV, R-multiple, zero gamma, last-good, fingerprint.",
		Description: "Alphabetical definitions of the broker, risk, options, market-state, and system terms the Canary handbook uses, each linked to the page that owns it.",
		Status:      statusPublished,
	},

	// ---- Reference ---------------------------------------------------------
	{
		Source:      "docs/docs/reference/cli.md",
		Section:     "reference",
		NavTitle:    "CLI",
		Summary:     "Every Canary subcommand with its flags and usage, generated from the command registry in the binary.",
		Description: "Generated reference for every Canary subcommand, with its usage line, flags, subcommands, guard class, and MCP counterpart.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/reference/mcp-tools.md",
		Section:     "reference",
		NavTitle:    "MCP tools",
		Summary:     "Every tool exposed by canary mcp, with parameters and invocation guidance. Generated from the registry.",
		Description: "Generated reference for every tool exposed by canary mcp, including parameters and invocation guidance.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/mcp-tools.html"},
	},
	{
		Source:      "docs/docs/reference/config.md",
		Section:     "reference",
		NavTitle:    "Configuration",
		Summary:     "TOML configuration, policy files, runtime platform settings, and environment variables.",
		Description: "Generated reference for Canary TOML configuration, policy files, runtime platform settings, and environment variables.",
		Status:      statusPublished,
		Legacy:      []string{"docs/reference/config.html"},
	},
	{
		Source:      "docs/docs/reference/edge-flex.md",
		Section:     "reference",
		NavTitle:    "Reporting Flex query",
		Summary:     "The exact shared Flex sections and fields Canary validates, generated from the parser manifest.",
		Description: "Generated reference for the canonical IBKR Activity Flex Query shared by reconciliation, statement-derived equity, and Canary Edge.",
		Status:      statusPublished,
	},
	{
		Source:      "docs/docs/reference/releases.md",
		Section:     "reference",
		NavTitle:    "Releases and support",
		Summary:     "Version scheme, what a release contains, how signatures are verified, and which versions are supported.",
		Description: "What each Canary release publishes, how the read-only and trading builds differ, how to verify a download against the maintainer's signing key, and how to report a security issue.",
		Status:      statusPublished,
	},

	// ---- Under the hood ----------------------------------------------------
	{
		Source:      "docs/docs/internals/architecture.md",
		Section:     "internals",
		NavTitle:    "Architecture",
		Summary:     "Runtime processes, typed contracts, external data flows, state ownership, and deployment boundaries.",
		Description: "System architecture, protocols, external data flows, process boundaries, and local persistence for Canary.",
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
		Description: "How the Canary daemon preserves state and evidence with SQLite, why the model exists, how data moves, and where the design must evolve.",
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
		Summary:     "Source quality, cluster logic, lifecycle decisions, and the evidence required before policy changes ship.",
		Description: "Contract for the broad-market risk dashboard, source quality, cluster logic, lifecycle decisions, and policy evidence.",
		Status:      statusPublished,
		Legacy:      []string{"docs/specs/risk-regime-dashboard.html"},
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
		Description: "How Canary is packaged and distributed: the plugin, the Claude Desktop bundle, directory metadata, signing, and the read-only boundary each channel ships with.",
		Status:      statusPublished,
		Legacy:      []string{"docs/guides/marketplace-readiness.html"},
	},
}

// navItems is the one definition of the site navigation. The generated pages
var navItems = []struct{ Label, Href string }{
	{"Documentation", hubHref},
	{"MCP tools", "docs/reference/mcp-tools.html"},
	{"Remote access", "canary-remote/"},
	{"Feedback", "feedback/"},
	{"GitHub", "https://github.com/osauer/canary"},
}

type headingInfo struct {
	Level int
	ID    string
	Text  string
}

type templateData struct {
	Lang            string
	Title           string
	Description     string
	Canonical       string
	RootPrefix      string
	Layout          string
	GeneratorNotice template.HTML
	// Alternates holds the hreflang links of a page that exists in more than
	// one language; empty otherwise, so lang.js has nothing to switch to.
	Alternates template.HTML
	Nav        template.HTML
	SideNav    template.HTML
	Body       template.HTML
	JSONLD     template.JS
	SocialHead template.HTML
	HubHref    string
	HomeHref   string
	UI         uiStrings
}

var documentTemplate = template.Must(template.New("document").Parse(`<!doctype html>
{{.GeneratorNotice}}
<html lang="{{.Lang}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} | Canary</title>
  <meta name="description" content="{{.Description}}">
  <link rel="canonical" href="{{.Canonical}}">
{{.Alternates}}  <link rel="icon" type="image/png" href="{{.RootPrefix}}social/canary-icon.png">
{{.SocialHead}}
  <script type="application/ld+json">{{.JSONLD}}</script>
  <link rel="stylesheet" href="{{.RootPrefix}}shared.css">
  <script src="{{.RootPrefix}}lang.js"></script>
</head>
<body class="docpage layout-{{.Layout}}">
  <div class="topline"></div>
  <header class="wrap nav" aria-label="Primary">
    <a class="brand" href="{{.RootPrefix}}{{.HomeHref}}" aria-label="Canary home"><img src="{{.RootPrefix}}social/canary-icon.png" width="192" height="192" alt="">Canary</a>
{{.Nav}}  </header>
  <div class="wrap shell">
{{.SideNav}}    <main class="doc">
{{.Body}}    </main>
  </div>
  <footer>
    <div class="wrap"><a href="{{.RootPrefix}}{{.HomeHref}}">Canary</a><a href="{{.RootPrefix}}{{.HubHref}}">{{.UI.FooterDocs}}</a><a href="https://github.com/osauer/canary">GitHub</a><a href="https://github.com/osauer/canary/blob/main/PRIVACY.md">{{.UI.FooterPrivacy}}</a><a href="https://github.com/osauer/canary/blob/main/SECURITY.md">{{.UI.FooterSecurity}}</a></div>
    <div class="wrap fineprint">{{.UI.Fineprint}}</div>
  </footer>
</body>
</html>
`))

// The notice is passed as data rather than written inline: html/template
var redirectTemplate = template.Must(template.New("redirect").Parse(`<!doctype html>
{{.Notice}}
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex">
  <meta http-equiv="refresh" content="0; url={{.Target}}">
  <link rel="canonical" href="{{.Canonical}}">
  <title>{{.Title}} | Canary</title>
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
	// english maps an English source path to its manifest entry;
	// translations holds every translated page by language and source.
	english      map[string]pageSpec
	translations translationSet
	// landing holds the landing page's string catalogue by language slug.
	landing map[string]*landingCatalogue
	// unresolved collects relative links that point at nothing tracked, so the
	// run can fail with the whole list rather than one at a time.
	unresolved []string
}

func newSiteRenderer(root string, tracked map[string]bool) (*siteRenderer, error) {
	generated := make(map[string]string, len(pages))
	english := make(map[string]pageSpec, len(pages))
	for _, page := range pages {
		if page.planned() {
			continue
		}
		generated[filepath.ToSlash(page.Source)] = filepath.ToSlash(page.output())
		english[filepath.ToSlash(page.Source)] = page
	}
	r := &siteRenderer{
		root:      root,
		generated: generated,
		tracked:   tracked,
		english:   english,
		markdown: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			// Attributes let a translated heading keep the English anchor
			// (`## 開始之前 {#before-you-start}`): the automatic id of a
			// Chinese heading is the bare word "heading", which no cross-page
			// link can reach.
			goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithAttribute()),
		),
	}
	translations, err := discoverTranslations(tracked, english, r.read)
	if err != nil {
		return nil, err
	}
	r.translations = translations
	if err := r.loadLanding(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *siteRenderer) read(repoPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.root, filepath.FromSlash(repoPath)))
}

// pageVersions lists the output path of a page in every language it exists
// in, keyed by language tag; english is the English source path, or "" for
// the hub. A page that exists in one language only gets no hreflang links.
func (r *siteRenderer) pageVersions(englishSource string) map[string]string {
	versions := map[string]string{}
	for _, lang := range languages {
		if englishSource == "" {
			versions[lang.Tag] = lang.hubOutput()
			continue
		}
		if lang.isEnglish() {
			versions[lang.Tag] = r.generated[englishSource]
		} else if t := r.translations.lookup(lang, englishSource); t != nil {
			versions[lang.Tag] = t.output()
		}
	}
	return versions
}

// alternatesHTML renders the hreflang links that let a search engine send a
// reader straight to the page in their language. x-default is English, the
// master.
// socialHeadHTML is a handbook page's link preview: what a chat app or social
// site shows when the page is shared, in the page's language.
func socialHeadHTML(lang *langSpec, page pageSpec, title, description, canonical string, versions map[string]string) string {
	// A page without a diagram of its own still previews, so a shared link
	// never renders as a bare URL.
	image := page.SocialImage
	if image == "" {
		image = lang.SocialImage
	}
	esc := template.HTMLEscapeString
	fullTitle, description, imageURL := esc(title+" | Canary"), esc(description), esc(publicBaseURL+image)
	var out strings.Builder
	fmt.Fprintf(&out, "  <meta property=\"og:type\" content=\"article\">\n")
	fmt.Fprintf(&out, "  <meta property=\"og:site_name\" content=\"Canary\">\n")
	out.WriteString(localeTagsHTML(lang, versions))
	fmt.Fprintf(&out, "  <meta property=\"og:url\" content=\"%s\">\n", esc(canonical))
	fmt.Fprintf(&out, "  <meta property=\"og:title\" content=\"%s\">\n", fullTitle)
	fmt.Fprintf(&out, "  <meta property=\"og:description\" content=\"%s\">\n", description)
	fmt.Fprintf(&out, "  <meta property=\"og:image\" content=\"%s\">\n", imageURL)
	fmt.Fprintf(&out, "  <meta name=\"twitter:card\" content=\"summary_large_image\">\n")
	fmt.Fprintf(&out, "  <meta name=\"twitter:title\" content=\"%s\">\n", fullTitle)
	fmt.Fprintf(&out, "  <meta name=\"twitter:description\" content=\"%s\">\n", description)
	fmt.Fprintf(&out, "  <meta name=\"twitter:image\" content=\"%s\">", imageURL)
	return out.String()
}

// localeTagsHTML names the page's Open Graph locale and the languages it also
// exists in.
func localeTagsHTML(lang *langSpec, versions map[string]string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "  <meta property=\"og:locale\" content=\"%s\">\n", lang.OGLocale)
	for _, other := range languages {
		if _, ok := versions[other.Tag]; ok && other != lang {
			fmt.Fprintf(&out, "  <meta property=\"og:locale:alternate\" content=\"%s\">\n", other.OGLocale)
		}
	}
	return out.String()
}

func alternatesHTML(versions map[string]string) string {
	if len(versions) < 2 {
		return ""
	}
	var out strings.Builder
	for _, lang := range languages {
		output, ok := versions[lang.Tag]
		if !ok {
			continue
		}
		fmt.Fprintf(&out, "  <link rel=\"alternate\" hreflang=\"%s\" href=\"%s\">\n", lang.Tag, template.HTMLEscapeString(siteURL(output)))
	}
	fmt.Fprintf(&out, "  <link rel=\"alternate\" hreflang=\"x-default\" href=\"%s\">\n", template.HTMLEscapeString(siteURL(versions[english.Tag])))
	return out.String()
}

// siteURL is the public URL of a repository output path. Directory indexes
// publish as the directory.
func siteURL(output string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(output), "docs/")
	return publicBaseURL + strings.TrimSuffix(rel, "index.html")
}

// navHTML renders the shared navigation for a page, with the language
// selector after it. versions says where this page exists in each language;
// a language the page is missing in links to that language's hub, so the
// selector is always complete and never a dead end.
func navHTML(lang *langSpec, rootPrefix, current string, versions map[string]string) (string, error) {
	currentDir := filepath.Dir(current)
	var out strings.Builder
	out.WriteString("    <nav class=\"nav-links\" aria-label=\"Site\">\n")
	for i, item := range navItems {
		href := item.Href
		if i == 0 {
			href = lang.hubHref()
		}
		if !strings.Contains(href, "://") {
			href = rootPrefix + href
		}
		label := item.Label
		if i < len(lang.ui.Nav) {
			label = lang.ui.Nav[i]
		}
		fmt.Fprintf(&out, "      <a href=\"%s\">%s</a>\n", template.HTMLEscapeString(href), template.HTMLEscapeString(label))
	}
	out.WriteString("    </nav>\n")
	selector, err := langLinksHTML(lang, currentDir, versions)
	if err != nil {
		return "", err
	}
	out.WriteString(selector)
	return out.String(), nil
}

// langLinksHTML renders the language selector for a page in currentDir.
func langLinksHTML(lang *langSpec, currentDir string, versions map[string]string) (string, error) {
	var out strings.Builder
	out.WriteString("    <nav class=\"lang-links\" aria-label=\"Language\">\n")
	for _, other := range languages {
		target, ok := versions[other.Tag]
		if !ok {
			target = other.hubOutput()
		}
		href, err := filepath.Rel(currentDir, filepath.ToSlash(target))
		if err != nil {
			return "", err
		}
		href = filepath.ToSlash(href)
		if strings.HasSuffix(href, "index.html") {
			href = strings.TrimSuffix(href, "index.html")
			if href == "" {
				href = "./"
			}
		}
		mark := ""
		if other.Tag == lang.Tag {
			mark = " aria-current=\"true\""
		}
		fmt.Fprintf(&out, "      <a href=\"%s\" hreflang=\"%s\" lang=\"%s\" data-lang=\"%s\"%s>%s</a>\n",
			template.HTMLEscapeString(href), other.Tag, other.Tag, other.Tag, mark, template.HTMLEscapeString(other.Name))
	}
	out.WriteString("    </nav>\n")
	return out.String(), nil
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
//
// In a translated language the tree links the translated page where one
// exists and the English page otherwise, marked EN, so the tree always shows
// the whole handbook.
func (r *siteRenderer) sideNavHTML(lang *langSpec, rootPrefix, current string) (string, error) {
	currentDir := filepath.Dir(current)

	here, hereTitle := "", ""
	for _, page := range pages {
		if page.planned() {
			continue
		}
		source := filepath.ToSlash(page.Source)
		output := filepath.ToSlash(page.output())
		if t := r.translations.lookup(lang, source); t != nil {
			output = t.output()
		}
		if output == current {
			here = page.Section
		}
	}
	if here != "" {
		hereTitle = lang.section(here).Title
	}

	var out strings.Builder
	out.WriteString("    <details class=\"sidenav\">\n")
	out.WriteString("      <summary class=\"sidenav-summary\">" + template.HTMLEscapeString(lang.ui.Documentation))
	if hereTitle != "" {
		// The closed control on a phone says which section the reader is in,
		// so the tree does not have to be opened to answer "where am I".
		fmt.Fprintf(&out, "<span class=\"sidenav-here\">%s</span>", template.HTMLEscapeString(hereTitle))
	}
	out.WriteString("</summary>\n")
	fmt.Fprintf(&out, "      <nav class=\"sidenav-tree\" aria-label=\"%s\">\n", template.HTMLEscapeString(lang.ui.Documentation))

	overview, overviewMark := "sidenav-overview", ""
	if current == lang.hubOutput() {
		overview += " is-current"
		overviewMark = " aria-current=\"page\""
	}
	fmt.Fprintf(&out, "        <a class=\"%s\" href=\"%s\"%s>%s</a>\n",
		overview, template.HTMLEscapeString(rootPrefix+lang.hubHref()), overviewMark, template.HTMLEscapeString(lang.ui.Overview))

	out.WriteString("        <ul class=\"sidenav-sections\">\n")
	for _, section := range sections {
		class, open := "sidenav-section", ""
		if section.Slug == here {
			class, open = "sidenav-section is-here", " open"
		}
		fmt.Fprintf(&out, "          <li class=\"%s\">\n", class)
		fmt.Fprintf(&out, "            <details class=\"sidenav-group\"%s>\n", open)
		fmt.Fprintf(&out, "              <summary class=\"sidenav-heading\">%s</summary>\n", template.HTMLEscapeString(lang.section(section.Slug).Title))
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
			englishOnly := ""
			if t := r.translations.lookup(lang, filepath.ToSlash(page.Source)); t != nil {
				output = t.output()
				title = template.HTMLEscapeString(t.navTitle)
			} else if !lang.isEnglish() {
				englishOnly = fmt.Sprintf("<span class=\"sidenav-lang\" lang=\"en\">%s</span>", template.HTMLEscapeString(lang.ui.EnglishOnly))
			}
			href, err := filepath.Rel(currentDir, output)
			if err != nil {
				return "", err
			}
			mark := ""
			if output == current {
				mark = " class=\"is-current\" aria-current=\"page\""
			}
			fmt.Fprintf(&out, "                <li><a href=\"%s\"%s>%s%s</a></li>\n",
				template.HTMLEscapeString(filepath.ToSlash(href)), mark, title, englishOnly)
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
			fmt.Fprintf(&out, "        <p class=\"sidenav-note\">%s</p>\n", template.HTMLEscapeString(lang.ui.DimmedNote))
			break
		}
	}
	out.WriteString("      </nav>\n")
	out.WriteString("    </details>\n")
	return out.String(), nil
}

// render builds the English page.
func (r *siteRenderer) render(page pageSpec) ([]byte, error) {
	source, err := r.read(filepath.ToSlash(page.Source))
	if err != nil {
		return nil, err
	}
	return r.renderDoc(english, page, nil, source)
}

// renderTranslation builds one translated page from its Markdown body.
func (r *siteRenderer) renderTranslation(t *translation) ([]byte, error) {
	return r.renderDoc(t.lang, t.page, t, t.body)
}

// renderDoc renders a page's Markdown in one language. page is always the
// English manifest entry; t is nil for the English page itself. Relative links
// in the Markdown are written against the English source in both cases, so a
// translator copies them verbatim.
func (r *siteRenderer) renderDoc(lang *langSpec, page pageSpec, t *translation, source []byte) ([]byte, error) {
	englishSource := filepath.ToSlash(page.Source)
	output := filepath.ToSlash(page.output())
	description := page.Description
	label := englishSource
	if t != nil {
		output = t.output()
		description = t.description
		label = t.source
	}

	doc := r.markdown.Parser().Parse(text.NewReader(source))
	headings, err := r.transform(doc, source, page, lang, output)
	if err != nil {
		return nil, err
	}
	if len(headings) == 0 || headings[0].Level != 1 || headings[0].Text == "" {
		return nil, fmt.Errorf("%s must start with one H1 title", label)
	}
	for _, heading := range headings[1:] {
		if heading.Level == 1 {
			return nil, fmt.Errorf("%s has more than one H1", label)
		}
	}

	var body bytes.Buffer
	if err := r.markdown.Renderer().Render(&body, source, doc); err != nil {
		return nil, err
	}
	bodyHTML := wrapTables(body.String())
	if page.Layout == "architecture" {
		bodyHTML = decorateArchitecture(bodyHTML, headings, lang.ui.OnThisPage)
	}
	versions := r.pageVersions(englishSource)
	if t != nil {
		note, err := translationNoteHTML(t, output, versions[english.Tag])
		if err != nil {
			return nil, err
		}
		bodyHTML = insertAfterH1(bodyHTML, note)
	}

	canonical := siteURL(output)
	rootPrefix := relativeRootPrefix(output)

	ld := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "TechArticle",
		"headline":    headings[0].Text,
		"description": description,
		"url":         canonical,
		"inLanguage":  lang.Tag,
		"author": map[string]string{
			"@type": "Person",
			"name":  "Oliver Sauer",
			"url":   "https://github.com/osauer",
		},
		"isPartOf": map[string]string{
			"@type": "SoftwareApplication",
			"name":  "Canary",
			"url":   publicBaseURL,
		},
	}
	if t != nil {
		ld["translationOfWork"] = map[string]string{
			"@type": "TechArticle",
			"url":   siteURL(versions[english.Tag]),
		}
	}
	jsonLD, err := json.Marshal(ld)
	if err != nil {
		return nil, err
	}

	layout := page.Layout
	if layout == "" {
		layout = "standard"
	}
	sideNav, err := r.sideNavHTML(lang, rootPrefix, output)
	if err != nil {
		return nil, err
	}
	nav, err := navHTML(lang, rootPrefix, output, versions)
	if err != nil {
		return nil, err
	}
	data := templateData{
		Lang:            lang.Tag,
		Title:           headings[0].Text,
		Description:     description,
		Canonical:       canonical,
		RootPrefix:      rootPrefix,
		Layout:          layout,
		GeneratorNotice: generatorNotice,
		Alternates:      template.HTML(alternatesHTML(versions)),
		Nav:             template.HTML(nav),
		SideNav:         template.HTML(sideNav),
		Body:            template.HTML(bodyHTML), // Goldmark escapes source HTML by default.
		JSONLD:          template.JS(jsonLD),     // json.Marshal produces valid script data.
		HubHref:         lang.hubHref(),
		HomeHref:        lang.homeHref(),
		UI:              lang.ui,
	}
	data.SocialHead = template.HTML(socialHeadHTML(lang, page, data.Title, data.Description, canonical, versions))

	var out bytes.Buffer
	if err := documentTemplate.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// translationNoteHTML is the line under a translated page's title: which
// version is authoritative, and, when the English source has moved on since
// the translation was made, that the page may be behind. The link carries
// data-lang so lang.js records the reader's choice instead of sending them
// back.
func translationNoteHTML(t *translation, output, englishOutput string) (string, error) {
	href, err := filepath.Rel(filepath.Dir(output), englishOutput)
	if err != nil {
		return "", err
	}
	class, text := "translation-note", t.lang.ui.Translated
	if t.stale {
		class, text = "translation-note is-stale", t.lang.ui.Stale
	}
	return fmt.Sprintf("<p class=\"%s\">%s<a href=\"%s\" hreflang=\"en\" data-lang=\"en\">%s</a></p>\n",
		class, template.HTMLEscapeString(text), template.HTMLEscapeString(filepath.ToSlash(href)), template.HTMLEscapeString(t.lang.ui.ReadEnglish)), nil
}

func insertAfterH1(body, note string) string {
	end := strings.Index(body, "</h1>")
	if end < 0 {
		return note + body
	}
	end += len("</h1>\n")
	if end > len(body) {
		end = len(body)
	}
	return body[:end] + note + body[end:]
}

const generatorNotice = template.HTML("<!-- Generated from Markdown by scripts/docgen/docs-html. DO NOT EDIT. -->")

const hubDescription = "The Canary handbook: install and first run, the daily desk routine, how to read each measurement, generated command and tool references, and the system design underneath."

// renderHub builds the handbook index of one language from the manifest.
// Planned pages appear while only finished pages are reachable and indexable.
// A translated hub lists every page: translated ones under their translated
// title, the rest under the English title with an EN mark, linking to the
// English page.
func (r *siteRenderer) renderHub(lang *langSpec) ([]byte, error) {
	output := lang.hubOutput()
	rootPrefix := relativeRootPrefix(output)
	hubDir := filepath.Dir(filepath.ToSlash(output))

	var body strings.Builder
	fmt.Fprintf(&body, "<h1>%s</h1>\n", template.HTMLEscapeString(lang.ui.Documentation))
	fmt.Fprintf(&body, "<p class=\"hub-lead\">%s</p>\n", template.HTMLEscapeString(lang.ui.HubLead))

	for _, section := range sections {
		localised := lang.section(section.Slug)
		fmt.Fprintf(&body, "<section class=\"hub-section\" id=\"%s\">\n", section.Slug)
		fmt.Fprintf(&body, "<h2>%s</h2>\n", template.HTMLEscapeString(localised.Title))
		fmt.Fprintf(&body, "<p class=\"hub-blurb\">%s</p>\n", template.HTMLEscapeString(localised.Blurb))
		body.WriteString("<ul class=\"hub-list\">\n")
		for _, page := range pages {
			if page.Section != section.Slug {
				continue
			}
			title := template.HTMLEscapeString(page.NavTitle)
			summary := template.HTMLEscapeString(page.Summary)
			if page.planned() {
				fmt.Fprintf(&body, "<li class=\"hub-item is-planned\"><span class=\"hub-title\">%s</span><span class=\"hub-summary\">%s</span><span class=\"hub-flag\">%s</span></li>\n", title, summary, template.HTMLEscapeString(lang.ui.NotWrittenYet))
				continue
			}
			target := filepath.ToSlash(page.output())
			englishOnly := ""
			if t := r.translations.lookup(lang, filepath.ToSlash(page.Source)); t != nil {
				target = t.output()
				title = template.HTMLEscapeString(t.navTitle)
				summary = template.HTMLEscapeString(t.summary)
			} else if !lang.isEnglish() {
				englishOnly = fmt.Sprintf("<span class=\"hub-lang\" lang=\"en\">%s</span>", template.HTMLEscapeString(lang.ui.EnglishOnly))
			}
			href, err := filepath.Rel(hubDir, target)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&body, "<li class=\"hub-item\"><a href=\"%s\"><span class=\"hub-title\">%s%s</span><span class=\"hub-summary\">%s</span></a></li>\n",
				template.HTMLEscapeString(filepath.ToSlash(href)), title, englishOnly, summary)
		}
		body.WriteString("</ul>\n</section>\n")
	}

	canonical := siteURL(output)
	jsonLD, err := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CollectionPage",
		"name":        "Canary documentation",
		"description": lang.ui.HubDescription,
		"url":         canonical,
		"inLanguage":  lang.Tag,
		"isPartOf": map[string]string{
			"@type": "SoftwareApplication",
			"name":  "Canary",
			"url":   publicBaseURL,
		},
	})
	if err != nil {
		return nil, err
	}

	sideNav, err := r.sideNavHTML(lang, rootPrefix, output)
	if err != nil {
		return nil, err
	}
	versions := r.pageVersions("")
	nav, err := navHTML(lang, rootPrefix, output, versions)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	err = documentTemplate.Execute(&out, templateData{
		Lang:            lang.Tag,
		Title:           lang.ui.Documentation,
		Description:     lang.ui.HubDescription,
		Canonical:       canonical,
		RootPrefix:      rootPrefix,
		Layout:          "hub",
		GeneratorNotice: generatorNotice,
		Alternates:      template.HTML(alternatesHTML(versions)),
		Nav:             template.HTML(nav),
		SideNav:         template.HTML(sideNav),
		Body:            template.HTML(body.String()),
		JSONLD:          template.JS(jsonLD),
		HubHref:         lang.hubHref(),
		HomeHref:        lang.homeHref(),
		UI:              lang.ui,
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

// transform collects the headings and rewrites links. lang and output say
// which language and output path the page renders to, so a translated page's
// links reach translated targets where they exist.
func (r *siteRenderer) transform(doc ast.Node, source []byte, page pageSpec, lang *langSpec, output string) ([]headingInfo, error) {
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
			n.Destination = r.rewriteDestination(page, lang, output, n.Destination)
		case *ast.Image:
			n.Destination = r.rewriteDestination(page, lang, output, n.Destination)
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
// Links are written against the English source's directory in every language;
// output is where the page being rendered lands, which for a translation is a
// different directory, so each link is re-based onto it. A translated page
// prefers the translated version of a linked page when one exists.
func (r *siteRenderer) rewriteDestination(page pageSpec, lang *langSpec, output string, destination []byte) []byte {
	raw := string(destination)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Path == "" || strings.HasPrefix(raw, "#") {
		return destination
	}

	sourceDir := filepath.Dir(filepath.ToSlash(page.Source))
	outputDir := filepath.Dir(filepath.ToSlash(output))
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(sourceDir, filepath.FromSlash(parsed.Path))))

	if target, ok := r.generated[resolved]; ok {
		if t := r.translations.lookup(lang, resolved); t != nil {
			target = t.output()
		}
		if rel, err := filepath.Rel(outputDir, target); err == nil {
			parsed.Path = filepath.ToSlash(rel)
			return []byte(parsed.String())
		}
	}
	if strings.HasPrefix(resolved, "docs/") && !strings.HasSuffix(resolved, ".md") && r.tracked[resolved] {
		if outputDir == sourceDir {
			return destination
		}
		if rel, err := filepath.Rel(outputDir, resolved); err == nil {
			parsed.Path = filepath.ToSlash(rel)
			return []byte(parsed.String())
		}
	}
	if r.tracked[resolved] {
		github := &url.URL{
			Scheme:   "https",
			Host:     "github.com",
			Path:     "/osauer/canary/blob/main/" + resolved,
			RawQuery: parsed.RawQuery,
			Fragment: parsed.Fragment,
		}
		return []byte(github.String())
	}
	// A relative link that resolves to nothing tracked is dead on the site.
	r.unresolved = append(r.unresolved, fmt.Sprintf("%s -> %s", page.Source, raw))
	return destination
}

func wrapTables(body string) string {
	body = strings.ReplaceAll(body, "<table>\n", "<div class=\"tblwrap\">\n<table>\n")
	body = strings.ReplaceAll(body, "</table>\n", "</table>\n</div>\n")
	return body
}

func decorateArchitecture(body string, headings []headingInfo, onThisPage string) string {
	var toc strings.Builder
	label := template.HTMLEscapeString(onThisPage)
	fmt.Fprintf(&toc, "<nav class=\"toc\" aria-label=\"%s\">\n<span class=\"toc-label\">%s</span>\n<ul>\n", label, label)
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

// validateManifest checks that every source is tracked and every generated
// twin is duplicate authority rather than evidence that a page will ship.
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
		if tracked[output] {
			return fmt.Errorf("generated output must not be tracked: %s", output)
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
			if tracked[legacy] {
				return fmt.Errorf("generated legacy redirect must not be tracked: %s", legacy)
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
		for _, lang := range languages {
			t := r.translations.lookup(lang, filepath.ToSlash(page.Source))
			if t == nil {
				continue
			}
			data, err := r.renderTranslation(t)
			if err != nil {
				return nil, fmt.Errorf("render %s: %w", t.source, err)
			}
			out = append(out, artifact{t.output(), data})
		}
	}
	for _, lang := range languages {
		hub, err := r.renderHub(lang)
		if err != nil {
			return nil, fmt.Errorf("render %s hub: %w", lang.Tag, err)
		}
		out = append(out, artifact{lang.hubOutput(), hub})
	}
	for _, lang := range languages {
		landing, err := r.renderLanding(lang)
		if err != nil {
			return nil, fmt.Errorf("render %s landing page: %w", lang.Tag, err)
		}
		out = append(out, artifact{landingOutput(lang), landing})
	}
	if len(r.unresolved) > 0 {
		sort.Strings(r.unresolved)
		return nil, fmt.Errorf("relative links resolve to nothing tracked:\n  %s", strings.Join(r.unresolved, "\n  "))
	}
	return out, nil
}

// translationsReport lists every translation and whether it is behind its
// English source, for the docs-translations gate.
func translationsReport(root string) (string, int, error) {
	tracked, err := trackedFiles(root)
	if err != nil {
		return "", 0, err
	}
	if err := validateManifest(tracked); err != nil {
		return "", 0, err
	}
	renderer, err := newSiteRenderer(root, tracked)
	if err != nil {
		return "", 0, err
	}
	var out strings.Builder
	behind := renderer.translations.report(&out)
	landingBehind, err := renderer.landingReport(&out)
	if err != nil {
		return "", 0, err
	}
	return out.String(), behind + landingBehind, nil
}

func run(root, outputRoot string) (int, error) {
	tracked, err := trackedFiles(root)
	if err != nil {
		return 0, err
	}
	if err := validateManifest(tracked); err != nil {
		return 0, err
	}
	renderer, err := newSiteRenderer(root, tracked)
	if err != nil {
		return 0, err
	}
	items, err := renderer.artifacts()
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		relative, ok := strings.CutPrefix(filepath.ToSlash(item.path), "docs/")
		if !ok || relative == "" {
			return 0, fmt.Errorf("generated path is outside the site root: %s", item.path)
		}
		target := filepath.Join(outputRoot, filepath.FromSlash(relative))
		if err := writeAtomic(target, item.data); err != nil {
			return 0, fmt.Errorf("write %s: %w", item.path, err)
		}
	}
	return len(items), nil
}

func main() {
	root := flag.String("root", ".", "repository root")
	outputRoot := flag.String("output-root", "", "site build directory (required unless -translations)")
	translations := flag.Bool("translations", false, "list every translation and whether it is behind its English source, then exit")
	flag.Parse()
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}
	if *translations {
		report, _, err := translationsReport(absRoot)
		if err != nil {
			fatal(err)
		}
		fmt.Print(report)
		return
	}
	if *outputRoot == "" {
		fatal(errors.New("-output-root is required"))
	}
	absOutputRoot, err := filepath.Abs(*outputRoot)
	if err != nil {
		fatal(err)
	}
	count, err := run(absRoot, absOutputRoot)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("docs-html-build: generated %d file(s)\n", count)
}

func fatal(err error) {
	if err == nil {
		err = errors.New("unknown error")
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
