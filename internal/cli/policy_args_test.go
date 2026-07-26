package cli

import "testing"

// Run() hoists flags before positionals, so subcommand detection must skip
// flags and their values. Regression for the live 2026-07-13 miss where
// `canary policy capital-event reconcile --report X` arrived as
// ["--report","X","capital-event","reconcile"] and fell through to show.
func TestFirstPositionalIndexSkipsHoistedFlags(t *testing.T) {
	cases := []struct {
		args []string
		want int // index of the subcommand token, -1 when absent
	}{
		{[]string{"capital-event", "reconcile"}, 0},
		{[]string{"--report", "recon-abc", "capital-event", "reconcile"}, 2},
		{[]string{"--amount", "100", "--note", "wire", "capital-event", "deposit"}, 4},
		{[]string{"--explain", "show"}, 1},
		{[]string{"--explain"}, -1},
		{[]string{"--json", "dismiss", "--line", "cash-1"}, 1},
		{[]string{"--reason=already declared", "dismiss"}, 1}, // = form takes no extra token
		{[]string{}, -1},
	}
	for _, tc := range cases {
		if got := firstPositionalIndex(tc.args); got != tc.want {
			t.Fatalf("firstPositionalIndex(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}
