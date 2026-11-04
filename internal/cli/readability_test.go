package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestHelpListsEveryRegisteredCommand(t *testing.T) {
	t.Setenv("CANARY_COLOR", "never")
	var out bytes.Buffer
	PrintUsage(&out)
	words := strings.Fields(out.String())
	for _, spec := range Catalog() {
		if !slices.Contains(words, spec.Name) {
			t.Errorf("registered command %q cannot be found in top-level help", spec.Name)
		}
	}
}

func TestCalendarCatalogCoversAvailableMarkets(t *testing.T) {
	listed := map[marketcal.Market]bool{}
	for _, spec := range Catalog() {
		if spec.Name != "calendar" {
			continue
		}
		for _, flag := range spec.Flags {
			if flag.Name != "market" {
				continue
			}
			for _, value := range flag.Values {
				market, ok := marketcal.NormalizeMarket(value)
				if !ok {
					t.Errorf("calendar documentation lists unsupported market %q", value)
				}
				listed[market] = true
			}
		}
	}
	for _, market := range marketcal.AllMarkets() {
		if !listed[market] {
			t.Errorf("available calendar %q cannot be found in the documented market choices", market)
		}
	}
}

func TestHelpActionsNeedNoConnection(t *testing.T) {
	for _, parent := range []string{"order", "orders", "proposals", "proposals queue"} {
		for _, action := range commandActions(parent) {
			argv := append(strings.Fields(parent), action.name, "--help")
			t.Run(strings.Join(argv, "_"), func(t *testing.T) {
				var out, errOut bytes.Buffer
				// A nil connection makes accidental reads or broker calls fail this test.
				if code := Run(t.Context(), &Env{Stdout: &out, Stderr: &errOut}, argv[0], argv[1:]); code != 0 {
					t.Fatalf("exit %d: %s", code, errOut.String())
				}
				if !strings.Contains(out.String(), "canary "+parent+" "+action.name) {
					t.Fatal("help lost its action name", out.String())
				}
				if action.name != "queue" && !strings.Contains(out.String(), "Flags") {
					t.Fatal("missing applicable flags", out.String())
				}
			})
		}
	}
}

func TestReadableOutputFitsTerminal(t *testing.T) {
	names := map[string]bool{"help": true, "help_order": true, "help_order_preview": true, "help_proposals": true, "help_orders_history": true, "help_proposals_queue": true, "order_preview_bond": true, "order_preview_ineligible": true, "positions_bond_risk": true, "trading_status": true, "settings_show": true, "stress": true, "stress_details": true}
	for _, width := range []int{80, 120} {
		t.Setenv("COLUMNS", fmt.Sprint(width))
		for _, tc := range renderGoldenCases() {
			if names[tc.name] {
				for _, color := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%d/color=%t", tc.name, width, color), func(t *testing.T) {
						out := renderGolden(t, tc, color)
						for line := range strings.SplitSeq(out, "\n") {
							if visibleLen(line) > width {
								t.Fatalf("%d cells exceeds %d: %q", visibleLen(line), width, line)
							}
						}
					})
				}
			}
		}
	}
}

func TestPreviewEligibilityIsNotTokenAuthority(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		for _, minted := range []bool{false, true} {
			var out bytes.Buffer
			r := goldenBondPreview()
			r.SubmitEligible = eligible
			r.TokenMinted = minted
			renderOrderPreviewText(&Env{Stdout: &out}, &r)
			headline, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
			want := "NOT ELIGIBLE TO SUBMIT"
			if eligible {
				want = "ELIGIBLE TO SUBMIT"
			}
			if !strings.HasSuffix(headline, want) || eligible && strings.Contains(headline, "NOT ELIGIBLE") {
				t.Fatal("headline did not follow eligibility", headline)
			}
			if !strings.Contains(out.String(), "No order submitted.") || !strings.Contains(out.String(), fmt.Sprintf("Token minted   %t", minted)) {
				t.Fatal("lost separate token/submission facts", out.String())
			}
			if eligible && !strings.Contains(out.String(), "Submission still requires approval and trading checks.") {
				t.Fatal("eligibility portrayed as authority")
			}
		}
	}
}

func TestStressAssessmentTimeAlwaysVisible(t *testing.T) {
	for _, details := range []bool{false, true} {
		for _, dated := range []bool{false, true} {
			r := goldenStress()
			want := r.AsOf.Local().Format("2 Jan 15:04 MST")
			if !dated {
				r.AsOf = time.Time{}
				want = "unavailable"
			}
			var out bytes.Buffer
			renderStress(&Env{Stdout: &out}, r, details)
			if !strings.Contains(out.String(), "Assessment generated") || !strings.Contains(out.String(), want) || strings.Count(out.String(), "Assessment generated") != 1 {
				t.Fatal("missing or duplicate assessment time", out.String())
			}
		}
	}
}

func TestPresentationLeavesJSONUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		argv   []string
		method string
		result any
	}{
		{"preview", []string{"order", "preview", "buy", "DE000SYN0000", "10000", "--type", "BOND", "--currency", "EUR", "--json"}, rpc.MethodOrderPreview, goldenBondPreview()},
		{"stress", []string{"stress", "--json"}, rpc.MethodStressSnapshot, goldenStress()},
		{"settings", []string{"settings", "show", "--json"}, rpc.MethodSettingsGet, goldenSettings()},
		{"trading", []string{"trading", "status", "--json"}, rpc.MethodTradingStatus, rpc.TradingStatus{Blocked: true, Freeze: true, CanWrite: false}},
		{"orders", []string{"orders", "history", "--json"}, rpc.MethodOrdersHistory, rpc.OrdersHistoryResult{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expected, err := json.MarshalIndent(tc.result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range []string{"80", "120"} {
				t.Setenv("COLUMNS", width)
				for _, color := range []bool{false, true} {
					var out, errOut bytes.Buffer
					reply := tc.result
					if tc.method == rpc.MethodStressSnapshot {
						reply = rpc.StressSnapshotResult{Stress: tc.result.(rpc.StressResult)}
					}
					env := &Env{Stdout: &out, Stderr: &errOut, Color: color, Conn: goldenConn{tc.method: reply}}
					if code := Run(t.Context(), env, tc.argv[0], tc.argv[1:]); code != 0 {
						t.Fatalf("exit %d: %s", code, errOut.String())
					}
					if out.String() != string(expected)+"\n" {
						t.Fatalf("JSON changed at width=%s color=%t:\n%s", width, color, out.String())
					}
				}
			}
		})
	}
}

func TestSharedRowsKeepStylesAndAllWords(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	var out bytes.Buffer
	env := &Env{Stdout: &out, Color: true}
	value := strings.Repeat("unusually-long-contract-identifier", 5)
	statusRow(env, &out, "Long label beyond column", env.yellow(value))
	sgr := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	plain := sgr.ReplaceAllString(out.String(), "")
	if strings.Contains(plain, "\x1b") {
		t.Fatal("split ANSI escape", out.String())
	}
	joined := strings.Join(strings.Fields(strings.TrimPrefix(plain, "Long label beyond column\n")), "")
	if joined != value {
		t.Fatal("lost characters during wrapping", plain)
	}
	for line := range strings.SplitSeq(plain, "\n") {
		if len(line) > 80 {
			t.Fatal("row exceeds width", line)
		}
	}
	out.Reset()
	displayRow(env, &out, "External", "text\x1b[2J\nforged")
	if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "\nforged") {
		t.Fatal("display row lost sanitization", out.String())
	}
}

func TestHelpColourPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode, noColor string
		colored       bool
	}{{"", "", false}, {"", "1", false}, {"always", "1", true}, {"never", "", false}} {
		t.Setenv("CANARY_COLOR", tc.mode)
		t.Setenv("NO_COLOR", tc.noColor)
		var out bytes.Buffer
		PrintUsage(&out)
		if strings.Contains(out.String(), ansiCyan) != tc.colored || strings.Contains(out.String(), ansiBold) != tc.colored {
			t.Fatalf("colour policy %q/%q mismatch", tc.mode, tc.noColor)
		}
	}
}

func TestPreviewTokensRemainCopyable(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	token := strings.Repeat("synthetic-token-segment_", 40) + ".signature"
	for _, color := range []bool{false, true} {
		for _, opportunity := range []bool{false, true} {
			var out bytes.Buffer
			env := &Env{Stdout: &out, Color: color}
			if opportunity {
				renderOpportunityPreviewText(env, &rpc.OpportunityExercisePreviewResult{PreviewToken: token})
			} else {
				r := goldenBondPreview()
				r.PreviewToken = token
				renderOrderPreviewText(env, &r)
			}
			if !strings.Contains(out.String(), token+"\n") || strings.Count(out.String(), token) != 1 {
				t.Fatal("token is split or duplicated")
			}
		}
	}
}

func TestPreflightRequirementWording(t *testing.T) {
	for _, tc := range []struct {
		status   string
		required bool
		want     string
	}{
		{rpc.OrderWhatIfStatusAccepted, false, "accepted · requirement satisfied"},
		{rpc.OrderWhatIfStatusUnavailable, true, "unavailable · required before submission"},
		{rpc.OrderWhatIfStatusRejected, true, "rejected · required before submission"},
	} {
		var out bytes.Buffer
		r := goldenBondPreview()
		r.WhatIf.Status, r.WhatIf.RequiredForSubmit = tc.status, tc.required
		renderOrderPreviewText(&Env{Stdout: &out}, &r)
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), tc.want) {
			t.Fatal("misleading broker preflight", out.String())
		}
	}
}

func TestNarrowBondTableKeepsComparisonAndDetails(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	rows, _ := goldenBondRisk()
	rows[0].Reason = "Long synthetic explanation that must remain attached to this bond and wrap without losing its final words."
	var out bytes.Buffer
	renderBondsTable(&Env{Stdout: &out}, &out, rows)
	plain := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(plain, "SYMBOL CLASS CCY MATURITY DAYS POS MARK VALUE") {
		t.Fatal("lost comparable columns", out.String())
	}
	for _, row := range rows {
		for _, fact := range []string{row.Symbol, row.ISIN, row.CUSIP, row.Reason} {
			if fact != "" && !strings.Contains(plain, fact) {
				t.Fatal("lost bond detail", fact)
			}
		}
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if visibleLen(line) > 80 {
			t.Fatal("overflow", line)
		}
	}
}

func TestNarrowBondsAttachIdentityToRepeatedSymbols(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	rows, _ := goldenBondRisk()
	rows[0].Symbol, rows[1].Symbol = "SYNB", "SYNB"
	var out bytes.Buffer
	renderBondsTable(&Env{Stdout: &out}, &out, rows)
	lines := strings.Split(out.String(), "\n")
	index := 0
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "SYNB ") {
			id := rows[index].ISIN
			if id == "" {
				id = rows[index].CUSIP
			}
			if i+1 >= len(lines) || !strings.Contains(lines[i+1], id) {
				t.Fatal("identity detached from bond", out.String())
			}
			index++
		}
	}
	if index != 2 {
		t.Fatal("missing repeated-symbol bond", out.String())
	}
}
