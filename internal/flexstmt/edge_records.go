package flexstmt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SectionCoverage reports what the retained XML actually carried. Present is
// about the section container, not whether the period happened to have rows.
// ObservedFields is diagnostic evidence, not a substitute for the canonical
// query manifest.
type SectionCoverage struct {
	Key            string
	Present        bool
	RowCount       int
	ObservedFields []string
	// MissingFields is populated only when at least one row exists. An empty
	// section proves the section was selected, but its XML cannot prove which
	// field columns the IBKR query retained.
	MissingFields []string
}

// Trade is one execution-level Flex trade row. Broker identifiers remain
// internal evidence and are never public API fields.
type Trade struct {
	RecordID           string
	AccountID          string
	ConID              int64
	UnderlyingConID    int64
	Symbol             string
	UnderlyingSymbol   string
	AssetClass         string
	Currency           string
	FXRateToBase       *float64
	TradeID            string
	OrderID            string
	ExecutionID        string
	TransactionID      string
	ExecutedAt         time.Time
	ReportDate         time.Time
	Side               string
	Quantity           *float64
	Price              *float64
	Multiplier         *float64
	Proceeds           *float64
	Commission         *float64
	CommissionCurrency string
	Taxes              *float64
	OpenClose          string
	CostBasis          *float64
	RealizedPNL        *float64
	MTMPNL             *float64
	ClosePrice         *float64
	NetCash            *float64
	LevelOfDetail      string
}

// Instrument is canonical contract metadata keyed by IBKR ConID.
type Instrument struct {
	RecordID         string
	ConID            int64
	UnderlyingConID  int64
	Symbol           string
	UnderlyingSymbol string
	AssetClass       string
	Currency         string
	ListingExchange  string
	Multiplier       *float64
	Strike           *float64
	Expiry           string
	PutCall          string
	ReportDate       time.Time
}

// OpenPosition is one statement-date exact-contract position anchor.
type OpenPosition struct {
	RecordID         string
	AccountID        string
	ConID            int64
	UnderlyingConID  int64
	Symbol           string
	UnderlyingSymbol string
	AssetClass       string
	Currency         string
	FXRateToBase     *float64
	ReportDate       time.Time
	Quantity         *float64
	Multiplier       *float64
	MarkPrice        *float64
	CostBasisPrice   *float64
	CostBasisMoney   *float64
	UnrealizedPNL    *float64
	Side             string
	OpenDate         time.Time
	LevelOfDetail    string
}

// OptionEvent is one broker-reported exercise, assignment, expiration, or
// related underlying delivery row.
type OptionEvent struct {
	RecordID         string
	AccountID        string
	ConID            int64
	UnderlyingConID  int64
	Symbol           string
	UnderlyingSymbol string
	AssetClass       string
	Currency         string
	FXRateToBase     *float64
	Date             time.Time
	TransactionType  string
	Quantity         *float64
	TradePrice       *float64
	MarkPrice        *float64
	Proceeds         *float64
	CommissionTax    *float64
	CostBasis        *float64
	RealizedPNL      *float64
	MTMPNL           *float64
	TradeID          string
}

// CorporateAction is one exact-contract broker corporate-action row. Quantity
// is the only field the position replay treats as a path mutation.
type CorporateAction struct {
	RecordID         string
	AccountID        string
	ConID            int64
	UnderlyingConID  int64
	Symbol           string
	UnderlyingSymbol string
	AssetClass       string
	Currency         string
	FXRateToBase     *float64
	Multiplier       *float64
	ReportDate       time.Time
	Date             time.Time
	Quantity         *float64
	Proceeds         *float64
	Amount           *float64
	RealizedPNL      *float64
	MTMPNL           *float64
	Type             string
	TransactionID    string
}

// FXRate is one dated broker conversion rate. Rate converts FromCurrency into
// ToCurrency exactly as stated by IBKR.
type FXRate struct {
	RecordID     string
	Date         time.Time
	FromCurrency string
	ToCurrency   string
	Rate         *float64
}

type xmlEdgeResponse struct {
	Statements []xmlEdgeStatement `xml:"FlexStatements>FlexStatement"`
}

type xmlEdgeStatement struct {
	AccountID        string               `xml:"accountId,attr"`
	Trades           []xmlTrade           `xml:"Trades>Trade"`
	Instruments      []xmlInstrument      `xml:"SecuritiesInfo>SecurityInfo"`
	Positions        []xmlOpenPosition    `xml:"OpenPositions>OpenPosition"`
	OptionEvents     []xmlOptionEvent     `xml:"OptionEAE>OptionEAE"`
	CorporateActions []xmlCorporateAction `xml:"CorporateActions>CorporateAction"`
	FXRates          []xmlFXRate          `xml:"ConversionRates>ConversionRate"`
}

type xmlTrade struct {
	AccountID, ConID, UnderlyingConID, Symbol, UnderlyingSymbol string
	AssetClass, Currency, FXRateToBase, TradeID, OrderID        string
	ExecutionID, TransactionID, DateTime, TradeDate, TradeTime  string
	ReportDate, Side, Quantity, Price, Multiplier, Proceeds     string
	Commission, CommissionCurrency, Taxes, OpenClose, CostBasis string
	RealizedPNL, MTMPNL, ClosePrice, NetCash, LevelOfDetail     string
}

func (v *xmlTrade) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	attrs := attributes(start)
	v.AccountID = attrs["accountId"]
	v.ConID = attrs["conid"]
	v.UnderlyingConID = attrs["underlyingConid"]
	v.Symbol = attrs["symbol"]
	v.UnderlyingSymbol = attrs["underlyingSymbol"]
	v.AssetClass = attrs["assetCategory"]
	v.Currency = attrs["currency"]
	v.FXRateToBase = attrs["fxRateToBase"]
	v.TradeID = attrs["tradeID"]
	v.OrderID = firstNonEmpty(attrs["ibOrderID"], attrs["orderID"])
	v.ExecutionID = firstNonEmpty(attrs["ibExecID"], attrs["execID"])
	v.TransactionID = attrs["transactionID"]
	v.DateTime = attrs["dateTime"]
	v.TradeDate = attrs["tradeDate"]
	v.TradeTime = attrs["tradeTime"]
	v.ReportDate = attrs["reportDate"]
	v.Side = attrs["buySell"]
	v.Quantity = attrs["quantity"]
	v.Price = attrs["tradePrice"]
	v.Multiplier = attrs["multiplier"]
	v.Proceeds = attrs["proceeds"]
	v.Commission = firstNonEmpty(attrs["IBCommission"], attrs["ibCommission"])
	v.CommissionCurrency = firstNonEmpty(attrs["IBCommissionCurrency"], attrs["ibCommissionCurrency"])
	v.Taxes = attrs["taxes"]
	v.OpenClose = attrs["openCloseIndicator"]
	v.CostBasis = attrs["cost"]
	v.RealizedPNL = attrs["fifoPnlRealized"]
	v.MTMPNL = attrs["mtmPnl"]
	v.ClosePrice = attrs["closePrice"]
	v.NetCash = attrs["netCash"]
	v.LevelOfDetail = attrs["levelOfDetail"]
	return d.Skip()
}

type xmlInstrument struct {
	ConID, UnderlyingConID, Symbol, UnderlyingSymbol, AssetClass string
	Currency, ListingExchange, Multiplier, Strike, Expiry        string
	PutCall, ReportDate                                          string
}

func (v *xmlInstrument) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	a := attributes(start)
	v.ConID, v.UnderlyingConID = a["conid"], a["underlyingConid"]
	v.Symbol, v.UnderlyingSymbol = a["symbol"], a["underlyingSymbol"]
	v.AssetClass, v.Currency = a["assetCategory"], a["currency"]
	v.ListingExchange, v.Multiplier = a["listingExchange"], a["multiplier"]
	v.Strike, v.Expiry, v.PutCall, v.ReportDate = a["strike"], a["expiry"], a["putCall"], a["reportDate"]
	return d.Skip()
}

type xmlOpenPosition struct {
	AccountID, ConID, UnderlyingConID, Symbol, UnderlyingSymbol string
	AssetClass, Currency, FXRateToBase, ReportDate, Quantity    string
	Multiplier, MarkPrice, CostBasisPrice, CostBasisMoney       string
	UnrealizedPNL, Side, OpenDate, LevelOfDetail                string
}

func (v *xmlOpenPosition) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	a := attributes(start)
	v.AccountID, v.ConID, v.UnderlyingConID = a["accountId"], a["conid"], a["underlyingConid"]
	v.Symbol, v.UnderlyingSymbol = a["symbol"], a["underlyingSymbol"]
	v.AssetClass, v.Currency, v.FXRateToBase = a["assetCategory"], a["currency"], a["fxRateToBase"]
	v.ReportDate, v.Quantity, v.Multiplier = a["reportDate"], firstNonEmpty(a["position"], a["quantity"]), a["multiplier"]
	v.MarkPrice, v.CostBasisPrice, v.CostBasisMoney = a["markPrice"], a["costBasisPrice"], a["costBasisMoney"]
	v.UnrealizedPNL, v.Side = a["fifoPnlUnrealized"], a["side"]
	v.OpenDate, v.LevelOfDetail = a["openDateTime"], a["levelOfDetail"]
	return d.Skip()
}

type xmlOptionEvent struct {
	AccountID, ConID, UnderlyingConID, Symbol, UnderlyingSymbol string
	AssetClass, Currency, FXRateToBase, Date, TransactionType   string
	Quantity, TradePrice, MarkPrice, Proceeds, CommissionTax    string
	CostBasis, RealizedPNL, MTMPNL, TradeID                     string
}

func (v *xmlOptionEvent) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	a := attributes(start)
	v.AccountID, v.ConID, v.UnderlyingConID = a["accountId"], a["conid"], a["underlyingConid"]
	v.Symbol, v.UnderlyingSymbol = a["symbol"], a["underlyingSymbol"]
	v.AssetClass, v.Currency, v.FXRateToBase = a["assetCategory"], a["currency"], a["fxRateToBase"]
	v.Date, v.TransactionType, v.Quantity = a["date"], a["transactionType"], a["quantity"]
	v.TradePrice, v.MarkPrice, v.Proceeds = a["tradePrice"], a["markPrice"], a["proceeds"]
	// IBKR's current OptionEAE wire attribute is misspelled with one "s".
	// Keep the corrected variants as parse-only aliases in case the broker
	// repairs the spelling without changing the Portal label.
	v.CommissionTax = firstNonEmpty(a["commisionsAndTax"], a["commissionsAndTax"], a["commissionAndTax"])
	v.CostBasis = firstNonEmpty(a["costBasis"], a["cost"])
	v.RealizedPNL = firstNonEmpty(a["realizedPnl"], a["fifoPnlRealized"])
	v.MTMPNL, v.TradeID = a["mtmPnl"], a["tradeID"]
	return d.Skip()
}

type xmlCorporateAction struct {
	AccountID, ConID, UnderlyingConID, Symbol, UnderlyingSymbol string
	AssetClass, Currency, FXRateToBase, Multiplier, ReportDate  string
	Date, Quantity, Proceeds, Amount, RealizedPNL, MTMPNL       string
	Type, TransactionID                                         string
}

func (v *xmlCorporateAction) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	a := attributes(start)
	v.AccountID, v.ConID, v.UnderlyingConID = a["accountId"], a["conid"], a["underlyingConid"]
	v.Symbol, v.UnderlyingSymbol = a["symbol"], a["underlyingSymbol"]
	v.AssetClass, v.Currency, v.FXRateToBase = a["assetCategory"], a["currency"], a["fxRateToBase"]
	v.Multiplier, v.ReportDate, v.Date = a["multiplier"], a["reportDate"], firstNonEmpty(a["dateTime"], a["date"])
	v.Quantity, v.Proceeds, v.Amount = a["quantity"], a["proceeds"], a["amount"]
	v.RealizedPNL, v.MTMPNL, v.Type, v.TransactionID = a["fifoPnlRealized"], a["mtmPnl"], a["type"], a["transactionID"]
	return d.Skip()
}

type xmlFXRate struct {
	Date, FromCurrency, ToCurrency, Rate string
}

func (v *xmlFXRate) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	a := attributes(start)
	v.Date, v.FromCurrency, v.ToCurrency, v.Rate = firstNonEmpty(a["dateTime"], a["reportDate"]), a["fromCurrency"], a["toCurrency"], a["rate"]
	return d.Skip()
}

func parseEdgeRecords(data []byte, statements []Statement) error {
	var doc xmlEdgeResponse
	if err := xml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse edge flex records: %w", err)
	}
	if len(doc.Statements) != len(statements) {
		return fmt.Errorf("edge flex statement count mismatch")
	}
	coverage, err := inspectCoverage(data)
	if err != nil {
		return err
	}
	for i, raw := range doc.Statements {
		st := &statements[i]
		st.ManifestVersion = ManifestVersion
		st.Coverage = cloneCoverage(coverage)
		if err := parseTrades(raw, st); err != nil {
			return err
		}
		if err := parseInstruments(raw, st); err != nil {
			return err
		}
		if err := parsePositions(raw, st); err != nil {
			return err
		}
		if err := parseOptionEvents(raw, st); err != nil {
			return err
		}
		if err := parseCorporateActions(raw, st); err != nil {
			return err
		}
		if err := parseFXRates(raw, st); err != nil {
			return err
		}
	}
	return nil
}

func parseTrades(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.Trades {
		conid, err := optionalInt64("trade conid", v.ConID)
		if err != nil {
			return err
		}
		underlying, err := optionalInt64("trade underlying conid", v.UnderlyingConID)
		if err != nil {
			return err
		}
		executed, err := parseExecutionTime(v.DateTime, v.TradeDate, v.TradeTime)
		if err != nil {
			return fmt.Errorf("trade %d execution time: %w", n, err)
		}
		reportDate, err := optionalDate(v.ReportDate)
		if err != nil {
			return fmt.Errorf("trade %d report date: %w", n, err)
		}
		trade := Trade{
			AccountID: firstNonEmpty(v.AccountID, raw.AccountID, st.AccountID), ConID: conid, UnderlyingConID: underlying,
			Symbol: clean(v.Symbol), UnderlyingSymbol: clean(v.UnderlyingSymbol), AssetClass: upper(v.AssetClass), Currency: upper(v.Currency),
			TradeID: clean(v.TradeID), OrderID: clean(v.OrderID), ExecutionID: clean(v.ExecutionID), TransactionID: clean(v.TransactionID),
			ExecutedAt: executed, ReportDate: reportDate, Side: upper(v.Side), CommissionCurrency: upper(v.CommissionCurrency),
			OpenClose: upper(v.OpenClose), LevelOfDetail: upper(v.LevelOfDetail),
		}
		for label, pair := range map[string]struct {
			raw string
			dst **float64
		}{
			"fx rate": {v.FXRateToBase, &trade.FXRateToBase}, "quantity": {v.Quantity, &trade.Quantity}, "price": {v.Price, &trade.Price},
			"multiplier": {v.Multiplier, &trade.Multiplier}, "proceeds": {v.Proceeds, &trade.Proceeds}, "commission": {v.Commission, &trade.Commission},
			"taxes": {v.Taxes, &trade.Taxes}, "cost basis": {v.CostBasis, &trade.CostBasis}, "realized pnl": {v.RealizedPNL, &trade.RealizedPNL},
			"mtm pnl": {v.MTMPNL, &trade.MTMPNL}, "close price": {v.ClosePrice, &trade.ClosePrice}, "net cash": {v.NetCash, &trade.NetCash},
		} {
			value, err := optionalFloat("trade "+label, pair.raw)
			if err != nil {
				return fmt.Errorf("trade %d: %w", n, err)
			}
			*pair.dst = value
		}
		if identity := firstNonEmpty(trade.ExecutionID, trade.TradeID, trade.TransactionID); identity != "" {
			trade.RecordID = brokerRecordID("trade", identity)
		} else {
			trade.RecordID = brokerRecordID("trade", strconv.FormatInt(trade.ConID, 10), trade.ExecutedAt.Format(time.RFC3339Nano), trade.Side, floatText(trade.Quantity), floatText(trade.Price))
		}
		st.Trades = append(st.Trades, trade)
	}
	return nil
}

func parseInstruments(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.Instruments {
		conid, err := optionalInt64("instrument conid", v.ConID)
		if err != nil {
			return err
		}
		underlying, err := optionalInt64("instrument underlying conid", v.UnderlyingConID)
		if err != nil {
			return err
		}
		reportDate, err := optionalDate(v.ReportDate)
		if err != nil {
			return err
		}
		item := Instrument{ConID: conid, UnderlyingConID: underlying, Symbol: clean(v.Symbol), UnderlyingSymbol: clean(v.UnderlyingSymbol), AssetClass: upper(v.AssetClass), Currency: upper(v.Currency), ListingExchange: upper(v.ListingExchange), Expiry: clean(v.Expiry), PutCall: upper(v.PutCall), ReportDate: reportDate}
		if item.Multiplier, err = optionalFloat("instrument multiplier", v.Multiplier); err != nil {
			return fmt.Errorf("instrument %d: %w", n, err)
		}
		if item.Strike, err = optionalFloat("instrument strike", v.Strike); err != nil {
			return fmt.Errorf("instrument %d: %w", n, err)
		}
		item.RecordID = brokerRecordID("instrument", strconv.FormatInt(item.ConID, 10), item.ReportDate.Format("2006-01-02"), item.Symbol, item.AssetClass)
		st.Instruments = append(st.Instruments, item)
	}
	return nil
}

func parsePositions(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.Positions {
		conid, err := optionalInt64("position conid", v.ConID)
		if err != nil {
			return err
		}
		underlying, err := optionalInt64("position underlying conid", v.UnderlyingConID)
		if err != nil {
			return err
		}
		reportDate, err := optionalDate(v.ReportDate)
		if err != nil {
			return fmt.Errorf("position %d report date: %w", n, err)
		}
		openDate, err := optionalDate(v.OpenDate)
		if err != nil {
			return fmt.Errorf("position %d open date: %w", n, err)
		}
		item := OpenPosition{AccountID: firstNonEmpty(v.AccountID, raw.AccountID, st.AccountID), ConID: conid, UnderlyingConID: underlying, Symbol: clean(v.Symbol), UnderlyingSymbol: clean(v.UnderlyingSymbol), AssetClass: upper(v.AssetClass), Currency: upper(v.Currency), ReportDate: reportDate, Side: upper(v.Side), OpenDate: openDate, LevelOfDetail: upper(v.LevelOfDetail)}
		for label, pair := range map[string]struct {
			raw string
			dst **float64
		}{
			"fx rate": {v.FXRateToBase, &item.FXRateToBase}, "quantity": {v.Quantity, &item.Quantity}, "multiplier": {v.Multiplier, &item.Multiplier},
			"mark price": {v.MarkPrice, &item.MarkPrice}, "cost basis price": {v.CostBasisPrice, &item.CostBasisPrice}, "cost basis money": {v.CostBasisMoney, &item.CostBasisMoney}, "unrealized pnl": {v.UnrealizedPNL, &item.UnrealizedPNL},
		} {
			value, err := optionalFloat("position "+label, pair.raw)
			if err != nil {
				return fmt.Errorf("position %d: %w", n, err)
			}
			*pair.dst = value
		}
		item.RecordID = brokerRecordID("position", item.AccountID, strconv.FormatInt(item.ConID, 10), item.ReportDate.Format("2006-01-02"), item.LevelOfDetail)
		st.Positions = append(st.Positions, item)
	}
	return nil
}

func parseOptionEvents(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.OptionEvents {
		conid, err := optionalInt64("option event conid", v.ConID)
		if err != nil {
			return err
		}
		underlying, err := optionalInt64("option event underlying conid", v.UnderlyingConID)
		if err != nil {
			return err
		}
		date, err := optionalDate(v.Date)
		if err != nil {
			return fmt.Errorf("option event %d date: %w", n, err)
		}
		item := OptionEvent{AccountID: firstNonEmpty(v.AccountID, raw.AccountID, st.AccountID), ConID: conid, UnderlyingConID: underlying, Symbol: clean(v.Symbol), UnderlyingSymbol: clean(v.UnderlyingSymbol), AssetClass: upper(v.AssetClass), Currency: upper(v.Currency), Date: date, TransactionType: upper(v.TransactionType), TradeID: clean(v.TradeID)}
		for label, pair := range map[string]struct {
			raw string
			dst **float64
		}{
			"fx rate": {v.FXRateToBase, &item.FXRateToBase}, "quantity": {v.Quantity, &item.Quantity}, "trade price": {v.TradePrice, &item.TradePrice}, "mark price": {v.MarkPrice, &item.MarkPrice}, "proceeds": {v.Proceeds, &item.Proceeds}, "commission and tax": {v.CommissionTax, &item.CommissionTax}, "cost basis": {v.CostBasis, &item.CostBasis}, "realized pnl": {v.RealizedPNL, &item.RealizedPNL}, "mtm pnl": {v.MTMPNL, &item.MTMPNL},
		} {
			value, err := optionalFloat("option event "+label, pair.raw)
			if err != nil {
				return fmt.Errorf("option event %d: %w", n, err)
			}
			*pair.dst = value
		}
		if item.TradeID != "" {
			item.RecordID = brokerRecordID("option-event", item.TradeID)
		} else {
			item.RecordID = brokerRecordID("option-event", strconv.FormatInt(item.ConID, 10), item.Date.Format(time.RFC3339Nano), item.TransactionType, floatText(item.Quantity))
		}
		st.OptionEvents = append(st.OptionEvents, item)
	}
	return nil
}

func parseCorporateActions(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.CorporateActions {
		conid, err := optionalInt64("corporate action conid", v.ConID)
		if err != nil {
			return err
		}
		underlying, err := optionalInt64("corporate action underlying conid", v.UnderlyingConID)
		if err != nil {
			return err
		}
		reportDate, err := optionalDate(v.ReportDate)
		if err != nil {
			return err
		}
		date, err := optionalDate(v.Date)
		if err != nil {
			return fmt.Errorf("corporate action %d date: %w", n, err)
		}
		item := CorporateAction{AccountID: firstNonEmpty(v.AccountID, raw.AccountID, st.AccountID), ConID: conid, UnderlyingConID: underlying, Symbol: clean(v.Symbol), UnderlyingSymbol: clean(v.UnderlyingSymbol), AssetClass: upper(v.AssetClass), Currency: upper(v.Currency), ReportDate: reportDate, Date: date, Type: upper(v.Type), TransactionID: clean(v.TransactionID)}
		for label, pair := range map[string]struct {
			raw string
			dst **float64
		}{
			"fx rate": {v.FXRateToBase, &item.FXRateToBase}, "multiplier": {v.Multiplier, &item.Multiplier}, "quantity": {v.Quantity, &item.Quantity}, "proceeds": {v.Proceeds, &item.Proceeds}, "amount": {v.Amount, &item.Amount}, "realized pnl": {v.RealizedPNL, &item.RealizedPNL}, "mtm pnl": {v.MTMPNL, &item.MTMPNL},
		} {
			value, err := optionalFloat("corporate action "+label, pair.raw)
			if err != nil {
				return fmt.Errorf("corporate action %d: %w", n, err)
			}
			*pair.dst = value
		}
		if item.TransactionID != "" {
			item.RecordID = brokerRecordID("corporate-action", item.TransactionID)
		} else {
			item.RecordID = brokerRecordID("corporate-action", strconv.FormatInt(item.ConID, 10), item.Date.Format(time.RFC3339Nano), item.Type, floatText(item.Quantity))
		}
		st.CorporateActions = append(st.CorporateActions, item)
	}
	return nil
}

func parseFXRates(raw xmlEdgeStatement, st *Statement) error {
	for n, v := range raw.FXRates {
		date, err := optionalDate(v.Date)
		if err != nil {
			return fmt.Errorf("fx rate %d date: %w", n, err)
		}
		rate, err := optionalFloat("fx rate", v.Rate)
		if err != nil {
			return fmt.Errorf("fx rate %d: %w", n, err)
		}
		item := FXRate{Date: date, FromCurrency: upper(v.FromCurrency), ToCurrency: upper(v.ToCurrency), Rate: rate}
		item.RecordID = brokerRecordID("fx-rate", item.Date.Format(time.RFC3339Nano), item.FromCurrency, item.ToCurrency)
		st.FXRates = append(st.FXRates, item)
	}
	return nil
}

func inspectCoverage(data []byte) ([]SectionCoverage, error) {
	manifest := CanonicalQueryManifest()
	byContainer := make(map[string]int, len(manifest))
	byRow := make(map[string][]int, len(manifest))
	coverage := make([]SectionCoverage, len(manifest))
	for i, section := range manifest {
		coverage[i].Key = section.Key
		byContainer[section.Container] = i
		byRow[section.Row] = append(byRow[section.Row], i)
	}
	observed := make([]map[string]struct{}, len(manifest))
	for i := range observed {
		observed[i] = make(map[string]struct{})
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("inspect Flex query coverage: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if i, ok := byContainer[start.Name.Local]; ok {
			coverage[i].Present = true
		}
		for _, i := range byRow[start.Name.Local] {
			if len(start.Attr) == 0 {
				continue
			}
			coverage[i].RowCount++
			for _, attr := range start.Attr {
				observed[i][attr.Name.Local] = struct{}{}
			}
		}
	}
	for i := range coverage {
		for field := range observed[i] {
			coverage[i].ObservedFields = append(coverage[i].ObservedFields, field)
		}
		sort.Strings(coverage[i].ObservedFields)
		if coverage[i].RowCount > 0 {
			for _, required := range manifest[i].RequiredFields {
				if _, ok := observed[i][required]; !ok {
					coverage[i].MissingFields = append(coverage[i].MissingFields, required)
				}
			}
		}
	}
	return coverage, nil
}

func cloneCoverage(in []SectionCoverage) []SectionCoverage {
	out := make([]SectionCoverage, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].ObservedFields = append([]string(nil), in[i].ObservedFields...)
		out[i].MissingFields = append([]string(nil), in[i].MissingFields...)
	}
	return out
}

func attributes(start xml.StartElement) map[string]string {
	out := make(map[string]string, len(start.Attr))
	for _, attr := range start.Attr {
		out[attr.Name.Local] = strings.TrimSpace(attr.Value)
	}
	return out
}

func optionalFloat(label, raw string) (*float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not numeric", label, raw)
	}
	if value != value || value > 1.7976931348623157e308 || value < -1.7976931348623157e308 {
		return nil, fmt.Errorf("%s is not finite", label)
	}
	return &value, nil
}

func optionalInt64(label, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not an integer", label, raw)
	}
	return value, nil
}

func optionalDate(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	return parseFlexDate(raw)
}

func parseExecutionTime(dateTime, tradeDate, tradeTime string) (time.Time, error) {
	if strings.TrimSpace(dateTime) != "" {
		return parseFlexDate(dateTime)
	}
	tradeDate = strings.TrimSpace(tradeDate)
	if tradeDate == "" {
		return time.Time{}, fmt.Errorf("missing dateTime and tradeDate")
	}
	tradeTime = strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(tradeTime))
	if tradeTime == "" {
		return parseFlexDate(tradeDate)
	}
	if len(tradeTime) == 4 {
		tradeTime += "00"
	}
	return parseFlexDate(tradeDate + ";" + tradeTime)
}

func brokerRecordID(kind string, values ...string) string {
	h := sha256.New()
	h.Write([]byte("canary.flex.record.v1\x00" + kind))
	for _, value := range values {
		h.Write([]byte{0})
		h.Write([]byte(value))
	}
	return kind + "-synth-" + hex.EncodeToString(h.Sum(nil)[:12])
}

func floatText(v *float64) string {
	if v == nil {
		return "missing"
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

func clean(v string) string { return strings.TrimSpace(v) }
func upper(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
