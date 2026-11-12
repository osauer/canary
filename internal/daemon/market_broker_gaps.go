package daemon

import (
	"errors"
	"fmt"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// marketRowError is the text a market overview row carries when its read
// failed. A broker refusing the row outright (a kind it does not supply, a
// market the login holds no quote right for) is a gap the reader is told
// about in plain words, as the regime names its missing inputs; any other
// error, a fault to retry, keeps its own text, as every row always has.
//
// Wording approved by the owner on 2026-11-12 05:09 CET. Modelled on
// regimeGapWarnings.
func marketRowError(kind broker.Kind, row rpc.MarketInstrument, err error) string {
	refusal := brokerRefusal(err)
	if refusal == nil {
		return err.Error()
	}
	name := brokerDisplayName(kind)
	if right, ok := errors.AsType[*broker.NoQuoteRightError](refusal); ok {
		return fmt.Sprintf("This %s login has no %s quote right, so %s is missing from this overview.", name, right.Market, row.Name)
	}
	return fmt.Sprintf("%s has no %s, so %s is missing from this overview.", name, marketRowKindWords(row.Kind), row.Name)
}

// marketRowKindWords says what kind of data a row needs, for a reader.
func marketRowKindWords(kind string) string {
	switch kind {
	case "index":
		return "index quotes"
	case "future":
		return "futures"
	}
	return "quotes for this name"
}
