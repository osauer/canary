// Package publichttp chooses the request identity Canary presents to public
// data sources. Requests are anonymous by default: they never carry an
// operator name, contact address or personal URL, and never claim to be a web
// browser.
//
// The single exception is www.bls.gov. BLS publishes that it blocks robots
// without information that can be used to contact the owner
// (https://www.bls.gov/bls/pss.htm). On 2026-09-24 the owner approved
// identifying Canary to BLS, and only to BLS, by its product URL. That
// identity names the product, not an operator.
//
// Callers retain their own transport, deadlines, formats and retry policy.
package publichttp

import (
	"net/http"
	"strings"
)

// blsUserAgent is the owner-approved product identity. BLS rejected github.com
// URLs in its identity check, so it names the product site instead.
const blsUserAgent = "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)"

// userAgents lists the destinations whose compatible identity differs from Go's
// default. An empty value suppresses the header.
var userAgents = map[string]string{
	"www.bls.gov":      blsUserAgent,
	"en.wikipedia.org": "Canary-public-feeds/1.0",
	// The earnings endpoint rejected named clients in the existing witness.
	"api.nasdaq.com": "",
}

// SetUserAgent applies the destination's identity policy from the package
// documentation. Apply it again when an existing redirect policy permits a new
// destination.
func SetUserAgent(req *http.Request) {
	userAgent, ok := userAgents[strings.ToLower(req.URL.Hostname())]
	if !ok {
		// FRED has rejected custom product identities while accepting Go's default.
		userAgent = "Go-http-client/1.1"
	}
	// An explicitly empty value suppresses Go's default User-Agent on the wire.
	req.Header.Set("User-Agent", userAgent)
}
