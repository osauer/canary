package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

type checkParamsRequest struct {
	Symbol string `json:"symbol"`
	Limit  int    `json:"limit,omitempty"`
}

var (
	checkParamsMethod = Define[checkParamsRequest, struct{}]("test.check_params")
	checkParamsNone   = Define[NoParams, struct{}]("test.check_params_none")
	checkParamsRaw    = Define[json.RawMessage, struct{}]("test.check_params_raw")
)

// A field the declared request lacks is refused, never dropped; a known
// request, an empty one and a free-form one pass.
func TestCheckParamsRefusesWhatTheDeclarationLacks(t *testing.T) {
	for _, tc := range []struct {
		method, params, refusal string
	}{
		{checkParamsMethod.Name, `{"symbol":"AAPL","limit":3}`, ""},
		{checkParamsMethod.Name, ``, ""},
		{checkParamsMethod.Name, `null`, ""},
		{checkParamsMethod.Name, `{"symbol":"AAPL","limt":3}`, `unknown field "limt"`},
		{checkParamsMethod.Name, `{"symbol":3}`, "cannot unmarshal"},
		{checkParamsMethod.Name, `{"symbol":"AAPL"} {}`, "more than one JSON value"},
		{checkParamsNone.Name, `{}`, ""},
		{checkParamsNone.Name, `{"symbol":"AAPL"}`, `unknown field "symbol"`},
		{checkParamsRaw.Name, `{"any":["shape"]}`, ""},
		{"test.not_declared", `{"anything":1}`, ""},
	} {
		err := CheckParams(tc.method, json.RawMessage(tc.params))
		switch {
		case tc.refusal == "" && err != nil:
			t.Errorf("%s %s: refused: %v", tc.method, tc.params, err)
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s %s: want refusal %q, got %v", tc.method, tc.params, tc.refusal, err)
		}
	}
}

// The same method may answer several result shapes, never two request types.
func TestDefineRefusesASecondRequestType(t *testing.T) {
	Define[checkParamsRequest, int](checkParamsMethod.Name)
	defer func() {
		if recover() == nil {
			t.Fatal("a second request type for one method was accepted")
		}
	}()
	Define[NoParams, struct{}](checkParamsMethod.Name)
}
