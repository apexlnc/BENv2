package template

import (
	"time"

	"github.com/osteele/liquid"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The closed variable set of SPEC §5.6 is spelled exactly once, here. Each
// entry carries both halves of what a variable is: the shape the load-time
// walk checks references against, and the projection that produces the value
// the engine binds at render. Neither half can name a variable the other does
// not, because there is only one list of names.
//
// The two halves used to be separate lists, and a drift between them had no
// harmless direction: a name only the walk knew was a false refusal at load,
// and a name only the binding knew was an unknown variable reaching render —
// the failure §5.6 exists to prevent.

// field is one property of an object in the descriptor: the property's own
// shape, plus how to reach its Go value from T, the type of the object that
// owns it. T is what makes a mis-wired projection — a blocker field listed
// under issue — a compile error rather than a render-time panic.
type field[T any] struct {
	shape shape
	from  func(T) any
}

// object builds an object shape from properties that all project from T. The
// resulting shape binds a T into the map the engine sees.
func object[T any](fields map[string]field[T]) shape {
	props := make(map[string]shape, len(fields))
	tainted := false
	for name, f := range fields {
		props[name] = f.shape
		// An object holding fenced content anywhere beneath it carries the
		// taint: emitting the object emits the fence, and reshaping the object
		// reshapes the fence.
		tainted = tainted || f.shape.tainted
	}
	return shape{
		kind:    kindObject,
		fields:  props,
		tainted: tainted,
		bind: func(src any) any {
			owner := src.(T)
			out := make(map[string]any, len(fields))
			for name, f := range fields {
				out[name] = f.shape.bind(f.from(owner))
			}
			return out
		},
	}
}

// text and flag declare scalar properties.
func text[T any](get func(T) string) field[T] {
	return field[T]{shape: stringShape, from: func(t T) any { return get(t) }}
}

// untrusted declares a string property whoever filed the issue controls: it
// binds fenced under the name a prompt writes it as (fence.go), and the walk
// refuses every use but a whole emission. Marking it here is the point of the
// descriptor being one list — a variable cannot be added to the closed set and
// separately forgotten by the fencing rule.
func untrusted[T any](name, note string, get func(T) string) field[T] {
	return field[T]{
		shape: shape{kind: kindString, untrusted: true, tainted: true, bind: func(src any) any {
			return fence(name, note, src.(string))
		}},
		from: func(t T) any { return get(t) },
	}
}

// untrustedOptional is untrusted for a value that is legitimately absent on some
// attempts. Absent binds the **empty string**, not null, and anything else fences
// exactly as untrusted does.
//
// Empty rather than null, and this is the one place the untrusted span and the
// nullable members of §5.6 pull in opposite directions. `attempt` and
// `run.previous_outcome` bind null so that `{% if %}` sees a falsy value — Liquid
// treats "" as *true*, so empty would make every guard fire. But an untrusted
// variable may not appear in a condition at all (ErrUntrustedUse), so no prompt
// can test this one, so it gains nothing from null; and a null emitted unguarded
// fails the strict backstop, which is the only thing a prompt *can* do with it.
// Null would therefore make the variable unrenderable on exactly the attempts
// where it is absent, with no legal way to guard it.
//
// The shape stays untrusted and tainted whatever the value is, which is the point
// of declaring it here rather than branching inside a plain field: the walk's
// refusal is a property of the *variable*, decided once at load, so a prompt
// cannot pass validation on the attempt where the value is empty and refuse on the
// one where it is not.
func untrustedOptional[T any](name, note string, get func(T) string) field[T] {
	return field[T]{
		shape: shape{kind: kindString, untrusted: true, tainted: true, bind: func(src any) any {
			s := src.(string)
			if s == "" {
				// Not an empty fence: a fence around nothing is a delimiter pair
				// telling the agent to distrust the void.
				return ""
			}
			return fence(name, note, s)
		}},
		from: func(t T) any { return get(t) },
	}
}

func flag[T any](get func(T) bool) field[T] {
	return field[T]{shape: boolShape, from: func(t T) any { return get(t) }}
}

// nested declares a property whose value is another described object.
func nested[T, C any](child shape, get func(T) C) field[T] {
	return field[T]{shape: child, from: func(t T) any { return get(t) }}
}

// list declares an array property. Elements bind through elem, so an array of
// described objects projects element-wise rather than leaking Go structs into
// the engine.
func list[T, E any](elem shape, get func(T) []E) field[T] {
	s := shape{
		kind:    kindArray,
		elem:    &elem,
		tainted: elem.tainted,
		bind: func(src any) any {
			in := src.([]E)
			out := make([]any, len(in))
			for i, e := range in {
				out[i] = elem.bind(e)
			}
			return out
		},
	}
	return field[T]{shape: s, from: func(t T) any { return get(t) }}
}

// blockerBinding mirrors core.Blocker (SPEC §8.3).
var blockerBinding = object(map[string]field[core.Blocker]{
	"identifier": text(func(b core.Blocker) string { return b.Identifier }),
	"state":      text(func(b core.Blocker) string { return b.State }),
	"open":       flag(func(b core.Blocker) bool { return b.Open }),
})

// issueBinding mirrors the normalized issue model (SPEC §8.3, core.Issue)
// minus adapter-internal verdicts like Dispatchable, which §5.6 does not
// expose to a prompt.
//
// Title and body are the untrusted span: GitHub lets whoever filed the issue
// write and later edit both, and they are the two fields #49 binds queue
// approval to. Everything else here is the tracker's own answer — an
// identifier, a state, a URL, a timestamp — or, for labels and assignees,
// needs the triage rights that granting `ben-queue` already implies.
var issueBinding = object(map[string]field[core.Issue]{
	"identifier": text(func(i core.Issue) string { return i.Identifier }),
	"title":      untrusted("issue.title", fenceNoteIssue, func(i core.Issue) string { return i.Title }),
	"body":       untrusted("issue.body", fenceNoteIssue, func(i core.Issue) string { return i.Body }),
	"labels":     list(stringShape, func(i core.Issue) []string { return i.Labels }),
	"state":      text(func(i core.Issue) string { return i.State }),
	"assignees":  list(stringShape, func(i core.Issue) []string { return i.Assignees }),
	"blockers":   list(blockerBinding, func(i core.Issue) []core.Blocker { return i.Blockers }),
	"url":        text(func(i core.Issue) string { return i.URL }),
	// Timestamps bind as RFC 3339 strings: predictable prompt text, and the
	// date filter parses them.
	"created_at": text(func(i core.Issue) string { return i.CreatedAt.UTC().Format(time.RFC3339) }),
	"updated_at": text(func(i core.Issue) string { return i.UpdatedAt.UTC().Format(time.RFC3339) }),
})

var runBinding = object(map[string]field[Run]{
	"id": text(func(r Run) string { return r.ID }),
	"previous_outcome": {shape: stringShape, from: func(r Run) any {
		// SPEC §5.6: null when this record has no prior run outcome — which can
		// include an evidence-floored attempt 2 — otherwise "succeeded" or the
		// §7.3 failure reason.
		if r.PreviousOutcome == "" {
			return nil
		}
		return r.PreviousOutcome
	}},
	// The prior attempt's account, fenced (SPEC §5.6, #61). Doubly untrusted,
	// and that is the whole security content of the variable: it is agent output
	// produced by an agent that had already read the fenced issue body, so
	// anything an attacker can put in that body the agent can be induced to
	// restate — and this variable would carry it into the next prompt *outside*
	// the fence it arrived in, laundering it in one hop.
	//
	// Every string in it is agent-authored, including the ones git proves exist:
	// git is authoritative about a commit's existence and a file's path, never
	// about the text the agent chose for either. So BEN composes one preformatted
	// string and fences it whole, rather than an object whose safety would rest on
	// one field's presence.
	"previous_attempt": untrustedOptional("run.previous_attempt", fenceNoteAgent, func(r Run) string {
		return r.PreviousAttempt
	}),
})

// rootBinding is the closed variable set itself (SPEC §5.6).
var rootBinding = object(map[string]field[Vars]{
	"issue": nested(issueBinding, func(v Vars) core.Issue { return v.Issue }),
	"attempt": {shape: intShape, from: func(v Vars) any {
		// SPEC §5.6: null on the first attempt. A null root and an absent one
		// are indistinguishable to the engine — falsy in a condition, and a
		// strict-backstop failure if emitted unguarded — so the descriptor
		// binds the null rather than varying the key set per attempt.
		if v.Attempt < 2 {
			return nil
		}
		return v.Attempt
	}},
	"workspace": text(func(v Vars) string { return v.Workspace }),
	"run":       nested(runBinding, func(v Vars) Run { return v.Run }),
})

// rootScope is the walk's base scope. It is a copy: a top-level
// {% assign %} binds into the outermost scope map.
func rootScope() map[string]shape {
	scope := make(map[string]shape, len(rootBinding.fields))
	for name, s := range rootBinding.fields {
		scope[name] = s
	}
	return scope
}

// bindings projects one attempt's Vars into what the engine renders against.
func bindings(v Vars) liquid.Bindings {
	return rootBinding.bind(v).(map[string]any)
}
