package tui

import (
	"fmt"
	"strings"

	"github.com/osauer/ibkr/v2/internal/cli"
)

func confirmationFor(line string, catalog []cli.CommandSpec) (*confirmation, error) {
	tokens, err := parseCommandLine(line)
	if err != nil {
		return nil, err
	}
	if len(tokens) > 0 && tokens[0] == "ibkr" {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	cmd := tokens[0]
	args := tokens[1:]
	spec, ok := findSpec(catalog, cmd)
	if !ok {
		return nil, nil
	}
	if spec.TUI == cli.TUIExternal {
		return nil, nil
	}
	if commandNeedsConfirm(spec, args) {
		return &confirmation{
			line:    line,
			message: fmt.Sprintf("Confirm `%s` by typing yes, or press Esc to cancel.", strings.Join(tokens, " ")),
		}, nil
	}
	return nil, nil
}

func commandNeedsConfirm(spec cli.CommandSpec, args []string) bool {
	switch spec.Name {
	case "order":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "place", "modify", "cancel":
			return true
		default:
			return false
		}
	case "purge":
		return purgeNeedsConfirm(args)
	case "settings":
		// `set` writes a runtime preference, and trading.freeze plus the
		// trading-limit keys are accepted precisely because the TUI is an
		// interactive human terminal. That makes the prompt worth having.
		return len(args) > 0 && args[0] == "set"
	case "restart":
		return true
	case "update":
		return !hasFlag(args, "check")
	}
	return spec.Guard == cli.GuardConfirm
}

func purgeNeedsConfirm(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, arg := range args {
		switch arg {
		case "--save", "-save", "--save=true", "--record", "-record", "--record=true":
			return true
		}
	}
	for _, arg := range args {
		switch arg {
		case "status", "monitor":
			return false
		case "dry-run":
			return false
		case "restore":
			return hasFlag(args, "record") || hasFlag(args, "execute")
		case "execute":
			return true
		}
	}
	return true
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		raw := strings.TrimLeft(arg, "-")
		if i := strings.IndexByte(raw, '='); i >= 0 {
			raw = raw[:i]
		}
		if raw == name {
			return true
		}
	}
	return false
}
