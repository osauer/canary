package futusim

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/osauer/canary/v2/pkg/futu"
	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	qotgetbasicqot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getbasicqot"
	qotgetkl "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getkl"
	qotgetsecuritysnapshot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getsecuritysnapshot"
	qotgetstaticinfo "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getstaticinfo"
	qotgetsubinfo "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getsubinfo"
	qotrequesthistorykl "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_requesthistorykl"
	qotrequesthistoryklquota "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_requesthistoryklquota"
	qotsub "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_sub"
	qotupdatebasicqot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_updatebasicqot"
	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetfunds "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getfunds"
	trdgethistoryorderfilllist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_gethistoryorderfilllist"
	trdgethistoryorderlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_gethistoryorderlist"
	trdgetmaxtrdqtys "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getmaxtrdqtys"
	trdgetorderfilllist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getorderfilllist"
	trdgetorderlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getorderlist"
	trdgetpositionlist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getpositionlist"
	trdsubaccpush "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_subaccpush"
	trdupdateorder "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_updateorder"
	trdupdateorderfill "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_updateorderfill"
)

// wireTime is OpenD's time string; the simulator writes trade times in UTC and
// candle times in the market's own zone.
const wireTime = "2006-01-02 15:04:05"

var quoteMarkets = map[string]qotcommon.QotMarket{
	"HK": qotcommon.QotMarket_QotMarket_HK_Security,
	"US": qotcommon.QotMarket_QotMarket_US_Security,
	"SG": qotcommon.QotMarket_QotMarket_SG_Security,
}

var secMarkets = map[string]trdcommon.TrdSecMarket{
	"HK": trdcommon.TrdSecMarket_TrdSecMarket_HK,
	"US": trdcommon.TrdSecMarket_TrdSecMarket_US,
	"SG": trdcommon.TrdSecMarket_TrdSecMarket_SG,
}

var currencies = map[string]trdcommon.Currency{
	"HKD": trdcommon.Currency_Currency_HKD,
	"USD": trdcommon.Currency_Currency_USD,
	"SGD": trdcommon.Currency_Currency_SGD,
}

var periodMinutes = map[qotcommon.KLType]int{
	qotcommon.KLType_KLType_1Min:  1,
	qotcommon.KLType_KLType_3Min:  3,
	qotcommon.KLType_KLType_5Min:  5,
	qotcommon.KLType_KLType_15Min: 15,
	qotcommon.KLType_KLType_30Min: 30,
	qotcommon.KLType_KLType_60Min: 60,
	qotcommon.KLType_KLType_Day:   PeriodDay,
	qotcommon.KLType_KLType_Week:  PeriodWeek,
	qotcommon.KLType_KLType_Month: PeriodMonth,
}

var barSubTypes = map[qotcommon.SubType]qotcommon.KLType{
	qotcommon.SubType_SubType_KL_1Min:  qotcommon.KLType_KLType_1Min,
	qotcommon.SubType_SubType_KL_3Min:  qotcommon.KLType_KLType_3Min,
	qotcommon.SubType_SubType_KL_5Min:  qotcommon.KLType_KLType_5Min,
	qotcommon.SubType_SubType_KL_15Min: qotcommon.KLType_KLType_15Min,
	qotcommon.SubType_SubType_KL_30Min: qotcommon.KLType_KLType_30Min,
	qotcommon.SubType_SubType_KL_60Min: qotcommon.KLType_KLType_60Min,
	qotcommon.SubType_SubType_KL_Day:   qotcommon.KLType_KLType_Day,
	qotcommon.SubType_SubType_KL_Week:  qotcommon.KLType_KLType_Week,
	qotcommon.SubType_SubType_KL_Month: qotcommon.KLType_KLType_Month,
}

func securityCode(sec *qotcommon.Security) string {
	for prefix, market := range quoteMarkets {
		if int32(market) == sec.GetMarket() {
			return prefix + "." + sec.GetCode()
		}
	}
	return "?." + sec.GetCode()
}

func securityOf(code string) *qotcommon.Security {
	prefix, symbol, _ := strings.Cut(code, ".")
	return &qotcommon.Security{Market: new(int32(quoteMarkets[prefix])), Code: new(symbol)}
}

func ok() *int32 { return new(int32(0)) }

// admit spends the pacing window, or says the refusal.
func (s *Server) admit(protoID uint32) proto.Message {
	if err := s.pacer.Admit(protoID); err != nil {
		s.countRefusal(protoID)
		return s.refuseFrequency(protoID, err)
	}
	return nil
}

// admitFor spends one account's window for a protocol OpenD counts per account.
func (s *Server) admitFor(protoID uint32, account uint64) proto.Message {
	s.mu.Lock()
	pacer, ok := s.accountPacers[account]
	if !ok {
		pacer = futu.NewPacer(s.book.Clock.Now)
		s.accountPacers[account] = pacer
	}
	s.mu.Unlock()
	if err := pacer.Admit(protoID); err != nil {
		s.countRefusal(protoID)
		return s.refuseFrequency(protoID, err)
	}
	return nil
}

func (s *Server) countRefusal(protoID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusals[protoID]++
}

func (s *Server) refused(protoID uint32, err error) proto.Message {
	s.countRefusal(protoID)
	return refuse("synthetic: " + err.Error())
}

// answerRead answers the quote and trade reads. A protocol without a handler
// gets no answer, as OpenD answers nothing to a protocol it does not know.
func (s *Server) answerRead(conn *simConn, frame futu.Frame) proto.Message {
	decode := func(m proto.Message) proto.Message {
		if err := proto.Unmarshal(frame.Body, m); err != nil {
			return &qotsub.Response{RetType: new(retBody), RetMsg: new("synthetic: malformed request")}
		}
		return nil
	}
	switch frame.ProtoID {
	case futu.ProtoQotSub:
		var req qotsub.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerSub(conn, req.GetC2S())
	case futu.ProtoQotGetSubInfo:
		used, remaining := s.book.SubscriptionUsage()
		return &qotgetsubinfo.Response{RetType: ok(), S2C: &qotgetsubinfo.S2C{TotalUsedQuota: new(int32(used)), RemainQuota: new(int32(remaining))}}
	case futu.ProtoQotGetBasicQot:
		var req qotgetbasicqot.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerBasicQuotes(conn, req.GetC2S().GetSecurityList())
	case futu.ProtoQotGetSecuritySnapshot:
		var req qotgetsecuritysnapshot.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerSnapshot(req.GetC2S().GetSecurityList())
	case futu.ProtoQotGetStaticInfo:
		var req qotgetstaticinfo.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerStaticInfo(req.GetC2S().GetSecurityList())
	case futu.ProtoQotGetKL:
		var req qotgetkl.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerRecentBars(conn, req.GetC2S())
	case futu.ProtoQotRequestHistoryKL:
		var req qotrequesthistorykl.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerHistoryBars(req.GetC2S())
	case futu.ProtoQotRequestHistoryQuota:
		used, remaining, _ := s.book.HistoryUsage()
		return &qotrequesthistoryklquota.Response{RetType: ok(), S2C: &qotrequesthistoryklquota.S2C{UsedQuota: new(int32(used)), RemainQuota: new(int32(remaining))}}
	case futu.ProtoTrdGetFunds:
		var req trdgetfunds.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerFunds(req.GetC2S())
	case futu.ProtoTrdGetPositionList:
		var req trdgetpositionlist.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerPositions(req.GetC2S())
	case futu.ProtoTrdGetOrderList:
		var req trdgetorderlist.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		header := req.GetC2S().GetHeader()
		if refusal := s.tradeRead(frame.ProtoID, header, req.GetC2S().GetRefreshCache()); refusal != nil {
			return refusal
		}
		return &trdgetorderlist.Response{RetType: ok(), S2C: &trdgetorderlist.S2C{Header: header, OrderList: s.orderRows(header, time.Time{}, time.Time{})}}
	case futu.ProtoTrdGetOrderFillList:
		var req trdgetorderfilllist.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		header := req.GetC2S().GetHeader()
		if refusal := s.tradeRead(frame.ProtoID, header, req.GetC2S().GetRefreshCache()); refusal != nil {
			return refusal
		}
		return &trdgetorderfilllist.Response{RetType: ok(), S2C: &trdgetorderfilllist.S2C{Header: header, OrderFillList: s.fillRows(header, time.Time{}, time.Time{})}}
	case futu.ProtoTrdGetHistoryOrderList:
		var req trdgethistoryorderlist.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		header := req.GetC2S().GetHeader()
		if err := s.checkHeader(header); err != nil {
			return s.refused(frame.ProtoID, err)
		}
		if refusal := s.admitFor(frame.ProtoID, header.GetAccID()); refusal != nil {
			return refusal
		}
		from, to := filterWindow(req.GetC2S().GetFilterConditions())
		return &trdgethistoryorderlist.Response{RetType: ok(), S2C: &trdgethistoryorderlist.S2C{Header: header, OrderList: s.orderRows(header, from, to)}}
	case futu.ProtoTrdGetHistoryFillList:
		var req trdgethistoryorderfilllist.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		header := req.GetC2S().GetHeader()
		if err := s.checkHeader(header); err != nil {
			return s.refused(frame.ProtoID, err)
		}
		if refusal := s.admitFor(frame.ProtoID, header.GetAccID()); refusal != nil {
			return refusal
		}
		from, to := filterWindow(req.GetC2S().GetFilterConditions())
		return &trdgethistoryorderfilllist.Response{RetType: ok(), S2C: &trdgethistoryorderfilllist.S2C{Header: header, OrderFillList: s.fillRows(header, from, to)}}
	case futu.ProtoTrdGetMaxTrdQtys:
		var req trdgetmaxtrdqtys.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		return s.answerMaxQuantities(req.GetC2S())
	case futu.ProtoTrdSubAccPush:
		var req trdsubaccpush.Request
		if bad := decode(&req); bad != nil {
			return bad
		}
		s.mu.Lock()
		clear(conn.accounts)
		for _, id := range req.GetC2S().GetAccIDList() {
			conn.accounts[id] = true
		}
		s.mu.Unlock()
		return &trdsubaccpush.Response{RetType: ok(), S2C: &trdsubaccpush.S2C{}}
	}
	return nil
}

func (s *Server) answerSub(conn *simConn, c2s *qotsub.C2S) proto.Message {
	for _, sec := range c2s.GetSecurityList() {
		code := securityCode(sec)
		for _, subType := range c2s.GetSubTypeList() {
			var err error
			if c2s.GetIsSubOrUnSub() {
				err = s.book.Subscribe(code, subType)
			} else {
				err = s.book.Unsubscribe(code, subType)
			}
			if err != nil {
				return s.refused(futu.ProtoQotSub, err)
			}
			s.mu.Lock()
			key := line{code: code, subType: subType}
			if c2s.GetIsSubOrUnSub() {
				conn.lines[key] = true
			} else {
				delete(conn.lines, key)
			}
			s.mu.Unlock()
		}
		s.mu.Lock()
		switch {
		case c2s.GetIsSubOrUnSub() && c2s.GetIsRegOrUnRegPush():
			conn.pushes[code] = true
		case !c2s.GetIsSubOrUnSub():
			delete(conn.pushes, code)
		}
		s.mu.Unlock()
	}
	return &qotsub.Response{RetType: ok(), S2C: &qotsub.S2C{}}
}

func (s *Server) holds(conn *simConn, code string, subType qotcommon.SubType) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.lines[line{code: code, subType: int32(subType)}]
}

func (s *Server) basicRow(code string) (*qotcommon.BasicQot, error) {
	in, err := s.book.Instrument(code)
	if err != nil {
		return nil, err
	}
	q, err := s.book.Quote(code)
	if err != nil {
		return nil, err
	}
	return &qotcommon.BasicQot{
		Security:        securityOf(code),
		Name:            new(in.Name),
		IsSuspended:     new(false),
		ListTime:        new(in.Listed),
		PriceSpread:     new(in.Tick),
		UpdateTime:      new(q.At.UTC().Format(wireTime)),
		HighPrice:       new(q.High),
		OpenPrice:       new(q.DayOpen),
		LowPrice:        new(q.Low),
		CurPrice:        new(q.Last),
		LastClosePrice:  new(q.PrevClose),
		Volume:          new(q.Volume),
		Turnover:        new(float64(q.Volume) * q.Last),
		TurnoverRate:    new(float64(0)),
		Amplitude:       new(float64(0)),
		UpdateTimestamp: new(float64(q.At.Unix())),
	}, nil
}

func (s *Server) answerBasicQuotes(conn *simConn, secs []*qotcommon.Security) proto.Message {
	rows := make([]*qotcommon.BasicQot, 0, len(secs))
	for _, sec := range secs {
		code := securityCode(sec)
		if !s.holds(conn, code, qotcommon.SubType_SubType_Basic) {
			return s.refused(futu.ProtoQotGetBasicQot, fmt.Errorf("%s has no line", code))
		}
		row, err := s.basicRow(code)
		if err != nil {
			return s.refused(futu.ProtoQotGetBasicQot, err)
		}
		rows = append(rows, row)
	}
	return &qotgetbasicqot.Response{RetType: ok(), S2C: &qotgetbasicqot.S2C{BasicQotList: rows}}
}

func (s *Server) answerSnapshot(secs []*qotcommon.Security) proto.Message {
	limit, _ := futu.LimitFor(futu.ProtoQotGetSecuritySnapshot)
	if len(secs) > limit.PerRequest {
		return s.refused(futu.ProtoQotGetSecuritySnapshot, fmt.Errorf("%d instruments in one request, limit %d", len(secs), limit.PerRequest))
	}
	rows := make([]*qotgetsecuritysnapshot.Snapshot, 0, len(secs))
	for _, sec := range secs {
		code := securityCode(sec)
		in, err := s.book.Instrument(code)
		if err != nil {
			return s.refused(futu.ProtoQotGetSecuritySnapshot, err)
		}
		q, err := s.book.Quote(code)
		if err != nil {
			return s.refused(futu.ProtoQotGetSecuritySnapshot, err)
		}
		rows = append(rows, &qotgetsecuritysnapshot.Snapshot{Basic: &qotgetsecuritysnapshot.SnapshotBasicData{
			Security:        securityOf(code),
			Name:            new(in.Name),
			Type:            new(int32(securityType(in))),
			IsSuspend:       new(false),
			ListTime:        new(in.Listed),
			LotSize:         new(int32(in.LotSize)),
			PriceSpread:     new(in.Tick),
			UpdateTime:      new(q.At.UTC().Format(wireTime)),
			HighPrice:       new(q.High),
			OpenPrice:       new(q.DayOpen),
			LowPrice:        new(q.Low),
			LastClosePrice:  new(q.PrevClose),
			CurPrice:        new(q.Last),
			Volume:          new(q.Volume),
			Turnover:        new(float64(q.Volume) * q.Last),
			TurnoverRate:    new(float64(0)),
			UpdateTimestamp: new(float64(q.At.Unix())),
			AskPrice:        new(q.Ask),
			BidPrice:        new(q.Bid),
			AskVol:          new(in.LotSize * 10),
			BidVol:          new(in.LotSize * 10),
		}})
	}
	return &qotgetsecuritysnapshot.Response{RetType: ok(), S2C: &qotgetsecuritysnapshot.S2C{SnapshotList: rows}}
}

func securityType(in Instrument) qotcommon.SecurityType {
	if in.Type == "etf" {
		return qotcommon.SecurityType_SecurityType_Trust
	}
	return qotcommon.SecurityType_SecurityType_Eqty
}

func (s *Server) answerStaticInfo(secs []*qotcommon.Security) proto.Message {
	rows := make([]*qotcommon.SecurityStaticInfo, 0, len(secs))
	for i, sec := range secs {
		in, err := s.book.Instrument(securityCode(sec))
		if err != nil {
			return s.refused(futu.ProtoQotGetStaticInfo, err)
		}
		rows = append(rows, &qotcommon.SecurityStaticInfo{Basic: &qotcommon.SecurityStaticBasic{
			Security: securityOf(in.Code),
			Id:       new(int64(i + 1)),
			LotSize:  new(int32(in.LotSize)),
			SecType:  new(int32(securityType(in))),
			Name:     new(in.Name),
			ListTime: new(in.Listed),
		}})
	}
	return &qotgetstaticinfo.Response{RetType: ok(), S2C: &qotgetstaticinfo.S2C{StaticInfoList: rows}}
}

func barRows(bars []BookBar, zone *time.Location) []*qotcommon.KLine {
	rows := make([]*qotcommon.KLine, 0, len(bars))
	for _, bar := range bars {
		rows = append(rows, &qotcommon.KLine{
			Time:       new(bar.At.In(zone).Format(wireTime)),
			IsBlank:    new(false),
			HighPrice:  new(bar.High),
			OpenPrice:  new(bar.Open),
			LowPrice:   new(bar.Low),
			ClosePrice: new(bar.Close),
			Volume:     new(bar.Volume),
			Turnover:   new(float64(bar.Volume) * bar.Close),
			Timestamp:  new(float64(bar.At.Unix())),
		})
	}
	return rows
}

func (s *Server) answerRecentBars(conn *simConn, c2s *qotgetkl.C2S) proto.Message {
	code := securityCode(c2s.GetSecurity())
	klType := qotcommon.KLType(c2s.GetKlType())
	subscribed := false
	for subType, kind := range barSubTypes {
		if kind == klType && s.holds(conn, code, subType) {
			subscribed = true
		}
	}
	if !subscribed {
		return s.refused(futu.ProtoQotGetKL, fmt.Errorf("%s has no candle line of this period", code))
	}
	limit, _ := futu.LimitFor(futu.ProtoQotGetKL)
	count := min(int(c2s.GetReqNum()), limit.PerRequest)
	bars, zone, err := s.book.RecentBars(code, periodMinutes[klType], count)
	if err != nil {
		return s.refused(futu.ProtoQotGetKL, err)
	}
	return &qotgetkl.Response{RetType: ok(), S2C: &qotgetkl.S2C{Security: c2s.GetSecurity(), KlList: barRows(bars, zone)}}
}

func (s *Server) answerHistoryBars(c2s *qotrequesthistorykl.C2S) proto.Message {
	code := securityCode(c2s.GetSecurity())
	offset := 0
	if key := c2s.GetNextReqKey(); len(key) > 0 {
		parsed, err := strconv.Atoi(string(key))
		if err != nil {
			return s.refused(futu.ProtoQotRequestHistoryKL, errors.New("next page key is not one this simulator issued"))
		}
		offset = parsed
	} else {
		if refusal := s.admit(futu.ProtoQotRequestHistoryKL); refusal != nil {
			return refusal
		}
		if err := s.book.RequestHistory(code); err != nil {
			return s.refused(futu.ProtoQotRequestHistoryKL, err)
		}
	}
	bars, zone, err := s.book.HistoryBars(code, periodMinutes[qotcommon.KLType(c2s.GetKlType())], c2s.GetBeginTime(), c2s.GetEndTime())
	if err != nil {
		return s.refused(futu.ProtoQotRequestHistoryKL, err)
	}
	pageSize := int(c2s.GetMaxAckKLNum())
	if pageSize <= 0 {
		pageSize = len(bars)
	}
	end := min(offset+pageSize, len(bars))
	s2c := &qotrequesthistorykl.S2C{Security: c2s.GetSecurity(), KlList: barRows(bars[min(offset, len(bars)):end], zone)}
	if end < len(bars) {
		s2c.NextReqKey = []byte(strconv.Itoa(end))
	}
	return &qotrequesthistorykl.Response{RetType: ok(), S2C: s2c}
}

// checkHeader refuses a header the book's account list could not have given.
func (s *Server) checkHeader(header *trdcommon.TrdHeader) error {
	account, err := s.book.Account(header.GetAccID())
	if err != nil {
		return err
	}
	env := trdcommon.TrdEnv_TrdEnv_Real
	if account.Env == EnvSimulate {
		env = trdcommon.TrdEnv_TrdEnv_Simulate
	}
	if int32(env) != header.GetTrdEnv() {
		return fmt.Errorf("account %d is not in that environment", account.ID)
	}
	for _, market := range account.Markets {
		if int32(tradeMarket(market)) == header.GetTrdMarket() {
			return nil
		}
	}
	return fmt.Errorf("account %d is not authorised for market %d", account.ID, header.GetTrdMarket())
}

// tradeRead applies the header check and the pacing a trade read owes: the
// window counts only a read that refreshes OpenD's cache.
func (s *Server) tradeRead(protoID uint32, header *trdcommon.TrdHeader, refresh bool) proto.Message {
	if err := s.checkHeader(header); err != nil {
		return s.refused(protoID, err)
	}
	if refresh {
		return s.admitFor(protoID, header.GetAccID())
	}
	return nil
}

func (s *Server) answerFunds(c2s *trdgetfunds.C2S) proto.Message {
	header := c2s.GetHeader()
	if refusal := s.tradeRead(futu.ProtoTrdGetFunds, header, c2s.GetRefreshCache()); refusal != nil {
		return refusal
	}
	currency := "HKD"
	for name, value := range currencies {
		if int32(value) == c2s.GetCurrency() {
			currency = name
		}
	}
	funds, err := s.book.Funds(header.GetAccID(), currency)
	if err != nil {
		return s.refused(futu.ProtoTrdGetFunds, err)
	}
	row := &trdcommon.Funds{
		Power:             new(funds.TotalAssets * 2),
		TotalAssets:       new(funds.TotalAssets),
		Cash:              new(funds.Cash),
		MarketVal:         new(funds.MarketValue),
		FrozenCash:        new(float64(0)),
		DebtCash:          new(float64(0)),
		AvlWithdrawalCash: new(funds.Cash),
		Currency:          new(int32(currencies[currency])),
		AvailableFunds:    new(funds.Cash),
		InitialMargin:     new(funds.MarketValue / 2),
		MaintenanceMargin: new(funds.MarketValue / 4),
		UnrealizedPL:      new(funds.UnrealizedPnL),
		NetCashPower:      new(funds.Cash),
		LongMv:            new(funds.MarketValue),
		ShortMv:           new(float64(0)),
		RiskStatus:        new(int32(trdcommon.CltRiskStatus_CltRiskStatus_Level1)),
	}
	for _, name := range funds.Currencies {
		row.CashInfoList = append(row.CashInfoList, &trdcommon.AccCashInfo{
			Currency:         new(int32(currencies[name])),
			Cash:             new(funds.ByCurrency[name]),
			AvailableBalance: new(funds.ByCurrency[name]),
			NetCashPower:     new(funds.ByCurrency[name]),
		})
	}
	return &trdgetfunds.Response{RetType: ok(), S2C: &trdgetfunds.S2C{Header: header, Funds: row}}
}

func (s *Server) inHeaderMarket(header *trdcommon.TrdHeader, code string) bool {
	prefix, _, _ := strings.Cut(code, ".")
	return int32(tradeMarket(prefix)) == header.GetTrdMarket()
}

func bareCode(code string) (string, *int32) {
	prefix, symbol, _ := strings.Cut(code, ".")
	return symbol, new(int32(secMarkets[prefix]))
}

func (s *Server) answerPositions(c2s *trdgetpositionlist.C2S) proto.Message {
	header := c2s.GetHeader()
	if refusal := s.tradeRead(futu.ProtoTrdGetPositionList, header, c2s.GetRefreshCache()); refusal != nil {
		return refusal
	}
	positions, err := s.book.Positions(header.GetAccID())
	if err != nil {
		return s.refused(futu.ProtoTrdGetPositionList, err)
	}
	rows := make([]*trdcommon.Position, 0, len(positions))
	for _, p := range positions {
		if !s.inHeaderMarket(header, p.Code) {
			continue
		}
		in, err := s.book.Instrument(p.Code)
		if err != nil {
			return s.refused(futu.ProtoTrdGetPositionList, err)
		}
		q, err := s.book.Quote(p.Code)
		if err != nil {
			return s.refused(futu.ProtoTrdGetPositionList, err)
		}
		side := trdcommon.PositionSide_PositionSide_Long
		quantity := float64(p.Quantity)
		if quantity < 0 {
			side = trdcommon.PositionSide_PositionSide_Short
			quantity = -quantity
		}
		symbol, secMarket := bareCode(p.Code)
		pnl := (q.Last - p.Cost) * float64(p.Quantity)
		rows = append(rows, &trdcommon.Position{
			PositionID:       new(positionID(p)),
			PositionSide:     new(int32(side)),
			Code:             new(symbol),
			Name:             new(in.Name),
			Qty:              new(quantity),
			CanSellQty:       new(quantity),
			Price:            new(q.Last),
			Val:              new(q.Last * quantity),
			PlVal:            new(pnl),
			SecMarket:        secMarket,
			UnrealizedPL:     new(pnl),
			RealizedPL:       new(float64(0)),
			Currency:         new(int32(currencies[in.Currency])),
			DilutedCostPrice: new(p.Cost),
			AverageCostPrice: new(p.Cost),
		})
	}
	return &trdgetpositionlist.Response{RetType: ok(), S2C: &trdgetpositionlist.S2C{Header: header, PositionList: rows}}
}

func filterWindow(filter *trdcommon.TrdFilterConditions) (from, to time.Time) {
	from, _ = time.Parse(wireTime, filter.GetBeginTime())
	to, _ = time.Parse(wireTime, filter.GetEndTime())
	return from, to
}

func inWindow(at, from, to time.Time) bool {
	return (from.IsZero() || !at.Before(from)) && (to.IsZero() || !at.After(to))
}

func orderRow(o Order, in Instrument) *trdcommon.Order {
	side := trdcommon.TrdSide_TrdSide_Buy
	if o.Side == Sell {
		side = trdcommon.TrdSide_TrdSide_Sell
	}
	orderType := trdcommon.OrderType_OrderType_Market
	if o.Limit != nil {
		orderType = trdcommon.OrderType_OrderType_Normal
	}
	status := trdcommon.OrderStatus_OrderStatus_Submitted
	switch o.Status {
	case OrderFilled:
		status = trdcommon.OrderStatus_OrderStatus_Filled_All
	case OrderCancelled:
		status = trdcommon.OrderStatus_OrderStatus_Cancelled_All
	}
	symbol, secMarket := bareCode(o.Code)
	return &trdcommon.Order{
		TrdSide:     new(int32(side)),
		OrderType:   new(int32(orderType)),
		OrderStatus: new(int32(status)),
		OrderID:     new(o.ID),
		OrderIDEx:   new(fmt.Sprintf("sim-%d", o.ID)),
		Code:        new(symbol),
		Name:        new(in.Name),
		Qty:         new(float64(o.Quantity)),
		Price:       o.Limit,
		CreateTime:  new(o.PlacedAt.UTC().Format(wireTime)),
		UpdateTime:  new(o.UpdatedAt.UTC().Format(wireTime)),
		FillQty:     new(float64(o.Filled)),
		SecMarket:   secMarket,
		Currency:    new(int32(currencies[in.Currency])),
	}
}

func fillRow(f Fill, in Instrument) *trdcommon.OrderFill {
	side := trdcommon.TrdSide_TrdSide_Buy
	if f.Side == Sell {
		side = trdcommon.TrdSide_TrdSide_Sell
	}
	symbol, secMarket := bareCode(f.Code)
	return &trdcommon.OrderFill{
		TrdSide:    new(int32(side)),
		FillID:     new(f.ID),
		FillIDEx:   new(fmt.Sprintf("sim-%d", f.ID)),
		OrderID:    new(f.OrderID),
		Code:       new(symbol),
		Name:       new(in.Name),
		Qty:        new(float64(f.Quantity)),
		Price:      new(f.Price),
		CreateTime: new(f.At.UTC().Format(wireTime)),
		SecMarket:  secMarket,
	}
}

func (s *Server) orderRows(header *trdcommon.TrdHeader, from, to time.Time) []*trdcommon.Order {
	orders, err := s.book.Orders(header.GetAccID())
	if err != nil {
		return nil
	}
	rows := make([]*trdcommon.Order, 0, len(orders))
	for _, o := range orders {
		if !s.inHeaderMarket(header, o.Code) || !inWindow(o.PlacedAt, from, to) {
			continue
		}
		if in, err := s.book.Instrument(o.Code); err == nil {
			rows = append(rows, orderRow(o, in))
		}
	}
	return rows
}

func (s *Server) fillRows(header *trdcommon.TrdHeader, from, to time.Time) []*trdcommon.OrderFill {
	fills, err := s.book.Fills(header.GetAccID())
	if err != nil {
		return nil
	}
	rows := make([]*trdcommon.OrderFill, 0, len(fills))
	for _, f := range fills {
		if !s.inHeaderMarket(header, f.Code) || !inWindow(f.At, from, to) {
			continue
		}
		if in, err := s.book.Instrument(f.Code); err == nil {
			rows = append(rows, fillRow(f, in))
		}
	}
	return rows
}

func (s *Server) answerMaxQuantities(c2s *trdgetmaxtrdqtys.C2S) proto.Message {
	header := c2s.GetHeader()
	if err := s.checkHeader(header); err != nil {
		return s.refused(futu.ProtoTrdGetMaxTrdQtys, err)
	}
	if refusal := s.admitFor(futu.ProtoTrdGetMaxTrdQtys, header.GetAccID()); refusal != nil {
		return refusal
	}
	account, _ := s.book.Account(header.GetAccID())
	if strings.EqualFold(account.Type, "cash") {
		return s.refused(futu.ProtoTrdGetMaxTrdQtys, errors.New("a cash account has no maximum quantity read"))
	}
	var prefix string
	for name := range quoteMarkets {
		if int32(tradeMarket(name)) == header.GetTrdMarket() {
			prefix = name
		}
	}
	code := prefix + "." + c2s.GetCode()
	in, err := s.book.Instrument(code)
	if err != nil {
		return s.refused(futu.ProtoTrdGetMaxTrdQtys, err)
	}
	cash, _ := s.book.Cash(account.ID)
	price := c2s.GetPrice()
	if price <= 0 {
		return s.refused(futu.ProtoTrdGetMaxTrdQtys, errors.New("a price is required"))
	}
	lots := func(amount float64) float64 {
		return float64(int64(amount/price/float64(in.LotSize))) * float64(in.LotSize)
	}
	var held float64
	positions, _ := s.book.Positions(account.ID)
	for _, p := range positions {
		if p.Code == code {
			held = float64(p.Quantity)
		}
	}
	return &trdgetmaxtrdqtys.Response{RetType: ok(), S2C: &trdgetmaxtrdqtys.S2C{Header: header, MaxTrdQtys: &trdcommon.MaxTrdQtys{
		MaxCashBuy:          new(lots(cash[in.Currency])),
		MaxCashAndMarginBuy: new(lots(cash[in.Currency] * 2)),
		MaxPositionSell:     new(held),
		MaxSellShort:        new(float64(0)),
		MaxBuyBack:          new(float64(0)),
	}}}
}

// PushQuotes pushes the current quote of every line a connection registered
// for, as OpenD pushes a basic quote when a price moves.
func (s *Server) PushQuotes() {
	for _, conn := range s.connections() {
		s.mu.Lock()
		codes := make([]string, 0, len(conn.pushes))
		for code := range conn.pushes {
			codes = append(codes, code)
		}
		s.mu.Unlock()
		slices.Sort(codes)
		rows := make([]*qotcommon.BasicQot, 0, len(codes))
		for _, code := range codes {
			if row, err := s.basicRow(code); err == nil {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			continue
		}
		body, err := marshal(&qotupdatebasicqot.Response{RetType: ok(), S2C: &qotupdatebasicqot.S2C{BasicQotList: rows}})
		if err == nil {
			s.write(conn, futu.Frame{ProtoID: futu.ProtoQotUpdateBasicQot, Body: body})
		}
	}
}

// PushOrder pushes an order update to the connections subscribed to its
// account.
func (s *Server) PushOrder(o Order) {
	in, err := s.book.Instrument(o.Code)
	if err != nil {
		return
	}
	s.pushToAccount(o.AccountID, o.Code, futu.ProtoTrdUpdateOrder, func(header *trdcommon.TrdHeader) proto.Message {
		return &trdupdateorder.Response{RetType: ok(), S2C: &trdupdateorder.S2C{Header: header, Order: orderRow(o, in)}}
	})
}

// PushFill pushes a fill to the connections subscribed to its account.
func (s *Server) PushFill(f Fill) {
	in, err := s.book.Instrument(f.Code)
	if err != nil {
		return
	}
	s.pushToAccount(f.AccountID, f.Code, futu.ProtoTrdUpdateOrderFill, func(header *trdcommon.TrdHeader) proto.Message {
		return &trdupdateorderfill.Response{RetType: ok(), S2C: &trdupdateorderfill.S2C{Header: header, OrderFill: fillRow(f, in)}}
	})
}

func (s *Server) pushToAccount(accountID uint64, code string, protoID uint32, build func(*trdcommon.TrdHeader) proto.Message) {
	account, err := s.book.Account(accountID)
	if err != nil {
		return
	}
	env := trdcommon.TrdEnv_TrdEnv_Real
	if account.Env == EnvSimulate {
		env = trdcommon.TrdEnv_TrdEnv_Simulate
	}
	prefix, _, _ := strings.Cut(code, ".")
	body, err := marshal(build(&trdcommon.TrdHeader{
		TrdEnv:    new(int32(env)),
		AccID:     new(accountID),
		TrdMarket: new(int32(tradeMarket(prefix))),
	}))
	if err != nil {
		return
	}
	for _, conn := range s.connections() {
		s.mu.Lock()
		subscribed := conn.accounts[accountID]
		s.mu.Unlock()
		if subscribed {
			s.write(conn, futu.Frame{ProtoID: protoID, Body: body})
		}
	}
}
