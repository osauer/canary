package futu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/pkg/futu/internal/pb/common"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/getglobalstate"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/initconnect"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/keepalive"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/notify"
	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
)

// MinServerVersion is the lowest OpenD serverVer this client accepts, in
// OpenD's own major*100+minor form. It is declared from the documentation:
// OpenD 10.10.7008 of 2026-08-13 is the newest entry on the changelog page,
// read 2026-11-04. Proof pending: the first real InitConnect.
const MinServerVersion int32 = 1010

// programmingLanguage is what the client tells OpenD it is written in, which
// OpenD uses for its statistics only.
const programmingLanguage = "Go"

// ServerVersionError is the refusal of a session below [MinServerVersion];
// it matches [ErrServerVersion] and names the pair for the status row.
type ServerVersionError struct {
	Server  int32
	Minimum int32
}

// Error names both versions.
func (e *ServerVersionError) Error() string {
	return fmt.Sprintf("%v: OpenD %d, minimum %d", ErrServerVersion, e.Server, e.Minimum)
}

// Unwrap makes every ServerVersionError match [ErrServerVersion].
func (e *ServerVersionError) Unwrap() error { return ErrServerVersion }

// ErrHandshakeRefused is the class of an InitConnect OpenD answered with a
// code other than success. The client asks for no packet encryption, so an
// OpenD configured with an RSA key refuses here; the refusal's code is
// unconfirmed until the first real contact, so no decision reads it.
var ErrHandshakeRefused = errors.New("opend refused the connection")

// HandshakeError is one refused InitConnect.
type HandshakeError struct {
	Reply *Error
}

// Error quotes OpenD's answer.
func (e *HandshakeError) Error() string {
	return fmt.Sprintf("%v: %v", ErrHandshakeRefused, e.Reply)
}

// Unwrap makes every HandshakeError match [ErrHandshakeRefused] and the
// reply's own [*Error].
func (e *HandshakeError) Unwrap() []error { return []error{ErrHandshakeRefused, e.Reply} }

// SessionEnd names why a session ended, in words the status surface can
// branch on without reading OpenD's free text.
type SessionEnd string

const (
	// EndSocket is a closed or failed socket.
	EndSocket SessionEnd = "socket_closed"
	// EndHeartbeat is an OpenD that answered nothing for two keep-alive
	// intervals.
	EndHeartbeat SessionEnd = "heartbeat_lost"
	// EndFrame is a malformed frame or reply body.
	EndFrame SessionEnd = "frame_malformed"
	// EndKickedOut is the Futu account logging in elsewhere.
	EndKickedOut SessionEnd = "kicked_out"
	// EndPasswordChanged is the Futu login password changing.
	EndPasswordChanged SessionEnd = "password_changed"
	// EndLoginBanned is Futu refusing the login.
	EndLoginBanned SessionEnd = "login_banned"
	// EndDeviceLock is the device lock turning on.
	EndDeviceLock SessionEnd = "device_lock"
	// EndForceUpdate is OpenD requiring a newer client than this one.
	EndForceUpdate SessionEnd = "force_update"
	// EndScopeLost is an account list that no longer reports the pinned
	// account with the environment the session took from it.
	EndScopeLost SessionEnd = "scope_lost"
)

// ErrSessionEnded is the class of every [SessionEndError].
var ErrSessionEnded = errors.New("opend session ended")

// SessionEndError is why a session ended. Detail is OpenD's own description
// when it gave one, quoted and never read to decide anything.
type SessionEndError struct {
	Reason SessionEnd
	Detail string
}

// Error names the reason and quotes the detail.
func (e *SessionEndError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%v: %s", ErrSessionEnded, e.Reason)
	}
	return fmt.Sprintf("%v: %s: %q", ErrSessionEnded, e.Reason, e.Detail)
}

// Unwrap makes every SessionEndError match [ErrSessionEnded].
func (e *SessionEndError) Unwrap() error { return ErrSessionEnded }

// SessionInfo is what InitConnect answered.
type SessionInfo struct {
	ServerVersion     int32
	LoginUserID       uint64
	ConnID            uint64
	KeepAliveInterval time.Duration
}

// GlobalState is OpenD's own view of itself: the program status, the two
// logins and the per-market states as OpenD's QotMarketState names.
type GlobalState struct {
	ProgramStatus string
	ProgramDetail string
	ProgramReady  bool
	QuoteLoggedIn bool
	TradeLoggedIn bool
	ServerVersion int32
	BuildNo       int32
	ConnID        uint64
	ServerTime    time.Time
	Markets       map[string]string
}

func initConnectRequest(cfg Config) *initconnect.Request {
	return &initconnect.Request{C2S: &initconnect.C2S{
		ClientVer:           new(cfg.ClientVersion),
		ClientID:            new(cfg.ClientID),
		RecvNotify:          new(true),
		PacketEncAlgo:       new(int32(common.PacketEncAlgo_PacketEncAlgo_None)),
		PushProtoFmt:        new(int32(common.ProtoFmt_ProtoFmt_Protobuf)),
		ProgrammingLanguage: new(programmingLanguage),
	}}
}

// handshake runs InitConnect and the version gate.
func handshake(ctx context.Context, conn *connection, cfg Config) (SessionInfo, error) {
	var resp initconnect.Response
	if err := conn.request(ctx, ProtoInitConnect, initConnectRequest(cfg), &resp); err != nil {
		if replyErr, ok := errors.AsType[*Error](err); ok {
			return SessionInfo{}, &HandshakeError{Reply: replyErr}
		}
		return SessionInfo{}, err
	}
	s2c := resp.GetS2C()
	if s2c == nil {
		return SessionInfo{}, fmt.Errorf("%w: InitConnect reply carries no answer", ErrFrame)
	}
	info := SessionInfo{
		ServerVersion:     s2c.GetServerVer(),
		LoginUserID:       s2c.GetLoginUserID(),
		ConnID:            s2c.GetConnID(),
		KeepAliveInterval: time.Duration(s2c.GetKeepAliveInterval()) * time.Second,
	}
	if info.ServerVersion < MinServerVersion {
		return info, &ServerVersionError{Server: info.ServerVersion, Minimum: MinServerVersion}
	}
	return info, nil
}

func keepAliveRequest(now time.Time) *keepalive.Request {
	return &keepalive.Request{C2S: &keepalive.C2S{Time: new(now.Unix())}}
}

func globalStateRequest() *getglobalstate.Request {
	return &getglobalstate.Request{C2S: &getglobalstate.C2S{UserID: new(uint64(0))}}
}

// fetchGlobalState reads OpenD's state once.
func fetchGlobalState(ctx context.Context, conn *connection) (GlobalState, error) {
	var resp getglobalstate.Response
	if err := conn.request(ctx, ProtoGetGlobalState, globalStateRequest(), &resp); err != nil {
		return GlobalState{}, err
	}
	s2c := resp.GetS2C()
	if s2c == nil {
		return GlobalState{}, fmt.Errorf("%w: GetGlobalState reply carries no answer", ErrFrame)
	}
	status := s2c.GetProgramStatus()
	state := GlobalState{
		ProgramStatus: programStatusName(status.GetType()),
		ProgramDetail: status.GetStrExtDesc(),
		ProgramReady:  status != nil && status.GetType() == common.ProgramStatusType_ProgramStatusType_Ready,
		QuoteLoggedIn: s2c.GetQotLogined(),
		TradeLoggedIn: s2c.GetTrdLogined(),
		ServerVersion: s2c.GetServerVer(),
		BuildNo:       s2c.GetServerBuildNo(),
		ConnID:        s2c.GetConnID(),
		ServerTime:    time.Unix(s2c.GetTime(), 0).UTC(),
		Markets: map[string]string{
			"HK": marketStateName(s2c.GetMarketHK()),
			"US": marketStateName(s2c.GetMarketUS()),
			"SH": marketStateName(s2c.GetMarketSH()),
			"SZ": marketStateName(s2c.GetMarketSZ()),
		},
	}
	return state, nil
}

// programStatusName is OpenD's program status without its enum prefix.
func programStatusName(t common.ProgramStatusType) string {
	return strings.TrimPrefix(t.String(), "ProgramStatusType_")
}

func marketStateName(state int32) string {
	return strings.TrimPrefix(qotcommon.QotMarketState(state).String(), "QotMarketState_")
}

// quoteRights is OpenD's QotRight push as market to level, the level in
// OpenD's words without its enum prefix. A market OpenD left out is absent:
// the push says nothing about it, which is not the same as no right.
func quoteRights(right *notify.QotRight) map[string]string {
	if right == nil {
		return nil
	}
	name := func(level int32) string {
		return strings.TrimPrefix(qotcommon.QotRight(level).String(), "QotRight_")
	}
	rights := map[string]string{
		"HK": name(right.GetHkQotRight()),
		"US": name(right.GetUsQotRight()),
	}
	if right.ShQotRight != nil {
		rights["SH"] = name(right.GetShQotRight())
	}
	if right.SzQotRight != nil {
		rights["SZ"] = name(right.GetSzQotRight())
	}
	return rights
}

// classifyEnd turns the way a connection failed into the session's reason.
func classifyEnd(err error) error {
	if err == nil {
		return &SessionEndError{Reason: EndSocket}
	}
	if _, ok := errors.AsType[*SessionEndError](err); ok {
		return err
	}
	if errors.Is(err, ErrFrame) {
		return &SessionEndError{Reason: EndFrame, Detail: err.Error()}
	}
	if errors.Is(err, io.EOF) {
		return &SessionEndError{Reason: EndSocket}
	}
	return &SessionEndError{Reason: EndSocket, Detail: err.Error()}
}

// gatewayEventEnd is the session end a gateway event calls for, nil for the
// events that leave the session open.
func gatewayEventEnd(event *notify.GtwEvent) *SessionEndError {
	reason, ok := map[notify.GtwEventType]SessionEnd{
		notify.GtwEventType_GtwEventType_KickedOut:        EndKickedOut,
		notify.GtwEventType_GtwEventType_LoginPwdChanged:  EndPasswordChanged,
		notify.GtwEventType_GtwEventType_BanLogin:         EndLoginBanned,
		notify.GtwEventType_GtwEventType_EnableDeviceLock: EndDeviceLock,
		notify.GtwEventType_GtwEventType_ForceUpdate:      EndForceUpdate,
	}[notify.GtwEventType(event.GetEventType())]
	if !ok {
		return nil
	}
	return &SessionEndError{Reason: reason, Detail: event.GetDesc()}
}
