package template

import (
	"errors"
	"fmt"
)

// Named template errors (SPEC §5.6, §5.7).

// ErrEnginePanic marks a recovered panic from the Liquid engine. It is an
// engine bug, not an ordinary prompt render failure.
var ErrEnginePanic = errors.New("template engine panic")

// Located is implemented by every template error that carries a file and line.
//
// It exists so a *caller* can ask "does this error already say where it is?"
// without knowing which errors do. The config loader names the workflow file on
// every refusal it returns, and an error carrying `WORKFLOW.md:14` already
// answers that more precisely than the loader can — so it asks through this
// interface rather than listing types, which is a list that goes stale the first
// time a template error is added.
//
// Implemented by pointer receivers, matching how these errors are returned.
type Located interface {
	error
	// Location is the file and line the refusal points at.
	Location() (file string, line int)
}

// EnginePanicError preserves the value and stack from a recovered engine
// panic so the failed attempt has enough evidence for diagnosis. Error returns
// only the summary; diagnostic consumers can log Stack without leaking the
// full trace into compact failure reasons.
type EnginePanicError struct {
	Value any
	Stack []byte
}

func (e *EnginePanicError) Error() string {
	return fmt.Sprintf("%v: %v", ErrEnginePanic, e.Value)
}

// Unwrap deliberately exposes only the engine-bug classification. A panic
// value that happens to implement error is diagnostic evidence, not an
// underlying cause that callers should classify independently.
func (e *EnginePanicError) Unwrap() error {
	return ErrEnginePanic
}

// The strictness errors below fail the load; startup refuses and reload keeps
// last-known-good (B03).

// ErrUnknownVariable marks a variable path outside the closed set.
var ErrUnknownVariable = errors.New("unknown template variable")

// UnknownVariableError reports a template variable path outside the closed
// set — an unknown root, or a known root with an unknown property (the
// acceptance case: `{{ issue.titel }}` fails at load, not first render).
type UnknownVariableError struct {
	Ref    string // the offending path as written, e.g. "issue.titel"
	Detail string // what specifically is wrong, with the valid alternatives
	File   string
	Line   int
}

func (e *UnknownVariableError) Error() string {
	return fmt.Sprintf("unknown template variable %q at %s:%d: %s", e.Ref, e.File, e.Line, e.Detail)
}

func (e *UnknownVariableError) Location() (string, int) { return e.File, e.Line }

func (e *UnknownVariableError) Unwrap() error { return ErrUnknownVariable }

// ErrUnknownFilter marks a filter the engine does not define.
var ErrUnknownFilter = errors.New("unknown template filter")

// UnknownFilterError reports a filter the rendering engine does not define.
type UnknownFilterError struct {
	Name string
	File string
	Line int
}

func (e *UnknownFilterError) Error() string {
	return fmt.Sprintf("unknown template filter %q at %s:%d", e.Name, e.File, e.Line)
}

func (e *UnknownFilterError) Location() (string, int) { return e.File, e.Line }

func (e *UnknownFilterError) Unwrap() error { return ErrUnknownFilter }

// ErrReservedName marks a template binding a name the engine owns.
var ErrReservedName = errors.New("reserved template name")

// ReservedNameError reports a template binding a name the engine owns. A
// prompt may read such a name; binding it — as a loop binder, or an
// {% assign %}/{% capture %} target — is refused at load.
type ReservedNameError struct {
	Name   string
	Reason string
	File   string
	Line   int
}

func (e *ReservedNameError) Error() string {
	return fmt.Sprintf("reserved template name %q at %s:%d: %s", e.Name, e.File, e.Line, e.Reason)
}

func (e *ReservedNameError) Location() (string, int) { return e.File, e.Line }

func (e *ReservedNameError) Unwrap() error { return ErrReservedName }

// ErrUntrustedUse marks a refusal to let fenced content be reshaped, and
// ErrPromptTooLarge a prompt over the ceiling (SPEC §5.6). Both are the
// classification callers match on; the error values below carry the detail.
var (
	ErrUntrustedUse   = errors.New("untrusted template variable used outside a whole emission")
	ErrPromptTooLarge = errors.New("rendered prompt is over the ceiling")
)

// UntrustedUseError reports a template reading fenced content (SPEC §5.6)
// anywhere but as a whole emission. The restriction is what makes the fence
// dependable: a filter could truncate the closing delimiter off it, and a
// property access would measure or slice the fence rather than the content.
type UntrustedUseError struct {
	Ref    string
	Detail string
	File   string
	Line   int
}

func (e *UntrustedUseError) Error() string {
	return fmt.Sprintf("untrusted template variable %q at %s:%d: %s", e.Ref, e.File, e.Line, e.Detail)
}

func (e *UntrustedUseError) Location() (string, int) { return e.File, e.Line }

func (e *UntrustedUseError) Unwrap() error { return ErrUntrustedUse }

// PromptTooLargeError reports a rendered prompt over the configured ceiling.
// The attempt is refused rather than truncated: an issue body is
// attacker-controlled token spend billed against `limits.max_cost_usd`, and
// truncating would also cut the closing fence off the untrusted span.
type PromptTooLargeError struct {
	Bytes int
	Max   int
}

func (e *PromptTooLargeError) Error() string {
	return fmt.Sprintf("rendered prompt is %d bytes, over the %d-byte ceiling; refusing rather than truncating",
		e.Bytes, e.Max)
}

func (e *PromptTooLargeError) Unwrap() error { return ErrPromptTooLarge }

// ErrUnsupportedTag marks a tag outside the v1 prompt template contract.
var ErrUnsupportedTag = errors.New("unsupported template tag")

// UnsupportedTagError reports a tag the engine parses but the prompt contract
// does not admit (v1: `include` — a prompt is a single self-contained
// template).
type UnsupportedTagError struct {
	Tag    string
	Reason string
	File   string
	Line   int
}

func (e *UnsupportedTagError) Error() string {
	return fmt.Sprintf("unsupported template tag {%% %s %%} at %s:%d: %s", e.Tag, e.File, e.Line, e.Reason)
}

func (e *UnsupportedTagError) Location() (string, int) { return e.File, e.Line }

func (e *UnsupportedTagError) Unwrap() error { return ErrUnsupportedTag }
