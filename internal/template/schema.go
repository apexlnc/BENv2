package template

// The load-time walk validates property paths, not just root names, so the
// closed set (SPEC §5.6) is modeled as shapes: what each variable is, which
// properties it admits, and what those resolve to. `issue.titel` is caught
// here; `issue.title` resolves to a string.
//
// The shapes of the closed set itself are not declared here — they are one
// half of the binding descriptor in binding.go, so that the walk and the
// render-time binding cannot spell the variable set differently. This file is
// the shape algebra the walk applies to them.

type kind int

const (
	// kindAny is the checking frontier: filter output and other values whose
	// type the walk cannot know statically. Every access on it is admitted;
	// the render-time strict check (SPEC §5.6 backstop) covers the residue.
	kindAny kind = iota
	kindString
	kindInt
	kindBool
	kindObject
	kindArray
)

type shape struct {
	kind   kind
	fields map[string]shape // kindObject: the closed property set
	elem   *shape           // kindArray: the element shape
	// bind projects the Go value this shape describes into the value the
	// engine sees at render. Set by the binding descriptor (binding.go), and
	// nil on the shapes the walk synthesizes for values BEN never binds: the
	// frontier, and engine-owned metadata like forloop.
	bind func(src any) any
	// untrusted marks a value that whoever filed the issue controls. It binds
	// fenced (fence.go).
	untrusted bool
	// tainted marks a value that *contains* fenced content — an untrusted leaf
	// itself, an object or array with one anywhere beneath it, or a string
	// captured from a body that emitted one. It is what the walk tests, not
	// untrusted: `issue` is not itself authored by the reporter, but
	// `{{ issue | json | slice: 0, 400 }}` still cuts the closing delimiter off
	// the body inside it. Taint has to propagate or the fence is decorative.
	tainted bool
}

// container reports whether descending into s reaches a distinct sub-value
// rather than a view of s itself. Descending through a tainted container is
// how a prompt reaches the trusted leaves under it (`issue.state`); descending
// into a tainted scalar only ever measures or slices the fence.
func (s shape) container() bool { return s.kind == kindObject || s.kind == kindArray }

var anyShape = shape{kind: kindAny}

// The scalar shapes bind their Go value as-is.
var (
	stringShape = scalarShape(kindString)
	intShape    = scalarShape(kindInt)
	boolShape   = scalarShape(kindBool)
)

func scalarShape(k kind) shape {
	return shape{kind: k, bind: func(src any) any { return src }}
}

// forloopShape is the implicit loop metadata object Liquid binds inside
// {% for %} and {% tablerow %} bodies — under that one name for both tags.
// The engine owns the value, so this shape carries no projection. The engine's
// map also carries a ".cycles" entry backing {% cycle %}; it is deliberately
// absent here (an engine internal, and not a reachable Liquid identifier).
// Asserted against the engine in tests.
var forloopShape = shape{kind: kindObject, fields: map[string]shape{
	"index":   intShape,
	"index0":  intShape,
	"rindex":  intShape,
	"rindex0": intShape,
	"first":   boolShape,
	"last":    boolShape,
	"length":  intShape,
}}

// property resolves a `.name` (or ["name"]) access. The engine's values
// package additionally defines size on strings and size/first/last on arrays;
// the schema admits exactly those (asserted against the engine in tests).
func (s shape) property(name string) (shape, bool) {
	switch s.kind {
	case kindAny:
		return anyShape, true
	case kindObject:
		f, ok := s.fields[name]
		return f, ok
	case kindString:
		if name == "size" {
			return intShape, true
		}
		return shape{}, false
	case kindArray:
		switch name {
		case "size":
			return intShape, true
		case "first", "last":
			return *s.elem, true
		}
		return shape{}, false
	default:
		return shape{}, false
	}
}

// keyIndex resolves a ["name"] access. A bracket is an index, not a property
// selector: the engine answers it with IndexValue, where a map yields its
// entry and everything else yields nothing. In particular the pseudo-
// properties — `size` on a string or array, `first` and `last` on an array —
// are PropertyValue's, so `labels["size"]` is nil where `labels.size` is 3.
func (s shape) keyIndex(name string) (shape, bool) {
	switch s.kind {
	case kindAny:
		return anyShape, true
	case kindObject:
		f, ok := s.fields[name]
		return f, ok
	default:
		return shape{}, false
	}
}

// index resolves an integer [n] access.
func (s shape) index() (shape, bool) {
	switch s.kind {
	case kindAny:
		return anyShape, true
	case kindArray:
		return *s.elem, true
	default:
		return shape{}, false
	}
}

// propertyNames lists the admitted properties, for error messages.
func (s shape) propertyNames() []string {
	switch s.kind {
	case kindObject:
		names := make([]string, 0, len(s.fields))
		for n := range s.fields {
			names = append(names, n)
		}
		return names
	case kindString:
		return []string{"size"}
	case kindArray:
		return []string{"size", "first", "last"}
	default:
		return nil
	}
}
