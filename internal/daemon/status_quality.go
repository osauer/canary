package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const regimeProjectionConsumerRepairTimeout = 2 * time.Second

func (s *Server) statusDataQuality() []rpc.DataQualityHealth {
	out := []rpc.DataQualityHealth{}
	if s.zeroGamma != nil {
		if q, ok := gammaStatusQuality(s.zeroGamma.snapshotCurrent(rpc.GammaZeroScopeCombined, time.Now)); ok {
			out = append(out, q)
		}
	}
	if s.regimeSnapshots != nil {
		if snapshot, err := s.currentDecisionReadyRegimeSnapshot(s.regimeConsumerContext()); err == nil {
			out = append(out, regimeStatusQuality(snapshot)...)
		} else {
			pending, _ := s.regimeSnapshots.projectionFailure()
			quality := rpc.DataQualityHealth{
				Surface:         "regime",
				Status:          "unavailable",
				Summary:         "unavailable: no completed regime snapshot",
				PartialClusters: []string{"authority"},
			}
			if pending {
				quality.Status = "partial"
				quality.Summary = "partial: snapshot projection repair pending"
				quality.PartialClusters = []string{"projection"}
			}
			// The pending snapshot itself is not exposed as decision evidence,
			// but its observation timestamp still tells the operator which
			// publication is awaiting repair.
			if view, readErr := s.regimeSnapshots.current(); readErr == nil && view.Snapshot != nil {
				quality.AsOf = view.Snapshot.AsOf
			}
			out = append(out, quality)
		}
	}
	return out
}

func (s *Server) regimeConsumerContext() context.Context {
	if s == nil {
		return context.Background()
	}
	s.mu.Lock()
	ctx := s.serverCtx
	s.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// currentDecisionReadyRegimeSnapshot is the non-triggering read boundary for
// daemon consumers whose output can affect a risk decision. A successful
// snapshot CAS can briefly be ahead of its streak, rule-stage, or decision
// journal projection. Such a revision is repaired under a small caller- or
// daemon-bounded context before it is returned; when repair cannot complete,
// callers must withhold the snapshot. This helper calls current, never serve,
// so a brief, Stress journal tick, or status poll cannot start market-data work.
func (s *Server) currentDecisionReadyRegimeSnapshot(ctx context.Context) (*rpc.RegimeSnapshotResult, error) {
	if s == nil || s.regimeSnapshots == nil {
		return nil, fmt.Errorf("no daemon regime snapshot has completed yet")
	}
	cache := s.regimeSnapshots
	view, err := cache.current()
	if err != nil {
		return nil, err
	}
	if view.Snapshot == nil {
		return nil, fmt.Errorf("no daemon regime snapshot has completed yet")
	}

	pending, pendingRevision := cache.projectionFailure()
	if !pending || view.Revision < pendingRevision {
		// If a newer pending revision landed after current(), this detached
		// view is still the fully projected prior last-good and remains safe.
		return view.Snapshot, nil
	}
	if pendingRevision <= 0 || view.Revision != pendingRevision {
		return nil, fmt.Errorf("regime snapshot projection identity is inconsistent")
	}

	parent := ctx
	if parent == nil {
		parent = s.regimeConsumerContext()
	}
	repairCtx, cancel := context.WithTimeout(parent, regimeProjectionConsumerRepairTimeout)
	defer cancel()
	if err := s.repairRegimeSnapshotProjections(repairCtx); err != nil {
		return nil, err
	}

	view, err = cache.current()
	if err != nil {
		return nil, err
	}
	if view.Snapshot == nil {
		return nil, fmt.Errorf("no daemon regime snapshot has completed yet")
	}
	pending, pendingRevision = cache.projectionFailure()
	if pending && view.Revision >= pendingRevision {
		return nil, fmt.Errorf("regime snapshot projection revision %d remains pending", pendingRevision)
	}
	return view.Snapshot, nil
}

func statusDataFarms(farms []ibkrlib.DataFarmStatus) []rpc.DataFarmHealth {
	out := make([]rpc.DataFarmHealth, 0, len(farms))
	for _, farm := range farms {
		if !dataFarmNeedsAttention(farm.Status) {
			continue
		}
		out = append(out, rpc.DataFarmHealth{
			Name:    farm.Name,
			Type:    farm.Type,
			Status:  farm.Status,
			Code:    farm.Code,
			Message: farm.Message,
			AsOf:    farm.AsOf,
		})
	}
	return out
}

type farmReadiness struct {
	status      string
	message     string
	lastError   string
	lastErrorAt time.Time
}

func statusSubsystemFromReadiness(name string, r farmReadiness) rpc.SubsystemHealth {
	status := strings.TrimSpace(r.status)
	if status == "" {
		status = "ready"
	}
	return rpc.SubsystemHealth{
		Name:        name,
		Status:      status,
		Message:     r.message,
		LastError:   r.lastError,
		LastErrorAt: r.lastErrorAt,
	}
}

func marketDataFarmReadiness(connected bool, farms []ibkrlib.DataFarmStatus, impact string) farmReadiness {
	return farmTypeReadiness(connected, farms, "market", "market-data", impact)
}

func historicalDataFarmReadiness(connected bool, farms []ibkrlib.DataFarmStatus) farmReadiness {
	return farmTypeReadiness(connected, farms, "historical", "historical-data", "history and technical screens may time out")
}

func farmTypeReadiness(connected bool, farms []ibkrlib.DataFarmStatus, farmType, label, impact string) farmReadiness {
	if !connected {
		return farmReadiness{status: "unavailable"}
	}
	seenReady := false
	for _, farm := range farms {
		fType := strings.ToLower(strings.TrimSpace(farm.Type))
		status := strings.ToLower(strings.TrimSpace(farm.Status))
		if fType == "connectivity" && dataFarmNeedsAttention(status) {
			return farmImpairedReadiness("unavailable", farm, "TWS/server connectivity")
		}
		if fType != farmType {
			continue
		}
		switch status {
		case "ok", "inactive":
			seenReady = true
		case "disconnected", "broken":
			return farmImpairedReadiness("degraded", farm, label+" farm")
		}
	}
	if seenReady {
		return farmReadiness{status: "ready"}
	}
	return farmReadiness{
		status:  "degraded",
		message: fmt.Sprintf("no %s farm connection notice observed; %s", label, impact),
	}
}

func chainSubsystemHealth(connected bool, farms []ibkrlib.DataFarmStatus) rpc.SubsystemHealth {
	market := marketDataFarmReadiness(connected, farms, "option quote enrichment may time out")
	if market.status != "ready" {
		return statusSubsystemFromReadiness("chain", market)
	}
	for _, farm := range farms {
		if strings.ToLower(strings.TrimSpace(farm.Type)) != "security_definition" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(farm.Status))
		if dataFarmNeedsAttention(status) {
			return statusSubsystemFromReadiness("chain", farmImpairedReadiness("degraded", farm, "security-definition farm"))
		}
	}
	return rpc.SubsystemHealth{Name: "chain", Status: "ready"}
}

func farmImpairedReadiness(status string, farm ibkrlib.DataFarmStatus, label string) farmReadiness {
	farmName := strings.TrimSpace(farm.Name)
	if farmName == "" {
		farmName = strings.TrimSpace(farm.Type)
	}
	farmStatus := strings.TrimSpace(farm.Status)
	if farmStatus == "" {
		farmStatus = "unhealthy"
	}
	message := fmt.Sprintf("%s %s %s", label, farmName, farmStatus)
	if detail := strings.TrimSpace(farm.Message); detail != "" {
		message += ": " + detail
	}
	lastErr := farmStatus
	if farm.Code != 0 {
		lastErr = fmt.Sprintf("IBKR %d %s", farm.Code, farmStatus)
	}
	return farmReadiness{
		status:      status,
		message:     message,
		lastError:   lastErr,
		lastErrorAt: farm.AsOf,
	}
}

func dataFarmNeedsAttention(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "broken", "disconnected":
		return true
	default:
		return false
	}
}

func regimeSnapshotDataQuality(r *rpc.RegimeSnapshotResult) []rpc.DataQualityHealth {
	if r == nil {
		return nil
	}
	out := []rpc.DataQualityHealth{}
	if q, ok := gammaStatusQuality(r.GammaZero.Envelope); ok {
		out = append(out, q)
	}
	out = append(out, regimeStatusQuality(r)...)
	return out
}

func regimeStatusQuality(r *rpc.RegimeSnapshotResult) []rpc.DataQualityHealth {
	if r == nil {
		return nil
	}
	stale := staleRegimeClusters(r)
	partial := partialRegimeClusters(r)
	authoritySummary := ""
	if health := r.AuthorityHealth; health != nil {
		switch health.Status {
		case rpc.RegimeAuthorityStale:
			stale = append(stale, "authority")
			authoritySummary = "authority: stale last-good"
		case rpc.RegimeAuthorityUnavailable:
			partial = append(partial, "authority")
			authoritySummary = "authority: unavailable"
		case rpc.RegimeAuthorityFresh:
			if health.FailureCode != rpc.RegimeAuthorityFailureNone {
				partial = append(partial, "authority")
				authoritySummary = "authority: latest operation failed"
			}
		default:
			partial = append(partial, "authority")
			authoritySummary = "authority: invalid health"
		}
		if authoritySummary != "" && health.FailureCode != rpc.RegimeAuthorityFailureNone {
			authoritySummary += " (" + string(health.FailureCode) + ")"
		}
	}
	if len(stale) == 0 && len(partial) == 0 {
		return nil
	}
	status := "stale"
	var summary []string
	if len(partial) > 0 {
		status = "partial"
		summary = append(summary, "partial: "+strings.Join(partial, ", "))
	}
	if len(stale) > 0 {
		summary = append(summary, "stale: "+strings.Join(stale, ", "))
	}
	if authoritySummary != "" {
		summary = append(summary, authoritySummary)
	}
	q := rpc.DataQualityHealth{
		Surface:         "regime",
		Status:          status,
		StaleClusters:   stale,
		PartialClusters: partial,
		AsOf:            r.AsOf,
	}
	q.Summary = strings.Join(summary, "; ")
	return []rpc.DataQualityHealth{q}
}

func gammaStatusQuality(env rpc.GammaZeroSPXResult) (rpc.DataQualityHealth, bool) {
	return gammaStatusQualityAt(env, time.Now())
}

func gammaStatusQualityAt(env rpc.GammaZeroSPXResult, now time.Time) (rpc.DataQualityHealth, bool) {
	quality, ok := gammaStatusQualityClassified(env)
	if ok {
		quality.CadenceState = gammaOperationalCadence(&env, now)
	}
	return quality, ok
}

func gammaStatusQualityClassified(env rpc.GammaZeroSPXResult) (rpc.DataQualityHealth, bool) {
	switch env.Status {
	case rpc.GammaZeroStatusReady:
		if env.Result == nil {
			return rpc.DataQualityHealth{
				Surface:         "gamma",
				Status:          "partial",
				Summary:         "partial: gamma ready envelope missing result",
				PartialClusters: []string{"gamma"},
				AsOf:            gammaEnvelopeAsOf(env),
			}, true
		}
	case rpc.GammaZeroStatusComputing:
		summary := "partial: gamma computing"
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	case rpc.GammaZeroStatusCold:
		summary := "partial: gamma cold"
		if env.ColdReason != "" {
			summary = "partial: " + env.ColdReason
		}
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	case rpc.GammaZeroStatusError:
		summary := "partial: gamma error"
		if strings.TrimSpace(env.Error) != "" {
			summary = "partial: gamma error: " + strings.TrimSpace(env.Error)
		}
		return rpc.DataQualityHealth{
			Surface:         "gamma",
			Status:          "partial",
			Summary:         summary,
			PartialClusters: []string{"gamma"},
			AsOf:            gammaEnvelopeAsOf(env),
		}, true
	default:
		return rpc.DataQualityHealth{}, false
	}
	if gammaSPXCanonicalRankable(env.Result) && gammaOnlySPYUnavailable(env.Result) {
		return rpc.DataQualityHealth{}, false
	}
	if !gammaResultDegraded(env.Result) {
		return rpc.DataQualityHealth{}, false
	}
	rankability := ""
	if env.Result != nil && env.Result.Quality != nil {
		rankability = env.Result.Quality.Rankability
	}
	status := "degraded"
	summary := "degraded: gamma not rankable"
	if rankability != "" {
		summary = "degraded: gamma " + rankability
	}
	if rankability == rpc.GammaRankabilityBlocked || rankability == rpc.GammaRankabilityUnavailable {
		status = "partial"
	}
	if reason := gammaStatusRankabilityReason(env.Result); reason != "" {
		summary += " (" + reason + ")"
	} else if gammaHasSPYUnavailable(env.Result) {
		summary = "degraded: SPY excluded"
	} else if gammaHasSPXUnavailable(env.Result) {
		summary = "degraded: SPX excluded"
	} else if gammaHasSPXCacheFallback(env.Result) {
		summary = "degraded: SPX cache fallback"
	} else if gammaHasOIMissing(env.Result) {
		switch gammaOIMissingSummaryClass(env.Result, env.Result.AsOf) {
		case "spx":
			summary = "degraded: partial SPX option OI (unexpected: SPX OI should be session-stable)"
		case "rth":
			summary = "degraded: partial option OI (unexpected: sampled during RTH)"
		case "spy_off_hours":
			summary = "degraded: partial SPY option OI (expected: sampled outside RTH)"
		default:
			summary = "degraded: partial option OI"
		}
	}
	return rpc.DataQualityHealth{
		Surface:          "gamma",
		Status:           status,
		Summary:          summary,
		DegradedClusters: []string{"gamma"},
		AsOf:             env.Result.AsOf,
	}, true
}

func gammaStatusRankabilityReason(c *rpc.GammaZeroComputed) string {
	if c == nil || c.Quality == nil {
		return ""
	}
	reason := strings.TrimSpace(c.Quality.RankabilityReason)
	if !strings.EqualFold(reason, "spx_coverage: SPX slice is not rankable") {
		return reason
	}
	spx, ok := c.Quality.ByUnderlying["SPX"]
	if !ok {
		return reason
	}
	if gammaQualityHasBlocker(&spx, "derived_iv_share") || gammaQualityHasBlocker(&spx, "model_source") {
		if spx.Coverage.DerivedIVPct > 0 {
			return "spx_coverage: SPX model source blocked: " + formatStatusPct(spx.Coverage.DerivedIVPct) + " of priced legs used derived IV"
		}
		return "spx_coverage: SPX model source blocked"
	}
	if spx.RankabilityReason != "" {
		return "spx_coverage: SPX " + spx.RankabilityReason
	}
	return reason
}

func formatStatusPct(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f%%", v), "0"), ".")
}

func gammaEnvelopeAsOf(env rpc.GammaZeroSPXResult) time.Time {
	if env.Result != nil && !env.Result.AsOf.IsZero() {
		return env.Result.AsOf
	}
	if env.StartedAt != nil {
		return *env.StartedAt
	}
	return time.Time{}
}

func gammaResultDegraded(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	if c.Quality == nil {
		return true
	}
	switch c.Quality.Rankability {
	case rpc.GammaRankabilityRankable:
		return false
	case rpc.GammaRankabilityContextOnly:
		return false
	default:
		return true
	}
}

func gammaSPXCanonicalRankable(c *rpc.GammaZeroComputed) bool {
	return c != nil &&
		c.Scope == rpc.GammaZeroScopeSPX &&
		c.Quality != nil &&
		c.Quality.Rankability == rpc.GammaRankabilityRankable
}

func gammaOnlySPYUnavailable(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	seenSPYUnavailable := false
	for _, rawCode := range gammaWarningCodes(c) {
		code := strings.ToLower(strings.TrimSpace(rawCode))
		if code == "" || code == "no_crossing_in_window" || code == "strike_budget_capped" {
			continue
		}
		if strings.HasPrefix(code, "spy_unavailable:") {
			seenSPYUnavailable = true
			continue
		}
		return false
	}
	return seenSPYUnavailable
}

func gammaHasSPYUnavailable(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spy_unavailable:") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPYUnavailable(sub) {
			return true
		}
	}
	return false
}

func gammaHasSPXUnavailable(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spx_unavailable:") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPXUnavailable(sub) {
			return true
		}
	}
	return false
}

func gammaHasSPXCacheFallback(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if strings.HasPrefix(w.Code, "spx_cache_fallback") {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasSPXCacheFallback(sub) {
			return true
		}
	}
	return false
}

func gammaHasOIMissing(c *rpc.GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	for _, w := range c.WarningDetails {
		if w.Code == "oi_missing" {
			return true
		}
	}
	for _, sub := range c.PerIndex {
		if gammaHasOIMissing(sub) {
			return true
		}
	}
	return false
}

func gammaOIMissingSummaryClass(c *rpc.GammaZeroComputed, inheritedAsOf time.Time) string {
	if c == nil {
		return ""
	}
	asOf := inheritedAsOf
	if !c.AsOf.IsZero() {
		asOf = c.AsOf
	}
	for _, w := range c.WarningDetails {
		if w.Code == "oi_missing" {
			if asOf.IsZero() {
				asOf = time.Now()
			}
			scope := strings.ToUpper(strings.TrimSpace(w.Scope))
			if scope == "" {
				scope = strings.ToUpper(gammaStatusQualityScope(c))
			}
			if scope == "SPX" {
				return "spx"
			}
			if gammaClassifySession(asOf) == rpc.SessionRTH {
				return "rth"
			}
			if scope == "SPY" {
				return "spy_off_hours"
			}
			return "unknown"
		}
	}
	best := ""
	for _, sub := range c.PerIndex {
		switch got := gammaOIMissingSummaryClass(sub, asOf); got {
		case "spx":
			return got
		case "rth":
			best = got
		case "spy_off_hours":
			if best == "" {
				best = got
			}
		case "unknown":
			if best == "" {
				best = got
			}
		}
	}
	return best
}

func gammaStatusQualityScope(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX"
	case rpc.GammaZeroScopeCombined:
		return "SPY+SPX"
	default:
		return "SPY"
	}
}

func staleRegimeClusters(r *rpc.RegimeSnapshotResult) []string {
	candidates := []struct {
		name string
		rows []regimeClusterQualityRow
	}{
		{name: "vol", rows: []regimeClusterQualityRow{
			{status: r.VIXTermStructure.Status, band: bandForVIX(r.VIXTermStructure)},
			{status: r.VolOfVol.Status, band: bandForVolOfVol(r.VolOfVol)},
		}},
		{name: "credit", rows: []regimeClusterQualityRow{
			{status: r.HYGSPYDivergence.Status, band: bandForHYGSPY(r.HYGSPYDivergence)},
			{status: r.CreditSpreads.Status, band: bandForCreditSpreads(r.CreditSpreads)},
		}},
		{name: "funding", rows: []regimeClusterQualityRow{{status: r.FundingStress.Status, band: bandForFundingStress(r.FundingStress)}}},
		{name: "FX", rows: []regimeClusterQualityRow{{status: r.USDJPY.Status, band: bandForUSDJPY(r.USDJPY)}}},
		{name: "gamma", rows: []regimeClusterQualityRow{{status: r.GammaZero.Status, band: bandForGamma(r.GammaZero)}}},
		{name: "breadth", rows: []regimeClusterQualityRow{{status: r.Breadth.Status, band: bandForBreadth(r.Breadth)}}},
	}
	out := []string{}
	for _, c := range candidates {
		if regimeClusterScheduled(*r, c.name) {
			continue
		}
		if clusterEvidenceIsStale(c.rows) {
			out = append(out, c.name)
		}
	}
	return out
}

type regimeClusterQualityRow struct {
	status string
	band   string
}

func clusterEvidenceIsStale(rows []regimeClusterQualityRow) bool {
	stale := false
	for _, row := range rows {
		if row.status == rpc.RegimeStatusStale {
			stale = true
		}
	}
	return stale
}

func partialRegimeClusters(r *rpc.RegimeSnapshotResult) []string {
	volPartial := regimeRequiredFieldsMissing(r.VIXTermStructure.Status, r.VIXTermStructure.Band, r.VIXTermStructure.FieldsMissing)
	creditPartial := regimeRequiredFieldsMissing(r.HYGSPYDivergence.Status, r.HYGSPYDivergence.Band, r.HYGSPYDivergence.FieldsMissing) ||
		regimeRequiredFieldsMissing(r.CreditSpreads.Status, r.CreditSpreads.Band, r.CreditSpreads.FieldsMissing)
	candidates := []struct {
		name     string
		statuses []string
		partial  bool
	}{
		{name: "vol", statuses: []string{r.VIXTermStructure.Status, r.VolOfVol.Status}, partial: volPartial},
		{name: "credit", statuses: []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}, partial: creditPartial},
		{name: "funding", statuses: []string{r.FundingStress.Status}, partial: regimeRequiredFieldsMissing(r.FundingStress.Status, r.FundingStress.Band, r.FundingStress.FieldsMissing)},
		{name: "FX", statuses: []string{r.USDJPY.Status}, partial: regimeRequiredFieldsMissing(r.USDJPY.Status, r.USDJPY.Band, r.USDJPY.FieldsMissing)},
		{name: "gamma", statuses: []string{r.GammaZero.Status}, partial: regimeRequiredFieldsMissing(r.GammaZero.Status, r.GammaZero.Band, r.GammaZero.FieldsMissing)},
		{name: "breadth", statuses: []string{r.Breadth.Status}, partial: regimeRequiredFieldsMissing(r.Breadth.Status, r.Breadth.Band, r.Breadth.FieldsMissing)},
	}
	out := []string{}
	for _, c := range candidates {
		if c.partial ||
			hasRegimeStatus(c.statuses, rpc.RegimeStatusComputing) ||
			hasRegimeStatus(c.statuses, rpc.RegimeStatusUnavailable) ||
			hasRegimeStatus(c.statuses, rpc.RegimeStatusError) {
			out = append(out, c.name)
		}
	}
	return out
}

func regimeRequiredFieldsMissing(status string, band string, fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case rpc.RegimeStatusOK, rpc.RegimeStatusStale:
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "", "unranked":
		return true
	default:
		return false
	}
}

func hasRegimeStatus(statuses []string, want string) bool {
	for _, status := range statuses {
		if strings.EqualFold(strings.TrimSpace(status), want) {
			return true
		}
	}
	return false
}
