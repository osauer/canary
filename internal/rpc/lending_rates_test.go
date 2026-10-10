package rpc

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeLendingRateSymbols(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"normalize", []string{" xyz ", "ABC.X", "xyz", "123"}, []string{"123", "ABC.X", "XYZ"}},
		{"max length", []string{strings.Repeat("X", 32)}, []string{strings.Repeat("X", 32)}},
		{"empty", nil, nil},
		{"too many", make([]string, 101), nil},
		{"blank", []string{" "}, nil},
		{"too long", []string{strings.Repeat("X", 33)}, nil},
		{"leading dot", []string{".XYZ"}, nil},
		{"punctuation", []string{"X-Y"}, nil},
		{"non ASCII", []string{"Ä"}, nil},
		{"embedded whitespace", []string{"X Y"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeLendingRateSymbols(tc.in)
			if (err != nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeLendingRateSymbolsAllocationBudget(t *testing.T) {
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got, err := NormalizeLendingRateSymbols([]string{"XYZ"})
			if err != nil || len(got) != 1 || got[0] != "XYZ" {
				b.Fatal("normalization changed", err)
			}
		}
	})
	t.Logf("one symbol: %d bytes/op, %d ns/op", result.AllocedBytesPerOp(), result.NsPerOp())
	if result.AllocedBytesPerOp() > 1024 {
		t.Fatal("symbol normalization recompiles its validation expression")
	}
}
