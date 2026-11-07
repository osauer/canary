package futu

import (
	"context"
	"fmt"
	"time"

	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	qotgetkl "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getkl"
	qotrequesthistorykl "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_requesthistorykl"
	qotsub "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_sub"
)

// BarPeriod is a candle's length.
type BarPeriod string

// The candle lengths OpenD serves that the daemon reads.
const (
	Bar1Min   BarPeriod = "1m"
	Bar3Min   BarPeriod = "3m"
	Bar5Min   BarPeriod = "5m"
	Bar15Min  BarPeriod = "15m"
	Bar30Min  BarPeriod = "30m"
	Bar60Min  BarPeriod = "60m"
	BarDay    BarPeriod = "1d"
	BarWeek   BarPeriod = "1w"
	BarMonth  BarPeriod = "1mo"
	barLayout           = "2006-01-02 15:04:05"
)

var barTypes = map[BarPeriod]qotcommon.KLType{
	Bar1Min:  qotcommon.KLType_KLType_1Min,
	Bar3Min:  qotcommon.KLType_KLType_3Min,
	Bar5Min:  qotcommon.KLType_KLType_5Min,
	Bar15Min: qotcommon.KLType_KLType_15Min,
	Bar30Min: qotcommon.KLType_KLType_30Min,
	Bar60Min: qotcommon.KLType_KLType_60Min,
	BarDay:   qotcommon.KLType_KLType_Day,
	BarWeek:  qotcommon.KLType_KLType_Week,
	BarMonth: qotcommon.KLType_KLType_Month,
}

var barSubTypes = map[BarPeriod]qotcommon.SubType{
	Bar1Min:  qotcommon.SubType_SubType_KL_1Min,
	Bar3Min:  qotcommon.SubType_SubType_KL_3Min,
	Bar5Min:  qotcommon.SubType_SubType_KL_5Min,
	Bar15Min: qotcommon.SubType_SubType_KL_15Min,
	Bar30Min: qotcommon.SubType_SubType_KL_30Min,
	Bar60Min: qotcommon.SubType_SubType_KL_60Min,
	BarDay:   qotcommon.SubType_SubType_KL_Day,
	BarWeek:  qotcommon.SubType_SubType_KL_Week,
	BarMonth: qotcommon.SubType_SubType_KL_Month,
}

// Bar is one candle. A blank candle carries only its time: OpenD marks a
// period with no trade that way.
type Bar struct {
	Time     time.Time
	Text     string
	Blank    bool
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   int64
	Turnover float64
}

func barFromRow(row *qotcommon.KLine) Bar {
	return Bar{
		Time:     timeFromSeconds(row.GetTimestamp()),
		Text:     row.GetTime(),
		Blank:    row.GetIsBlank(),
		Open:     row.GetOpenPrice(),
		High:     row.GetHighPrice(),
		Low:      row.GetLowPrice(),
		Close:    row.GetClosePrice(),
		Volume:   row.GetVolume(),
		Turnover: row.GetTurnover(),
	}
}

func barsFromRows(rows []*qotcommon.KLine) []Bar {
	out := make([]Bar, 0, len(rows))
	for _, row := range rows {
		out = append(out, barFromRow(row))
	}
	return out
}

func barType(period BarPeriod) (qotcommon.KLType, error) {
	klType, ok := barTypes[period]
	if !ok {
		return 0, fmt.Errorf("opend candle period %q", period)
	}
	return klType, nil
}

// SubscribeBars takes a line for an instrument's candles of one period, which
// [Connector.RecentBars] needs; it costs one unit of the subscription quota.
func (c *Connector) SubscribeBars(ctx context.Context, code string, period BarPeriod) error {
	subType, ok := barSubTypes[period]
	if !ok {
		return fmt.Errorf("opend candle period %q", period)
	}
	sec, err := security(code)
	if err != nil {
		return err
	}
	conn, err := c.session()
	if err != nil {
		return err
	}
	req := &qotsub.Request{C2S: &qotsub.C2S{
		SecurityList: []*qotcommon.Security{sec},
		SubTypeList:  []int32{int32(subType)},
		IsSubOrUnSub: new(true),
	}}
	var resp qotsub.Response
	if err := conn.request(ctx, ProtoQotSub, req, &resp); err != nil {
		return c.subscribeRefused(ctx, conn, []string{code}, err)
	}
	return nil
}

func recentBarsRequest(sec *qotcommon.Security, klType qotcommon.KLType, count int32) *qotgetkl.Request {
	return &qotgetkl.Request{C2S: &qotgetkl.C2S{
		RehabType: new(int32(qotcommon.RehabType_RehabType_None)),
		KlType:    new(int32(klType)),
		Security:  sec,
		ReqNum:    new(count),
	}}
}

// RecentBars reads the latest candles of a subscribed instrument; the count
// is capped at OpenD's documented limit per request.
func (c *Connector) RecentBars(ctx context.Context, code string, period BarPeriod, count int) ([]Bar, error) {
	klType, err := barType(period)
	if err != nil {
		return nil, err
	}
	sec, err := security(code)
	if err != nil {
		return nil, err
	}
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	limit, _ := LimitFor(ProtoQotGetKL)
	var resp qotgetkl.Response
	if err := conn.request(ctx, ProtoQotGetKL, recentBarsRequest(sec, klType, int32(min(count, limit.PerRequest))), &resp); err != nil {
		return nil, err
	}
	return barsFromRows(resp.GetS2C().GetKlList()), nil
}

func historyBarsRequest(sec *qotcommon.Security, klType qotcommon.KLType, from, to string, page []byte) *qotrequesthistorykl.Request {
	limit, _ := LimitFor(ProtoQotRequestHistoryKL)
	return &qotrequesthistorykl.Request{C2S: &qotrequesthistorykl.C2S{
		RehabType:   new(int32(qotcommon.RehabType_RehabType_None)),
		KlType:      new(int32(klType)),
		Security:    sec,
		BeginTime:   new(from),
		EndTime:     new(to),
		MaxAckKLNum: new(int32(limit.PerRequest)),
		NextReqKey:  page,
	}}
}

// HistoryBars reads the candles between two instants, paging until OpenD has
// no more. An instrument's first request in seven days costs one unit of the
// history quota and a repeat none; only the first page spends the pacing
// window, as the interface page documents. from and to are the market's own
// local time: OpenD reads the strings that way.
func (c *Connector) HistoryBars(ctx context.Context, code string, period BarPeriod, from, to time.Time) ([]Bar, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	return readHistoryBars(ctx, conn, code, period, from, to)
}

func readHistoryBars(ctx context.Context, conn *connection, code string, period BarPeriod, from, to time.Time) ([]Bar, error) {
	klType, err := barType(period)
	if err != nil {
		return nil, err
	}
	sec, err := security(code)
	if err != nil {
		return nil, err
	}
	var bars []Bar
	var page []byte
	for {
		var resp qotrequesthistorykl.Response
		req := historyBarsRequest(sec, klType, from.Format(barLayout), to.Format(barLayout), page)
		if page == nil {
			err = conn.request(ctx, ProtoQotRequestHistoryKL, req, &resp)
		} else {
			err = conn.requestUnpaced(ctx, ProtoQotRequestHistoryKL, req, &resp)
		}
		if err != nil {
			return bars, err
		}
		bars = append(bars, barsFromRows(resp.GetS2C().GetKlList())...)
		page = resp.GetS2C().GetNextReqKey()
		if len(page) == 0 {
			return bars, nil
		}
	}
}
