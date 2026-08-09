package ibkr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// OptionExerciseRequest describes one exerciseOptions wire request. It is an
// instruction with position-changing side effects if IBKR accepts it, not a
// preview. The request does not itself carry paper-gate or application-level
// submit authority.
type OptionExerciseRequest struct {
	// TickerID correlates broker callbacks. Connector.ExerciseOptions allocates
	// one when it is zero; Connection.ExerciseOptions requires a positive value.
	TickerID int

	// Contract must be a non-nil OPT contract with symbol, expiry, positive
	// strike, and a C or P right.
	Contract         *Contract
	ExerciseAction   int    // ExerciseAction must be OptionExerciseActionExercise or OptionExerciseActionLapse.
	ExerciseQuantity int    // ExerciseQuantity is a positive number of option contracts.
	Account          string // Account is required and is trimmed before encoding.
	Override         int    // Override is the broker exercise override flag and must be 0 or 1.
	ManualOrderTime  string // ManualOrderTime is sent only when the negotiated server version supports it.
}

const (
	// OptionExerciseActionExercise asks IBKR to exercise the specified quantity.
	OptionExerciseActionExercise = 1
	// OptionExerciseActionLapse asks IBKR to let the specified quantity lapse.
	OptionExerciseActionLapse = 2
)

// ExerciseOptions sends an option exercise or lapse instruction through the
// connector's active connection. A zero TickerID is replaced with a new request
// ID. The method checks ctx only before sending and does not wait for broker
// acknowledgement; a nil error means only that the frame was accepted for the
// socket write. Once the call reaches an active Connection, the default build
// returns [ErrTradingDisabled] before validation or transmission.
func (c *Connector) ExerciseOptions(ctx context.Context, req OptionExerciseRequest) error {
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("exerciseOptions context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	return c.ExerciseOptionsForSession(ctx, binding, req)
}

// ExerciseOptionsForSession sends an option exercise or lapse instruction
// only on the exact Connector socket generation named by binding. A reconnect
// or disconnect before request-ID reservation or frame transmission is refused.
func (c *Connector) ExerciseOptionsForSession(ctx context.Context, binding ConnectorSessionBinding, req OptionExerciseRequest) error {
	return c.ExerciseOptionsForSessionGuarded(ctx, binding, req, nil)
}

// ExerciseOptionsForSessionGuarded carries a final authority guard through
// pacing to the exact exercise/lapse socket write.
func (c *Connector) ExerciseOptionsForSessionGuarded(ctx context.Context, binding ConnectorSessionBinding, req OptionExerciseRequest, guard func() error) error {
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("exerciseOptions context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	if !c.SessionCurrent(binding) {
		return definitelyUnsent(fmt.Errorf("broker session binding is not current for this Connector"))
	}
	conn := binding.connection
	if req.TickerID == 0 {
		var err error
		var epoch uint64
		req.TickerID, epoch, err = conn.reserveNextRequestIDForEpoch(binding.epoch)
		if err != nil {
			return definitelyUnsent(err)
		}
		defer conn.discardRequestIDReservation(req.TickerID)
		return conn.exerciseOptionsForEpochGuarded(ctx, req, &epoch, guard)
	}
	return conn.exerciseOptionsForEpochGuarded(ctx, req, &binding.epoch, guard)
}

// ExerciseOptions validates and sends an IBKR option exercise or lapse
// instruction. It can change a position if IBKR accepts it. A nil error means
// only that the frame was accepted for the socket write; the method does not
// wait for broker acknowledgement or finality. In the default build it returns
// [ErrTradingDisabled] before validation or transmission. The "trading" build
// tag enables this low-level wire method but does not grant submit authority.
func (c *Connection) ExerciseOptions(req OptionExerciseRequest) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	return c.exerciseOptionsForEpochGuarded(context.Background(), req, nil, nil)
}

func (c *Connection) exerciseOptionsForEpochGuarded(ctx context.Context, req OptionExerciseRequest, expectedEpoch *uint64, guard func() error) error {
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("exerciseOptions context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if err := validateOptionExerciseRequest(req); err != nil {
		return definitelyUnsent(err)
	}
	if c == nil {
		return definitelyUnsent(fmt.Errorf("no active connection"))
	}
	if !c.IsConnected() {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	var (
		epoch uint64
		err   error
	)
	if expectedEpoch != nil {
		epoch, err = c.claimRequestIDForEpoch(req.TickerID, *expectedEpoch)
	} else {
		epoch, err = c.claimRequestIDCurrentEpoch(req.TickerID)
	}
	if err != nil {
		return definitelyUnsent(err)
	}
	return c.sendExerciseOptionsFrameGuarded(ctx, req, epoch, guard)
}

func (c *Connection) sendExerciseOptionsFrame(req OptionExerciseRequest, epoch uint64) error {
	return c.sendExerciseOptionsFrameGuarded(context.Background(), req, epoch, nil)
}

func (c *Connection) sendExerciseOptionsFrameGuarded(ctx context.Context, req OptionExerciseRequest, epoch uint64, guard func() error) error {
	msg, err := c.encodeExerciseOptionsMessage(req)
	if err != nil {
		return definitelyUnsent(err)
	}
	return c.sendMessageWithTypeContextForEpochGuarded(ctx, msg, RequestTypeOrder, epoch, true, guard)
}

func validateOptionExerciseRequest(req OptionExerciseRequest) error {
	if req.Contract == nil {
		return fmt.Errorf("exerciseOptions contract is required")
	}
	if req.TickerID <= 0 {
		return fmt.Errorf("exerciseOptions ticker id must be positive")
	}
	if !strings.EqualFold(req.Contract.SecType, "OPT") {
		return fmt.Errorf("exerciseOptions supports option contracts only")
	}
	if strings.TrimSpace(req.Contract.Symbol) == "" {
		return fmt.Errorf("exerciseOptions contract symbol is required")
	}
	if strings.TrimSpace(req.Contract.Expiry) == "" {
		return fmt.Errorf("exerciseOptions contract expiry is required")
	}
	if req.Contract.Strike <= 0 {
		return fmt.Errorf("exerciseOptions contract strike is required")
	}
	right := strings.ToUpper(strings.TrimSpace(req.Contract.Right))
	if right != "C" && right != "P" {
		return fmt.Errorf("exerciseOptions contract right must be C or P")
	}
	switch req.ExerciseAction {
	case OptionExerciseActionExercise, OptionExerciseActionLapse:
	default:
		return fmt.Errorf("exerciseOptions action must be 1 (exercise) or 2 (lapse)")
	}
	if req.ExerciseQuantity <= 0 {
		return fmt.Errorf("exerciseOptions quantity must be positive")
	}
	if strings.TrimSpace(req.Account) == "" {
		return fmt.Errorf("exerciseOptions account is required")
	}
	if req.Override != 0 && req.Override != 1 {
		return fmt.Errorf("exerciseOptions override must be 0 or 1")
	}
	return nil
}

func (c *Connection) encodeExerciseOptionsMessage(req OptionExerciseRequest) ([]byte, error) {
	if err := validateOptionExerciseRequest(req); err != nil {
		return nil, err
	}
	contract := req.Contract
	multiplier := ""
	if contract.Multiplier != 0 {
		multiplier = strconv.Itoa(contract.Multiplier)
	}
	fields := []any{
		exerciseOptions,
		2, // message version used by the documented exerciseOptions request shape
		req.TickerID,
		contract.ConID,
		strings.ToUpper(strings.TrimSpace(contract.Symbol)),
		"OPT",
		strings.TrimSpace(contract.Expiry),
		strconv.FormatFloat(contract.Strike, 'f', -1, 64),
		strings.ToUpper(strings.TrimSpace(contract.Right)),
		multiplier,
		ifEmpty(contract.Exchange, "SMART"),
		ifEmpty(contract.Currency, "USD"),
		strings.TrimSpace(contract.LocalSymbol),
		strings.TrimSpace(contract.TradingClass),
		req.ExerciseAction,
		req.ExerciseQuantity,
		strings.TrimSpace(req.Account),
		req.Override,
	}
	if c.serverVersion >= minServerVerManualOrderTime {
		fields = append(fields, strings.TrimSpace(req.ManualOrderTime))
	}
	return c.encodeMsg(fields...), nil
}
