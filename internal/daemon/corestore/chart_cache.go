package corestore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const chartCacheKind = "market_chart_history.current.v1"

// ChartCacheKind names chart cache documents for callers outside the store.
const ChartCacheKind = chartCacheKind

// ChartCacheMaxBytes bounds retained chart payloads independently of broker,
// accounting and decision evidence. SQLite pages, WAL and backups add overhead.
const ChartCacheMaxBytes = 512 << 20

// LoadChartCache returns a digest-verified public-market chart document.
// A cache miss is not evidence that the requested market period was empty.
func (s *Store) LoadChartCache(ctx context.Context, key string) (StateDocument, bool, error) {
	if err := validateChartKey(key); err != nil {
		return StateDocument{}, false, err
	}
	return s.GetStateDocument(ctx, "market/chart/"+key, chartCacheKind)
}

// SaveChartCache publishes one bounded current series, evicting only this
// display cache's oldest documents at its size or 750-series limit. It never
// appends market ticks to the immutable evidence ledger. The caller serializes
// read/merge/write and validates exact instrument and source-time semantics.
func (s *Store) SaveChartCache(ctx context.Context, key string, payload []byte) (StateDocument, error) {
	if err := validateChartKey(key); err != nil {
		return StateDocument{}, err
	}
	if len(payload) > 2<<20 || !json.Valid(payload) {
		return StateDocument{}, fmt.Errorf("invalid or oversized chart cache document")
	}
	var saved StateDocument
	err := s.criticalMutation(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		scope := "market/chart/" + key
		var revision int64
		err := tx.QueryRowContext(ctx, "SELECT revision FROM state_documents WHERE scope_key=? AND kind=?", scope, chartCacheKind).Scan(&revision)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		saved, err = compareAndSwapStateTx(ctx, tx, StateDocumentCAS{ScopeKey: scope, Kind: chartCacheKind, ExpectedRevision: revision, JSON: payload}, now)
		if err != nil {
			return err
		}
		var count, size int64
		if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(length(document_json)),0) FROM state_documents WHERE kind=?", chartCacheKind).Scan(&count, &size); err != nil {
			return err
		}
		for count > 750 || size > ChartCacheMaxBytes {
			var oldest string
			var bytes int64
			if err = tx.QueryRowContext(ctx, "SELECT scope_key,length(document_json) FROM state_documents WHERE kind=? AND scope_key<>? ORDER BY updated_at,scope_key LIMIT 1", chartCacheKind, scope).Scan(&oldest, &bytes); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM state_documents WHERE kind=? AND scope_key=?", chartCacheKind, oldest); err != nil {
				return err
			}
			count--
			size -= bytes
		}
		_, err = advanceHeadTx(ctx, tx, 0, now)
		return err
	})
	return saved, err
}

func validateChartKey(key string) error {
	b, err := hex.DecodeString(key)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("invalid chart cache identity")
	}
	return nil
}
