package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// queueArmInputMaxBytes bounds the arm object read from standard input.
const queueArmInputMaxBytes = 16 << 10

// runProposalsQueue carries one owner-signed reduction to the next regular
// session. prepare and arm are private backend handoffs like prepare and
// prepared-status: prepare prints its private reference only as JSON, and
// arm reads everything it needs, the reference included, as one JSON object
// on standard input. cancel, list and status never touch a reference.
func runProposalsQueue(ctx context.Context, env *Env, args []string) int {
	verbIdx := -1
	for i, arg := range args {
		switch arg {
		case "prepare", "arm", "cancel", "list", "status":
			verbIdx = i
		}
		if verbIdx >= 0 {
			break
		}
	}
	if verbIdx < 0 {
		return fail(env, "proposals queue: usage is `canary proposals queue prepare|arm|cancel|list|status`")
	}
	verb := args[verbIdx]
	rest := append(append([]string{}, args[:verbIdx]...), args[verbIdx+1:]...)
	switch verb {
	case "prepare":
		return runProposalsQueuePrepare(ctx, env, rest)
	case "arm":
		return runProposalsQueueArm(ctx, env, rest)
	case "cancel":
		return runProposalsQueueCancel(ctx, env, rest)
	case "list":
		return runProposalsQueueList(ctx, env, rest)
	default:
		return runProposalsQueueStatus(ctx, env, rest)
	}
}

func runProposalsQueuePrepare(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals queue prepare")
	jsonOut := fs.Bool("json", false, "emit private backend handoff JSON")
	qty := fs.Int("quantity", 0, "lower the signed maximum below the proposal quantity")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || !*jsonOut {
		return fail(env, "proposals queue prepare requires KEY REVISION --json; retain its private reference only in the backend")
	}
	var res rpc.TradeProposalQueuePrepareResult
	p := rpc.TradeProposalQueuePrepareParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsQueuePrepare, p, &res); err != nil {
		return fail(env, "proposals queue prepare: %v", err)
	}
	return printJSON(env, res)
}

// queueArmInput is the one JSON object `proposals queue arm --stdin` reads;
// none of it travels in argv.
type queueArmInput struct {
	QueuedRef    string `json:"queued_ref"`
	TermsDigest  string `json:"terms_digest"`
	DeskActionID string `json:"desk_action_id,omitempty"`
	Credential   string `json:"credential,omitempty"`
	Envelope     string `json:"envelope,omitempty"`
}

func readQueueArmInput(env *Env) (queueArmInput, error) {
	var in queueArmInput
	if env.Stdin == nil {
		return in, fmt.Errorf("queue arm requires standard input")
	}
	raw, err := io.ReadAll(io.LimitReader(env.Stdin, queueArmInputMaxBytes+1))
	if err != nil || len(raw) > queueArmInputMaxBytes {
		return in, fmt.Errorf("cannot read a bounded queue arm object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("queue arm reads one JSON object {queued_ref, terms_digest, desk_action_id, credential, envelope}")
	}
	if strings.TrimSpace(in.QueuedRef) == "" || strings.TrimSpace(in.TermsDigest) == "" {
		return in, fmt.Errorf("queue arm requires queued_ref and terms_digest")
	}
	return in, nil
}

func runProposalsQueueArm(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals queue arm")
	jsonOut := fs.Bool("json", false, "emit the arm result as JSON")
	fromStdin := fs.Bool("stdin", false, "read the arm object from standard input")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || !*fromStdin {
		return fail(env, "proposals queue arm requires --stdin; the queued reference never travels in argv")
	}
	in, err := readQueueArmInput(env)
	if err != nil {
		return fail(env, "%v", err)
	}
	var res rpc.TradeProposalQueueResult
	p := rpc.TradeProposalQueueArmParams{QueuedRef: in.QueuedRef, TermsDigest: in.TermsDigest, DeskActionID: in.DeskActionID,
		Credential: in.Credential, Envelope: in.Envelope, Origin: env.Origin}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsQueueArm, p, &res); err != nil {
		return fail(env, "proposals queue arm: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderQueueResult(env, "arm", &res)
}

func runProposalsQueueCancel(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals queue cancel")
	jsonOut := fs.Bool("json", false, "emit the cancel result as JSON")
	all := fs.Bool("all", false, "cancel every queued authorisation not yet sent")
	reason := fs.String("reason", "", "cancel reason")
	preparedOnly := fs.Bool("prepared-only", false, "cancel only while the record is still prepared (an unconfirmed review)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *all == (fs.NArg() == 1) || fs.NArg() > 1 {
		return fail(env, "proposals queue cancel: usage is `canary proposals queue cancel QUEUE_ID` or `canary proposals queue cancel --all`")
	}
	var res rpc.TradeProposalQueueResult
	p := rpc.TradeProposalQueueCancelParams{QueueID: fs.Arg(0), All: *all, Reason: strings.TrimSpace(*reason), Origin: env.Origin, PreparedOnly: *preparedOnly}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsQueueCancel, p, &res); err != nil {
		return fail(env, "proposals queue cancel: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderQueueResult(env, "cancel", &res)
}

func runProposalsQueueList(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals queue list")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	live := fs.Bool("live", false, "list only records that are not final")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "proposals queue list: usage is `canary proposals queue list [--live]`")
	}
	var res rpc.TradeProposalQueueListResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsQueueList, rpc.TradeProposalQueueListParams{LiveOnly: *live}, &res); err != nil {
		return fail(env, "proposals queue list: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	if len(res.Queues) == 0 {
		fmt.Fprintln(env.Stdout, "No queued authorisations.")
		return 0
	}
	for _, q := range res.Queues {
		renderQueuedAuth(env.Stdout, q)
	}
	return 0
}

func runProposalsQueueStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals queue status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "proposals queue status: usage is `canary proposals queue status QUEUE_ID`")
	}
	var res rpc.TradeProposalQueueResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsQueueStatus, rpc.TradeProposalQueueStatusParams{QueueID: fs.Arg(0)}, &res); err != nil {
		return fail(env, "proposals queue status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderQueueResult(env, "status", &res)
}

// renderQueueResult prints an arm, cancel or status result; a refusal exits
// non-zero with its first blocker.
func renderQueueResult(env *Env, verb string, res *rpc.TradeProposalQueueResult) int {
	if !res.Accepted {
		if len(res.Blockers) > 0 {
			return fail(env, "proposals queue %s: %s", verb, nonEmpty(res.Blockers[0].Message, res.Blockers[0].Code))
		}
		return fail(env, "proposals queue %s: %s", verb, nonEmpty(res.Message, "not accepted"))
	}
	if res.Message != "" {
		fmt.Fprintln(env.Stdout, res.Message)
	}
	if res.Queue != nil {
		renderQueuedAuth(env.Stdout, *res.Queue)
	}
	for _, q := range res.Queues {
		renderQueuedAuth(env.Stdout, q)
	}
	return 0
}

// renderQueuedAuth prints one record: what it may send, when, and where it
// stands, in local time.
func renderQueuedAuth(out io.Writer, q rpc.QueuedAuth) {
	t := q.Terms
	bound := "not below"
	if strings.EqualFold(t.Action, rpc.OrderActionBuy) {
		bound = "not above"
	}
	fmt.Fprintf(out, "%s  %s  %s at most %d %s, %s %s, %s–%s\n", t.QueueID, strings.ReplaceAll(q.State, "_", " "),
		strings.ToLower(t.Action), t.MaxQuantity, queuedContractLabel(t.Contract), bound, formatQueuedPrice(t.WorstPrice, t.Currency),
		t.NotBefore.Local().Format("Mon 2 Jan 15:04"), t.NotAfter.Local().Format("15:04 MST"))
	if q.HoldReason != "" {
		fmt.Fprintf(out, "      Held: %s (%s)\n", q.HoldReason, q.HoldCode)
	}
	if q.OrderRef != "" {
		fmt.Fprintf(out, "      Order: %s  %d at %s  filled %g\n", q.OrderRef, q.QuantitySent, formatQueuedPrice(q.LimitPrice, t.Currency), q.FilledQuantity)
	}
	if q.Reason != "" && q.ReasonCode != "" {
		fmt.Fprintf(out, "      %s: %s\n", strings.ReplaceAll(q.ReasonCode, "_", " "), q.Reason)
	}
}

func queuedContractLabel(c rpc.ContractParams) string {
	if c.LocalSymbol != "" {
		return c.LocalSymbol
	}
	if strings.EqualFold(c.SecType, "OPT") {
		return strings.TrimSpace(fmt.Sprintf("%s %s %g %s", c.Symbol, c.Expiry, c.Strike, strings.ToUpper(c.Right)))
	}
	return c.Symbol
}

// formatQueuedPrice prints a price with its currency.
func formatQueuedPrice(price float64, currency string) string {
	return strings.TrimSpace(fmt.Sprintf("%g %s", price, currency))
}
