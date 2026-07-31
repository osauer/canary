package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	regimeBuilderSpecDoc = "https://osauer.dev/canary/docs/internals/regime-dashboard.html"
	regimeBuilderSource  = "point-in-time panel"
)

// RegimePointInTimeRow is a captured market panel and forward stress target
// used to rebuild a regime observation without future data.
type RegimePointInTimeRow struct {
	Date             string                    `json:"date,omitempty"`
	AsOf             time.Time                 `json:"as_of,omitzero"`
	Case             string                    `json:"case,omitempty"`
	MarketCluster    string                    `json:"market_cluster,omitempty"`
	VIXTermStructure RegimePointInTimeVIXTerm  `json:"vix_term_structure"`
	VolOfVol         RegimePointInTimeVolOfVol `json:"vol_of_vol"`
	HYGSPYDivergence RegimePointInTimeHYGSPY   `json:"hyg_spy_divergence"`
	CreditSpreads    RegimePointInTimeCredit   `json:"credit_spreads"`
	FundingStress    RegimePointInTimeFunding  `json:"funding_stress"`
	USDJPY           RegimePointInTimeUSDJPY   `json:"usd_jpy"`
	GammaZero        *RegimePointInTimeGamma   `json:"gamma_zero,omitempty"`
	Breadth          RegimePointInTimeBreadth  `json:"breadth"`
	Target           RegimeBacktestTarget      `json:"target"`
	Notes            string                    `json:"notes,omitempty"`
}

// RegimePointInTimeMeta carries source, status, and observation time shared by
// captured regime clusters.
type RegimePointInTimeMeta struct {
	Status   string    `json:"status,omitempty"`
	Source   string    `json:"source,omitempty"`
	AsOf     time.Time `json:"as_of,omitzero"`
	AsOfDate string    `json:"as_of_date,omitempty"`
}

// RegimePointInTimeVIXTerm captures VIX term-structure inputs and their shared
// point-in-time metadata.
type RegimePointInTimeVIXTerm struct {
	RegimePointInTimeMeta
	VIX          *float64 `json:"vix,omitempty"`
	VIX3M        *float64 `json:"vix3m,omitempty"`
	Ratio        *float64 `json:"ratio,omitempty"`
	VIXPrevClose *float64 `json:"vix_prev_close,omitempty"`
	VIXChangePct *float64 `json:"vix_change_pct,omitempty"`
}

// RegimePointInTimeVolOfVol captures volatility-of-volatility inputs.
type RegimePointInTimeVolOfVol struct {
	RegimePointInTimeMeta
	Last      *float64 `json:"last,omitempty"`
	Change20D *float64 `json:"change_20d_pct,omitempty"`
}

// RegimePointInTimeHYGSPY captures high-yield credit and equity divergence
// inputs.
type RegimePointInTimeHYGSPY struct {
	RegimePointInTimeMeta
	HYGPrice     *float64 `json:"hyg_price,omitempty"`
	HYG50DMA     *float64 `json:"hyg_50dma,omitempty"`
	SPYPrice     *float64 `json:"spy_price,omitempty"`
	SPY52WHigh   *float64 `json:"spy_52w_high,omitempty"`
	SPYPrevClose *float64 `json:"spy_prev_close,omitempty"`
	SPYChange    *float64 `json:"spy_change,omitempty"`
	SPYChangePct *float64 `json:"spy_change_pct,omitempty"`
}

// RegimePointInTimeCredit captures investment-grade and high-yield spread
// inputs.
type RegimePointInTimeCredit struct {
	RegimePointInTimeMeta
	HYOAS       *float64 `json:"hy_oas,omitempty"`
	IGOAS       *float64 `json:"ig_oas,omitempty"`
	HYIGSpread  *float64 `json:"hy_ig_spread,omitempty"`
	HY20DChange *float64 `json:"hy_oas_20d_change,omitempty"`
}

// RegimePointInTimeFunding captures commercial-paper and Treasury-bill funding
// spread inputs.
type RegimePointInTimeFunding struct {
	RegimePointInTimeMeta
	CP3M      *float64 `json:"cp_3m_rate,omitempty"`
	TBill3M   *float64 `json:"tbill_3m_rate,omitempty"`
	SpreadBps *float64 `json:"spread_bps,omitempty"`
}

// RegimePointInTimeUSDJPY captures USD/JPY level and weekly-change inputs.
type RegimePointInTimeUSDJPY struct {
	RegimePointInTimeMeta
	Last         *float64 `json:"last,omitempty"`
	Close7DAgo   *float64 `json:"close_7d_ago,omitempty"`
	WeeklyChange *float64 `json:"weekly_change_pct,omitempty"`
}

// RegimePointInTimeGamma captures a gamma envelope with its source and trust
// classification.
type RegimePointInTimeGamma struct {
	Trusted  bool                   `json:"trusted,omitempty"`
	Method   string                 `json:"method,omitempty"`
	Source   string                 `json:"source,omitempty"`
	AsOf     time.Time              `json:"as_of,omitzero"`
	Envelope rpc.GammaZeroSPXResult `json:"envelope"`
}

// RegimePointInTimeBreadth captures moving-average participation and new-high
// versus new-low inputs.
type RegimePointInTimeBreadth struct {
	RegimePointInTimeMeta
	PctAbove50DMA  *float64 `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA *float64 `json:"pct_above_200dma,omitempty"`
	NewHighsToday  int      `json:"new_highs_today,omitempty"`
	NewLowsToday   int      `json:"new_lows_today,omitempty"`
	NetNewHighsPct *float64 `json:"net_new_highs_pct,omitempty"`
}

func readRegimePointInTimeRows(r io.Reader) ([]RegimePointInTimeRow, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []RegimePointInTimeRow
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row RegimePointInTimeRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if row.Date == "" && row.AsOf.IsZero() {
			return nil, fmt.Errorf("line %d: date or as_of is required", lineNo)
		}
		if row.Date != "" {
			if _, err := time.Parse("2006-01-02", row.Date); err != nil {
				return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
			}
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func buildRegimeBacktestObservations(rows []RegimePointInTimeRow) []RegimeBacktestObservation {
	out := make([]RegimeBacktestObservation, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildRegimeBacktestObservation(row))
	}
	return out
}

func buildRegimeBacktestObservation(row RegimePointInTimeRow) RegimeBacktestObservation {
	asOf := regimePointInTimeAsOf(row)
	regime := rpc.RegimeSnapshotResult{
		AsOf:    asOf,
		SpecDoc: regimeBuilderSpecDoc,
	}
	regime.VIXTermStructure = buildRegimePITVIX(row.VIXTermStructure, row.Date, asOf)
	regime.VolOfVol = buildRegimePITVolOfVol(row.VolOfVol, row.Date, asOf)
	regime.HYGSPYDivergence = buildRegimePITHYGSPY(row.HYGSPYDivergence, row.Date, asOf)
	regime.CreditSpreads = buildRegimePITCredit(row.CreditSpreads, row.Date, asOf)
	regime.FundingStress = buildRegimePITFunding(row.FundingStress, row.Date, asOf)
	regime.USDJPY = buildRegimePITUSDJPY(row.USDJPY, row.Date, asOf)
	regime.GammaZero, regime.WarningDetails, regime.DataQuality = buildRegimePITGamma(row.GammaZero, row.Date, asOf)
	regime.Breadth = buildRegimePITBreadth(row.Breadth, row.Date, asOf)
	backfillBacktestRegimeComposite(&regime)
	regime.Summary = buildRegimePITSummary(regime)
	regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	rpc.CompactRegimeSnapshot(&regime)
	return RegimeBacktestObservation{
		Date:          row.Date,
		AsOf:          row.AsOf,
		Case:          row.Case,
		MarketCluster: row.MarketCluster,
		Regime:        regime,
		Target:        row.Target,
		Notes:         row.Notes,
	}
}

func writeRegimeBacktestObservationsJSONL(w io.Writer, rows []RegimeBacktestObservation) error {
	enc := json.NewEncoder(w)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func regimePointInTimeAsOf(row RegimePointInTimeRow) time.Time {
	if !row.AsOf.IsZero() {
		return row.AsOf
	}
	if row.Date != "" {
		if t, err := time.Parse("2006-01-02", row.Date); err == nil {
			return t
		}
	}
	return time.Time{}
}

func buildRegimePITVIX(in RegimePointInTimeVIXTerm, date string, asOf time.Time) rpc.RegimeVIXTerm {
	out := rpc.RegimeVIXTerm{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("VIX/VIX3M", "<0.92", "0.92-1.00", ">1.00", "trips >1.00"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:       regimePITStatus(in.RegimePointInTimeMeta, in.VIX != nil && in.VIX3M != nil),
		VIX:          in.VIX,
		VIX3M:        in.VIX3M,
		Ratio:        in.Ratio,
		VIXPrevClose: in.VIXPrevClose,
		VIXChangePct: in.VIXChangePct,
	}
	if out.Ratio == nil && in.VIX != nil && in.VIX3M != nil && *in.VIX3M != 0 {
		out.Ratio = new(*in.VIX / *in.VIX3M)
	}
	if out.VIXChangePct == nil && in.VIX != nil && in.VIXPrevClose != nil && *in.VIXPrevClose != 0 {
		out.VIXChangePct = new((*in.VIX - *in.VIXPrevClose) / *in.VIXPrevClose * 100)
	}
	out.Band = classifyRegimePITVIX(out.Ratio, out.Status)
	out.BandReason = regimeBandReason(out.Band, "VIX/VIX3M")
	out.VIXQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.VIX3MQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	if out.Ratio == nil {
		out.FieldsMissing = append(out.FieldsMissing, "ratio")
	}
	return out
}

func buildRegimePITVolOfVol(in RegimePointInTimeVolOfVol, date string, asOf time.Time) rpc.RegimeVolOfVol {
	out := rpc.RegimeVolOfVol{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("VVIX", "<90", "90-110", ">110", "trips >110"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:    regimePITStatus(in.RegimePointInTimeMeta, in.Last != nil),
		Symbol:    "VVIX",
		Last:      in.Last,
		Change20D: in.Change20D,
		AsOfDate:  regimePITAsOfDate(in.RegimePointInTimeMeta, date),
		Source:    regimePITSource(in.RegimePointInTimeMeta),
	}
	out.Band = classifyRegimePITVolOfVol(out.Last, out.Status)
	out.BandReason = regimeBandReason(out.Band, "VVIX")
	out.ValueQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	return out
}

func buildRegimePITHYGSPY(in RegimePointInTimeHYGSPY, date string, asOf time.Time) rpc.RegimeHYGSPYDivergence {
	out := rpc.RegimeHYGSPYDivergence{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("HYG/SPY divergence", "HYG >= 50-DMA", "HYG < 50-DMA", "HYG < 50-DMA while SPY near 52w high", "trips HYG <50dma near 52w high"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:       regimePITStatus(in.RegimePointInTimeMeta, in.HYGPrice != nil && in.HYG50DMA != nil),
		HYGPrice:     in.HYGPrice,
		HYG50DMA:     in.HYG50DMA,
		SPYPrice:     in.SPYPrice,
		SPY52WHigh:   in.SPY52WHigh,
		SPYPrevClose: in.SPYPrevClose,
		SPYChange:    in.SPYChange,
		SPYChangePct: in.SPYChangePct,
	}
	if out.SPYChange == nil && in.SPYPrice != nil && in.SPYPrevClose != nil {
		out.SPYChange = new(*in.SPYPrice - *in.SPYPrevClose)
	}
	if out.SPYChangePct == nil && in.SPYPrice != nil && in.SPYPrevClose != nil && *in.SPYPrevClose != 0 {
		out.SPYChangePct = new((*in.SPYPrice - *in.SPYPrevClose) / *in.SPYPrevClose * 100)
	}
	out.Band = classifyRegimePITHYGSPY(out, out.Status)
	out.BandReason = regimeBandReason(out.Band, "HYG/SPY")
	out.HYGQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.HYG50DMAQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.SPYQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.SPY52WHighQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	if out.SPYPrice == nil {
		out.FieldsMissing = append(out.FieldsMissing, "spy_price")
	}
	if out.SPY52WHigh == nil {
		out.FieldsMissing = append(out.FieldsMissing, "spy_52w_high")
	}
	return out
}

func buildRegimePITCredit(in RegimePointInTimeCredit, date string, asOf time.Time) rpc.RegimeCreditSpreads {
	out := rpc.RegimeCreditSpreads{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("HY OAS", "<4.0 and not widening", "4.0-5.5 or +0.50pp/20d", ">5.5 or +1.00pp/20d", "trips >5.5"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:      regimePITStatus(in.RegimePointInTimeMeta, in.HYOAS != nil),
		HYOAS:       in.HYOAS,
		IGOAS:       in.IGOAS,
		HYIGSpread:  in.HYIGSpread,
		HY20DChange: in.HY20DChange,
		AsOfDate:    regimePITAsOfDate(in.RegimePointInTimeMeta, date),
		Source:      regimePITSource(in.RegimePointInTimeMeta),
	}
	if out.HYIGSpread == nil && in.HYOAS != nil && in.IGOAS != nil {
		out.HYIGSpread = new(*in.HYOAS - *in.IGOAS)
	}
	out.Band = classifyRegimePITCredit(out, out.Status)
	out.BandReason = regimeBandReason(out.Band, "HY OAS")
	out.HYOASQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.IGOASQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.SpreadQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	if out.IGOAS == nil {
		out.FieldsMissing = append(out.FieldsMissing, "ig_oas")
	}
	return out
}

func buildRegimePITFunding(in RegimePointInTimeFunding, date string, asOf time.Time) rpc.RegimeFundingStress {
	out := rpc.RegimeFundingStress{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("CP 90d AA financial - 3m T-bill", "<25 bp", "25-75 bp", ">75 bp", "trips >75 bp"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:    regimePITStatus(in.RegimePointInTimeMeta, in.SpreadBps != nil || (in.CP3M != nil && in.TBill3M != nil)),
		CP3M:      in.CP3M,
		TBill3M:   in.TBill3M,
		SpreadBps: in.SpreadBps,
		AsOfDate:  regimePITAsOfDate(in.RegimePointInTimeMeta, date),
		Source:    regimePITSource(in.RegimePointInTimeMeta),
	}
	if out.SpreadBps == nil && in.CP3M != nil && in.TBill3M != nil {
		out.SpreadBps = new((*in.CP3M - *in.TBill3M) * 100)
	}
	out.Band = classifyRegimePITFunding(out.SpreadBps, out.Status)
	out.BandReason = regimeBandReason(out.Band, "funding spread")
	out.CP3MQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.TBill3MQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.SpreadQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	return out
}

func buildRegimePITUSDJPY(in RegimePointInTimeUSDJPY, date string, asOf time.Time) rpc.RegimeUSDJPY {
	out := rpc.RegimeUSDJPY{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("USD/JPY weekly change", "yen move <1%", "yen strengthens 1-2%", "yen strengthens >2%", "trips yen +2%/week"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:       regimePITStatus(in.RegimePointInTimeMeta, in.WeeklyChange != nil || (in.Last != nil && in.Close7DAgo != nil)),
		Symbol:       "USD.JPY",
		Last:         in.Last,
		Close7DAgo:   in.Close7DAgo,
		WeeklyChange: in.WeeklyChange,
	}
	if out.WeeklyChange == nil && in.Last != nil && in.Close7DAgo != nil && *in.Close7DAgo != 0 {
		out.WeeklyChange = new((*in.Last - *in.Close7DAgo) / *in.Close7DAgo * 100)
	}
	out.Band = classifyRegimePITUSDJPY(out.WeeklyChange, out.Status)
	out.BandReason = regimeBandReason(out.Band, "USD/JPY")
	out.LastQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	out.Close7DAgoQuality = regimePITQuality(in.RegimePointInTimeMeta, asOf)
	if out.Close7DAgo == nil {
		out.FieldsMissing = append(out.FieldsMissing, "close_7d_ago")
	}
	if out.WeeklyChange == nil {
		out.FieldsMissing = append(out.FieldsMissing, "weekly_change_pct")
	}
	return out
}

func buildRegimePITGamma(in *RegimePointInTimeGamma, date string, asOf time.Time) (rpc.RegimeGammaZero, []rpc.RegimeWarning, []rpc.DataQualityHealth) {
	out := rpc.RegimeGammaZero{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("SPY+SPX zero gamma", "spot >2% above zero-gamma", "within +/-2%", "spot below zero-gamma", "trips spot below zero-gamma"),
			AsOf:       regimePITAsOf(RegimePointInTimeMeta{Source: "gamma snapshot log"}, date, asOf),
		},
		Status: rpc.RegimeStatusUnavailable,
	}
	reason := "point-in-time gamma snapshot missing; ex-gamma mode leaves gamma unranked"
	if in != nil && in.Trusted {
		env := in.Envelope
		if env.Result != nil {
			if env.Result.Method == "" {
				env.Result.Method = strings.TrimSpace(in.Method)
			}
			if env.Result.Source == "" {
				env.Result.Source = strings.TrimSpace(in.Source)
			}
			if env.Result.AsOf.IsZero() {
				env.Result.AsOf = in.AsOf
			}
		}
		if ok, why := trustedRegimePITGamma(env); ok {
			out.Status = rpc.RegimeStatusOK
			out.Envelope = env
			out.Band = classifyRegimePITGamma(env.Result)
			out.BandReason = regimeBandReason(out.Band, "dealer gamma")
			out.ZeroGammaQuality = &rpc.Quality{AsOf: env.Result.AsOf, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy, Source: env.Result.Method}
			out.GammaTotalAbsQuality = &rpc.Quality{AsOf: env.Result.AsOf, FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate, Source: env.Result.Source}
			return out, nil, nil
		} else {
			reason = "point-in-time gamma snapshot rejected: " + why
		}
	}
	out.Envelope = rpc.GammaZeroSPXResult{
		Status:         rpc.GammaZeroStatusCold,
		ColdReasonCode: "point_in_time_gamma_missing",
		ColdReason:     reason,
		ColdAction:     "Provide a trusted gamma_zero.envelope with method, source, and as_of to include gamma in replay.",
	}
	warnings := []rpc.RegimeWarning{{
		Code:     "gamma_zero_point_in_time_unavailable",
		Scope:    "gamma_zero",
		Severity: "warning",
		Message:  reason,
		Impact:   "dealer gamma is unranked; replay is explicitly ex-gamma for this row.",
		Action:   "Attach trusted method-stamped point-in-time gamma snapshots before running with gamma.",
	}}
	quality := []rpc.DataQualityHealth{{
		Surface:          "gamma",
		Status:           "degraded",
		Summary:          "degraded: point-in-time gamma unavailable",
		DegradedClusters: []string{"gamma"},
		AsOf:             asOf,
	}}
	return out, warnings, quality
}

func buildRegimePITBreadth(in RegimePointInTimeBreadth, date string, asOf time.Time) rpc.RegimeBreadth {
	out := rpc.RegimeBreadth{
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Thresholds: rpc.HeuristicThresholds("S&P 500 % above 50-DMA", ">55", "40-55", "<40", "trips <40% (50d)"),
			AsOf:       regimePITAsOf(in.RegimePointInTimeMeta, date, asOf),
		},
		Status:        regimePITStatus(in.RegimePointInTimeMeta, in.PctAbove50DMA != nil),
		NewHighsToday: in.NewHighsToday,
		NewLowsToday:  in.NewLowsToday,
		ValueQuality:  regimePITQuality(in.RegimePointInTimeMeta, asOf),
	}
	if in.PctAbove50DMA != nil {
		out.PctAbove50DMA = *in.PctAbove50DMA
	}
	if in.PctAbove200DMA != nil {
		out.PctAbove200DMA = *in.PctAbove200DMA
	}
	if in.NetNewHighsPct != nil {
		out.NetNewHighsPct = *in.NetNewHighsPct
	}
	out.Envelope = rpc.BreadthSPXResult{
		State:          rpc.BreadthStateReady,
		PctAbove50DMA:  out.PctAbove50DMA,
		PctAbove200DMA: out.PctAbove200DMA,
		NewHighsToday:  out.NewHighsToday,
		NewLowsToday:   out.NewLowsToday,
		NetNewHighsPct: out.NetNewHighsPct,
		Source:         regimePITSource(in.RegimePointInTimeMeta),
		Method:         "point-in-time-panel",
		AsOf:           asOf,
	}
	if out.Status == rpc.RegimeStatusUnavailable {
		out.Envelope.State = rpc.BreadthStateCold
	}
	out.Band = classifyRegimePITBreadth(in.PctAbove50DMA, out.Status)
	out.BandReason = regimeBandReason(out.Band, "breadth")
	return out
}

func regimePITStatus(meta RegimePointInTimeMeta, available bool) string {
	status := strings.ToLower(strings.TrimSpace(meta.Status))
	if status != "" {
		return status
	}
	if available {
		return rpc.RegimeStatusOK
	}
	return rpc.RegimeStatusUnavailable
}

func regimePITAsOf(meta RegimePointInTimeMeta, date string, asOf time.Time) *rpc.RegimeAsOfSummary {
	rowAsOf := meta.AsOf
	if rowAsOf.IsZero() {
		rowAsOf = asOf
	}
	asOfDate := regimePITAsOfDate(meta, date)
	label := "point-in-time"
	if asOfDate != "" {
		label = "close " + asOfDate
	}
	return &rpc.RegimeAsOfSummary{
		Label:     label,
		Time:      rowAsOf,
		Date:      asOfDate,
		Freshness: "point_in_time",
		Source:    regimePITSource(meta),
	}
}

func regimePITAsOfDate(meta RegimePointInTimeMeta, date string) string {
	if meta.AsOfDate != "" {
		return meta.AsOfDate
	}
	return date
}

func regimePITSource(meta RegimePointInTimeMeta) string {
	if strings.TrimSpace(meta.Source) != "" {
		return strings.TrimSpace(meta.Source)
	}
	return regimeBuilderSource
}

func regimePITQuality(meta RegimePointInTimeMeta, asOf time.Time) *rpc.Quality {
	qAsOf := meta.AsOf
	if qAsOf.IsZero() {
		qAsOf = asOf
	}
	return &rpc.Quality{
		AsOf:           qAsOf,
		FreshnessClass: rpc.FreshnessDerived,
		Confidence:     rpc.ConfidenceEstimate,
		Source:         regimePITSource(meta),
	}
}

func regimeBandReason(band, row string) string {
	switch band {
	case "green":
		return row + " in normal heuristic band"
	case "yellow":
		return row + " in watch heuristic band"
	case "red":
		return row + " in stress heuristic band"
	default:
		return ""
	}
}

func classifyRegimePITVIX(ratio *float64, status string) string {
	if !regimePITRankable(status) || ratio == nil {
		return ""
	}
	switch {
	case *ratio < 0.92:
		return "green"
	case *ratio < 1.00:
		return "yellow"
	default:
		return "red"
	}
}

func classifyRegimePITVolOfVol(v *float64, status string) string {
	if !regimePITRankable(status) || v == nil {
		return ""
	}
	switch {
	case *v < 90:
		return "green"
	case *v < 110:
		return "yellow"
	default:
		return "red"
	}
}

func classifyRegimePITHYGSPY(r rpc.RegimeHYGSPYDivergence, status string) string {
	if !regimePITRankable(status) || r.HYGPrice == nil || r.HYG50DMA == nil {
		return ""
	}
	if *r.HYGPrice >= *r.HYG50DMA {
		return "green"
	}
	if r.SPYPrice == nil || r.SPY52WHigh == nil {
		return ""
	}
	if *r.SPYPrice >= 0.97**r.SPY52WHigh {
		return "red"
	}
	return "yellow"
}

func classifyRegimePITCredit(r rpc.RegimeCreditSpreads, status string) string {
	if !regimePITRankable(status) || r.HYOAS == nil {
		return ""
	}
	if *r.HYOAS >= 5.5 || (r.HY20DChange != nil && *r.HY20DChange >= 1.0) {
		return "red"
	}
	if *r.HYOAS >= 4.0 || (r.HY20DChange != nil && *r.HY20DChange >= 0.5) {
		return "yellow"
	}
	return "green"
}

func classifyRegimePITFunding(spread *float64, status string) string {
	if !regimePITRankable(status) || spread == nil {
		return ""
	}
	switch {
	case *spread < 25:
		return "green"
	case *spread < 75:
		return "yellow"
	default:
		return "red"
	}
}

func classifyRegimePITUSDJPY(weeklyChange *float64, status string) string {
	if !regimePITRankable(status) || weeklyChange == nil {
		return ""
	}
	yenMove := -*weeklyChange
	switch {
	case yenMove < 1.0:
		return "green"
	case yenMove < 2.0:
		return "yellow"
	default:
		return "red"
	}
}

func classifyRegimePITBreadth(value *float64, status string) string {
	if !regimePITRankable(status) || value == nil {
		return ""
	}
	switch {
	case *value < 40:
		return "red"
	case *value <= 55:
		return "yellow"
	default:
		return "green"
	}
}

func classifyRegimePITGamma(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		type weightedBand struct {
			band   string
			weight float64
		}
		var bands []weightedBand
		for _, key := range []string{"SPY", "SPX"} {
			sub := c.PerIndex[key]
			if sub == nil {
				continue
			}
			if band := classifyRegimePITGamma(sub); band != "" {
				weight := sub.GammaTotalAbs
				if weight <= 0 {
					weight = 1
					if key == "SPX" {
						weight = 100
					}
				}
				bands = append(bands, weightedBand{band: band, weight: weight})
			}
		}
		if len(bands) == 0 {
			return ""
		}
		first := bands[0].band
		for _, band := range bands[1:] {
			if band.band != first {
				first = ""
				break
			}
		}
		if first != "" {
			return first
		}
		var total, red float64
		for _, band := range bands {
			total += band.weight
			if band.band == "red" {
				red += band.weight
			}
		}
		if total > 0 && red/total >= 0.5 {
			return "red"
		}
		return "yellow"
	}
	if c.GapPct != nil {
		switch {
		case *c.GapPct > 2:
			return "green"
		case *c.GapPct >= -2:
			return "yellow"
		default:
			return "red"
		}
	}
	switch c.GammaSign {
	case "positive":
		return "green"
	case "negative":
		return "red"
	default:
		return ""
	}
}

func regimePITRankable(status string) bool {
	return status == rpc.RegimeStatusOK || status == rpc.RegimeStatusStale
}

func trustedRegimePITGamma(env rpc.GammaZeroSPXResult) (bool, string) {
	if env.Status != rpc.GammaZeroStatusReady {
		return false, "envelope status is not ready"
	}
	if env.Result == nil {
		return false, "envelope result is missing"
	}
	if strings.TrimSpace(env.Result.Method) == "" {
		return false, "method is missing"
	}
	if strings.TrimSpace(env.Result.Source) == "" {
		return false, "source is missing"
	}
	if env.Result.AsOf.IsZero() {
		return false, "as_of is missing"
	}
	if classifyRegimePITGamma(env.Result) == "" {
		return false, "gamma snapshot has no rankable crossing or sign"
	}
	return true, ""
}

func buildRegimePITSummary(r rpc.RegimeSnapshotResult) rpc.RegimeSummary {
	c := r.Composite
	return rpc.RegimeSummary{
		Label:             c.Verdict,
		Evidence:          regimePITClusterEvidence(c),
		IndicatorEvidence: regimePITEvidence(c),
		PunchLine:         regimePITPunchLine(r),
		Confidence:        regimePITConfidence(c),
		DominantRisks:     regimePITDominantRisks(r),
		NotAdvice:         "Regime read only; not investment advice or a trade recommendation.",
	}
}

func regimePITEvidence(c rpc.RegimeComposite) string {
	var parts []string
	if c.GreenCount > 0 {
		parts = append(parts, fmt.Sprintf("%d green", c.GreenCount))
	}
	if c.YellowCount > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow", c.YellowCount))
	}
	if c.RedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d red", c.RedCount))
	}
	if c.UnrankedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked", c.UnrankedCount))
	}
	if len(parts) == 0 {
		return "0 ranked"
	}
	return strings.Join(parts, " / ")
}

func regimePITClusterEvidence(c rpc.RegimeComposite) string {
	var parts []string
	if c.ClusterGreenCount > 0 {
		parts = append(parts, fmt.Sprintf("%d green clusters", c.ClusterGreenCount))
	}
	if c.ClusterYellowCount > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow clusters", c.ClusterYellowCount))
	}
	if c.ClusterRedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d red clusters", c.ClusterRedCount))
	}
	if c.ClusterUnrankedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked clusters", c.ClusterUnrankedCount))
	}
	if len(parts) == 0 {
		return "0 ranked clusters"
	}
	return strings.Join(parts, " / ")
}

func regimePITConfidence(c rpc.RegimeComposite) string {
	if c.ClusterRankedCount < verdictFloor {
		return "low"
	}
	if c.ClusterUnrankedCount > 0 {
		return "medium"
	}
	return "high"
}

func regimePITDominantRisks(r rpc.RegimeSnapshotResult) []string {
	risks := map[string]string{
		"equity volatility": r.VIXTermStructure.Band,
		"vol-of-vol":        r.VolOfVol.Band,
		"ETF credit proxy":  r.HYGSPYDivergence.Band,
		"credit spreads":    r.CreditSpreads.Band,
		"funding stress":    r.FundingStress.Band,
		"FX carry":          r.USDJPY.Band,
		"dealer gamma":      r.GammaZero.Band,
		"breadth":           r.Breadth.Band,
	}
	names := make([]string, 0, len(risks))
	for name, band := range risks {
		if band == "red" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func regimePITPunchLine(r rpc.RegimeSnapshotResult) string {
	if len(r.WarningDetails) > 0 {
		return r.Composite.Verdict + "; " + r.WarningDetails[0].Impact
	}
	return r.Composite.Verdict + "."
}
