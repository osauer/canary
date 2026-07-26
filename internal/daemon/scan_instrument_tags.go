package daemon

import (
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	scanInstrumentTagETF            = "etf"
	scanInstrumentTagBroadIndexETF  = "broad_index_etf"
	scanInstrumentTagLeveragedETP   = "leveraged_etp"
	scanInstrumentTagSectorETP      = "sector_etp"
	scanInstrumentTagSingleStockETP = "single_stock_etp"
)

// scanInstrumentTags returns conservative, prompt-facing labels for common
// exchange-traded products that IBKR's stock scanner may still report as STK.
// Absence of tags means "not in the local table", not "definitely common stock".
func scanInstrumentTags(row rpc.ScanRow) []string {
	symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
	if symbol == "" {
		return nil
	}
	tags := scanInstrumentTagsBySymbol[symbol]
	if len(tags) == 0 {
		return nil
	}
	return append([]string(nil), tags...)
}

var scanInstrumentTagsBySymbol = map[string][]string{
	"AAPD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AAPU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AMDD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AMDG": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AMDL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AMDU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"AMUU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"CONL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"DIA":  {scanInstrumentTagETF, scanInstrumentTagBroadIndexETF},
	"DLLL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"DOG":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"DXD":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"FAZ":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"FAS":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"GGLL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"GGLS": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"GLD":  {scanInstrumentTagETF},
	"HYG":  {scanInstrumentTagETF},
	"IBIT": {scanInstrumentTagETF},
	"IWM":  {scanInstrumentTagETF, scanInstrumentTagBroadIndexETF},
	"LABD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"LABU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"MSFD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"MSFU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"MULL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"MUU":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"NVDD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"NVDL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"NVDU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"PLTD": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"PLTU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"PSQ":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"QID":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"QLD":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"QQQ":  {scanInstrumentTagETF, scanInstrumentTagBroadIndexETF},
	"SDOW": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SDS":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SH":   {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SMH":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"SOXL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"SOXS": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"SPXL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SPXU": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SPY":  {scanInstrumentTagETF, scanInstrumentTagBroadIndexETF},
	"SQQQ": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"SSO":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"TECL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"TECS": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSectorETP},
	"TLT":  {scanInstrumentTagETF},
	"TNA":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"TQQQ": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"TSLL": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"TSLQ": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagSingleStockETP},
	"TZA":  {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"UDOW": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"UPRO": {scanInstrumentTagETF, scanInstrumentTagLeveragedETP, scanInstrumentTagBroadIndexETF},
	"XLB":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLE":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLF":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLI":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLK":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLP":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLRE": {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLU":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLV":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
	"XLY":  {scanInstrumentTagETF, scanInstrumentTagSectorETP},
}
