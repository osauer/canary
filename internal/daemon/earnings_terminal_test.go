package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func syntheticTerminalDocument(now time.Time) earningsTerminalDocument {
	return earningsTerminalDocument{
		Version: earningsTerminalDocumentVersion, ReviewedAt: now,
		Contracts: []earningsTerminalRecord{{
			Contract: earningsTerminalContract{ConID: 1001, Symbol: "ACMEQ", SecType: "STK"},
			Issuer:   "Acme Example, Inc.", CIK: "0000001001",
			Classification:  earningsTerminalClassEquityCancelled,
			EffectiveDate:   now.AddDate(0, -1, 0).Format(time.DateOnly),
			VerifiedAt:      now,
			RevalidateAfter: now.AddDate(1, 0, 0),
			Evidence: []earningsTerminalReference{
				{Kind: "finra_uniform_practice_advisory", URL: "https://www.finra.org/sites/default/files/example-advisory.pdf"},
				{Kind: "sec_filing", URL: "https://www.sec.gov/Archives/edgar/data/1001/example.htm"},
			},
		}},
	}
}

func writeTerminalImport(t *testing.T, doc earningsTerminalDocument) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terminal-evidence.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEarningsTerminalAuthorityPersistsExactContractAndRecovers(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	store := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now)))
	if err := store.UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatalf("attach import: %v", err)
	}

	match, found := store.terminalEarningsFor(risk.NameInput{
		Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STOCK",
	}, now)
	if !found || match.Status != rpc.EarningsStatusTerminalNonReporting ||
		match.Info.AuthorityRevision != 1 || !strings.HasPrefix(match.Info.AuthorityFingerprint, "sha256:") ||
		match.Info.AuthorityBinding != rpc.BuildEarningsTerminalAuthorityBinding("ACMEQ", match.Info) ||
		!match.Info.AuthorityReviewedAt.Equal(now) ||
		len(match.Info.Evidence) != 2 {
		t.Fatalf("terminal match = %+v found=%v", match, found)
	}
	if _, found := store.terminalEarningsFor(risk.NameInput{
		Symbol: "ACMEQ", StockConID: 2002, StockSecType: "STK",
	}, now); found {
		t.Fatal("same ticker with another ConID received a terminal classification")
	}
	conflict, found := store.terminalEarningsFor(risk.NameInput{
		Symbol: "RENAMED", StockConID: 1001, StockSecType: "STK",
	}, now)
	if !found || conflict.Status != rpc.EarningsStatusConflictingSources || conflict.Reason != earningsTerminalReasonIdentityConflict {
		t.Fatalf("identity conflict = %+v found=%v", conflict, found)
	}
	expired, found := store.terminalEarningsFor(risk.NameInput{
		Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK",
	}, now.AddDate(1, 0, 0))
	if !found || expired.Status != rpc.EarningsStatusTerminalEvidenceExpired {
		t.Fatalf("expired classification = %+v found=%v", expired, found)
	}

	doc, ok, err := authority.GetStateDocument(context.Background(), earningsAuthorityScope, earningsTerminalStateKind)
	if err != nil || !ok || doc.Revision != 1 {
		t.Fatalf("SQLite authority: ok=%v revision=%d err=%v", ok, doc.Revision, err)
	}
	restarted := newEarningsTerminalStore("")
	if err := restarted.UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err != nil {
		t.Fatalf("restart attach: %v", err)
	}
	recovered, found := restarted.terminalEarningsFor(risk.NameInput{
		Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK",
	}, now.Add(time.Hour))
	if !found || recovered.Info.AuthorityRevision != 1 || recovered.Info.AuthorityFingerprint != match.Info.AuthorityFingerprint {
		t.Fatalf("recovered = %+v found=%v", recovered, found)
	}
}

func TestEarningsTerminalDoesNotExemptSameTickerOptionLegs(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	store := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now)))
	if err := store.UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	name := risk.NameInput{
		Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK",
		Legs: []risk.LegInput{{Desc: "ACMEQ option", Quantity: 1}},
	}
	if _, found := store.terminalEarningsFor(name, now); found {
		t.Fatal("exact stock terminal evidence exempted an option leg without an underlying ConID")
	}
}

func TestAnalysisPositionsExcludesOnlyCurrentExactTerminalStock(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	store := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now)))
	if err := store.UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	srv := &Server{earningsTerminal: store}
	positions := &rpc.PositionsResult{Stocks: []rpc.PositionView{
		{Symbol: "ACMEQ", SecType: "STK", ConID: 1001, Quantity: 4, Mark: 1, MarketValue: 4, Currency: "USD"},
		{Symbol: "ACMEQ", SecType: "STK", ConID: 2002, Quantity: 5, Mark: 2, MarketValue: 10, Currency: "USD"},
		{Symbol: "OTHER", SecType: "STK", ConID: 3003, Quantity: 1, Mark: 3, MarketValue: 3, Currency: "USD"},
	}, Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"}}
	filtered := srv.analysisPositions(positions, now)
	if filtered == positions || len(positions.Stocks) != 3 {
		t.Fatal("analysis projection mutated or replaced raw broker positions")
	}
	if len(filtered.Stocks) != 2 || filtered.Stocks[0].ConID != 2002 || filtered.Stocks[1].ConID != 3003 {
		t.Fatalf("analysis stocks = %+v", filtered.Stocks)
	}
	if filtered.Portfolio == nil || filtered.Portfolio.ExposureBase == nil || len(filtered.ByUnderlying) != 2 {
		t.Fatal("analysis projection did not rebuild dependent aggregates")
	}

	if got := srv.analysisPositions(positions, now.AddDate(1, 0, 0)); got != positions {
		t.Fatal("expired terminal evidence excluded a position from analysis")
	}
	positions.Stocks[0].Symbol = "RENAMED"
	if got := srv.analysisPositions(positions, now); got != positions {
		t.Fatal("identity-conflicting terminal evidence excluded a position from analysis")
	}
}

func TestEarningsTerminalAuthorityRejectsRollbackAndUnsafeInput(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	current := syntheticTerminalDocument(now)
	if err := newEarningsTerminalStore(writeTerminalImport(t, current)).UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}

	older := syntheticTerminalDocument(now.Add(-time.Hour))
	if err := newEarningsTerminalStore(writeTerminalImport(t, older)).UseCoreStore(t.Context(), authority, now); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("older import error = %v, want rollback rejection", err)
	}

	changedAtSameReview := syntheticTerminalDocument(now)
	changedAtSameReview.ReviewedAt = now.Add(time.Hour)
	changedAtSameReview.Contracts[0].Issuer = "Changed Example, Inc."
	if err := newEarningsTerminalStore(writeTerminalImport(t, changedAtSameReview)).UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "without a newer verified_at") {
		t.Fatalf("same-review mutation error = %v", err)
	}

	changedAtSameCatalogReview := syntheticTerminalDocument(now)
	changedAtSameCatalogReview.Contracts = append(changedAtSameCatalogReview.Contracts, earningsTerminalRecord{
		Contract:        earningsTerminalContract{ConID: 2002, Symbol: "OTHERQ", SecType: "STK"},
		Issuer:          "Other Example, Inc.",
		CIK:             "0000002002",
		Classification:  earningsTerminalClassIssuerDissolved,
		EffectiveDate:   now.AddDate(0, -1, 0).Format(time.DateOnly),
		VerifiedAt:      now,
		RevalidateAfter: now.AddDate(1, 0, 0),
		Evidence: []earningsTerminalReference{
			{Kind: "finra_uniform_practice_advisory", URL: "https://www.finra.org/sites/default/files/other-advisory.pdf"},
			{Kind: "sec_filing", URL: "https://www.sec.gov/Archives/edgar/data/2002/other.htm"},
		},
	})
	if err := newEarningsTerminalStore(writeTerminalImport(t, changedAtSameCatalogReview)).UseCoreStore(t.Context(), authority, now); err == nil || !strings.Contains(err.Error(), "catalog changed without a newer reviewed_at") {
		t.Fatalf("same-catalog-review mutation error = %v", err)
	}

	newerCatalogOlderRecord := syntheticTerminalDocument(now.Add(time.Hour))
	newerCatalogOlderRecord.Contracts[0].VerifiedAt = now.Add(-time.Hour)
	if err := newEarningsTerminalStore(writeTerminalImport(t, newerCatalogOlderRecord)).UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "precedes retained review") {
		t.Fatalf("newer catalog with older record error = %v", err)
	}

	badURL := syntheticTerminalDocument(now.Add(time.Hour))
	badURL.Contracts[0].Evidence[0].URL = "https://example.com/instructions"
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, badURL), now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("unallowlisted evidence error = %v", err)
	}

	wrongSECIdentity := syntheticTerminalDocument(now.Add(time.Hour))
	wrongSECIdentity.Contracts[0].Evidence[1].URL = "https://www.sec.gov/Archives/edgar/data/2002/wrong-issuer.htm"
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, wrongSECIdentity), now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "does not match cik") {
		t.Fatalf("wrong SEC identity error = %v", err)
	}

	zeroCIK := syntheticTerminalDocument(now.Add(time.Hour))
	zeroCIK.Contracts[0].CIK = "0000000000"
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, zeroCIK), now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "not all zero") {
		t.Fatalf("all-zero CIK error = %v", err)
	}
	optionalCIK := syntheticTerminalDocument(now.Add(time.Hour))
	optionalCIK.Contracts[0].CIK = ""
	optionalCIK.Contracts[0].Evidence[1] = earningsTerminalReference{
		Kind: "nasdaq_delisting_notice", URL: "https://www.nasdaq.com/press-release/example-delisting-notice",
	}
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, optionalCIK), now.Add(time.Hour)); err != nil {
		t.Fatalf("optional CIK with independent non-SEC evidence: %v", err)
	}

	futureCatalogReview := syntheticTerminalDocument(now.Add(time.Second))
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, futureCatalogReview), now); err == nil || !strings.Contains(err.Error(), "reviewed_at is in the future") {
		t.Fatalf("future reviewed_at error = %v", err)
	}
	futureRecordReview := syntheticTerminalDocument(now)
	futureRecordReview.Contracts[0].VerifiedAt = now.Add(time.Second)
	futureRecordReview.Contracts[0].RevalidateAfter = now.AddDate(1, 0, 0)
	if _, err := decodeEarningsTerminalDocument(mustTerminalJSON(t, futureRecordReview), now); err == nil || !strings.Contains(err.Error(), "verified_at is missing or in the future") {
		t.Fatalf("future verified_at error = %v", err)
	}

	missingCatalogReview := syntheticTerminalDocument(now.Add(time.Hour))
	missingCatalogReview.ReviewedAt = time.Time{}
	if _, err := readEarningsTerminalImport(writeTerminalImport(t, missingCatalogReview), now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "reviewed_at") {
		t.Fatalf("missing reviewed_at error = %v", err)
	}

	operatorAuthoredTombstone := syntheticTerminalDocument(now.Add(time.Hour))
	operatorAuthoredTombstone.Tombstones = []earningsTerminalTombstone{{ConID: 2002, RevokedAt: now}}
	if _, err := readEarningsTerminalImport(writeTerminalImport(t, operatorAuthoredTombstone), now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "unknown field \"tombstones\"") {
		t.Fatalf("operator-authored tombstone error = %v", err)
	}

	worldReadable := writeTerminalImport(t, syntheticTerminalDocument(now.Add(time.Hour)))
	if err := os.Chmod(worldReadable, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newEarningsTerminalStore(worldReadable).UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe file mode error = %v", err)
	}
}

func TestEarningsTerminalAuthorityAllowsExplicitRevocation(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	if err := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now))).UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	empty := earningsTerminalDocument{Version: earningsTerminalDocumentVersion, ReviewedAt: now.Add(time.Hour), Contracts: []earningsTerminalRecord{}}
	revoked := newEarningsTerminalStore(writeTerminalImport(t, empty))
	if err := revoked.UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, found := revoked.terminalEarningsFor(risk.NameInput{Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK"}, now.Add(time.Hour)); found {
		t.Fatal("revoked exact contract remained classified")
	}

	resurrected := syntheticTerminalDocument(now)
	if err := newEarningsTerminalStore(writeTerminalImport(t, resurrected)).UseCoreStore(t.Context(), authority, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "reviewed_at precedes") {
		t.Fatalf("post-revocation resurrection error = %v", err)
	}

	retained, ok, err := authority.GetStateDocument(t.Context(), earningsAuthorityScope, earningsTerminalStateKind)
	if err != nil || !ok {
		t.Fatalf("retained revocation state: ok=%v err=%v", ok, err)
	}
	retainedCatalog, err := decodeEarningsTerminalDocument(retained.JSON, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(retainedCatalog.Tombstones) != 1 || retainedCatalog.Tombstones[0].ConID != 1001 || !retainedCatalog.Tombstones[0].RevokedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("retained tombstones = %+v", retainedCatalog.Tombstones)
	}

	bumpedWrapperOldRecord := syntheticTerminalDocument(now)
	bumpedWrapperOldRecord.ReviewedAt = now.Add(2 * time.Hour)
	if err := newEarningsTerminalStore(writeTerminalImport(t, bumpedWrapperOldRecord)).UseCoreStore(t.Context(), authority, now.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "not after retained revocation watermark") {
		t.Fatalf("bumped-wrapper old-record resurrection error = %v", err)
	}
	atRevocationWatermark := syntheticTerminalDocument(now.Add(time.Hour))
	atRevocationWatermark.ReviewedAt = now.Add(2 * time.Hour)
	if err := newEarningsTerminalStore(writeTerminalImport(t, atRevocationWatermark)).UseCoreStore(t.Context(), authority, now.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "not after retained revocation watermark") {
		t.Fatalf("equal-watermark reactivation error = %v", err)
	}

	reviewedAgain := syntheticTerminalDocument(now.Add(3 * time.Hour))
	reactivated := newEarningsTerminalStore(writeTerminalImport(t, reviewedAgain))
	if err := reactivated.UseCoreStore(t.Context(), authority, now.Add(3*time.Hour)); err != nil {
		t.Fatalf("legitimate re-review reactivation: %v", err)
	}
	match, found := reactivated.terminalEarningsFor(risk.NameInput{Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK"}, now.Add(3*time.Hour))
	if !found || match.Status != rpc.EarningsStatusTerminalNonReporting {
		t.Fatalf("reactivated classification = %+v found=%v", match, found)
	}
	reactivatedState, ok, err := authority.GetStateDocument(t.Context(), earningsAuthorityScope, earningsTerminalStateKind)
	if err != nil || !ok {
		t.Fatalf("reactivated state: ok=%v err=%v", ok, err)
	}
	reactivatedCatalog, err := decodeEarningsTerminalDocument(reactivatedState.JSON, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(reactivatedCatalog.Tombstones) != 1 || !reactivatedCatalog.Tombstones[0].RevokedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("reactivation erased revocation watermark: %+v", reactivatedCatalog.Tombstones)
	}
}

func TestEarningsTerminalAuthorityChangeObservationsAreAtomicAndTyped(t *testing.T) {
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)

	first := syntheticTerminalDocument(base)
	if err := newEarningsTerminalStore(writeTerminalImport(t, first)).UseCoreStore(t.Context(), authority, base); err != nil {
		t.Fatal(err)
	}
	updated := syntheticTerminalDocument(base.Add(time.Hour))
	updated.Contracts[0].Issuer = "Acme Example Holdings, Inc."
	if err := newEarningsTerminalStore(writeTerminalImport(t, updated)).UseCoreStore(t.Context(), authority, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	revoked := earningsTerminalDocument{
		Version: earningsTerminalDocumentVersion, ReviewedAt: base.Add(2 * time.Hour),
		Contracts: []earningsTerminalRecord{},
	}
	if err := newEarningsTerminalStore(writeTerminalImport(t, revoked)).UseCoreStore(t.Context(), authority, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	reviewedAgain := syntheticTerminalDocument(base.Add(3 * time.Hour))
	if err := newEarningsTerminalStore(writeTerminalImport(t, reviewedAgain)).UseCoreStore(t.Context(), authority, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	observations, err := authority.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsTerminalObservationSource,
		Kind: earningsTerminalObservationKind, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 4 {
		t.Fatalf("change observations = %d, want 4", len(observations))
	}
	wantActions := []string{
		earningsTerminalChangeImport,
		earningsTerminalChangeUpdate,
		earningsTerminalChangeRevoke,
		earningsTerminalChangeUpdate,
	}
	changes := make([]earningsTerminalAuthorityChange, len(observations))
	for i, observation := range observations {
		if !observation.DecisionEligible || observation.ContentType != "application/json" {
			t.Fatalf("observation[%d] contract = eligible:%v content_type:%q", i, observation.DecisionEligible, observation.ContentType)
		}
		if strings.Contains(string(observation.Payload), "ACMEQ") || strings.Contains(string(observation.Payload), "Acme Example") || strings.Contains(string(observation.Payload), "https://") {
			t.Fatalf("observation[%d] leaked user prose or source content: %s", i, observation.Payload)
		}
		if err := json.Unmarshal(observation.Payload, &changes[i]); err != nil {
			t.Fatalf("decode observation[%d]: %v", i, err)
		}
		if changes[i].Action != wantActions[i] || changes[i].OldRevision != int64(i) || changes[i].NewRevision != int64(i+1) {
			t.Fatalf("observation[%d] = %+v", i, changes[i])
		}
		if len(changes[i].Contracts) != 1 || changes[i].Contracts[0].ConID != 1001 {
			t.Fatalf("observation[%d] dispositions = %+v", i, changes[i].Contracts)
		}
		if i > 0 && changes[i].OldCatalogFingerprint != changes[i-1].NewCatalogFingerprint {
			t.Fatalf("observation fingerprint chain broke at %d", i)
		}
	}
	if changes[0].Contracts[0].Disposition != earningsTerminalDispositionAdded ||
		changes[1].Contracts[0].Disposition != earningsTerminalDispositionUpdated ||
		changes[2].Contracts[0].Disposition != earningsTerminalDispositionRevoked ||
		changes[3].Contracts[0].Disposition != earningsTerminalDispositionReactivated {
		t.Fatalf("disposition sequence = %q, %q, %q, %q",
			changes[0].Contracts[0].Disposition, changes[1].Contracts[0].Disposition,
			changes[2].Contracts[0].Disposition, changes[3].Contracts[0].Disposition)
	}
	if !changes[2].Contracts[0].RevocationWatermark.Equal(base.Add(2*time.Hour)) ||
		!changes[3].Contracts[0].RevocationWatermark.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("revocation watermark was not retained across reactivation: revoke=%s reactivate=%s",
			changes[2].Contracts[0].RevocationWatermark, changes[3].Contracts[0].RevocationWatermark)
	}
	state, ok, err := authority.GetStateDocument(t.Context(), earningsAuthorityScope, earningsTerminalStateKind)
	if err != nil || !ok {
		t.Fatalf("state after observations: ok=%v err=%v", ok, err)
	}
	if state.Revision != changes[len(changes)-1].NewRevision {
		t.Fatalf("state revision = %d, final observation revision = %d", state.Revision, changes[len(changes)-1].NewRevision)
	}
	parsed, err := decodeEarningsTerminalDocument(state.JSON, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := earningsTerminalDocumentFingerprint(parsed); got != changes[len(changes)-1].NewCatalogFingerprint {
		t.Fatalf("state fingerprint = %q, final observation = %q", got, changes[len(changes)-1].NewCatalogFingerprint)
	}
}

func TestEarningsTerminalAuthorityInitializeObservation(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	if err := newEarningsTerminalStore("").UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	observations, err := authority.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsTerminalObservationSource,
		Kind: earningsTerminalObservationKind, Limit: 10,
	})
	if err != nil || len(observations) != 1 {
		t.Fatalf("initialize observations = %d err=%v", len(observations), err)
	}
	var change earningsTerminalAuthorityChange
	if err := json.Unmarshal(observations[0].Payload, &change); err != nil {
		t.Fatal(err)
	}
	if change.Action != earningsTerminalChangeInitialize || change.OldRevision != 0 || change.NewRevision != 1 || len(change.Contracts) != 0 {
		t.Fatalf("initialize change = %+v", change)
	}
}

func TestEarningsTerminalAuthorityChangeFailurePublishesNeitherHalf(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	empty := earningsTerminalDocument{
		Version:   earningsTerminalDocumentVersion,
		Contracts: []earningsTerminalRecord{}, Tombstones: []earningsTerminalTombstone{},
	}
	if _, err := commitEarningsTerminalAuthorityChange(
		t.Context(), authority, corestore.StateDocument{}, false,
		earningsTerminalDocument{}, empty, earningsTerminalChangeInitialize, nil, time.Time{},
	); err == nil || !strings.Contains(err.Error(), "observation time is required") {
		t.Fatalf("invalid coupled commit error = %v", err)
	}
	if _, ok, err := authority.GetStateDocument(t.Context(), earningsAuthorityScope, earningsTerminalStateKind); err != nil || ok {
		t.Fatalf("failed coupled commit published state: ok=%v err=%v", ok, err)
	}
	observations, err := authority.ListObservations(t.Context(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Source: earningsTerminalObservationSource,
		Kind: earningsTerminalObservationKind, Limit: 10,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("failed coupled commit published observations: count=%d err=%v", len(observations), err)
	}
}

func mustTerminalJSON(t *testing.T, doc earningsTerminalDocument) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
