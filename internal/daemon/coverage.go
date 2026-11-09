package daemon

import (
	"slices"

	"github.com/osauer/canary/v2/internal/rpc"
)

// CoverageStatus says how a broker serves a capability.
type CoverageStatus string

// The statuses. Under OpenD dispatch refuses a NotServed capability with
// not_applicable; every other status reaches the handler.
const (
	// Served: it works, and a test or a live check below shows it.
	Served CoverageStatus = "served"
	// Partial: it works with the gap the note names.
	Partial CoverageStatus = "partial"
	// Unverified: it answers, but nothing has confirmed it on this broker.
	Unverified CoverageStatus = "unverified"
	// Refused: it is reached, and the handler refuses or answers unavailable
	// in the words the note gives.
	Refused CoverageStatus = "refused"
	// NotServed: dispatch answers not_applicable; the handler never runs.
	NotServed CoverageStatus = "not_served"
)

// Coverage is one broker's side of a capability.
type Coverage struct {
	Status CoverageStatus
	// Note is the gap, the refusal or the open question, in the reader's words.
	Note string
	// Tests exercise the capability on this broker; each must exist.
	Tests []string
	// Smoke names the live wire-smoke probes that read it from a real
	// gateway (scripts/wire-smoke.sh); each must exist.
	Smoke []string
	// Seen is a check outside the tests and the smoke, as prose with its
	// source, or empty.
	Seen string
}

// Capability is one thing a reader can do with Canary, as each broker serves
// it. Most are Canary's own features computed from what a broker supplies
// (regime, stress, Edge); Needs names those inputs. The routes name their
// capability (routes.go); a capability without methods is work the daemon
// does on its own, and one that needs no input is broker-independent.
type Capability struct {
	Name  string
	Needs []string
	IBKR  Coverage
	OpenD Coverage
}

// Supply says whether a broker supplies an input.
type Supply string

// The supplies.
const (
	Supplied    Supply = "supplied"
	Limited     Supply = "limited"      // supplied within the limit the note names
	NotSupplied Supply = "not_supplied" // the broker has no such data
	ToVerify    Supply = "to_verify"    // not yet known whether the broker supplies it
)

// Input is data a broker supplies and Canary's capabilities read.
type Input struct {
	Name  string
	IBKR  Supply
	OpenD Supply
	// Note is OpenD's side, with its source.
	Note string
}

// futuDocs is the public OpenAPI documentation every OpenD note below that
// names a message Canary has not vendored was read against: the message
// list, each message's page and the quota page, at the stated version.
const futuDocs = "Futu OpenAPI docs v10.12, openapi.futunn.com, read 2026-11-08"

// inputs is what each broker supplies, from the vendored OpenD protocol
// (pkg/futu/internal/pb), the OpenD design (internal-docs/design/broker-opend.md)
// and, for every message not vendored, futuDocs. The owner decided
// (2026-11-08) that each row is settled before a capability is built on it,
// so that nothing is built on an input OpenD lacks; the lookup settled the
// seven rows that were open. A row says what the broker supplies, not what
// Canary reads: a Limited or Supplied input a capability does not use yet is
// a Canary gap.
var inputs = []Input{
	{Name: "session", IBKR: Supplied, OpenD: Supplied},
	{Name: "account and positions", IBKR: Supplied, OpenD: Supplied, Note: "Trd_GetFunds and Trd_GetPositionList"},
	{Name: "intraday P&L", IBKR: Supplied, OpenD: Limited, Note: "today's P&L per position only (Trd_GetPositionList td_plVal, vendored), polled, at most 10 refreshed reads per 30 s per account; Trd_GetFunds carries no account-level daily P&L (its unrealizedPL and realizedPL are futures-only) and there is no P&L stream like IBKR's; " + futuDocs},
	{Name: "quotes", IBKR: Supplied, OpenD: Limited, Note: "only in markets where the login holds the quote right; a subscription quota shared with other clients of the same OpenD"},
	{Name: "price bars", IBKR: Supplied, OpenD: Limited, Note: "a history quota counted over seven days"},
	{Name: "contract lookup", IBKR: Supplied, OpenD: Supplied, Note: "Qot_GetStaticInfo"},
	{Name: "ISIN listings", IBKR: Supplied, OpenD: NotSupplied, Note: "no message takes an ISIN: a security is market plus code and Qot_GetSearchQuote (3262, not vendored) is documented for keywords only; Qot_GetCompanyProfile (3243, not vendored) returns a code's ISIN, the reverse direction, for equities and funds; " + futuDocs},
	{Name: "option chains and Greeks", IBKR: Supplied, OpenD: Limited, Note: "Qot_GetOptionChain (3209, not vendored) lists a chain's contracts without quotes for HK stock, US stock, HSI and HHI underlyings, 30 days of expiries per call, 10 calls per 30 s, no expired chains; implied volatility, delta, gamma, vega, theta and rho per contract come from the vendored snapshot (Qot_GetSecuritySnapshot OptionSnapshotExData) or basic quote (OptionBasicQotExData), and option subscriptions draw on their own quota of 20 to 400 chains by account tier; " + futuDocs},
	{Name: "bonds", IBKR: Supplied, OpenD: NotSupplied, Note: "no quote or trade message serves a bond: the quotation and trading coverage tables list no bond contract in any market and no message page names bonds; SecurityType_Bond is only an enum value and Trd_GetFunds carries one aggregate bondAssets figure; " + futuDocs},
	{Name: "broker statements", IBKR: Supplied, OpenD: NotSupplied, Note: "OpenD has no statement service like IBKR Flex"},
	{Name: "borrow and fee data", IBKR: Supplied, OpenD: Limited, Note: "Trd_GetMarginRatio (2223, not vendored) returns per stock whether it is shortable, the short pool remaining in shares and today's borrow rate (shortFeeRate), for HK and US stocks only, 100 codes per call, 10 calls per 30 s; no rate history; " + futuDocs},
	{Name: "earnings calendar", IBKR: Supplied, OpenD: Limited, Note: "Qot_GetEarningsCalendar (3401, not vendored) lists a market's reporters with date, session, EPS and revenue actuals and estimates, by market, watchlist or positions rather than by symbol, at most seven days per call, 60 calls per 30 s; IBKR's comes from Wall Street Horizon; " + futuDocs},
	{Name: "quoted FX rates", IBKR: Supplied, OpenD: NotSupplied, Note: "no message returns an exchange rate: the quotation coverage table marks Forex as not available to API users and Trd_GetFunds converts into the account currency without exposing the rate; Canary implies a rate from the account's own totals; " + futuDocs},
	{Name: "order placement and preview", IBKR: Supplied, OpenD: Supplied, Note: "Trd_PlaceOrder and Trd_ModifyOrder; the preview is a maximum quantity (Trd_GetMaxTrdQtys), not a what-if"},
	{Name: "order and fill history", IBKR: Supplied, OpenD: Supplied, Note: "Trd_GetHistoryOrderList and Trd_GetHistoryOrderFillList"},
}

// Gap is why a capability is not fully served on OpenD.
type Gap string

// The gaps.
const (
	NoGap Gap = ""
	// BrokerGap: OpenD does not supply an input the capability needs. It
	// stays a gap unless another source supplies that input.
	BrokerGap Gap = "broker"
	// UnknownGap: whether OpenD supplies a needed input is not yet known.
	UnknownGap Gap = "to verify"
	// CanaryGap: OpenD supplies everything it needs; Canary does not read
	// it there yet.
	CanaryGap Gap = "Canary"
)

// OpenDGap derives why c is not fully served on OpenD from what it needs and
// what OpenD supplies, with the inputs that decide it.
func (c Capability) OpenDGap() (Gap, []string) {
	if c.OpenD.Status == Served || c.OpenD.Status == Partial {
		return NoGap, nil
	}
	var missing, unknown []string
	for _, need := range c.Needs {
		switch inputSupply(need) {
		case NotSupplied:
			missing = append(missing, need)
		case ToVerify:
			unknown = append(unknown, need)
		}
	}
	switch {
	case len(missing) > 0:
		return BrokerGap, missing
	case len(unknown) > 0:
		return UnknownGap, unknown
	case c.OpenD.Status == Unverified:
		return NoGap, nil
	}
	return CanaryGap, nil
}

// inputSupply is OpenD's supply of a named input.
func inputSupply(name string) Supply {
	for _, in := range inputs {
		if in.Name == name {
			return in.OpenD
		}
	}
	return ""
}

// Shared words for the table below.
const (
	openDLogin       = "one real moomoo US login (Desk's moomoo page, 2026-11-07)"
	ownerDecides     = "what it should say on OpenD is the owner's decision"
	readsIBKR        = "reads the IBKR connector"
	noStatements     = "built from broker statements, which OpenD does not provide"
	ordersNotOnOpenD = "Canary does not send orders through OpenD: place and manage orders in the Futu or moomoo app"
)

// served is IBKR's side of a capability the live smoke does not read.
var served = Coverage{Status: Served}

// capabilities is the coverage of each broker, one row per capability, in the
// order the coverage page lists them. IBKR serves everything; the OpenD column
// is what this build does there, each row from a test, the probe of every
// method against the simulator (2026-11-08) or the note. Why a row is not
// served on OpenD is derived from Needs and inputs (OpenDGap), not written
// here.
var capabilities = []Capability{
	{Name: "status and data health", Needs: []string{"session"},
		IBKR:  Coverage{Status: Served, Smoke: []string{"status"}},
		OpenD: Coverage{Status: Served, Tests: []string{"TestOpenDHealthCarriesItsOwnRows", "TestOpenDStatusReportsTheSession"}, Seen: openDLogin}},
	{Name: "account", Needs: []string{"account and positions"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "funds figures and currency conversion are tested on the simulator only", Tests: []string{"TestAsiaUsersReadTheirBookInLocalCurrency"}, Seen: "account list on " + openDLogin}},
	{Name: "daily P&L", Needs: []string{"intraday P&L"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Note: "Desk's moomoo page says it is not served; it answers on the simulator"}},
	{Name: "positions", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "options, futures and other holdings are listed apart and left out of exposure and limit checks; tested on the simulator only", Tests: []string{"TestOpenDRowsCarryAnIdentityAndTheJoin", "TestOpenDHoldingThatIsNotAStockIsListedApart", "TestAsiaUserOptionIsListedApart"}}},
	{Name: "quotes", Needs: []string{"quotes"},
		IBKR:  Coverage{Status: Served, Smoke: []string{"quote-spy"}},
		OpenD: Coverage{Status: Partial, Note: "only in markets where the login holds the quote right", Tests: []string{"TestAMarketWithoutAQuoteRightIsNamedAndLeavesTheOthers", "TestLinesGoToTheBookThenTheRegimeAndTheRestAreSnapshots"}, Seen: "Hong Kong and US quotes on " + openDLogin}},
	{Name: "price history", Needs: []string{"price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "market display", Needs: []string{"quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Note: "the display stream was not probed"}},
	{Name: "market overview", Needs: []string{"quotes", "price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "prices the held names and the US ETF references from OpenD; OpenD gives no index or futures data, so those rows are named as missing and the overview reads partial; history is not followed for OpenD rows, so price bars are not drawn here; tested on the simulator only", Tests: []string{"TestOpenDMarketOverviewPricesWhatOpenDCarriesAndNamesTheRest"}}},
	{Name: "portfolio allocation", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: readsIBKR, Tests: []string{"TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp"}}},
	{Name: "market tape", Needs: []string{"quotes", "price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: "joins on IBKR contract ids", Tests: []string{"TestMarketTapeCollectionDoesNotStartUnderOpenD"}}},
	{Name: "technical analysis", Needs: []string{"price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: readsIBKR, Tests: []string{"TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp"}}},
	{Name: "option chains", Needs: []string{"option chains and Greeks"},
		IBKR:  Coverage{Status: Served, Smoke: []string{"chain-iv"}},
		OpenD: Coverage{Status: NotServed, Note: readsIBKR + ", as do the option picker and setup option discovery", Tests: []string{"TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp", "TestOpenDUpNeverAnswersGatewayUnavailable"}}},
	{Name: "recorded option chains", Needs: []string{"option chains and Greeks"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "gamma", Needs: []string{"option chains and Greeks"},
		IBKR:  Coverage{Status: Served, Smoke: []string{"gamma", "gamma-spx"}},
		OpenD: Coverage{Status: NotServed, Note: readsIBKR, Tests: []string{"TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp"}}},
	{Name: "bond lookup", Needs: []string{"bonds"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: readsIBKR, Tests: []string{"TestOpenDUpNeverAnswersGatewayUnavailable"}}},
	{Name: "breadth", Needs: []string{"price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "answers from breadth already stored, still labelled IBKR HMDS, and never refreshed; " + ownerDecides}},
	{Name: "regime", Needs: []string{"quotes", "price bars"},
		IBKR:  Coverage{Status: Served, Smoke: []string{"regime"}},
		OpenD: Coverage{Status: Partial, Note: "reads HYG and SPY from OpenD; OpenD gives no index, currency or option-chain data, so the VIX term structure, USD/JPY and gamma rows are named as missing, and with two or more rows missing the headline stays \"data incomplete\"; a missing quote right is named too; tested on the simulator only", Tests: []string{"TestOpenDRegimeReadsListedInputsAndRefusesIndicesAndFX", "TestOpenDRegimeAndStressAnswerEndToEnd", "TestOpenDRegimeNamesAMissingQuoteRight"}}},
	{Name: "portfolio stress", Needs: []string{"account and positions", "quotes", "price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "composes from the OpenD regime, so its market context carries the regime's missing rows and its input health reads degraded; tested on the simulator only", Tests: []string{"TestOpenDRegimeAndStressAnswerEndToEnd"}}},
	{Name: "macro",
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Note: "reads no broker; the simulator tests run without macro sources"}},
	{Name: "market calendar and events",
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "brief", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Partial, Note: "names what OpenD does not serve and the holdings it does not analyse", Tests: []string{"TestBriefCoverageNamesTheGapOnlyUnderOpenD", "TestBriefNamesAHoldingItDoesNotAnalyse"}}},
	{Name: "Edge", Needs: []string{"broker statements"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: noStatements, Tests: []string{"TestIBKROnlyMethodsAnswerNotApplicable"}}},
	{Name: "reconciliation", Needs: []string{"broker statements"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: noStatements, Tests: []string{"TestIBKROnlyMethodsAnswerNotApplicable"}}},
	{Name: "statement reporting", Needs: []string{"broker statements"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: noStatements, Tests: []string{"TestIBKROnlyMethodsAnswerNotApplicable"}}},
	{Name: "FX attribution", Needs: []string{"broker statements"},
		IBKR:  served,
		OpenD: Coverage{Status: Refused, Note: noStatements + ", so it answers with no attribution"}},
	{Name: "borrow availability", Needs: []string{"borrow and fee data"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: "IBKR's borrow and fee data", Tests: []string{"TestBorrowSourcesReadAsIBKROnlyUnderOpenD", "TestIBKROnlyMethodsAnswerNotApplicable"}}},
	{Name: "earnings (WSH)", Needs: []string{"earnings calendar"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: "the earnings calendar comes through IBKR", Tests: []string{"TestOpenDHasNoWSHEarningsProvider"}}},
	{Name: "cash sweep", Needs: []string{"quoted FX rates", "bonds", "order placement and preview"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: "sizes orders from a quoted ledger rate, and OpenD's rate is implied", Tests: []string{"TestSweepAndLevellingDoNotRunUnderOpenD"}}},
	{Name: "cash policy",
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "core sleeve", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "core listing lookup", Needs: []string{"ISIN listings"},
		IBKR:  served,
		OpenD: Coverage{Status: NotServed, Note: "lists the listings IBKR knows for an ISIN", Tests: []string{"TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp"}}},
	{Name: "strategy", Needs: []string{"account and positions", "quotes", "price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "setups", Needs: []string{"quotes", "price bars"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "stock add plans", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "rules, mandate and risk policy", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Note: "a holding in another currency without a sound rate counts as not measured", Tests: []string{"TestOpenDVerdictWords"}}},
	{Name: "alerts", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "proposals", Needs: []string{"account and positions", "quotes"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "orders", Needs: []string{"order placement and preview"},
		IBKR:  served,
		OpenD: Coverage{Status: Refused, Note: ordersNotOnOpenD, Tests: []string{"TestOpenDDaemonAuthorisesNoBrokerWrite", "TestOpenDTradingStatusChecks", "TestFutuConnectorHasNoWriteMethod"}}},
	{Name: "order history", Needs: []string{"order and fill history"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Note: "order and fill history are tested on the simulator only"}},
	{Name: "option exercise", Needs: []string{"option chains and Greeks", "order placement and preview"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "Desk authority and execution", Needs: []string{"order placement and preview"},
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
	{Name: "watchlist and holding locks",
		IBKR:  served,
		OpenD: Coverage{Status: Unverified, Tests: []string{"TestAWatchlistRoundTripStoresNoIdentity"}}},
	{Name: "settings",
		IBKR:  served,
		OpenD: Coverage{Status: Unverified}},
}

// streamCapabilities names the capability of the two streams the connection
// serves; every other method's is on its route.
var streamCapabilities = map[string]string{
	rpc.MethodDisplaySubscribe: "market display",
	rpc.MethodQuoteSubscribe:   "quotes",
}

// capabilityOf returns the capability a method belongs to.
func (s *Server) capabilityOf(method string) (Capability, bool) {
	name, ok := streamCapabilities[method]
	if !ok {
		var r route
		if r, ok = s.route(method); ok {
			name = r.capability
		}
	}
	for _, c := range capabilities {
		if ok && c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}

// notServedHere reports whether this daemon's broker does not serve method,
// so dispatch answers not_applicable before any handler runs.
func (s *Server) notServedHere(method string) bool {
	if s.opend == nil {
		return false
	}
	c, ok := s.capabilityOf(method)
	return ok && c.OpenD.Status == NotServed
}

// BrokerInputs returns what each broker supplies, for the coverage page.
func BrokerInputs() []Input { return slices.Clone(inputs) }

// Capabilities returns the coverage table in page order, for the coverage page.
func Capabilities() []Capability { return slices.Clone(capabilities) }

// CapabilityMethods returns each capability's methods, sorted, for the
// coverage page. A capability without methods is work the daemon does on
// its own.
func CapabilityMethods() map[string][]string {
	out := map[string][]string{}
	for name, r := range (&Server{}).routeTable() {
		out[r.capability] = append(out[r.capability], name)
	}
	for name, capability := range streamCapabilities {
		out[capability] = append(out[capability], name)
	}
	for _, methods := range out {
		slices.Sort(methods)
	}
	return out
}
