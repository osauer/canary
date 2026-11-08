// Package daemon is a synthetic stand-in for Canary's daemon: one route per
// way a handler can, or cannot, reach the IBKR connector.
package daemon

import (
	"slices"

	"github.com/osauer/canary/v2/internal/rpc/api"
)

type conn struct{ account string }

// Server holds the connector the way Canary's daemon does.
type Server struct {
	connector *conn
	opend     *int
}

type route struct {
	call       func() (any, error)
	capability string
}

func serve[P, R any](_ api.Method[P, R], capability string, h func() (R, error)) route {
	return route{func() (any, error) { return h() }, capability}
}

func (s *Server) routeTable() map[string]route {
	return map[string]route{
		api.Clean.Name:    serve(api.Clean, "status", s.handleClean),
		api.Direct.Name:   serve(api.Direct, "positions", s.handleDirect),
		api.Callback.Name: serve(api.Callback, "positions", s.handleCallback),
		api.Seamed.Name:   serve(api.Seamed, "positions", s.handleSeamed),
		api.Order.Name:    serve(api.Order, "orders", s.handleOrder),
		api.Lookup.Name:   serve(api.Lookup, "status", s.handleLookup),
	}
}

// handleLookup builds the route table, as the coverage lookup does, and runs
// none of its handlers.
func (s *Server) handleLookup() (int, error) { return len(s.routeTable()), nil }

func (s *Server) handleClean() (int, error) { return 1, nil }

func (s *Server) handleDirect() (int, error) {
	if s.gatewayConnector() == nil {
		return 0, nil
	}
	return 1, nil
}

// handleCallback reads the connector only inside a function the standard
// library calls back.
func (s *Server) handleCallback() (int, error) {
	xs := []int{2, 1}
	slices.SortFunc(xs, func(a, b int) int {
		if s.connector != nil {
			return a - b
		}
		return b - a
	})
	return xs[0], nil
}

func (s *Server) handleSeamed() (int, error) {
	if s.brokerReads() == nil {
		return 0, nil
	}
	return 1, nil
}

func (s *Server) handleOrder() (int, error) {
	if s.gatewayConnector() == nil {
		return 0, nil
	}
	return 1, nil
}

func (s *Server) gatewayConnector() *conn {
	if s.connector == nil {
		s.triggerReconnect()
	}
	return s.connector
}

func (s *Server) brokerReads() *conn { return s.adapterFor(s.gatewayConnector()) }

func (s *Server) adapterFor(c *conn) *conn { return c }

func (s *Server) triggerReconnect() { s.connector = &conn{} }

type ibkrScope struct{ s *Server }

func (i ibkrScope) Scope() string { return i.s.connector.account }

var _ = ibkrScope{}.Scope
