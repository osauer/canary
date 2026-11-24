package rpc

import "time"

// MethodAddMarket is bounded, read-only discovery for the owner's Add composer.
const MethodAddMarket = "add.market"

// AddMarketParams identifies an underlying and optionally an exact selected call.
type AddMarketParams struct {
	Underlying ContractParams `json:"underlying"`
	// Right picks the side: C, the default, for calls; P for protective
	// puts. Selected and Refresh carry the same right.
	Right    string             `json:"right,omitempty"`
	Selected *ContractParams    `json:"selected,omitempty"`
	Refresh  []AddCallSelection `json:"refresh,omitempty"`
	Discover bool               `json:"discover,omitempty"`
}

// AddCallSelection refreshes one previously resolved horizon without re-ranking.
type AddCallSelection struct {
	Horizon  int            `json:"horizon_days"`
	Contract ContractParams `json:"contract"`
}

// AddCallCandidate is a horizon and its eligible exact call, or its absence.
type AddCallCandidate struct {
	Horizon         int                         `json:"horizon_days"`
	DTE             int                         `json:"dte,omitempty"`
	Contract        *ContractParams             `json:"contract,omitempty"`
	Quote           *SetupOptionQuote           `json:"quote,omitempty"`
	PriceIncrements []SetupOptionPriceIncrement `json:"price_increments,omitempty"`
	Unavailable     string                      `json:"unavailable,omitempty"`
	Planning        *AddCallPlanning            `json:"planning,omitempty"`
	// DeltaBand is the owner's delta range for this horizon; Desk reads it
	// rather than keeping its own copy.
	DeltaBand *AddDeltaBand `json:"delta_band,omitempty"`
	// HedgeRoomContracts is, for a put on a hedge symbol, the contracts of
	// this row that still fit under rule 12's over-hedge line.
	HedgeRoomContracts *int `json:"hedge_room_contracts,omitempty"`
	// UnansweredStrikes are the strikes quoted beside this row whose quotes
	// had not answered when the round stopped waiting (one second after its
	// first usable answer): the row was chosen without them, and one of them
	// might have been nearer the delta target.
	UnansweredStrikes []float64 `json:"unanswered_strikes,omitempty"`
}

// AddDeltaBand bounds a suggested option's live delta, inclusive, and names
// the delta selection aims at. A put's band is written without its sign; its
// quote's delta keeps the broker's sign.
type AddDeltaBand struct {
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Target float64 `json:"target"`
}

// AddPutCover is what a protective put may still cover: the shares held
// long, the shares the long puts held cover, and the contracts of 100 shares
// left after working put purchases and share sales reserve their units.
// It is unavailable when current holdings or commitments cannot be read.
//
// A put on a Rulebook hedge symbol protects the whole long book instead
// (Kind index_protection): HeldShares and CoveredShares are then zero,
// RoomContracts is the most contracts any listed put may still add, each
// row's own in AddCallCandidate.HedgeRoomContracts, and IndexProtection
// carries rule 12's measurement.
type AddPutCover struct {
	Kind            string              `json:"kind"` // shares | index_protection
	HeldShares      float64             `json:"held_shares"`
	CoveredShares   float64             `json:"covered_shares"`
	RoomContracts   int                 `json:"room_contracts"`
	IndexProtection *AddIndexProtection `json:"index_protection,omitempty"`
}

// Put cover kinds.
const (
	AddPutCoverShares          = "shares"
	AddPutCoverIndexProtection = "index_protection"
)

// AddIndexProtection is rule 12's measurement of the book an index put
// protects, in base currency: the long book (delta-adjusted gross long
// exposure), the short delta the held hedge puts give, the band top and the
// over-hedge line past which more puts count as risk, and the short delta
// still fitting under that line. WorkingContracts are working put buys on
// the symbol, charged against each row's room. When the line cannot be
// measured, Unmeasured says why and the room is the option cap.
type AddIndexProtection struct {
	Unmeasured       string  `json:"unmeasured,omitempty"`
	BaseCurrency     string  `json:"base_currency,omitempty"`
	LongBase         float64 `json:"long_base"`
	ProtectionBase   float64 `json:"protection_base"`
	RatioPct         float64 `json:"ratio_pct"`
	BandTopPct       float64 `json:"band_top_pct"`
	LinePct          float64 `json:"line_pct"`
	RoomBase         float64 `json:"room_base"`
	WorkingContracts int     `json:"working_contracts,omitempty"`
}

// AddCallPlanning is closed-session context, never live selection or order
// evidence. ObservedAt is when the source was received, not an exchange time;
// a broker midpoint receipt never acquires a session date by inference.
type AddCallPlanning struct {
	Price       *float64  `json:"price,omitempty"`
	PriceSource string    `json:"price_source,omitempty"` // broker_midpoint
	ObservedAt  time.Time `json:"observed_at,omitzero"`
	DeltaStatus string    `json:"delta_status"` // in_range | unavailable | out_of_range
}

// AddUnderlying is the resolved stock with the broker's lot: an order's
// shares are at least MinSize and a multiple of SizeIncrement, each 1 where
// the broker gives none.
type AddUnderlying struct {
	ContractParams
	MinSize       int `json:"min_size"`
	SizeIncrement int `json:"size_increment"`
}

// AddMarketResult contains observations, never order authority or risk capacity.
type AddMarketResult struct {
	Version         int                         `json:"version"`
	Underlying      AddUnderlying               `json:"underlying"`
	Quote           *Quote                      `json:"quote,omitempty"`
	PriceIncrements []SetupOptionPriceIncrement `json:"price_increments,omitempty"`
	Calls           []AddCallCandidate          `json:"calls,omitempty"`
	// Puts replace Calls for right P, in the same row shape.
	Puts []AddCallCandidate `json:"puts,omitempty"`
	// PutCover is present for right P when the book can be measured.
	PutCover           *AddPutCover      `json:"put_cover,omitempty"`
	Selected           *AddCallCandidate `json:"selected,omitempty"`
	AsOf               time.Time         `json:"as_of"`
	OptionsSession     MarketSession     `json:"options_session"`
	OptionsLastSession *MarketSession    `json:"options_last_session,omitempty"`
	OptionMode         string            `json:"option_mode"` // live | planning | unavailable
	// OptionCap is the option contracts one order may carry under the
	// order limits in force; 0 when they cannot be read.
	OptionCap int `json:"option_cap,omitempty"`
}
