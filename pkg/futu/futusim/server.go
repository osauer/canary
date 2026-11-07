package futusim

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/common"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/getglobalstate"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/getuserinfo"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/initconnect"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/keepalive"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/notify"
	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetacclist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getacclist"
)

// unlockProto is OpenD's trade unlock. The simulator does not implement it:
// a frame carrying it fails the test that owns the server.
const unlockProto = 2005

// Synthetic values the simulator answers with.
const (
	defaultBuildNo     int32  = 7008
	defaultKeepAlive   int32  = 10
	defaultLoginUserID uint64 = 900001
	firstConnID        uint64 = 5000
	syntheticAESKey           = "0000000000000000"
)

// Options configures a [Server]; the zero value serves the default book with
// a current OpenD on a free loopback port.
type Options struct {
	// Listen is the address to bind; empty is a free loopback port.
	Listen string
	// Book is the state served; nil is [Default].
	Book *Book
	// ServerVersion is the serverVer answered; zero is the client's minimum.
	ServerVersion int32
	// BuildNo is the serverBuildNo answered.
	BuildNo int32
	// KeepAliveSeconds is the interval InitConnect tells the client.
	KeepAliveSeconds int32
	// RequireRSA refuses InitConnect as an OpenD with an RSA key refuses a
	// client that asks for no encryption. The refusal's code is a placeholder
	// until the first real capture replaces it.
	RequireRSA bool
	// ProgramStatus is the status GetGlobalState reports; zero is Ready.
	ProgramStatus common.ProgramStatusType
	// Record, when set, receives every frame in both directions.
	Record *Recorder
	// OnForbidden is told about a frame the simulator must never receive.
	OnForbidden func(reason string)
}

// Server is the simulated OpenD: the real framing and protobuf over TCP, with
// the switches a test uses to put a client through OpenD's states.
type Server struct {
	opts     Options
	book     *Book
	listener net.Listener
	pacer    *futu.Pacer
	// accountPacers hold the windows OpenD counts per account.
	accountPacers map[uint64]*futu.Pacer

	mu         sync.Mutex
	conns      map[*simConn]struct{}
	nextConnID uint64
	silent     bool
	// staticInfoDown refuses every static information request.
	staticInfoDown bool
	programStatus  common.ProgramStatusType
	quoteLoggedIn  bool
	tradeLoggedIn  bool
	hiddenAccount  map[uint64]bool
	quoteRights    map[string]int32
	userInfoFlags  []int32
	userInfoDelay  time.Duration
	refusals       map[uint32]int
	forbidden      int
	closed         bool

	wg sync.WaitGroup
}

// line is one held subscription: an instrument and a data type.
type line struct {
	code    string
	subType int32
}

type simConn struct {
	nc      net.Conn
	id      uint64
	writeMu sync.Mutex
	// lines, pushes and accounts are guarded by the server's mutex.
	lines    map[line]bool
	pushes   map[string]bool
	accounts map[uint64]bool
}

// Listen starts a server on its own goroutines; [Server.Close] ends it.
func Listen(opts Options) (*Server, error) {
	if opts.Book == nil {
		opts.Book = Default()
	}
	if opts.ServerVersion == 0 {
		opts.ServerVersion = futu.MinServerVersion
	}
	if opts.BuildNo == 0 {
		opts.BuildNo = defaultBuildNo
	}
	if opts.KeepAliveSeconds == 0 {
		opts.KeepAliveSeconds = defaultKeepAlive
	}
	if opts.ProgramStatus == common.ProgramStatusType_ProgramStatusType_None {
		opts.ProgramStatus = common.ProgramStatusType_ProgramStatusType_Ready
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("futusim: listen: %w", err)
	}
	s := &Server{
		opts:          opts,
		book:          opts.Book,
		listener:      listener,
		pacer:         futu.NewPacer(opts.Book.Clock.Now),
		accountPacers: make(map[uint64]*futu.Pacer),
		conns:         make(map[*simConn]struct{}),
		nextConnID:    firstConnID,
		programStatus: opts.ProgramStatus,
		quoteLoggedIn: true,
		tradeLoggedIn: true,
		hiddenAccount: make(map[uint64]bool),
		quoteRights:   map[string]int32{"HK": quoteRightLevel1, "US": quoteRightLevel3, "SG": quoteRightLevel1, "MY": quoteRightNo, "JP": quoteRightNo},
		refusals:      make(map[uint32]int),
	}
	s.wg.Go(s.accept)
	return s, nil
}

// Start runs a server for one test: a failure to bind fails the test, the
// server closes with the test, and a frame the simulator must never receive
// fails it too.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.OnForbidden == nil {
		opts.OnForbidden = func(reason string) { t.Errorf("futusim: %s", reason) }
	}
	s, err := Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Book is the state the server serves.
func (s *Server) Book() *Book { return s.book }

// Close ends every connection and waits for them.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for conn := range s.conns {
		_ = conn.nc.Close()
	}
	s.mu.Unlock()
	_ = s.listener.Close()
	s.wg.Wait()
}

// SetStaticInfoDown makes OpenD refuse every static information request, as it
// can while its quote server is not logged in.
func (s *Server) SetStaticInfoDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staticInfoDown = down
}

// SetSilent makes OpenD accept frames and answer none, as a hung OpenD does.
func (s *Server) SetSilent(silent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent = silent
}

// SetProgramStatus changes the status GetGlobalState reports and pushes it.
func (s *Server) SetProgramStatus(status common.ProgramStatusType) {
	s.mu.Lock()
	s.programStatus = status
	s.mu.Unlock()
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:          new(int32(notify.NotifyType_NotifyType_ProgramStatus)),
			ProgramStatus: &notify.ProgramStatus{ProgramStatus: &common.ProgramStatus{Type: status.Enum()}},
		},
	})
}

// SetLoggedIn changes the two logins GetGlobalState reports and pushes them.
func (s *Server) SetLoggedIn(quote, trade bool) {
	s.mu.Lock()
	s.quoteLoggedIn, s.tradeLoggedIn = quote, trade
	s.mu.Unlock()
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:          new(int32(notify.NotifyType_NotifyType_ConnStatus)),
			ConnectStatus: &notify.ConnectStatus{QotLogined: new(quote), TrdLogined: new(trade)},
		},
	})
}

// SetAccountListed adds or removes an account from the account list.
func (s *Server) SetAccountListed(id uint64, listed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hiddenAccount[id] = !listed
}

// KickOut pushes OpenD's kicked-out gateway event to every connection.
func (s *Server) KickOut() {
	s.PushGatewayEvent(notify.GtwEventType_GtwEventType_KickedOut, "synthetic: account logged in elsewhere")
}

// PushGatewayEvent pushes one gateway event to every connection.
func (s *Server) PushGatewayEvent(event notify.GtwEventType, desc string) {
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:  new(int32(notify.NotifyType_NotifyType_GtwEvent)),
			Event: &notify.GtwEvent{EventType: new(int32(event)), Desc: new(desc)},
		},
	})
}

// PushQuotaState pushes the quota the book grants and its spend.
func (s *Server) PushQuotaState() {
	used, _ := s.book.SubscriptionUsage()
	history, _, _ := s.book.HistoryUsage()
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:     new(int32(notify.NotifyType_NotifyType_APIQuota)),
			ApiQuota: &notify.APIQuota{SubQuota: new(int32(s.book.SubscriptionQuota())), HistoryKLQuota: new(int32(s.book.HistoryQuota()))},
		},
	})
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:      new(int32(notify.NotifyType_NotifyType_UsedQuota)),
			UsedQuota: &notify.UsedQuota{UsedSubQuota: new(int32(used)), UsedKLineQuota: new(int32(history))},
		},
	})
}

// PushQuoteRights pushes one quote-right state: market to OpenD's QotRight.
func (s *Server) PushQuoteRights(hk, us int32) {
	s.mu.Lock()
	s.quoteRights["HK"], s.quoteRights["US"] = hk, us
	s.mu.Unlock()
	s.Push(futu.ProtoNotify, &notify.Response{
		RetType: new(int32(0)),
		S2C: &notify.S2C{
			Type:     new(int32(notify.NotifyType_NotifyType_QotRight)),
			QotRight: &notify.QotRight{HkQotRight: new(hk), UsQotRight: new(us), CnQotRight: new(int32(0))},
		},
	})
}

// Push sends one message to every connection; its serial is zero, since it
// answers nothing.
func (s *Server) Push(protoID uint32, message proto.Message) {
	body, err := marshal(message)
	if err != nil {
		return
	}
	for _, conn := range s.connections() {
		s.write(conn, futu.Frame{ProtoID: protoID, Body: body})
	}
}

// connections lists the connected clients in the order they connected, so a
// scripted session pushes in one order.
func (s *Server) connections() []*simConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*simConn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	slices.SortFunc(conns, func(a, b *simConn) int { return cmp.Compare(a.id, b.id) })
	return conns
}

// dropConnection forgets a client and releases the lines only it held: OpenD
// releases a closed connection's lines itself.
func (s *Server) dropConnection(conn *simConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
	for held := range conn.lines {
		shared := false
		for other := range s.conns {
			shared = shared || other.lines[held]
		}
		if !shared {
			s.book.releaseLine(held.code, held.subType)
		}
	}
}

// Refusals is how many requests of the protocol the simulator's pacing or
// quotas refused.
func (s *Server) Refusals(protoID uint32) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusals[protoID]
}

// ForbiddenFrames is how many frames arrived that must never arrive.
func (s *Server) ForbiddenFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forbidden
}

// Connections is how many clients are connected now.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) accept() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		conn := &simConn{nc: nc, id: s.nextConnID, lines: make(map[line]bool), pushes: make(map[string]bool), accounts: make(map[uint64]bool)}
		s.nextConnID++
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(conn) })
	}
}

func (s *Server) serve(conn *simConn) {
	defer func() {
		_ = conn.nc.Close()
		s.dropConnection(conn)
	}()
	for {
		frame, err := futu.ReadFrame(conn.nc)
		if err != nil {
			return
		}
		if s.opts.Record != nil {
			s.opts.Record.Add(DirectionIn, frame)
		}
		if frame.ProtoID == unlockProto {
			s.mu.Lock()
			s.forbidden++
			s.mu.Unlock()
			if s.opts.OnForbidden != nil {
				s.opts.OnForbidden("the client sent the trade unlock (2005), which no build may send")
			}
			return
		}
		s.mu.Lock()
		silent := s.silent
		s.mu.Unlock()
		if silent {
			continue
		}
		reply := s.answer(conn, frame)
		if reply == nil {
			continue
		}
		body, err := marshal(reply)
		if err != nil {
			return
		}
		s.write(conn, futu.Frame{ProtoID: frame.ProtoID, Serial: frame.Serial, Body: body})
	}
}

func (s *Server) write(conn *simConn, frame futu.Frame) {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	if s.opts.Record != nil {
		s.opts.Record.Add(DirectionOut, frame)
	}
	if err := futu.WriteFrame(conn.nc, frame); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		_ = conn.nc.Close()
	}
}

func marshal(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// answer builds the reply to one request. The pacing table in pkg/futu is the
// one source: a request past its window gets OpenD's frequency refusal.
func (s *Server) answer(conn *simConn, frame futu.Frame) proto.Message {
	// A cached read and a later page owe the window nothing; their handlers
	// decide once they have read the request.
	if limit, ok := futu.LimitFor(frame.ProtoID); !ok || (!limit.CachedReadsFree && !limit.FirstPageOnly && !limit.PerAccount) {
		if refusal := s.admit(frame.ProtoID); refusal != nil {
			return refusal
		}
	}
	switch frame.ProtoID {
	case futu.ProtoInitConnect:
		return s.answerInitConnect(conn, frame)
	case futu.ProtoKeepAlive:
		return s.answerKeepAlive(frame)
	case futu.ProtoGetGlobalState:
		return s.answerGlobalState(conn)
	case futu.ProtoTrdGetAccList:
		return s.answerAccList(frame)
	case futu.ProtoGetUserInfo:
		return s.answerUserInfo(frame)
	}
	return s.answerRead(conn, frame)
}

// refuseFrequency is OpenD's answer to a request past its window. Every
// generated Response carries retType, retMsg and errCode under the same field
// numbers, so one message encodes the refusal of any protocol.
func (s *Server) refuseFrequency(protoID uint32, err error) proto.Message {
	return refuse(fmt.Sprintf("synthetic: protocol %d: %v", protoID, err))
}

func refuse(message string) proto.Message {
	return &notify.Response{RetType: new(retFailed), RetMsg: new(message)}
}

const (
	retFailed = int32(-1)
	retBody   = int32(-500)
)

func (s *Server) answerInitConnect(conn *simConn, frame futu.Frame) proto.Message {
	var req initconnect.Request
	if err := proto.Unmarshal(frame.Body, &req); err != nil {
		return &initconnect.Response{RetType: new(retBody), RetMsg: new("synthetic: malformed InitConnect")}
	}
	if s.opts.RequireRSA {
		return &initconnect.Response{
			RetType: new(retFailed),
			ErrCode: new(int32(placeholderRSARefusal)),
			RetMsg:  new("synthetic: this OpenD has an RSA key and refuses unencrypted connections"),
		}
	}
	return &initconnect.Response{
		RetType: new(int32(0)),
		S2C: &initconnect.S2C{
			ServerVer:         new(s.opts.ServerVersion),
			LoginUserID:       new(defaultLoginUserID),
			ConnID:            new(conn.id),
			ConnAESKey:        new(syntheticAESKey),
			KeepAliveInterval: new(s.opts.KeepAliveSeconds),
		},
	}
}

// placeholderRSARefusal stands for the errCode of the RSA refusal, which is
// unconfirmed until the first real capture replaces it; no decision reads it.
const placeholderRSARefusal = 1

func (s *Server) answerKeepAlive(frame futu.Frame) proto.Message {
	var req keepalive.Request
	if err := proto.Unmarshal(frame.Body, &req); err != nil {
		return &keepalive.Response{RetType: new(retBody), RetMsg: new("synthetic: malformed KeepAlive")}
	}
	return &keepalive.Response{RetType: new(int32(0)), S2C: &keepalive.S2C{Time: new(s.book.Clock.Now().Unix())}}
}

func (s *Server) answerGlobalState(conn *simConn) proto.Message {
	s.mu.Lock()
	status, quote, trade := s.programStatus, s.quoteLoggedIn, s.tradeLoggedIn
	s.mu.Unlock()
	state := func(market string) *int32 {
		if s.book.MarketOpen(market) {
			return new(int32(3)) // QotMarketState_Morning
		}
		return new(int32(6)) // QotMarketState_Closed
	}
	return &getglobalstate.Response{
		RetType: new(int32(0)),
		S2C: &getglobalstate.S2C{
			MarketHK:       state("HK"),
			MarketUS:       state("US"),
			MarketSH:       new(int32(6)),
			MarketSZ:       new(int32(6)),
			MarketHKFuture: new(int32(6)),
			QotLogined:     new(quote),
			TrdLogined:     new(trade),
			ServerVer:      new(s.opts.ServerVersion),
			ServerBuildNo:  new(s.opts.BuildNo),
			Time:           new(s.book.Clock.Now().Unix()),
			ProgramStatus:  &common.ProgramStatus{Type: status.Enum()},
			ConnID:         new(conn.id),
		},
	}
}

// The quote rights the simulator's logins hold by default: QotRight values.
const (
	quoteRightLevel1 = int32(2)
	quoteRightNo     = int32(5)
	quoteRightLevel3 = int32(6)
)

// SetQuoteRight sets a market's right. A market at QotRight_No refuses quotes as
// OpenD 10.12 does: the whole request, in the words it uses.
func (s *Server) SetQuoteRight(market string, level int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quoteRights[market] = level
}

// quoteRefusal is OpenD's refusal of a request with a code its login has no right
// for, nil when the login has them all.
func (s *Server) quoteRefusal(action string, secs []*qotcommon.Security) proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sec := range secs {
		code := securityCode(sec)
		market, _, _ := strings.Cut(code, ".")
		if right, known := s.quoteRights[market]; known && right == quoteRightNo {
			return refuse(fmt.Sprintf("No permission to %s quotes for %s. Please check %s Security MarketStocks quote permissions.", action, code, market))
		}
	}
	return nil
}

// SetUserInfoDelay makes GetUserInfo slow, so a test can push during a refresh.
func (s *Server) SetUserInfoDelay(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userInfoDelay = delay
}

// UserInfoFlags lists the flag of each GetUserInfo request so far.
func (s *Server) UserInfoFlags() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.userInfoFlags)
}

// answerUserInfo gives each part only when its flag asks, as OpenD's definition
// says; no flag is the whole answer, the login's name included.
func (s *Server) answerUserInfo(frame futu.Frame) proto.Message {
	var req getuserinfo.Request
	if err := proto.Unmarshal(frame.Body, &req); err != nil {
		return &getuserinfo.Response{RetType: new(retBody), RetMsg: new("synthetic: malformed GetUserInfo")}
	}
	flag := req.GetC2S().GetFlag()
	s.mu.Lock()
	rights := maps.Clone(s.quoteRights)
	s.userInfoFlags = append(s.userInfoFlags, flag)
	delay := s.userInfoDelay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	answer := &getuserinfo.S2C{}
	if req.GetC2S().Flag == nil || flag&int32(getuserinfo.UserInfoField_UserInfoField_Basic) != 0 {
		answer.NickName, answer.UserID = new("synthetic"), new(int64(1))
	}
	if req.GetC2S().Flag == nil || flag&int32(getuserinfo.UserInfoField_UserInfoField_QotRight) != 0 {
		answer.HkQotRight, answer.UsQotRight = new(rights["HK"]), new(rights["US"])
		answer.SgStockQotRight, answer.MyStockQotRight, answer.JpStockQotRight = new(rights["SG"]), new(rights["MY"]), new(rights["JP"])
	}
	return &getuserinfo.Response{RetType: new(int32(0)), S2C: answer}
}

// answerAccList leaves a real account out unless the request asks for universal
// accounts, as OpenD 10.12 does for a universal account.
func (s *Server) answerAccList(frame futu.Frame) proto.Message {
	var req trdgetacclist.Request
	if err := proto.Unmarshal(frame.Body, &req); err != nil {
		return &trdgetacclist.Response{RetType: new(retBody), RetMsg: new("synthetic: malformed Trd_GetAccList")}
	}
	universal := req.GetC2S().GetNeedGeneralSecAccount()
	s.mu.Lock()
	tradeLoggedIn := s.tradeLoggedIn
	hidden := maps.Clone(s.hiddenAccount)
	s.mu.Unlock()
	if !tradeLoggedIn {
		return &trdgetacclist.Response{RetType: new(retFailed), RetMsg: new("synthetic: the trade server is not logged in")}
	}
	var rows []*trdcommon.TrdAcc
	for _, account := range s.book.Accounts() {
		if hidden[account.ID] || (account.Env == EnvReal && !universal) {
			continue
		}
		rows = append(rows, accountRow(account))
	}
	return &trdgetacclist.Response{RetType: new(int32(0)), S2C: &trdgetacclist.S2C{AccList: rows}}
}

func accountRow(account Account) *trdcommon.TrdAcc {
	env := trdcommon.TrdEnv_TrdEnv_Real
	if account.Env == EnvSimulate {
		env = trdcommon.TrdEnv_TrdEnv_Simulate
	}
	markets := make([]int32, 0, len(account.Markets))
	for _, market := range account.Markets {
		markets = append(markets, int32(tradeMarket(market)))
	}
	accType := trdcommon.TrdAccType_TrdAccType_Margin
	if strings.EqualFold(account.Type, "cash") {
		accType = trdcommon.TrdAccType_TrdAccType_Cash
	}
	return &trdcommon.TrdAcc{
		TrdEnv:            new(int32(env)),
		AccID:             new(account.ID),
		TrdMarketAuthList: markets,
		AccType:           new(int32(accType)),
		SecurityFirm:      new(int32(trdcommon.SecurityFirm_SecurityFirm_FutuSecurities)),
	}
}

func tradeMarket(market string) trdcommon.TrdMarket {
	switch market {
	case "HK":
		return trdcommon.TrdMarket_TrdMarket_HK
	case "US":
		return trdcommon.TrdMarket_TrdMarket_US
	case "SG":
		return trdcommon.TrdMarket_TrdMarket_SG
	}
	return trdcommon.TrdMarket_TrdMarket_Unknown
}
