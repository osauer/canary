package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type syntheticEarningsIdentityClient struct {
	detail *ibkrlib.ContractDetailsLite
	err    error
}

const syntheticIdentityFingerprint = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func (c syntheticEarningsIdentityClient) ResolveWSHStockIdentity(context.Context, string, int) (*ibkrlib.ContractDetailsLite, error) {
	return c.detail, c.err
}

func TestFetchEarningsIdentityProjectsOnlyClosedExactBrokerClasses(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const conID = 7001
	tests := []struct {
		name    string
		detail  *ibkrlib.ContractDetailsLite
		want    string
		failure bool
	}{
		{name: "exact ETF", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STK", StockType: "ETF"}, want: earningsIdentityNotApplicable},
		{name: "exact common issuer", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STK", StockType: "COMMON"}, want: earningsIdentityIssuer},
		{name: "missing classification", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STK"}, want: earningsIdentityUnknown},
		{name: "unknown closed code", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STK", StockType: "FUND"}, want: earningsIdentityUnknown},
		{name: "noncanonical ETF token", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STK", StockType: "etf"}, want: earningsIdentityUnknown},
		{name: "mismatched contract", detail: &ibkrlib.ContractDetailsLite{ConID: conID + 1, SecType: "STK", StockType: "ETF"}, want: earningsIdentityUnknown, failure: true},
		{name: "noncanonical returned type", detail: &ibkrlib.ContractDetailsLite{ConID: conID, SecType: "STOCK", StockType: "ETF"}, want: earningsIdentityUnknown, failure: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fetchEarningsIdentityFrom(t.Context(), "SYNTH1", conID, now, syntheticEarningsIdentityClient{detail: tc.detail})
			if got.Outcome != tc.want || (got.Failure != nil) != tc.failure || (err != nil) != tc.failure {
				t.Fatal("identity classification did not stay within the closed contract")
			}
			if got.Failure != nil && (got.Failure.Stage != rpc.SourceFailureStageWSHContractResolve || got.Failure.Code != rpc.SourceFailureContractUnavailable) {
				t.Fatal("identity mismatch did not use the typed resolution failure")
			}
		})
	}
}

func TestFetchEarningsIdentityRetainsProofOnlyForTemporaryLookupFailure(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	result, err := fetchEarningsIdentityFrom(t.Context(), "SYNTH1", 7001, now, syntheticEarningsIdentityClient{
		err: &ibkrlib.WSHError{Kind: ibkrlib.WSHErrorContractResolution, Operation: "resolve_contract"},
	})
	if err == nil || result.Failure == nil || !result.RetainProof {
		t.Fatal("temporary exact lookup failure did not preserve the typed last-good policy")
	}
	result, err = fetchEarningsIdentityFrom(t.Context(), "SYNTH1", 7001, now, syntheticEarningsIdentityClient{
		detail: &ibkrlib.ContractDetailsLite{ConID: 7002, SecType: "STK", StockType: "ETF"},
	})
	if err == nil || result.Failure == nil || result.RetainProof {
		t.Fatal("positive identity mismatch was treated as a temporary lookup failure")
	}
}

func TestEarningsIdentityInputSecurityTypeAliases(t *testing.T) {
	for _, secType := range []string{"STK", "STOCK", "ETF"} {
		if canonicalEarningsIdentitySecType(secType) != "STK" {
			t.Fatal("stock identity alias was not canonicalized")
		}
	}
	for _, secType := range []string{"", "OPT", "FUND"} {
		if canonicalEarningsIdentitySecType(secType) != "" {
			t.Fatal("non-stock identity type was accepted")
		}
	}
}

func TestEarningsIdentityAggregateIsBoundAndFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const conID = 7001
	providerNext := now.Add(earningsFreshWindow)
	noDateProviders := map[string]earningsProviderState{
		earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{
			Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &providerNext,
		}},
		earningsWSHProvider: {LastAttempt: earningsProviderAttempt{
			Status: rpc.EarningsStatusTransportFailure, AttemptedAt: now, CompletedAt: now, NextAttempt: &providerNext,
			LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, FailedAt: now, Retryable: false},
		}},
	}
	identityNext := now.Add(earningsFreshWindow)
	identity := &earningsIdentityState{
		LastAttempt:       earningsIdentityAttempt{ConID: conID, SecType: "STK", Outcome: earningsIdentityNotApplicable, AttemptedAt: now, CompletedAt: now, NextAttempt: &identityNext},
		LastNotApplicable: &earningsIdentityProof{ConID: conID, SecType: "STK", ObservedAt: now, AuthorityRevision: 1, AuthorityFingerprint: syntheticIdentityFingerprint, ObservationID: 1},
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["SYNTH1"] = earningsSymbolState{
		Resolution: resolveEarningsState(noDateProviders, identity, now), Providers: noDateProviders, Identity: identity, UpdatedAt: now,
	}

	view, ok := cache.resolutionForIdentity("SYNTH1", conID, "STOCK")
	if !ok || view.Status != rpc.EarningsStatusNotApplicable || view.Identity == nil || !view.Identity.NotApplicable || len(view.Providers) != 2 {
		t.Fatal("exact ETF proof did not resolve independently of provider failures")
	}
	if view.Providers[1].LastFailure == nil && view.Providers[0].LastFailure == nil {
		t.Fatal("WSH entitlement failure disappeared behind applicability")
	}
	if public, _ := cache.resolution("SYNTH1"); public.Status == rpc.EarningsStatusNotApplicable {
		t.Fatal("unbound resolution reused contract applicability")
	}
	if mismatch, _ := cache.resolutionForIdentity("SYNTH1", conID+1, "STK"); mismatch.Status == rpc.EarningsStatusNotApplicable {
		t.Fatal("changed contract identity reused applicability")
	}
	if !earningsIdentityDue(identity, earningsRefreshTarget{Symbol: "SYNTH1", ConID: conID + 1, SecType: "STK"}, now) {
		t.Fatal("changed contract identity was not immediately due")
	}
	cache.clock = func() time.Time { return identityNext }
	if due, _ := cache.resolutionForIdentity("SYNTH1", conID, "STK"); due.Status != rpc.EarningsStatusNotApplicable {
		t.Fatal("due refresh created a transient applicability gap")
	}
	cache.clock = func() time.Time { return now.Add(2 * earningsTTL) }
	if old, _ := cache.resolutionForIdentity("SYNTH1", conID, "STK"); old.Status != rpc.EarningsStatusNotApplicable {
		t.Fatal("proof age invented an applicability expiry")
	}

	entry := earningsEntry{Date: "2026-08-15", ObservedAt: now}
	dateProviders := map[string]earningsProviderState{earningsNasdaqProvider: {
		LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusDate, Entry: &entry, AttemptedAt: now, CompletedAt: now, NextAttempt: &providerNext},
		LastGood:    &entry,
	}}
	if got := resolveEarningsState(dateProviders, identity, now); got.Status != rpc.EarningsStatusConflictingSources || got.Entry != nil {
		t.Fatal("date plus broker nonissuer did not fail closed")
	}
}

func TestEarningsIdentityPersistsRetryAndRetainedProofAcrossRealReopen(t *testing.T) {
	base := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const conID = 7001
	const sentinel = "SENTINEL-7F4C8A1E"
	const privateSymbol = "SYNTH7F4C8A1E"
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "daemon.db")
	openStore := func() *corestore.Store {
		store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	newCache := func(now *time.Time, logs *[]string) *earningsCache {
		cache := newEarningsCacheMemory(func(format string, args ...any) {
			*logs = append(*logs, fmt.Sprintf(format, args...))
		})
		cache.clock = func() time.Time { return *now }
		cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
			body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix(nasdaqSymbol(privateSymbol))}, http.StatusOK)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}
		return cache
	}

	now := base
	var logs []string
	store := openStore()
	cache := newCache(&now, &logs)
	identityCalls := 0
	if err := cache.setIdentityFetcher(func(context.Context, string, int) (earningsIdentityFetchResult, error) {
		identityCalls++
		return earningsIdentityFetchResult{Outcome: earningsIdentityNotApplicable}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: privateSymbol, ConID: conID, SecType: "ETF"})
	if identityCalls != 1 {
		t.Fatal("initial identity fetch count mismatch")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	now = base.Add(earningsFreshWindow)
	store = openStore()
	cache = newCache(&now, &logs)
	identityCalls = 0
	if err := cache.setIdentityFetcher(func(context.Context, string, int) (earningsIdentityFetchResult, error) {
		identityCalls++
		return earningsIdentityFailure(rpc.SourceFailureContractUnavailable, now), errors.New(sentinel)
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if view, _ := cache.resolutionForIdentity(privateSymbol, conID, "STOCK"); view.Status != rpc.EarningsStatusNotApplicable {
		t.Fatal("reopen lost exact applicability before its due refresh")
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: privateSymbol, ConID: conID, SecType: "STOCK"})
	wantRetry := now.Add(earningsContractResolutionRetry)
	view, ok := cache.resolutionForIdentity(privateSymbol, conID, "STK")
	if !ok || view.Status != rpc.EarningsStatusNotApplicable || view.Identity == nil || !view.Identity.NotApplicable ||
		view.Identity.Outcome != earningsIdentityUnknown || view.Identity.LastFailure == nil ||
		view.Identity.NextAttempt == nil || !view.Identity.NextAttempt.Equal(wantRetry) ||
		view.Identity.AuthorityRevision <= 0 || !validAlertRegistryFingerprint(view.Identity.AuthorityFingerprint) ||
		!validOpaqueEarningsIdentityObservationID(view.Identity.ObservationID) ||
		view.Identity.ProofOutcome != rpc.EarningsStatusNotApplicable ||
		view.Identity.AuthorityBinding == "" ||
		view.Identity.AuthorityBinding != rpc.BuildEarningsIdentityAuthorityBinding(privateSymbol, *view.Identity) {
		t.Fatal("temporary exact-resolution failure did not retain typed applicability and retry")
	}
	observations, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsIdentityObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatal("identity attempts did not create one typed observation each")
	}
	firstHash := sha256.Sum256(observations[0].Payload)
	if view.Identity.AuthorityFingerprint != "sha256:"+hex.EncodeToString(firstHash[:]) {
		t.Fatal("retained proof lost its append-only observation linkage")
	}
	if view.Identity.ObservationID != opaqueEarningsIdentityObservationID(observations[0].ID, view.Identity.AuthorityFingerprint) {
		t.Fatal("retained proof lost its opaque observation linkage")
	}
	for _, observation := range observations {
		payload := string(observation.Payload)
		if strings.Contains(payload, sentinel) || strings.Contains(payload, privateSymbol) || strings.Contains(payload, "stock_type") || strings.Contains(payload, "ETF") {
			t.Fatal("identity observation escaped the closed typed payload")
		}
	}
	doc, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatal("state read failed")
	}
	var persisted earningsPersistEnvelope
	if json.Unmarshal(doc.JSON, &persisted) != nil || persisted.Symbols[privateSymbol].Identity == nil ||
		persisted.Symbols[privateSymbol].Identity.LastNotApplicable == nil ||
		persisted.Symbols[privateSymbol].Identity.LastNotApplicable.ObservationID != observations[0].ID {
		t.Fatal("persisted proof did not retain its immutable observation receipt")
	}
	if strings.Contains(string(doc.JSON), sentinel) || strings.Contains(strings.Join(logs, "\n"), sentinel) || strings.Contains(strings.Join(logs, "\n"), privateSymbol) {
		t.Fatal("private identity input or free text escaped the typed boundary")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	now = wantRetry.Add(-time.Minute)
	store = openStore()
	cache = newCache(&now, &logs)
	identityCalls = 0
	if err := cache.setIdentityFetcher(func(context.Context, string, int) (earningsIdentityFetchResult, error) {
		identityCalls++
		return earningsIdentityFetchResult{Outcome: earningsIdentityIssuer}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: privateSymbol, ConID: conID, SecType: "STK"})
	if identityCalls != 0 {
		t.Fatal("identity was fetched before the persisted retry")
	}
	now = wantRetry
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: privateSymbol, ConID: conID, SecType: "STK"})
	if identityCalls != 1 {
		t.Fatal("identity was not fetched exactly once at the persisted retry")
	}
	if got, _ := cache.resolutionForIdentity(privateSymbol, conID, "STK"); got.Status == rpc.EarningsStatusNotApplicable {
		t.Fatal("successful issuer classification retained obsolete applicability")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEarningsIdentityProofRestartRejectsUnboundObservationAuthority(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		appendObservation  bool
		observationOutcome string
		observationVersion int
		mutateObservation  func(*corestore.ObservationInput)
		mutateProof        func(*earningsIdentityProof, corestore.ObservationReceipt)
	}{
		{name: "missing observation"},
		{name: "wrong receipt id", appendObservation: true, mutateProof: func(proof *earningsIdentityProof, receipt corestore.ObservationReceipt) {
			proof.ObservationID = receipt.ID + 1
		}},
		{name: "wrong digest", appendObservation: true, mutateProof: func(proof *earningsIdentityProof, _ corestore.ObservationReceipt) {
			proof.AuthorityFingerprint = syntheticIdentityFingerprint
		}},
		{name: "wrong payload", appendObservation: true, observationOutcome: earningsIdentityIssuer},
		{name: "wrong payload version", appendObservation: true, observationVersion: earningsIdentityObservationVersion + 1},
		{name: "future state revision", appendObservation: true, mutateProof: func(proof *earningsIdentityProof, _ corestore.ObservationReceipt) {
			proof.AuthorityRevision = 2
		}},
		{name: "decision ineligible", appendObservation: true, mutateObservation: func(observation *corestore.ObservationInput) {
			observation.DecisionEligible = false
		}},
		{name: "wrong source", appendObservation: true, mutateObservation: func(observation *corestore.ObservationInput) {
			observation.Source = "ibkr.contract_details.other"
		}},
		{name: "wrong kind", appendObservation: true, mutateObservation: func(observation *corestore.ObservationInput) {
			observation.Kind = "earnings_dates.identity_outcome.other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openMarketTestCoreStore(t)
			const conID = 7001
			next := now.Add(earningsFreshWindow)
			stateAttempt := earningsIdentityAttempt{
				ConID: conID, SecType: "STK", Outcome: earningsIdentityNotApplicable,
				AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
			}
			observationAttempt := stateAttempt
			if test.observationOutcome != "" {
				observationAttempt.Outcome = test.observationOutcome
			}
			observation, err := earningsIdentityObservation(observationAttempt)
			if err != nil {
				t.Fatal("identity observation fixture encoding failed")
			}
			if test.observationVersion != 0 {
				observation.Payload, err = json.Marshal(earningsIdentityObservationPayload{
					Version: test.observationVersion, Attempt: observationAttempt,
				})
				if err != nil {
					t.Fatal("identity observation version fixture encoding failed")
				}
			}
			if test.mutateObservation != nil {
				test.mutateObservation(&observation)
			}
			receipt := corestore.ObservationReceipt{}
			if test.appendObservation {
				receipt, err = store.AppendObservation(t.Context(), observation)
				if err != nil {
					t.Fatal("identity observation fixture append failed")
				}
			}
			proof := &earningsIdentityProof{
				ConID: conID, SecType: "STK", ObservedAt: now, AuthorityRevision: 1,
				AuthorityFingerprint: syntheticIdentityFingerprint, ObservationID: 9001,
			}
			if receipt.ID > 0 {
				proof.AuthorityFingerprint = earningsIdentityDigestFingerprint(receipt.PayloadSHA256)
				proof.ObservationID = receipt.ID
			}
			if test.mutateProof != nil {
				test.mutateProof(proof, receipt)
			}
			identity := &earningsIdentityState{LastAttempt: stateAttempt, LastNotApplicable: proof}
			providerNext := now.Add(earningsFreshWindow)
			providers := map[string]earningsProviderState{earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{
				Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &providerNext,
			}}}
			state := earningsSymbolState{
				Resolution: resolveEarningsState(providers, identity, now), Providers: providers, Identity: identity, UpdatedAt: now,
			}
			raw, err := json.Marshal(earningsPersistEnvelope{
				Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{"SYNTH1": state},
			})
			if err != nil {
				t.Fatal("identity state fixture encoding failed")
			}
			if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
				ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
			}); err != nil {
				t.Fatal("identity state fixture write failed")
			}
			cache := newEarningsCacheMemory(nil)
			cache.clock = func() time.Time { return now.Add(time.Minute) }
			err = cache.UseCoreStore(store)
			if err == nil {
				t.Fatal("unbound identity observation authority survived restart")
			}
			for _, forbidden := range []string{"SYNTH1", "7001", "9001", "TYPE_CODE"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatal("identity authority startup error exposed private evidence")
				}
			}
		})
	}
}

func TestEarningsIdentityDefinitiveSupersessionCASRollbackSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	identityOutcome := earningsIdentityNotApplicable
	newCache := func() *earningsCache {
		cache := newEarningsCacheMemory(nil)
		cache.clock = func() time.Time { return now }
		cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
			body := nasdaqTestPayload(t, map[string]any{"announcement": nasdaqAnnouncementPrefix("SYNTH1")}, http.StatusOK)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}
		if err := cache.setIdentityFetcher(func(context.Context, string, int) (earningsIdentityFetchResult, error) {
			return earningsIdentityFetchResult{Outcome: identityOutcome}, nil
		}); err != nil {
			t.Fatal("identity fetch fixture setup failed")
		}
		return cache
	}
	cache := newCache()
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal("identity authority fixture initialization failed")
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH1", ConID: 7001, SecType: "STK"})
	committed, ok := cache.resolutionForIdentity("SYNTH1", 7001, "STK")
	if !ok || committed.Status != rpc.EarningsStatusNotApplicable || committed.Identity == nil || !committed.Identity.NotApplicable {
		t.Fatal("initial receipt-bound non-reporting proof was not committed")
	}
	identityBefore, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsIdentityObservationSource, Kind: earningsIdentityObservationKind,
	})
	if err != nil || len(identityBefore) != 1 {
		t.Fatal("initial identity observation fixture is incomplete")
	}
	providerBefore, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(providerBefore) != 1 {
		t.Fatal("initial provider observation fixture is incomplete")
	}

	now = now.Add(earningsFreshWindow)
	identityOutcome = earningsIdentityIssuer
	doc, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatal("identity authority fixture state read failed")
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, ExpectedRevision: doc.Revision, JSON: doc.JSON,
	}); err != nil {
		t.Fatal("identity authority fixture revision advance failed")
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH1", ConID: 7001, SecType: "STK"})
	identityAfterConflict, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsIdentityObservationSource, Kind: earningsIdentityObservationKind,
	})
	if err != nil || len(identityAfterConflict) != len(identityBefore) {
		t.Fatal("failed definitive supersession CAS left an identity observation")
	}
	providerAfterConflict, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(providerAfterConflict) != len(providerBefore) {
		t.Fatal("failed definitive supersession CAS left a provider observation")
	}
	if view, ok := cache.resolutionForIdentity("SYNTH1", 7001, "STK"); !ok || view.Status != rpc.EarningsStatusNotApplicable || view.Identity == nil || !view.Identity.NotApplicable {
		t.Fatal("failed definitive supersession changed in-memory committed state")
	}

	// A crash/restart after the failed CAS can recover only the old complete
	// proof transaction. Retrying the definitive issuer observation then clears
	// that proof in the same transaction that retains the superseding receipt.
	restarted := newCache()
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal("restart could not recover the last complete proof transaction")
	}
	if view, ok := restarted.resolutionForIdentity("SYNTH1", 7001, "STK"); !ok || view.Status != rpc.EarningsStatusNotApplicable {
		t.Fatal("restart did not recover the last complete proof transaction")
	}
	restarted.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH1", ConID: 7001, SecType: "STK"})
	if view, ok := restarted.resolutionForIdentity("SYNTH1", 7001, "STK"); !ok || view.Status == rpc.EarningsStatusNotApplicable || view.Identity == nil || view.Identity.NotApplicable || view.Identity.Outcome != earningsIdentityIssuer {
		t.Fatal("successful definitive issuer supersession retained the old exemption")
	}
	identityAfterCommit, err := store.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsIdentityObservationSource, Kind: earningsIdentityObservationKind,
	})
	if err != nil || len(identityAfterCommit) != len(identityBefore)+1 {
		t.Fatal("successful definitive supersession did not append exactly one identity receipt")
	}
	afterCrash := newCache()
	if err := afterCrash.UseCoreStore(store); err != nil {
		t.Fatal("post-supersession restart failed")
	}
	if view, ok := afterCrash.resolutionForIdentity("SYNTH1", 7001, "STK"); !ok || view.Status == rpc.EarningsStatusNotApplicable || view.Identity == nil || view.Identity.NotApplicable || view.Identity.Outcome != earningsIdentityIssuer {
		t.Fatal("restart resurrected a definitively superseded exemption")
	}
}

func TestEarningsV2MigrationPreservesProviderDeadlinesAndMakesIdentityDue(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	next := now.Add(17 * time.Hour)
	providers := map[string]earningsProviderState{
		earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &next}},
		earningsWSHProvider:    {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusUnsupportedSecurity, AttemptedAt: now, CompletedAt: now, NextAttempt: &next}},
	}
	legacyProviders := map[string]earningsProviderStateLegacy{
		earningsNasdaqProvider: {LastAttempt: earningsProviderAttemptLegacy{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &next}},
		earningsWSHProvider:    {LastAttempt: earningsProviderAttemptLegacy{Status: rpc.EarningsStatusUnsupportedSecurity, AttemptedAt: now, CompletedAt: now, NextAttempt: &next}},
	}
	v2 := earningsPersistEnvelopeV2{Version: earningsPreviousPersistVersion, Symbols: map[string]earningsSymbolStateV2{
		"SYNTH1": {Resolution: resolveEarningsProviders(providers, now), Providers: legacyProviders, UpdatedAt: now},
	}}
	raw, err := json.Marshal(v2)
	if err != nil {
		t.Fatal(err)
	}
	store := openMarketTestCoreStore(t)
	created, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now.Add(time.Minute) }
	identityCalls := 0
	if err := cache.setIdentityFetcher(func(context.Context, string, int) (earningsIdentityFetchResult, error) {
		identityCalls++
		return earningsIdentityFetchResult{Outcome: earningsIdentityIssuer}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok || doc.Revision != created.Revision+1 {
		t.Fatal("v2 authority was not migrated in place")
	}
	var migrated earningsPersistEnvelope
	if err := json.Unmarshal(doc.JSON, &migrated); err != nil || migrated.Version != earningsPersistVersion {
		t.Fatal("migrated authority version mismatch")
	}
	for _, provider := range migrated.Symbols["SYNTH1"].Providers {
		if provider.LastAttempt.NextAttempt == nil || !provider.LastAttempt.NextAttempt.Equal(next) {
			t.Fatal("v2 provider retry deadline changed during migration")
		}
	}
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH1", ConID: 7001, SecType: "STOCK"})
	if identityCalls != 1 {
		t.Fatal("migrated v2 row did not make missing identity immediately due")
	}
}

func TestAssembleEarningsBrokerNonIssuerAndOverrideConflict(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const conID = 7001
	next := now.Add(earningsFreshWindow)
	providers := map[string]earningsProviderState{earningsWSHProvider: {LastAttempt: earningsProviderAttempt{
		Status: rpc.EarningsStatusTransportFailure, AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
		LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, FailedAt: now, Retryable: false},
	}}}
	identity := &earningsIdentityState{
		LastAttempt:       earningsIdentityAttempt{ConID: conID, SecType: "STK", Outcome: earningsIdentityNotApplicable, AttemptedAt: now, CompletedAt: now, NextAttempt: &next},
		LastNotApplicable: &earningsIdentityProof{ConID: conID, SecType: "STK", ObservedAt: now, AuthorityRevision: 1, AuthorityFingerprint: syntheticIdentityFingerprint, ObservationID: 1},
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["SYNTH1"] = earningsSymbolState{Resolution: resolveEarningsState(providers, identity, now), Providers: providers, Identity: identity, UpdatedAt: now}
	srv := &Server{earnings: cache}
	name := risk.NameInput{Symbol: "SYNTH1", StockConID: conID, StockSecType: "STOCK"}
	earnings, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{name}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusNotApplicable || infos[0].Source != "broker_identity" ||
		infos[0].Identity == nil || !infos[0].Identity.NotApplicable || len(infos[0].Providers) != 1 || infos[0].Providers[0].LastFailure == nil {
		t.Fatal("broker nonissuer did not resolve while preserving provider failure")
	}
	if got := earnings["SYNTH1"]; !got.NotApplicable || got.Known || got.TerminalNonReporting || got.Reason != risk.EarningsReasonBrokerNonIssuer {
		t.Fatal("broker nonissuer risk input mismatch")
	}
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded || health.Status != rpc.SourceStatusOK || health.LastFailure == nil || health.NextAttempt == nil ||
		!health.NextAttempt.Equal(next) || len(health.Notes) != 1 || !strings.Contains(health.Notes[0], "retained provider issue") {
		t.Fatal("resolved broker nonissuer degraded source health or hid failure")
	}
	mixedName := name
	mixedName.Legs = []risk.LegInput{{Desc: "synthetic option", Quantity: 1}}
	earnings, infos = srv.assembleEarnings(t.Context(), []risk.NameInput{mixedName}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusTransportFailure || infos[0].Identity != nil || earnings["SYNTH1"].NotApplicable {
		t.Fatal("stock identity proof exempted a mixed stock-and-option group")
	}

	srv.platformSettings = &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion, Features: platformFeatureSettingsData{
		Rulebook: platformRulebookSettingsData{EarningsOverrides: map[string]string{"SYNTH1": "2026-08-15"}},
	}}}
	earnings, infos = srv.assembleEarnings(t.Context(), []risk.NameInput{name}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusConflictingSources || earnings["SYNTH1"].Known || earnings["SYNTH1"].NotApplicable {
		t.Fatal("manual date overruled broker nonissuer instead of failing closed")
	}
}

// TestAssembleEarningsCommonIssuerWithoutDateRemainsUnknown pins the operator
// policy of 2026-08-03: even a broker-proven common issuer in the post-report
// gap is a disclosed vendor-definitive state, never a degraded source. The
// name's own rules keep reading unknown — the row, not the source health,
// carries the missing-date truth, and exemption stays impossible.
func TestAssembleEarningsCommonIssuerWithoutDateRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const conID = 7001
	next := now.Add(earningsFreshWindow)
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{
		Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
	}}}
	identity := &earningsIdentityState{LastAttempt: earningsIdentityAttempt{
		ConID: conID, SecType: "STK", Outcome: earningsIdentityIssuer, AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
	}}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["SYNTH1"] = earningsSymbolState{Resolution: resolveEarningsState(providers, identity, now), Providers: providers, Identity: identity, UpdatedAt: now}
	srv := &Server{earnings: cache}
	earnings, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "SYNTH1", StockConID: conID, StockSecType: "STOCK"}}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusNoDatePublished || infos[0].Identity == nil ||
		infos[0].Identity.Outcome != earningsIdentityIssuer || infos[0].Identity.NotApplicable {
		t.Fatal("COMMON identity changed the provider's typed no-date outcome")
	}
	if got := earnings["SYNTH1"]; got.Known || got.NotApplicable || got.TerminalNonReporting || got.Reason != rpc.EarningsStatusNoDatePublished {
		t.Fatal("COMMON issuer without a date became usable or exempt")
	}
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded || health.Status != rpc.SourceStatusOK || len(health.Notes) != 1 || !strings.Contains(health.Notes[0], "vendor definitive") {
		t.Fatalf("COMMON issuer gap was not disclosed as vendor-definitive: %+v degraded=%v", health, degraded)
	}
}

func TestAssembleEarningsDoesNotUseHedgeSymbolsAsApplicabilityAuthority(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	srv := &Server{}
	policy := risk.DefaultRulebookPolicy()
	if !policy.IsHedgeSymbol("SPY") {
		t.Fatal("test requires a configured hedge symbol")
	}
	_, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "SPY", StockConID: 7001, StockSecType: "STK"}}, policy, marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Symbol != "SPY" {
		t.Fatal("configured hedge symbol bypassed broker identity applicability")
	}
}

func TestNasdaqNoDateWithExactWSHNotEntitledRemainsUnknownButHealthy(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	base := map[string]earningsProviderState{
		earningsNasdaqProvider: {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished}},
		earningsWSHProvider:    {LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusTransportFailure, LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: false, FailedAt: now}}},
	}
	if got := resolveEarningsProviders(base, now); got.Status != rpc.EarningsStatusNoDatePublished {
		t.Fatalf("exact entitlement tuple = %s, want no_date_published", got.Status)
	}
	info := rpc.EarningsInfo{Status: rpc.EarningsStatusNoDatePublished, Providers: projectEarningsProviders(base)}
	health, degraded := rulesEarningsSourceHealth([]rpc.EarningsInfo{info}, now)
	if degraded || health.Status != rpc.SourceStatusOK || health.LastFailure == nil {
		t.Fatalf("typed no-date entitlement health = %+v degraded=%v", health, degraded)
	}
	unsupported := make(map[string]earningsProviderState, len(base))
	maps.Copy(unsupported, base)
	nasdaq := unsupported[earningsNasdaqProvider]
	nasdaq.LastAttempt.Status = rpc.EarningsStatusUnsupportedSecurity
	unsupported[earningsNasdaqProvider] = nasdaq
	if got := resolveEarningsProviders(unsupported, now); got.Status != rpc.EarningsStatusUnsupportedSecurity {
		t.Fatalf("exact unsupported entitlement tuple = %s", got.Status)
	}
	info = rpc.EarningsInfo{Status: rpc.EarningsStatusUnsupportedSecurity, Source: "fetched", Providers: projectEarningsProviders(unsupported)}
	if health, degraded := rulesEarningsSourceHealth([]rpc.EarningsInfo{info}, now); degraded || health.Status != rpc.SourceStatusOK {
		t.Fatalf("typed unsupported entitlement health = %+v degraded=%v", health, degraded)
	}
	for _, mutate := range []func(map[string]earningsProviderState){
		func(p map[string]earningsProviderState) {
			p[earningsWSHProvider] = earningsProviderState{LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusTransportFailure, LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: "other", Retryable: false}}}
		},
		func(p map[string]earningsProviderState) {
			p[earningsWSHProvider] = earningsProviderState{LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusTransportFailure, LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureNotEntitled, Stage: rpc.SourceFailureStageWSHMetadata, Retryable: true}}}
		},
		func(p map[string]earningsProviderState) {
			p[earningsWSHProvider] = earningsProviderState{LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusTransportFailure, LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageWSHMetadata}}}
		},
	} {
		providers := make(map[string]earningsProviderState, len(base))
		maps.Copy(providers, base)
		mutate(providers)
		if got := resolveEarningsProviders(providers, now); got.Status == rpc.EarningsStatusNoDatePublished {
			t.Fatal("non-exact WSH failure was accepted as no-date")
		}
		info := rpc.EarningsInfo{Status: rpc.EarningsStatusTransportFailure, Providers: projectEarningsProviders(providers)}
		if _, degraded := rulesEarningsSourceHealth([]rpc.EarningsInfo{info}, now); !degraded {
			t.Fatal("non-exact WSH failure did not degrade earnings health")
		}
	}
}

func TestEarningsValidationErrorsDoNotExposeIdentityKeys(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	const sentinel = "SENTINEL9D4F1C7B"
	raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{
		strings.ToLower(sentinel): {Providers: map[string]earningsProviderState{}, UpdatedAt: now},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeEarningsEnvelopeV4(raw, now)
	if err == nil {
		t.Fatal("invalid authority unexpectedly decoded")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(strings.ToUpper(err.Error()), sentinel) {
		t.Fatal("authority validation error exposed the private identity key")
	}
}

func TestBriefUnresolvedEarningsExcludesBothApplicabilityClasses(t *testing.T) {
	rules := &rpc.RulesResult{Earnings: []rpc.EarningsInfo{
		{Symbol: "SYNTH1", Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable},
		{Symbol: "SYNTH2", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting},
		{Symbol: "SYNTH3", Source: "unknown", Status: rpc.EarningsStatusNoDatePublished},
	}}
	got := briefUnresolvedEarnings(rules)
	if len(got) != 1 || !strings.Contains(got[0], "SYNTH3") {
		t.Fatal("daily brief mixed resolved applicability with unresolved issuer evidence")
	}
}

func TestRulesEarningsSourceHealthDisclosesRetainedIdentityFailureWithoutDegrading(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	next := now.Add(earningsContractResolutionRetry)
	health, degraded := rulesEarningsSourceHealth([]rpc.EarningsInfo{{
		Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable,
		Identity: &rpc.EarningsIdentityInfo{
			Outcome: earningsIdentityUnknown, NotApplicable: true, NextAttempt: &next,
			LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureContractUnavailable, Stage: rpc.SourceFailureStageWSHContractResolve, FailedAt: now},
		},
	}}, now)
	if degraded || health.Status != rpc.SourceStatusOK || health.NextAttempt == nil || !health.NextAttempt.Equal(next) ||
		len(health.Notes) != 1 || !strings.Contains(health.Notes[0], "retained broker identity issue") {
		t.Fatal("retained identity failure was hidden or degraded valid applicability")
	}
}

func TestBriefEarningsPositivelyDisclosesApplicabilityAndRetainedIssue(t *testing.T) {
	rules := &rpc.RulesResult{
		Earnings: []rpc.EarningsInfo{
			{Symbol: "SYNTH1", Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable},
			{Symbol: "SYNTH2", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting},
		},
		InputHealth: []rpc.SourceHealth{{
			Source: "earnings", Status: rpc.SourceStatusOK,
			Notes: []string{"retained broker identity issue: code=contract_unavailable stage=wsh_contract_resolve retry=scheduled"},
		}},
	}
	for _, row := range briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false) {
		if row.Kind != "earnings" {
			continue
		}
		if row.Status != rpc.BriefStatusOK || !strings.Contains(row.Detail, "1 broker-proven nonissuer") ||
			!strings.Contains(row.Detail, "1 terminal/non-reporting") || !strings.Contains(row.Detail, "informational issue") {
			t.Fatal("brief did not positively disclose both applicability classes and retained evidence")
		}
		return
	}
	t.Fatal("brief earnings row missing")
}
