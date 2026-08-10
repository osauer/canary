package ibkr

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// IBKROrder is the mutable, low-level order and contract representation used by
// Connection order-write and WhatIf methods. It is a wire request and local
// observation, not proof that IBKR accepted, filled, cancelled, or finalized an
// order.
//
// Prices use the contract's quote currency and quantities use the broker's
// instrument units (shares for stock and contracts for options). Most optional
// numeric fields use zero to mean unspecified. Validation and sending may fill
// OrderID, ClientID, Account, OpenClose, and TIF and may update WhatIf,
// Transmit, Status, Remaining, and timestamp fields in place.
type IBKROrder struct {
	OrderID  int    // OrderID is the session-scoped broker order ID; zero requests allocation.
	ClientID int    // ClientID is the TWS API client ID; zero uses the connection's configured ID.
	PermID   int    // PermID is IBKR's permanent order ID; zero means not observed.
	Account  string // Account is the target broker account; empty uses connection account data.

	// Contract details
	Symbol       string
	SecType      string
	ConID        int
	Exchange     string
	Currency     string
	Expiry       string
	Strike       float64 // Strike is in the contract's quote currency; zero means unspecified.
	Right        string
	Multiplier   string // Multiplier is the broker's decimal string, such as "100" for many options.
	PrimaryExch  string
	LocalSymbol  string
	TradingClass string
	SecIDType    string
	SecID        string

	// Order details
	Action    string  // Action must be BUY or SELL.
	TotalQty  int     // TotalQty is a positive number of shares or contracts.
	OrderType string  // MKT, LMT, STP, etc.
	LmtPrice  float64 // LmtPrice is in quote-currency units; zero means unspecified.
	// LmtPriceSet distinguishes an explicit zero combo price from an omitted
	// price. Single-contract orders continue to require a positive price.
	LmtPriceSet bool
	AuxPrice    float64 // AuxPrice is a stop price or trailing amount; zero means unspecified.
	ComboLegs   []ComboLeg

	// Time in force
	TIF           string // TIF is the broker time-in-force code; empty defaults to DAY.
	OcaGroup      string
	OcaType       int
	OrderRef      string // Our reference
	Transmit      bool   // Transmit is set true by write and WhatIf helpers before encoding.
	WhatIf        bool   // WhatIf is forced true for previews and false for order submission.
	OpenClose     string // OpenClose is O or C; connector helpers default an empty value to O.
	Origin        int
	ParentID      int
	BlockOrder    bool
	SweepToFill   bool
	DisplaySize   int
	TriggerMethod int
	OutsideRth    bool // OutsideRth asks IBKR to permit execution outside regular trading hours.
	Hidden        bool

	// State
	Status        string  // Status is locally assigned or broker-observed and is not finality by itself.
	Filled        int     // Filled is the observed filled quantity; zero may mean none or not observed.
	Remaining     int     // Remaining is the observed open quantity; zero may mean none or not observed.
	AvgFillPrice  float64 // AvgFillPrice is in quote-currency units; zero may mean unavailable.
	LastFillPrice float64 // LastFillPrice is in quote-currency units; zero may mean unavailable.

	// Timestamps
	CreatedTime   time.Time  // CreatedTime is local process time; IsZero reports unknown.
	SubmittedTime time.Time  // SubmittedTime is local socket-write time; IsZero reports unknown.
	FilledTime    *time.Time // FilledTime is nil when no fill time is recorded locally.
	CancelledTime *time.Time // CancelledTime is nil when no cancellation time is recorded locally.

	// Error tracking
	LastError string
	WhyHeld   string

	// Misc optional parameters
	GoodAfterTime                  string
	GoodTillDate                   string
	Rule80A                        string
	SettlingFirm                   string
	AllOrNone                      bool
	MinQty                         int
	PercentOffset                  float64
	ETradeOnly                     bool
	FirmQuoteOnly                  bool
	NbboPriceCap                   float64
	AuctionStrategy                int
	StartingPrice                  float64
	StockRefPrice                  float64
	Delta                          float64
	StockRangeLower                float64
	StockRangeUpper                float64
	OverridePercentageConstraints  bool
	Volatility                     float64
	VolatilityType                 int
	DeltaNeutralOrderType          string
	DeltaNeutralAuxPrice           float64
	DeltaNeutralConID              int
	DeltaNeutralSettlingFirm       string
	DeltaNeutralClearingAccount    string
	DeltaNeutralClearingIntent     string
	DeltaNeutralOpenClose          string
	DeltaNeutralShortSale          bool
	DeltaNeutralShortSaleSlot      int
	DeltaNeutralDesignatedLocation string
	ContinuousUpdate               int
	ReferencePriceType             int
	TrailStopPrice                 float64 // TrailStopPrice is the initial stop price in quote-currency units.
	TrailingPercent                float64 // TrailingPercent is the broker percentage value, not a fraction.
	LmtPriceOffset                 float64 // LmtPriceOffset is the TRAIL LIMIT offset in price units.
	BasisPoints                    float64
	BasisPointsType                int
	ScaleInitLevelSize             int
	ScaleSubsLevelSize             int
	ScalePriceIncrement            float64
	ScalePriceAdjustValue          float64
	ScalePriceAdjustInterval       int
	ScaleProfitOffset              float64
	ScaleAutoReset                 bool
	ScaleInitPosition              int
	ScaleInitFillQty               int
	ScaleRandomPercent             bool
	HedgeType                      string
	HedgeParam                     string
	OptOutSmartRouting             bool
	ClearingAccount                string
	ClearingIntent                 string
	NotHeld                        bool
	ModelCode                      string
	ShortSaleSlot                  int
	DesignatedLocation             string
	ExemptCode                     int
	DiscretionaryAmt               float64
	FaGroup                        string
	FaMethod                       string
	FaPercentage                   string
	FaProfile                      string
}

// ValidateOrder performs local, basic validation of order. It requires a
// non-nil order, symbol, positive quantity, BUY or SELL action, and an order
// type, and validates the price combinations used by LMT, STP, STP LMT, TRAIL,
// and TRAIL LIMIT orders. If TIF is empty, ValidateOrder mutates it to "DAY".
//
// A nil result does not mean the broker will accept the order and does not
// provide submit authority. Connection state, account and contract details,
// encoder support, application policy, and broker eligibility are checked
// elsewhere.
func ValidateOrder(order *IBKROrder) error {
	if order == nil {
		return fmt.Errorf("order is nil")
	}

	if order.Symbol == "" {
		return fmt.Errorf("symbol is required")
	}

	if order.TotalQty <= 0 {
		return fmt.Errorf("quantity must be positive")
	}

	if order.Action != "BUY" && order.Action != "SELL" {
		return fmt.Errorf("action must be BUY or SELL")
	}

	if order.OrderType == "" {
		return fmt.Errorf("order type is required")
	}

	// Validate limit price for limit orders
	if order.OrderType == "LMT" {
		if strings.EqualFold(order.SecType, "BAG") {
			if !order.LmtPriceSet || math.IsNaN(order.LmtPrice) || math.IsInf(order.LmtPrice, 0) {
				return fmt.Errorf("finite explicit limit price required for combo limit orders")
			}
		} else if order.LmtPrice <= 0 {
			return fmt.Errorf("limit price required for limit orders")
		}
	}

	// Validate stop price for stop orders
	if (order.OrderType == "STP" || order.OrderType == "STP LMT") && order.AuxPrice <= 0 {
		return fmt.Errorf("stop price required for stop orders")
	}
	if order.OrderType == "TRAIL" || order.OrderType == "TRAIL LIMIT" {
		if order.TrailStopPrice <= 0 {
			return fmt.Errorf("trail stop price required for trailing stop orders")
		}
		hasAmount := order.AuxPrice > 0
		hasPercent := order.TrailingPercent > 0
		if hasAmount == hasPercent {
			return fmt.Errorf("trailing stop orders require exactly one of aux price or trailing percent")
		}
		if order.OrderType == "TRAIL LIMIT" && order.LmtPriceOffset <= 0 {
			return fmt.Errorf("limit price offset required for trailing stop limit orders")
		}
		if order.OrderType == "TRAIL" && order.LmtPriceOffset != 0 {
			return fmt.Errorf("limit price offset is only supported for trailing stop limit orders")
		}
		if order.LmtPrice != 0 {
			return fmt.Errorf("limit price must not be set on trailing stop orders; use limit price offset for TRAIL LIMIT")
		}
	}

	// Default TIF if not specified
	if order.TIF == "" {
		order.TIF = "DAY"
	}

	return nil
}
