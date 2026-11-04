package futu

import (
	"errors"
	"fmt"
	"time"
)

// OpenD's reply result codes (retType in every reply envelope).
const (
	// RetTypeSucceed is a reply that carries its answer.
	RetTypeSucceed int32 = 0
	// RetTypeFailed is a request OpenD refused; errCode and retMsg say why.
	RetTypeFailed int32 = -1
	// RetTypeTimeOut is a request OpenD gave up on; the session is suspect.
	RetTypeTimeOut int32 = -100
	// RetTypeDisconnect is a reply OpenD could not deliver over a lost link.
	RetTypeDisconnect int32 = -200
	// RetTypeUnknown is the envelope's default before OpenD filled it.
	RetTypeUnknown int32 = -400
	// RetTypeInvalid is a request body OpenD could not decode.
	RetTypeInvalid int32 = -500
)

// Error is a reply OpenD answered with a result code other than success.
// RetType and ErrCode are OpenD's; Message is OpenD's retMsg, quoted on a
// surface and never read to make a decision.
type Error struct {
	ProtoID uint32
	RetType int32
	ErrCode int32
	Message string
}

// Error renders the code pair and the quoted message.
func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("opend protocol %d: retType %d errCode %d", e.ProtoID, e.RetType, e.ErrCode)
	}
	return fmt.Sprintf("opend protocol %d: retType %d errCode %d: %q", e.ProtoID, e.RetType, e.ErrCode, e.Message)
}

// Session reports whether the error condemns the session rather than the
// one request: a timeout, a lost link or a body OpenD could not decode. The
// caller drops the session on a session error and redials; a request error
// leaves the session in place.
func (e *Error) Session() bool {
	switch e.RetType {
	case RetTypeTimeOut, RetTypeDisconnect, RetTypeInvalid:
		return true
	}
	return false
}

// ErrFrame is the class of every malformed inbound frame: a header without
// the FT flag, a body format other than protobuf, a body shorter than its
// declared length, or a body whose SHA-1 does not match the header. The
// session is not trustworthy after one and is dropped.
var ErrFrame = errors.New("opend frame malformed")

// ErrPaced is the class of a request the client refused locally because
// OpenD's documented frequency limit for that protocol is spent in the
// current window; a [PacedError] carries the window and when it opens.
var ErrPaced = errors.New("opend request paced")

// PacedError is the refusal of one request under the pacing table.
type PacedError struct {
	Limit Limit
	// RetryAfter is how long until the oldest request in the window ages
	// out and one more may be sent.
	RetryAfter time.Duration
}

// Error names the protocol, the limit and the wait.
func (e *PacedError) Error() string {
	return fmt.Sprintf("%v: %s (%d) allows %d requests per %s; next in %s",
		ErrPaced, e.Limit.Name, e.Limit.ProtoID, e.Limit.Requests, e.Limit.Window, e.RetryAfter.Round(time.Millisecond))
}

// Unwrap makes every PacedError match [ErrPaced].
func (e *PacedError) Unwrap() error { return ErrPaced }

// ErrServerVersion is the refusal of a session whose OpenD reports a server
// version below the one this client was built against. The status row
// prints the pair.
var ErrServerVersion = errors.New("opend server version below the client's minimum")

// ErrTradingDisabled is returned by every order-writing method in the
// default build, before any frame is written.
var ErrTradingDisabled = errors.New("trading disabled: build with -tags trading to enable order placement")
