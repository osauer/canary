package rpc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDefectCarriesTheHintAndUnwraps(t *testing.T) {
	base := errors.New("inconsistent Edge option coverage")
	err := Defect(base)
	if !strings.HasSuffix(err.Error(), DefectHint) || !strings.HasPrefix(err.Error(), base.Error()) {
		t.Fatalf("message = %q", err.Error())
	}
	if !errors.Is(err, base) || !IsDefect(fmt.Errorf("edge: %w", err)) {
		t.Fatal("a defect must unwrap to its cause and stay a defect when wrapped")
	}
	if Defect(nil) != nil || IsDefect(errors.New("plain")) {
		t.Fatal("nil stays nil; a plain error is no defect")
	}
}
