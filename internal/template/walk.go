package template

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osteele/liquid/parser"
	"github.com/osteele/liquid/render"
)

// The load-time strictness walk (SPEC §5.6): visit every node of the parsed
// template — including branches a given render never takes — validate each
// variable reference against the closed set, and collect filter names for
// the engine probe. Fails fast with the first violation.
//
// Scoping: tags that bind names are honored. {% assign %} and {% capture %}
// make the name visible from that point to the end of the enclosing block;
// {% for %}/{% tablerow %} bind the loop variable and forloop inside the body
// only. A name bound inside a branch is deliberately not visible after the
// branch closes — it may be undefined at render, and strictness rejects
// maybe-undefined.

type walker struct {
	scopes  []map[string]shape
	filters map[string]parser.SourceLoc // filter name → first use, for the probe
	// taintedEmissions counts whole emissions of fenced content, so
	// {% capture %} can tell whether the string it just built contains a
	// fence. Without it, capture launders: the emission inside the body is
	// legal, and the captured string would come back out as an ordinary one
	// that a filter is free to truncate the closing delimiter off.
	taintedEmissions int
}

func newWalker() *walker {
	return &walker{
		scopes:  []map[string]shape{rootScope()},
		filters: map[string]parser.SourceLoc{},
	}
}

func (w *walker) push() { w.scopes = append(w.scopes, map[string]shape{}) }
func (w *walker) pop()  { w.scopes = w.scopes[:len(w.scopes)-1] }

func (w *walker) bind(name string, s shape) { w.scopes[len(w.scopes)-1][name] = s }

// reservedBindings are names the engine owns. A template may read them;
// binding one is refused. Modeling the engine's shadowing rules instead would
// not be enough, for two reasons:
//
//   - Precedence runs the other way. loopRenderer.render sets forloop once and
//     then the loop variable on every iteration, so a binder named forloop
//     wins and the metadata becomes unreachable — {{ forloop.index }} would
//     load against the metadata shape and fail at render on the element.
//   - Worse, {% cycle %} type-asserts forloop back to the loop record it wrote
//     (osteele/liquid tags/iteration_tags.go:59-60, whose own comment concedes
//     "could panic if the user spoofs us"). That is a panic, not a render
//     error, so run-containment (SPEC §5.7) could not hold.
//
// Refusing the bind closes both, and makes the two binds in the loop case
// below incapable of colliding.
var reservedBindings = map[string]string{
	"forloop": "the engine owns forloop inside a loop body; a prompt may read it, not bind it",
}

// checkBindable refuses a template's attempt to bind an engine-owned name.
func (w *walker) checkBindable(name string, loc parser.SourceLoc) error {
	if reason, ok := reservedBindings[name]; ok {
		return &ReservedNameError{Name: name, Reason: reason, File: loc.Pathname, Line: loc.LineNo}
	}
	return nil
}

func (w *walker) lookup(name string) (shape, bool) {
	for i := len(w.scopes) - 1; i >= 0; i-- {
		if s, ok := w.scopes[i][name]; ok {
			return s, true
		}
	}
	return shape{}, false
}

func (w *walker) inScope() []string {
	var names []string
	for _, sc := range w.scopes {
		for n := range sc {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (w *walker) node(n render.Node) error {
	switch n := n.(type) {
	case *render.SeqNode:
		for _, c := range n.Children {
			if err := w.node(c); err != nil {
				return err
			}
		}
	case *render.ObjectNode:
		return w.emission(n.Args, n.SourceLocation())
	case *render.TagNode:
		return w.tag(n.Name, n.Args, n.SourceLocation())
	case *render.BlockNode:
		return w.block(n)
	}
	// TextNode, RawNode, TrimNode carry no expressions.
	return nil
}

func (w *walker) block(n *render.BlockNode) error {
	loc := n.SourceLocation()
	switch n.Name {
	case "comment", "raw":
		// Dead text; the engine never evaluates it (and normally elides it
		// from the tree). Skipped defensively.
		return nil

	case "if", "unless", "case":
		if err := w.expr(n.Args, loc); err != nil {
			return err
		}
		if err := w.scopedBody(n.Body); err != nil {
			return err
		}
		for _, cl := range n.Clauses {
			// elsif and when conditions evaluate in the enclosing scope.
			if cl.Args != "" {
				if err := w.expr(cl.Args, cl.SourceLocation()); err != nil {
					return err
				}
			}
			if err := w.scopedBody(cl.Body); err != nil {
				return err
			}
		}
		return nil

	case "for", "tablerow":
		binder, elem, err := w.loopArgs(n.Name, n.Args, loc)
		if err != nil {
			return err
		}
		if err := w.checkBindable(binder, loc); err != nil {
			return err
		}
		w.push()
		w.bind(binder, elem)
		// forloop, not <tag>loop: the engine binds the same name for both loop
		// tags (osteele/liquid tags/iteration_tags.go forloopVarName). The tag
		// name selects the decorator — tablerow's <tr>/<td> emission — not the
		// metadata variable, and this engine has no tablerowloop.
		w.bind("forloop", forloopShape)
		err = w.body(n.Body)
		w.pop()
		if err != nil {
			return err
		}
		for _, cl := range n.Clauses { // {% else %}: empty collection
			if err := w.scopedBody(cl.Body); err != nil {
				return err
			}
		}
		return nil

	case "capture":
		toks, err := lex(n.Args)
		if err != nil {
			return fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
		}
		if len(toks) != 1 || toks[0].kind != tokIdent {
			return fmt.Errorf("malformed {%% capture %%} target %q at %s:%d", n.Args, loc.Pathname, loc.LineNo)
		}
		if err := w.checkBindable(toks[0].text, loc); err != nil {
			return err
		}
		before := w.taintedEmissions
		if err := w.scopedBody(n.Body); err != nil {
			return err
		}
		// A capture over fenced content produces a string that still holds the
		// fence, so the binding carries the taint forward — {{ c }} stays
		// legal, {{ c | slice: 0, 200 }} does not. Nested captures compose:
		// emitting the tainted `c` inside another capture taints that one too.
		captured := stringShape
		if w.taintedEmissions > before {
			captured = shape{kind: kindString, tainted: true, bind: stringShape.bind}
		}
		w.bind(toks[0].text, captured)
		return nil

	default:
		// A block this walk does not know would silently weaken strictness;
		// the v1 engine registers none beyond the cases above.
		return &UnsupportedTagError{Tag: n.Name, Reason: "not part of the v1 prompt template contract", File: loc.Pathname, Line: loc.LineNo}
	}
}

func (w *walker) tag(name, args string, loc parser.SourceLoc) error {
	switch name {
	case "break", "continue":
		return nil

	case "include":
		return &UnsupportedTagError{Tag: name, Reason: "a prompt is a single self-contained template (SPEC §5.6)", File: loc.Pathname, Line: loc.LineNo}

	case "assign":
		toks, err := lex(args)
		if err != nil {
			return fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
		}
		if len(toks) < 3 || toks[0].kind != tokIdent || toks[1].kind != tokPunct || toks[1].text != "=" {
			return fmt.Errorf("malformed {%% assign %%} %q at %s:%d", args, loc.Pathname, loc.LineNo)
		}
		if err := w.checkBindable(toks[0].text, loc); err != nil {
			return err
		}
		rhs := toks[2:]
		if err := w.checkTokens(rhs, loc); err != nil {
			return err
		}
		w.bind(toks[0].text, w.wholeRefShape(rhs))
		return nil

	default:
		// cycle and anything future: validate references generically.
		return w.expr(args, loc)
	}
}

// scopedBody walks a block body in a fresh scope.
func (w *walker) scopedBody(body []render.Node) error {
	w.push()
	err := w.body(body)
	w.pop()
	return err
}

func (w *walker) body(body []render.Node) error {
	for _, c := range body {
		if err := w.node(c); err != nil {
			return err
		}
	}
	return nil
}

// expr validates one expression source string in the current scope. Every
// position but an {{ … }} object node arrives here, and none of them may read
// an untrusted variable.
func (w *walker) expr(src string, loc parser.SourceLoc) error {
	toks, err := lex(src)
	if err != nil {
		return fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
	}
	return w.check(toks, loc, false)
}

// emission validates an {{ … }} object node, the one position that may read an
// untrusted variable — and then only when the expression is nothing but that
// variable, so the fence around it reaches the output whole.
func (w *walker) emission(src string, loc parser.SourceLoc) error {
	toks, err := lex(src)
	if err != nil {
		return fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
	}
	return w.check(toks, loc, isWholeRef(toks))
}

func (w *walker) checkTokens(toks []token, loc parser.SourceLoc) error {
	return w.check(toks, loc, false)
}

// check validates one expression's references and collects its filter names.
// bare says the expression is exactly one variable reference being emitted.
func (w *walker) check(toks []token, loc parser.SourceLoc, bare bool) error {
	a, err := analyze(toks)
	if err != nil {
		return fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
	}
	for _, r := range a.refs {
		if err := w.checkRef(r, loc, bare); err != nil {
			return err
		}
	}
	for _, f := range a.filters {
		if _, seen := w.filters[f]; !seen {
			w.filters[f] = loc
		}
	}
	return nil
}

func (w *walker) checkRef(r ref, loc parser.SourceLoc, bare bool) error {
	s, ok := w.lookup(r.root)
	if !ok {
		return &UnknownVariableError{
			Ref:    r.String(),
			Detail: fmt.Sprintf("%q is not defined; variables in scope: %s", r.root, strings.Join(w.inScope(), ", ")),
			File:   loc.Pathname,
			Line:   loc.LineNo,
		}
	}
	for i, seg := range r.segs {
		if s.tainted && !s.container() {
			// Reaching into a fenced value: .size would measure the fence, and
			// an index would cut it. Descending through a tainted *container*
			// is fine — that is how a prompt reaches the trusted leaves under
			// it — so only a scalar is refused here.
			return &UntrustedUseError{
				Ref:    r.String(),
				Detail: fmt.Sprintf("%q carries fenced content and can only be emitted whole, not accessed into", r.prefix(i-1)),
				File:   loc.Pathname,
				Line:   loc.LineNo,
			}
		}
		var next shape
		switch seg.kind {
		case segProp:
			next, ok = s.property(seg.prop)
			if !ok {
				detail := fmt.Sprintf("%q has no property %q", r.prefix(i-1), seg.prop)
				if props := s.propertyNames(); props != nil {
					slices.Sort(props)
					detail += fmt.Sprintf(" (properties: %s)", strings.Join(props, ", "))
				}
				return &UnknownVariableError{Ref: r.String(), Detail: detail, File: loc.Pathname, Line: loc.LineNo}
			}
		case segKey:
			next, ok = s.keyIndex(seg.prop)
			if !ok {
				detail := fmt.Sprintf("%q has no key %q", r.prefix(i-1), seg.prop)
				switch props := s.propertyNames(); {
				case s.kind == kindObject:
					slices.Sort(props)
					detail += fmt.Sprintf(" (keys: %s)", strings.Join(props, ", "))
				case props != nil:
					// The pseudo-properties are the usual reason for landing
					// here, and a dot is all that separates the two.
					slices.Sort(props)
					detail += fmt.Sprintf("; a bracket indexes rather than selects a property — reach %s with a dot instead", strings.Join(props, ", "))
				}
				return &UnknownVariableError{Ref: r.String(), Detail: detail, File: loc.Pathname, Line: loc.LineNo}
			}
		case segIndex:
			next, ok = s.index()
			if !ok {
				return &UnknownVariableError{
					Ref:    r.String(),
					Detail: fmt.Sprintf("%q is not indexable", r.prefix(i-1)),
					File:   loc.Pathname,
					Line:   loc.LineNo,
				}
			}
		case segDynamic:
			next, ok = s.index()
			if !ok {
				return &UnknownVariableError{
					Ref:    r.String(),
					Detail: fmt.Sprintf("dynamic access on %q cannot be validated against the closed set", r.prefix(i-1)),
					File:   loc.Pathname,
					Line:   loc.LineNo,
				}
			}
		}
		s = next
	}
	if s.tainted {
		if !bare {
			return &UntrustedUseError{
				Ref: r.String(),
				Detail: fmt.Sprintf("%q carries content the issue author wrote, which renders fenced; it may only be emitted on its own, as {{ %s }}",
					r.String(), r.String()),
				File: loc.Pathname,
				Line: loc.LineNo,
			}
		}
		// A legal whole emission of fenced content. {% capture %} turns the
		// body it wraps into an ordinary string, so it has to know.
		w.taintedEmissions++
	}
	return nil
}

// isWholeRef reports whether toks are exactly one variable reference — no
// filters, no operators, no literals. That is the only shape in which an
// untrusted variable reaches the output with its fence around it.
func isWholeRef(toks []token) bool {
	if len(toks) == 0 || toks[0].kind != tokIdent || reserved[toks[0].text] {
		return false
	}
	_, n, err := parseRef(toks)
	return err == nil && n == len(toks)
}

// loopArgs parses `{% for x in <collection> [reversed] [limit: n] [offset: n] %}`
// (and the `tablerow` form, which adds `cols: n`) by grammar position, not by
// token spelling: everything but the modifier names is a checkable expression.
// It returns the binder name and the statically-known element shape.
func (w *walker) loopArgs(tag, args string, loc parser.SourceLoc) (string, shape, error) {
	toks, err := lex(args)
	if err != nil {
		return "", shape{}, fmt.Errorf("%v at %s:%d", err, loc.Pathname, loc.LineNo)
	}
	spec, err := parseLoop(toks)
	if err != nil {
		return "", shape{}, fmt.Errorf("malformed {%% %s %%} %q: %v at %s:%d", tag, args, err, loc.Pathname, loc.LineNo)
	}
	if err := w.checkTokens(spec.collection, loc); err != nil {
		return "", shape{}, err
	}
	for _, m := range spec.modifiers {
		if err := w.checkTokens(m.value, loc); err != nil {
			return "", shape{}, err
		}
	}

	// The element shape survives modifiers — they trail the collection rather
	// than being part of it — when the collection is exactly one array
	// reference. Filters and compound expressions stay at the checking
	// frontier.
	elem := anyShape
	if collection := w.wholeRefShape(spec.collection); collection.kind == kindArray {
		elem = *collection.elem
	}
	return spec.binder, elem, nil
}

// wholeRefShape resolves the shape of a token stream that is exactly one
// already-validated variable reference; anything else — filters, literals,
// operators — is the checking frontier, anyShape.
func (w *walker) wholeRefShape(toks []token) shape {
	if len(toks) == 0 || toks[0].kind != tokIdent || reserved[toks[0].text] {
		return anyShape
	}
	r, n, err := parseRef(toks)
	if err != nil || n != len(toks) {
		return anyShape
	}
	s, ok := w.lookup(r.root)
	if !ok {
		return anyShape
	}
	for _, seg := range r.segs {
		switch seg.kind {
		case segProp:
			s, ok = s.property(seg.prop)
		case segKey:
			s, ok = s.keyIndex(seg.prop)
		default:
			s, ok = s.index()
		}
		if !ok {
			return anyShape
		}
	}
	return s
}
