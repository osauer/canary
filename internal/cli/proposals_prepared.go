package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runProposalsPrepare(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals prepare")
	jsonOut := fs.Bool("json", false, "emit private backend handoff JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to proposal quantity")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || !*jsonOut {
		return fail(env, "proposals prepare requires KEY REVISION --json; retain its private reference only in the backend")
	}
	var res rpc.TradeProposalPrepareResult
	p := rpc.TradeProposalPreviewParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, TimeoutMs: int(timeout.Milliseconds())}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPrepare, p, &res); err != nil {
		return fail(env, "proposals prepare: %v", err)
	}
	return printJSON(env, res)
}

func readPreparedProposalReference(env *Env) (string, error) {
	if env.Stdin == nil {
		return "", fmt.Errorf("prepared proposal reference requires standard input")
	}
	raw, err := io.ReadAll(io.LimitReader(env.Stdin, 4097))
	if err != nil || len(raw) > 4096 {
		return "", fmt.Errorf("cannot read bounded prepared proposal reference")
	}
	ref := strings.TrimSpace(string(raw))
	if ref == "" || strings.ContainsAny(ref, " \t\n\r") {
		return "", fmt.Errorf("one nonempty prepared proposal reference is required on standard input")
	}
	return ref, nil
}

func runProposalsPreparedStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals prepared-status")
	jsonOut := fs.Bool("json", false, "emit the passive preparation receipt")
	fromStdin := fs.Bool("prepared-ref-stdin", false, "read the private reference from standard input")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || !*fromStdin {
		return fail(env, "proposals prepared-status requires --prepared-ref-stdin")
	}
	reference, err := readPreparedProposalReference(env)
	if err != nil {
		return fail(env, "%v", err)
	}
	var res rpc.TradeProposalPreparedStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPreparedStatus, rpc.TradeProposalPreparedStatusParams{PreparedRef: reference}, &res); err != nil {
		return fail(env, "proposals prepared-status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	if res.Preparation == nil {
		fmt.Fprintln(env.Stdout, "Prepared proposal unavailable")
		return 1
	}
	fmt.Fprintf(env.Stdout, "Prepared proposal %s: %s\n", res.Preparation.ID, res.Preparation.State)
	return 0
}
