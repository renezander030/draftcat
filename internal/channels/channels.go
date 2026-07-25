// Package channels is the single source of truth for which operator channels
// draftcat actually implements.
//
// The validator (internal/validate) and the engine (package main) both read
// this list, so a config that passes `draftcat validate` cannot name a channel
// the engine would silently ignore at runtime. Before this package existed the
// two disagreed: "slack" was accepted by the validator, no Slack channel was
// ever built, and step.Channel was never read — so an operator who wrote
// `channel: slack` got a clean validate run and their approvals went to
// Telegram anyway. An approval gate that routes somewhere the operator is not
// watching is worse than no gate, because it still looks like it held.
//
// A name belongs here ONLY when a working OperatorChannel implementation ships
// in the binary. Adding a name without an implementation reintroduces exactly
// the trap this package exists to close.
package channels

import "sort"

// Telegram is the only implemented operator channel today.
const Telegram = "telegram"

// implemented maps channel name -> one-line description, shown in validator
// messages so the operator sees what they can actually pick.
var implemented = map[string]string{
	Telegram: "Telegram bot with inline approve/skip/adjust buttons",
}

// IsImplemented reports whether name is a channel the engine can actually
// route an approval to.
func IsImplemented(name string) bool {
	_, ok := implemented[name]
	return ok
}

// Names returns the implemented channel names, sorted, for error messages.
func Names() []string {
	out := make([]string, 0, len(implemented))
	for k := range implemented {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Describe returns the one-line description for name, or "" if unimplemented.
func Describe(name string) string { return implemented[name] }
