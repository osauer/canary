package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func gatewayAPIHealth() *rpc.GatewayAPIHealth {
	return &rpc.GatewayAPIHealth{TradingProtocolMax: ibkrlib.TradingProtocolMax, ConfigReadProtocol: ibkrlib.ConfigReadProtocol, ConfigState: "not_requested"}
}

// gatewaySetupStore keeps the last settings read of the current session. The
// epoch ties it to one connector publication, so a reconnect empties it until
// the next read lands.
type gatewaySetupStore struct {
	mu         sync.Mutex
	epoch      uint64
	diagnostic ibkrlib.APIConfigDiagnostic
}

func (st *gatewaySetupStore) set(epoch uint64, d ibkrlib.APIConfigDiagnostic) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.epoch, st.diagnostic = epoch, d
}

func (st *gatewaySetupStore) get(epoch uint64) (ibkrlib.APIConfigDiagnostic, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.epoch != epoch || st.diagnostic.State == "" {
		return ibkrlib.APIConfigDiagnostic{}, false
	}
	return st.diagnostic, true
}

// gatewaySetupSettle is how long after a connect the automatic read waits for
// the main session to finish its start, so the read never competes with it.
const gatewaySetupSettle = 3 * time.Second

// observeGatewaySetup makes the one automatic settings read of a session. It
// runs after each connect, on the isolated diagnostic socket, and keeps only
// the allowlisted observation. A read that lands after the session changed is
// dropped rather than filed against the new one.
func (s *Server) observeGatewaySetup(ctx context.Context) {
	if ctx == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(gatewaySetupSettle):
	}
	_, _ = s.readGatewaySetup(ctx)
}

// readGatewaySetup reads the settings on the diagnostic socket and files the
// observation under the session it was read from. The returned diagnostic is
// what the read saw; ok is false when there was no session to read or it
// changed under the read.
func (s *Server) readGatewaySetup(ctx context.Context) (ibkrlib.APIConfigDiagnostic, bool) {
	if !s.gatewayDiagnosticsMu.TryLock() {
		return ibkrlib.APIConfigDiagnostic{State: "unavailable", Reason: "probe_in_progress"}, false
	}
	defer s.gatewayDiagnosticsMu.Unlock()
	s.mu.Lock()
	connector, epoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	if connector == nil || !connector.IsConnected() {
		return ibkrlib.APIConfigDiagnostic{State: "unavailable", Reason: "not_connected"}, false
	}
	session, ok := connector.CaptureSession()
	if !ok {
		return ibkrlib.APIConfigDiagnostic{State: "unavailable", Reason: "not_connected"}, false
	}
	diagnostic := connector.ReadAPIConfig(ctx)
	s.mu.Lock()
	current := s.connector == connector && s.connectorEpoch == epoch
	s.mu.Unlock()
	if !current || !connector.SessionReceiptCurrent(session) {
		return ibkrlib.APIConfigDiagnostic{State: "unavailable", Reason: "session_changed"}, false
	}
	s.gatewaySetup.set(epoch, diagnostic)
	return diagnostic, true
}

func (s *Server) handleStatusHealthRequest(ctx context.Context, req *rpc.Request) (*rpc.HealthResult, error) {
	var params rpc.HealthParams
	if err := decodeFor(api.StatusHealth, req.Params, &params); err != nil {
		return nil, err
	}
	res := s.handleStatusHealth()
	if !params.GatewayDiagnostics {
		return res, nil
	}
	if !res.Connected {
		res.GatewayAPI.ConfigState, res.GatewayAPI.Reason = "unavailable", "not_connected"
		return res, nil
	}
	diagnostic, ok := s.readGatewaySetup(ctx)
	if !ok {
		if diagnostic.Reason == "session_changed" {
			res = s.statusHealthSnapshot()
		}
		res.GatewayAPI.ConfigState, res.GatewayAPI.Reason = diagnostic.State, diagnostic.Reason
		return res, nil
	}
	applyGatewayDiagnostic(res.GatewayAPI, diagnostic)
	res.GatewaySetup = s.gatewaySetupHealth(res)
	return res, nil
}

func applyGatewayDiagnostic(out *rpc.GatewayAPIHealth, diagnostic ibkrlib.APIConfigDiagnostic) {
	out.ConfigState, out.Reason = diagnostic.State, diagnostic.Reason
	out.DiagnosticProtocol, out.AsOf = diagnostic.ServerVersion, diagnostic.AsOf
	if diagnostic.State != "observed" {
		return
	}
	if settings := diagnostic.Settings; settings != nil {
		out.Settings = &rpc.GatewayAPISettings{
			ReadOnly: settings.ReadOnly, DownloadOpenOrders: settings.DownloadOpenOrders,
			VolumeInLots: settings.VolumeInLots, RetainOrdersOnReconnect: settings.RetainOrdersOnReconnect,
			RejectAboveRate: settings.RejectAboveRate, LocalhostOnly: settings.LocalhostOnly,
			LoopbackTrusted: settings.LoopbackTrusted, CreateAPILog: settings.CreateAPILog,
			MarketDataInAPILog: settings.MarketDataInAPILog, LoggingLevel: settings.LoggingLevel,
			SocketPort: settings.SocketPort, MasterClientID: settings.MasterClientID,
		}
	}
	if p := diagnostic.Precautions; p != nil {
		out.Precautions = &rpc.GatewayAPIPrecautions{Bypassed: append([]string{}, p.Bypassed...)}
	}
	if l := diagnostic.LockAndExit; l != nil {
		out.LockAndExit = &rpc.GatewayLockAndExit{Type: l.Type, Time: l.Time, Period: l.Period}
	}
}

// gatewaySetupHealth reads the session's filed observation against what the
// daemon needs from the application: its own client id, the host it dials and
// whether orders are enabled.
func (s *Server) gatewaySetupHealth(res *rpc.HealthResult) *rpc.GatewaySetupHealth {
	if res == nil || !res.Connected {
		return nil
	}
	s.mu.Lock()
	epoch, host := s.connectorEpoch, s.endpoint.Host
	s.mu.Unlock()
	diagnostic, ok := s.gatewaySetup.get(epoch)
	if !ok {
		return &rpc.GatewaySetupHealth{State: rpc.GatewaySetupUnknown, Reason: "not_read_yet"}
	}
	if diagnostic.State != "observed" {
		return &rpc.GatewaySetupHealth{State: rpc.GatewaySetupUnknown, Reason: diagnostic.Reason, AsOf: diagnostic.AsOf}
	}
	notes := gatewaySetupNotes(diagnostic, res.ClientID, host, res.Trading.Mode)
	state := rpc.GatewaySetupAsExpected
	if len(notes) > 0 {
		state = rpc.GatewaySetupAttention
	}
	return &rpc.GatewaySetupHealth{State: state, AsOf: diagnostic.AsOf, Notes: notes}
}

// gatewaySetupNotes turns the settings that differ from what Canary needs into
// short sentences. Each names the setting as the application labels it and the
// consequence; a setting the broker did not report is not a difference.
func gatewaySetupNotes(d ibkrlib.APIConfigDiagnostic, clientID int, host, tradingMode string) []string {
	var notes []string
	on := func(b *bool) bool { return b != nil && *b }
	off := func(b *bool) bool { return b != nil && !*b }
	if st := d.Settings; st != nil {
		ordersEnabled := tradingMode == "live" || tradingMode == "paper"
		if ordersEnabled && on(st.ReadOnly) {
			notes = append(notes, "Read-Only API is on: no order can go out.")
		}
		if st.MasterClientID == nil || *st.MasterClientID != clientID {
			notes = append(notes, fmt.Sprintf("Master API client ID is not %d: orders and fills from TWS or other apps stay unseen.", clientID))
		}
		if off(st.DownloadOpenOrders) {
			notes = append(notes, "Open orders are not downloaded at connect: orders placed elsewhere stay unseen.")
		}
		if on(st.RejectAboveRate) {
			notes = append(notes, "Fast request bursts are rejected instead of slowed: three rejections cut the session.")
		}
		if isLoopbackHost(host) && off(st.LocalhostOnly) && off(st.LoopbackTrusted) {
			notes = append(notes, "127.0.0.1 is not a trusted IP: every reconnect waits for a click in the Gateway.")
		}
		if st.LoggingLevel != nil && *st.LoggingLevel == "detail" {
			notes = append(notes, "API log level is Detail: the log grows fast; use it only while troubleshooting.")
		}
		if on(st.MarketDataInAPILog) {
			notes = append(notes, "Market data is written to the API log: the log grows fast.")
		}
	}
	if l := d.LockAndExit; l != nil && l.Type != "restart" {
		when := l.Time
		if l.Period != "" {
			when += " " + l.Period
		}
		if l.Type == "logoff" {
			notes = append(notes, fmt.Sprintf("Daily auto log-off at %s instead of auto restart: the Gateway stays signed out until someone signs in.", when))
		} else {
			notes = append(notes, "No daily auto restart is set: the Gateway will not come back by itself after the broker's nightly reset.")
		}
	}
	return notes
}
