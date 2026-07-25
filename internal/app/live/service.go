package live

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/daemonclient"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

type alertCandidateClient interface {
	AlertCandidates(context.Context) (*rpc.AlertCandidateSnapshot, error)
}

// AlertSnapshotAuthority is the single app-side owner of durable candidate
// observation and delivery. Live reads its redacted current view only for
// restart priming; every producer observation goes through Observe.
type AlertSnapshotAuthority interface {
	Observe(context.Context, rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error)
	Current(time.Time) state.AlertDeliveryView
}

// Service owns the app host's refreshable daemon snapshot, poll serialization,
// source-freshness metadata, and best-effort event subscribers. Values stored
// in the snapshot are treated as immutable after publication.
type Service struct {
	client      daemonclient.Client
	pollEvery   time.Duration
	canaryEvery time.Duration
	now         func() time.Time

	pollMu         sync.Mutex
	alertMu        sync.Mutex
	mu             sync.Mutex
	snapshot       Snapshot
	hashes         map[string]string
	lastEventAt    map[string]time.Time
	subs           map[chan Event]struct{}
	nextCanary     time.Time
	nextNudges     time.Time
	alertAuthority AlertSnapshotAuthority
}

// Snapshot is the app's cached adapter view of daemon results. It preserves
// daemon-authored financial and policy semantics; callers must not reinterpret
// quote moves as position P/L or treat the cache as runtime authority.
type Snapshot struct {
	Version         int64                       `json:"version"`
	UpdatedAt       time.Time                   `json:"updated_at,omitzero"`
	Status          *rpc.HealthResult           `json:"status,omitempty"`
	Calendar        *rpc.MarketCalendarResult   `json:"market_calendar,omitempty"`
	Account         *rpc.AccountResult          `json:"account,omitempty"`
	Positions       *rpc.PositionsResult        `json:"positions,omitempty"`
	Quotes          *MarketQuotes               `json:"market_quotes,omitempty"`
	MarketEvents    *rpc.MarketEventsResult     `json:"market_events,omitempty"`
	Regime          *rpc.RegimeMonitorResult    `json:"regime,omitempty"`
	Canary          *rpc.StressResult           `json:"canary,omitempty"`
	AlertCandidates *rpc.AlertCandidateSnapshot `json:"-"`
	Rules           *rpc.RulesResult            `json:"rules,omitempty"`
	Brief           *rpc.BriefResult            `json:"brief,omitempty"`
	Nudges          *rpc.NudgesSnapshotResult   `json:"nudges,omitempty"`
	Trading         *rpc.TradingStatus          `json:"trading,omitempty"`
	AutoTrade       *rpc.AutoTradeStatus        `json:"auto_trade,omitempty"`
	Opportunities   *rpc.OpportunitySnapshot    `json:"opportunities,omitempty"`
	Proposals       *rpc.TradeProposalSnapshot  `json:"proposals,omitempty"`
	Settings        *rpc.PlatformSettings       `json:"settings,omitempty"`
	Errors          []SourceError               `json:"errors,omitempty"`
	Sources         map[string]SourceMeta       `json:"sources,omitempty"`
}

// publicNudgesSnapshot is the browser/SSE projection. Reconciliation uses a
// separate allowlisted HTTP DTO; keeping the full daemon RPC value here would
// let future private fields bypass that boundary through bootstrap or events.
type publicNudgesSnapshot struct {
	AsOf                  time.Time                       `json:"as_of"`
	Candidates            []rpc.NudgeCandidate            `json:"candidates"`
	SourceHealth          publicNudgeSourceHealth         `json:"source_health"`
	ConfirmedFlowCoverage *rpc.NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	Context               *rpc.NudgeSnapshotContext       `json:"context,omitempty"`
}

type publicNudgeSourceHealth struct {
	Aggregate      string               `json:"aggregate"`
	Policy         rpc.NudgeInputHealth `json:"policy"`
	Reconciliation rpc.NudgeInputHealth `json:"reconciliation"`
	Capital        rpc.NudgeInputHealth `json:"capital"`
	Pins           rpc.NudgeInputHealth `json:"pins"`
	Cadence        rpc.NudgeInputHealth `json:"cadence"`
	ConfirmedFlow  rpc.NudgeInputHealth `json:"confirmed_flow"`
}

func projectPublicNudges(value *rpc.NudgesSnapshotResult) *publicNudgesSnapshot {
	if value == nil {
		return nil
	}
	health := rpc.NormalizeNudgeSourceHealth(value.SourceHealth, len(value.Candidates))
	return &publicNudgesSnapshot{
		AsOf: value.AsOf, Candidates: value.Candidates,
		SourceHealth: publicNudgeSourceHealth{
			Aggregate: health.Aggregate, Policy: health.Policy, Reconciliation: health.Reconciliation,
			Capital: health.Capital, Pins: health.Pins, Cadence: health.Cadence, ConfirmedFlow: health.ConfirmedFlow,
		},
		ConfirmedFlowCoverage: value.ConfirmedFlowCoverage, Context: value.Context,
	}
}

// MarshalJSON is the one public snapshot boundary shared by bootstrap,
// snapshot reads, and full-snapshot SSE messages.
func (snapshot Snapshot) MarshalJSON() ([]byte, error) {
	type snapshotAlias Snapshot
	return json.Marshal(struct {
		snapshotAlias
		Nudges *publicNudgesSnapshot `json:"nudges,omitempty"`
	}{snapshotAlias: snapshotAlias(snapshot), Nudges: projectPublicNudges(snapshot.Nudges)})
}

// MarketQuotes holds observed underlying-market prices and per-symbol fetch
// errors. These market moves are not Daily, Open, Unrealized, or Realized P/L.
type MarketQuotes struct {
	AsOf   time.Time            `json:"as_of,omitzero"`
	Quotes map[string]rpc.Quote `json:"quotes,omitempty"`
	Errors map[string]string    `json:"errors,omitempty"`
}

// SourceError is the public, allowlisted failure record for one app refresh;
// Message omits raw broker, transport, and daemon error text.
type SourceError struct {
	Source  string    `json:"source"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// SourceMeta distinguishes the last app observation from the last successful
// refresh. State and Reason say whether retained data is current, stale, never
// observed, or unavailable.
type SourceMeta struct {
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	State         string    `json:"state,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// Source states and reasons classify app transport freshness independently of
// producer-authored data quality carried inside daemon results.
const (
	SourceStateNotObserved             = "not_observed"
	SourceStateCurrent                 = "current"
	SourceStateStale                   = "stale"
	SourceStateUnavailable             = "unavailable"
	SourceReasonNone                   = ""
	SourceReasonNotObserved            = "not_observed"
	SourceReasonPollStale              = "poll_stale"
	SourceReasonTransportUnavailable   = "transport_unavailable"
	SourceReasonProducerUnavailable    = "producer_unavailable"
	SourceReasonPersistenceUnavailable = "persistence_unavailable"
	nudgesPollEvery                    = time.Minute
)

var (
	errCanaryResultUnavailable    = errors.New("canary result unavailable")
	errRegimeResultUnavailable    = errors.New("regime result unavailable")
	errRulesResultUnavailable     = errors.New("rules result unavailable")
	errBriefResultUnavailable     = errors.New("brief result unavailable")
	errAlertCandidatesUnavailable = errors.New("alert candidate snapshot unavailable")
	errAlertDeliveryDispatch      = errors.New("alert delivery dispatch unavailable")
)

// Event carries one typed live-cache change to SSE adapters. Subscribers must
// recover from dropped events by reading a full snapshot.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Diagnostics is a concurrency-safe snapshot of live subscriber count, event
// timestamps, and cache version.
type Diagnostics struct {
	Subscribers int                  `json:"subscribers"`
	LastEventAt map[string]time.Time `json:"last_event_at,omitempty"`
	Version     int64                `json:"version"`
}

// New constructs an unstarted live service. Non-positive intervals select the
// default poll cadences.
func New(client daemonclient.Client, pollEvery, canaryEvery time.Duration) *Service {
	if pollEvery <= 0 {
		pollEvery = 5 * time.Second
	}
	if canaryEvery <= 0 {
		canaryEvery = time.Minute
	}
	return &Service{
		client:      client,
		pollEvery:   pollEvery,
		canaryEvery: canaryEvery,
		now:         time.Now,
		hashes:      map[string]string{},
		lastEventAt: map[string]time.Time{},
		subs:        map[chan Event]struct{}{},
		snapshot: Snapshot{Sources: map[string]SourceMeta{
			"nudges":           {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
			"canary":           {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
			"regime":           {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
			"alert_candidates": {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
			"rules":            {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
			"brief":            {State: SourceStateNotObserved, Reason: SourceReasonNotObserved},
		}},
	}
}

// SetAlertSnapshotAuthority wires the sole durable alert observer. A prior
// durable view is synchronously invalidated through that observer before the
// app can serve it after restart; startup fails if the transition cannot be
// persisted.
func (s *Service) SetAlertSnapshotAuthority(authority AlertSnapshotAuthority) error {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	now := s.now().UTC()
	s.mu.Lock()
	s.alertAuthority = authority
	s.mu.Unlock()
	if authority == nil {
		return nil
	}
	view := authority.Current(now)
	if !view.Initialized {
		return nil
	}
	unavailable := unavailableAlertCandidateSnapshot(now, nil, nil, view)
	if unavailable == nil {
		return errAlertCandidatesUnavailable
	}
	if _, err := authority.Observe(context.Background(), *unavailable); err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshot.AlertCandidates = cloneAlertCandidateSnapshot(unavailable)
	s.snapshot.Sources["alert_candidates"] = sourceUnavailableWithReason(s.snapshot.Sources["alert_candidates"], now, SourceReasonProducerUnavailable)
	s.snapshot.UpdatedAt = now
	s.snapshot.Version++
	s.mu.Unlock()
	return nil
}

func sourceCurrent(now time.Time) SourceMeta {
	return SourceMeta{UpdatedAt: now, LastSuccessAt: now, State: SourceStateCurrent}
}

func sourceUnavailable(prior SourceMeta, now time.Time) SourceMeta {
	return sourceUnavailableWithReason(prior, now, SourceReasonTransportUnavailable)
}

func sourceUnavailableWithReason(prior SourceMeta, now time.Time, reason string) SourceMeta {
	return SourceMeta{
		UpdatedAt:     now,
		LastSuccessAt: prior.LastSuccessAt,
		State:         SourceStateUnavailable,
		Reason:        reason,
	}
}

// Start performs initial refreshes, starts quote and alert-freshness workers,
// and blocks on periodic polling until ctx is canceled. Cancellation closes all
// subscriber channels.
func (s *Service) Start(ctx context.Context) {
	go s.runAlertFreshnessGuard(ctx)
	_ = s.pollStatus(ctx)
	s.startMarketQuoteStreams(ctx)
	_ = s.PollOnce(ctx)
	t := time.NewTicker(s.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.closeSubscribers()
			return
		case <-t.C:
			_ = s.PollOnce(ctx)
		}
	}
}

func (s *Service) pollStatus(ctx context.Context) Snapshot {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	now := s.now().UTC()
	s.mu.Lock()
	snap := cloneSnapshot(s.snapshot)
	if snap.Sources == nil {
		snap.Sources = map[string]SourceMeta{}
	}
	s.mu.Unlock()

	var events []Event
	errors := []SourceError{}
	if status, err := s.client.Status(ctx); err != nil {
		errors = append(errors, sourceErr("status", err, now))
		snap.Sources["status"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Status = status
		snap.Sources["status"] = SourceMeta{UpdatedAt: now}
		if s.changed("status", status) {
			events = append(events, Event{Type: "status", Data: status})
		}
	}

	s.mu.Lock()
	snap.UpdatedAt = now
	snap.Errors = errors
	snap.Version++
	s.snapshot = snap
	out := cloneSnapshot(s.snapshot)
	s.mu.Unlock()

	events = append(events, Event{Type: "snapshot", Data: out})
	for _, ev := range events {
		s.publish(ev)
	}
	return out
}

func (s *Service) startMarketQuoteStreams(ctx context.Context) {
	for _, item := range marketQuoteContracts {
		go s.runMarketQuoteStream(ctx, item)
	}
}

func (s *Service) runMarketQuoteStream(ctx context.Context, item marketQuoteContract) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.client.StreamQuote(ctx, item.contract, func(frame rpc.Frame) error {
			s.applyMarketQuoteFrame(item.label, frame)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.applyMarketQuoteError(item.label, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollEvery):
		}
	}
}

// PollOnce serializes one full refresh, preserves last-known values beside
// explicit source failure metadata, publishes change/full-snapshot events, and
// returns a cloned post-poll view.
func (s *Service) PollOnce(ctx context.Context) Snapshot {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	now := s.now().UTC()
	s.mu.Lock()
	snap := cloneSnapshot(s.snapshot)
	if snap.Sources == nil {
		snap.Sources = map[string]SourceMeta{}
	}
	pollCanary := s.nextCanary.IsZero() || !now.Before(s.nextCanary)
	pollNudges := s.nextNudges.IsZero() || !now.Before(s.nextNudges)
	s.mu.Unlock()

	var events []Event
	errors := []SourceError{}

	if status, err := s.client.Status(ctx); err != nil {
		errors = append(errors, sourceErr("status", err, now))
		snap.Sources["status"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Status = status
		snap.Sources["status"] = SourceMeta{UpdatedAt: now}
		if s.changed("status", status) {
			events = append(events, Event{Type: "status", Data: status})
		}
	}
	if calendar, err := s.client.MarketCalendar(ctx); err != nil {
		errors = append(errors, sourceErr("calendar", err, now))
		snap.Sources["calendar"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Calendar = calendar
		snap.Sources["calendar"] = SourceMeta{UpdatedAt: now}
		if s.changed("calendar", calendar) {
			events = append(events, Event{Type: "market_calendar", Data: calendar})
		}
	}
	if account, err := s.client.Account(ctx); err != nil {
		errors = append(errors, sourceErr("account", err, now))
		snap.Sources["account"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Account = account
		snap.Sources["account"] = SourceMeta{UpdatedAt: now}
		if s.changed("account", account) {
			events = append(events, Event{Type: "account", Data: account})
		}
	}
	if positions, err := s.client.Positions(ctx); err != nil {
		errors = append(errors, sourceErr("positions", err, now))
		snap.Sources["positions"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Positions = positions
		snap.Sources["positions"] = SourceMeta{UpdatedAt: now}
		if s.changed("positions", positions) {
			events = append(events, Event{Type: "positions", Data: positions})
		}
	}
	if len(events) > 0 {
		snap = s.publishSnapshot(now, snap, errors, events)
		events = nil
	}
	if symbols := liveMarketEventSymbols(snap.Positions); len(symbols) > 0 {
		if marketEvents, err := s.client.MarketEvents(ctx, rpc.MarketEventsParams{Symbols: symbols}); err != nil {
			errors = append(errors, sourceErr("market_events", err, now))
			snap.Sources["market_events"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
		} else {
			snap.MarketEvents = marketEvents
			snap.Sources["market_events"] = SourceMeta{UpdatedAt: now}
			if s.changed("market_events", marketEvents) {
				events = append(events, Event{Type: "market_events", Data: marketEvents})
			}
		}
	}
	if quotes, err := s.marketQuotes(ctx, now, snap.Positions, snap.Quotes); err != nil {
		errors = append(errors, sourceErr("market_quotes", err, now))
		snap.Sources["market_quotes"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
		if quotes != nil {
			snap.Quotes = mergeMarketQuotes(snap.Quotes, quotes)
			if s.changed("market_quotes", snap.Quotes) {
				events = append(events, Event{Type: "market_quotes", Data: snap.Quotes})
			}
		}
	} else {
		snap.Quotes = mergeMarketQuotes(snap.Quotes, quotes)
		snap.Sources["market_quotes"] = SourceMeta{UpdatedAt: now}
		if s.changed("market_quotes", snap.Quotes) {
			events = append(events, Event{Type: "market_quotes", Data: snap.Quotes})
		}
	}
	if trading, err := s.client.TradingStatus(ctx); err != nil {
		errors = append(errors, sourceErr("trading", err, now))
		snap.Sources["trading"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Trading = trading
		snap.Sources["trading"] = SourceMeta{UpdatedAt: now}
		if s.changed("trading", trading) {
			events = append(events, Event{Type: "trading", Data: trading})
		}
	}
	if autoTrade, err := s.client.AutoTradeStatus(ctx); err != nil {
		errors = append(errors, sourceErr("auto_trade", err, now))
		snap.Sources["auto_trade"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.AutoTrade = autoTrade
		snap.Sources["auto_trade"] = SourceMeta{UpdatedAt: now}
		if s.changed("auto_trade", autoTrade) {
			events = append(events, Event{Type: "auto_trade", Data: autoTrade})
		}
	}
	if proposals, err := s.client.TradeProposalsSnapshot(ctx, rpc.TradeProposalSnapshotParams{}); err != nil {
		errors = append(errors, sourceErr("proposals", err, now))
		snap.Sources["proposals"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Proposals = proposals
		snap.Sources["proposals"] = SourceMeta{UpdatedAt: now}
		if s.changed("proposals", proposals) {
			events = append(events, Event{Type: "proposals", Data: proposals})
		}
	}
	if opportunities, err := s.client.OpportunitiesSnapshot(ctx, rpc.OpportunitySnapshotParams{}); err != nil {
		errors = append(errors, sourceErr("opportunities", err, now))
		snap.Sources["opportunities"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Opportunities = opportunities
		snap.Sources["opportunities"] = SourceMeta{UpdatedAt: now}
		if s.changed("opportunities", opportunities) {
			events = append(events, Event{Type: "opportunities", Data: opportunities})
		}
	}
	if settings, err := s.client.Settings(ctx); err != nil {
		errors = append(errors, sourceErr("settings", err, now))
		snap.Sources["settings"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Settings = settings
		snap.Sources["settings"] = SourceMeta{UpdatedAt: now}
		if s.changed("settings", settings) {
			events = append(events, Event{Type: "settings", Data: settings})
		}
	}
	if pollNudges {
		if nudges, err := s.client.NudgesSnapshot(ctx); err != nil {
			snap.Sources["nudges"] = sourceUnavailable(snap.Sources["nudges"], now)
		} else if nudges == nil {
			snap.Sources["nudges"] = sourceUnavailable(snap.Sources["nudges"], now)
		} else {
			nudges.SourceHealth = rpc.NormalizeNudgeSourceHealth(nudges.SourceHealth, len(nudges.Candidates))
			snap.Nudges = nudges
			snap.Sources["nudges"] = sourceCurrent(now)
			if s.changed("nudges", nudges) {
				events = append(events, Event{Type: "nudges", Data: projectPublicNudges(nudges)})
			}
		}
		s.mu.Lock()
		s.nextNudges = now.Add(nudgesPollEvery)
		s.mu.Unlock()
	}
	if pollCanary {
		canary, regime, err := s.client.CanaryWithRegime(ctx)
		if err != nil || canary == nil || regime == nil {
			snap.Sources["canary"] = sourceUnavailable(snap.Sources["canary"], now)
			snap.Sources["regime"] = sourceUnavailable(snap.Sources["regime"], now)
			switch {
			case err != nil:
				errors = append(errors, sourceErr("canary", err, now))
				if strings.HasPrefix(err.Error(), "regime:") {
					errors = append(errors, sourceErr("regime", err, now))
				}
			default:
				if canary == nil {
					errors = append(errors, sourceErr("canary", errCanaryResultUnavailable, now))
				}
				if regime == nil {
					errors = append(errors, sourceErr("regime", errRegimeResultUnavailable, now))
				}
			}
		} else {
			snap.Regime = regime
			snap.Sources["regime"] = regimeSourceMeta(snap.Sources["regime"], now, regime)
			if s.changed("regime", regime) {
				events = append(events, Event{Type: "regime", Data: regime})
			}
			snap.Canary = canary
			snap.Sources["canary"] = sourceCurrent(now)
			if s.changed("canary", canary) {
				events = append(events, Event{Type: "canary", Data: canary})
			}
		}
		// Rules ride the canary cadence: same inputs (positions/account),
		// same daily-discipline freshness needs, no extra poll knob. Observe
		// them before reading the source-neutral snapshot so the snapshot
		// includes this cycle's complete unfiltered Rulebook evaluation.
		if rules, err := s.client.Rules(ctx); err != nil {
			errors = append(errors, sourceErr("rules", err, now))
			snap.Sources["rules"] = sourceUnavailable(snap.Sources["rules"], now)
		} else if rules == nil {
			errors = append(errors, sourceErr("rules", errRulesResultUnavailable, now))
			snap.Sources["rules"] = sourceUnavailable(snap.Sources["rules"], now)
		} else {
			snap.Rules = rules
			snap.Sources["rules"] = sourceCurrent(now)
			if s.changed("rules", rules) {
				events = append(events, Event{Type: "rules", Data: rules})
			}
		}
		// The daemon observes Order Integrity from this same read. The app does
		// not run a parallel order-mismatch watch; the composed candidate
		// snapshot below is the only alert input.
		_, _ = s.client.OrdersOpen(ctx, rpc.OrdersOpenParams{})
		// Canary, Rulebook, and Order Integrity have now all observed this
		// cycle. Read their composed source-neutral snapshot without a second
		// broker evaluation or a full-cycle lag.
		if alertClient, ok := s.client.(alertCandidateClient); ok {
			alertSnapshot, source, err := s.pollAlertCandidates(ctx, alertClient, now)
			snap.AlertCandidates = alertSnapshot
			snap.Sources["alert_candidates"] = source
			if err != nil {
				errorSource := "alert_candidates"
				if isAlertDeliveryDispatchError(err) {
					errorSource = "alert_delivery"
				}
				errors = append(errors, sourceErr(errorSource, err, now))
			}
		}
		// The brief composes canary and other daily-discipline inputs, so it
		// shares this one-minute cadence instead of the five-second app poll.
		if brief, err := s.client.Brief(ctx); err != nil {
			errors = append(errors, sourceErr("brief", err, now))
			snap.Sources["brief"] = sourceUnavailable(snap.Sources["brief"], now)
		} else if brief == nil {
			errors = append(errors, sourceErr("brief", errBriefResultUnavailable, now))
			snap.Sources["brief"] = sourceUnavailable(snap.Sources["brief"], now)
		} else {
			snap.Brief = brief
			snap.Sources["brief"] = sourceCurrent(now)
			if s.changed("brief", brief) {
				events = append(events, Event{Type: "brief", Data: brief})
			}
		}
		s.mu.Lock()
		s.nextCanary = now.Add(s.canaryEvery)
		s.mu.Unlock()
	}

	s.mu.Lock()
	snap.AlertCandidates = cloneAlertCandidateSnapshot(s.snapshot.AlertCandidates)
	if source, ok := s.snapshot.Sources["alert_candidates"]; ok {
		snap.Sources["alert_candidates"] = source
	}
	snap.UpdatedAt = now
	snap.Errors = errors
	snap.Version++
	s.snapshot = snap
	out := cloneSnapshot(s.snapshot)
	s.mu.Unlock()

	events = append(events, Event{Type: "snapshot", Data: out})
	for _, ev := range events {
		s.publish(ev)
	}
	return out
}

func (s *Service) pollAlertCandidates(ctx context.Context, client alertCandidateClient, now time.Time) (*rpc.AlertCandidateSnapshot, SourceMeta, error) {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	// Reserve observation order before the RPC. Otherwise the freshness guard
	// can persist a later synthetic AsOf while this producer read is in flight.
	snapshot, err := client.AlertCandidates(ctx)
	s.mu.Lock()
	prior := cloneAlertCandidateSnapshot(s.snapshot.AlertCandidates)
	priorSource := s.snapshot.Sources["alert_candidates"]
	authority := s.alertAuthority
	s.mu.Unlock()

	current, source, observeErr := observeAlertCandidates(ctx, snapshot, err, authority, now, s.canaryEvery, prior, priorSource)
	s.mu.Lock()
	s.snapshot.AlertCandidates = cloneAlertCandidateSnapshot(current)
	s.snapshot.Sources["alert_candidates"] = source
	s.mu.Unlock()
	return current, source, observeErr
}

func observeAlertCandidates(
	ctx context.Context,
	snapshot *rpc.AlertCandidateSnapshot,
	err error,
	authority AlertSnapshotAuthority,
	now time.Time,
	maxAge time.Duration,
	prior *rpc.AlertCandidateSnapshot,
	priorSource SourceMeta,
) (*rpc.AlertCandidateSnapshot, SourceMeta, error) {
	if err != nil || snapshot == nil {
		if err == nil {
			err = errAlertCandidatesUnavailable
		}
		unavailable := unavailableAlertCandidateSnapshot(now, nil, prior, currentAlertDeliveryView(authority, now))
		if persistErr := observeUnavailableAlertSnapshot(ctx, authority, unavailable); persistErr != nil {
			return unavailable, sourceUnavailableWithReason(priorSource, now, SourceReasonPersistenceUnavailable), errors.Join(err, persistErr)
		}
		return unavailable, sourceUnavailable(priorSource, now), err
	}
	if err := rpc.ValidateAlertCandidateSnapshot(*snapshot); err != nil {
		unavailable := unavailableAlertCandidateSnapshot(now, nil, prior, currentAlertDeliveryView(authority, now))
		if persistErr := observeUnavailableAlertSnapshot(ctx, authority, unavailable); persistErr != nil {
			return unavailable, sourceUnavailableWithReason(priorSource, now, SourceReasonPersistenceUnavailable), errors.Join(err, persistErr)
		}
		return unavailable, sourceUnavailableWithReason(priorSource, now, SourceReasonProducerUnavailable), err
	}

	current := cloneAlertCandidateSnapshot(snapshot)
	if prior != nil && prior.AuthorityScope != current.AuthorityScope {
		// SourceMeta is public transport/freshness posture. Never carry a last
		// success timestamp from a different private broker authority.
		priorSource = SourceMeta{}
	}
	if alertCandidateSnapshotExpired(current, now, maxAge) {
		current = staleAlertCandidateSnapshot(now, current)
	}
	if authority != nil {
		if applied, err := authority.Observe(ctx, *current); err != nil {
			if alertSnapshotObservationApplied(applied, current) {
				return current, alertCandidateSourceMeta(priorSource, now, current), errors.Join(errAlertDeliveryDispatch, err)
			}
			fallbackSeed := current
			reason := SourceReasonPersistenceUnavailable
			if errors.Is(err, state.ErrAlertDeliveryOldSnapshot) {
				fallbackSeed = nil
				reason = SourceReasonProducerUnavailable
			}
			unavailable := unavailableAlertCandidateSnapshot(now, fallbackSeed, prior, authority.Current(now))
			if persistErr := observeUnavailableAlertSnapshot(ctx, authority, unavailable); persistErr != nil {
				err = errors.Join(err, persistErr)
				reason = SourceReasonPersistenceUnavailable
			}
			return unavailable, sourceUnavailableWithReason(priorSource, now, reason), err
		}
	}
	return current, alertCandidateSourceMeta(priorSource, now, current), nil
}

func observeUnavailableAlertSnapshot(ctx context.Context, authority AlertSnapshotAuthority, snapshot *rpc.AlertCandidateSnapshot) error {
	if authority == nil || snapshot == nil {
		return nil
	}
	_, err := authority.Observe(ctx, *snapshot)
	return err
}

func isAlertDeliveryDispatchError(err error) bool {
	return errors.Is(err, errAlertDeliveryDispatch)
}

func alertSnapshotObservationApplied(view state.AlertDeliveryView, snapshot *rpc.AlertCandidateSnapshot) bool {
	return snapshot != nil && view.Initialized && view.AuthorityScope == snapshot.AuthorityScope && view.AsOf.Equal(snapshot.AsOf)
}

func currentAlertDeliveryView(authority AlertSnapshotAuthority, now time.Time) state.AlertDeliveryView {
	if authority == nil {
		return state.AlertDeliveryView{}
	}
	return authority.Current(now)
}

func unavailableAlertCandidateSnapshot(now time.Time, valid, prior *rpc.AlertCandidateSnapshot, view state.AlertDeliveryView) *rpc.AlertCandidateSnapshot {
	asOf := now
	var expected []rpc.AlertSource
	authorityScope := ""
	if valid != nil && len(valid.Coverage.ExpectedSources) > 0 {
		expected = append([]rpc.AlertSource{}, valid.Coverage.ExpectedSources...)
		authorityScope = valid.AuthorityScope
		if !valid.AsOf.Before(asOf) {
			asOf = valid.AsOf.Add(time.Nanosecond)
		}
	}
	if authorityScope == "" {
		authorityScope = view.AuthorityScope
	}
	if len(expected) == 0 && view.Initialized && len(view.Coverage.ExpectedSources) > 0 {
		expected = append([]rpc.AlertSource{}, view.Coverage.ExpectedSources...)
	}
	if view.Initialized && !view.AsOf.Before(asOf) {
		asOf = view.AsOf.Add(time.Nanosecond)
	}
	if len(expected) == 0 && prior != nil && len(prior.Coverage.ExpectedSources) > 0 {
		expected = append([]rpc.AlertSource{}, prior.Coverage.ExpectedSources...)
		if !prior.AsOf.Before(asOf) {
			asOf = prior.AsOf.Add(time.Nanosecond)
		}
	}
	if authorityScope == "" && prior != nil {
		authorityScope = prior.AuthorityScope
	}
	if len(expected) == 0 || authorityScope == "" {
		return nil
	}
	slices.Sort(expected)
	sources := make([]rpc.AlertSourceCoverage, 0, len(expected))
	for _, source := range expected {
		sources = append(sources, rpc.AlertSourceCoverage{
			Source: source, Status: "unavailable", Reason: "producer_unavailable",
			EvidenceHealth: rpc.AlertEvidenceUnavailable,
		})
	}
	return &rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AuthorityScope: authorityScope,
		AsOf:           asOf,
		CurrentState:   rpc.AlertSnapshotUnknown,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageUnavailable,
			Freshness:       rpc.AlertCoverageUnknown,
			AsOf:            asOf,
			ExpectedSources: expected,
			CoveredSources:  []rpc.AlertSource{},
		},
		Sources:    sources,
		Candidates: []rpc.AlertCandidate{},
	}
}

func alertCandidateSourceMeta(prior SourceMeta, now time.Time, snapshot *rpc.AlertCandidateSnapshot) SourceMeta {
	if snapshot == nil || snapshot.Coverage.State == rpc.AlertCoverageUnavailable || snapshot.Coverage.Freshness == rpc.AlertCoverageUnknown {
		return sourceUnavailableWithReason(prior, now, SourceReasonProducerUnavailable)
	}
	if snapshot.Coverage.Freshness == rpc.AlertCoverageStale {
		return SourceMeta{UpdatedAt: now, LastSuccessAt: snapshot.Coverage.AsOf.UTC(), State: SourceStateStale, Reason: SourceReasonPollStale}
	}
	return SourceMeta{UpdatedAt: now, LastSuccessAt: snapshot.Coverage.AsOf.UTC(), State: SourceStateCurrent}
}

func alertCandidateSnapshotExpired(snapshot *rpc.AlertCandidateSnapshot, now time.Time, maxAge time.Duration) bool {
	if snapshot == nil || snapshot.Coverage.Freshness != rpc.AlertCoverageCurrent || snapshot.Coverage.AsOf.IsZero() || maxAge <= 0 {
		return false
	}
	return now.After(snapshot.Coverage.AsOf.Add(maxAge))
}

func regimeSourceMeta(prior SourceMeta, now time.Time, regime *rpc.RegimeMonitorResult) SourceMeta {
	if regime == nil || regime.AuthorityHealth == nil {
		return sourceCurrent(now)
	}
	health := regime.AuthorityHealth
	switch health.Status {
	case rpc.RegimeAuthorityFresh:
		return SourceMeta{UpdatedAt: now, LastSuccessAt: now, State: SourceStateCurrent}
	case rpc.RegimeAuthorityStale:
		return SourceMeta{UpdatedAt: now, LastSuccessAt: now, State: SourceStateStale, Reason: SourceReasonPollStale}
	default:
		return sourceUnavailable(prior, now)
	}
}

// PollNudgesOnce exercises the dedicated one-minute governance source without
// polling account or market surfaces. Daemon evaluator health remains inside
// Nudges; this method owns only app transport freshness.
func (s *Service) PollNudgesOnce(ctx context.Context) Snapshot {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	now := s.now().UTC()
	s.mu.Lock()
	snap := cloneSnapshot(s.snapshot)
	if snap.Sources == nil {
		snap.Sources = map[string]SourceMeta{}
	}
	s.mu.Unlock()
	var events []Event
	if nudges, err := s.client.NudgesSnapshot(ctx); err != nil {
		snap.Sources["nudges"] = sourceUnavailable(snap.Sources["nudges"], now)
	} else if nudges == nil {
		snap.Sources["nudges"] = sourceUnavailable(snap.Sources["nudges"], now)
	} else {
		nudges.SourceHealth = rpc.NormalizeNudgeSourceHealth(nudges.SourceHealth, len(nudges.Candidates))
		snap.Nudges = nudges
		snap.Sources["nudges"] = sourceCurrent(now)
		if s.changed("nudges", nudges) {
			events = append(events, Event{Type: "nudges", Data: projectPublicNudges(nudges)})
		}
	}
	s.mu.Lock()
	s.nextNudges = now.Add(nudgesPollEvery)
	snap.UpdatedAt = now
	snap.Version++
	s.snapshot = snap
	out := cloneSnapshot(s.snapshot)
	s.mu.Unlock()
	events = append(events, Event{Type: "snapshot", Data: out})
	for _, event := range events {
		s.publish(event)
	}
	return out
}

func liveMarketEventSymbols(positions *rpc.PositionsResult) []string {
	if positions == nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(value string) {
		sym := normalizeQuoteLabel(value)
		if sym == "" || seen[sym] {
			return
		}
		seen[sym] = true
		out = append(out, sym)
	}
	for _, stock := range positions.Stocks {
		if !rpc.ExpectsMarketData(stock) {
			continue
		}
		add(stock.Symbol)
	}
	for _, group := range positions.ByUnderlying {
		if !rpc.ExpectsMarketDataGroup(group) {
			continue
		}
		add(group.Underlying)
		if group.Stock != nil {
			add(group.Stock.Symbol)
		}
		for _, opt := range group.Options {
			add(opt.Symbol)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Service) publishSnapshot(now time.Time, snap Snapshot, errors []SourceError, events []Event) Snapshot {
	s.mu.Lock()
	snap.UpdatedAt = now
	snap.Errors = errors
	snap.Version++
	s.snapshot = snap
	out := cloneSnapshot(s.snapshot)
	s.mu.Unlock()

	events = append(events, Event{Type: "snapshot", Data: out})
	for _, ev := range events {
		s.publish(ev)
	}
	return out
}

type marketQuoteContract struct {
	label    string
	contract rpc.ContractParams
}

var marketQuoteContracts = []marketQuoteContract{
	{
		label:    "SPY",
		contract: rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
	},
	{
		label:    "QQQ",
		contract: rpc.ContractParams{Symbol: "QQQ", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
	},
	{
		label:    "IWM",
		contract: rpc.ContractParams{Symbol: "IWM", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
	},
	{
		label:    "VIX",
		contract: rpc.ContractParams{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
	},
	{
		label:    "HYG",
		contract: rpc.ContractParams{Symbol: "HYG", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
	},
	{
		label:    "TLT",
		contract: rpc.ContractParams{Symbol: "TLT", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
	},
}

const maxUnderlyingQuoteContracts = 24

func (s *Service) marketQuotes(ctx context.Context, now time.Time, positions *rpc.PositionsResult, existing *MarketQuotes) (*MarketQuotes, error) {
	type result struct {
		label string
		quote *rpc.Quote
		err   error
	}
	freshFor := max(2*s.pollEvery, 15*time.Second)
	contracts := marketQuoteContractsFor(positions, existing, now, freshFor)
	results := make(chan result, len(contracts))
	var wg sync.WaitGroup
	for _, item := range contracts {
		wg.Go(func() {
			quote, err := s.client.Quote(ctx, item.contract)
			results <- result{label: item.label, quote: quote, err: err}
		})
	}
	wg.Wait()
	close(results)

	out := &MarketQuotes{
		AsOf:   now,
		Quotes: map[string]rpc.Quote{},
		Errors: map[string]string{},
	}
	for res := range results {
		if res.err != nil {
			out.Errors[res.label] = res.err.Error()
			continue
		}
		if res.quote != nil {
			out.Quotes[res.label] = *res.quote
		}
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	if len(out.Quotes) == 0 {
		out.Quotes = nil
	}
	if len(out.Errors) > 0 {
		return out, errors.New(marketQuoteError(out.Errors))
	}
	return out, nil
}

func marketQuoteContractsFor(positions *rpc.PositionsResult, existing *MarketQuotes, now time.Time, freshFor time.Duration) []marketQuoteContract {
	out := make([]marketQuoteContract, 0, len(marketQuoteContracts)+maxUnderlyingQuoteContracts)
	seen := map[string]bool{}
	for _, item := range marketQuoteContracts {
		label := normalizeQuoteLabel(item.label)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		if marketQuoteFresh(existing, label, now, freshFor) {
			continue
		}
		out = append(out, item)
	}
	if positions == nil {
		return out
	}
	added := 0
	for _, group := range positions.ByUnderlying {
		if added >= maxUnderlyingQuoteContracts {
			break
		}
		if !rpc.ExpectsMarketDataGroup(group) {
			continue
		}
		item, ok := underlyingQuoteContract(group)
		if !ok {
			continue
		}
		label := normalizeQuoteLabel(item.label)
		if label == "" || seen[label] {
			continue
		}
		item.label = label
		item.contract.Symbol = label
		out = append(out, item)
		seen[label] = true
		added++
	}
	return out
}

func marketQuoteFresh(existing *MarketQuotes, label string, now time.Time, maxAge time.Duration) bool {
	if existing == nil || maxAge <= 0 {
		return false
	}
	quote, ok := existing.Quotes[normalizeQuoteLabel(label)]
	if !ok {
		return false
	}
	at := quoteFreshnessTime(quote)
	if at.IsZero() {
		at = existing.AsOf
	}
	if at.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if at.After(now) {
		return true
	}
	return now.Sub(at) <= maxAge
}

func quoteFreshnessTime(quote rpc.Quote) time.Time {
	switch {
	case !quote.QuotePriceAt.IsZero():
		return quote.QuotePriceAt
	case !quote.PriceAt.IsZero():
		return quote.PriceAt
	case !quote.AsOf.IsZero():
		return quote.AsOf
	default:
		return time.Time{}
	}
}

func underlyingQuoteContract(group rpc.PositionGroup) (marketQuoteContract, bool) {
	symbol := normalizeQuoteLabel(group.Underlying)
	if symbol == "" && group.Stock != nil {
		symbol = normalizeQuoteLabel(group.Stock.Symbol)
	}
	if symbol == "" && len(group.Options) > 0 {
		symbol = normalizeQuoteLabel(group.Options[0].Symbol)
	}
	if symbol == "" {
		return marketQuoteContract{}, false
	}

	if group.Stock != nil {
		contract := stockPositionQuoteContract(*group.Stock)
		contract.Symbol = symbol
		if contract.Currency == "" {
			contract.Currency = underlyingGroupCurrency(group)
		}
		return marketQuoteContract{label: symbol, contract: contract}, true
	}

	contract := fallbackUnderlyingQuoteContract(symbol, underlyingGroupCurrency(group))
	return marketQuoteContract{label: symbol, contract: contract}, true
}

func stockPositionQuoteContract(stock rpc.PositionView) rpc.ContractParams {
	contract := rpc.ContractParams{
		ConID:        stock.ConID,
		Symbol:       normalizeQuoteLabel(stock.Symbol),
		SecType:      requestQuoteSecType(stock.SecType),
		Exchange:     strings.ToUpper(strings.TrimSpace(stock.Exchange)),
		Currency:     normalizeQuoteLabel(stock.Currency),
		LocalSymbol:  strings.TrimSpace(stock.LocalSymbol),
		TradingClass: strings.TrimSpace(stock.TradingClass),
		Multiplier:   stock.Multiplier,
	}
	if contract.SecType == "" {
		contract.SecType = "STK"
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if contract.Exchange == "" && contract.ConID == 0 {
		contract.Exchange = "SMART"
	}
	return contract
}

func fallbackUnderlyingQuoteContract(symbol, currency string) rpc.ContractParams {
	contract := rpc.ContractParams{
		Symbol:   symbol,
		SecType:  "STK",
		Exchange: "SMART",
		Currency: normalizeQuoteLabel(currency),
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if index, ok := indexUnderlyingContracts[symbol]; ok {
		return index
	}
	return contract
}

var indexUnderlyingContracts = map[string]rpc.ContractParams{
	"SPX": {Symbol: "SPX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
	"NDX": {Symbol: "NDX", SecType: "IND", Exchange: "NASDAQ", PrimaryExch: "NASDAQ", Currency: "USD"},
	"RUT": {Symbol: "RUT", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
	"VIX": {Symbol: "VIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
}

func requestQuoteSecType(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "":
		return "STK"
	case "IND", "INDEX":
		return "IND"
	default:
		return ""
	}
}

func underlyingGroupCurrency(group rpc.PositionGroup) string {
	if group.Stock != nil {
		if ccy := normalizeQuoteLabel(group.Stock.Currency); ccy != "" {
			return ccy
		}
	}
	for _, option := range group.Options {
		if ccy := normalizeQuoteLabel(option.Currency); ccy != "" {
			return ccy
		}
	}
	return "USD"
}

func normalizeQuoteLabel(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func (s *Service) applyMarketQuoteFrame(label string, frame rpc.Frame) {
	now := s.now().UTC()
	s.mu.Lock()
	if s.snapshot.Quotes == nil {
		s.snapshot.Quotes = &MarketQuotes{}
	}
	if s.snapshot.Quotes.Quotes == nil {
		s.snapshot.Quotes.Quotes = map[string]rpc.Quote{}
	}
	if frame.Error != nil {
		if s.snapshot.Quotes.Errors == nil {
			s.snapshot.Quotes.Errors = map[string]string{}
		}
		s.snapshot.Quotes.Errors[label] = frame.Error.Code + ": " + frame.Error.Message
		s.snapshot.Quotes.AsOf = now
		out := cloneMarketQuotes(s.snapshot.Quotes)
		s.mu.Unlock()
		s.publish(Event{Type: "market_quotes", Data: out})
		return
	}

	if s.snapshot.Quotes.Errors != nil {
		delete(s.snapshot.Quotes.Errors, label)
		if len(s.snapshot.Quotes.Errors) == 0 {
			s.snapshot.Quotes.Errors = nil
		}
	}
	quote := s.snapshot.Quotes.Quotes[label]
	if quote.Symbol == "" {
		quote.Symbol = label
	}
	if frame.T.IsZero() {
		frame.T = now
	}
	quote.AsOf = frame.T
	quote.PriceAt = frame.T
	quote.QuotePriceAt = frame.T
	if frame.Bid != nil {
		quote.Bid = frame.Bid
	}
	if frame.Ask != nil {
		quote.Ask = frame.Ask
	}
	if frame.Last != nil {
		quote.Last = frame.Last
	}
	if frame.BidSize != nil {
		quote.BidSize = frame.BidSize
	}
	if frame.AskSize != nil {
		quote.AskSize = frame.AskSize
	}
	if frame.DataType != "" {
		quote.DataType = frame.DataType
	}
	if price := marketQuoteFramePrice(frame); price != nil {
		quote.Price = price
		quote.QuotePrice = price
		quote.PriceSource = marketQuoteFramePriceSource(frame)
		quote.QuotePriceSource = quote.PriceSource
		if quote.PrevClose != nil && *quote.PrevClose != 0 {
			change := *price - *quote.PrevClose
			changePct := change / *quote.PrevClose * 100
			quote.Change = new(change)
			quote.ChangePct = new(changePct)
			quote.QuoteChange = new(change)
			quote.QuoteChangePct = new(changePct)
		}
	}
	s.snapshot.Quotes.Quotes[label] = quote
	s.snapshot.Quotes.AsOf = now
	out := cloneMarketQuotes(s.snapshot.Quotes)
	s.mu.Unlock()
	s.publish(Event{Type: "market_quotes", Data: out})
}

func (s *Service) applyMarketQuoteError(label string, err error) {
	now := s.now().UTC()
	s.mu.Lock()
	if s.snapshot.Quotes == nil {
		s.snapshot.Quotes = &MarketQuotes{}
	}
	if s.snapshot.Quotes.Errors == nil {
		s.snapshot.Quotes.Errors = map[string]string{}
	}
	s.snapshot.Quotes.Errors[label] = err.Error()
	s.snapshot.Quotes.AsOf = now
	out := cloneMarketQuotes(s.snapshot.Quotes)
	s.mu.Unlock()
	s.publish(Event{Type: "market_quotes", Data: out})
}

func marketQuoteFramePrice(frame rpc.Frame) *float64 {
	if frame.Last != nil {
		return new(*frame.Last)
	}
	if frame.Bid != nil && frame.Ask != nil {
		return new((*frame.Bid + *frame.Ask) / 2)
	}
	if frame.Bid != nil {
		return new(*frame.Bid)
	}
	if frame.Ask != nil {
		return new(*frame.Ask)
	}
	return nil
}

func marketQuoteFramePriceSource(frame rpc.Frame) string {
	if frame.Last != nil {
		return "last"
	}
	if frame.Bid != nil && frame.Ask != nil {
		return "midpoint"
	}
	if frame.Bid != nil {
		return "bid"
	}
	if frame.Ask != nil {
		return "ask"
	}
	return ""
}

func marketQuoteError(errs map[string]string) string {
	if len(errs) == 0 {
		return ""
	}
	normalized := map[string]string{}
	for symbol, msg := range errs {
		if label := normalizeQuoteLabel(symbol); label != "" && msg != "" {
			normalized[label] = msg
		}
	}
	parts := make([]string, 0, len(errs))
	seen := map[string]bool{}
	for _, symbol := range []string{"SPY", "QQQ", "IWM", "VIX", "HYG", "TLT"} {
		if msg := normalized[symbol]; msg != "" {
			parts = append(parts, symbol+": "+msg)
			seen[symbol] = true
		}
	}
	rest := make([]string, 0, len(errs))
	for symbol := range normalized {
		if symbol != "" && !seen[symbol] {
			rest = append(rest, symbol)
			seen[symbol] = true
		}
	}
	slices.Sort(rest)
	for _, symbol := range rest {
		if msg := normalized[symbol]; msg != "" {
			parts = append(parts, symbol+": "+msg)
		}
	}
	return strings.Join(parts, " | ")
}

func (s *Service) runAlertFreshnessGuard(ctx context.Context) {
	timer := time.NewTimer(s.alertFreshnessDelay(s.now().UTC()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			now := s.now().UTC()
			s.expireAlertSnapshot(now)
			timer.Reset(s.alertFreshnessDelay(now))
		}
	}
}

func (s *Service) alertFreshnessDelay(now time.Time) time.Duration {
	s.mu.Lock()
	source := s.snapshot.Sources["alert_candidates"]
	s.mu.Unlock()
	if source.State != SourceStateCurrent || source.LastSuccessAt.IsZero() {
		return s.canaryEvery
	}
	delay := source.LastSuccessAt.Add(s.canaryEvery).Add(time.Nanosecond).Sub(now)
	if delay < time.Millisecond {
		return time.Millisecond
	}
	return delay
}

func (s *Service) expireAlertSnapshot(now time.Time) {
	s.alertMu.Lock()
	defer s.alertMu.Unlock()
	s.mu.Lock()
	prior := cloneAlertCandidateSnapshot(s.snapshot.AlertCandidates)
	priorSource := s.snapshot.Sources["alert_candidates"]
	authority := s.alertAuthority
	s.mu.Unlock()
	if prior == nil || priorSource.State != SourceStateCurrent || priorSource.LastSuccessAt.IsZero() || now.Sub(priorSource.LastSuccessAt) <= s.canaryEvery {
		return
	}

	aged := staleAlertCandidateSnapshot(now, prior)
	source := SourceMeta{
		UpdatedAt: now, LastSuccessAt: priorSource.LastSuccessAt,
		State: SourceStateStale, Reason: SourceReasonPollStale,
	}
	if authority != nil {
		if applied, err := authority.Observe(context.Background(), *aged); err != nil && !alertSnapshotObservationApplied(applied, aged) {
			aged = unavailableAlertCandidateSnapshot(now, nil, prior, authority.Current(now))
			source = sourceUnavailableWithReason(priorSource, now, SourceReasonPersistenceUnavailable)
			if aged != nil {
				_, _ = authority.Observe(context.Background(), *aged)
			}
		}
	}
	s.mu.Lock()
	s.snapshot.AlertCandidates = cloneAlertCandidateSnapshot(aged)
	s.snapshot.Sources["alert_candidates"] = source
	s.snapshot.UpdatedAt = now
	s.snapshot.Version++
	s.mu.Unlock()
}

// Snapshot returns a cloned cache view without performing daemon RPC. It ages
// source metadata at read time and may fail-close an expired alert-candidate
// snapshot through the configured app store.
func (s *Service) Snapshot() Snapshot {
	now := s.now().UTC()
	s.expireAlertSnapshot(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := cloneSnapshot(s.snapshot)
	ageSource := func(name string, maxAge time.Duration) {
		source, ok := out.Sources[name]
		if !ok || source.State != SourceStateCurrent || source.LastSuccessAt.IsZero() || now.Sub(source.LastSuccessAt) <= maxAge {
			return
		}
		source.State = SourceStateStale
		source.Reason = SourceReasonPollStale
		out.Sources[name] = source
	}
	ageSource("nudges", nudgesPollEvery)
	for _, name := range []string{"canary", "regime", "rules", "brief"} {
		ageSource(name, s.canaryEvery)
	}
	return out
}

func staleAlertCandidateSnapshot(now time.Time, snapshot *rpc.AlertCandidateSnapshot) *rpc.AlertCandidateSnapshot {
	if snapshot == nil {
		return nil
	}
	out := cloneAlertCandidateSnapshot(snapshot)
	out.AsOf = now
	out.Coverage.Freshness = rpc.AlertCoverageStale
	for i := range out.Sources {
		if !out.Sources[i].Covered {
			continue
		}
		out.Sources[i].Status = "stale"
		out.Sources[i].Reason = "freshness_expired"
		out.Sources[i].EvidenceHealth = rpc.AlertEvidenceStale
	}
	staleSources := make(map[rpc.AlertSource]bool, len(out.Sources))
	for _, source := range out.Sources {
		if source.Covered && source.EvidenceHealth == rpc.AlertEvidenceStale {
			staleSources[source.Source] = true
		}
	}
	for i := range out.Candidates {
		candidate := &out.Candidates[i]
		if staleSources[candidate.Source] && candidate.EvidenceHealth == rpc.AlertEvidenceCurrent &&
			(candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated) {
			candidate.EvidenceHealth = rpc.AlertEvidenceStale
		}
	}
	hasActive := false
	for _, candidate := range out.Candidates {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			hasActive = true
			break
		}
	}
	if hasActive {
		out.CurrentState = rpc.AlertSnapshotActive
	} else {
		out.CurrentState = rpc.AlertSnapshotUnknown
	}
	return out
}

// Diagnostics returns copied subscriber and publication metadata.
func (s *Service) Diagnostics() Diagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := make(map[string]time.Time, len(s.lastEventAt))
	maps.Copy(last, s.lastEventAt)
	return Diagnostics{Subscribers: len(s.subs), LastEventAt: last, Version: s.snapshot.Version}
}

// Subscribe registers a bounded best-effort event channel and returns an
// idempotent release function that unregisters and closes it. Slow subscribers
// may miss events and must resynchronize from [Service.Snapshot].
func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, release
}

func (s *Service) publish(ev Event) {
	s.mu.Lock()
	s.lastEventAt[ev.Type] = s.now().UTC()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Service) closeSubscribers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
}

func (s *Service) changed(key string, value any) bool {
	b, err := json.Marshal(value)
	if err != nil {
		return true
	}
	hash := string(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashes[key] == hash {
		return false
	}
	s.hashes[key] = hash
	return true
}

func sourceErr(source string, _ error, at time.Time) SourceError {
	// Errors originate at broker, transport, and daemon boundaries and are
	// therefore untrusted browser input. Preserve the source and observation
	// time for operator diagnostics, but expose only a stable allowlisted
	// message on the app snapshot. Raw causes belong in local logs.
	return SourceError{Source: source, Message: "Source temporarily unavailable.", At: at}
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.Errors = append([]SourceError(nil), in.Errors...)
	out.Sources = maps.Clone(in.Sources)
	out.Quotes = cloneMarketQuotes(in.Quotes)
	if in.MarketEvents != nil {
		events := *in.MarketEvents
		events.Flags = append([]rpc.MarketEventFlag(nil), in.MarketEvents.Flags...)
		events.SourceHealth = append([]rpc.SourceHealth(nil), in.MarketEvents.SourceHealth...)
		events.WarningDetails = append([]rpc.DataWarning(nil), in.MarketEvents.WarningDetails...)
		if in.MarketEvents.BySymbol != nil {
			events.BySymbol = make(map[string][]rpc.MarketEventFlag, len(in.MarketEvents.BySymbol))
			for sym, flags := range in.MarketEvents.BySymbol {
				events.BySymbol[sym] = append([]rpc.MarketEventFlag(nil), flags...)
			}
		}
		out.MarketEvents = &events
	}
	out.Regime = cloneRegimeMonitor(in.Regime)
	out.AlertCandidates = cloneAlertCandidateSnapshot(in.AlertCandidates)
	out.Brief = cloneBriefResult(in.Brief)
	out.Settings = clonePlatformSettings(in.Settings)
	if in.Nudges != nil {
		nudges := *in.Nudges
		nudges.Candidates = append([]rpc.NudgeCandidate(nil), in.Nudges.Candidates...)
		if in.Nudges.Reconciliation != nil {
			reconciliation := *in.Nudges.Reconciliation
			nudges.Reconciliation = &reconciliation
		}
		if in.Nudges.ConfirmedFlowCoverage != nil {
			coverage := *in.Nudges.ConfirmedFlowCoverage
			nudges.ConfirmedFlowCoverage = &coverage
		}
		nudges.Context = cloneNudgeSnapshotContext(in.Nudges.Context)
		out.Nudges = &nudges
	}
	if in.AutoTrade != nil {
		autoTrade := *in.AutoTrade
		autoTrade.Blockers = append([]rpc.TradingBlocker(nil), in.AutoTrade.Blockers...)
		autoTrade.Policy.Blockers = append([]rpc.TradingBlocker(nil), in.AutoTrade.Policy.Blockers...)
		out.AutoTrade = &autoTrade
	}
	if in.Proposals != nil {
		proposals := *in.Proposals
		proposals.Proposals = append([]rpc.TradeProposal(nil), in.Proposals.Proposals...)
		for i := range proposals.Proposals {
			proposals.Proposals[i].Details = append([]string(nil), in.Proposals.Proposals[i].Details...)
			proposals.Proposals[i].MarketFlags = append([]rpc.MarketEventFlag(nil), in.Proposals.Proposals[i].MarketFlags...)
			proposals.Proposals[i].Blockers = append([]rpc.TradingBlocker(nil), in.Proposals.Proposals[i].Blockers...)
		}
		proposals.Blockers = append([]rpc.TradingBlocker(nil), in.Proposals.Blockers...)
		out.Proposals = &proposals
	}
	return out
}

func cloneAlertCandidateSnapshot(in *rpc.AlertCandidateSnapshot) *rpc.AlertCandidateSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	if in.Coverage.ExpectedSources != nil {
		out.Coverage.ExpectedSources = append([]rpc.AlertSource{}, in.Coverage.ExpectedSources...)
	}
	if in.Coverage.CoveredSources != nil {
		out.Coverage.CoveredSources = append([]rpc.AlertSource{}, in.Coverage.CoveredSources...)
	}
	if in.Sources != nil {
		out.Sources = append([]rpc.AlertSourceCoverage{}, in.Sources...)
	}
	if in.Candidates != nil {
		out.Candidates = append([]rpc.AlertCandidate{}, in.Candidates...)
	}
	return &out
}

func cloneNudgeSnapshotContext(in *rpc.NudgeSnapshotContext) *rpc.NudgeSnapshotContext {
	if in == nil {
		return nil
	}
	out := *in
	if in.Shadow != nil {
		shadow := *in.Shadow
		out.Shadow = &shadow
	}
	if in.Drawdown != nil {
		drawdown := *in.Drawdown
		drawdown.ConsumedPct = cloneValue(in.Drawdown.ConsumedPct)
		out.Drawdown = &drawdown
	}
	return &out
}

func cloneBriefResult(in *rpc.BriefResult) *rpc.BriefResult {
	if in == nil {
		return nil
	}
	out := *in

	// Review movement (post-trade).
	out.Review.SessionPnL.EquityBase = cloneValue(in.Review.SessionPnL.EquityBase)
	out.Review.SessionPnL.DailyPnLBase = cloneValue(in.Review.SessionPnL.DailyPnLBase)
	out.Review.Attribution.Rows = append([]rpc.BriefMover(nil), in.Review.Attribution.Rows...)
	out.Review.Attribution.OtherPnLBase = cloneValue(in.Review.Attribution.OtherPnLBase)
	out.Review.RulesDelta.Transitions = append([]rpc.BriefRuleTransition(nil), in.Review.RulesDelta.Transitions...)
	out.Review.RulesDelta.Added = append([]string(nil), in.Review.RulesDelta.Added...)
	out.Review.RulesDelta.Removed = append([]string(nil), in.Review.RulesDelta.Removed...)
	out.Review.Overrides.Rows = append([]rpc.BriefOverride(nil), in.Review.Overrides.Rows...)
	out.Review.CapitalEvents.LatchAgeDays = cloneValue(in.Review.CapitalEvents.LatchAgeDays)
	out.Review.CapitalEvents.ConsumedPctAtLatch = cloneValue(in.Review.CapitalEvents.ConsumedPctAtLatch)
	out.Review.CapitalEvents.AdjustedPeakBase = cloneValue(in.Review.CapitalEvents.AdjustedPeakBase)
	out.Review.Reconcile.DaysRemaining = cloneValue(in.Review.Reconcile.DaysRemaining)
	out.Review.OneTap.Blockers = append([]string(nil), in.Review.OneTap.Blockers...)
	out.Review.WorkingOrders.Count = cloneValue(in.Review.WorkingOrders.Count)

	// Ready movement (pre-trade).
	out.Ready.Breadth.PctAbove50DMA = cloneValue(in.Ready.Breadth.PctAbove50DMA)
	out.Ready.Breadth.PctAbove200DMA = cloneValue(in.Ready.Breadth.PctAbove200DMA)
	out.Ready.Breadth.NetNewHighsPct = cloneValue(in.Ready.Breadth.NetNewHighsPct)
	out.Ready.Gamma.Spot = cloneValue(in.Ready.Gamma.Spot)
	out.Ready.Gamma.ZeroGamma = cloneValue(in.Ready.Gamma.ZeroGamma)
	out.Ready.Gamma.GapPct = cloneValue(in.Ready.Gamma.GapPct)
	out.Ready.MarketEvents = append([]rpc.BriefMarketEventRow(nil), in.Ready.MarketEvents...)
	for i := range out.Ready.MarketEvents {
		out.Ready.MarketEvents[i].Symbols = append([]string(nil), in.Ready.MarketEvents[i].Symbols...)
	}
	out.Ready.Capital.ConsumedPct = cloneValue(in.Ready.Capital.ConsumedPct)
	out.Ready.Capital.DrawdownBase = cloneValue(in.Ready.Capital.DrawdownBase)
	out.Ready.Capital.AdjustedPeakBase = cloneValue(in.Ready.Capital.AdjustedPeakBase)
	out.Ready.Latch.AgeDays = cloneValue(in.Ready.Latch.AgeDays)
	out.Ready.Latch.ConsumedPctAtLatch = cloneValue(in.Ready.Latch.ConsumedPctAtLatch)
	out.Ready.PremiumAtRisk.AmountBase = cloneValue(in.Ready.PremiumAtRisk.AmountBase)
	out.Ready.HedgeCost.AmountBase = cloneValue(in.Ready.HedgeCost.AmountBase)
	out.Ready.PolicyDrift.Rows = append([]rpc.PolicyPinStatus(nil), in.Ready.PolicyDrift.Rows...)
	out.Ready.Artefacts.Rows = append([]rpc.BriefArtefact(nil), in.Ready.Artefacts.Rows...)
	if in.Ready.MonthlyPulse != nil {
		monthly := *in.Ready.MonthlyPulse
		out.Ready.MonthlyPulse = &monthly
	}
	return &out
}

func cloneValue[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func clonePlatformSettings(in *rpc.PlatformSettings) *rpc.PlatformSettings {
	if in == nil {
		return nil
	}
	out := *in
	out.MarketData.Quality.QuoteCounts = maps.Clone(in.MarketData.Quality.QuoteCounts)
	out.MarketData.Quality.DataQuality = append([]rpc.DataQualityHealth(nil), in.MarketData.Quality.DataQuality...)
	return &out
}

func cloneMarketQuotes(in *MarketQuotes) *MarketQuotes {
	if in == nil {
		return nil
	}
	out := *in
	out.Quotes = maps.Clone(in.Quotes)
	out.Errors = maps.Clone(in.Errors)
	return &out
}

func mergeMarketQuotes(existing, update *MarketQuotes) *MarketQuotes {
	if existing == nil {
		return cloneMarketQuotes(update)
	}
	if update == nil {
		return cloneMarketQuotes(existing)
	}
	out := cloneMarketQuotes(existing)
	if out == nil {
		out = &MarketQuotes{}
	}
	if !update.AsOf.IsZero() {
		out.AsOf = update.AsOf
	}
	if len(update.Quotes) > 0 {
		if out.Quotes == nil {
			out.Quotes = map[string]rpc.Quote{}
		}
		if out.Errors != nil {
			for symbol := range update.Quotes {
				delete(out.Errors, symbol)
			}
		}
		maps.Copy(out.Quotes, update.Quotes)
	}
	if len(update.Errors) > 0 {
		if out.Errors == nil {
			out.Errors = map[string]string{}
		}
		maps.Copy(out.Errors, update.Errors)
	} else if len(update.Quotes) > 0 && out.Errors != nil && len(out.Errors) == 0 {
		out.Errors = nil
	}
	if len(out.Quotes) == 0 {
		out.Quotes = nil
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out
}

func cloneRegimeMonitor(in *rpc.RegimeMonitorResult) *rpc.RegimeMonitorResult {
	if in == nil {
		return nil
	}
	out := *in
	if in.AuthorityHealth != nil {
		health := *in.AuthorityHealth
		if in.AuthorityHealth.LastSuccessAt != nil {
			lastSuccess := *in.AuthorityHealth.LastSuccessAt
			health.LastSuccessAt = &lastSuccess
		}
		if in.AuthorityHealth.LastSuccessAgeSeconds != nil {
			age := *in.AuthorityHealth.LastSuccessAgeSeconds
			health.LastSuccessAgeSeconds = &age
		}
		out.AuthorityHealth = &health
	}
	out.Lifecycle.Evidence = append([]rpc.LifecycleEvidence(nil), in.Lifecycle.Evidence...)
	out.Lifecycle.ConfirmedBy = append([]string(nil), in.Lifecycle.ConfirmedBy...)
	out.Lifecycle.Unconfirmed = append([]string(nil), in.Lifecycle.Unconfirmed...)
	out.Lifecycle.Suppressed = append([]string(nil), in.Lifecycle.Suppressed...)
	out.Lifecycle.RejectedBy = append([]string(nil), in.Lifecycle.RejectedBy...)
	out.WarningDetails = append([]rpc.RegimeWarning(nil), in.WarningDetails...)
	out.DataQuality = append([]rpc.DataQualityHealth(nil), in.DataQuality...)
	out.SourceHealth = append([]rpc.CompactSourceHealth(nil), in.SourceHealth...)
	out.Indicators = append([]rpc.RegimeMonitorIndicator(nil), in.Indicators...)
	return &out
}
