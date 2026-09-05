package config

import (
	"errors"
	"fmt"
	"math"

	"gopkg.in/yaml.v3"
)

// The numeric scalar types the raw schema decodes through (SPEC §5.2, §5.3).
//
// The loader's posture is strict and no-coercion — `version: "1"` is refused
// with the right value in the wrong type — but yaml.v3 coerces numbers on its
// own, below where any schema rule can see it:
//
//   - A `!!float` scalar decoded into an `int` field is **truncated silently**.
//     `version: 1.5` loaded as version 1, so the guard meant to refuse a file
//     this daemon does not understand accepted one by rounding it off;
//     `limits.max_attempts: 2.7` loaded as 2; and
//     `substrate.airlock.delete_after_idle_ms: 0.9` loaded as 0, which for that
//     key is the *valid* "leave the profile's own window" spelling — so an
//     operator's written window disappeared with no refusal anywhere. `-.inf`
//     is the sharpest of them: it decodes into an int as math.MinInt64.
//   - `.nan` and `.inf` decode into a `float64` field intact, and NaN then
//     defeats every ordered comparison written about it. `limits.max_cost_usd`
//     — the one knob bounding attacker-driven token spend — passed its
//     "must be positive when set" guard (`<= 0` is false for NaN), passed both
//     consumers' `> 0` gates, and reached the child argv as `+Inf`.
//
// This is a **dependency behaviour**, not a schema rule, and that is why it is
// refused here at the decode boundary rather than field by field afterwards: a
// numeric key added to the raw schema later inherits the refusal by declaring
// its type, and there is no list somewhere else to forget it in. validate keeps
// a finiteness rule of its own for the Config it may be handed without a file
// behind it.
type (
	// yamlInt is an integer field.
	//
	// It refuses by the node's **resolved tag**, not by comparing the value to
	// its own truncation: `1.0`, `1e3` and `-.inf` are all `!!float`, and "the
	// value happens to be integral" is not the contract this package keeps
	// anywhere else — a quoted `"1"` is refused for exactly that reason.
	yamlInt int

	// yamlFloat is a floating-point field, refusing the two spellings that are
	// numbers to YAML and not to anything that compares them.
	yamlFloat float64
)

// The float tag yaml.v3's resolver assigns; the one fact both types turn on.
const yamlFloatTag = "!!float"

func (n *yamlInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == yamlFloatTag {
		return &NonIntegerError{Value: node.Value, Line: node.Line, Column: node.Column}
	}
	var v int
	if err := node.Decode(&v); err != nil {
		// A *yaml.TypeError, deliberately returned unchanged: yaml.v3 accumulates
		// that kind with the rest of the document's type errors and reports them
		// together, which is what a wrong-typed value got before this type existed.
		return err
	}
	*n = yamlInt(v)
	return nil
}

// Int projects a decoded field onto the plain int the resolved Config holds.
//
// Deliberately not nil-safe, unlike Float below: a nil field means the key was
// absent, and every caller has to branch on that anyway to record provenance and
// pick the default. Reading it as 0 here would be the silent substitution this
// file exists to stop, one layer up.
func (n *yamlInt) Int() int { return int(*n) }

func (n *yamlFloat) UnmarshalYAML(node *yaml.Node) error {
	var v float64
	if err := node.Decode(&v); err != nil {
		return err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return &NonFiniteError{Value: node.Value, Line: node.Line, Column: node.Column}
	}
	*n = yamlFloat(v)
	return nil
}

// Float projects a decoded field onto a fresh *float64 for the resolved Config.
// A fresh one, because the config's pointer is its "is the cap set at all?"
// answer and must not alias the raw document. Nil-safe, because the config's
// field is a pointer too and nil is the value that carries "absent" forward.
func (n *yamlFloat) Float() *float64 {
	if n == nil {
		return nil
	}
	v := float64(*n)
	return &v
}

// The numeric-spelling refusals. Sentinels beside the types so a caller can
// assert on the refusal without reaching for its detail (AGENTS.md conventions).
var (
	ErrNonIntegerValue = errors.New("integer field written as a float")
	ErrNonFiniteValue  = errors.New("float field written as a non-finite value")
)

// NonIntegerError reports a float spelling written where the schema declares an
// integer (SPEC §5.3).
type NonIntegerError struct {
	// Field is the dotted path, resolved by load from the node's position.
	// Empty when the position could not be matched to a key — the line still
	// stands, so the refusal still says where to look.
	Field string
	// Value is the scalar exactly as the file wrote it.
	Value string
	// Line is the line in the **file**, not in the front matter (see
	// frontMatterFirstLine); Column is the same in both.
	Line, Column int
}

func (e *NonIntegerError) Error() string {
	return fmt.Sprintf("invalid %s: %s is not an integer — YAML would truncate it, and this loader does not coerce",
		numericSite(e.Field, "integer field", e.Line), e.Value)
}

func (e *NonIntegerError) Unwrap() error { return ErrNonIntegerValue }

func (e *NonIntegerError) position() (int, int)         { return e.Line, e.Column }
func (e *NonIntegerError) locate(path string, line int) { e.Field, e.Line = path, line }

// NonFiniteError reports `.nan` or `.inf` written into a float field.
type NonFiniteError struct {
	Field string
	Value string
	// Line is the line in the file — see NonIntegerError.
	Line, Column int
}

func (e *NonFiniteError) Error() string {
	return fmt.Sprintf("invalid %s: %s is not a finite number — every bound written about this value is false for it",
		numericSite(e.Field, "float field", e.Line), e.Value)
}

func (e *NonFiniteError) Unwrap() error { return ErrNonFiniteValue }

func (e *NonFiniteError) position() (int, int)         { return e.Line, e.Column }
func (e *NonFiniteError) locate(path string, line int) { e.Field, e.Line = path, line }

// numericSite renders the "invalid <site>" half of both messages.
func numericSite(field, kind string, line int) string {
	if field == "" {
		return fmt.Sprintf("%s at line %d", kind, line)
	}
	return fmt.Sprintf("%s (line %d)", field, line)
}

// numericRefusal is what the two have in common: the node position they are
// raised with, and somewhere to put the field path and the file line once
// something that can see the whole document has worked both out.
type numericRefusal interface {
	error
	position() (line, column int)
	locate(path string, line int)
}

// locateNumeric completes a numeric refusal — the dotted path of the key it
// belongs to, and its line in the file — or reports that err is not one.
//
// The refusal is raised inside a scalar's UnmarshalYAML, which is handed the
// value node alone: it knows neither the key above it nor that the document it
// is in was cut out of a larger file. Rather than teach every containing struct
// a decoder, this re-parses the front matter as a node tree — the same parse,
// kept as positions — and walks it for the node at that position. An alias is
// the exception: yaml.v3 hands UnmarshalYAML the anchor node, including the
// anchor's position, rather than the alias node that occupied this field. In
// that case the position cannot name the consuming field, so leave the field
// unnamed rather than blaming the unrelated key that owns the anchor. The line
// still says where the refused scalar was written.
func locateNumeric(err error, frontMatter string) (numericRefusal, bool) {
	var num numericRefusal
	if !errors.As(err, &num) {
		return nil, false
	}
	line, column := num.position()
	var path string
	var doc yaml.Node
	if e := yaml.Unmarshal([]byte(frontMatter), &doc); e == nil {
		if !nodePositionReachedByAlias(&doc, line, column) {
			path, _ = nodePathAt(&doc, "", line, column)
		}
	}
	num.locate(path, line+frontMatterFirstLine-1)
	return num, true
}

// nodePositionReachedByAlias reports whether an alias in the document can
// resolve to the node at line and column. It follows an alias once through the
// target's ordinary content, but never follows aliases inside that target: YAML
// permits recursive aliases, and all this check needs to know is whether the
// decoder could have replaced a consuming node's position with an anchor's.
func nodePositionReachedByAlias(n *yaml.Node, line, column int) bool {
	if n.Kind == yaml.AliasNode && nodeTreeHasPosition(n.Alias, line, column) {
		return true
	}
	for _, c := range n.Content {
		if nodePositionReachedByAlias(c, line, column) {
			return true
		}
	}
	return false
}

func nodeTreeHasPosition(n *yaml.Node, line, column int) bool {
	if n == nil {
		return false
	}
	if n.Line == line && n.Column == column {
		return true
	}
	for _, c := range n.Content {
		if nodeTreeHasPosition(c, line, column) {
			return true
		}
	}
	return false
}

func nodePathAt(n *yaml.Node, prefix string, line, column int) (string, bool) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if path, ok := nodePathAt(c, prefix, line, column); ok {
				return path, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			path := key.Value
			if prefix != "" {
				path = prefix + "." + key.Value
			}
			if value.Line == line && value.Column == column {
				return path, true
			}
			if p, ok := nodePathAt(value, path, line, column); ok {
				return p, true
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			path := appendProvenanceIndex(prefix, i)
			if c.Line == line && c.Column == column {
				return path, true
			}
			if p, ok := nodePathAt(c, path, line, column); ok {
				return p, true
			}
		}
	}
	return "", false
}
