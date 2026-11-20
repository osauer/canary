package corestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RiskPolicyFingerprintSeen reports whether the journal holds a risk-policy
// event of this kind with this policy fingerprint: whether the daemon that
// owns this store ever ran on a policy with exactly these settings.
func (s *Store) RiskPolicyFingerprintSeen(ctx context.Context, kind, fingerprint string) (bool, error) {
	if kind == "" || fingerprint == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM risk_policy_events WHERE kind=? AND policy_fingerprint=? LIMIT 1`, kind, fingerprint).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read risk policy events: %w", s.latchCorruptRead(err))
	}
	return true, nil
}
