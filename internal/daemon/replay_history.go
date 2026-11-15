package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The exit replay's daily history (internal-docs/design/long-option-exits.md,
// LO-V1). A replay must give the same answer tomorrow without the gateway, so
// it reads retained history before the broker and keeps what it used:
//
//   - its own retained bars, one state document per underlying, written by
//     every replay that read more than it held and never evicted;
//   - the mandate's decision-grade closes (mandate_history.go): retained and
//     never evicted, but only for names held now, only the newest 320
//     sessions, and closes without a high or low;
//   - the market-history chart cache: high and low included, but a display
//     cache that evicts its oldest series at 750 (corestore/chart_cache.go);
//   - the broker, through the chart path, only when the retained sources do
//     not span the days the name needs.
//
// Neither existing store could hold this without changing its contract: the
// mandate's series is a bounded rolling window its verdict judges freshness
// on, and the chart cache's eviction is what bounds it. The replay's document
// is the union of the bars it read, so later runs only add to it.

const (
	stateKindReplayHistory = "edge_replay_history"
	replayHistoryDocVer    = 1
	// replayHistoryKeep bounds one document at about ten years of sessions.
	replayHistoryKeep = 2600
	// replayRestatedPct is the close difference on an overlapping day that
	// marks a fresher read as a restatement (a split): what was read before
	// it is then dropped rather than spliced onto a different basis.
	replayRestatedPct = 1.0
	// replaySameClosePct is the difference under which a close-only bar
	// leaves an earlier bar's high and low in place.
	replaySameClosePct = 0.5
	// replayHistoryWait bounds how long one replay waits for the broker's
	// history reads (replayHistoryBudget takes at most half of the request's
	// remaining deadline); replayHistoryFetchWindow bounds one read, which
	// goes on after the replay has answered.
	replayHistoryWait        = 25 * time.Second
	replayHistoryFetchWindow = 4 * time.Minute
)

// replayHistorySource is the method line the result carries.
const replayHistorySource = "Daily bars come from the history earlier replays retained, then the mandate's decision-grade closes (no high or low), then the market-history cache, and from the broker only when those do not span the days a name needs; a cash-index underlying is read as the index. The bars used are retained, so a later run without the gateway gives the same answer."

// replayHistoryDoc is one underlying's retained daily history.
type replayHistoryDoc struct {
	Version  int                `json:"version"`
	Contract rpc.ContractParams `json:"contract"`
	// AsOf is the newest source time of the bars merged into it.
	AsOf time.Time          `json:"as_of"`
	Bars []replayHistoryBar `json:"bars"`
}

type replayHistoryBar struct {
	Day   string  `json:"day"`
	Close float64 `json:"close"`
	High  float64 `json:"high,omitempty"`
	Low   float64 `json:"low,omitempty"`
}

// replaySource is one read of an underlying's daily bars and its source
// time.
type replaySource struct {
	asOf time.Time
	bars map[string]edgecore.UnderlyingBar
}

// replayHistoryContract is the contract an underlying's history is read
// for: a cash index (index names it) as the index, with the identity the
// market tape and the market view's index references use so their retained
// series serves the replay; every other underlying as a stock.
func replayHistoryContract(underlying edgecore.ExitReplayUnderlying, index func(string) (rpc.ContractParams, bool)) rpc.ContractParams {
	currency := underlying.Currency
	if currency == "" {
		currency = "USD"
	}
	if contract, ok := index(underlying.Symbol); ok {
		contract.PrimaryExch = ""
		return contract
	}
	return rpc.ContractParams{Symbol: underlying.Symbol, SecType: "STK", Exchange: "SMART", Currency: currency}
}

func replayHistoryScope(c rpc.ContractParams) string {
	sum := sha256.Sum256([]byte(c.SecType + "|" + c.Symbol + "|" + c.Currency))
	return "edge/replay/history/" + hex.EncodeToString(sum[:])
}

// replayRead is one underlying's history as the replay gathers it: the
// retained sources read first and, when they fall short, the broker read
// started for it.
type replayRead struct {
	underlying edgecore.ExitReplayUnderlying
	contract   rpc.ContractParams
	params     rpc.MarketHistoryParams
	retained   replaySource
	revision   int64
	sources    []replaySource
	merged     []edgecore.UnderlyingBar
	note       edgecore.ExitReplayHistoryNote
	fetch      *replayFetch
}

// replayFetch is one broker history read for the replay. It runs on its own
// deadline, so a replay that stops waiting leaves it to finish and keep its
// bars for the next run; done closes once result and err are set.
type replayFetch struct {
	done   chan struct{}
	result *rpc.MarketHistoryResult
	err    error
}

// replayFetchStore holds the broker reads in flight, one per underlying and
// range, so a run that starts while an earlier run's read is still going
// joins it instead of asking the broker again.
type replayFetchStore struct {
	mu       sync.Mutex
	inflight map[string]*replayFetch
}

// replayHistoryBudget is how long one replay waits for the broker's history
// reads: replayHistoryWait, and at most half of what remains of the
// request's own deadline, so the replay answers within it.
func replayHistoryBudget(ctx context.Context) time.Duration {
	wait := replayHistoryWait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)/2)
	}
	return max(wait, 0)
}

// replayHistoryRetained reads one underlying's retained daily bars and, when
// they do not span the days it needs, starts the broker read (or joins the
// one in flight). Nothing here waits on the broker.
func (s *Server) replayHistoryRetained(ctx context.Context, underlying edgecore.ExitReplayUnderlying, historyRange string) *replayRead {
	contract := replayHistoryContract(underlying, rpc.IndexUnderlyingContract)
	read := &replayRead{underlying: underlying, contract: contract, params: rpc.MarketHistoryParams{Contract: contract, Range: historyRange}}
	read.retained, read.revision = s.loadReplayHistory(ctx, contract)
	read.sources = []replaySource{read.retained}
	if series := s.replayMandateCloses(ctx, contract); series != nil {
		src := replaySource{asOf: series.FetchedAt, bars: map[string]edgecore.UnderlyingBar{}}
		for i, day := range series.Dates {
			if at, err := time.Parse(time.DateOnly, day); err == nil {
				src.bars[day] = edgecore.UnderlyingBar{Day: at, Close: series.Closes[i]}
			}
		}
		read.sources = append(read.sources, src)
	}
	key, _, identityErr := marketHistoryIdentity(read.params)
	if identityErr == nil {
		if saved, _, err := s.loadMarketHistory(ctx, key); err == nil && saved != nil {
			read.sources = append(read.sources, replayHistorySourceOf(saved.Result))
		}
	}
	read.merged = mergeReplaySources(read.sources)
	first := underlying.First.AddDate(0, 0, -replayHistoryLeadDays)
	switch {
	case edgecore.ExitReplayHistoryCovers(read.merged, first, underlying.Last):
	case identityErr != nil:
		read.note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryInstrumentUnavailable, Detail: identityErr.Error()}
	case s.gatewayConnector() == nil && s.replayHistoryFetchForTest == nil:
		read.note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryGatewayOffline, Detail: replayHeldText(read.merged)}
	default:
		read.fetch = s.startReplayFetch(ctx, read)
	}
	return read
}

// replayMandateCloses is the mandate's retained daily series for the
// contract the replay reads, through the mandate's own identity rules
// (mandate_history_identity.go): the document under the identity-bound key
// of that contract, which is where the mandate keeps an underlying held only
// through options (mandateOptionUnderlyingContract asks the same contract),
// else a legacy symbol-only document whose contract matches, as the mandate
// accepts it. A held stock's series is bound to the position's contract ID
// and listing, which the replay does not know: the series the mandate has
// loaded for the symbol serve when they match the contract and all name one
// contract ID, and two distinct contracts serve none.
func (s *Server) replayMandateCloses(ctx context.Context, contract rpc.ContractParams) *mandateHistorySeries {
	need := bindMandateHistoryNeed(mandateHistoryNeed{Key: "u:" + contract.Symbol, Contract: contract})
	if series := s.mandateHistoryForNeed(ctx, need); series != nil {
		return series
	}
	logical := mandateHistoryLogicalKey(need.Key)
	st := &s.mandateHistory
	st.mu.Lock()
	defer st.mu.Unlock()
	var found *mandateHistorySeries
	for key, candidate := range st.series {
		if candidate == nil || mandateHistoryLogicalKey(key) != logical || !mandateSeriesMatchesNeed(candidate, need) {
			continue
		}
		if found != nil && found.Contract.ConID != candidate.Contract.ConID {
			return nil
		}
		if found == nil || candidate.FetchedAt.After(found.FetchedAt) {
			found = candidate
		}
	}
	return found
}

// startReplayFetch starts the broker read for an underlying, or returns the
// one already in flight. The read runs for up to replayHistoryFetchWindow
// whether or not a replay still waits for it, and keeps what it read merged
// with the retained sources, so the next run finds the bars without the
// broker.
func (s *Server) startReplayFetch(ctx context.Context, read *replayRead) *replayFetch {
	key := replayHistoryScope(read.contract) + "|" + read.params.Range
	st := &s.replayFetches
	st.mu.Lock()
	defer st.mu.Unlock()
	if f := st.inflight[key]; f != nil {
		return f
	}
	if st.inflight == nil {
		st.inflight = map[string]*replayFetch{}
	}
	f := &replayFetch{done: make(chan struct{})}
	st.inflight[key] = f
	fetch := s.marketHistoryRequest
	if s.replayHistoryFetchForTest != nil {
		fetch = s.replayHistoryFetchForTest
	}
	contract, params := read.contract, read.params
	others := slices.Clone(read.sources[1:])
	go func() {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replayHistoryFetchWindow)
		defer cancel()
		result, err := fetch(fetchCtx, params)
		retained, revision := s.loadReplayHistory(fetchCtx, contract)
		sources := append([]replaySource{retained}, others...)
		if err == nil && result != nil && (result.Cache == nil || !result.Cache.RefreshFailed) {
			sources = append(sources, replayHistorySourceOf(*result))
		}
		s.saveReplayHistory(fetchCtx, contract, retained, revision, mergeReplaySources(sources), sources)
		st.mu.Lock()
		f.result, f.err = result, err
		delete(st.inflight, key)
		st.mu.Unlock()
		close(f.done)
	}()
	return f
}

// replayHistoryFinish waits for an underlying's broker read until wait is
// done and returns its daily bars and, when they do not span the days it
// needs, why. A read still running when wait ends leaves the name out as
// history_fetching; the read goes on and keeps its bars.
func (s *Server) replayHistoryFinish(ctx, wait context.Context, read *replayRead) ([]edgecore.UnderlyingBar, edgecore.ExitReplayHistoryNote) {
	note, merged := read.note, read.merged
	if read.fetch == nil {
		s.saveReplayHistory(ctx, read.contract, read.retained, read.revision, merged, read.sources)
	} else {
		select {
		case <-read.fetch.done:
			result, err := read.fetch.result, read.fetch.err
			switch {
			case err != nil && errors.Is(err, ibkrlib.ErrIBKRUnavailable):
				note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryGatewayOffline, Detail: replayHeldText(merged)}
			case err != nil && (marketHistoryVerdict(err) || errors.Is(err, ibkrlib.ErrNoListing)):
				note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryInstrumentUnavailable, Detail: errText(err)}
			case err != nil:
				note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryReadFailed, Detail: errText(err)}
			case result != nil && result.Cache != nil && result.Cache.RefreshFailed:
				note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryReadFailed, Detail: "the broker's read failed; " + replayHeldText(merged)}
			case result != nil:
				merged = mergeReplaySources(append(slices.Clone(read.sources), replayHistorySourceOf(*result)))
				note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryNotRetained, Detail: replayHeldText(merged)}
			}
		case <-wait.Done():
			note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryFetching, Detail: "the broker is still sending it; " + replayHeldText(merged)}
		}
	}
	if edgecore.ExitReplayHistoryCovers(merged, read.underlying.First, read.underlying.Last) {
		note = edgecore.ExitReplayHistoryNote{}
	} else if note.Reason == "" {
		note = edgecore.ExitReplayHistoryNote{Reason: rpc.EdgeReplayHistoryNotRetained, Detail: replayHeldText(merged)}
	}
	return merged, note
}

// replayHeldText says what daily history Canary holds for a name.
func replayHeldText(bars []edgecore.UnderlyingBar) string {
	if len(bars) == 0 {
		return "no retained history"
	}
	return fmt.Sprintf("history from %s to %s", bars[0].Day.Format(time.DateOnly), bars[len(bars)-1].Day.Format(time.DateOnly))
}

func replayHistorySourceOf(result rpc.MarketHistoryResult) replaySource {
	src := replaySource{asOf: result.AsOf, bars: map[string]edgecore.UnderlyingBar{}}
	if result.Interval != "1 day" {
		return src
	}
	for _, point := range result.Points {
		if !isFinitePrice(point.Value) || point.At.IsZero() {
			continue
		}
		y, m, d := point.At.UTC().Date()
		bar := edgecore.UnderlyingBar{Day: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Close: point.Value}
		if point.High != nil && point.Low != nil {
			bar.High, bar.Low = *point.High, *point.Low
		}
		src.bars[bar.Day.Format(time.DateOnly)] = bar
	}
	return src
}

// mergeReplaySources merges reads oldest source time first: a fresher bar
// replaces an older one, except that a close-only bar with the same close
// keeps the older bar's high and low, and a fresher read that restates an
// overlapping close drops everything read before it.
func mergeReplaySources(sources []replaySource) []edgecore.UnderlyingBar {
	ordered := slices.Clone(sources)
	slices.SortStableFunc(ordered, func(a, b replaySource) int { return a.asOf.Compare(b.asOf) })
	acc := map[string]edgecore.UnderlyingBar{}
	for _, src := range ordered {
		for day, bar := range src.bars {
			if old, ok := acc[day]; ok && math.Abs(bar.Close-old.Close)/old.Close*100 > replayRestatedPct {
				clear(acc)
				break
			}
		}
		for day, bar := range src.bars {
			old, ok := acc[day]
			if ok && bar.High <= 0 && old.High > 0 && math.Abs(bar.Close-old.Close)/old.Close*100 <= replaySameClosePct {
				continue
			}
			acc[day] = bar
		}
	}
	out := make([]edgecore.UnderlyingBar, 0, len(acc))
	for _, day := range slices.Sorted(maps.Keys(acc)) {
		out = append(out, acc[day])
	}
	return out
}

func (s *Server) loadReplayHistory(ctx context.Context, contract rpc.ContractParams) (replaySource, int64) {
	src := replaySource{bars: map[string]edgecore.UnderlyingBar{}}
	if s.coreStore == nil {
		return src, 0
	}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, replayHistoryScope(contract), stateKindReplayHistory)
	if err != nil || !ok {
		return src, 0
	}
	var saved replayHistoryDoc
	if json.Unmarshal(doc.JSON, &saved) != nil || saved.Version != replayHistoryDocVer || saved.Contract.Symbol != contract.Symbol || saved.Contract.SecType != contract.SecType || saved.Contract.Currency != contract.Currency {
		return src, doc.Revision
	}
	src.asOf = saved.AsOf
	for _, bar := range saved.Bars {
		at, err := time.Parse(time.DateOnly, bar.Day)
		if err != nil || !isFinitePrice(bar.Close) {
			continue
		}
		src.bars[bar.Day] = edgecore.UnderlyingBar{Day: at, Close: bar.Close, High: bar.High, Low: bar.Low}
	}
	return src, doc.Revision
}

// saveReplayHistory keeps the merged bars when they differ from the retained
// ones. A failed save leaves this run's answer unchanged and is logged.
func (s *Server) saveReplayHistory(ctx context.Context, contract rpc.ContractParams, retained replaySource, revision int64, merged []edgecore.UnderlyingBar, sources []replaySource) {
	if s.coreStore == nil || len(merged) == 0 {
		return
	}
	if len(merged) > replayHistoryKeep {
		merged = merged[len(merged)-replayHistoryKeep:]
	}
	same := len(merged) == len(retained.bars)
	doc := replayHistoryDoc{Version: replayHistoryDocVer, Contract: rpc.ContractParams{Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, Currency: contract.Currency}}
	for _, src := range sources {
		if src.asOf.After(doc.AsOf) {
			doc.AsOf = src.asOf
		}
	}
	for _, bar := range merged {
		day := bar.Day.Format(time.DateOnly)
		if old, ok := retained.bars[day]; !ok || old.Close != bar.Close || old.High != bar.High || old.Low != bar.Low {
			same = false
		}
		doc.Bars = append(doc.Bars, replayHistoryBar{Day: day, Close: bar.Close, High: bar.High, Low: bar.Low})
	}
	if same {
		return
	}
	raw, err := json.Marshal(doc)
	if err == nil {
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: replayHistoryScope(contract), Kind: stateKindReplayHistory, ExpectedRevision: revision, JSON: raw})
	}
	if err != nil && s.logger != nil {
		s.logger.Warnf("edge replay: keeping %d daily bars of %s failed: %v; this run's answer stands, the next run reads again", len(doc.Bars), strings.ToUpper(contract.Symbol), err)
	}
}
