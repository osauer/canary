package live

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPublicSourceFailureOmitsRawBoundaryError(t *testing.T) {
	const privateDetail = "private daemon socket detail"
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		Errors: []SourceError{sourceErr("status", errors.New(privateDetail), now)},
		Sources: map[string]SourceMeta{
			"status": sourceUnavailable(SourceMeta{}, now),
		},
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(privateDetail)) {
		t.Fatalf("public snapshot contains raw boundary error: %s", encoded)
	}
	for _, want := range []string{publicSourceUnavailableMessage, SourceStateUnavailable, SourceReasonTransportUnavailable} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("public snapshot = %s, want allowlisted %q", encoded, want)
		}
	}
}
