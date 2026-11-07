package futu

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetfunds "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getfunds"
)

// ErrMarketNotAuthorised is a trade-class request for a market the pinned
// account's list row does not authorise.
var ErrMarketNotAuthorised = errors.New("opend account is not authorised for this market")

// CashRow is one currency's cash inside the funds. Nil means OpenD did not
// send the field.
type CashRow struct {
	Currency         string
	Cash             *float64
	AvailableBalance *float64
	NetCashPower     *float64
}

// Funds is the pinned account's funds in one currency. OpenD's required fields
// are plain; its optional ones are pointers, nil when OpenD did not send them,
// never zero.
type Funds struct {
	Currency            string
	Power               float64
	TotalAssets         float64
	Cash                float64
	MarketValue         float64
	FrozenCash          float64
	AvailableWithdrawal float64
	AvailableFunds      *float64
	InitialMargin       *float64
	MaintenanceMargin   *float64
	UnrealizedPnL       *float64
	RealizedPnL         *float64
	NetCashPower        *float64
	LongMarketValue     *float64
	ShortMarketValue    *float64
	MarginCallMargin    *float64
	RiskStatus          string
	CashRows            []CashRow
}

// marketCurrency is the currency funds are read in for a market: OpenD
// requires one on a universal account and ignores it otherwise.
var marketCurrency = map[int32]trdcommon.Currency{
	int32(trdcommon.TrdMarket_TrdMarket_HK): trdcommon.Currency_Currency_HKD,
	int32(trdcommon.TrdMarket_TrdMarket_US): trdcommon.Currency_Currency_USD,
	int32(trdcommon.TrdMarket_TrdMarket_SG): trdcommon.Currency_Currency_SGD,
	int32(trdcommon.TrdMarket_TrdMarket_JP): trdcommon.Currency_Currency_JPY,
	int32(trdcommon.TrdMarket_TrdMarket_AU): trdcommon.Currency_Currency_AUD,
	int32(trdcommon.TrdMarket_TrdMarket_MY): trdcommon.Currency_Currency_MYR,
	int32(trdcommon.TrdMarket_TrdMarket_CA): trdcommon.Currency_Currency_CAD,
	int32(trdcommon.TrdMarket_TrdMarket_CN): trdcommon.Currency_Currency_CNH,
}

func currencyName(currency int32) string {
	return strings.TrimPrefix(trdcommon.Currency(currency).String(), "Currency_")
}

// firmCurrency is each firm's home currency: a universal account's list of markets
// says nothing about its base, and the first one named may not be home.
var firmCurrency = map[string]trdcommon.Currency{
	"FutuSecurities": trdcommon.Currency_Currency_HKD,
	"FutuInc":        trdcommon.Currency_Currency_USD,
	"FutuSG":         trdcommon.Currency_Currency_SGD,
	"FutuAU":         trdcommon.Currency_Currency_AUD,
	"FutuCA":         trdcommon.Currency_Currency_CAD,
	"FutuMY":         trdcommon.Currency_Currency_MYR,
	"FutuJP":         trdcommon.Currency_Currency_JPY,
}

// ErrUnknownFirm is a real account whose firm has no known home currency. Its base
// is not guessed from the markets it lists.
var ErrUnknownFirm = errors.New("OpenD reports a firm with no known base currency for this account")

const (
	// fundsRoundingUnit is the most OpenD's totals can be off: half a cent. It sends
	// them to three places by its definitions, two is the coarsest seen in its docs.
	fundsRoundingUnit = 0.005
	// impliedRateMaxError is how far rounding may move an implied rate, as a share of
	// the rate, before it is refused. At 0.01% a holding's converted value is off by
	// less than a quote moves in a quiet minute; below about 100 in either total the
	// ratio is noise. A precision guard, not owner policy.
	impliedRateMaxError = 1e-4
)

func fundsRequest(header *trdcommon.TrdHeader, firm string, refresh bool) *trdgetfunds.Request {
	c2s := &trdgetfunds.C2S{Header: header, RefreshCache: new(refresh)}
	if currency, ok := firmCurrency[firm]; ok {
		c2s.Currency = new(int32(currency))
	} else if currency, ok := marketCurrency[header.GetTrdMarket()]; ok {
		c2s.Currency = new(int32(currency))
	}
	return &trdgetfunds.Request{C2S: c2s}
}

// Funds reads the pinned account's funds, in the currency of its first
// authorised market. Without refresh OpenD answers from its own cache, which
// its server keeps current; that read costs no pacing. With refresh it asks
// its server and spends one request of the 30-second window.
func (c *Connector) Funds(ctx context.Context, refresh bool) (Funds, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return Funds{}, err
	}
	if len(headers) == 0 {
		return Funds{}, fmt.Errorf("%w: the account list names no market", ErrMarketNotAuthorised)
	}
	health := c.Health()
	if _, known := firmCurrency[health.Firm]; !known && health.Environment == EnvironmentReal {
		return Funds{}, fmt.Errorf("%w: %s", ErrUnknownFirm, health.Firm)
	}
	return readFunds(ctx, conn, headers[0], health.Firm, refresh)
}

// ImpliedRates works out, for each currency, how many base units one unit is worth:
// the account's total in base over its total in that currency, both OpenD's own
// conversion. OpenD offers no forex quote. The base is read again last and a pass
// whose base total or cash moved gives no rates, so both sides of a ratio come from
// one funds state. A currency whose totals are absent or too small to divide
// precisely has no rate, which its reader takes as not measured.
func (c *Connector) ImpliedRates(ctx context.Context, base string, currencies []string) (map[string]float64, error) {
	conn, headers, err := c.tradeHeaders()
	if err != nil {
		return nil, err
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("%w: the account list names no market", ErrMarketNotAuthorised)
	}
	read := func(currency string) (Funds, error) {
		return readFundsIn(ctx, conn, headers[0], currency)
	}
	before, err := read(base)
	if err != nil {
		return nil, err
	}
	totals := make(map[string]float64, len(currencies))
	for _, currency := range currencies {
		if currency == base {
			continue
		}
		funds, err := read(currency)
		if err != nil {
			return nil, err
		}
		if funds.Currency == currency {
			totals[currency] = funds.TotalAssets
		}
	}
	after, err := read(base)
	if err != nil {
		return nil, err
	}
	rates := map[string]float64{base: 1}
	if before.TotalAssets != after.TotalAssets || !slices.EqualFunc(before.CashRows, after.CashRows, sameCash) {
		return rates, nil
	}
	for currency, total := range totals {
		if rate, ok := impliedRate(before.TotalAssets, total); ok {
			rates[currency] = rate
		}
	}
	return rates, nil
}

// impliedRate is base over other, refused when either is not a positive finite
// total or when rounding could move the result past impliedRateMaxError.
func impliedRate(base, other float64) (float64, bool) {
	if !(base > 0) || !(other > 0) || math.IsInf(base, 0) || math.IsInf(other, 0) {
		return 0, false
	}
	if fundsRoundingUnit/base+fundsRoundingUnit/other > impliedRateMaxError {
		return 0, false
	}
	return base / other, true
}

func sameCash(a, b CashRow) bool {
	same := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Currency == b.Currency && same(a.Cash, b.Cash)
}

// readFundsIn reads the funds in one currency, from OpenD's cache.
func readFundsIn(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, currency string) (Funds, error) {
	value, known := trdcommon.Currency_value["Currency_"+currency]
	if !known {
		return Funds{}, fmt.Errorf("%w: currency %q", ErrFrame, currency)
	}
	req := fundsRequest(header, "", false)
	req.C2S.Currency = new(value)
	return decodeFundsReply(ctx, conn, req, false)
}

func readFunds(ctx context.Context, conn *connection, header *trdcommon.TrdHeader, firm string, refresh bool) (Funds, error) {
	return decodeFundsReply(ctx, conn, fundsRequest(header, firm, refresh), refresh)
}

func decodeFundsReply(ctx context.Context, conn *connection, req *trdgetfunds.Request, refresh bool) (Funds, error) {
	var resp trdgetfunds.Response
	var err error
	if refresh {
		err = conn.request(ctx, ProtoTrdGetFunds, req, &resp)
	} else {
		err = conn.requestUnpaced(ctx, ProtoTrdGetFunds, req, &resp)
	}
	if err != nil {
		return Funds{}, err
	}
	row := resp.GetS2C().GetFunds()
	if row == nil {
		return Funds{}, fmt.Errorf("%w: Trd_GetFunds reply carries no funds", ErrFrame)
	}
	currency := row.GetCurrency()
	if row.Currency == nil {
		currency = req.GetC2S().GetCurrency()
	}
	funds := Funds{
		Currency:            currencyName(currency),
		Power:               row.GetPower(),
		TotalAssets:         row.GetTotalAssets(),
		Cash:                row.GetCash(),
		MarketValue:         row.GetMarketVal(),
		FrozenCash:          row.GetFrozenCash(),
		AvailableWithdrawal: row.GetAvlWithdrawalCash(),
		AvailableFunds:      row.AvailableFunds,
		InitialMargin:       row.InitialMargin,
		MaintenanceMargin:   row.MaintenanceMargin,
		UnrealizedPnL:       row.UnrealizedPL,
		RealizedPnL:         row.RealizedPL,
		NetCashPower:        row.NetCashPower,
		LongMarketValue:     row.LongMv,
		ShortMarketValue:    row.ShortMv,
		MarginCallMargin:    row.MarginCallMargin,
	}
	if row.RiskStatus != nil {
		funds.RiskStatus = strings.TrimPrefix(trdcommon.CltRiskStatus(row.GetRiskStatus()).String(), "CltRiskStatus_")
	}
	for _, cash := range row.GetCashInfoList() {
		funds.CashRows = append(funds.CashRows, CashRow{
			Currency:         currencyName(cash.GetCurrency()),
			Cash:             cash.Cash,
			AvailableBalance: cash.AvailableBalance,
			NetCashPower:     cash.NetCashPower,
		})
	}
	return funds, nil
}
