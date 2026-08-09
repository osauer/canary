// Package relay connects the local Canary app host to an optional public relay.
// The relay is transport only: it forwards allowlisted HTTP and streaming paths
// to the local host, while device grants, sessions, authorization, app state,
// daemon access, and broker authority remain local.
package relay

import (
	"context"
)

// Status is a concurrency-safe snapshot of relay transport state. URL is the
// public app origin, Connected reports only the connector transport, and
// Message is diagnostic text rather than authorization or app-health state.
type Status struct {
	Mode      string `json:"mode"`
	URL       string `json:"url,omitempty"`
	Connected bool   `json:"connected"`
	Message   string `json:"message,omitempty"`
}

// Client is the app host's optional relay transport. Implementations may expose
// the local host through an allowlisted remote route but do not authenticate
// devices or authorize app operations.
type Client interface {
	// Run maintains transport activity until ctx cancellation or implementation
	// shutdown. Callers normally invoke it in a background goroutine.
	Run(ctx context.Context)
	// Status returns the latest detached transport status.
	Status() Status
	// PairingURL annotates raw with non-authorizing transport routing data.
	PairingURL(raw string) string
}

// Noop is a disabled relay client. PublicURL is reported for local app links,
// but Run starts no transport and PairingURL leaves URLs unchanged.
type Noop struct {
	PublicURL string
}

// Run implements Client without starting background work.
func (n Noop) Run(context.Context) {}

// Status returns the disabled relay state and configured public URL.
func (n Noop) Status() Status {
	return Status{
		Mode:      "none",
		URL:       n.PublicURL,
		Connected: false,
		Message:   "relay contract not configured in MVP; trusted HTTPS relay origin is required for production iPhone install and Web Push",
	}
}

// PairingURL returns raw unchanged because Noop has no remote route.
func (n Noop) PairingURL(raw string) string {
	return raw
}
