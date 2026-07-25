package cli

import (
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// PreviewRenderAccount renders synthetic account data with the production text
// renderer.
func PreviewRenderAccount(env *Env, a *rpc.AccountResult) {
	renderAccountText(env, a)
}

// PreviewRenderPositionsByUnderlying renders synthetic positions grouped by
// underlying with the production text renderer.
func PreviewRenderPositionsByUnderlying(env *Env, r *rpc.PositionsResult) {
	renderPositionsByUnderlying(env, r)
}

// PreviewRenderPositions renders synthetic position rows with the production
// text renderer.
func PreviewRenderPositions(env *Env, r *rpc.PositionsResult) {
	renderPositionsText(env, r)
}

// PreviewRenderChainExpiries renders a synthetic expiry list with the
// production text renderer.
func PreviewRenderChainExpiries(env *Env, r *rpc.ChainExpiriesResult, withIV bool) {
	renderChainExpiriesText(env, r, withIV)
}

// PreviewRenderChainStrikes renders a synthetic strike grid with the production
// text renderer.
func PreviewRenderChainStrikes(env *Env, c *rpc.ChainResult) {
	renderChainText(env, c)
}

// PreviewRenderQuoteSnapshot renders synthetic quote rows with the production
// text renderer.
func PreviewRenderQuoteSnapshot(env *Env, qs []rpc.Quote) {
	renderQuoteSnapshotText(env, qs)
}

// PreviewRenderHistory renders synthetic daily history with the production
// text renderer.
func PreviewRenderHistory(env *Env, r *rpc.HistoryDailyResult) {
	renderHistoryText(env, r)
}

// PreviewRenderScan renders synthetic scanner results with the production text
// renderer.
func PreviewRenderScan(env *Env, r *rpc.ScanResult) {
	renderScanText(env, r)
}

// PreviewRenderSize renders a synthetic position-size result with the
// production text renderer.
func PreviewRenderSize(env *Env, r *risk.SizeResult) {
	renderSizeText(env, r)
}

// PreviewRenderStatus renders synthetic daemon health with the production text
// renderer.
func PreviewRenderStatus(env *Env, h *rpc.HealthResult) {
	renderStatusText(env, h)
}

// PreviewRenderRegime renders a synthetic regime snapshot with the production
// text renderer.
func PreviewRenderRegime(env *Env, r *rpc.RegimeSnapshotResult) {
	renderRegimeText(env, r)
}

// PreviewRenderStress renders a synthetic stress result with the production
// text renderer.
func PreviewRenderStress(env *Env, r *rpc.StressResult) {
	renderStressText(env, env.Stdout, r)
}
