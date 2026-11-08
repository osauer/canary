package rpc

import (
	"context"
	"reflect"
	"slices"
)

// Method is one daemon method: its wire name, the request it takes and the
// result it answers. Package api declares one for every method. Clients send
// through Call and the daemon decodes through the same declaration, so the
// compiler holds both sides to one request type.
type Method[P, R any] struct{ Name string }

// NoParams is the request of a method that takes none.
type NoParams struct{}

// requestTypes is written only while package api initialises its declarations.
var requestTypes = map[string]reflect.Type{}

// Define declares method name with request P and result R. A method that
// answers several result shapes (account.summary's settlement diagnostics) has
// one declaration per shape; they must share P, and a second request type for
// the same name panics at start.
func Define[P, R any](name string) Method[P, R] {
	t := reflect.TypeFor[P]()
	if prev, ok := requestTypes[name]; ok && prev != t {
		panic("rpc: " + name + " is declared with two request types: " + prev.String() + " and " + t.String())
	}
	requestTypes[name] = t
	return Method[P, R]{Name: name}
}

// RequestType returns the request type declared for a method name.
func RequestType(name string) (reflect.Type, bool) {
	t, ok := requestTypes[name]
	return t, ok
}

// DeclaredMethods returns every declared method name, sorted.
func DeclaredMethods() []string {
	names := make([]string, 0, len(requestTypes))
	for name := range requestTypes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Caller sends one request and decodes its result into out: a daemon
// connection, or an in-process reader standing in for one.
type Caller interface {
	Call(ctx context.Context, method string, params, out any) error
}

// CallerFunc adapts a call function to Caller.
type CallerFunc func(ctx context.Context, method string, params, out any) error

// Call calls f.
func (f CallerFunc) Call(ctx context.Context, method string, params, out any) error {
	return f(ctx, method, params, out)
}

// Call sends method m with request p through c and decodes the result into
// out. NoParams is sent as no params, the same bytes a nil request sent
// before declarations.
func Call[P, R any](ctx context.Context, c Caller, m Method[P, R], p P, out *R) error {
	var params any = p
	if _, none := any(p).(NoParams); none {
		params = nil
	}
	return c.Call(ctx, m.Name, params, out)
}
