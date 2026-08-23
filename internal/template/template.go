// Package template implements the strict Liquid prompt layer (SPEC §5.6):
// unknown variables and unknown filters fail at load, not first render.
//
// Stock Liquid is lax, and osteele/liquid's strict options are render-time
// only, so load-time enforcement is this package's walk: every parsed node's
// expression is scanned for variable references, each is validated against
// the closed set (issue, attempt, workspace, run) down to its properties,
// and every filter name is probed against the engine. The render-time strict
// check (Engine.StrictVariables) stays on as the backstop for what the walk
// cannot see — chiefly property access on filter output.
//
// One consequence of the backstop worth knowing: a legitimately-null value
// ({{ attempt }} on numbered attempt 1, or {{ run.previous_outcome }} when the
// record has no prior outcome) emitted unguarded also fails the render, because
// the engine cannot distinguish null from undefined. Guard nullable emissions
// as the canonical publish snippet does.
package template

import (
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/expressions"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Vars is the closed template variable set (SPEC §5.6). One Vars describes
// one attempt.
type Vars struct {
	Issue core.Issue
	// Attempt is the 1-based attempt number. Values below 2 bind `attempt`
	// as null — the SPEC's "null/absent only when the numbered attempt is 1".
	Attempt int
	// Workspace is the absolute workspace path.
	Workspace string
	Run       Run
}

// Run is the `run` template variable.
type Run struct {
	// ID is unique per attempt.
	ID string
	// PreviousOutcome is empty when this record has no prior run outcome
	// (binds null), including an evidence-derived attempt floor; otherwise it
	// is "succeeded" or the failure reason string from SPEC §7.3.
	PreviousOutcome string
	// PreviousAttempt is the bounded account of the previous attempt: its
	// outcome, the commits and files it left on the branch, and the tail of what
	// it said (SPEC §5.6, §9.6). It is empty on the same occasions
	// PreviousOutcome is — attempt 1, an evidence-derived floor, a fresh host
	// after a restart — plus the §9.6 continuation track, which resumes a session
	// that already holds its own history.
	//
	// Empty binds the **empty string**, not null, unlike the two nullable members
	// above: no prompt can guard an untrusted variable with `{% if %}`, so an
	// unguarded emission is its only legal use, and a null emitted unguarded fails
	// the strict backstop. See untrustedOptional for the whole argument.
	//
	// Untrusted, and more so than an issue body: the agent that wrote it had
	// already read that body. It renders fenced and may only be emitted whole.
	PreviousAttempt string
}

// Prompt is a loaded, strictness-validated prompt template.
type Prompt struct {
	tpl *liquid.Template
}

var (
	enginesOnce sync.Once
	// renderEngine parses the real template and renders with the strict
	// backstop; probeEngine answers "does this filter exist" without
	// strictness noise.
	renderEngine *liquid.Engine
	probeEngine  *liquid.Engine
)

func engines() (*liquid.Engine, *liquid.Engine) {
	enginesOnce.Do(func() {
		renderEngine = liquid.NewEngine()
		renderEngine.StrictVariables()
		probeEngine = liquid.NewEngine()
	})
	return renderEngine, probeEngine
}

// Load parses and validates one prompt template. path and line locate the
// source inside its file (the body offset within WORKFLOW.md) so errors point
// at real file lines. Any error means the template is unusable: startup
// refuses, reload keeps last-known-good (SPEC §5.7).
func Load(source, path string, line int) (*Prompt, error) {
	render, probe := engines()
	tpl, err := render.ParseTemplateLocation([]byte(source), path, line)
	if err != nil {
		return nil, fmt.Errorf("prompt template: %w", err)
	}

	w := newWalker()
	if err := w.node(tpl.GetRoot()); err != nil {
		return nil, err
	}
	for name, loc := range w.filters {
		if !filterDefined(probe, name) {
			return nil, &UnknownFilterError{Name: name, File: loc.Pathname, Line: loc.LineNo}
		}
	}
	return &Prompt{tpl: tpl}, nil
}

// filterDefined asks the engine itself whether a filter exists, so the
// load-time check can never drift from what render would accept: an
// UndefinedFilter cause is the only "no" — any other render error (e.g. an
// argument-type complaint) proves the filter is registered.
//
// Unlike Prompt.Render, this load-time probe intentionally does not recover
// engine panics. A panic here is allowed to propagate as a loud load-time
// engine/configuration incompatibility, not be misclassified as an unknown
// filter; it is outside SPEC §5.7's per-run containment boundary.
// config.Watch contains a panic from this probe on the reload path and keeps
// the last-known-good runtime; the initial startup load still propagates it.
func filterDefined(probe *liquid.Engine, name string) bool {
	tpl, err := probe.ParseString("{{ nil | " + name + " }}")
	if err != nil {
		return false
	}
	_, rerr := tpl.RenderString(liquid.Bindings{})
	var err2 error = rerr
	for err2 != nil {
		se, ok := err2.(liquid.SourceError)
		if !ok {
			return true
		}
		cause := se.Cause()
		if cause == nil {
			return true
		}
		if _, undefined := cause.(expressions.UndefinedFilter); undefined {
			return false
		}
		err2 = cause
	}
	return true
}

// DefaultMaxPromptBytes is the ceiling a zero Limits applies. It is generous
// against any hand-written ticket and still bounds what an issue body can
// spend; a deployment tunes it through configuration rather than editing this.
const DefaultMaxPromptBytes = 256 << 10

// Limits bound one render. The zero value applies BEN's defaults, so a caller
// cannot leave the prompt unbounded by omission.
type Limits struct {
	// MaxPromptBytes caps the rendered prompt. Zero means
	// DefaultMaxPromptBytes; a negative value removes the ceiling, which only
	// a test has any business doing.
	MaxPromptBytes int
}

func (l Limits) maxPromptBytes() int {
	if l.MaxPromptBytes == 0 {
		return DefaultMaxPromptBytes
	}
	return l.MaxPromptBytes
}

// Render evaluates the prompt for one attempt. A residual failure here —
// the strict backstop, a filter error, or a prompt over the ceiling — fails
// only this run attempt (SPEC §5.7).
func (p *Prompt) Render(v Vars, limits Limits) (out string, err error) {
	b := bindings(v)
	// Keep recovery scoped to the Liquid call. If BEN's binding projection
	// above ever panics, it must not be mislabeled as an engine bug.
	defer func() {
		if value := recover(); value != nil {
			out = ""
			err = fmt.Errorf("rendering prompt template: %w", &EnginePanicError{
				Value: value,
				Stack: debug.Stack(),
			})
		}
	}()
	out, err = p.tpl.RenderString(b)
	if err != nil {
		return "", fmt.Errorf("rendering prompt template: %w", err)
	}
	// Measured on the rendered bytes, which is what the attempt is billed for
	// and what reaches the harness's stdin (SPEC §7.6).
	if max := limits.maxPromptBytes(); max > 0 && len(out) > max {
		return "", fmt.Errorf("rendering prompt template: %w", &PromptTooLargeError{Bytes: len(out), Max: max})
	}
	return out, nil
}
