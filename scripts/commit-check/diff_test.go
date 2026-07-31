package main

import (
	"bytes"
	"strings"
	"testing"
)

func rawMeta(oldMode, newMode, status string) string {
	return ":" + oldMode + " " + newMode + " " +
		strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " " + status
}

func TestParseRawDiffPreservesRenameAndUnusualPaths(t *testing.T) {
	t.Parallel()
	var raw bytes.Buffer
	raw.WriteString(rawMeta("100644", "100644", "M"))
	raw.WriteByte(0)
	raw.WriteString("docs/space and\nnewline.md")
	raw.WriteByte(0)
	raw.WriteString(rawMeta("100644", "100644", "R091"))
	raw.WriteByte(0)
	raw.WriteString("README.md")
	raw.WriteByte(0)
	raw.WriteString("web/app/read me.js")
	raw.WriteByte(0)

	got, err := parseRawDiff(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("changes = %d, want 2", len(got))
	}
	if got[0].status != 'M' || got[0].paths[0] != "docs/space and\nnewline.md" {
		t.Fatalf("modified entry = %+v", got[0])
	}
	if got[1].status != 'R' || len(got[1].paths) != 2 ||
		got[1].paths[0] != "README.md" || got[1].paths[1] != "web/app/read me.js" {
		t.Fatalf("rename entry = %+v", got[1])
	}
}

func TestParseRawDiffFailsClosedOnMalformedOrUnsupportedInput(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"bad metadata":       []byte("not raw\x00path\x00"),
		"bad mode":           []byte(rawMeta("888888", "100644", "M") + "\x00path\x00"),
		"unsupported status": []byte(rawMeta("100644", "100644", "U") + "\x00path\x00"),
		"missing rename path": []byte(
			rawMeta("100644", "100644", "R100") + "\x00old\x00",
		),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRawDiff(raw); err == nil {
				t.Fatalf("parseRawDiff(%q) succeeded", raw)
			}
		})
	}
}
