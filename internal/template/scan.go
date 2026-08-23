package template

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The engine compiles expressions into opaque closures, so the load-time walk
// lexes each node's expression source itself to extract variable references
// and filter names. The token rules mirror the engine's expression scanner
// (osteele/liquid expressions/scanner.rl): string literals without escapes,
// numbers, identifiers that may contain hyphens and a trailing '?',
// `.name` properties, `name:` keywords, and punctuation. Divergence from the
// engine's lexing would mis-validate, so the rules are asserted against the
// engine's behavior in tests.

type tokKind int

const (
	tokIdent    tokKind = iota // bare identifier
	tokProperty                // .name
	tokKeyword                 // name: (filter name with args, or loop/filter keyword argument)
	tokLiteral                 // string, number
	tokPunct                   // operators and delimiters, incl. == != <= >= .. | [ ] ( ) , =
)

type token struct {
	kind tokKind
	text string // identifier/property/keyword name (without '.' or ':'), literal source, or punct
}

// reserved are expression words that are operators or constants, never
// variable references.
var reserved = map[string]bool{
	"and": true, "or": true, "contains": true, "in": true,
	"true": true, "false": true, "nil": true,
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentTail(r rune) bool {
	return r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// lex tokenizes one expression source string (a node's Args text).
func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		r, w := utf8.DecodeRuneInString(src[i:])
		switch {
		case unicode.IsSpace(r):
			i += w

		case r == '\'' || r == '"':
			end := strings.IndexByte(src[i+1:], byte(r))
			if end < 0 {
				return nil, fmt.Errorf("unterminated string literal in %q", src)
			}
			toks = append(toks, token{tokLiteral, src[i : i+end+2]})
			i += end + 2

		case r >= '0' && r <= '9' || (r == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i + 1
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			// Fraction only when '.' is followed by a digit; `1..5` is
			// number, range operator, number.
			if j+1 < len(src) && src[j] == '.' && src[j+1] >= '0' && src[j+1] <= '9' {
				j += 2
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			toks = append(toks, token{tokLiteral, src[i:j]})
			i = j

		case isIdentStart(r):
			name, j := lexIdent(src, i)
			if j < len(src) && src[j] == ':' {
				toks = append(toks, token{tokKeyword, name})
				j++
			} else {
				toks = append(toks, token{tokIdent, name})
			}
			i = j

		case r == '.':
			if nr, _ := utf8.DecodeRuneInString(src[i+1:]); isIdentStart(nr) {
				name, j := lexIdent(src, i+1)
				toks = append(toks, token{tokProperty, name})
				i = j
			} else if strings.HasPrefix(src[i:], "..") {
				toks = append(toks, token{tokPunct, ".."})
				i += 2
			} else {
				toks = append(toks, token{tokPunct, "."})
				i++
			}

		default:
			if op := src[i:min(i+2, len(src))]; op == "==" || op == "!=" || op == "<=" || op == ">=" {
				toks = append(toks, token{tokPunct, op})
				i += 2
			} else {
				toks = append(toks, token{tokPunct, string(r)})
				i += w
			}
		}
	}
	return toks, nil
}

// lexIdent consumes an identifier starting at src[start] and returns it with
// the index past its end. A single trailing '?' is part of the identifier.
func lexIdent(src string, start int) (string, int) {
	j := start
	for j < len(src) {
		r, w := utf8.DecodeRuneInString(src[j:])
		if !isIdentTail(r) {
			break
		}
		j += w
	}
	if j < len(src) && src[j] == '?' {
		j++
	}
	return src[start:j], j
}

// A ref is one variable reference: a root identifier plus its access chain.
type segKind int

const (
	segProp    segKind = iota // .name
	segKey                    // ["name"]
	segIndex                  // [int literal]
	segDynamic                // [computed expression]
)

type pathSeg struct {
	kind  segKind
	prop  string // segProp, segKey
	index int    // segIndex
}

type ref struct {
	root string
	segs []pathSeg
}

// String reconstructs the path as written, for error messages.
func (r ref) String() string {
	var b strings.Builder
	b.WriteString(r.root)
	for _, s := range r.segs {
		switch s.kind {
		case segProp:
			b.WriteByte('.')
			b.WriteString(s.prop)
		case segKey:
			fmt.Fprintf(&b, "[%q]", s.prop)
		case segIndex:
			fmt.Fprintf(&b, "[%d]", s.index)
		case segDynamic:
			b.WriteString("[…]")
		}
	}
	return b.String()
}

// prefix reconstructs the path up to and including seg i.
func (r ref) prefix(i int) string {
	return ref{root: r.root, segs: r.segs[:i+1]}.String()
}

// analysis is what one expression contributes to the strictness walk.
type analysis struct {
	refs    []ref
	filters []string
}

// analyze extracts variable references and filter names from an expression
// token stream. Bracket contents are analyzed recursively: a lone literal is
// a static index, anything else is a dynamic access whose own references are
// still validated.
func analyze(toks []token) (analysis, error) {
	var a analysis
	i := 0
	for i < len(toks) {
		t := toks[i]
		switch {
		case t.kind == tokPunct && t.text == "|":
			// The next token names the filter: tokKeyword when it has
			// arguments (`| replace: 'a', 'b'`), tokIdent when bare.
			i++
			if i < len(toks) && (toks[i].kind == tokIdent || toks[i].kind == tokKeyword) {
				a.filters = append(a.filters, toks[i].text)
				i++
			}

		case t.kind == tokIdent && !reserved[t.text]:
			r, n, err := parseRef(toks[i:])
			if err != nil {
				return analysis{}, err
			}
			sub, err := analyzeBrackets(toks[i : i+n])
			if err != nil {
				return analysis{}, err
			}
			a.refs = append(a.refs, r)
			a.refs = append(a.refs, sub.refs...)
			a.filters = append(a.filters, sub.filters...)
			i += n

		default:
			i++
		}
	}
	return a, nil
}

// parseRef parses a variable reference at the head of toks, returning it and
// the number of tokens consumed.
func parseRef(toks []token) (ref, int, error) {
	r := ref{root: toks[0].text}
	i := 1
	for i < len(toks) {
		switch {
		case toks[i].kind == tokProperty:
			r.segs = append(r.segs, pathSeg{kind: segProp, prop: toks[i].text})
			i++
		case toks[i].kind == tokPunct && toks[i].text == "[":
			depth := 1
			j := i + 1
			for j < len(toks) && depth > 0 {
				if toks[j].kind == tokPunct {
					switch toks[j].text {
					case "[":
						depth++
					case "]":
						depth--
					}
				}
				j++
			}
			if depth != 0 {
				return ref{}, 0, fmt.Errorf("unbalanced brackets in template expression")
			}
			r.segs = append(r.segs, bracketSeg(toks[i+1:j-1]))
			i = j
		default:
			return r, i, nil
		}
	}
	return r, i, nil
}

// bracketSeg classifies one bracket's contents as a static or dynamic access.
// A bracket is never a property access, however it is spelled: the engine
// compiles `x[…]` to IndexValue and `x.name` to PropertyValue
// (osteele/liquid expressions/builders.go), and those resolve differently —
// only an object answers a string key, and the pseudo-properties `size`,
// `first` and `last` live on PropertyValue alone.
func bracketSeg(inner []token) pathSeg {
	if len(inner) == 1 && inner[0].kind == tokLiteral {
		lit := inner[0].text
		if n, err := strconv.Atoi(lit); err == nil {
			return pathSeg{kind: segIndex, index: n}
		}
		if len(lit) >= 2 && (lit[0] == '\'' || lit[0] == '"') {
			return pathSeg{kind: segKey, prop: lit[1 : len(lit)-1]}
		}
	}
	return pathSeg{kind: segDynamic}
}

// The loop tag grammar, mirroring the engine's `loop` production
// (osteele/liquid expressions/expressions.y):
//
//	loop:           IDENTIFIER "in" filtered loop_modifiers
//	filtered:       expr ('|' (IDENTIFIER | KEYWORD filter_params))*
//	filter_params:  expr (',' expr)*
//	loop_modifiers: (IDENTIFIER | KEYWORD expr)*
//	expr:           (LITERAL | IDENTIFIER | '(' … ')') (PROPERTY | '[' … ']')*
//
// A modifier is a modifier because of where it sits, not how it is spelled:
// only the trailing run after the collection expression is modifier position.
// The same words elsewhere are ordinary expression material — a collection
// root (`reversed.items`), a filter name (`| limit: 1`), or a nested
// reference (`limit: reversed.size`) — and parseLoop leaves them in the
// checkable spans so the strictness walk refuses them (SPEC §5.6).

type loopModifier struct {
	name  string
	value []token // KEYWORD modifiers; nil for a bare modifier
}

type loopSpec struct {
	binder     string
	collection []token
	modifiers  []loopModifier
}

// The engine's closed modifier sets: expressions.y rejects any other spelling
// in modifier position at parse time, so a template carrying one never reaches
// this walk. Listed here so modifier position is entered deliberately rather
// than by falling through — anything else is malformed, not silently dropped.
var (
	bareLoopModifiers    = map[string]bool{"reversed": true}
	keywordLoopModifiers = map[string]bool{"limit": true, "offset": true, "cols": true}
)

// parseLoop splits a loop tag's args into its binder, collection expression,
// and modifiers. Both `for` and `tablerow` compile through the same engine
// production, so both parse here.
func parseLoop(toks []token) (loopSpec, error) {
	if len(toks) < 3 || toks[0].kind != tokIdent || toks[1].kind != tokIdent || toks[1].text != "in" {
		return loopSpec{}, errors.New(`expected "<name> in <collection>"`)
	}
	spec := loopSpec{binder: toks[0].text}
	rest := toks[2:]

	n, err := filteredLen(rest)
	if err != nil {
		return loopSpec{}, err
	}
	spec.collection, rest = rest[:n], rest[n:]

	for len(rest) > 0 {
		switch {
		case rest[0].kind == tokIdent && bareLoopModifiers[rest[0].text]:
			spec.modifiers = append(spec.modifiers, loopModifier{name: rest[0].text})
			rest = rest[1:]
		case rest[0].kind == tokKeyword && keywordLoopModifiers[rest[0].text]:
			n, err := exprLen(rest[1:])
			if err != nil {
				return loopSpec{}, fmt.Errorf("modifier %q: %v", rest[0].text, err)
			}
			spec.modifiers = append(spec.modifiers, loopModifier{name: rest[0].text, value: rest[1 : 1+n]})
			rest = rest[1+n:]
		default:
			return loopSpec{}, fmt.Errorf("unexpected %q after the collection expression", rest[0].text)
		}
	}
	return spec, nil
}

// filteredLen returns the token length of the `filtered` expression at the
// head of toks: one expr plus its filter chain.
func filteredLen(toks []token) (int, error) {
	i, err := exprLen(toks)
	if err != nil {
		return 0, err
	}
	for i < len(toks) && toks[i].kind == tokPunct && toks[i].text == "|" {
		i++
		if i == len(toks) || (toks[i].kind != tokIdent && toks[i].kind != tokKeyword) {
			return 0, errors.New(`expected a filter name after "|"`)
		}
		named := toks[i]
		i++
		if named.kind != tokKeyword {
			continue // bare filter: no arguments to consume
		}
		for {
			n, err := exprLen(toks[i:])
			if err != nil {
				return 0, fmt.Errorf("filter %q: %v", named.text, err)
			}
			i += n
			if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == "," {
				i++
				continue
			}
			break
		}
	}
	return i, nil
}

// exprLen returns the token length of the `expr` at the head of toks: one
// primary — literal, identifier, or parenthesized group — plus its postfix
// chain of properties and index brackets.
func exprLen(toks []token) (int, error) {
	if len(toks) == 0 {
		return 0, errors.New("expected an expression")
	}
	var i int
	switch {
	case toks[0].kind == tokLiteral || toks[0].kind == tokIdent:
		i = 1
	case toks[0].kind == tokPunct && toks[0].text == "(":
		n, err := groupLen(toks, "(", ")")
		if err != nil {
			return 0, err
		}
		i = n
	default:
		return 0, fmt.Errorf("expected an expression, got %q", toks[0].text)
	}
	for i < len(toks) {
		switch {
		case toks[i].kind == tokProperty:
			i++
		case toks[i].kind == tokPunct && toks[i].text == "[":
			n, err := groupLen(toks[i:], "[", "]")
			if err != nil {
				return 0, err
			}
			i += n
		default:
			return i, nil
		}
	}
	return i, nil
}

// groupLen returns the token length of the balanced open…closing group
// starting at toks[0]. Groups of the other bracket kind nest freely inside it.
func groupLen(toks []token, open, closing string) (int, error) {
	depth := 0
	for i, t := range toks {
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case open:
			depth++
		case closing:
			if depth--; depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced %q in template expression", open)
}

// analyzeBrackets validates references nested inside a ref's dynamic bracket
// expressions (e.g. the `x` of `list[x]`). Static single-literal brackets
// contribute nothing.
func analyzeBrackets(toks []token) (analysis, error) {
	var a analysis
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tokPunct || toks[i].text != "[" {
			continue
		}
		depth := 1
		j := i + 1
		for j < len(toks) && depth > 0 {
			if toks[j].kind == tokPunct {
				switch toks[j].text {
				case "[":
					depth++
				case "]":
					depth--
				}
			}
			j++
		}
		inner := toks[i+1 : j-1]
		if len(inner) != 1 || inner[0].kind != tokLiteral {
			sub, err := analyze(inner)
			if err != nil {
				return analysis{}, err
			}
			a.refs = append(a.refs, sub.refs...)
			a.filters = append(a.filters, sub.filters...)
		}
		i = j - 1
	}
	return a, nil
}
