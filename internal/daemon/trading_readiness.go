package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	tradingReadinessFileVersion = 1

	tradingPaperSmokeStatusValid        = "valid"
	tradingPaperSmokeStatusMissing      = "missing"
	tradingPaperSmokeStatusStale        = "stale"
	tradingPaperSmokeStatusMismatch     = "mismatch"
	tradingPaperSmokeStatusFailed       = "failed"
	tradingPaperSmokeStatusUnreadable   = "unreadable"
	tradingPaperSmokeStatusUnsigned     = "unsigned"
	tradingPaperSmokeEndpointClassPaper = "paper"
	tradingPaperSmokeResultPassed       = "passed"
	tradingPaperSmokeResultFailed       = "failed"

	// tradingPaperSmokeMACPrefix domain-separates evidence MACs from preview
	// tokens, which share the same key but MAC a bare base64 JSON body that
	// can never start with this prefix.
	tradingPaperSmokeMACPrefix = "ibkr-paper-smoke-v1."

	tradingReadinessStateScope = "daemon"
	tradingReadinessStateKind  = "trading_readiness_v1"
)

type tradingReadinessStore struct {
	// Path is retained only for the explicit legacy codec. Live reads and
	// writes never fall back to trading-readiness.json after cutover.
	Path      string
	signer    *orderTokenSigner
	mu        sync.RWMutex
	authority *corestore.Store
	published corestore.StateDocument
}

type tradingReadinessFile struct {
	Version    int                        `json:"version"`
	PaperSmoke *tradingPaperSmokeEvidence `json:"paper_smoke,omitempty"`
	// PaperSmokeMAC authenticates PaperSmoke as daemon-authored. It sits
	// beside the evidence, not inside it, so the MAC input is simply the
	// marshalled evidence object. Older binaries ignore the field.
	PaperSmokeMAC string `json:"paper_smoke_mac,omitempty"`
}

type tradingPaperSmokeEvidence struct {
	Account       string    `json:"account"`
	Endpoint      string    `json:"endpoint"`
	EndpointClass string    `json:"endpoint_class"`
	ClientID      int       `json:"client_id"`
	Version       string    `json:"version"`
	Result        string    `json:"result"`
	At            time.Time `json:"at"`
}

type tradingPaperSmokeCheck struct {
	Status   string
	Message  string
	Action   string
	Evidence *tradingPaperSmokeEvidence
}

func newTradingReadinessStore(path string, signer *orderTokenSigner) *tradingReadinessStore {
	return &tradingReadinessStore{Path: path, signer: signer}
}

// initializeLockedOrderSignerAndReadiness must run only after the caller owns
// both the daemon instance lock and daemon.db's persistence lock. It is the
// first allowed point for creating the signer key. Tests may inject a signer;
// production startup creates it here, then initializes readiness with that
// exact signer before attachCoreOrderAuthority binds token verification.
func (s *Server) initializeLockedOrderSignerAndReadiness(ctx context.Context, authority *corestore.Store) error {
	if s == nil || authority == nil {
		return fmt.Errorf("initialize locked order authority: unavailable server or store")
	}
	if s.orderTokens == nil {
		path, err := orderTokenKeyPathForDatabase(s.coreStorePath)
		if err != nil {
			return fmt.Errorf("resolve order preview token key: %w", err)
		}
		signer, err := newOrderTokenSigner(path, s.now)
		if err != nil {
			return fmt.Errorf("initialize order preview token signer: %w", err)
		}
		s.orderTokens = signer
	}
	if s.tradingReadiness == nil {
		path := filepath.Join(filepath.Dir(s.coreStorePath), "trading-readiness.json")
		s.tradingReadiness = newTradingReadinessStore(path, s.orderTokens)
	} else {
		s.tradingReadiness.signer = s.orderTokens
	}
	if err := s.tradingReadiness.UseCoreStore(ctx, authority); err != nil {
		return fmt.Errorf("initialize trading readiness: %w", err)
	}
	return nil
}

func (s *tradingReadinessStore) attachedTo(authority *corestore.Store) bool {
	if s == nil || authority == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authority == authority && s.published.Revision > 0
}

// UseCoreStore attaches the sole live readiness authority. A missing document
// is deliberately initialized empty: legacy smoke evidence is not imported
// across the cutover's trust boundary. Existing SQLite evidence survives a
// normal daemon restart.
func (s *tradingReadinessStore) UseCoreStore(ctx context.Context, authority *corestore.Store) error {
	if s == nil || authority == nil || s.signer == nil {
		return fmt.Errorf("trading readiness authority is unavailable")
	}
	if !authority.Health().Ready {
		return fmt.Errorf("trading readiness authority is blocked")
	}
	doc, ok, err := authority.GetStateDocument(ctx, tradingReadinessStateScope, tradingReadinessStateKind)
	if err != nil {
		return fmt.Errorf("load trading readiness authority: %w", err)
	}
	if !ok {
		raw, marshalErr := json.Marshal(tradingReadinessFile{Version: tradingReadinessFileVersion})
		if marshalErr != nil {
			return fmt.Errorf("initialize trading readiness authority: %w", marshalErr)
		}
		doc, err = authority.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: tradingReadinessStateScope, Kind: tradingReadinessStateKind, JSON: raw,
		})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			doc, ok, err = authority.GetStateDocument(ctx, tradingReadinessStateScope, tradingReadinessStateKind)
			if err == nil && !ok {
				err = fmt.Errorf("readiness document disappeared after concurrent initialization")
			}
		}
		if err != nil {
			return fmt.Errorf("initialize trading readiness authority: %w", err)
		}
	}
	if _, err := decodeTradingReadinessDocument(doc.JSON); err != nil {
		return fmt.Errorf("validate trading readiness authority: %w", err)
	}
	// Publish only after the document exists durably and validates.
	s.mu.Lock()
	s.authority = authority
	s.published = doc
	s.mu.Unlock()
	return nil
}

// signPaperSmoke MACs daemon-authored paper-smoke evidence with the order
// preview token key. Same-uid processes can read that key, so this is an
// interlock against hand-edited evidence, not a security boundary.
func (s *orderTokenSigner) signPaperSmoke(ev tradingPaperSmokeEvidence) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", fmt.Errorf("paper-smoke evidence signer is not configured")
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("marshal paper-smoke evidence: %w", err)
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(tradingPaperSmokeMACPrefix + base64.RawURLEncoding.EncodeToString(raw)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyPaperSmoke re-marshals the loaded evidence and compares MACs.
// time.Time round-trips RFC3339Nano deterministically, so
// verify-by-re-marshal is stable across save/load cycles.
func (s *orderTokenSigner) verifyPaperSmoke(ev tradingPaperSmokeEvidence, mac string) bool {
	if mac == "" {
		return false
	}
	want, err := s.signPaperSmoke(ev)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(want), []byte(mac))
}

func decodeTradingReadinessDocument(data []byte) (*tradingReadinessFile, error) {
	var f tradingReadinessFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse trading readiness document: %w", err)
	}
	if f.Version == 0 {
		f.Version = tradingReadinessFileVersion
	}
	if f.Version != tradingReadinessFileVersion {
		return nil, fmt.Errorf("unsupported trading-readiness version %d", f.Version)
	}
	return &f, nil
}

func (s *tradingReadinessStore) Load() (*tradingReadinessFile, error) {
	if s == nil {
		return nil, fmt.Errorf("trading readiness authority is unavailable")
	}
	s.mu.RLock()
	authority := s.authority
	s.mu.RUnlock()
	if authority == nil {
		return nil, fmt.Errorf("trading readiness authority is unavailable")
	}
	if !authority.Health().Ready {
		return nil, fmt.Errorf("trading readiness authority is blocked")
	}
	doc, ok, err := authority.GetStateDocument(context.Background(), tradingReadinessStateScope, tradingReadinessStateKind)
	if err != nil {
		return nil, fmt.Errorf("load trading readiness authority: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("trading readiness authority document is missing")
	}
	return decodeTradingReadinessDocument(doc.JSON)
}

func (s *tradingReadinessStore) SavePaperSmoke(ev tradingPaperSmokeEvidence) error {
	if s == nil {
		return fmt.Errorf("trading readiness authority is unavailable")
	}
	mac, err := s.signer.signPaperSmoke(ev)
	if err != nil {
		return err
	}
	f := tradingReadinessFile{
		Version:       tradingReadinessFileVersion,
		PaperSmoke:    &ev,
		PaperSmokeMAC: mac,
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal trading readiness: %w", err)
	}
	for range 8 {
		s.mu.RLock()
		authority := s.authority
		s.mu.RUnlock()
		if authority == nil {
			return fmt.Errorf("trading readiness authority is unavailable")
		}
		if !authority.Health().Ready {
			return fmt.Errorf("trading readiness authority is blocked")
		}
		current, ok, err := authority.GetStateDocument(context.Background(), tradingReadinessStateScope, tradingReadinessStateKind)
		if err != nil {
			return fmt.Errorf("load trading readiness authority: %w", err)
		}
		if !ok {
			return fmt.Errorf("trading readiness authority document is missing")
		}
		saved, err := authority.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: tradingReadinessStateScope, Kind: tradingReadinessStateKind,
			ExpectedRevision: current.Revision, JSON: data,
		})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("save trading readiness authority: %w", err)
		}
		// Publish only after the durable CAS succeeds.
		s.mu.Lock()
		s.published = saved
		s.mu.Unlock()
		return nil
	}
	return fmt.Errorf("save trading readiness authority: too many revision conflicts")
}

func (s *tradingReadinessStore) CheckPaperSmoke(liveAccount, liveEndpoint string, clientID int, version string, maxAge time.Duration, now time.Time) tradingPaperSmokeCheck {
	if s == nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `canary trading paper-smoke` against the pinned paper account first.",
		}
	}
	f, err := s.Load()
	if err != nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusUnreadable,
			Message: "paper-smoke evidence is unreadable",
			Action:  "Inspect daemon storage health, then rerun `canary trading paper-smoke`.",
		}
	}
	if f.PaperSmoke == nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `canary trading paper-smoke` against the pinned paper account first.",
		}
	}
	ev := f.PaperSmoke
	if s.signer == nil || !s.signer.verifyPaperSmoke(*ev, f.PaperSmokeMAC) {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusUnsigned,
			Message:  "paper-smoke evidence is not signed by this daemon",
			Action:   "Run `canary trading paper-smoke`; hand-written evidence is not accepted.",
			Evidence: ev,
		}
	}
	if ev.Result != tradingPaperSmokeResultPassed {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusFailed,
			Message:  "last paper-smoke evidence did not pass",
			Action:   "Fix the paper order lifecycle and rerun `canary trading paper-smoke`.",
			Evidence: ev,
		}
	}
	if ev.EndpointClass != tradingPaperSmokeEndpointClassPaper ||
		!paperSmokeEvidenceMatchesLiveGate(*ev, liveAccount, liveEndpoint, clientID, version) {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusMismatch,
			Message:  "paper-smoke evidence does not match the live account family, pinned host, client ID, binary version, or paper endpoint requirement",
			Action:   "Run `canary trading paper-smoke` on the paper gateway that pairs with this live setup before enabling live.",
			Evidence: ev,
		}
	}
	if ev.At.IsZero() || now.Sub(ev.At) > maxAge || ev.At.After(now.Add(5*time.Minute)) {
		return tradingPaperSmokeCheck{
			Status:   tradingPaperSmokeStatusStale,
			Message:  "paper-smoke evidence is stale",
			Action:   "Rerun `canary trading paper-smoke` before enabling live.",
			Evidence: ev,
		}
	}
	return tradingPaperSmokeCheck{
		Status:   tradingPaperSmokeStatusValid,
		Message:  "paper-smoke evidence is fresh and matches the live gate",
		Evidence: ev,
	}
}

func paperSmokeEvidenceMatchesLiveGate(ev tradingPaperSmokeEvidence, liveAccount, liveEndpoint string, clientID int, version string) bool {
	if ev.ClientID != clientID {
		return false
	}
	if version != "" && ev.Version != version {
		return false
	}
	if !looksPaperEndpoint(ev.Endpoint, ev.Account) {
		return false
	}
	if accountFamilyKey(ev.Account) != accountFamilyKey(liveAccount) {
		return false
	}
	return endpointHost(ev.Endpoint) == endpointHost(liveEndpoint)
}

func looksPaperEndpoint(endpoint, account string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err == nil && host != "" {
		switch port {
		case "4002", "7497":
			return true
		}
	}
	return looksPaper(0, account)
}

func endpointHost(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err == nil {
		return host
	}
	return endpoint
}

func accountFamilyKey(account string) string {
	account = strings.ToUpper(strings.TrimSpace(account))
	if after, ok := strings.CutPrefix(account, "DU"); ok {
		return "U" + after
	}
	return account
}
