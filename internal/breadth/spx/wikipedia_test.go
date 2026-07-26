package spx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestParseHTMLFromFixture pins the parser against the same checked-in
// HTML snippet the standalone refresh-spx-members script tests against.
// When Wikipedia's table structure changes, this test breaks at unit-
// test time so both callers (release script and daemon's runtime
// refresher) get the regression at the same place.
func TestParseHTMLFromFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "wikipedia-snippet.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := ParseHTML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"AAPL", "BRK.B", "GOOGL", "MMM", "MSFT", "NVDA"}
	if !slices.Equal(got, want) {
		t.Errorf("symbols:\n  want %v\n  got  %v", want, got)
	}
}

// TestParseHTMLRejectsMissingTable covers the page-restructure failure
// mode: the regex anchor is gone, so we return an explicit error
// rather than a partial/empty list that a caller might silently install.
func TestParseHTMLRejectsMissingTable(t *testing.T) {
	html := []byte(`<html><body><p>Page redesigned, table gone.</p></body></html>`)
	_, err := ParseHTML(html)
	if err == nil {
		t.Fatal("expected error when constituents table is absent")
	}
}

// TestParseHTMLFiltersNonTickers covers two real footguns from live
// Wikipedia state: footnote markers like [1] and UI cruft like "edit".
// Neither should be admitted as tickers.
func TestParseHTMLFiltersNonTickers(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><th>Symbol</th><th>Security</th></tr>
<tr><td><a href="/q">AAPL</a></td><td>Apple</td></tr>
<tr><td><a href="/q">[1]</a></td><td>Footnote</td></tr>
<tr><td><a href="/q">edit</a></td><td>UI cruft</td></tr>
<tr><td><a href="/q">MSFT</a></td><td>Microsoft</td></tr>
</table>`)
	got, err := ParseHTML(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"AAPL", "MSFT"}
	if !slices.Equal(got, want) {
		t.Errorf("filter:\n  want %v\n  got  %v", want, got)
	}
}

// TestParseHTMLDeduplicates covers the rare transient case where the
// same ticker appears twice in the source HTML (edit-conflict state).
func TestParseHTMLDeduplicates(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><td><a>AAPL</a></td></tr>
<tr><td><a>MSFT</a></td></tr>
<tr><td><a>AAPL</a></td></tr>
</table>`)
	got, err := ParseHTML(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(got, []string{"AAPL", "MSFT"}) {
		t.Errorf("dedup: got %v", got)
	}
}

// TestParseHTMLSortsAscending pins the deterministic-output contract.
// Both the release-time generator and the runtime refresher write the
// list verbatim; sorting in the parser keeps the on-disk shape stable
// across days when membership hasn't changed.
func TestParseHTMLSortsAscending(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><td><a>ZTS</a></td></tr>
<tr><td><a>AAPL</a></td></tr>
<tr><td><a>MMM</a></td></tr>
</table>`)
	got, err := ParseHTML(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"AAPL", "MMM", "ZTS"}
	if !slices.Equal(got, want) {
		t.Errorf("sort:\n  want %v\n  got  %v", want, got)
	}
}

// TestUserAgentFormat pins the contact-path string format. Wikipedia
// ops correlate scrapes to projects via the UA; the +URL hint and the
// version segment have to stay readable.
func TestUserAgentFormat(t *testing.T) {
	ua := UserAgent("v0.33.0")
	if !strings.HasPrefix(ua, "canary/v0.33.0 ") {
		t.Errorf("missing canary/<version> prefix: %q", ua)
	}
	if !strings.Contains(ua, "github.com/osauer/canary") {
		t.Errorf("missing project URL: %q", ua)
	}
}

// TestUserAgentEmptyVersionFallback pins the dev-build fallback so an
// unstamped binary (`go run` without ldflags) still presents a valid
// UA rather than `canary/ ` with a blank version.
func TestUserAgentEmptyVersionFallback(t *testing.T) {
	ua := UserAgent("")
	if !strings.HasPrefix(ua, "canary/dev ") {
		t.Errorf("empty version should fall back to dev: %q", ua)
	}
}

// TestFetchAndParseHappyPath exercises the full fetch + parse roundtrip
// against an httptest server. Confirms both that the HTTP layer pulls
// the body and that the User-Agent header lands on the request.
func TestFetchAndParseHappyPath(t *testing.T) {
	body := []byte(`<table id="constituents">
<tr><td><a>AAPL</a></td></tr>
<tr><td><a>MSFT</a></td></tr>
</table>`)
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	symbols, asOf, err := FetchAndParse(context.Background(), srv.URL, "v0.33.0")
	if err != nil {
		t.Fatalf("FetchAndParse: %v", err)
	}
	if !slices.Equal(symbols, []string{"AAPL", "MSFT"}) {
		t.Errorf("symbols: got %v", symbols)
	}
	if asOf.IsZero() {
		t.Error("asOf should be set")
	}
	if !strings.Contains(gotUA, "canary/v0.33.0") {
		t.Errorf("User-Agent not propagated: %q", gotUA)
	}
}

// TestFetchAndParseNon200 confirms an HTTP 4xx/5xx surfaces as an
// error rather than handing back an empty parse result.
func TestFetchAndParseNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, _, err := FetchAndParse(context.Background(), srv.URL, "v0.33.0")
	if err == nil {
		t.Fatal("expected error on HTTP 503")
	}
}

// TestSanityBoundsConstants pins the band's existence and shape so a
// refactor that drops or inverts the bounds doesn't silently let
// nonsense lists land. The exact numbers are documented at the
// constant definitions; this test catches "MinMembers > MaxMembers"
// and "both zeroed" mistakes.
func TestSanityBoundsConstants(t *testing.T) {
	if MinMembers <= 0 || MaxMembers <= 0 {
		t.Fatalf("sanity bounds must be positive: min=%d max=%d", MinMembers, MaxMembers)
	}
	if MinMembers >= MaxMembers {
		t.Fatalf("min must be < max: min=%d max=%d", MinMembers, MaxMembers)
	}
}
