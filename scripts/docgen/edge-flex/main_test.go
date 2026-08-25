package main

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestRenderContainsEveryCanonicalRequirement(t *testing.T) {
	body := render()
	for _, section := range flexstmt.CanonicalQueryManifest() {
		if !strings.Contains(body, "### "+section.Label) {
			t.Errorf("missing section %q", section.Label)
		}
		for _, field := range section.RequiredFields {
			if !strings.Contains(body, "- `"+field+"`") {
				t.Errorf("section %q missing field %q", section.Key, field)
			}
		}
	}
	for _, phrase := range []string{"Optional currency conversion rates", "its absence never blocks reporting setup", "not substitutes"} {
		if !strings.Contains(body, phrase) {
			t.Fatalf("generated reference missing optional FX guidance %q", phrase)
		}
	}
}
