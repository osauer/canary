package alerts

import (
	"errors"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
)

// RecordPushAck stores a displayed or opened receipt the authenticated paired
// device reported for a journaled notice, then lets the proof relay report
// it. It does not take the dispatcher lock: a receipt never waits behind a
// transport call.
func (d *Dispatcher) RecordPushAck(noticeID, deviceID, event string, deviceAt time.Time) (state.PushAckOutcome, error) {
	if d.Store == nil {
		return state.PushAckOutcome{}, errors.New("alert delivery store unavailable")
	}
	outcome, err := d.Store.RecordPushAck(noticeID, deviceID, event, deviceAt, d.now())
	if err != nil {
		return outcome, err
	}
	if outcome.Recorded {
		d.notifyJournal()
	}
	return outcome, nil
}
