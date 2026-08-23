package template

import (
	"reflect"
	"strings"
	"testing"
)

func TestLexTokenRules(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want []token
	}{
		{"issue.title", []token{{tokIdent, "issue"}, {tokProperty, "title"}}},
		{"a.b[c]", []token{{tokIdent, "a"}, {tokProperty, "b"}, {tokPunct, "["}, {tokIdent, "c"}, {tokPunct, "]"}}},
		// Hyphens are part of identifiers, mirroring the engine's scanner.
		{"x-y", []token{{tokIdent, "x-y"}}},
		{"done?", []token{{tokIdent, "done?"}}},
		// `1..5` is number, range operator, number — not a float.
		{"1..5", []token{{tokLiteral, "1"}, {tokPunct, ".."}, {tokLiteral, "5"}}},
		{"-1.5", []token{{tokLiteral, "-1.5"}}},
		{"replace: 'a', \"b\"", []token{{tokKeyword, "replace"}, {tokLiteral, "'a'"}, {tokPunct, ","}, {tokLiteral, `"b"`}}},
		{`a == "x | y"`, []token{{tokIdent, "a"}, {tokPunct, "=="}, {tokLiteral, `"x | y"`}}},
		{"a != b and c >= 2", []token{{tokIdent, "a"}, {tokPunct, "!="}, {tokIdent, "b"}, {tokIdent, "and"}, {tokIdent, "c"}, {tokPunct, ">="}, {tokLiteral, "2"}}},
	} {
		got, err := lex(tc.src)
		if err != nil {
			t.Errorf("lex(%q) error: %v", tc.src, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("lex(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestLexUnterminatedString(t *testing.T) {
	if _, err := lex("'oops"); err == nil {
		t.Error("lex('oops) = ok, want error")
	}
}

func TestAnalyzeExtractsRefsAndFilters(t *testing.T) {
	toks, err := lex(`issue.title | replace: run.id, 'x' | upcase`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyze(toks)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range a.refs {
		paths = append(paths, r.String())
	}
	if !reflect.DeepEqual(paths, []string{"issue.title", "run.id"}) {
		t.Errorf("refs = %v", paths)
	}
	if !reflect.DeepEqual(a.filters, []string{"replace", "upcase"}) {
		t.Errorf("filters = %v", a.filters)
	}
}

func TestAnalyzeReservedWordsAreNotRefs(t *testing.T) {
	toks, err := lex(`a and b or c contains 'x' in true false nil`)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analyze(toks)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range a.refs {
		paths = append(paths, r.String())
	}
	if !reflect.DeepEqual(paths, []string{"a", "b", "c"}) {
		t.Errorf("refs = %v, want only a, b, c", paths)
	}
}

// tokensText renders a token span readably: `.name` for properties, `name:`
// for keywords, source text for everything else.
func tokensText(toks []token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		switch t.kind {
		case tokProperty:
			parts[i] = "." + t.text
		case tokKeyword:
			parts[i] = t.text + ":"
		default:
			parts[i] = t.text
		}
	}
	return strings.Join(parts, " ")
}

func modifiersText(mods []loopModifier) string {
	parts := make([]string, len(mods))
	for i, m := range mods {
		parts[i] = m.name
		if m.value != nil {
			parts[i] += ": " + tokensText(m.value)
		}
	}
	return strings.Join(parts, "; ")
}

// A loop modifier is a modifier because of its grammar position, not its
// spelling: the same words are ordinary expression material in the collection
// (`reversed.items`), as a filter name (`| limit: 1`), or in a modifier value
// (`limit: reversed.size`).
func TestParseLoopSplitsByGrammarPosition(t *testing.T) {
	for _, tc := range []struct {
		args       string
		binder     string
		collection string
		modifiers  string
	}{
		{args: "x in a", binder: "x", collection: "a"},
		{args: "x in issue.labels", binder: "x", collection: "issue .labels"},
		{args: "x in a reversed", binder: "x", collection: "a", modifiers: "reversed"},
		{args: "x in a limit: 1", binder: "x", collection: "a", modifiers: "limit: 1"},
		{args: "x in a limit: 1 offset: 2 reversed", binder: "x", collection: "a", modifiers: "limit: 1; offset: 2; reversed"},
		{args: "x in a cols: 2", binder: "x", collection: "a", modifiers: "cols: 2"},
		// Modifier values are expressions, and their references still count.
		{args: "x in a limit: b.size", binder: "x", collection: "a", modifiers: "limit: b .size"},
		{args: "x in a offset: b[c] reversed", binder: "x", collection: "a", modifiers: "offset: b [ c ]; reversed"},
		// Modifier spellings outside modifier position stay in the collection.
		{args: "x in reversed", binder: "x", collection: "reversed"},
		{args: "x in reversed.items", binder: "x", collection: "reversed .items"},
		{args: "x in limit", binder: "x", collection: "limit"},
		{args: "x in a.reversed.cols", binder: "x", collection: "a .reversed .cols"},
		{args: "x in a | reversed", binder: "x", collection: "a | reversed"},
		{args: "x in a | limit: 1", binder: "x", collection: "a | limit: 1"},
		{args: "x in a | offset: 1 reversed", binder: "x", collection: "a | offset: 1", modifiers: "reversed"},
		// Filter chains, argument lists, groups, and indexing all belong to
		// the collection expression; the modifier run starts after them.
		{args: "x in a | replace: 'p', 'q' limit: 2", binder: "x", collection: "a | replace: 'p' , 'q'", modifiers: "limit: 2"},
		{args: "x in a | upcase | join: ', '", binder: "x", collection: "a | upcase | join: ', '"},
		{args: "x in (1..5) limit: 2", binder: "x", collection: "( 1 .. 5 )", modifiers: "limit: 2"},
		{args: "x in a[b] reversed", binder: "x", collection: "a [ b ]", modifiers: "reversed"},
		{args: "x in a[b[c]].d limit: 1", binder: "x", collection: "a [ b [ c ] ] .d", modifiers: "limit: 1"},
	} {
		toks, err := lex(tc.args)
		if err != nil {
			t.Errorf("lex(%q) error: %v", tc.args, err)
			continue
		}
		spec, err := parseLoop(toks)
		if err != nil {
			t.Errorf("parseLoop(%q) error: %v", tc.args, err)
			continue
		}
		if spec.binder != tc.binder {
			t.Errorf("parseLoop(%q) binder = %q, want %q", tc.args, spec.binder, tc.binder)
		}
		if got := tokensText(spec.collection); got != tc.collection {
			t.Errorf("parseLoop(%q) collection = %q, want %q", tc.args, got, tc.collection)
		}
		if got := modifiersText(spec.modifiers); got != tc.modifiers {
			t.Errorf("parseLoop(%q) modifiers = %q, want %q", tc.args, got, tc.modifiers)
		}
	}
}

// parseLoop refuses what it cannot classify rather than dropping tokens: an
// unclassified token is a hole in the strictness walk. The engine rejects all
// of these at parse time too, so they are unreachable through Load.
func TestParseLoopRefusesMalformedArgs(t *testing.T) {
	for _, args := range []string{
		"",
		"x",
		"x in",
		"x on a",
		"a b c",
		"x in a bogusmod",     // undefined loop modifier
		"x in a limit",        // keyword modifier written bare
		"x in a limit:",       // modifier without a value
		"x in a | ",           // filter chain without a filter name
		"x in a == b",         // the loop collection is not a condition
		"x in a limit: | 1",   // modifier values are unfiltered expressions
		"x in a[b reversed",   // unbalanced index bracket
		"x in (1..5 reversed", // unbalanced group
	} {
		toks, err := lex(args)
		if err != nil {
			continue // lexer refusal is also a refusal
		}
		if spec, err := parseLoop(toks); err == nil {
			t.Errorf("parseLoop(%q) = %+v, want error", args, spec)
		}
	}
}

func TestAnalyzeBracketAccess(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want []string
	}{
		// A bracket stays a bracket: it is an index, not a property selector,
		// so the path reads back as written.
		{`issue["title"]`, []string{`issue["title"]`}},
		{"list[0]", []string{"list[0]"}},
		// A computed index is a dynamic segment, and its own refs are
		// validated too.
		{"list[idx]", []string{"list[…]", "idx"}},
		{"list[outer[inner]]", []string{"list[…]", "outer[…]", "inner"}},
	} {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		a, err := analyze(toks)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, r := range a.refs {
			paths = append(paths, r.String())
		}
		if !reflect.DeepEqual(paths, tc.want) {
			t.Errorf("analyze(%q) refs = %v, want %v", tc.src, paths, tc.want)
		}
	}
}
