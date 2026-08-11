package apphttp

import (
	"net"
	nethttp "net/http"
	"slices"
	"strings"
)

// Preview read grant: an isolated loopback preview instance started with
// `canary app --preview-read-grant` serves GET read routes to unpaired local
// browsers, so the preview pane renders without a pairing round-trip. The
// grant is deliberately narrow and fails closed:
//
//   - loopback peer only, and never the wider isLocalMac interface set;
//   - exact Host match against the bound loopback address, which also
//     excludes relay-forwarded traffic (the relay rewrites Host to the
//     public origin) and DNS-rebinding hosts;
//   - no foreign Origin header;
//   - GET/HEAD only, and only on routes registered through requireRead.
//
// Mutating routes always require a paired device, so a granted browser can
// look at everything and change nothing.

// previewReadHosts returns the exact Host values the grant accepts for a
// listen address. An unparsable address disables the grant.
func previewReadHosts(addr string) []string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || port == "" {
		return nil
	}
	return []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
}

func (h *handler) previewReadGranted(r *nethttp.Request) bool {
	if len(h.readHosts) == 0 {
		return false
	}
	if r.Method != nethttp.MethodGet && r.Method != nethttp.MethodHead {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(peer); ip == nil || !ip.IsLoopback() {
		return false
	}
	if !slices.Contains(h.readHosts, strings.ToLower(strings.TrimSpace(r.Host))) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(h.readOrigins, strings.ToLower(origin)) {
		return false
	}
	return true
}
