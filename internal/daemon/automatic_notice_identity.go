package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// automaticRecordNoticeID is the identity of the owner's veto opportunity,
// not of a refresh, a proposed order revision or a deferred send time.
func automaticRecordNoticeID(r automaticSubmissionRecord) string {
	if !r.waiting() {
		return ""
	}
	return automaticRecordWindowID(r)
}

// automaticRecordWindowID also identifies a completed veto, so an exact retry
// can acknowledge it without exposing a terminal record as a pending notice.
func automaticRecordWindowID(r automaticSubmissionRecord) string {
	if r.CreatedAt.IsZero() {
		return ""
	}
	raw, _ := json.Marshal(struct {
		Account, Mode, Key, Episode string
		Start                       time.Time
		Quantity                    int
	}{r.AccountID, r.AccountMode, r.Key, r.Episode, r.CreatedAt.UTC(), r.clockQuantity()})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
