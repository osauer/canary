package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// earningsCache serves per-symbol next-earnings dates for the trading
// rulebook (rules 6-8). Fetches never run on the snapshot path. Provider
// attempts, their last-good values, and the aggregate resolution are committed
// atomically before readers can observe them. Ambiguity is always unknown,
// never a guessed date.

const (
	earningsStoreFilename              = "earnings-dates.json"
	earningsPersistVersion             = 4
	earningsIdentityPersistVersion     = 3
	earningsPreviousPersistVersion     = 2
	earningsProviderObservationVersion = 3
	earningsLegacyVersion              = 1
	earningsNasdaqParserContractLegacy = 1
	// v2 accepted a nested no-date announcement, so only its no-date outcomes
	// need re-reading under a stricter grammar. v3 additionally demanded a
	// nested data.status for a dated announcement, an envelope the endpoint
	// does not serve; v4 reads the top-level status as the one authority for
	// dated and no-date announcements alike. Previous is the due rule's "re-read
	// this" mark; FirstExact is the oldest label a persisted attempt may still
	// carry. They coincide today and move independently.
	earningsNasdaqParserContractPrevious   = 2
	earningsNasdaqParserContractFirstExact = 2
	earningsNasdaqParserContract           = 4
	earningsFreshWindow                    = 24 * time.Hour
	earningsTTL                            = 45 * 24 * time.Hour
	earningsFetchTimeout                   = 8 * time.Second
	earningsFailureRetry                   = 15 * time.Minute
	earningsAuthorityCommitRetry           = time.Minute
	// A temporary connector-inactive mark is not a provider verdict. Keep its
	// durable retry inside the connector's bounded 12-hour mark lifetime so a
	// restart cannot turn that session-local observation into the 45-day
	// unsupported-security quiet period below.
	earningsContractResolutionRetry = 5 * time.Minute
	// Format, entitlement, protocol, and other non-retryable provider failures
	// remain due failures, but one failed read is enough for the daily source
	// cadence. Their typed outcome and next attempt survive daemon restart.
	earningsNonRetryableFailureRetry = 24 * time.Hour
	earningsFetchConcurrency         = 4
	earningsNasdaqUserAgent          = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 canary-earnings/1.0"
	earningsAuthorityScope           = "market/events/earnings"
	// Keep the established state kind: a newer payload under the same key makes
	// older binaries reject the authority instead of silently reading a stale
	// sibling document.
	earningsStateKind = "earnings_dates.current.v1"

	// Legacy observations are immutable evidence from the former JSON cache.
	earningsObservationKind   = "earnings_dates.snapshot.v1"
	earningsObservationSource = "nasdaq.earnings_calendar"

	earningsProviderObservationKind = "earnings_dates.provider_outcome.v3"
	earningsNasdaqProvider          = "nasdaq"
	earningsWSHProvider             = "ibkr_wsh"
	earningsWSHObservationSource    = "ibkr.wsh_event_calendar"

	earningsIdentityObservationVersion = 1
	earningsIdentityObservationKind    = "earnings_dates.identity_outcome.v1"
	earningsIdentityObservationSource  = "ibkr.contract_details"
)

const (
	earningsReasonConsensus        = "consensus"
	earningsReasonSingleSource     = "single_source"
	earningsReasonRetainedLastGood = "retained_last_good"
	earningsReasonConflicting      = "conflicting_sources"
	earningsReasonDateElapsed      = "date_elapsed"
	earningsReasonBrokerNonIssuer  = "broker_nonissuer"
)

const (
	earningsIdentityNotApplicable = "not_applicable"
	earningsIdentityIssuer        = "issuer"
	earningsIdentityUnknown       = "unknown"
)

type earningsEntry struct {
	// Date is the next earnings date, YYYY-MM-DD (ET calendar date).
	Date string `json:"date"`
	// TimeOfDay is "amc", "bmo", or "" when the provider did not say.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged unconfirmed dates.
	Estimated  bool      `json:"estimated,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// earningsPersistEnvelopeV1 is deliberately pinned for the unpublished JSON
// cutover importer. Never point it at the live v4 authority.
type earningsPersistEnvelopeV1 struct {
	Version int                      `json:"version"`
	Entries map[string]earningsEntry `json:"entries"`
}

type earningsPersistEnvelope struct {
	Version int                            `json:"version"`
	Symbols map[string]earningsSymbolState `json:"symbols"`
}

type earningsProviderAttempt struct {
	Status                string             `json:"status"`
	Entry                 *earningsEntry     `json:"entry,omitempty"`
	AttemptedAt           time.Time          `json:"attempted_at"`
	CompletedAt           time.Time          `json:"completed_at"`
	NextAttempt           *time.Time         `json:"next_attempt,omitempty"`
	LastFailure           *rpc.SourceFailure `json:"last_failure,omitempty"`
	ParserContractVersion int                `json:"parser_contract_version,omitempty"`
}

type earningsProviderState struct {
	LastAttempt earningsProviderAttempt `json:"last_attempt"`
	LastGood    *earningsEntry          `json:"last_good,omitempty"`
}

// earningsIdentityAttempt is independent broker contract-details evidence.
// ConID and SecType bind the proof to one current held stock identity. Outcome
// is deliberately closed and never retains the broker's raw StockType.
type earningsIdentityAttempt struct {
	ConID       int                `json:"con_id"`
	SecType     string             `json:"sec_type"`
	Outcome     string             `json:"outcome"`
	AttemptedAt time.Time          `json:"attempted_at"`
	CompletedAt time.Time          `json:"completed_at"`
	NextAttempt *time.Time         `json:"next_attempt"`
	LastFailure *rpc.SourceFailure `json:"last_failure,omitempty"`
}

type earningsIdentityState struct {
	LastAttempt       earningsIdentityAttempt `json:"last_attempt"`
	LastNotApplicable *earningsIdentityProof  `json:"last_not_applicable,omitempty"`
}

type earningsIdentityProof struct {
	ConID                int       `json:"con_id"`
	SecType              string    `json:"sec_type"`
	ObservedAt           time.Time `json:"observed_at"`
	AuthorityRevision    int64     `json:"authority_revision"`
	AuthorityFingerprint string    `json:"authority_fingerprint"`
	ObservationID        int64     `json:"observation_id"`
}

type earningsIdentityObservationPayload struct {
	Version int                     `json:"version"`
	Attempt earningsIdentityAttempt `json:"attempt"`
}

// earningsResolution is persisted as the exact cross-provider decision made
// at commit time. Readers re-resolve against the current clock so TTL expiry
// cannot leave a formerly valid date usable forever.
type earningsResolution struct {
	Status string         `json:"status"`
	Reason string         `json:"reason,omitempty"`
	Entry  *earningsEntry `json:"entry,omitempty"`
	Stale  bool           `json:"stale,omitempty"`
}

type earningsSymbolState struct {
	Resolution earningsResolution               `json:"resolution"`
	Providers  map[string]earningsProviderState `json:"providers"`
	Identity   *earningsIdentityState           `json:"identity,omitempty"`
	UpdatedAt  time.Time                        `json:"updated_at"`
}

type earningsProviderAttemptLegacy struct {
	Status      string             `json:"status"`
	Entry       *earningsEntry     `json:"entry,omitempty"`
	AttemptedAt time.Time          `json:"attempted_at"`
	CompletedAt time.Time          `json:"completed_at"`
	NextAttempt *time.Time         `json:"next_attempt,omitempty"`
	LastFailure *rpc.SourceFailure `json:"last_failure,omitempty"`
}

type earningsProviderStateLegacy struct {
	LastAttempt earningsProviderAttemptLegacy `json:"last_attempt"`
	LastGood    *earningsEntry                `json:"last_good,omitempty"`
}

// The v2 and v3 state shapes are pinned for strict in-place migration. Keeping
// separate provider types prevents an older document from smuggling the v4
// parser-contract field.
type earningsSymbolStateV2 struct {
	Resolution earningsResolution                     `json:"resolution"`
	Providers  map[string]earningsProviderStateLegacy `json:"providers"`
	UpdatedAt  time.Time                              `json:"updated_at"`
}

type earningsPersistEnvelopeV2 struct {
	Version int                              `json:"version"`
	Symbols map[string]earningsSymbolStateV2 `json:"symbols"`
}

type earningsSymbolStateV3 struct {
	Resolution earningsResolution                     `json:"resolution"`
	Providers  map[string]earningsProviderStateLegacy `json:"providers"`
	Identity   *earningsIdentityState                 `json:"identity,omitempty"`
	UpdatedAt  time.Time                              `json:"updated_at"`
}

type earningsPersistEnvelopeV3 struct {
	Version int                              `json:"version"`
	Symbols map[string]earningsSymbolStateV3 `json:"symbols"`
}

// earningsResolutionView is the cache's typed rulebook integration surface.
// Provider data is already redacted and ordered deterministically.
type earningsResolutionView struct {
	Status    string
	Reason    string
	Entry     earningsEntry
	Stale     bool
	Providers []rpc.EarningsProviderInfo
	Identity  *rpc.EarningsIdentityInfo
}

type earningsProviderFetchResult struct {
	Status  string
	Entry   earningsEntry
	Failure *rpc.SourceFailure
}

// The error return is local-log-only. Implementations must put only stable,
// allowlisted data in Result; raw upstream text never enters persistence/RPC.
type earningsProviderFetcher func(context.Context, string) (earningsProviderFetchResult, error)

type earningsIdentityFetchResult struct {
	Outcome     string
	Failure     *rpc.SourceFailure
	RetainProof bool
}

type earningsIdentityFetcher func(context.Context, string, int) (earningsIdentityFetchResult, error)

type earningsRefreshTarget struct {
	Symbol  string
	ConID   int
	SecType string
}

type earningsCache struct {
	mu       sync.Mutex
	symbols  map[string]earningsSymbolState
	inflight map[string]bool
	store    *earningsStore
	client   *http.Client
	logf     func(format string, args ...any)
	clock    func() time.Time
	// fetchURL is swappable for tests; %s receives the provider symbol.
	fetchURL string

	secondaryProvider string
	secondaryFetch    earningsProviderFetcher
	identityFetch     earningsIdentityFetcher
	// authorityRetryNotBefore is an ephemeral publication-failure gate. It
	// prevents a due provider result from being fetched on every snapshot while
	// SQLite is unavailable without publishing or changing a durable deadline.
	authorityRetryNotBefore map[string]time.Time
}

func newEarningsCache(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheCold(dir, logf)
	if loaded, err := c.store.loadLegacy(c.clock()); err != nil {
		c.logf("earnings cache load failed (cold start): %v", err)
	} else if loaded != nil {
		c.symbols = migrateEarningsV1(loaded, c.clock())
	}
	return c
}

// newEarningsCacheCold installs the legacy codec without reading it. Server
// construction runs before the persistence lock, so production uses this and
// leaves legacy reads exclusively to the unpublished cutover importer.
func newEarningsCacheCold(dir string, logf func(string, ...any)) *earningsCache {
	c := newEarningsCacheMemory(logf)
	c.store = &earningsStore{dir: dir}
	return c
}

func newEarningsCacheMemory(logf func(string, ...any)) *earningsCache {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &earningsCache{
		symbols:                 map[string]earningsSymbolState{},
		inflight:                map[string]bool{},
		client:                  &http.Client{Timeout: earningsFetchTimeout},
		logf:                    logf,
		clock:                   time.Now,
		fetchURL:                "https://api.nasdaq.com/api/analyst/%s/earnings-date",
		authorityRetryNotBefore: map[string]time.Time{},
	}
}

// setSecondaryProvider installs the approved independent provider. Production
// uses ibkr_wsh; the narrow hook also permits deterministic provider-agreement
// tests without network or Gateway access.
func (c *earningsCache) setSecondaryProvider(provider string, fetch earningsProviderFetcher) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != earningsWSHProvider {
		return fmt.Errorf("unsupported secondary earnings provider %q", provider)
	}
	if fetch == nil {
		return errors.New("secondary earnings provider fetch is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secondaryProvider = provider
	c.secondaryFetch = fetch
	return nil
}

func (c *earningsCache) setIdentityFetcher(fetch earningsIdentityFetcher) error {
	if fetch == nil {
		return errors.New("earnings identity fetch is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identityFetch = fetch
	return nil
}

// UseCoreStore replaces any legacy JSON projection loaded by construction
// with the current daemon.db document. Missing state initializes a cold v4
// document. Failure is returned before the authority pointer or in-memory
// projection changes.
func (c *earningsCache) UseCoreStore(store *corestore.Store) error {
	if c == nil {
		return errors.New("earnings cache: nil cache")
	}
	if store == nil {
		return errors.New("earnings cache: nil corestore")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = &earningsStore{}
	}
	loaded, err := c.store.useCoreStore(store, c.clock())
	if err != nil {
		return err
	}
	c.symbols = loaded
	c.inflight = map[string]bool{}
	c.authorityRetryNotBefore = map[string]time.Time{}
	return nil
}

const nasdaqProviderSymbolMaxLen = 32

// nasdaqSymbol maps IBKR symbols to the provider's spelling: broker spaces
// become dots. Only the provider's bounded ASCII symbol grammar may reach the
// request URL or shape an accepted announcement prefix.
func nasdaqSymbol(sym string) string {
	// Only ordinary broker padding is normalized. Keeping every other byte lets
	// the canonical validator reject controls and non-ASCII whitespace instead
	// of silently deleting untrusted input.
	sym = strings.ToUpper(strings.Trim(sym, " "))
	sym = strings.ReplaceAll(sym, " ", ".")
	if !validNasdaqProviderSymbol(sym) {
		return ""
	}
	return sym
}

func validNasdaqProviderSymbol(sym string) bool {
	if len(sym) == 0 || len(sym) > nasdaqProviderSymbolMaxLen {
		return false
	}
	previousWasAlphaNumeric := false
	for i := 0; i < len(sym); i++ {
		char := sym[i]
		isAlphaNumeric := char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if isAlphaNumeric {
			previousWasAlphaNumeric = true
			continue
		}
		if (char != '.' && char != '-') || !previousWasAlphaNumeric || i == len(sym)-1 {
			return false
		}
		previousWasAlphaNumeric = false
	}
	return previousWasAlphaNumeric
}

// get returns the aggregate usable date and whether its evidence is degraded
// or stale. Conflicts and all typed unknown outcomes return ok=false.
func (c *earningsCache) get(sym string) (entry earningsEntry, stale, ok bool) {
	view, ok := c.resolution(sym)
	if !ok || view.Status != rpc.EarningsStatusDate {
		return earningsEntry{}, false, false
	}
	return view.Entry, view.Stale, true
}

func (c *earningsCache) resolution(sym string) (earningsResolutionView, bool) {
	return c.resolutionWithIdentity(sym, 0, "")
}

// resolutionForIdentity projects broker applicability only when it is bound
// to the caller's current exact stock identity. A changed or missing identity
// invalidates the proof immediately without erasing provider outcomes.
func (c *earningsCache) resolutionForIdentity(sym string, conID int, secType string) (earningsResolutionView, bool) {
	return c.resolutionWithIdentity(sym, conID, secType)
}

func (c *earningsCache) resolutionWithIdentity(sym string, conID int, secType string) (earningsResolutionView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sym = strings.ToUpper(strings.TrimSpace(sym))
	state, ok := c.symbols[sym]
	if !ok {
		return earningsResolutionView{}, false
	}
	identity := state.Identity
	if !earningsIdentityMatches(identity, conID, secType) {
		identity = nil
	}
	resolved := resolveEarningsState(state.Providers, identity, c.clock())
	view := earningsResolutionView{Status: resolved.Status, Reason: resolved.Reason, Stale: resolved.Stale}
	if resolved.Entry != nil {
		view.Entry = *resolved.Entry
	}
	view.Providers = projectEarningsProviders(state.Providers)
	view.Identity = projectEarningsIdentity(sym, identity, c.clock())
	return view, true
}

func (c *earningsCache) kickRefreshTargets(ctx context.Context, targets []earningsRefreshTarget) {
	now := c.clock()
	var todo []earningsRefreshTarget
	c.mu.Lock()
	for _, target := range targets {
		sym := strings.ToUpper(strings.TrimSpace(target.Symbol))
		if sym == "" || c.inflight[sym] {
			continue
		}
		target.Symbol = sym
		target.SecType = canonicalEarningsIdentitySecType(target.SecType)
		state := c.symbols[sym]
		if !c.anyRefreshDueLocked(state, target, now) {
			continue
		}
		c.inflight[sym] = true
		todo = append(todo, target)
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	go c.refresh(context.WithoutCancel(ctx), todo)
}

func (c *earningsCache) providerNamesLocked() []string {
	providers := []string{earningsNasdaqProvider}
	if c.secondaryFetch != nil && c.secondaryProvider != "" {
		providers = append(providers, c.secondaryProvider)
	}
	return providers
}

func (c *earningsCache) anyRefreshDueLocked(state earningsSymbolState, target earningsRefreshTarget, now time.Time) bool {
	if retryAt, blocked := c.authorityRetryNotBefore[target.Symbol]; blocked && now.Before(retryAt) {
		return false
	}
	for _, provider := range c.providerNamesLocked() {
		if earningsProviderDue(provider, state.Providers[provider], now) {
			return true
		}
	}
	return c.identityFetch != nil && earningsIdentityTargetValid(target) && earningsIdentityDue(state.Identity, target, now)
}

func earningsProviderDue(provider string, state earningsProviderState, now time.Time) bool {
	if provider == earningsNasdaqProvider && state.LastAttempt.Status == rpc.EarningsStatusFormatChange &&
		state.LastAttempt.ParserContractVersion != earningsNasdaqParserContract {
		return true
	}
	if provider == earningsNasdaqProvider && state.LastAttempt.Status == rpc.EarningsStatusNoDatePublished &&
		state.LastAttempt.ParserContractVersion == earningsNasdaqParserContractPrevious {
		return true
	}
	if state.LastAttempt.AttemptedAt.IsZero() {
		return true
	}
	if state.LastAttempt.NextAttempt == nil {
		return true
	}
	return !now.Before(*state.LastAttempt.NextAttempt)
}

func canonicalEarningsIdentitySecType(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF":
		return "STK"
	default:
		return ""
	}
}

func earningsIdentityTargetValid(target earningsRefreshTarget) bool {
	return target.ConID > 0 && canonicalEarningsIdentitySecType(target.SecType) == "STK"
}

func earningsIdentityMatches(state *earningsIdentityState, conID int, secType string) bool {
	if state == nil || conID <= 0 || canonicalEarningsIdentitySecType(secType) != "STK" {
		return false
	}
	attempt := state.LastAttempt
	return attempt.ConID == conID && attempt.SecType == "STK"
}

func earningsIdentityProofMatches(state *earningsIdentityState, conID int, secType string) bool {
	if state == nil || state.LastNotApplicable == nil || conID <= 0 || canonicalEarningsIdentitySecType(secType) != "STK" {
		return false
	}
	proof := state.LastNotApplicable
	return proof.ConID == conID && proof.SecType == "STK"
}

func earningsIdentityDue(state *earningsIdentityState, target earningsRefreshTarget, now time.Time) bool {
	if !earningsIdentityTargetValid(target) {
		return false
	}
	if !earningsIdentityMatches(state, target.ConID, target.SecType) {
		return true
	}
	return state.LastAttempt.NextAttempt == nil || !now.Before(*state.LastAttempt.NextAttempt)
}

func (c *earningsCache) refresh(ctx context.Context, targets []earningsRefreshTarget) {
	sem := make(chan struct{}, earningsFetchConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			c.refreshTarget(ctx, target)
		})
	}
	wg.Wait()
}

type earningsCompletedProvider struct {
	provider string
	attempt  earningsProviderAttempt
	localErr error
}

func (c *earningsCache) refreshOne(ctx context.Context, sym string) {
	c.refreshTarget(ctx, earningsRefreshTarget{Symbol: sym})
}

func (c *earningsCache) refreshTarget(ctx context.Context, target earningsRefreshTarget) {
	sym := strings.ToUpper(strings.TrimSpace(target.Symbol))
	target.Symbol = sym
	target.SecType = canonicalEarningsIdentitySecType(target.SecType)
	c.mu.Lock()
	now := c.clock()
	state := cloneEarningsSymbolState(c.symbols[sym])
	providers := c.providerNamesLocked()
	secondaryProvider, secondaryFetch := c.secondaryProvider, c.secondaryFetch
	identityFetch := c.identityFetch
	c.mu.Unlock()

	var completed []earningsCompletedProvider
	for _, provider := range providers {
		if !earningsProviderDue(provider, state.Providers[provider], now) {
			continue
		}
		attemptedAt := c.clock()
		var result earningsProviderFetchResult
		var err error
		switch provider {
		case earningsNasdaqProvider:
			result, err = c.fetchNasdaqProvider(ctx, sym)
		case secondaryProvider:
			result, err = secondaryFetch(ctx, sym)
		default:
			result = transportFailureResult(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageAuthorityPersist, false, c.clock())
			err = fmt.Errorf("unsupported earnings provider %q", provider)
		}
		completedAt := c.clock()
		completed = append(completed, earningsCompletedProvider{
			provider: provider,
			attempt:  normalizeEarningsAttempt(provider, sym, result, attemptedAt, completedAt),
			localErr: err,
		})
	}
	var completedIdentity *earningsIdentityAttempt
	var identityErr error
	retainIdentityProof := false
	if identityFetch != nil && earningsIdentityTargetValid(target) && earningsIdentityDue(state.Identity, target, now) {
		attemptedAt := c.clock()
		result, err := identityFetch(ctx, sym, target.ConID)
		completedAt := c.clock()
		attempt := normalizeEarningsIdentityAttempt(target, result, attemptedAt, completedAt)
		completedIdentity = &attempt
		identityErr = err
		retainIdentityProof = result.RetainProof
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	defer delete(c.inflight, sym)
	if len(completed) == 0 && completedIdentity == nil {
		return
	}
	observations, err := earningsProviderObservations(sym, completed)
	if err != nil {
		c.logf("earnings provider outcome encode failed: %v", err)
		return
	}
	identityObservationIndex := -1
	if completedIdentity != nil {
		observation, encodeErr := earningsIdentityObservation(*completedIdentity)
		if encodeErr != nil {
			c.logf("earnings broker identity outcome encode failed")
			return
		}
		identityObservationIndex = 0
		observations = append([]corestore.ObservationInput{observation}, observations...)
	}
	for _, item := range completed {
		if item.localErr != nil {
			code, stage, retryable := "", "", false
			if item.attempt.LastFailure != nil {
				code = item.attempt.LastFailure.Code
				stage = item.attempt.LastFailure.Stage
				retryable = item.attempt.LastFailure.Retryable
			}
			c.logf("earnings provider %s outcome status=%s code=%s stage=%s retryable=%t", item.provider, item.attempt.Status, code, stage, retryable)
		}
	}
	if completedIdentity != nil {
		identityCopy := *completedIdentity
		if identityErr != nil {
			code, stage, retryable := "", "", false
			if identityCopy.LastFailure != nil {
				code = identityCopy.LastFailure.Code
				stage = identityCopy.LastFailure.Stage
				retryable = identityCopy.LastFailure.Retryable
			}
			c.logf("earnings broker identity outcome=%s code=%s stage=%s retryable=%t", identityCopy.Outcome, code, stage, retryable)
		}
	}
	decisionAt := c.clock()
	candidate, err := c.store.commitBound(context.WithoutCancel(ctx), observations, decisionAt,
		func(authorityRevision int64, receipts []corestore.ObservationReceipt) (map[string]earningsSymbolState, error) {
			if authorityRevision > 0 && len(receipts) != len(observations) {
				return nil, errors.New("earnings observation receipt count mismatch")
			}
			var identityReceipt *corestore.ObservationReceipt
			if identityObservationIndex >= 0 {
				if authorityRevision <= 0 || len(receipts) <= identityObservationIndex {
					return nil, errors.New("earnings identity observation authority unavailable")
				}
				receipt := receipts[identityObservationIndex]
				expectedDigest := sha256.Sum256(observations[identityObservationIndex].Payload)
				if receipt.ID <= 0 || receipt.PayloadSHA256 != expectedDigest {
					return nil, errors.New("earnings identity observation receipt invalid")
				}
				identityReceipt = &receipt
			}

			// Merge into the newest committed symbol snapshot. Other symbols may
			// have committed while provider calls were in flight; the store revision
			// is global and is intentionally consumed only under this lock.
			candidate := cloneEarningsSymbols(c.symbols)
			symbolState := cloneEarningsSymbolState(candidate[sym])
			if symbolState.Providers == nil {
				symbolState.Providers = map[string]earningsProviderState{}
			}
			for _, item := range completed {
				providerState := symbolState.Providers[item.provider]
				providerState.LastAttempt = item.attempt
				if item.attempt.Status == rpc.EarningsStatusDate && item.attempt.Entry != nil {
					entry := *item.attempt.Entry
					providerState.LastGood = &entry
				}
				symbolState.Providers[item.provider] = providerState
			}
			if completedIdentity != nil {
				identityCopy := *completedIdentity
				identityState := &earningsIdentityState{LastAttempt: identityCopy}
				switch {
				case identityCopy.Outcome == earningsIdentityNotApplicable && identityCopy.LastFailure == nil:
					if identityReceipt == nil {
						return nil, errors.New("earnings identity observation receipt missing")
					}
					identityState.LastNotApplicable = &earningsIdentityProof{
						ConID: identityCopy.ConID, SecType: identityCopy.SecType, ObservedAt: identityCopy.CompletedAt,
						AuthorityRevision:    authorityRevision,
						AuthorityFingerprint: earningsIdentityDigestFingerprint(identityReceipt.PayloadSHA256),
						ObservationID:        identityReceipt.ID,
					}
				case retainIdentityProof && identityCopy.LastFailure != nil && earningsIdentityProofMatches(symbolState.Identity, identityCopy.ConID, identityCopy.SecType):
					proof := *symbolState.Identity.LastNotApplicable
					identityState.LastNotApplicable = &proof
				}
				symbolState.Identity = identityState
			}
			symbolState.Resolution = resolveEarningsState(symbolState.Providers, symbolState.Identity, decisionAt)
			symbolState.UpdatedAt = decisionAt
			candidate[sym] = symbolState
			return candidate, nil
		})
	if err != nil {
		// Publishing an uncommitted attempt would make a transient memory result
		// outrun restart authority. SQLite health reports the authority failure.
		c.logf("earnings provider authority commit failed")
		c.authorityRetryNotBefore[sym] = c.clock().Add(earningsAuthorityCommitRetry)
		return
	}
	delete(c.authorityRetryNotBefore, sym)
	c.symbols = candidate
}

func normalizeEarningsAttempt(provider, symbol string, result earningsProviderFetchResult, attemptedAt, completedAt time.Time) earningsProviderAttempt {
	status := result.Status
	if !validEarningsProviderStatus(status) {
		status = rpc.EarningsStatusTransportFailure
		result.Entry = earningsEntry{}
		result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, Retryable: false}
	}
	if completedAt.Before(attemptedAt) {
		completedAt = attemptedAt
	}
	if status == rpc.EarningsStatusDate {
		if result.Entry.ObservedAt.IsZero() {
			result.Entry.ObservedAt = completedAt
		}
		if validateEarningsRowShape(symbol, result.Entry) != nil {
			status = rpc.EarningsStatusFormatChange
			result.Entry = earningsEntry{}
			result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: earningsProviderDecodeStage(provider), Retryable: false}
		} else if result.Entry.Date < earningsCalendarDate(completedAt) {
			status = rpc.EarningsStatusNoDatePublished
			result.Entry = earningsEntry{}
			result.Failure = nil
		}
	}
	if (status == rpc.EarningsStatusFormatChange || status == rpc.EarningsStatusTransportFailure) && result.Failure == nil {
		result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: earningsProviderDecodeStage(provider), Retryable: false}
	}
	attempt := earningsProviderAttempt{Status: status, AttemptedAt: attemptedAt, CompletedAt: completedAt}
	if provider == earningsNasdaqProvider {
		attempt.ParserContractVersion = earningsNasdaqParserContract
	}
	if status == rpc.EarningsStatusDate {
		entry := result.Entry
		attempt.Entry = &entry
	} else if result.Failure != nil {
		failure := *result.Failure
		failure.FailedAt = completedAt
		if !validEarningsSourceFailure(failure) {
			failure = rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, FailedAt: completedAt, Retryable: false}
		}
		attempt.LastFailure = &failure
	}
	next := earningsNextAttempt(status, attempt.LastFailure, completedAt)
	attempt.NextAttempt = &next
	return attempt
}

func normalizeEarningsIdentityAttempt(target earningsRefreshTarget, result earningsIdentityFetchResult, attemptedAt, completedAt time.Time) earningsIdentityAttempt {
	if completedAt.Before(attemptedAt) {
		completedAt = attemptedAt
	}
	outcome := strings.TrimSpace(result.Outcome)
	if !validEarningsIdentityOutcome(outcome) {
		outcome = earningsIdentityUnknown
		result.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, Retryable: false}
	}
	attempt := earningsIdentityAttempt{
		ConID: target.ConID, SecType: canonicalEarningsIdentitySecType(target.SecType), Outcome: outcome,
		AttemptedAt: attemptedAt, CompletedAt: completedAt,
	}
	if result.Failure != nil {
		failure := *result.Failure
		failure.FailedAt = completedAt
		if !validEarningsSourceFailure(failure) {
			failure = rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageAuthorityPersist, FailedAt: completedAt, Retryable: false}
		}
		attempt.LastFailure = &failure
		attempt.Outcome = earningsIdentityUnknown
	}
	next := completedAt.Add(earningsFreshWindow)
	if attempt.LastFailure != nil && attempt.LastFailure.Stage == rpc.SourceFailureStageWSHContractResolve {
		next = completedAt.Add(earningsContractResolutionRetry)
	}
	attempt.NextAttempt = &next
	return attempt
}

func earningsProviderDecodeStage(provider string) string {
	if provider == earningsWSHProvider {
		return rpc.SourceFailureStageWSHDecode
	}
	return rpc.SourceFailureStageNasdaqSchema
}

func earningsNextAttempt(status string, failure *rpc.SourceFailure, completedAt time.Time) time.Time {
	switch status {
	case rpc.EarningsStatusDate, rpc.EarningsStatusNoDatePublished:
		return completedAt.Add(earningsFreshWindow)
	case rpc.EarningsStatusUnsupportedSecurity:
		return completedAt.Add(earningsTTL)
	case rpc.EarningsStatusFormatChange:
		return completedAt.Add(earningsNonRetryableFailureRetry)
	case rpc.EarningsStatusTransportFailure:
		if failure != nil && !failure.Retryable {
			return completedAt.Add(earningsNonRetryableFailureRetry)
		}
		if failure != nil && failure.Stage == rpc.SourceFailureStageWSHContractResolve {
			return completedAt.Add(earningsContractResolutionRetry)
		}
		return completedAt.Add(earningsFailureRetry)
	default:
		return completedAt.Add(earningsFailureRetry)
	}
}

const nasdaqAnnouncementLead = "Earnings announcement* for "

// nasdaqAnnouncementPrefix is the only accepted Nasdaq announcement lead. It
// deliberately binds the provider's fixed wording to the exact symbol used in
// the request so arbitrary text ending in a date can never become evidence.
func nasdaqAnnouncementPrefix(providerSymbol string) string {
	return nasdaqAnnouncementLead + providerSymbol + ":"
}

type earningsProviderError struct {
	status  string
	failure *rpc.SourceFailure
	detail  error
}

func (e *earningsProviderError) Error() string {
	if e == nil || e.detail == nil {
		return "earnings provider outcome"
	}
	return e.detail.Error()
}

func providerOutcomeError(status, code, stage string, retryable bool, detail error) error {
	var failure *rpc.SourceFailure
	if code != "" {
		failure = &rpc.SourceFailure{Code: code, Stage: stage, Retryable: retryable}
	}
	return &earningsProviderError{status: status, failure: failure, detail: detail}
}

func (c *earningsCache) fetchNasdaqProvider(ctx context.Context, sym string) (earningsProviderFetchResult, error) {
	entry, err := c.fetchOne(ctx, sym)
	if err == nil {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusDate, Entry: entry}, nil
	}
	if providerErr, ok := errors.AsType[*earningsProviderError](err); ok {
		return earningsProviderFetchResult{Status: providerErr.status, Failure: providerErr.failure}, err
	}
	return transportFailureResult(rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, c.clock()), err
}

func transportFailureResult(code, stage string, retryable bool, at time.Time) earningsProviderFetchResult {
	return earningsProviderFetchResult{
		Status:  rpc.EarningsStatusTransportFailure,
		Failure: &rpc.SourceFailure{Code: code, Stage: stage, FailedAt: at, Retryable: retryable},
	}
}

// fetchOne preserves the original focused fetch seam while returning typed
// provider errors that refreshOne can safely project into persistence.
func (c *earningsCache) fetchOne(ctx context.Context, sym string) (earningsEntry, error) {
	providerSymbol := nasdaqSymbol(sym)
	if providerSymbol == "" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusUnsupportedSecurity, "", "", false, errors.New("symbol unsupported by Nasdaq earnings provider"))
	}
	ctx, cancel := context.WithTimeout(ctx, earningsFetchTimeout)
	defer cancel()
	url := fmt.Sprintf(c.fetchURL, providerSymbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqRequest, false, err)
	}
	// Nasdaq's CDN rejects bare Go clients; this allowlisted browser header set
	// is the endpoint contract established by the original provider spike.
	for k, v := range map[string]string{
		"User-Agent":      earningsNasdaqUserAgent,
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "en-US,en;q=0.9",
		"Origin":          "https://www.nasdaq.com",
		"Referer":         "https://www.nasdaq.com/",
	} {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageNasdaqRequest, retryable, fmt.Errorf("status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusTransportFailure, rpc.SourceFailureTransportFailed, rpc.SourceFailureStageNasdaqRequest, true, err)
	}
	return parseNasdaqEarnings(body, providerSymbol, c.clock())
}

// parseNasdaqEarnings accepts only the observed authority envelope and typed
// announcement grammar for the exact provider symbol requested. Both a strict
// future date and a no-date publication require data.status to be absent and
// top-level status.rCode=200; they differ only in whether a date follows the
// exactly one trailing ASCII space after the symbol-bound prefix. Missing,
// null, empty, elapsed, or any other announcement is a format change.
// Top-level status is also authoritative for the explicit data:null
// unsupported envelope.
func parseNasdaqEarnings(body []byte, providerSymbol string, now time.Time) (earningsEntry, error) {
	if !json.Valid(body) {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqDecode, false, errors.New("nasdaq payload is not valid JSON"))
	}
	top, err := decodeExactAuthorityObject(body, "data", "status")
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload has invalid authority fields"))
	}
	dataRaw, hasData := top["data"]
	if !hasData {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload has no explicit data field"))
	}
	if jsonRawIsNull(dataRaw) {
		statusRaw, hasStatus := top["status"]
		if rCode, ok := nasdaqStatusCode(statusRaw); hasStatus && ok && rCode == http.StatusBadRequest {
			return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusUnsupportedSecurity, "", "", false, errors.New("nasdaq reports unsupported security"))
		}
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq null data has no supported typed status"))
	}
	data, err := decodeExactAuthorityObject(dataRaw, "announcement", "status")
	if err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload has invalid data object"))
	}
	announcementRaw, hasAnnouncement := data["announcement"]
	if !hasAnnouncement || jsonRawIsNull(announcementRaw) {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement is absent"))
	}
	var announcement string
	if err := json.Unmarshal(announcementRaw, &announcement); err != nil {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement has invalid type"))
	}
	if announcement == "" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement is empty"))
	}
	if !validNasdaqProviderSymbol(providerSymbol) {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement format changed"))
	}
	prefix := nasdaqAnnouncementPrefix(providerSymbol)
	if _, hasDataStatus := data["status"]; hasDataStatus {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload has conflicting status authority"))
	}
	topStatusRaw, hasTopStatus := top["status"]
	rCode, ok := nasdaqStatusCode(topStatusRaw)
	if !hasTopStatus || !ok || rCode != http.StatusOK {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq payload status is inconsistent with data"))
	}
	if announcement == prefix+" " {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusNoDatePublished, "", "", false, errors.New("nasdaq published no earnings date"))
	}
	dateText, ok := strings.CutPrefix(announcement, prefix+" ")
	if !ok || dateText == "" {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement format changed"))
	}
	t, ok := parseNasdaqAnnouncementDate(dateText)
	if !ok {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement date is invalid"))
	}
	if t.Format(time.DateOnly) < earningsCalendarDate(now) {
		return earningsEntry{}, providerOutcomeError(rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema, false, errors.New("nasdaq announcement date has elapsed"))
	}
	return earningsEntry{Date: t.Format(time.DateOnly), ObservedAt: now}, nil
}

// parseNasdaqAnnouncementDate reads the two spellings the endpoint publishes:
// a scheduled date carries a zero-padded day, a Zacks-estimated one does not.
// Each spelling must round-trip its own layout exactly, so anything else stays
// a refusal rather than a guess.
func parseNasdaqAnnouncementDate(dateText string) (time.Time, bool) {
	for _, layout := range []string{"Jan 2, 2006", "Jan 02, 2006"} {
		if t, err := time.Parse(layout, dateText); err == nil && t.Format(layout) == dateText {
			return t, true
		}
	}
	return time.Time{}, false
}

func nasdaqStatusCode(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return 0, false
	}
	status, err := decodeExactAuthorityObject(raw, "rCode")
	if err != nil {
		return 0, false
	}
	rawCode, ok := status["rCode"]
	if !ok || jsonRawIsNull(rawCode) {
		return 0, false
	}
	var rCode int
	if err := json.Unmarshal(rawCode, &rCode); err != nil {
		return 0, false
	}
	return rCode, true
}

// decodeExactAuthorityObject reads only the named authority fields while
// tolerating unrelated provider prose. Authority names are exact ASCII
// contracts: duplicate or case-folded aliases are equivocation and fail.
func decodeExactAuthorityObject(raw []byte, authorityKeys ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("authority value is not an object")
	}
	authority := make(map[string]struct{}, len(authorityKeys))
	for _, key := range authorityKeys {
		authority[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(authorityKeys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("authority object key is invalid")
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("authority object key is invalid")
		}
		matched := ""
		for expected := range authority {
			if key == expected {
				matched = expected
				break
			}
			if strings.EqualFold(key, expected) {
				return nil, errors.New("authority object key has invalid case")
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("authority object value is invalid")
		}
		if matched == "" {
			continue
		}
		if _, duplicate := fields[matched]; duplicate {
			return nil, errors.New("authority object key is duplicated")
		}
		fields[matched] = append(json.RawMessage(nil), value...)
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("authority object is incomplete")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("authority object has trailing JSON")
	}
	return fields, nil
}

func jsonRawIsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// rejectDuplicateJSONKeys closes the duplicate-key gap left by encoding/json's
// last-value-wins behavior. Errors never echo object keys because persisted
// maps may be keyed by private held symbols.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanUniqueJSONValue(decoder); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("earnings authority has trailing JSON")
	}
	return nil
}

func scanUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("earnings authority is not valid JSON")
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		seenKeys := make([]string, 0)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errors.New("earnings authority object key is invalid")
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("earnings authority object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("earnings authority has a duplicate object key")
			}
			for _, prior := range seenKeys {
				if strings.EqualFold(prior, key) {
					return errors.New("earnings authority has an ambiguous object key")
				}
			}
			seen[key] = struct{}{}
			seenKeys = append(seenKeys, key)
			if err := scanUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("earnings authority object is incomplete")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("earnings authority array is incomplete")
		}
		return nil
	default:
		return errors.New("earnings authority has an unexpected delimiter")
	}
}

func earningsCalendarDate(now time.Time) string {
	loc, err := time.LoadLocation("America/New_York")
	if err == nil {
		now = now.In(loc)
	}
	return now.Format(time.DateOnly)
}

func resolveEarningsProviders(providers map[string]earningsProviderState, now time.Time) earningsResolution {
	type candidate struct {
		entry    earningsEntry
		retained bool
	}
	var candidates []candidate
	statuses := map[string]int{}
	for _, state := range providers {
		attempt := state.LastAttempt
		statuses[attempt.Status]++
		switch attempt.Status {
		case rpc.EarningsStatusDate:
			if attempt.Entry != nil && usableEarningsEntry(*attempt.Entry, now) {
				candidates = append(candidates, candidate{entry: *attempt.Entry})
			}
		case rpc.EarningsStatusTransportFailure:
			if state.LastGood != nil && usableEarningsEntry(*state.LastGood, now) {
				candidates = append(candidates, candidate{entry: *state.LastGood, retained: true})
			}
		}
	}
	if len(candidates) > 0 {
		chosen := candidates[0].entry
		stale := false
		for _, candidate := range candidates {
			if candidate.entry.Date != chosen.Date || incompatibleEarningsSessions(candidate.entry.TimeOfDay, chosen.TimeOfDay) {
				return earningsResolution{Status: rpc.EarningsStatusConflictingSources, Reason: earningsReasonConflicting}
			}
			if chosen.TimeOfDay == "" {
				chosen.TimeOfDay = candidate.entry.TimeOfDay
			}
			chosen.Estimated = chosen.Estimated || candidate.entry.Estimated
			if candidate.entry.ObservedAt.Before(chosen.ObservedAt) {
				chosen.ObservedAt = candidate.entry.ObservedAt
			}
			stale = stale || candidate.retained || now.Sub(candidate.entry.ObservedAt) > earningsFreshWindow
		}
		reason := earningsReasonSingleSource
		if len(candidates) > 1 {
			reason = earningsReasonConsensus
		} else if candidates[0].retained {
			reason = earningsReasonRetainedLastGood
		}
		return earningsResolution{Status: rpc.EarningsStatusDate, Reason: reason, Entry: &chosen, Stale: stale}
	}
	if status, ok := earningsNasdaqObservedWithWSHNotEntitled(providers); ok {
		return earningsResolution{Status: status, Reason: status}
	}

	if statuses[rpc.EarningsStatusFormatChange] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusFormatChange, Reason: rpc.EarningsStatusFormatChange}
	}
	if statuses[rpc.EarningsStatusTransportFailure] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusTransportFailure, Reason: rpc.EarningsStatusTransportFailure}
	}
	if statuses[rpc.EarningsStatusUnsupportedSecurity] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusUnsupportedSecurity, Reason: rpc.EarningsStatusUnsupportedSecurity}
	}
	if statuses[rpc.EarningsStatusNoDatePublished] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished}
	}
	if statuses[rpc.EarningsStatusDate] > 0 {
		return earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: earningsReasonDateElapsed}
	}
	return earningsResolution{Status: rpc.EarningsStatusTransportFailure, Reason: rpc.EarningsStatusTransportFailure}
}

func earningsNasdaqObservedWithWSHNotEntitled(providers map[string]earningsProviderState) (string, bool) {
	if len(providers) != 2 {
		return "", false
	}
	nasdaq, hasNasdaq := providers[earningsNasdaqProvider]
	wsh, hasWSH := providers[earningsWSHProvider]
	if !hasNasdaq || !hasWSH || (nasdaq.LastAttempt.Status != rpc.EarningsStatusNoDatePublished && nasdaq.LastAttempt.Status != rpc.EarningsStatusUnsupportedSecurity) || wsh.LastAttempt.Status != rpc.EarningsStatusTransportFailure {
		return "", false
	}
	failure := wsh.LastAttempt.LastFailure
	if failure == nil || failure.Code != rpc.SourceFailureNotEntitled || failure.Retryable ||
		(failure.Stage != rpc.SourceFailureStageWSHMetadata && failure.Stage != rpc.SourceFailureStageWSHEvent) {
		return "", false
	}
	return nasdaq.LastAttempt.Status, true
}

func resolveEarningsState(providers map[string]earningsProviderState, identity *earningsIdentityState, now time.Time) earningsResolution {
	providerResolution := resolveEarningsProviders(providers, now)
	if !usableEarningsNotApplicable(identity, now) {
		return providerResolution
	}
	if providerResolution.Status == rpc.EarningsStatusDate || providerResolution.Status == rpc.EarningsStatusConflictingSources {
		return earningsResolution{Status: rpc.EarningsStatusConflictingSources, Reason: earningsReasonConflicting}
	}
	return earningsResolution{Status: rpc.EarningsStatusNotApplicable, Reason: earningsReasonBrokerNonIssuer}
}

func usableEarningsNotApplicable(identity *earningsIdentityState, now time.Time) bool {
	if identity == nil || identity.LastNotApplicable == nil {
		return false
	}
	proof := identity.LastNotApplicable
	return proof.ConID > 0 && proof.SecType == "STK" && !proof.ObservedAt.IsZero() && !now.Before(proof.ObservedAt) &&
		proof.AuthorityRevision > 0 && validAlertRegistryFingerprint(proof.AuthorityFingerprint) && proof.ObservationID > 0
}

func usableEarningsEntry(entry earningsEntry, now time.Time) bool {
	if entry.ObservedAt.IsZero() || now.Before(entry.ObservedAt) || now.Sub(entry.ObservedAt) > earningsTTL {
		return false
	}
	return entry.Date >= earningsCalendarDate(now)
}

func incompatibleEarningsSessions(a, b string) bool {
	return a != "" && b != "" && a != b
}

func projectEarningsProviders(providers map[string]earningsProviderState) []rpc.EarningsProviderInfo {
	names := make([]string, 0, len(providers))
	for provider := range providers {
		names = append(names, provider)
	}
	sort.Strings(names)
	out := make([]rpc.EarningsProviderInfo, 0, len(names))
	for _, provider := range names {
		state := providers[provider]
		attempt := state.LastAttempt
		info := rpc.EarningsProviderInfo{
			Provider: provider, Status: attempt.Status, AttemptedAt: attempt.AttemptedAt,
			NextAttempt: cloneTimePointer(attempt.NextAttempt), LastFailure: cloneEarningsSourceFailure(attempt.LastFailure),
		}
		if attempt.Entry != nil {
			info.Date = attempt.Entry.Date
			info.TimeOfDay = attempt.Entry.TimeOfDay
			info.Estimated = attempt.Entry.Estimated
			info.ObservedAt = attempt.Entry.ObservedAt
		}
		if state.LastGood != nil {
			info.LastGoodDate = state.LastGood.Date
		}
		out = append(out, info)
	}
	return out
}

func projectEarningsIdentity(symbol string, identity *earningsIdentityState, now time.Time) *rpc.EarningsIdentityInfo {
	if identity == nil {
		return nil
	}
	attempt := identity.LastAttempt
	info := &rpc.EarningsIdentityInfo{
		Outcome: attempt.Outcome, NotApplicable: usableEarningsNotApplicable(identity, now), AttemptedAt: attempt.AttemptedAt,
		NextAttempt: cloneTimePointer(attempt.NextAttempt), LastFailure: cloneEarningsSourceFailure(attempt.LastFailure),
	}
	if info.NotApplicable {
		proof := identity.LastNotApplicable
		info.ProofObservedAt = proof.ObservedAt
		info.AuthorityRevision = proof.AuthorityRevision
		info.AuthorityFingerprint = proof.AuthorityFingerprint
		info.ObservationID = opaqueEarningsIdentityObservationID(proof.ObservationID, proof.AuthorityFingerprint)
		info.ProofOutcome = rpc.EarningsStatusNotApplicable
		info.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(symbol, *info)
	}
	return info
}

func earningsIdentityObservation(attempt earningsIdentityAttempt) (corestore.ObservationInput, error) {
	payload, err := json.Marshal(earningsIdentityObservationPayload{Version: earningsIdentityObservationVersion, Attempt: attempt})
	if err != nil {
		return corestore.ObservationInput{}, fmt.Errorf("encode earnings identity observation: %w", err)
	}
	metadata, err := json.Marshal(struct {
		Version int    `json:"version"`
		Outcome string `json:"outcome"`
	}{earningsIdentityObservationVersion, attempt.Outcome})
	if err != nil {
		return corestore.ObservationInput{}, fmt.Errorf("encode earnings identity metadata: %w", err)
	}
	return corestore.ObservationInput{
		ScopeKey: earningsAuthorityScope, Source: earningsIdentityObservationSource,
		Kind: earningsIdentityObservationKind, ObservedAt: attempt.CompletedAt,
		ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
	}, nil
}

func earningsIdentityDigestFingerprint(digest [sha256.Size]byte) string {
	return "sha256:" + hex.EncodeToString(digest[:])
}

func opaqueEarningsIdentityObservationID(id int64, fingerprint string) string {
	if id <= 0 || !validAlertRegistryFingerprint(fingerprint) {
		return ""
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d:%s", earningsIdentityObservationKind, id, fingerprint))
	return "oid:sha256:" + hex.EncodeToString(sum[:])
}

func validOpaqueEarningsIdentityObservationID(value string) bool {
	const prefix = "oid:sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	raw := value[len(prefix):]
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == raw
}

func earningsProviderObservations(sym string, completed []earningsCompletedProvider) ([]corestore.ObservationInput, error) {
	observations := make([]corestore.ObservationInput, 0, len(completed))
	for _, item := range completed {
		payload, err := json.Marshal(struct {
			Version  int                     `json:"version"`
			Symbol   string                  `json:"symbol"`
			Provider string                  `json:"provider"`
			Attempt  earningsProviderAttempt `json:"attempt"`
		}{earningsProviderObservationVersion, sym, item.provider, item.attempt})
		if err != nil {
			return nil, fmt.Errorf("encode %s earnings observation: %w", item.provider, err)
		}
		metadata, err := json.Marshal(struct {
			Version  int    `json:"version"`
			Provider string `json:"provider"`
			Status   string `json:"status"`
		}{earningsProviderObservationVersion, item.provider, item.attempt.Status})
		if err != nil {
			return nil, fmt.Errorf("encode %s earnings metadata: %w", item.provider, err)
		}
		observations = append(observations, corestore.ObservationInput{
			ScopeKey: earningsAuthorityScope, Source: earningsObservationSourceForProvider(item.provider),
			Kind: earningsProviderObservationKind, ObservedAt: item.attempt.CompletedAt,
			ContentType: "application/json", Payload: payload, MetadataJSON: metadata, DecisionEligible: true,
		})
	}
	return observations, nil
}

func earningsObservationSourceForProvider(provider string) string {
	if provider == earningsWSHProvider {
		return earningsWSHObservationSource
	}
	return earningsObservationSource
}

func validEarningsProviderStatus(status string) bool {
	switch status {
	case rpc.EarningsStatusDate, rpc.EarningsStatusNoDatePublished,
		rpc.EarningsStatusUnsupportedSecurity, rpc.EarningsStatusFormatChange,
		rpc.EarningsStatusTransportFailure:
		return true
	default:
		return false
	}
}

func validEarningsIdentityOutcome(outcome string) bool {
	switch outcome {
	case earningsIdentityNotApplicable, earningsIdentityIssuer, earningsIdentityUnknown:
		return true
	default:
		return false
	}
}

func validEarningsSourceFailure(failure rpc.SourceFailure) bool {
	return rpc.ValidSourceFailure(&failure) && !failure.FailedAt.IsZero()
}

// earningsStore persists v4 state across restarts. The JSON save/load methods
// remain v1-only for the sealed legacy cutover path and isolated tests.
type earningsStore struct {
	dir       string
	authority *corestore.Store
	revision  int64
}

func (s *earningsStore) useCoreStore(store *corestore.Store, now time.Time) (map[string]earningsSymbolState, error) {
	if s == nil {
		return nil, errors.New("earnings store: nil store")
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil {
		return nil, fmt.Errorf("read earnings authority: %w", err)
	}
	loaded := map[string]earningsSymbolState{}
	if !ok {
		raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
		if err != nil {
			return nil, fmt.Errorf("encode initial earnings authority: %w", err)
		}
		doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize earnings authority: %w", err)
		}
	} else {
		if err := rejectDuplicateJSONKeys(doc.JSON); err != nil {
			return nil, fmt.Errorf("decode earnings authority header: %w", err)
		}
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(doc.JSON, &header); err != nil {
			return nil, fmt.Errorf("decode earnings authority header: %w", err)
		}
		switch header.Version {
		case earningsLegacyVersion:
			legacy, err := decodeEarningsEnvelopeV1(doc.JSON, now, true)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v1: %w", err)
			}
			loaded = migrateEarningsV1(legacy, now)
			raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
			if err != nil {
				return nil, fmt.Errorf("encode migrated earnings authority: %w", err)
			}
			doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
			if err != nil {
				return nil, fmt.Errorf("persist migrated earnings authority: %w", err)
			}
		case earningsPreviousPersistVersion:
			loaded, err = decodeEarningsEnvelopeV2(doc.JSON, now)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v2: %w", err)
			}
			raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
			if err != nil {
				return nil, fmt.Errorf("encode migrated earnings authority: %w", err)
			}
			doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
			if err != nil {
				return nil, fmt.Errorf("persist migrated earnings authority: %w", err)
			}
		case earningsIdentityPersistVersion:
			loaded, err = decodeEarningsEnvelopeV3(doc.JSON, now)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v3: %w", err)
			}
			raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: loaded})
			if err != nil {
				return nil, fmt.Errorf("encode migrated earnings authority: %w", err)
			}
			doc, err = store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
			if err != nil {
				return nil, fmt.Errorf("persist migrated earnings authority: %w", err)
			}
		case earningsPersistVersion:
			loaded, err = decodeEarningsEnvelopeV4(doc.JSON, now)
			if err != nil {
				return nil, fmt.Errorf("decode earnings authority v4: %w", err)
			}
		default:
			return nil, fmt.Errorf("decode earnings authority: unsupported version %d", header.Version)
		}
	}
	if err := validateEarningsIdentityProofObservations(context.Background(), store, loaded, doc.Revision); err != nil {
		return nil, fmt.Errorf("validate earnings identity observation authority: %w", err)
	}
	s.authority = store
	s.revision = doc.Revision
	return loaded, nil
}

func (s *earningsStore) commitBound(
	ctx context.Context,
	observations []corestore.ObservationInput,
	now time.Time,
	build func(authorityRevision int64, receipts []corestore.ObservationReceipt) (map[string]earningsSymbolState, error),
) (map[string]earningsSymbolState, error) {
	if s == nil {
		return nil, errors.New("earnings store: nil store")
	}
	if len(observations) == 0 {
		return nil, errors.New("earnings observations are required")
	}
	if build == nil {
		return nil, errors.New("earnings state builder is required")
	}
	if s.authority == nil {
		candidate, err := build(0, nil)
		if err != nil {
			return nil, err
		}
		if err := validateEarningsSymbols(candidate, now); err != nil {
			return nil, err
		}
		if err := s.save(resolvedEarningsEntries(candidate, now)); err != nil {
			return nil, err
		}
		return candidate, nil
	}
	update := corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind,
		ExpectedRevision: s.revision,
	}
	var candidate map[string]earningsSymbolState
	saved, _, err := s.authority.CompareAndSwapStateDocumentWithBoundObservations(ctx, update, observations,
		func(authorityRevision int64, receipts []corestore.ObservationReceipt) ([]byte, error) {
			var buildErr error
			candidate, buildErr = build(authorityRevision, receipts)
			if buildErr != nil {
				return nil, buildErr
			}
			if err := validateEarningsSymbols(candidate, now); err != nil {
				return nil, err
			}
			payload, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: candidate})
			if err != nil {
				return nil, fmt.Errorf("encode earnings authority: %w", err)
			}
			return payload, nil
		})
	if err != nil {
		return nil, fmt.Errorf("commit earnings authority: %w", err)
	}
	s.revision = saved.Revision
	return candidate, nil
}

func (s *earningsStore) loadLegacy(now time.Time) (map[string]earningsEntry, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, earningsStoreFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read earnings cache: %w", err)
	}
	return decodeEarningsEnvelopeV1(data, now, false)
}

func decodeEarningsEnvelopeV1(data []byte, now time.Time, strict bool) (map[string]earningsEntry, error) {
	var env earningsPersistEnvelopeV1
	var err error
	if strict {
		if err := rejectDuplicateJSONKeys(data); err != nil {
			return nil, fmt.Errorf("decode earnings cache: %w", err)
		}
		err = decodeStrictMarketEventJSON(data, &env)
	} else {
		err = json.Unmarshal(data, &env)
	}
	if err != nil {
		return nil, fmt.Errorf("decode earnings cache: %w", err)
	}
	if env.Version != earningsLegacyVersion {
		if strict {
			return nil, fmt.Errorf("invalid earnings version %d", env.Version)
		}
		return nil, nil
	}
	if strict && env.Entries == nil {
		return nil, errors.New("earnings authority has no entries map")
	}
	entries := make(map[string]earningsEntry, len(env.Entries))
	for sym, entry := range env.Entries {
		if err := validateEarningsRow(sym, entry, now); err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		if now.Sub(entry.ObservedAt) <= earningsTTL {
			entries[strings.ToUpper(sym)] = entry
		}
	}
	return entries, nil
}

func decodeEarningsEnvelopeV2(data []byte, now time.Time) (map[string]earningsSymbolState, error) {
	var env earningsPersistEnvelopeV2
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if err := decodeStrictMarketEventJSON(data, &env); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if env.Version != earningsPreviousPersistVersion {
		return nil, fmt.Errorf("invalid earnings version %d", env.Version)
	}
	if env.Symbols == nil {
		return nil, errors.New("earnings authority has no symbols map")
	}
	converted := make(map[string]earningsSymbolState, len(env.Symbols))
	for symbol, state := range env.Symbols {
		converted[symbol] = earningsSymbolState{
			Resolution: state.Resolution, Providers: migrateEarningsProviderStates(state.Providers), UpdatedAt: state.UpdatedAt,
		}
	}
	if err := validateEarningsSymbols(converted, now); err != nil {
		return nil, err
	}
	return cloneEarningsSymbols(converted), nil
}

func decodeEarningsEnvelopeV3(data []byte, now time.Time) (map[string]earningsSymbolState, error) {
	var env earningsPersistEnvelopeV3
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if err := decodeStrictMarketEventJSON(data, &env); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if env.Version != earningsIdentityPersistVersion {
		return nil, fmt.Errorf("invalid earnings version %d", env.Version)
	}
	if env.Symbols == nil {
		return nil, errors.New("earnings authority has no symbols map")
	}
	converted := make(map[string]earningsSymbolState, len(env.Symbols))
	for symbol, state := range env.Symbols {
		converted[symbol] = earningsSymbolState{
			Resolution: state.Resolution, Providers: migrateEarningsProviderStates(state.Providers),
			Identity: state.Identity, UpdatedAt: state.UpdatedAt,
		}
	}
	if err := validateEarningsSymbols(converted, now); err != nil {
		return nil, err
	}
	return cloneEarningsSymbols(converted), nil
}

func decodeEarningsEnvelopeV4(data []byte, now time.Time) (map[string]earningsSymbolState, error) {
	var env earningsPersistEnvelope
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if err := decodeStrictMarketEventJSON(data, &env); err != nil {
		return nil, fmt.Errorf("decode earnings authority: %w", err)
	}
	if env.Version != earningsPersistVersion {
		return nil, fmt.Errorf("invalid earnings version %d", env.Version)
	}
	if env.Symbols == nil {
		return nil, errors.New("earnings authority has no symbols map")
	}
	if err := validateEarningsSymbols(env.Symbols, now); err != nil {
		return nil, err
	}
	return cloneEarningsSymbols(env.Symbols), nil
}

func migrateEarningsProviderStates(legacy map[string]earningsProviderStateLegacy) map[string]earningsProviderState {
	if legacy == nil {
		return nil
	}
	providers := make(map[string]earningsProviderState, len(legacy))
	for provider, state := range legacy {
		attempt := state.LastAttempt
		converted := earningsProviderAttempt{
			Status: attempt.Status, Entry: attempt.Entry, AttemptedAt: attempt.AttemptedAt,
			CompletedAt: attempt.CompletedAt, NextAttempt: attempt.NextAttempt, LastFailure: attempt.LastFailure,
		}
		if provider == earningsNasdaqProvider && attempt.Status == rpc.EarningsStatusFormatChange {
			converted.ParserContractVersion = earningsNasdaqParserContractLegacy
		}
		providers[provider] = earningsProviderState{LastAttempt: converted, LastGood: state.LastGood}
	}
	return providers
}

var errInvalidEarningsIdentityObservation = errors.New("invalid retained earnings identity observation")

func validateEarningsIdentityProofObservations(ctx context.Context, store *corestore.Store, symbols map[string]earningsSymbolState, stateRevision int64) error {
	for _, state := range symbols {
		if state.Identity == nil || state.Identity.LastNotApplicable == nil {
			continue
		}
		proof := state.Identity.LastNotApplicable
		if store == nil || proof.AuthorityRevision <= 0 || proof.AuthorityRevision > stateRevision || proof.ObservationID <= 0 {
			return errInvalidEarningsIdentityObservation
		}
		observation, found, err := store.ExactDecisionEligibleObservation(ctx, proof.ObservationID,
			earningsAuthorityScope, earningsIdentityObservationSource, earningsIdentityObservationKind, proof.ObservedAt)
		if err != nil {
			return fmt.Errorf("%w: authority read failed", errInvalidEarningsIdentityObservation)
		}
		if !found || observation.ID != proof.ObservationID ||
			!validEarningsIdentityProofObservation(*state.Identity, *proof, observation) {
			return errInvalidEarningsIdentityObservation
		}
	}
	return nil
}

func validEarningsIdentityProofObservation(state earningsIdentityState, proof earningsIdentityProof, observation corestore.Observation) bool {
	if observation.ID != proof.ObservationID || observation.ScopeKey != earningsAuthorityScope ||
		observation.Source != earningsIdentityObservationSource || observation.Kind != earningsIdentityObservationKind ||
		observation.ContentType != "application/json" || !observation.DecisionEligible ||
		!observation.ObservedAt.Equal(proof.ObservedAt) {
		return false
	}
	payloadDigest := sha256.Sum256(observation.Payload)
	if payloadDigest != observation.PayloadSHA256 || earningsIdentityDigestFingerprint(observation.PayloadSHA256) != proof.AuthorityFingerprint {
		return false
	}
	var payload earningsIdentityObservationPayload
	if decodeStrictMarketEventJSON(observation.Payload, &payload) != nil || payload.Version != earningsIdentityObservationVersion {
		return false
	}
	attempt := payload.Attempt
	if attempt.ConID != proof.ConID || attempt.SecType != "STK" || attempt.Outcome != earningsIdentityNotApplicable ||
		attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) ||
		!attempt.CompletedAt.Equal(proof.ObservedAt) || attempt.LastFailure != nil || attempt.NextAttempt == nil ||
		!attempt.NextAttempt.Equal(attempt.CompletedAt.Add(earningsFreshWindow)) {
		return false
	}
	if state.LastAttempt.LastFailure == nil && !sameEarningsIdentityProofAttempt(state.LastAttempt, attempt) {
		return false
	}
	return true
}

func sameEarningsIdentityProofAttempt(a, b earningsIdentityAttempt) bool {
	return a.ConID == b.ConID && a.SecType == b.SecType && a.Outcome == b.Outcome &&
		a.AttemptedAt.Equal(b.AttemptedAt) && a.CompletedAt.Equal(b.CompletedAt) &&
		a.NextAttempt != nil && b.NextAttempt != nil && a.NextAttempt.Equal(*b.NextAttempt) &&
		a.LastFailure == nil && b.LastFailure == nil
}

func validateEarningsSymbols(symbols map[string]earningsSymbolState, now time.Time) error {
	for symbol, state := range symbols {
		canonical := strings.ToUpper(strings.TrimSpace(symbol))
		if canonical == "" || canonical != symbol || state.Providers == nil || state.UpdatedAt.IsZero() || now.Before(state.UpdatedAt) {
			return errors.New("invalid earnings symbol state")
		}
		if !validAggregateEarningsStatus(state.Resolution.Status) {
			return errors.New("invalid earnings resolution")
		}
		recomputed := resolveEarningsState(state.Providers, state.Identity, state.UpdatedAt)
		if !sameEarningsResolution(state.Resolution, recomputed) {
			if !legacyWSHNotEntitledAggregate(state.Resolution, state.Providers, recomputed) {
				return errors.New("inconsistent earnings resolution")
			}
			state.Resolution = recomputed
			symbols[symbol] = state
		}
		if state.Resolution.Status == rpc.EarningsStatusDate {
			if state.Resolution.Entry == nil || validateEarningsRowShape(symbol, *state.Resolution.Entry) != nil {
				return errors.New("invalid earnings resolution date")
			}
		} else if state.Resolution.Entry != nil {
			return errors.New("unresolved earnings state carries a date")
		}
		for provider, providerState := range state.Providers {
			if provider != earningsNasdaqProvider && provider != earningsWSHProvider {
				return errors.New("invalid earnings provider")
			}
			if err := validateEarningsProviderState(symbol, provider, providerState, now); err != nil {
				return err
			}
			if state.UpdatedAt.Before(providerState.LastAttempt.CompletedAt) {
				return errors.New("earnings resolution predates provider attempt")
			}
		}
		if state.Identity != nil {
			if err := validateEarningsIdentityState(*state.Identity, now); err != nil {
				return err
			}
			if state.UpdatedAt.Before(state.Identity.LastAttempt.CompletedAt) {
				return errors.New("earnings resolution predates identity attempt")
			}
		}
	}
	return nil
}

func legacyWSHNotEntitledAggregate(stored earningsResolution, providers map[string]earningsProviderState, recomputed earningsResolution) bool {
	observed, exact := earningsNasdaqObservedWithWSHNotEntitled(providers)
	return stored.Status == rpc.EarningsStatusTransportFailure && stored.Reason == rpc.EarningsStatusTransportFailure &&
		stored.Entry == nil && !stored.Stale && exact && recomputed.Status == observed &&
		(observed == rpc.EarningsStatusNoDatePublished || observed == rpc.EarningsStatusUnsupportedSecurity)
}

func validAggregateEarningsStatus(status string) bool {
	return validEarningsProviderStatus(status) || status == rpc.EarningsStatusConflictingSources || status == rpc.EarningsStatusNotApplicable
}

func sameEarningsResolution(a, b earningsResolution) bool {
	if a.Status != b.Status || a.Reason != b.Reason || a.Stale != b.Stale {
		return false
	}
	if a.Entry == nil || b.Entry == nil {
		return a.Entry == nil && b.Entry == nil
	}
	return *a.Entry == *b.Entry
}

func validateEarningsProviderState(symbol, provider string, state earningsProviderState, now time.Time) error {
	attempt := state.LastAttempt
	if !validEarningsProviderStatus(attempt.Status) || attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) || now.Before(attempt.CompletedAt) {
		return errors.New("invalid earnings provider attempt")
	}
	if provider == earningsNasdaqProvider {
		if !validNasdaqParserContract(attempt.Status, attempt.ParserContractVersion) {
			return errors.New("invalid earnings Nasdaq parser contract")
		}
	} else if attempt.ParserContractVersion != 0 {
		return errors.New("unexpected earnings parser contract")
	}
	if attempt.NextAttempt == nil || attempt.NextAttempt.Before(attempt.CompletedAt) {
		return errors.New("invalid earnings provider retry")
	}
	if attempt.Status == rpc.EarningsStatusDate {
		if attempt.Entry == nil || validateEarningsRowShape(symbol, *attempt.Entry) != nil || attempt.LastFailure != nil {
			return errors.New("invalid earnings provider date")
		}
	} else if attempt.Entry != nil {
		return errors.New("unresolved earnings provider attempt carries a date")
	}
	if attempt.Status == rpc.EarningsStatusTransportFailure || attempt.Status == rpc.EarningsStatusFormatChange {
		if attempt.LastFailure == nil || !validEarningsSourceFailure(*attempt.LastFailure) || !attempt.LastFailure.FailedAt.Equal(attempt.CompletedAt) {
			return errors.New("invalid earnings provider failure")
		}
	} else if attempt.LastFailure != nil {
		return errors.New("semantic earnings provider outcome carries a failure")
	}
	if state.LastGood != nil {
		if err := validateEarningsRowShape(symbol, *state.LastGood); err != nil || now.Before(state.LastGood.ObservedAt) {
			return errors.New("invalid earnings provider last-good")
		}
	}
	return nil
}

func validNasdaqParserContract(status string, version int) bool {
	switch version {
	case 0:
		// Only migrated pre-version attempts may remain unlabelled, and an old
		// format failure must be identified so the corrected parser retries it.
		return status != rpc.EarningsStatusFormatChange
	case earningsNasdaqParserContractLegacy:
		return status == rpc.EarningsStatusFormatChange
	default:
		// Every exact contract labels its own outcomes coherently, so a
		// superseded one stays readable; a version this binary never wrote is
		// not a contract it can vouch for.
		return version >= earningsNasdaqParserContractFirstExact && version <= earningsNasdaqParserContract
	}
}

func validateEarningsIdentityState(state earningsIdentityState, now time.Time) error {
	attempt := state.LastAttempt
	if attempt.ConID <= 0 || attempt.SecType != "STK" || !validEarningsIdentityOutcome(attempt.Outcome) ||
		attempt.AttemptedAt.IsZero() || attempt.CompletedAt.IsZero() || attempt.CompletedAt.Before(attempt.AttemptedAt) || now.Before(attempt.CompletedAt) {
		return errors.New("invalid earnings identity attempt")
	}
	if attempt.NextAttempt == nil {
		return errors.New("invalid earnings identity retry")
	}
	wantNext := attempt.CompletedAt.Add(earningsFreshWindow)
	if attempt.LastFailure != nil {
		if attempt.Outcome != earningsIdentityUnknown || !validEarningsSourceFailure(*attempt.LastFailure) ||
			!attempt.LastFailure.FailedAt.Equal(attempt.CompletedAt) {
			return errors.New("invalid earnings identity failure")
		}
		if attempt.LastFailure.Stage == rpc.SourceFailureStageWSHContractResolve {
			wantNext = attempt.CompletedAt.Add(earningsContractResolutionRetry)
		}
	} else if attempt.Outcome != earningsIdentityNotApplicable && attempt.Outcome != earningsIdentityIssuer && attempt.Outcome != earningsIdentityUnknown {
		return errors.New("invalid earnings identity outcome")
	}
	if !attempt.NextAttempt.Equal(wantNext) {
		return errors.New("invalid earnings identity retry")
	}
	if state.LastNotApplicable != nil {
		proof := state.LastNotApplicable
		if proof.ConID != attempt.ConID || proof.SecType != "STK" || proof.ObservedAt.IsZero() ||
			now.Before(proof.ObservedAt) || proof.ObservedAt.After(attempt.CompletedAt) || proof.AuthorityRevision <= 0 ||
			!validAlertRegistryFingerprint(proof.AuthorityFingerprint) || proof.ObservationID <= 0 {
			return errors.New("invalid earnings identity proof")
		}
		if attempt.LastFailure != nil && (attempt.LastFailure.Stage != rpc.SourceFailureStageWSHContractResolve || !attempt.LastFailure.Retryable) {
			return errors.New("invalid earnings identity proof retention")
		}
		if attempt.LastFailure == nil && (attempt.Outcome != earningsIdentityNotApplicable || !proof.ObservedAt.Equal(attempt.CompletedAt)) {
			return errors.New("invalid earnings identity proof")
		}
	} else if attempt.Outcome == earningsIdentityNotApplicable && attempt.LastFailure == nil {
		return errors.New("missing earnings identity proof")
	}
	return nil
}

func validateEarningsRow(sym string, entry earningsEntry, now time.Time) error {
	if err := validateEarningsRowShape(sym, entry); err != nil {
		return err
	}
	if now.Before(entry.ObservedAt) {
		return errors.New("invalid earnings row")
	}
	return nil
}

func validateEarningsRowShape(sym string, entry earningsEntry) error {
	canonical := strings.ToUpper(strings.TrimSpace(sym))
	if canonical == "" || canonical != sym || nasdaqSymbol(canonical) == "" || entry.ObservedAt.IsZero() {
		return errors.New("invalid earnings row")
	}
	parsed, err := time.Parse(time.DateOnly, entry.Date)
	if err != nil || parsed.Format(time.DateOnly) != entry.Date {
		return errors.New("invalid earnings date")
	}
	switch entry.TimeOfDay {
	case "", "amc", "bmo":
		return nil
	default:
		return errors.New("invalid earnings session")
	}
}

// save writes only the sealed v1 JSON format used by the cutover importer.
func (s *earningsStore) save(entries map[string]earningsEntry) error {
	if s.authority != nil {
		return errors.New("legacy earnings save unavailable after SQLite authority attachment")
	}
	env := earningsPersistEnvelopeV1{Version: earningsLegacyVersion, Entries: entries}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, earningsStoreFilename)
	tmp, err := os.CreateTemp(s.dir, earningsStoreFilename+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode %s: %w", earningsStoreFilename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", earningsStoreFilename, err)
	}
	return nil
}

func migrateEarningsV1(entries map[string]earningsEntry, now time.Time) map[string]earningsSymbolState {
	symbols := make(map[string]earningsSymbolState, len(entries))
	for symbol, entry := range entries {
		next := entry.ObservedAt.Add(earningsFreshWindow)
		entryCopy := entry
		provider := earningsProviderState{
			LastAttempt: earningsProviderAttempt{
				Status: rpc.EarningsStatusDate, Entry: &entryCopy,
				AttemptedAt: entry.ObservedAt, CompletedAt: entry.ObservedAt, NextAttempt: &next,
			},
			LastGood: &entryCopy,
		}
		providers := map[string]earningsProviderState{earningsNasdaqProvider: provider}
		symbols[symbol] = earningsSymbolState{
			Resolution: resolveEarningsState(providers, nil, now), Providers: providers, UpdatedAt: now,
		}
	}
	return symbols
}

func resolvedEarningsEntries(symbols map[string]earningsSymbolState, now time.Time) map[string]earningsEntry {
	entries := map[string]earningsEntry{}
	for symbol, state := range symbols {
		resolution := resolveEarningsState(state.Providers, state.Identity, now)
		if resolution.Status == rpc.EarningsStatusDate && resolution.Entry != nil {
			entries[symbol] = *resolution.Entry
		}
	}
	return entries
}

func cloneEarningsSymbols(in map[string]earningsSymbolState) map[string]earningsSymbolState {
	out := make(map[string]earningsSymbolState, len(in))
	for symbol, state := range in {
		out[symbol] = cloneEarningsSymbolState(state)
	}
	return out
}

func cloneEarningsSymbolState(in earningsSymbolState) earningsSymbolState {
	out := in
	if in.Resolution.Entry != nil {
		entry := *in.Resolution.Entry
		out.Resolution.Entry = &entry
	}
	out.Providers = make(map[string]earningsProviderState, len(in.Providers))
	for provider, state := range in.Providers {
		copyState := state
		copyState.LastAttempt.Entry = cloneEarningsEntry(state.LastAttempt.Entry)
		copyState.LastAttempt.NextAttempt = cloneTimePointer(state.LastAttempt.NextAttempt)
		copyState.LastAttempt.LastFailure = cloneEarningsSourceFailure(state.LastAttempt.LastFailure)
		copyState.LastGood = cloneEarningsEntry(state.LastGood)
		out.Providers[provider] = copyState
	}
	if in.Identity != nil {
		identity := *in.Identity
		identity.LastAttempt.NextAttempt = cloneTimePointer(in.Identity.LastAttempt.NextAttempt)
		identity.LastAttempt.LastFailure = cloneEarningsSourceFailure(in.Identity.LastAttempt.LastFailure)
		if in.Identity.LastNotApplicable != nil {
			proof := *in.Identity.LastNotApplicable
			identity.LastNotApplicable = &proof
		}
		out.Identity = &identity
	}
	return out
}

func cloneEarningsEntry(entry *earningsEntry) *earningsEntry {
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	return &copyEntry
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneEarningsSourceFailure(failure *rpc.SourceFailure) *rpc.SourceFailure {
	if failure == nil {
		return nil
	}
	copyFailure := *failure
	return &copyFailure
}
