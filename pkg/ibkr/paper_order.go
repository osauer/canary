package ibkr

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const paperOrderMode = "paper"

// PaperOrderGate identifies the paper account and connection coordinates that
// the narrow paper-order wrappers validate before writing a frame. Mode must be
// "paper", Account must name a concrete DU account, ClientID must be positive,
// and Endpoint or Host and Port must identify the configured connection.
//
// A valid PaperOrderGate is caller-supplied evidence, not authorization to
// submit or cancel an order, proof of the connected broker session's identity,
// or a broker acknowledgement. Callers remain responsible for preview, policy,
// freeze, journaling, and reconciliation controls.
type PaperOrderGate struct {
	Mode    string // Mode must equal "paper", ignoring case and surrounding space.
	Account string // Account must be a non-aggregate account whose name starts with DU.

	// Endpoint may contain host:port. When it parses successfully, it takes
	// precedence over Host and Port during validation.
	Endpoint string
	Host     string // Host is required when Endpoint does not supply one.
	Port     int    // Port is required when Endpoint does not supply one; zero means absent.
	ClientID int    // ClientID must be positive and match a nonzero configured client ID.
}

func (g PaperOrderGate) validateConnection(c *Connection) error {
	if err := g.validate(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("paper order gate: connection is nil")
	}
	if strings.TrimSpace(c.config.Account) != "" &&
		!strings.EqualFold(strings.TrimSpace(c.config.Account), strings.TrimSpace(g.Account)) {
		return fmt.Errorf("paper order gate account %q does not match connection account %q", g.Account, c.config.Account)
	}
	// Endpoint takes precedence over Host and Port, as validate() reads it;
	// an endpoint-only gate must be checked against the connection too.
	host, port := g.endpointParts()
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(c.config.Host)
	}
	if port == 0 {
		port = c.config.Port
	}
	if strings.TrimSpace(c.config.Host) != "" && !strings.EqualFold(strings.TrimSpace(c.config.Host), host) {
		return fmt.Errorf("paper order gate host %q does not match connection host %q", host, c.config.Host)
	}
	if c.config.Port != 0 && port != c.config.Port {
		return fmt.Errorf("paper order gate port %d does not match connection port %d", port, c.config.Port)
	}
	if c.config.ClientID != 0 && g.ClientID != c.config.ClientID {
		return fmt.Errorf("paper order gate client ID %d does not match connection client ID %d", g.ClientID, c.config.ClientID)
	}
	return nil
}

func (g PaperOrderGate) validate() error {
	if !strings.EqualFold(strings.TrimSpace(g.Mode), paperOrderMode) {
		return fmt.Errorf("paper order gate requires mode=paper")
	}
	account := strings.TrimSpace(g.Account)
	if account == "" {
		return fmt.Errorf("paper order gate requires account")
	}
	if strings.EqualFold(account, "All") {
		return fmt.Errorf("paper order gate rejects aggregate account All")
	}
	host, port := g.endpointParts()
	if host == "" {
		return fmt.Errorf("paper order gate requires endpoint host")
	}
	if port == 0 {
		return fmt.Errorf("paper order gate requires endpoint port")
	}
	if g.ClientID <= 0 {
		return fmt.Errorf("paper order gate requires positive client ID")
	}
	if !paperAccount(account) {
		return fmt.Errorf("paper order gate requires a concrete DU paper account")
	}
	return nil
}

func (g PaperOrderGate) endpointParts() (string, int) {
	host := strings.TrimSpace(g.Host)
	port := g.Port
	endpoint := strings.TrimSpace(g.Endpoint)
	if endpoint == "" {
		return host, port
	}
	if h, p, err := net.SplitHostPort(endpoint); err == nil {
		host = h
		if parsed, parseErr := strconv.Atoi(p); parseErr == nil {
			port = parsed
		}
		return host, port
	}
	if host == "" {
		host = endpoint
	}
	return host, port
}

func paperAccount(account string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(account)), "DU")
}
