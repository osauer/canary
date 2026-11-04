package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerConfigDecodesAndRefusesWhatCannotStart(t *testing.T) {
	load := func(t *testing.T, body string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	plain, err := load(t, "").Resolve()
	if err != nil || plain.IsOpenD() {
		t.Fatalf("no [broker] section: %+v %v", plain, err)
	}
	ibkr, err := load(t, "[broker]\nkind = \"ibkr\"\n").Resolve()
	if err != nil || ibkr.IsOpenD() {
		t.Fatalf("kind ibkr: %+v %v", ibkr, err)
	}

	opend, err := load(t, "[broker]\nkind = \"opend\"\n[opend]\nhost = \"10.0.0.2\"\nport = 11112\naccount = \"1000001\"\nclient_id = \"desk\"\n").Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !opend.IsOpenD() || opend.OpenD.HostOrDefault() != "10.0.0.2" || opend.OpenD.PortOrDefault() != 11112 || opend.OpenD.Account != "1000001" || opend.OpenD.ClientIDOrDefault() != "desk" {
		t.Fatalf("opend %+v", opend.OpenD)
	}
	defaults, err := load(t, "[broker]\nkind = \"opend\"\n[opend]\naccount = \"7\"\n").Resolve()
	if err != nil || defaults.OpenD.HostOrDefault() != "127.0.0.1" || defaults.OpenD.PortOrDefault() != DefaultOpenDPort || defaults.OpenD.ClientIDOrDefault() != "canary" {
		t.Fatalf("defaults %+v %v", defaults, err)
	}

	if _, err := load(t, "[broker]\nkind = \"opend\"\n").Resolve(); !errors.Is(err, ErrOpenDAccountRequired) {
		t.Fatalf("kind opend without an account: %v", err)
	} else if !strings.Contains(err.Error(), "[opend] account") {
		t.Fatalf("the refusal does not name the key: %v", err)
	}
	if _, err := load(t, "[broker]\nkind = \"opend\"\n[opend]\naccount = \"U1234567\"\n").Resolve(); err == nil || !strings.Contains(err.Error(), "accID") {
		t.Fatalf("an account that is not a number: %v", err)
	}
	if _, err := load(t, "[broker]\nkind = \"schwab\"\n").Resolve(); err == nil || !strings.Contains(err.Error(), "schwab") {
		t.Fatalf("an unknown kind: %v", err)
	}
}

func TestPolicyDirFollowsTheXDGConfigRootAndNotTheConfigFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CANARY_CONFIG", filepath.Join(t.TempDir(), "elsewhere.toml"))
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := PolicyDir(); got != "~/.config/ibkr/policies" {
		t.Fatalf("a file override moved the policies: %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	if got := PolicyDir(); got != filepath.Join(root, "ibkr", "policies") {
		t.Fatalf("policy dir %s", got)
	}
}

// A Futu frame never goes to a port where TWS or the Gateway listens.
func TestOpenDRefusesAnIBKRAPIPort(t *testing.T) {
	validate := func(port int) error {
		cfg := Config{Broker: Broker{Kind: "opend"}, OpenD: OpenD{Account: "1000001", Port: &port}}
		return cfg.ValidateBroker()
	}
	for _, port := range []int{7496, 7497, 4001, 4002} {
		if err := validate(port); err == nil || !strings.Contains(err.Error(), "IBKR API port") {
			t.Errorf("port %d: %v", port, err)
		}
	}
	if err := validate(11111); err != nil {
		t.Fatalf("OpenD's own port: %v", err)
	}
}
