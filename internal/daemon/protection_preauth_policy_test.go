package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func preAuthPolicyTOML(authority string, version int) string {
	return strings.Join([]string{
		`kind = "ibkr.protection_policy"`,
		`schema_version = 1`,
		`policy_id = "preauth-test"`,
		`policy_version = ` + strconv.Itoa(version),
		`[authority]`,
		`close_reduce_only = true`,
		`auto_submit = false`,
		authority,
		"",
	}, "\n")
}

func writePreAuthPolicy(t *testing.T, body string) *protectionPolicyManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "protection-policy.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return newProtectionPolicyManager(path, true, time.Minute, func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) })
}

func TestPreAuthorisedPolicyParsesListAndWindow(t *testing.T) {
	t.Parallel()
	m := writePreAuthPolicy(t, preAuthPolicyTOML("pre_authorised = [\"trailing_stop\", \"option_loss_exit\", \"option_profit_trail\", \"budget_reduction\"]\nveto_window = \"45m\"", 1))
	policy, source, err := m.loadPolicy()
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if source != "file" {
		t.Fatalf("source = %q, want file", source)
	}
	for _, bucket := range []string{"trailing_stop", "option_loss_exit", "option_profit_trail", "budget_reduction"} {
		if !policy.Authority.preAuthorised(bucket) {
			t.Fatalf("bucket %s not pre-authorised after parse", bucket)
		}
	}
	if policy.Authority.preAuthorised("theta_hygiene") || policy.Authority.preAuthorised("") {
		t.Fatal("a bucket outside the list reads as pre-authorised")
	}
	if got := policy.Authority.vetoWindow(); got != 45*time.Minute {
		t.Fatalf("veto window = %s, want 45m", got)
	}
}

func TestPreAuthorisedPolicyDefaultsToNothingAndThirtyMinutes(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	if len(policy.Authority.PreAuthorised) != 0 {
		t.Fatalf("embedded default pre-authorises %v", policy.Authority.PreAuthorised)
	}
	if got := policy.Authority.vetoWindow(); got != 30*time.Minute {
		t.Fatalf("default veto window = %s, want 30m", got)
	}
	m := writePreAuthPolicy(t, preAuthPolicyTOML("", 1))
	parsed, _, err := m.loadPolicy()
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if len(parsed.Authority.PreAuthorised) != 0 || parsed.Authority.vetoWindow() != 30*time.Minute {
		t.Fatalf("file without the keys parsed as %+v", parsed.Authority)
	}
	// A pre-existing file's fingerprint must not move: the new keys stay
	// out of the projection when absent.
	raw, _ := json.Marshal(parsed.Authority)
	if strings.Contains(string(raw), "pre_authorised") || strings.Contains(string(raw), "veto_window") {
		t.Fatalf("absent keys entered the fingerprint projection: %s", raw)
	}
}

func TestPreAuthorisedPolicyRejectsUnknownDuplicateAndShortWindow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		authority string
		want      string
	}{
		"unknown bucket":       {`pre_authorised = ["risk_reduction"]`, "not a pre-authorisable bucket"},
		"opening bucket":       {`pre_authorised = ["buy_add"]`, "not a pre-authorisable bucket"},
		"duplicate":            {`pre_authorised = ["trailing_stop", "trailing_stop"]`, "twice"},
		"short window":         {`veto_window = "4m"`, "below the 5m0s minimum"},
		"bad window":           {`veto_window = "soon"`, "not a duration"},
		"auto_submit stays no": {"auto_submit = true\npre_authorised = [\"trailing_stop\"]", "auto_submit must be false"},
		"close_reduce_only":    {"close_reduce_only = false\npre_authorised = [\"trailing_stop\"]", "close_reduce_only must be true"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := preAuthPolicyTOML(tc.authority, 1)
			if strings.Contains(tc.authority, "auto_submit = true") {
				body = strings.Replace(body, "auto_submit = false\n", "", 1)
			}
			if strings.Contains(tc.authority, "close_reduce_only = false") {
				body = strings.Replace(body, "close_reduce_only = true\n", "", 1)
			}
			m := writePreAuthPolicy(t, body)
			_, _, err := m.loadPolicy()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			m.reload()
			if st := m.Status(); st.Status != rpc.ProtectionPolicyStatusError {
				t.Fatalf("manager status = %q, want error (fail closed)", st.Status)
			}
		})
	}
}

func TestPreAuthorisedListChangeIsAPolicyRevision(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "protection-policy.toml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(preAuthPolicyTOML(`pre_authorised = []`, 1))
	m := newProtectionPolicyManager(path, true, time.Minute, func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) })
	m.reload()
	before := m.Status()
	if before.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("initial status = %+v", before)
	}
	// Adding a bucket without bumping the version is drift: the active
	// policy keeps the empty list and writes stay blocked.
	write(preAuthPolicyTOML(`pre_authorised = ["trailing_stop"]`, 1))
	m.reload()
	active, st := m.Active()
	if st.Status != rpc.ProtectionPolicyStatusDrift || active.Authority.preAuthorised("trailing_stop") {
		t.Fatalf("same-version edit adopted: status=%q active=%v", st.Status, active.Authority.PreAuthorised)
	}
	write(preAuthPolicyTOML(`pre_authorised = ["trailing_stop"]`, 2))
	m.reload()
	active, st = m.Active()
	if st.Status != rpc.ProtectionPolicyStatusActive || !active.Authority.preAuthorised("trailing_stop") || st.PolicyVersion != 2 {
		t.Fatalf("version bump not adopted: status=%q version=%d active=%v", st.Status, st.PolicyVersion, active.Authority.PreAuthorised)
	}
	if st.Fingerprint.Key == before.Fingerprint.Key {
		t.Fatal("pre_authorised change did not enter the policy fingerprint")
	}
}
