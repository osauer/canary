package futu

import (
	"sync"
	"time"
)

// OpenD protocol ids this client speaks, from the public client's ProtoId
// table (confirmed against each interface page named in [Limits]). There is
// no constant for the trade unlock (2005): no encoder for it exists.
const (
	ProtoInitConnect            uint32 = 1001
	ProtoGetGlobalState         uint32 = 1002
	ProtoNotify                 uint32 = 1003
	ProtoKeepAlive              uint32 = 1004
	ProtoTrdGetAccList          uint32 = 2001
	ProtoTrdSubAccPush          uint32 = 2008
	ProtoTrdGetFunds            uint32 = 2101
	ProtoTrdGetPositionList     uint32 = 2102
	ProtoTrdGetMaxTrdQtys       uint32 = 2111
	ProtoTrdGetOrderList        uint32 = 2201
	ProtoTrdPlaceOrder          uint32 = 2202
	ProtoTrdModifyOrder         uint32 = 2205
	ProtoTrdUpdateOrder         uint32 = 2208
	ProtoTrdGetOrderFillList    uint32 = 2211
	ProtoTrdUpdateOrderFill     uint32 = 2218
	ProtoTrdGetHistoryOrderList uint32 = 2221
	ProtoTrdGetHistoryFillList  uint32 = 2222
	ProtoQotSub                 uint32 = 3001
	ProtoQotRegQotPush          uint32 = 3002
	ProtoQotGetSubInfo          uint32 = 3003
	ProtoQotGetBasicQot         uint32 = 3004
	ProtoQotUpdateBasicQot      uint32 = 3005
	ProtoQotGetKL               uint32 = 3006
	ProtoQotRequestHistoryKL    uint32 = 3103
	ProtoQotRequestHistoryQuota uint32 = 3104
	ProtoQotGetStaticInfo       uint32 = 3202
	ProtoQotGetSecuritySnapshot uint32 = 3203
)

// Limit is one row of OpenD's documented per-interface limits: how many
// requests a 30-second window admits (0 when the page states no frequency
// limit), how many items one request may carry (0 when the page states no
// size), and the page and date the numbers were read from. Trade-class
// limits are per account; this client is pinned to one account, so the row
// applies to the connection.
type Limit struct {
	ProtoID uint32
	Name    string
	// Requests admitted per Window; 0 means the page states no limit.
	Requests int
	Window   time.Duration
	// PerRequest is the largest item count one request may carry; 0 means
	// the page states none.
	PerRequest int
	// Source is the documentation page, Read the date it was read.
	Source string
	Read   string
}

const (
	docQuote = "https://openapi.futunn.com/futu-api-doc/en/quote/"
	docTrade = "https://openapi.futunn.com/futu-api-doc/en/trade/"
	docIntro = "https://openapi.futunn.com/futu-api-doc/en/intro/"
	// window is the one frequency window OpenD documents.
	window = 30 * time.Second
)

// Limits is the pacing table, one source for the client's [Pacer] and the
// simulator's enforcement. Every protocol the client sends has a row, so
// that "no documented limit" is a recorded reading and not an omission.
var Limits = []Limit{
	{ProtoID: ProtoInitConnect, Name: "InitConnect", Source: "https://openapi.futunn.com/futu-api-doc/en/quick/opend-base.html", Read: "2026-11-04"},
	{ProtoID: ProtoGetGlobalState, Name: "GetGlobalState", Source: docIntro + "authority.html", Read: "2026-11-04"},
	{ProtoID: ProtoKeepAlive, Name: "KeepAlive", Source: docIntro + "authority.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetAccList, Name: "Trd_GetAccList", Source: docTrade + "get-acc-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdSubAccPush, Name: "Trd_SubAccPush", Source: docTrade + "sub-acc-push.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetFunds, Name: "Trd_GetFunds", Requests: 10, Window: window, Source: docTrade + "get-funds.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetPositionList, Name: "Trd_GetPositionList", Requests: 10, Window: window, Source: docTrade + "get-position-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetMaxTrdQtys, Name: "Trd_GetMaxTrdQtys", Requests: 10, Window: window, Source: docTrade + "get-max-trd-qtys.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetOrderList, Name: "Trd_GetOrderList", Requests: 10, Window: window, Source: docTrade + "get-order-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetOrderFillList, Name: "Trd_GetOrderFillList", Requests: 10, Window: window, Source: docTrade + "get-order-fill-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetHistoryOrderList, Name: "Trd_GetHistoryOrderList", Requests: 10, Window: window, Source: docTrade + "get-history-order-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoTrdGetHistoryFillList, Name: "Trd_GetHistoryOrderFillList", Requests: 10, Window: window, Source: docTrade + "get-history-order-fill-list.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotSub, Name: "Qot_Sub", Source: docQuote + "sub.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotRegQotPush, Name: "Qot_RegQotPush", Source: docQuote + "sub.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotGetSubInfo, Name: "Qot_GetSubInfo", Source: docQuote + "query-subscription.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotGetBasicQot, Name: "Qot_GetBasicQot", Source: docQuote + "get-stock-quote.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotGetKL, Name: "Qot_GetKL", PerRequest: 1000, Source: docQuote + "get-kl.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotRequestHistoryKL, Name: "Qot_RequestHistoryKL", Requests: 60, Window: window, PerRequest: 1000, Source: docQuote + "request-history-kline.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotRequestHistoryQuota, Name: "Qot_RequestHistoryKLQuota", Source: docQuote + "get-history-kl-quota.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotGetStaticInfo, Name: "Qot_GetStaticInfo", Source: docQuote + "get-static-info.html", Read: "2026-11-04"},
	{ProtoID: ProtoQotGetSecuritySnapshot, Name: "Qot_GetSecuritySnapshot", Requests: 60, Window: window, PerRequest: 400, Source: docQuote + "get-market-snapshot.html", Read: "2026-11-04"},
}

// Readings the pages state as rules rather than per-interface numbers.
const (
	// SubscriptionMinimumHold is how long a subscription is held before it
	// may be released ("you can unsubscribe after subscribing after at
	// least one minute", sub.html, read 2026-11-04).
	SubscriptionMinimumHold = time.Minute
	// HistoryQuotaWindow is the window in which one history request per
	// instrument costs one unit of the historical candlestick quota and
	// repeats are free (authority.html, read 2026-11-04).
	HistoryQuotaWindow = 7 * 24 * time.Hour
)

// SubscriptionQuotaTiers are the subscription quotas OpenD grants by
// account tier, lowest tier first; the historical candlestick quota has
// the same tiers (authority.html, read 2026-11-04). The quota is OpenD-wide
// across every connection, so the client learns its spend from OpenD and
// never from this table.
var SubscriptionQuotaTiers = []int{100, 300, 1000, 2000}

// LimitFor returns the table row for a protocol.
func LimitFor(protoID uint32) (Limit, bool) {
	for _, l := range Limits {
		if l.ProtoID == protoID {
			return l, true
		}
	}
	return Limit{}, false
}

// Pacer admits requests under [Limits] with a sliding window per protocol,
// so the client never sends the request OpenD would refuse. A protocol
// without a documented frequency limit is always admitted. One Pacer serves
// one connection and is safe for concurrent use.
type Pacer struct {
	now  func() time.Time
	mu   sync.Mutex
	sent map[uint32][]time.Time
}

// NewPacer builds a pacer over [Limits] on the given clock (nil for the
// wall clock).
func NewPacer(now func() time.Time) *Pacer {
	if now == nil {
		now = time.Now
	}
	return &Pacer{now: now, sent: make(map[uint32][]time.Time)}
}

// Admit records one request for the protocol, or refuses it with a
// [PacedError] when the window is full. A refusal records nothing.
func (p *Pacer) Admit(protoID uint32) error {
	limit, ok := LimitFor(protoID)
	if !ok || limit.Requests == 0 {
		return nil
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	recent := p.sent[protoID][:0:0]
	for _, at := range p.sent[protoID] {
		if now.Sub(at) < limit.Window {
			recent = append(recent, at)
		}
	}
	if len(recent) >= limit.Requests {
		p.sent[protoID] = recent
		return &PacedError{Limit: limit, RetryAfter: limit.Window - now.Sub(recent[0])}
	}
	p.sent[protoID] = append(recent, now)
	return nil
}

// Spent reports how many requests the protocol's current window holds.
func (p *Pacer) Spent(protoID uint32) int {
	limit, ok := LimitFor(protoID)
	if !ok || limit.Requests == 0 {
		return 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, at := range p.sent[protoID] {
		if now.Sub(at) < limit.Window {
			n++
		}
	}
	return n
}
