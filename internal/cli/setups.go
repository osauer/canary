package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runSetups(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "setups")
	specPath := fs.String("spec", "", "versioned setup JSON file, or - for stdin")
	symbol := fs.String("symbol", "", "one US stock underlying")
	conID := fs.Int("con-id", 0, "exact underlying contract ID when known")
	atText := fs.String("at", "", "past decision time in RFC3339 with timezone; reconstruction only")
	expiry := fs.String("expiry", "", "listed option expiry YYYYMMDD")
	strike := fs.String("strike", "", "owner-selected call strike; requires expiry")
	session := fs.String("session", "", "market session date YYYY-MM-DD; coverage only")
	jsonOut := fs.Bool("json", false, "emit typed observation evidence")
	orderRef := fs.String("order-ref", "", "markouts: one Canary order reference")
	since := fs.String("since", "", "markouts: fills on or after YYYY-MM-DD (New York)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() == 1 && fs.Arg(0) == "markouts" {
		if *specPath != "" || *atText != "" || *expiry != "" || *strike != "" || *conID != 0 || *session != "" {
			return fail(env, "setups markouts: only --order-ref, --since, --symbol and --json apply")
		}
		return runSetupMarkouts(ctx, env, rpc.SetupMarkoutsParams{OrderRef: *orderRef, Since: *since, Symbol: *symbol})
	}
	if *orderRef != "" || *since != "" {
		return fail(env, "order-ref and since apply to setups markouts only")
	}
	if fs.NArg() == 1 && fs.Arg(0) == "coverage" {
		if *specPath != "" || *atText != "" || *expiry != "" || *strike != "" || *conID != 0 {
			return fail(env, "setups coverage: only --session and --symbol apply")
		}
		p, err := rpc.NormalizeSetupCoverageParams(rpc.SetupCoverageParams{Session: *session, Symbol: *symbol})
		if err != nil {
			return fail(env, "setups coverage: %v", err)
		}
		var out rpc.SetupCoverageResult
		if err := env.Conn.Call(ctx, rpc.MethodSetupsCoverage, p, &out); err != nil {
			return fail(env, "setups coverage: %v", err)
		}
		return printJSON(env, out)
	}
	if *session != "" {
		return fail(env, "session applies to setups coverage only")
	}
	if fs.NArg() == 1 && fs.Arg(0) == "options" {
		if *specPath != "" || *atText != "" {
			return fail(env, "setups options: spec and at are not supported")
		}
		p := rpc.SetupOptionsParams{Underlying: rpc.ContractParams{Symbol: *symbol, ConID: *conID, SecType: "STK", Exchange: "SMART", Currency: "USD"}, Expiry: *expiry}
		if *strike != "" {
			v, err := strconv.ParseFloat(*strike, 64)
			if err != nil {
				return fail(env, "setups options: invalid strike")
			}
			p.Strike = &v
		}
		p, err := rpc.NormalizeSetupOptionsParams(p, time.Now())
		if err != nil {
			return fail(env, "setups options: %v", err)
		}
		var out rpc.SetupOptionsResult
		if err := env.Conn.Call(ctx, rpc.MethodSetupsOptions, p, &out); err != nil {
			return fail(env, "setups options: %v", err)
		}
		return printJSON(env, out)
	}
	if *expiry != "" || *strike != "" {
		return fail(env, "expiry and strike apply to setups options only")
	}
	if fs.NArg() != 1 || fs.Arg(0) != "evaluate" || *specPath == "" {
		return fail(env, "usage: canary setups evaluate --spec PATH|- --symbol SYMBOL [--con-id ID] [--at RFC3339] [--json]")
	}
	var reader io.Reader = env.Stdin
	if *specPath != "-" {
		file, err := os.Open(*specPath)
		if err != nil {
			return fail(env, "setups: %v", err)
		}
		defer file.Close()
		reader = file
	}
	if reader == nil {
		return fail(env, "setups: spec input unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(reader, 16385))
	if err != nil || len(body) > 16384 {
		return fail(env, "setups: spec must be at most 16 KiB")
	}
	var spec rpc.SetupSpec
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&spec); err != nil {
		return fail(env, "setups: invalid spec: %v", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return fail(env, "setups: spec must contain exactly one JSON object")
	}
	p := rpc.SetupEvaluateParams{Spec: spec, Contract: rpc.ContractParams{Symbol: *symbol, ConID: *conID, SecType: "STK", Exchange: "SMART", Currency: "USD"}}
	if *atText != "" {
		p.At, err = time.Parse(time.RFC3339, *atText)
		if err != nil {
			return fail(env, "setups: at requires RFC3339 with timezone")
		}
	}
	p, err = rpc.NormalizeSetupEvaluateParams(p, time.Now())
	if err != nil {
		return fail(env, "setups: %v", err)
	}
	var result rpc.SetupResult
	if err = env.Conn.Call(ctx, rpc.MethodSetupsEvaluate, p, &result); err != nil {
		return fail(env, "setups: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	riskReadLine(env, "Entry setup", result.Contract.Symbol, result.State)
	riskReadLine(env, "Rule", result.Spec.Template, result.Spec.Revision)
	riskReadLine(env, "Evidence", result.EvidenceKind, result.EvaluatedAt.Format(time.RFC3339))
	if result.SpikeAt != nil {
		riskReadLine(env, "Volume spike", result.SpikeAt.Format(time.RFC3339))
	}
	if result.Features.SpikeMultiple != nil {
		riskReadLine(env, "Activity", fmt.Sprintf("%.2f× same-slot mean across %d comparable sessions", *result.Features.SpikeMultiple, result.BaselineSessions))
	}
	if result.FirstConfirmedAt != nil {
		riskReadLine(env, "First price confirmation", result.FirstConfirmedAt.Format(time.RFC3339), result.ConfirmationType)
	}
	if result.ValidUntil != nil {
		riskReadLine(env, "Evidence deadline", result.ValidUntil.Format(time.RFC3339))
	}
	for _, reason := range result.Reasons {
		riskReadLine(env, "Reason", reason)
	}
	fmt.Fprintln(env.Stdout, "Observation only. Risk, option feasibility and exact order authority are separate.")
	return 0
}

// runSetupMarkouts prints the read-only entry-markout ledger as JSON. Rows are
// an entry diagnostic, not realized profit.
func runSetupMarkouts(ctx context.Context, env *Env, p rpc.SetupMarkoutsParams) int {
	p, err := rpc.NormalizeSetupMarkoutsParams(p)
	if err != nil {
		return fail(env, "setups markouts: %v", err)
	}
	var out rpc.SetupMarkoutsResult
	if err := env.Conn.Call(ctx, rpc.MethodSetupsMarkouts, p, &out); err != nil {
		return fail(env, "setups markouts: %v", err)
	}
	if out.Targets == nil {
		out.Targets = []rpc.SetupMarkoutTarget{}
	}
	return printJSON(env, out)
}
