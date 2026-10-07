package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A leveling repayment is approved once for all its conversions (owner
// answer 2026-10-06 18:00 CEST: the repayment card only). Three commands
// carry it between a backend such as Desk and the daemon, as prepare,
// prepared-status and queue arm do for one proposal:
//
//   - prepare-bundle prepares every conversion and states the exact terms;
//     it prints the private bundle reference only as JSON, for the backend
//     that keeps it.
//   - submit-bundle reads everything it needs, the reference included, as one
//     JSON object on standard input and sends the bundle once.
//   - bundle-status reads the reference from standard input and only reads.

// bundleSubmitInputMaxBytes bounds the submission object read from standard
// input: the reference and the owner's confirmation (at most 16 KiB).
const bundleSubmitInputMaxBytes = 24 << 10

func runProposalsPrepareBundle(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals prepare-bundle")
	jsonOut := fs.Bool("json", false, "emit the private backend handoff JSON, the bundle reference included")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout per conversion")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals prepare-bundle: usage is `canary proposals prepare-bundle BUNDLE_ID REVISION [--json]`")
	}
	var res rpc.TradeProposalPrepareBundleResult
	p := rpc.TradeProposalPrepareBundleParams{BundleID: fs.Arg(0), Revision: fs.Arg(1), TimeoutMs: int(timeout.Milliseconds())}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPrepareBundle, p, &res); err != nil {
		return fail(env, "proposals prepare-bundle: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderBundlePrepareText(env, &res)
	return 0
}

// bundleSubmitInput is the one JSON object `proposals submit-bundle --stdin`
// reads; none of it travels in argv.
type bundleSubmitInput struct {
	BundleRef    string                               `json:"bundle_ref"`
	BundleID     string                               `json:"bundle_id"`
	Revision     string                               `json:"revision"`
	TermsDigest  string                               `json:"terms_digest"`
	Confirmation *rpc.TradeProposalBundleConfirmation `json:"confirmation,omitempty"`
}

func readBundleSubmitInput(env *Env) (bundleSubmitInput, error) {
	var in bundleSubmitInput
	if env.Stdin == nil {
		return in, fmt.Errorf("submit-bundle requires standard input")
	}
	raw, err := io.ReadAll(io.LimitReader(env.Stdin, bundleSubmitInputMaxBytes+1))
	if err != nil || len(raw) > bundleSubmitInputMaxBytes {
		return in, fmt.Errorf("cannot read a bounded submit-bundle object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("submit-bundle reads one JSON object {bundle_ref, bundle_id, revision, terms_digest, confirmation}")
	}
	if strings.TrimSpace(in.BundleRef) == "" || strings.TrimSpace(in.BundleID) == "" || strings.TrimSpace(in.Revision) == "" || strings.TrimSpace(in.TermsDigest) == "" {
		return in, fmt.Errorf("submit-bundle requires bundle_ref, bundle_id, revision and terms_digest")
	}
	return in, nil
}

func runProposalsSubmitBundle(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals submit-bundle")
	jsonOut := fs.Bool("json", false, "emit the submission result as JSON")
	fromStdin := fs.Bool("stdin", false, "read the submission object from standard input")
	timeout := fs.Duration("timeout", 5*time.Second, "broker timeout per conversion")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || !*fromStdin {
		return fail(env, "proposals submit-bundle requires --stdin; the bundle reference never travels in argv")
	}
	in, err := readBundleSubmitInput(env)
	if err != nil {
		return fail(env, "%v", err)
	}
	var res rpc.TradeProposalSubmitBundleResult
	p := rpc.TradeProposalSubmitBundleParams{BundleRef: in.BundleRef, BundleID: in.BundleID, Revision: in.Revision, TermsDigest: in.TermsDigest,
		Confirmation: in.Confirmation, FastPath: true, TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsSubmitBundle, p, &res); err != nil {
		return fail(env, "proposals submit-bundle: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderBundleSubmitText(env, &res)
	return 0
}

func runProposalsBundleStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals bundle-status")
	jsonOut := fs.Bool("json", false, "emit the passive bundle receipt")
	fromStdin := fs.Bool("bundle-ref-stdin", false, "read the private bundle reference from standard input")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || !*fromStdin {
		return fail(env, "proposals bundle-status requires --bundle-ref-stdin")
	}
	reference, err := readPrivateReference(env, "prepared bundle reference")
	if err != nil {
		return fail(env, "%v", err)
	}
	var res rpc.TradeProposalPreparedBundleStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPreparedBundleStatus, rpc.TradeProposalPreparedBundleStatusParams{BundleRef: reference}, &res); err != nil {
		return fail(env, "proposals bundle-status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderBundleStatusText(env, &res)
	if res.Outcome == "" {
		return 1
	}
	return 0
}

// renderBundlePrepareText shows what a review would ask the owner to
// confirm: the loan, where it lands, and each conversion at its limit. It
// never prints the private reference.
func renderBundlePrepareText(env *Env, res *rpc.TradeProposalPrepareBundleResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	var terms rpc.LevelingBundleTerms
	if !res.Accepted || json.Unmarshal([]byte(res.Terms), &terms) != nil {
		displayLine(env, "Leveling repayment · not prepared; nothing was sent", env.bold)
		displayRow(env, out, "Bundle", res.BundleID)
		renderDisplayBlockers(env, res.Blockers)
		for i, leg := range res.Legs {
			for _, b := range leg.Blockers {
				displayLine(env, fmt.Sprintf("  Conversion %d: %s · %s", i+1, b.Code, b.Message), env.yellow)
			}
		}
		fmt.Fprintln(out)
		return
	}
	n := len(terms.Legs)
	displayLine(env, fmt.Sprintf("Leveling repayment · %d %s for one approval · review expires %s", n, pluralWord(n, "conversion", "conversions"), bundleClock(terms.ExpiresAt)), env.bold)
	displayRow(env, out, "Bundle", terms.BundleID)
	displayRow(env, out, "Revision", terms.Revision)
	displayRow(env, out, "Preparation", terms.PreparationID)
	displayRow(env, out, "Account", terms.AccountMode+" · base "+terms.BaseCurrency)
	displayRow(env, out, "Loan", fmt.Sprintf("%s (%s) · costs %s a year%s", bundleAmount(terms.Cash, terms.Currency, true), bundleAmount(terms.CashBase, terms.BaseCurrency, true),
		bundlePct(terms.LoanRate, terms.LoanRateBound, "at least"), bundleThrough(terms.LoanRateThrough)))
	displayRow(env, out, "After", fmt.Sprintf("at least %s once every conversion fills; the cushion is %s (%s)", bundleAmount(terms.LandsAtLeast, terms.Currency, true),
		bundleAmount(terms.Cushion, terms.Currency, true), cashSweepMoney(terms.CushionBase, terms.BaseCurrency)))
	displayRow(env, out, "Worth it", fmt.Sprintf("saves about %s within %d days · costs at most %s", cashSweepMoney(terms.SavingBase, terms.BaseCurrency), terms.PaybackDays,
		cashSweepMoney(terms.CostBase, terms.BaseCurrency)))
	if terms.HeldToCap {
		displayLine(env, "  Held to the order cap in force ("+cashSweepMoney(terms.OrderCapBase, terms.BaseCurrency)+"): this repays part of the loan, and Canary plans the rest in a later cycle.", nil)
	}
	if terms.FundingShort {
		displayLine(env, "  The currencies allowed to pay hold less than the loan, so this repays what they can.", nil)
	}
	for _, leg := range terms.Legs {
		displayLine(env, fmt.Sprintf("  Conversion %d · %s %d %s · %s %s %s", leg.Leg, leg.Action, leg.Quantity, leg.Pair, leg.OrderType, bundlePrice(leg.LimitPrice), leg.TIF), env.bold)
		displayLine(env, fmt.Sprintf("    quote bid %s / ask %s at %s", bundlePrice(leg.Bid), bundlePrice(leg.Ask), bundleClock(leg.QuoteAt)), nil)
		displayLine(env, fmt.Sprintf("    pays %s · receives %s", bundleBound(leg.Pays), bundleBound(leg.Receives)), nil)
		displayLine(env, fmt.Sprintf("    %s keeps at least %s · earns %s · saves about %s, costs at most %s", leg.FundingCurrency, bundleAmount(leg.KeepsAtLeast, leg.FundingCurrency, false),
			bundlePct(leg.FundingRate, leg.FundingRateBound, "at most"), cashSweepMoney(leg.SavingBase, terms.BaseCurrency), cashSweepMoney(leg.CostBase, terms.BaseCurrency)), nil)
	}
	displayLine(env, "Canary checks every conversion again before it sends any, then sends them in order and stops at the first that is not sent; nothing is sent twice.", nil)
	displayRow(env, out, "Terms digest", res.TermsDigest)
	displayLine(env, "The private bundle reference is printed only with --json, for the backend that sends it.", env.dim)
	fmt.Fprintln(out)
}

// renderBundleSubmitText says what became of the bundle and of each
// conversion, in send order.
func renderBundleSubmitText(env *Env, res *rpc.TradeProposalSubmitBundleResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	displayLine(env, "Leveling repayment · "+bundleOutcomeWords(res.Outcome, res.Sent, len(res.Legs)), bundleOutcomeStyle(env, res.Outcome))
	displayRow(env, out, "Bundle", res.BundleID)
	for _, leg := range res.Legs {
		what := leg.Key
		if leg.Proposal.Key != "" {
			what = fmt.Sprintf("%s %d %s", leg.Proposal.Action, leg.Proposal.Quantity, leg.Proposal.Symbol)
		}
		line := fmt.Sprintf("  Conversion %d · %s · %s", leg.Leg, strings.ReplaceAll(leg.Outcome, "_", " "), what)
		if leg.OrderRef != "" {
			line += " · order " + leg.OrderRef
		}
		displayLine(env, line, nil)
		if leg.Message != "" && leg.Outcome == rpc.BundleOutcomeNotSent {
			displayLine(env, "    "+leg.Message, env.dim)
		}
		for _, b := range leg.Blockers {
			displayLine(env, "    "+b.Code+" · "+b.Message, env.yellow)
		}
	}
	renderDisplayBlockers(env, res.Blockers)
	if res.Outcome == rpc.BundleOutcomeSent || res.Outcome == rpc.BundleOutcomePartlySent {
		displayLine(env, "Sent is not filled: each conversion is a limit order for today; canary orders shows how it fills.", env.dim)
	}
	fmt.Fprintln(out)
}

// renderBundleStatusText is the passive receipt: the bundle's outcome from
// Canary's records and each conversion's preparation and local order.
func renderBundleStatusText(env *Env, res *rpc.TradeProposalPreparedBundleStatusResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	if res.Outcome == "" {
		displayLine(env, "Leveling repayment · status unavailable", env.bold)
		renderDisplayBlockers(env, res.Blockers)
		fmt.Fprintln(out)
		return
	}
	displayLine(env, "Leveling repayment · "+bundleOutcomeWords(res.Outcome, res.Sent, len(res.Legs)), bundleOutcomeStyle(env, res.Outcome))
	displayRow(env, out, "Bundle", res.BundleID)
	displayRow(env, out, "Preparation", res.PreparationID)
	if !res.SubmittedAt.IsZero() {
		displayRow(env, out, "Submitted", bundleClock(res.SubmittedAt))
	} else {
		displayRow(env, out, "Expires", bundleClock(res.ExpiresAt))
	}
	if res.Message != "" {
		displayLine(env, "  "+res.Message, env.yellow)
	}
	for _, leg := range res.Legs {
		line := fmt.Sprintf("  Conversion %d · %s · %s", leg.Leg, strings.ReplaceAll(leg.Outcome, "_", " "), leg.Key)
		if leg.OrderRef != "" && (leg.Outcome == rpc.BundleOutcomeSent || leg.Outcome == rpc.BundleOutcomeUnknown) {
			line += " · order " + leg.OrderRef
		}
		if leg.Order != nil && leg.Order.Found {
			line += " · " + nonEmpty(leg.Order.Order.LifecycleStatus, leg.Order.Order.SendState)
		}
		displayLine(env, line, nil)
	}
	renderDisplayBlockers(env, res.Blockers)
	fmt.Fprintln(out)
}

// bundleOutcomeWords is a bundle outcome in the owner's words.
func bundleOutcomeWords(outcome string, sent, legs int) string {
	switch outcome {
	case rpc.BundleOutcomeSent:
		if legs == 1 {
			return "the conversion was sent"
		}
		return fmt.Sprintf("all %d conversions sent", legs)
	case rpc.BundleOutcomePartlySent:
		return fmt.Sprintf("%d of %d conversions sent", sent, legs)
	case rpc.BundleOutcomeNotSent:
		return "nothing was sent"
	case rpc.BundleOutcomeUnknown:
		if sent > 0 {
			return fmt.Sprintf("outcome not confirmed · %d of %d sent, one may have reached the broker", sent, legs)
		}
		return "outcome not confirmed · a conversion may have reached the broker"
	case rpc.BundleOutcomePrepared:
		return "prepared, not sent"
	case rpc.BundleOutcomeExpired:
		return "the review expired; nothing was sent"
	}
	return "not sent again"
}

func bundleOutcomeStyle(env *Env, outcome string) func(string) string {
	switch outcome {
	case rpc.BundleOutcomeSent:
		return env.green
	case rpc.BundleOutcomePartlySent, rpc.BundleOutcomeUnknown:
		return env.yellow
	}
	return env.bold
}

// bundleAmount is an amount with thousands separators, two decimals, a true
// minus sign and the currency after it; signed adds a plus to a positive
// balance.
func bundleAmount(v float64, ccy string, signed bool) string {
	text := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	whole, frac, _ := strings.Cut(text, ".")
	text = groupThousands(whole) + "." + frac
	switch {
	case v < 0 && text != "0.00":
		text = "−" + text
	case signed && v > 0 && text != "0.00":
		text = "+" + text
	}
	return text + " " + ccy
}

// bundleBound is an amount at the price limit with its bound.
func bundleBound(a rpc.LevelingBundleAmount) string {
	switch a.Bound {
	case rpc.LevelingBundleBoundAtMost:
		return "at most " + bundleAmount(a.Amount, a.Currency, false)
	case rpc.LevelingBundleBoundAtLeast:
		return "at least " + bundleAmount(a.Amount, a.Currency, false)
	}
	return bundleAmount(a.Amount, a.Currency, false)
}

// bundlePrice is a pair price to five decimals, IDEALPRO's grid.
func bundlePrice(v float64) string { return strconv.FormatFloat(v, 'f', 5, 64) }

// bundlePct is an annual rate; bound marks one standing in from the
// currency's other side.
func bundlePct(r float64, bound bool, word string) string {
	text := strconv.FormatFloat(r*100, 'f', 2, 64) + "%"
	if bound {
		return word + " " + text
	}
	return text
}

func bundleThrough(day string) string {
	if day == "" {
		return ""
	}
	return ", statements to " + day
}

// bundleClock is a wall-clock time in the local zone.
func bundleClock(t time.Time) string {
	if t.IsZero() {
		return "unavailable"
	}
	return t.Local().Format("15:04 MST")
}
