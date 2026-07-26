package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderVersionTextTrustReceiptShape(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	renderVersionText(&out, versionInfo{
		Program:     "canary",
		Version:     "v1.2.3",
		Commit:      "abcdef1234567890",
		VCSState:    "clean",
		Built:       "2026-05-25T18:42:00Z",
		Binary:      "/tmp/canary",
		BinaryMtime: "2026-05-25T19:00:00Z",
		GoVersion:   "go1.26.3",
		GOOS:        "darwin",
		GOARCH:      "arm64",
	}, versionTextStyle{})

	got := out.String()
	for _, want := range []string{
		"Canary CLI  v1.2.3",
		"Commit       abcdef1, clean tree",
		"Built        ",
		"Runtime      go1.26.3 darwin/arm64",
		"Binary       /tmp/canary",
		"Modified     ",
		"Trust        stamped build, clean tree",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("version output missing %q:\n%s", want, got)
		}
	}
}

func TestVersionTrust(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   versionInfo
		want string
	}{
		{
			name: "dirty wins",
			in:   versionInfo{Version: "v1.2.3-dirty", Commit: "abcdef1", Built: "2026-05-25T18:42:00Z", VCSState: "clean"},
			want: "dirty tree; rebuild after commit",
		},
		{
			name: "dev incomplete",
			in:   versionInfo{Version: "dev"},
			want: "local build; provenance incomplete",
		},
		{
			name: "clean stamped",
			in:   versionInfo{Version: "v1.2.3", Commit: "abcdef1", Built: "2026-05-25T18:42:00Z", VCSState: "clean"},
			want: "stamped build, clean tree",
		},
		{
			name: "stamped unknown tree",
			in:   versionInfo{Version: "v1.2.3", Commit: "abcdef1", Built: "2026-05-25T18:42:00Z"},
			want: "stamped build; tree state unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := versionTrust(tc.in)
			if got.Text != tc.want {
				t.Fatalf("versionTrust(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}
