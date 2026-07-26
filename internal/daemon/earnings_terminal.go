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
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	earningsTerminalDocumentVersion   = 1
	earningsTerminalStateKind         = "earnings_terminal_evidence.current.v1"
	earningsTerminalObservationKind   = "earnings_terminal_evidence.change.v1"
	earningsTerminalObservationSource = "operator.terminal_evidence"
	earningsTerminalMaxFileBytes      = 1 << 20

	earningsTerminalReasonIdentityConflict = "terminal_identity_conflict"
	earningsTerminalReasonEvidenceExpired  = "terminal_evidence_expired"
	earningsTerminalReasonSourceConflict   = "terminal_evidence_conflict"

	earningsTerminalClassEquityCancelled = "equity_interests_cancelled"
	earningsTerminalClassIssuerDissolved = "issuer_dissolved"

	earningsTerminalChangeInitialize = "initialize"
	earningsTerminalChangeImport     = "import"
	earningsTerminalChangeUpdate     = "update"
	earningsTerminalChangeRevoke     = "revoke"

	earningsTerminalDispositionAdded       = "added"
	earningsTerminalDispositionUpdated     = "updated"
	earningsTerminalDispositionRevoked     = "revoked"
	earningsTerminalDispositionReactivated = "reactivated"
)

type earningsTerminalDocument struct {
	Version    int                         `json:"version"`
	ReviewedAt time.Time                   `json:"reviewed_at,omitzero"`
	Contracts  []earningsTerminalRecord    `json:"contracts"`
	Tombstones []earningsTerminalTombstone `json:"tombstones,omitempty"`
}

// earningsTerminalImportDocument is the closed operator-file schema. The
// daemon derives tombstones while committing an import; an operator file
// cannot author or erase those retained revocation watermarks.
type earningsTerminalImportDocument struct {
	Version    int                      `json:"version"`
	ReviewedAt time.Time                `json:"reviewed_at"`
	Contracts  []earningsTerminalRecord `json:"contracts"`
}

type earningsTerminalRecord struct {
	Contract        earningsTerminalContract    `json:"contract"`
	Issuer          string                      `json:"issuer"`
	CIK             string                      `json:"cik,omitempty"`
	Classification  string                      `json:"classification"`
	EffectiveDate   string                      `json:"effective_date"`
	VerifiedAt      time.Time                   `json:"verified_at"`
	RevalidateAfter time.Time                   `json:"revalidate_after"`
	Evidence        []earningsTerminalReference `json:"evidence"`
}

type earningsTerminalContract struct {
	ConID   int    `json:"con_id"`
	Symbol  string `json:"symbol"`
	SecType string `json:"sec_type"`
}

type earningsTerminalTombstone struct {
	ConID     int       `json:"con_id"`
	RevokedAt time.Time `json:"revoked_at"`
}

// earningsTerminalReference accepts only a closed primary-source kind plus a
// validated HTTPS URL. User-authored prose never enters the served contract.
type earningsTerminalReference struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type earningsTerminalStored struct {
	record      earningsTerminalRecord
	fingerprint string
}

// earningsTerminalAuthorityChange is immutable audit evidence for one exact
// state-document revision. It contains only typed identities, dispositions,
// timestamps, and fingerprints; operator prose is never admitted.
type earningsTerminalAuthorityChange struct {
	Version               int                                   `json:"version"`
	Action                string                                `json:"action"`
	OldRevision           int64                                 `json:"old_revision"`
	NewRevision           int64                                 `json:"new_revision"`
	OldCatalogFingerprint string                                `json:"old_catalog_fingerprint"`
	NewCatalogFingerprint string                                `json:"new_catalog_fingerprint"`
	OldReviewedAt         time.Time                             `json:"old_reviewed_at"`
	NewReviewedAt         time.Time                             `json:"new_reviewed_at"`
	Contracts             []earningsTerminalContractDisposition `json:"contracts"`
}

type earningsTerminalContractDisposition struct {
	ConID                int       `json:"con_id"`
	Disposition          string    `json:"disposition"`
	OldRecordFingerprint string    `json:"old_record_fingerprint,omitempty"`
	NewRecordFingerprint string    `json:"new_record_fingerprint,omitempty"`
	RevocationWatermark  time.Time `json:"revocation_watermark,omitzero"`
}

// earningsTerminalStore owns the exact-contract terminal/non-reporting
// authority used by rules 6-8. daemon.db is the live source; an optional
// operator file is a validated startup import/update, never a runtime fallback.
type earningsTerminalStore struct {
	mu         sync.RWMutex
	importPath string
	revision   int64
	reviewedAt time.Time
	byConID    map[int]earningsTerminalStored
}

func newEarningsTerminalStore(importPath string) *earningsTerminalStore {
	return &earningsTerminalStore{importPath: strings.TrimSpace(importPath), byConID: map[int]earningsTerminalStored{}}
}

// UseCoreStore imports a configured operator document transactionally, then
// installs only the verified SQLite revision. With no import configured it
// loads the retained SQLite authority (or creates an explicit empty v1
// document). Malformed, future-version, or tampered authority fails startup.
func (s *earningsTerminalStore) UseCoreStore(ctx context.Context, store *corestore.Store, now time.Time) error {
	if s == nil || store == nil {
		return errors.New("earnings terminal authority is unavailable")
	}
	doc, ok, err := store.GetStateDocument(ctx, earningsAuthorityScope, earningsTerminalStateKind)
	if err != nil {
		return fmt.Errorf("load earnings terminal authority: %w", err)
	}
	var retained earningsTerminalDocument
	if ok {
		retained, err = decodeEarningsTerminalDocument(doc.JSON, now)
		if err != nil {
			return fmt.Errorf("decode retained earnings terminal authority: %w", err)
		}
	}
	var candidate earningsTerminalDocument
	var dispositions []earningsTerminalContractDisposition
	action := ""
	shouldCommit := false
	if s.importPath != "" {
		imported, err := readEarningsTerminalImport(expandUserPath(s.importPath), now)
		if err != nil {
			return err
		}
		if ok {
			if err := rejectEarningsTerminalRollback(retained, imported); err != nil {
				return fmt.Errorf("reject earnings terminal authority rollback: %w", err)
			}
		}
		candidate, dispositions, err = reconcileEarningsTerminalImport(retained, imported, ok)
		if err != nil {
			return fmt.Errorf("reject earnings terminal authority rollback: %w", err)
		}
		raw, err := json.Marshal(candidate)
		if err != nil {
			return fmt.Errorf("encode earnings terminal import: %w", err)
		}
		shouldCommit = !ok || !bytes.Equal(doc.JSON, raw)
		if shouldCommit {
			action = classifyEarningsTerminalChange(ok, retained, dispositions, true)
		}
	}
	if !ok {
		if s.importPath == "" {
			candidate = earningsTerminalDocument{
				Version:   earningsTerminalDocumentVersion,
				Contracts: []earningsTerminalRecord{}, Tombstones: []earningsTerminalTombstone{},
			}
			action = earningsTerminalChangeInitialize
			shouldCommit = true
		}
	}
	if shouldCommit {
		doc, err = commitEarningsTerminalAuthorityChange(ctx, store, doc, ok, retained, candidate, action, dispositions, now)
		if err != nil {
			return err
		}
		ok = true
	}
	parsed, err := decodeEarningsTerminalDocument(doc.JSON, now)
	if err != nil {
		return fmt.Errorf("decode earnings terminal authority: %w", err)
	}
	byConID := make(map[int]earningsTerminalStored, len(parsed.Contracts))
	for _, record := range parsed.Contracts {
		byConID[record.Contract.ConID] = earningsTerminalStored{
			record: record, fingerprint: earningsTerminalRecordFingerprint(record),
		}
	}
	s.mu.Lock()
	s.revision = doc.Revision
	s.reviewedAt = parsed.ReviewedAt
	s.byConID = byConID
	s.mu.Unlock()
	return nil
}

func readEarningsTerminalImport(path string, now time.Time) (earningsTerminalDocument, error) {
	f, err := os.Open(path)
	if err != nil {
		return earningsTerminalDocument{}, fmt.Errorf("read configured earnings terminal evidence: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return earningsTerminalDocument{}, fmt.Errorf("stat configured earnings terminal evidence: %w", err)
	}
	if !info.Mode().IsRegular() {
		return earningsTerminalDocument{}, errors.New("configured earnings terminal evidence is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return earningsTerminalDocument{}, errors.New("configured earnings terminal evidence must be private (mode 0600 or stricter)")
	}
	raw, err := io.ReadAll(io.LimitReader(f, earningsTerminalMaxFileBytes+1))
	if err != nil {
		return earningsTerminalDocument{}, fmt.Errorf("read configured earnings terminal evidence: %w", err)
	}
	if len(raw) > earningsTerminalMaxFileBytes {
		return earningsTerminalDocument{}, fmt.Errorf("configured earnings terminal evidence exceeds %d bytes", earningsTerminalMaxFileBytes)
	}
	var imported earningsTerminalImportDocument
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&imported); err != nil {
		return earningsTerminalDocument{}, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return earningsTerminalDocument{}, err
	}
	doc := earningsTerminalDocument{
		Version: imported.Version, ReviewedAt: imported.ReviewedAt,
		Contracts: imported.Contracts, Tombstones: []earningsTerminalTombstone{},
	}
	normalized, err := json.Marshal(doc)
	if err != nil {
		return earningsTerminalDocument{}, fmt.Errorf("normalize configured earnings terminal evidence: %w", err)
	}
	doc, err = decodeEarningsTerminalDocument(normalized, now)
	if err != nil {
		return earningsTerminalDocument{}, err
	}
	if doc.ReviewedAt.IsZero() {
		return earningsTerminalDocument{}, errors.New("reviewed_at is required for configured imports")
	}
	return doc, nil
}

func commitEarningsTerminalAuthorityChange(
	ctx context.Context,
	store *corestore.Store,
	oldDoc corestore.StateDocument,
	hadOld bool,
	oldCatalog, newCatalog earningsTerminalDocument,
	action string,
	dispositions []earningsTerminalContractDisposition,
	now time.Time,
) (corestore.StateDocument, error) {
	newRaw, err := json.Marshal(newCatalog)
	if err != nil {
		return corestore.StateDocument{}, fmt.Errorf("encode earnings terminal authority: %w", err)
	}
	expectedRevision := int64(0)
	oldFingerprint := ""
	oldReviewedAt := time.Time{}
	if hadOld {
		expectedRevision = oldDoc.Revision
		oldFingerprint = earningsTerminalDocumentFingerprint(oldCatalog)
		oldReviewedAt = oldCatalog.ReviewedAt
	}
	contractChanges := make([]earningsTerminalContractDisposition, len(dispositions))
	copy(contractChanges, dispositions)
	change := earningsTerminalAuthorityChange{
		Version: 1, Action: action,
		OldRevision: expectedRevision, NewRevision: expectedRevision + 1,
		OldCatalogFingerprint: oldFingerprint,
		NewCatalogFingerprint: earningsTerminalDocumentFingerprint(newCatalog),
		OldReviewedAt:         oldReviewedAt, NewReviewedAt: newCatalog.ReviewedAt,
		Contracts: contractChanges,
	}
	changeRaw, err := json.Marshal(change)
	if err != nil {
		return corestore.StateDocument{}, fmt.Errorf("encode earnings terminal authority observation: %w", err)
	}
	saved, _, err := store.CompareAndSwapStateDocumentWithObservations(ctx, corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsTerminalStateKind,
		ExpectedRevision: expectedRevision, JSON: newRaw,
	}, []corestore.ObservationInput{{
		ScopeKey: earningsAuthorityScope, Source: earningsTerminalObservationSource,
		Kind: earningsTerminalObservationKind, ObservedAt: now.UTC(),
		ContentType: "application/json", Payload: changeRaw, DecisionEligible: true,
	}})
	if err != nil {
		return corestore.StateDocument{}, fmt.Errorf("publish earnings terminal authority: %w", err)
	}
	return saved, nil
}

func reconcileEarningsTerminalImport(retained, imported earningsTerminalDocument, hadRetained bool) (earningsTerminalDocument, []earningsTerminalContractDisposition, error) {
	candidate := imported
	candidate.Tombstones = append([]earningsTerminalTombstone(nil), retained.Tombstones...)
	oldByConID := make(map[int]earningsTerminalRecord, len(retained.Contracts))
	for _, record := range retained.Contracts {
		oldByConID[record.Contract.ConID] = record
	}
	newByConID := make(map[int]earningsTerminalRecord, len(imported.Contracts))
	for _, record := range imported.Contracts {
		newByConID[record.Contract.ConID] = record
	}
	tombstones := make(map[int]earningsTerminalTombstone, len(retained.Tombstones))
	for _, tombstone := range retained.Tombstones {
		tombstones[tombstone.ConID] = tombstone
	}

	dispositions := make([]earningsTerminalContractDisposition, 0)
	if hadRetained {
		for conID, old := range oldByConID {
			if _, remains := newByConID[conID]; remains {
				continue
			}
			tombstone := earningsTerminalTombstone{ConID: conID, RevokedAt: imported.ReviewedAt}
			if prior, exists := tombstones[conID]; exists && prior.RevokedAt.After(tombstone.RevokedAt) {
				tombstone = prior
			}
			tombstones[conID] = tombstone
			dispositions = append(dispositions, earningsTerminalContractDisposition{
				ConID: conID, Disposition: earningsTerminalDispositionRevoked,
				OldRecordFingerprint: earningsTerminalRecordFingerprint(old),
				RevocationWatermark:  tombstone.RevokedAt,
			})
		}
	}

	for conID, next := range newByConID {
		old, wasActive := oldByConID[conID]
		tombstone, wasRevoked := tombstones[conID]
		if wasRevoked && !next.VerifiedAt.After(tombstone.RevokedAt) {
			return earningsTerminalDocument{}, nil, fmt.Errorf("con_id %d verified_at is not after retained revocation watermark", conID)
		}
		nextFingerprint := earningsTerminalRecordFingerprint(next)
		switch {
		case wasActive:
			oldFingerprint := earningsTerminalRecordFingerprint(old)
			if nextFingerprint != oldFingerprint {
				dispositions = append(dispositions, earningsTerminalContractDisposition{
					ConID: conID, Disposition: earningsTerminalDispositionUpdated,
					OldRecordFingerprint: oldFingerprint, NewRecordFingerprint: nextFingerprint,
					RevocationWatermark: tombstone.RevokedAt,
				})
			}
		case wasRevoked:
			dispositions = append(dispositions, earningsTerminalContractDisposition{
				ConID: conID, Disposition: earningsTerminalDispositionReactivated,
				NewRecordFingerprint: nextFingerprint, RevocationWatermark: tombstone.RevokedAt,
			})
		default:
			dispositions = append(dispositions, earningsTerminalContractDisposition{
				ConID: conID, Disposition: earningsTerminalDispositionAdded,
				NewRecordFingerprint: nextFingerprint,
			})
		}
	}

	candidate.Tombstones = candidate.Tombstones[:0]
	for _, tombstone := range tombstones {
		candidate.Tombstones = append(candidate.Tombstones, tombstone)
	}
	sort.Slice(candidate.Tombstones, func(i, j int) bool { return candidate.Tombstones[i].ConID < candidate.Tombstones[j].ConID })
	sort.Slice(dispositions, func(i, j int) bool { return dispositions[i].ConID < dispositions[j].ConID })
	return candidate, dispositions, nil
}

func classifyEarningsTerminalChange(hadRetained bool, retained earningsTerminalDocument, dispositions []earningsTerminalContractDisposition, configuredImport bool) string {
	if !hadRetained {
		if configuredImport {
			return earningsTerminalChangeImport
		}
		return earningsTerminalChangeInitialize
	}
	for _, disposition := range dispositions {
		if disposition.Disposition == earningsTerminalDispositionRevoked {
			return earningsTerminalChangeRevoke
		}
	}
	if configuredImport && retained.ReviewedAt.IsZero() && len(retained.Contracts) == 0 && len(retained.Tombstones) == 0 {
		return earningsTerminalChangeImport
	}
	return earningsTerminalChangeUpdate
}

// rejectEarningsTerminalRollback enforces catalog- and active-record monotonic
// review before reconcileEarningsTerminalImport applies the retained per-ConID
// revocation watermarks. Tombstones are daemon-owned and therefore excluded
// from the same-import comparison.
func rejectEarningsTerminalRollback(retained, imported earningsTerminalDocument) error {
	switch {
	case imported.ReviewedAt.Before(retained.ReviewedAt):
		return errors.New("reviewed_at precedes retained catalog review")
	case imported.ReviewedAt.Equal(retained.ReviewedAt) && earningsTerminalActiveCatalogFingerprint(imported) != earningsTerminalActiveCatalogFingerprint(retained):
		return errors.New("catalog changed without a newer reviewed_at")
	}
	oldByConID := make(map[int]earningsTerminalRecord, len(retained.Contracts))
	for _, record := range retained.Contracts {
		oldByConID[record.Contract.ConID] = record
	}
	for _, next := range imported.Contracts {
		old, exists := oldByConID[next.Contract.ConID]
		if !exists {
			continue
		}
		switch {
		case next.VerifiedAt.Before(old.VerifiedAt):
			return fmt.Errorf("con_id %d verified_at precedes retained review", next.Contract.ConID)
		case next.VerifiedAt.Equal(old.VerifiedAt) && earningsTerminalRecordFingerprint(next) != earningsTerminalRecordFingerprint(old):
			return fmt.Errorf("con_id %d changed without a newer verified_at", next.Contract.ConID)
		}
	}
	return nil
}

func decodeEarningsTerminalDocument(raw []byte, now time.Time) (earningsTerminalDocument, error) {
	var doc earningsTerminalDocument
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return earningsTerminalDocument{}, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return earningsTerminalDocument{}, err
	}
	if doc.Version != earningsTerminalDocumentVersion {
		return earningsTerminalDocument{}, fmt.Errorf("unsupported version %d", doc.Version)
	}
	if doc.Contracts == nil {
		doc.Contracts = []earningsTerminalRecord{}
	}
	if doc.Tombstones == nil {
		doc.Tombstones = []earningsTerminalTombstone{}
	}
	doc.ReviewedAt = doc.ReviewedAt.UTC()
	if doc.ReviewedAt.After(now.UTC()) {
		return earningsTerminalDocument{}, errors.New("reviewed_at is in the future")
	}
	if doc.ReviewedAt.IsZero() && (len(doc.Contracts) != 0 || len(doc.Tombstones) != 0) {
		return earningsTerminalDocument{}, errors.New("reviewed_at is required when contracts or tombstones are present")
	}
	activeVerifiedAt := make(map[int]time.Time, len(doc.Contracts))
	for i := range doc.Contracts {
		if err := normalizeAndValidateEarningsTerminalRecord(&doc.Contracts[i], doc.ReviewedAt, now); err != nil {
			return earningsTerminalDocument{}, fmt.Errorf("contracts[%d]: %w", i, err)
		}
		conID := doc.Contracts[i].Contract.ConID
		if _, duplicate := activeVerifiedAt[conID]; duplicate {
			return earningsTerminalDocument{}, fmt.Errorf("contracts[%d]: duplicate con_id", i)
		}
		activeVerifiedAt[conID] = doc.Contracts[i].VerifiedAt
	}
	sort.Slice(doc.Contracts, func(i, j int) bool { return doc.Contracts[i].Contract.ConID < doc.Contracts[j].Contract.ConID })
	seenTombstones := make(map[int]struct{}, len(doc.Tombstones))
	for i := range doc.Tombstones {
		tombstone := &doc.Tombstones[i]
		if tombstone.ConID <= 0 {
			return earningsTerminalDocument{}, fmt.Errorf("tombstones[%d]: con_id must be positive", i)
		}
		tombstone.RevokedAt = tombstone.RevokedAt.UTC()
		if tombstone.RevokedAt.IsZero() || tombstone.RevokedAt.After(now.UTC()) {
			return earningsTerminalDocument{}, fmt.Errorf("tombstones[%d]: revoked_at is missing or in the future", i)
		}
		if tombstone.RevokedAt.After(doc.ReviewedAt) {
			return earningsTerminalDocument{}, fmt.Errorf("tombstones[%d]: revoked_at is after document reviewed_at", i)
		}
		if _, duplicate := seenTombstones[tombstone.ConID]; duplicate {
			return earningsTerminalDocument{}, fmt.Errorf("tombstones[%d]: duplicate con_id", i)
		}
		seenTombstones[tombstone.ConID] = struct{}{}
		if verifiedAt, active := activeVerifiedAt[tombstone.ConID]; active && !verifiedAt.After(tombstone.RevokedAt) {
			return earningsTerminalDocument{}, fmt.Errorf("tombstones[%d]: active record verified_at is not after revocation watermark", i)
		}
	}
	sort.Slice(doc.Tombstones, func(i, j int) bool { return doc.Tombstones[i].ConID < doc.Tombstones[j].ConID })
	return doc, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	err := dec.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return err
}

func normalizeAndValidateEarningsTerminalRecord(record *earningsTerminalRecord, reviewedAt, now time.Time) error {
	if record.Contract.ConID <= 0 {
		return errors.New("contract.con_id must be positive")
	}
	record.Contract.Symbol = strings.ToUpper(strings.TrimSpace(record.Contract.Symbol))
	if !validTerminalSymbol(record.Contract.Symbol) {
		return errors.New("contract.symbol is invalid")
	}
	record.Contract.SecType = strings.ToUpper(strings.TrimSpace(record.Contract.SecType))
	if !isStockSecurityType(record.Contract.SecType) {
		return errors.New("contract.sec_type must be STK or STOCK")
	}
	record.Contract.SecType = "STK"
	record.Issuer = strings.TrimSpace(record.Issuer)
	if !validTerminalIssuer(record.Issuer) {
		return errors.New("issuer is invalid")
	}
	record.CIK = strings.TrimSpace(record.CIK)
	if record.CIK != "" && (len(record.CIK) != 10 || strings.Trim(record.CIK, "0123456789") != "" || record.CIK == "0000000000") {
		return errors.New("cik must be ten digits and not all zero")
	}
	record.Classification = strings.TrimSpace(record.Classification)
	if !slices.Contains([]string{earningsTerminalClassEquityCancelled, earningsTerminalClassIssuerDissolved}, record.Classification) {
		return errors.New("classification is not supported")
	}
	effective, err := time.Parse(time.DateOnly, strings.TrimSpace(record.EffectiveDate))
	if err != nil {
		return errors.New("effective_date must be YYYY-MM-DD")
	}
	record.EffectiveDate = effective.Format(time.DateOnly)
	record.VerifiedAt = record.VerifiedAt.UTC()
	record.RevalidateAfter = record.RevalidateAfter.UTC()
	if record.VerifiedAt.IsZero() || record.VerifiedAt.After(now.UTC()) {
		return errors.New("verified_at is missing or in the future")
	}
	if record.VerifiedAt.After(reviewedAt) {
		return errors.New("verified_at is after document reviewed_at")
	}
	if effective.After(record.VerifiedAt) {
		return errors.New("effective_date is after verified_at")
	}
	if !record.RevalidateAfter.After(record.VerifiedAt) {
		return errors.New("revalidate_after must be after verified_at")
	}
	if record.RevalidateAfter.Sub(record.VerifiedAt) > 366*24*time.Hour {
		return errors.New("revalidate_after must be within 366 days of verified_at")
	}
	if len(record.Evidence) < 2 {
		return errors.New("at least two independent primary-source references are required")
	}
	seenAuthorities, seenURLs := map[string]struct{}{}, map[string]struct{}{}
	for i := range record.Evidence {
		reference := &record.Evidence[i]
		reference.Kind = strings.TrimSpace(reference.Kind)
		reference.URL = strings.TrimSpace(reference.URL)
		authority, _, err := earningsTerminalReferenceMetadata(*reference)
		if err != nil {
			return fmt.Errorf("evidence[%d]: %w", i, err)
		}
		if _, duplicate := seenURLs[reference.URL]; duplicate {
			return fmt.Errorf("evidence[%d]: duplicate URL", i)
		}
		seenAuthorities[authority] = struct{}{}
		seenURLs[reference.URL] = struct{}{}
		if reference.Kind == "sec_filing" {
			if record.CIK == "" {
				return fmt.Errorf("evidence[%d]: SEC filing requires cik", i)
			}
			if !earningsTerminalSECReferenceMatchesCIK(reference.URL, record.CIK) {
				return fmt.Errorf("evidence[%d]: SEC filing path does not match cik", i)
			}
		}
	}
	if len(seenAuthorities) < 2 {
		return errors.New("evidence must include two independent authorities")
	}
	sort.Slice(record.Evidence, func(i, j int) bool {
		if record.Evidence[i].Kind == record.Evidence[j].Kind {
			return record.Evidence[i].URL < record.Evidence[j].URL
		}
		return record.Evidence[i].Kind < record.Evidence[j].Kind
	})
	return nil
}

func validTerminalSymbol(symbol string) bool {
	if len(symbol) == 0 || len(symbol) > 16 {
		return false
	}
	for _, r := range symbol {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(" .-", r) {
			continue
		}
		return false
	}
	return true
}

func validTerminalIssuer(issuer string) bool {
	if len(issuer) == 0 || len(issuer) > 160 {
		return false
	}
	for _, r := range issuer {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune(".,&'\u2019()-", r) {
			continue
		}
		return false
	}
	return true
}

func earningsTerminalReferenceMetadata(reference earningsTerminalReference) (authority, document string, err error) {
	u, err := url.Parse(reference.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("URL must be an uncredentialed HTTPS document URL without query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	switch reference.Kind {
	case "finra_uniform_practice_advisory":
		if host != "www.finra.org" || !strings.HasPrefix(u.EscapedPath(), "/sites/default/files/") {
			return "", "", errors.New("FINRA advisory URL is outside the allowlisted document path")
		}
		return "FINRA", "Uniform Practice Advisory", nil
	case "sec_filing":
		if host != "www.sec.gov" || !strings.HasPrefix(u.EscapedPath(), "/Archives/edgar/data/") {
			return "", "", errors.New("SEC filing URL is outside the allowlisted EDGAR path")
		}
		return "SEC", "EDGAR filing", nil
	case "nasdaq_delisting_notice":
		if host != "www.nasdaq.com" || !strings.HasPrefix(u.EscapedPath(), "/press-release/") {
			return "", "", errors.New("nasdaq notice URL is outside the allowlisted press-release path")
		}
		return "Nasdaq", "Delisting notice", nil
	default:
		return "", "", errors.New("evidence kind is not supported")
	}
}

func earningsTerminalSECReferenceMatchesCIK(referenceURL, cik string) bool {
	u, err := url.Parse(referenceURL)
	if err != nil {
		return false
	}
	remainder := strings.TrimPrefix(u.Path, "/Archives/edgar/data/")
	pathCIK, _, found := strings.Cut(remainder, "/")
	if !found || pathCIK == "" || strings.Trim(pathCIK, "0123456789") != "" {
		return false
	}
	normalize := func(value string) string {
		value = strings.TrimLeft(value, "0")
		if value == "" {
			return "0"
		}
		return value
	}
	return normalize(pathCIK) == normalize(cik)
}

type terminalEarningsMatch struct {
	Info   rpc.EarningsTerminalInfo
	Status string
	Reason string
}

// terminalEarningsFor returns found=true only when the exact held stock ConID
// exists in SQLite authority. A changed symbol/type for that ConID is an
// explicit conflict. A same-symbol holding with another ConID follows normal
// provider resolution; ticker text alone never grants the exception.
func (s *earningsTerminalStore) terminalEarningsFor(name risk.NameInput, now time.Time) (match terminalEarningsMatch, found bool) {
	// Position groups do not carry an option leg's exact underlying ConID.
	// Therefore an exact stock classification may exempt a stock-only holding,
	// but never option legs that merely share its ticker.
	if s == nil || name.StockConID <= 0 || len(name.Legs) != 0 {
		return terminalEarningsMatch{}, false
	}
	s.mu.RLock()
	stored, found := s.byConID[name.StockConID]
	revision := s.revision
	reviewedAt := s.reviewedAt
	s.mu.RUnlock()
	if !found {
		return terminalEarningsMatch{}, false
	}
	record := stored.record
	match.Info = rpc.EarningsTerminalInfo{
		ContractConID:        record.Contract.ConID,
		Issuer:               record.Issuer,
		CIK:                  record.CIK,
		Classification:       record.Classification,
		EffectiveDate:        record.EffectiveDate,
		VerifiedAt:           record.VerifiedAt,
		RevalidateAfter:      record.RevalidateAfter,
		AuthorityRevision:    revision,
		AuthorityReviewedAt:  reviewedAt,
		AuthorityFingerprint: stored.fingerprint,
		Evidence:             projectEarningsTerminalReferences(record.Evidence),
	}
	match.Info.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(record.Contract.Symbol, match.Info)
	if !strings.EqualFold(strings.TrimSpace(name.Symbol), record.Contract.Symbol) || !sameStockSecurityType(name.StockSecType, record.Contract.SecType) {
		match.Status = rpc.EarningsStatusConflictingSources
		match.Reason = earningsTerminalReasonIdentityConflict
		return match, true
	}
	if !now.Before(record.RevalidateAfter) {
		match.Status = rpc.EarningsStatusTerminalEvidenceExpired
		match.Reason = earningsTerminalReasonEvidenceExpired
		return match, true
	}
	match.Status = rpc.EarningsStatusTerminalNonReporting
	match.Reason = record.Classification
	return match, true
}

// analysisPositions returns a derived portfolio for advisory analysis. It
// removes only a currently verified, exact cancelled/dissolved stock contract.
// The broker position snapshot remains the account/reconciliation and order
// authority; callers that resolve or act on an individual position must use it.
func (s *Server) analysisPositions(pos *rpc.PositionsResult, now time.Time) *rpc.PositionsResult {
	if s == nil || s.earningsTerminal == nil || pos == nil {
		return pos
	}
	stocks := make([]rpc.PositionView, 0, len(pos.Stocks))
	changed := false
	for _, stock := range pos.Stocks {
		match, found := s.earningsTerminal.terminalEarningsFor(risk.NameInput{
			Symbol:       stock.Symbol,
			StockConID:   stock.ConID,
			StockSecType: stock.SecType,
		}, now)
		if found && match.Status == rpc.EarningsStatusTerminalNonReporting {
			changed = true
			continue
		}
		stocks = append(stocks, stock)
	}
	if !changed {
		return pos
	}
	out := *pos
	out.Stocks = stocks
	baseCcy := ""
	var netLiquidationBase *float64
	if pos.Portfolio != nil {
		baseCcy = pos.Portfolio.BaseCurrency
		netLiquidationBase = pos.Portfolio.NetLiquidationBase
	}
	out.ByUnderlying = groupByUnderlying(out.Stocks, out.Options, baseCcy, netLiquidationBase)
	out.Portfolio = buildPortfolioAggregatesWithBase(out.Stocks, out.Options, baseCcy)
	addPortfolioBaseContext(out.Portfolio, out.ByUnderlying, baseCcy, netLiquidationBase)
	if pos.Portfolio != nil {
		// This is account-currency sensitivity, not a position aggregate; it
		// comes from the currency ledger and remains valid for the analysis view.
		out.Portfolio.FXSensitivityPerPct = pos.Portfolio.FXSensitivityPerPct
		out.Portfolio.FXBaseCurrency = pos.Portfolio.FXBaseCurrency
	}
	// Coverage is reconciled against the raw broker position set and cannot be
	// copied into a derived analysis view without re-running that reconciliation.
	out.ProtectionCoverage = nil
	return &out
}

func projectEarningsTerminalReferences(in []earningsTerminalReference) []rpc.EarningsEvidenceReference {
	out := make([]rpc.EarningsEvidenceReference, 0, len(in))
	for _, reference := range in {
		authority, document, err := earningsTerminalReferenceMetadata(reference)
		if err != nil {
			continue // impossible after authority validation; fail closed at load
		}
		out = append(out, rpc.EarningsEvidenceReference{Authority: authority, Document: document, URL: reference.URL})
	}
	return out
}

func earningsTerminalRecordFingerprint(record earningsTerminalRecord) string {
	raw, _ := json.Marshal(record)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func earningsTerminalDocumentFingerprint(doc earningsTerminalDocument) string {
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func earningsTerminalActiveCatalogFingerprint(doc earningsTerminalDocument) string {
	return earningsTerminalDocumentFingerprint(earningsTerminalDocument{
		Version: doc.Version, ReviewedAt: doc.ReviewedAt,
		Contracts:  append([]earningsTerminalRecord(nil), doc.Contracts...),
		Tombstones: []earningsTerminalTombstone{},
	})
}

func isStockSecurityType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK":
		return true
	default:
		return false
	}
}

func sameStockSecurityType(got, want string) bool {
	return isStockSecurityType(got) && isStockSecurityType(want)
}
