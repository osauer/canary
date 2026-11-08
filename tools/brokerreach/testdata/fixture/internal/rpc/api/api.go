// Package api is a synthetic stand-in for Canary's method declarations.
package api

// Method names one daemon method.
type Method[P, R any] struct{ Name string }

// Define declares a method.
func Define[P, R any](name string) Method[P, R] { return Method[P, R]{name} }

// The fixture's methods.
var (
	Clean    = Define[struct{}, int]("clean")
	Direct   = Define[struct{}, int]("direct")
	Callback = Define[struct{}, int]("callback")
	Seamed   = Define[struct{}, int]("seamed")
	Order    = Define[struct{}, int]("order")
)
