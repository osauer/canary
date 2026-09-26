package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func applyReviewedTestConversions(t *testing.T, set PolicyFileSet, opts EnsureOptions) []PolicyFileAction {
	t.Helper()
	previewOpts := opts
	previewOpts.DryRun = true
	opts.Approved = map[string]PolicyMigrationApproval{}
	for _, a := range EnsurePolicyFiles(set, previewOpts) {
		if a.Action == PolicyFileFailed {
			t.Fatalf("preview failed: %+v", a)
		}
		if a.Action == PolicyFileWouldMigrate {
			opts.Approved[a.Policy] = PolicyMigrationApproval{Path: a.Path, BeforeSHA256: a.BeforeSHA256, AfterSHA256: a.AfterSHA256}
		}
	}
	if len(opts.Approved) == 0 {
		t.Fatal("fixture has no conversion")
	}
	return EnsurePolicyFiles(set, opts)
}

func TestReviewedPolicyConversionRejectsChangedOrUnknownFilesBeforeAnyWrite(t *testing.T) {
	for _, change := range []string{"edited", "removed", "already_converted", "unknown_policy", "wrong_path", "wrong_output"} {
		t.Run(change, func(t *testing.T) {
			set := policyTestSet(t)
			writePolicyTestFile(t, set.Protection, ownerLikeProtection)
			writePolicyTestFile(t, set.Constitution, ownerLikeConstitution)
			opts := EnsureOptions{Release: "test", Approved: map[string]PolicyMigrationApproval{}}
			for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
				if a.Action == PolicyFileWouldMigrate {
					opts.Approved[a.Policy] = PolicyMigrationApproval{a.Path, a.BeforeSHA256, a.AfterSHA256}
				}
			}
			a := opts.Approved[PolicyFileConstitution]
			switch change {
			case "edited":
				writePolicyTestFile(t, set.Constitution, ownerLikeConstitution+"\n# owner edit after review\n")
			case "removed":
				if err := os.Remove(set.Constitution); err != nil {
					t.Fatal(err)
				}
			case "already_converted":
				writePolicyTestFile(t, set.Constitution, string(ConstitutionPolicyTemplate("test")))
			case "unknown_policy":
				opts.Approved["unknown"] = a
			case "wrong_path":
				a.Path = filepath.Join(t.TempDir(), "elsewhere")
				opts.Approved[PolicyFileConstitution] = a
			case "wrong_output":
				a.AfterSHA256 = "wrong"
				opts.Approved[PolicyFileConstitution] = a
			}
			got := EnsurePolicyFiles(set, opts)
			if len(got) != 1 || got[0].Action != PolicyFileFailed {
				t.Fatalf("stale/unbound plan accepted: %+v", got)
			}
			if string(readPolicyTestFile(t, set.Protection)) != ownerLikeProtection {
				t.Fatal("another conversion ran before rejecting stale plan")
			}
			if _, err := os.Stat(set.Rulebook); !os.IsNotExist(err) {
				t.Fatal("apply created an unreviewed missing file")
			}
			if paths, _ := filepath.Glob(filepath.Join(filepath.Dir(set.Protection), "*.bak-*")); len(paths) != 0 {
				t.Fatal("rejected plan wrote backups")
			}
		})
	}
}

func TestReviewedPolicyConversionTouchesOnlySelectedFile(t *testing.T) {
	set := policyTestSet(t)
	writePolicyTestFile(t, set.Protection, ownerLikeProtection)
	applyReviewedTestConversions(t, set, EnsureOptions{Release: "test"})
	for _, path := range []string{set.Rulebook, set.Opportunity, set.Constitution} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unreviewed file created: %s", path)
		}
	}
}
