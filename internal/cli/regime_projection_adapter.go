package cli

import (
	"time"

	"github.com/osauer/canary/v2/internal/regimerows"
	"github.com/osauer/canary/v2/internal/rpc"
)

type regimeBand = regimerows.Band

const (
	bandUnranked = regimerows.BandUnranked
	bandGreen    = regimerows.BandGreen
	bandYellow   = regimerows.BandYellow
	bandRed      = regimerows.BandRed
)

// regimeRow is the CLI render shape. Classification and wording come from
// regimerows; this adapter only preserves the renderer's private field names.
type regimeRow struct {
	name      string
	cluster   string
	value     string
	asOf      string
	band      regimeBand
	reason    string
	status    string
	stateNote string
	quality   string
	streak    string
}

func renderRegimeRow(row regimerows.Row) regimeRow {
	return regimeRow{
		name:      row.Name,
		cluster:   row.Cluster,
		value:     row.Value,
		asOf:      row.AsOf,
		band:      row.Band,
		reason:    row.Reason,
		status:    row.Status,
		stateNote: row.StateNote,
		quality:   row.Quality,
		streak:    row.Streak,
	}
}

var (
	qualityTag = regimerows.QualityTag
	asOfLabel  = regimerows.AsOfLabel
	ifNonEmpty = regimerows.IfNonEmpty

	gammaRowLabel           = regimerows.GammaRowLabel
	gammaPlainQualityReason = regimerows.GammaPlainQualityReason
	gammaIsSPYProxy         = regimerows.GammaIsSPYProxy
	formatSpotPrice         = regimerows.FormatSpotPrice
	formatRegimeAgreement   = regimerows.FormatRegimeAgreement
	gammaRegimeWord         = regimerows.GammaRegimeWord
	gammaSingleRegimeBand   = regimerows.GammaSingleRegimeBand
	gammaCombinedRegimeBand = regimerows.GammaCombinedRegimeBand

	rowVIXTerm = func(now time.Time, row rpc.RegimeVIXTerm) regimeRow {
		return renderRegimeRow(regimerows.VIXTerm(now, row))
	}
	rowVolOfVol = func(now time.Time, row rpc.RegimeVolOfVol) regimeRow {
		return renderRegimeRow(regimerows.VolOfVol(now, row))
	}
	rowHYGSPY = func(now time.Time, row rpc.RegimeHYGSPYDivergence) regimeRow {
		return renderRegimeRow(regimerows.HYGSPY(now, row))
	}
	rowCreditSpreads = func(now time.Time, row rpc.RegimeCreditSpreads) regimeRow {
		return renderRegimeRow(regimerows.CreditSpreads(now, row))
	}
	rowFundingStress = func(now time.Time, row rpc.RegimeFundingStress) regimeRow {
		return renderRegimeRow(regimerows.FundingStress(now, row))
	}
	rowUSDJPY = func(now time.Time, row rpc.RegimeUSDJPY) regimeRow {
		return renderRegimeRow(regimerows.USDJPY(now, row))
	}
	rowGamma = func(now time.Time, row rpc.RegimeGammaZero) regimeRow {
		return renderRegimeRow(regimerows.Gamma(now, row))
	}
	rowBreadth = func(now time.Time, row rpc.RegimeBreadth) regimeRow {
		return renderRegimeRow(regimerows.Breadth(now, row))
	}
)
