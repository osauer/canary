// Command upgrade-fixtures-manifest writes the immutable provenance manifest
// for historical upgrade artifacts staged by refresh.sh.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type manifest struct {
	Version         int       `json:"version"`
	DigestAlgorithm string    `json:"digest_algorithm"`
	Fixtures        []fixture `json:"fixtures"`
}

type fixture struct {
	ID             string         `json:"id"`
	Classification string         `json:"classification"`
	Source         fixtureSource  `json:"source"`
	ArtifactSHA256 string         `json:"artifact_sha256"`
	Files          []fixtureFile  `json:"files"`
	Expectations   map[string]any `json:"synthetic_expectations"`
}

type fixtureSource struct {
	Tag            string   `json:"tag"`
	PeeledCommit   string   `json:"peeled_commit"`
	GeneratorPaths []string `json:"generator_paths"`
}

type fixtureFile struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SourceMode  string `json:"source_mode"`
	InstallMode string `json:"install_mode"`
}

func main() {
	root := flag.String("root", "", "staged testdata/upgrades root")
	v171Commit := flag.String("v171-commit", "", "peeled v1.7.1 commit")
	v221Commit := flag.String("v221-commit", "", "peeled v2.2.1 commit")
	v230Commit := flag.String("v230-commit", "", "peeled v2.3.0 commit")
	v254Commit := flag.String("v254-commit", "", "peeled v2.5.4 commit")
	flag.Parse()
	if *root == "" || *v171Commit == "" || *v221Commit == "" || *v230Commit == "" || *v254Commit == "" {
		fatalf("-root, -v171-commit, -v221-commit, -v230-commit, and -v254-commit are required")
	}

	specs := []fixture{
		{
			ID: "v1.7.1-file-authority", Classification: "must_succeed",
			Source: fixtureSource{
				Tag: "v1.7.1", PeeledCommit: *v171Commit,
				GeneratorPaths: []string{
					"scripts/upgrade-fixtures/generators/v1_7_1_fixture_test.go.txt",
				},
			},
			Files: fileSpecs(
				"v1.7.1-file-authority/state/ibkr/order-journal.jsonl",
				"v1.7.1-file-authority/state/ibkr/purge-ledger.json",
			),
			Expectations: map[string]any{
				"purge_order_ref":          "fixture-v171-purge",
				"restore_order_ref":        "fixture-v171-restore",
				"global_order_id_floor":    1702,
				"purge_leg_id":             "fixture-v171-leg",
				"purged_quantity":          4,
				"restored_quantity":        1,
				"purge_remaining_quantity": 3,
				"purge_fill_cursor_count":  2,
				"route_endpoint":           "127.0.0.1:4002",
				"route_client_id":          37,
				"route_account":            "SYNTHETIC-V171-PAPER",
				"route_mode":               "paper",
			},
		},
		{
			ID: "v2.2.1-file-authority", Classification: "must_succeed",
			Source: fixtureSource{
				Tag: "v2.2.1", PeeledCommit: *v221Commit,
				GeneratorPaths: []string{
					"scripts/upgrade-fixtures/generators/v2_2_1_fixture_test.go.txt",
				},
			},
			Files: fileSpecs(
				"v2.2.1-file-authority/state/ibkr/platform-settings.json",
				"v2.2.1-file-authority/state/ibkr/risk-capital-state.json",
				"v2.2.1-file-authority/state/ibkr/capital-events.jsonl",
				"v2.2.1-file-authority/state/ibkr/order-journal.jsonl",
				"v2.2.1-file-authority/state/ibkr/purge-ledger.json",
			),
			Expectations: map[string]any{
				"trading_freeze":             true,
				"capital_adjusted_peak_base": 250000,
				"declared_capital_flow_base": 1000,
				"retained_order_ref":         "fixture-purge-active",
				"global_order_id_floor":      4201,
				"purge_leg_id":               "fixture-leg",
				"purge_remaining_quantity":   2,
			},
		},
		{
			ID: "v2.3.0-schema-v1-authority", Classification: "must_succeed",
			Source: fixtureSource{
				Tag: "v2.3.0", PeeledCommit: *v230Commit,
				GeneratorPaths: []string{
					"scripts/upgrade-fixtures/generators/v2_3_0_core_fixture_test.go.txt",
					"scripts/upgrade-fixtures/generators/v2_3_0_head_fixture_test.go.txt",
				},
			},
			Files: fileSpecs(
				"v2.3.0-schema-v1-authority/daemon.db",
				"v2.3.0-schema-v1-authority/daemon.db.head",
			),
			Expectations: map[string]any{
				"schema_version":                1,
				"authority_epoch":               "42424242424242424242424242424242",
				"head_generation":               1,
				"state_scope":                   "fixture/history",
				"state_kind":                    "fixture.state.v1",
				"state_json":                    `{"fixture":true,"sentinel":"v2.3.0-state-survives-upgrade","source_tag":"v2.3.0"}`,
				"defective_observation_scope":   "market/contracts",
				"defective_observation_source":  "ibkr.tws.contract_details",
				"defective_observation_kind":    "contract_cache.snapshot.v3",
				"defective_observation_payload": `{"fixture":true,"classification":"exact-defective-contract-cache"}`,
				"control_observation_kind":      "contract_cache.snapshot.v3.control",
				"control_observation_payload":   `{"fixture":true,"classification":"near-control-must-survive"}`,
			},
		},
		{
			ID: "v2.5.4-schema-v3-authority", Classification: "must_succeed",
			Source: fixtureSource{
				Tag: "v2.5.4", PeeledCommit: *v254Commit,
				GeneratorPaths: []string{
					"scripts/upgrade-fixtures/generators/v2_5_4_core_fixture_test.go.txt",
					"scripts/upgrade-fixtures/generators/v2_5_4_head_fixture_test.go.txt",
				},
			},
			Files: fileSpecs(
				"v2.5.4-schema-v3-authority/daemon.db",
				"v2.5.4-schema-v3-authority/daemon.db.head",
			),
			Expectations: map[string]any{
				"schema_version":                3,
				"authority_epoch":               "54545454545454545454545454545454",
				"head_generation":               1,
				"state_scope":                   "fixture/history",
				"state_kind":                    "fixture.state.v1",
				"state_json":                    `{"fixture":true,"sentinel":"v2.5.4-state-survives-maintenance","source_tag":"v2.5.4"}`,
				"defective_observation_scope":   "market/contracts",
				"defective_observation_source":  "ibkr.tws.contract_details",
				"defective_observation_kind":    "contract_cache.snapshot.v3",
				"defective_observation_payload": `{"fixture":true,"classification":"exact-defective-contract-cache-v3"}`,
				"control_observation_kind":      "contract_cache.snapshot.v3.control",
				"control_observation_payload":   `{"fixture":true,"classification":"near-control-v3-must-survive"}`,
			},
		},
	}

	for i := range specs {
		for j := range specs[i].Files {
			path := filepath.Join(*root, filepath.FromSlash(specs[i].Files[j].Path))
			info, err := os.Lstat(path)
			if err != nil {
				fatalf("inspect %s: %v", path, err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				fatalf("%s is not a private regular file: mode=%v", path, info.Mode())
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				fatalf("read %s: %v", path, err)
			}
			sum := sha256.Sum256(raw)
			specs[i].Files[j].SHA256 = hex.EncodeToString(sum[:])
		}
		sort.Slice(specs[i].Files, func(a, b int) bool {
			return specs[i].Files[a].Path < specs[i].Files[b].Path
		})
		specs[i].ArtifactSHA256 = artifactDigest(specs[i].Files)
	}

	raw, err := json.MarshalIndent(manifest{
		Version: 1, DigestAlgorithm: "sha256(path NUL source_mode NUL file_sha256 LF)",
		Fixtures: specs,
	}, "", "  ")
	if err != nil {
		fatalf("marshal manifest: %v", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(*root, "manifest.json"), raw, 0o644); err != nil {
		fatalf("write manifest: %v", err)
	}
}

func fileSpecs(paths ...string) []fixtureFile {
	out := make([]fixtureFile, 0, len(paths))
	for _, path := range paths {
		out = append(out, fixtureFile{
			Path: path, SourceMode: "0600", InstallMode: "0600",
		})
	}
	return out
}

func artifactDigest(files []fixtureFile) string {
	hash := sha256.New()
	for _, file := range files {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.SourceMode))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strings.ToLower(file.SHA256)))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "upgrade fixture manifest: "+format+"\n", args...)
	os.Exit(1)
}
