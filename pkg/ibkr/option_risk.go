package ibkr

import (
	"strconv"
	"strings"
	"time"
)

// OptionRiskMeasurement is one atomic model-computation receipt from a
// non-sharing exact-contract subscription. Nil components are unavailable;
// a valid zero delta is observed. Prices and shared Greeks cannot freshen it.
type OptionRiskMeasurement struct {
	Contract     Contract
	RequestID    int
	SessionEpoch uint64
	RequestedAt  time.Time
	ReceivedAt   time.Time
	DataType     int
	IV           *float64
	Delta        *float64
	Underlying   *float64
}

// Epoch reports the physical socket generation for receipt comparison. It
// cannot create a binding or establish that a session is still current.
func (b ConnectorSessionBinding) Epoch() uint64 { return b.epoch }

func (c *Connector) handleExactOptionRisk(origin ConnectorSessionBinding, fields []string) {
	if len(fields) < 12 {
		return
	}
	id, err := strconv.Atoi(fields[1])
	if err != nil {
		return
	}
	tick, err := strconv.Atoi(fields[2])
	if err != nil || (tick != 13 && tick != 83) {
		return
	}
	// The reader holds the publication read side (rlockPublication).
	if !c.SessionCurrent(origin) {
		return
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()
	sub := c.subscriptions[c.reqIDMap[id]]
	if sub == nil || sub.ReqID != id || sub.exactSession != origin || sub.exactContract.ConID <= 0 || sub.exactContract.SecType != "OPT" {
		return
	}
	r := &OptionRiskMeasurement{Contract: sub.exactContract, RequestID: id, SessionEpoch: origin.epoch,
		RequestedAt: sub.LastTime, ReceivedAt: time.Now().UTC(), DataType: origin.connection.MarketDataType(id)}
	// A delayed tick is delayed even if a preceding notice said live. A
	// missing notice is unknown, and frozen computation remains frozen.
	if tick == 83 {
		r.DataType = 3
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64); err == nil && v > 0 && v < 100 {
		r.IV = &v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(fields[5]), 64); err == nil && saneGreek(v, 1.05) {
		r.Delta = &v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(fields[11]), 64); err == nil && v > 0 && v < 1e9 {
		r.Underlying = &v
	}
	// Replace the entire receipt, including missing values. Never assemble
	// components from different computations.
	sub.optionRisk = r
}

// OptionRiskForSession returns a caller-owned exact model receipt only while
// its subscription and physical connection remain current. It does no I/O.
func (c *Connector) OptionRiskForSession(binding ConnectorSessionBinding, key string) *OptionRiskMeasurement {
	if c == nil {
		return nil
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	if !c.SessionCurrent(binding) {
		return nil
	}
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	sub := c.subscriptions[key]
	if sub == nil || sub.exactSession != binding || sub.optionRisk == nil {
		return nil
	}
	out := *sub.optionRisk
	if out.IV != nil {
		out.IV = new(*out.IV)
	}
	if out.Delta != nil {
		out.Delta = new(*out.Delta)
	}
	if out.Underlying != nil {
		out.Underlying = new(*out.Underlying)
	}
	// A later non-live notice invalidates the receipt; a later live notice
	// cannot upgrade a computation captured as delayed/frozen/unknown.
	if current := binding.connection.MarketDataType(sub.ReqID); current != 1 {
		out.DataType = current
	}
	return &out
}
