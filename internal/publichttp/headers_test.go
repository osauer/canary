package publichttp

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestIdentityProfilesOnWire(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"www.bls.gov", "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)"},
		{"WWW.BLS.GOV", "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)"},
		{"en.wikipedia.org", "Canary-public-feeds/1.0"},
		{"api.nasdaq.com", ""},
		{"fred.stlouisfed.org", "Go-http-client/1.1"},
		{"cdn.cboe.com", "Go-http-client/1.1"},
		{"www.nasdaqtrader.com", "Go-http-client/1.1"},
		{"apps.bea.gov", "Go-http-client/1.1"},
		{"www.newyorkfed.org", "Go-http-client/1.1"},
		{"www.federalreserve.gov", "Go-http-client/1.1"},
		{"home.treasury.gov", "Go-http-client/1.1"},
		{"www.ecb.europa.eu", "Go-http-client/1.1"},
		{"www.bls.gov.example.invalid", "Go-http-client/1.1"},
		{"api.nasdaq.com.example.invalid", "Go-http-client/1.1"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := wireRequest(t, tc.host)
			if got.UserAgent() != tc.want {
				t.Fatalf("wire User-Agent = %q, want %q", got.UserAgent(), tc.want)
			}
			if tc.want == "" && got.Header.Values("User-Agent") != nil {
				t.Fatal("Nasdaq suppression leaked an empty header or Go's default")
			}
			if got.Header.Get("Accept") != "text/xml" || got.Header.Get("Accept-Language") != "en-US,en;q=0.9" {
				t.Fatal("source-specific content negotiation changed")
			}
			for _, key := range []string{"Cookie", "Authorization", "Origin", "Referer"} {
				if got.Header.Get(key) != "" {
					t.Fatalf("request identity added %s", key)
				}
			}
			if strings.Contains(got.UserAgent(), "operator.example") {
				t.Fatal("prior personal attribution survived header selection")
			}
		})
	}
}

// TestNoDestinationImpersonatesBrowserAndOnlyBLSNamesProduct covers every host
// with its own profile and the generic default, so a new browser identity or a
// product URL sent beyond BLS fails here rather than on a provider's policy.
func TestNoDestinationImpersonatesBrowserAndOnlyBLSNamesProduct(t *testing.T) {
	hosts := []string{"public-data.example", "fred.stlouisfed.org", "www.newyorkfed.org"}
	for host := range userAgents {
		hosts = append(hosts, host, strings.ToUpper(host))
	}
	for _, host := range hosts {
		got := wireRequest(t, host).UserAgent()
		if strings.Contains(got, "Mozilla/") || strings.Contains(got, "AppleWebKit") {
			t.Fatalf("%s receives a browser identity: %q", host, got)
		}
		isBLS := strings.EqualFold(host, "www.bls.gov")
		if strings.Contains(got, "osauer.dev") != isBLS || isBLS && got != "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)" {
			t.Fatalf("%s identity %q breaks the BLS-only product URL approval", host, got)
		}
	}
}

// wireRequest serializes a request after SetUserAgent and parses it back, so
// assertions see the header bytes a server would receive.
func wireRequest(t *testing.T, host string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/public-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "fixture/1.0 (+https://operator.example)")
	req.Header.Set("Accept", "text/xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	SetUserAgent(req)
	var wire bytes.Buffer
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}
	got, err := http.ReadRequest(bufio.NewReader(&wire))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { got.Body.Close() })
	return got
}
