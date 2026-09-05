package template

import (
	"errors"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func testIssue() core.Issue {
	return core.Issue{
		Identifier: "123",
		Title:      "Fix the flux capacitor",
		Body:       "It fluxes when it should capacitate.",
		Labels:     []string{"ben", "bug"},
		State:      "open",
		Assignees:  []string{"ben-bot"},
		Blockers: []core.Blocker{
			{Identifier: "122", State: "closed", Open: false},
		},
		URL:       "https://github.com/o/r/issues/123",
		CreatedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC),
	}
}

func load(t *testing.T, src string) (*Prompt, error) {
	t.Helper()
	return Load(src, "WORKFLOW.md", 1)
}

func mustLoad(t *testing.T, src string) *Prompt {
	t.Helper()
	p, err := load(t, src)
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", src, err)
	}
	return p
}

// mustParseUnchecked bypasses Load's strictness walk so tests can drive engine
// failures that valid prompts cannot reach.
func mustParseUnchecked(t *testing.T, src string) *Prompt {
	t.Helper()
	render, _ := engines()
	tpl, err := render.ParseString(src)
	if err != nil {
		t.Fatalf("ParseString(%q) failed: %v", src, err)
	}
	return &Prompt{tpl: tpl}
}

// --- load-time strictness: the closed set is enforced at load, not render ---

func TestLoadAcceptsClosedSetUsage(t *testing.T) {
	for _, src := range []string{
		"plain text, no liquid at all",
		"{{ issue.identifier }} {{ issue.title }} {{ issue.body }} {{ issue.url }}",
		"{{ issue.state }} {{ issue.created_at }} {{ issue.updated_at }}",
		"{{ attempt }} {{ workspace }} {{ target_branch }} {{ run.id }} {{ run.previous_outcome }}",
		"{{ issue.labels }} {{ issue.labels.size }} {{ issue.labels.first }} {{ issue.labels.last }}",
		"{{ issue.labels[0] }} {{ issue.assignees[1] }}",
		"{{ issue.state.size }} {{ workspace.size }}",
		`{{ issue["title"] }}`,
		"{% if attempt %}retry{% endif %}",
		`{% if run.previous_outcome == "succeeded" %}continue{% elsif attempt %}retry{% else %}first{% endif %}`,
		"{% unless attempt %}first{% endunless %}",
		"{% case issue.state %}{% when 'open' %}O{% when issue.url %}T{% else %}X{% endcase %}",
		"{% for l in issue.labels %}{{ l }} {{ l.size }} {{ forloop.index }}{% endfor %}",
		"{% for l in issue.labels reversed limit: 1 offset: 0 %}{{ l }}{% endfor %}",
		"{% for l in issue.labels limit: issue.labels.size offset: issue.blockers.size %}{{ l }}{% endfor %}",
		"{% tablerow l in issue.labels %}{{ l }} {{ l.size }} {{ forloop.index }}{% endtablerow %}",
		"{% tablerow l in issue.labels cols: 2 limit: 1 offset: 0 reversed %}{{ l }}{% endtablerow %}",
		"{% for b in issue.blockers %}{{ b.identifier }} {{ b.state }} {{ b.open }}{% endfor %}",
		"{% for b in issue.blockers limit: 1 offset: 0 %}{{ b.identifier }}{% endfor %}",
		"{% for x in (1..5) %}{{ x }}{% endfor %}",
		"{% for l in issue.labels %}{% else %}none{% endfor %}",
		"{% assign x = issue.state %}{{ x }} {{ x.size }}",
		"{% assign x = issue.state | upcase %}{{ x }}",
		"{% capture c %}{{ issue.state }}{% endcapture %}{{ c }} {{ c.size }}",
		"{% for l in issue.labels %}{% cycle 'x', 'y' %}{% endfor %}",
		"{% for l in issue.labels %}{% if l == 'ben' %}{% continue %}{% endif %}{% break %}{% endfor %}",
		"{% comment %}anything {{ at.all }} goes{% endcomment %}",
		"{% raw %}{{ not.liquid }}{% endraw %}",
		"{{ issue.state | replace: 'a', 'b' | truncate: 5 }}",
		`{{ issue.state | append: " | " }}`, // pipe inside a string is not a filter
		"{{ issue.labels | join: ', ' }}",
		"{{ issue.created_at | date: '%Y-%m-%d' }}",
		"{{ run.previous_outcome | default: 'none' }}",
	} {
		if _, err := load(t, src); err != nil {
			t.Errorf("Load(%q) = %v, want ok", src, err)
		}
	}
}

func TestLoadRejectsUnknownVariables(t *testing.T) {
	for _, tc := range []struct {
		src     string
		wantRef string
	}{
		{"{{ issue.titel }}", "issue.titel"}, // the acceptance case
		{"{{ isue.title }}", "isue.title"},
		{"{{ bogus }}", "bogus"},
		{"{{ issue.dispatchable }}", "issue.dispatchable"}, // adapter-internal, not in §5.6
		{"{{ run.bogus }}", "run.bogus"},
		{"{{ attempt.anything }}", "attempt.anything"},
		{"{{ workspace.path }}", "workspace.path"},
		{"{{ issue.labels.push }}", "issue.labels.push"},
		{"{{ issue.state[0] }}", "issue.state[0]"},
		{`{{ issue["titel"] }}`, `issue["titel"]`},
		// A bracket compiles to IndexValue, not PropertyValue, so the
		// pseudo-properties are unreachable through one — the engine answers
		// each of these with nil (#64).
		{`{{ issue.labels["size"] }}`, `issue.labels["size"]`},
		{`{{ issue.labels["first"] }}`, `issue.labels["first"]`},
		{`{{ issue.labels["last"] }}`, `issue.labels["last"]`},
		{`{{ issue.state["size"] }}`, `issue.state["size"]`},
		{`{{ workspace["size"] }}`, `workspace["size"]`},
		{`{{ issue["size"] }}`, `issue["size"]`},
		{"{% if issue.titel %}x{% endif %}", "issue.titel"},             // condition position: the render backstop's blind spot
		{"{% if attempt %}{{ issue.titel }}{% endif %}", "issue.titel"}, // branch not taken on first render
		{"{% case issue.state %}{% when issue.bogus %}x{% endcase %}", "issue.bogus"},
		{"{% for l in issue.bogus %}{% endfor %}", "issue.bogus"},
		{"{% for b in issue.blockers %}{{ b.bogus }}{% endfor %}", "b.bogus"},
		{"{% for b in issue.blockers limit: 1 %}{{ b.bogus }}{% endfor %}", "b.bogus"},
		{"{% for l in issue.labels %}{{ forloop.bogus }}{% endfor %}", "forloop.bogus"},
		{"{{ x }}{% assign x = issue.title %}", "x"},                  // use before assign
		{"{% if attempt %}{% assign y = 1 %}{% endif %}{{ y }}", "y"}, // maybe-undefined after the branch
		{"{% for l in issue.labels %}{% endfor %}{{ l }}", "l"},       // loop binder out of scope
		{"{{ 'x' | append: issue.titel }}", "issue.titel"},            // refs in filter args are validated
		{"{{ issue[workspace] }}", "issue[…]"},                        // dynamic access on a closed object
	} {
		_, err := load(t, tc.src)
		if !errors.Is(err, ErrUnknownVariable) {
			t.Errorf("Load(%q) = %v, want ErrUnknownVariable", tc.src, err)
		}
		var uv *UnknownVariableError
		if !errors.As(err, &uv) {
			t.Errorf("Load(%q) = %v, want UnknownVariableError", tc.src, err)
			continue
		}
		if uv.Ref != tc.wantRef {
			t.Errorf("Load(%q) flagged ref %q, want %q", tc.src, uv.Ref, tc.wantRef)
		}
	}
}

func TestLoadRejectsUnknownFilters(t *testing.T) {
	for _, tc := range []struct {
		src      string
		wantName string
	}{
		{"{{ issue.state | bogusfilter }}", "bogusfilter"},
		{"{{ issue.state | upcase | bogusfilter }}", "bogusfilter"},
		{"{{ issue.state | bogusfilter: 'arg' }}", "bogusfilter"},
		{"{% if attempt %}{{ issue.state | bogusfilter }}{% endif %}", "bogusfilter"}, // untaken branch
		{"{% assign x = issue.state | bogusfilter %}{{ x }}", "bogusfilter"},
	} {
		_, err := load(t, tc.src)
		if !errors.Is(err, ErrUnknownFilter) {
			t.Errorf("Load(%q) = %v, want ErrUnknownFilter", tc.src, err)
		}
		var uf *UnknownFilterError
		if !errors.As(err, &uf) {
			t.Errorf("Load(%q) = %v, want UnknownFilterError", tc.src, err)
			continue
		}
		if uf.Name != tc.wantName {
			t.Errorf("Load(%q) flagged filter %q, want %q", tc.src, uf.Name, tc.wantName)
		}
	}
}

// --- loop modifiers: recognized by grammar position, not by spelling ---

// Regression (#18): the walk once removed `reversed` and `limit:`/`offset:`/
// `cols:` tokens from loop args wherever they appeared, so the same spellings
// used as a collection root, a filter name, or inside a modifier value slipped
// past the closed-set check.
func TestLoadLoopModifierSpellingsOutsideModifierPosition(t *testing.T) {
	for _, tc := range []struct {
		src string
		// exactly one of these is set
		wantRef    string // UnknownVariableError.Ref
		wantFilter string // UnknownFilterError.Name
	}{
		// Collection roots.
		{src: "{% for x in reversed %}{{ x }}{% endfor %}", wantRef: "reversed"},
		{src: "{% for x in reversed.items %}{{ x }}{% endfor %}", wantRef: "reversed.items"},
		{src: "{% for x in limit %}{{ x }}{% endfor %}", wantRef: "limit"},
		{src: "{% tablerow x in reversed.items %}{{ x }}{% endtablerow %}", wantRef: "reversed.items"},
		{src: "{% tablerow x in cols %}{{ x }}{% endtablerow %}", wantRef: "cols"},
		// Nested references: modifier values and dynamic indices.
		{src: "{% for x in issue.labels limit: reversed.size %}{{ x }}{% endfor %}", wantRef: "reversed.size"},
		{src: "{% for x in issue.labels offset: bogus %}{{ x }}{% endfor %}", wantRef: "bogus"},
		{src: "{% for x in issue.labels[reversed] %}{{ x }}{% endfor %}", wantRef: "reversed"},
		{src: "{% tablerow x in issue.labels cols: reversed %}{{ x }}{% endtablerow %}", wantRef: "reversed"},
		// Filter names.
		{src: "{% for x in issue.labels | reversed %}{{ x }}{% endfor %}", wantFilter: "reversed"},
		{src: "{% for x in issue.labels | limit: 1 %}{{ x }}{% endfor %}", wantFilter: "limit"},
		{src: "{% for x in issue.labels | offset: 1 reversed %}{{ x }}{% endfor %}", wantFilter: "offset"},
		{src: "{% tablerow x in issue.labels | cols: 2 %}{{ x }}{% endtablerow %}", wantFilter: "cols"},
	} {
		_, err := load(t, tc.src)
		if tc.wantRef != "" {
			var uv *UnknownVariableError
			if !errors.As(err, &uv) {
				t.Errorf("Load(%q) = %v, want UnknownVariableError", tc.src, err)
			} else if uv.Ref != tc.wantRef {
				t.Errorf("Load(%q) flagged ref %q, want %q", tc.src, uv.Ref, tc.wantRef)
			}
			continue
		}
		var uf *UnknownFilterError
		if !errors.As(err, &uf) {
			t.Errorf("Load(%q) = %v, want UnknownFilterError", tc.src, err)
		} else if uf.Name != tc.wantFilter {
			t.Errorf("Load(%q) flagged filter %q, want %q", tc.src, uf.Name, tc.wantFilter)
		}
	}
}

// The element shape inferred from a direct collection reference survives the
// modifier run, for both loop tags: modifiers trail the collection expression
// rather than being part of it.
func TestLoadLoopElementShapeSurvivesModifiers(t *testing.T) {
	for _, tc := range []struct {
		src     string
		wantRef string // "" = must load
	}{
		{src: "{% for b in issue.blockers reversed limit: 1 offset: 0 %}{{ b.identifier }}{% endfor %}"},
		{src: "{% tablerow b in issue.blockers cols: 2 reversed %}{{ b.state }}{% endtablerow %}"},
		{src: "{% for b in issue.blockers reversed %}{{ b.bogus }}{% endfor %}", wantRef: "b.bogus"},
		{src: "{% for b in issue.blockers limit: 1 offset: 0 %}{{ b.bogus }}{% endfor %}", wantRef: "b.bogus"},
		{src: "{% tablerow b in issue.blockers cols: 2 %}{{ b.bogus }}{% endtablerow %}", wantRef: "b.bogus"},
		// A filtered collection is the checking frontier, so property access
		// on the element is admitted at load and left to the backstop.
		{src: "{% for b in issue.blockers | sort limit: 1 %}{{ b.bogus }}{% endfor %}"},
	} {
		_, err := load(t, tc.src)
		if tc.wantRef == "" {
			if err != nil {
				t.Errorf("Load(%q) = %v, want ok", tc.src, err)
			}
			continue
		}
		var uv *UnknownVariableError
		if !errors.As(err, &uv) {
			t.Errorf("Load(%q) = %v, want UnknownVariableError", tc.src, err)
		} else if uv.Ref != tc.wantRef {
			t.Errorf("Load(%q) flagged ref %q, want %q", tc.src, uv.Ref, tc.wantRef)
		}
	}
}

// The modifiers the walk accepts are the ones the engine applies.
func TestRenderAppliesLoopModifiers(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{"{% for l in issue.labels %}{{ l }},{% endfor %}", "ben,bug,"},
		{"{% for l in issue.labels reversed %}{{ l }},{% endfor %}", "bug,ben,"},
		{"{% for l in issue.labels limit: 1 %}{{ l }},{% endfor %}", "ben,"},
		{"{% for l in issue.labels offset: 1 %}{{ l }},{% endfor %}", "bug,"},
		// The engine applies reversed, then offset, then limit — written order
		// does not matter.
		{"{% for l in issue.labels limit: 1 offset: 1 reversed %}{{ l }},{% endfor %}", "ben,"},
		{"{% for l in issue.labels limit: issue.blockers.size %}{{ l }},{% endfor %}", "ben,"},
	} {
		p := mustLoad(t, tc.src)
		out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
		if err != nil {
			t.Errorf("Render(%q): %v", tc.src, err)
			continue
		}
		if out != tc.want {
			t.Errorf("Render(%q) = %q, want %q", tc.src, out, tc.want)
		}
	}
}

// --- forloop: one binding name for both loop tags ---

// The two loop tags and how each wraps a two-iteration body, so a test can
// state the per-iteration values once and check both.
var loopTags = []struct {
	name string
	src  func(body string) string         // wrap a body in the tag over issue.labels (2 elements)
	want func(iter0, iter1 string) string // the engine's output for that body
}{
	{
		name: "for",
		src:  func(b string) string { return "{% for l in issue.labels %}" + b + "{% endfor %}" },
		want: func(a, b string) string { return a + b },
	},
	{
		// tablerow's tag name buys the <tr>/<td> decorator, nothing else.
		name: "tablerow",
		src:  func(b string) string { return "{% tablerow l in issue.labels %}" + b + "{% endtablerow %}" },
		want: func(a, b string) string {
			return `<tr class="row1"><td class="col1">` + a + `</td><td class="col2">` + b + `</td></tr>`
		},
	},
}

// Regression (#22): the walk once bound the loop metadata as `n.Name+"loop"`,
// so a {% tablerow %} body got `tablerowloop` — a name the engine never binds.
// That refused working templates at load and let a broken one through to the
// render backstop, escaping the load-time guarantee (SPEC §5.6).
//
// Every field of the closed set, under both tags, must load *and* render to
// the value the engine actually produces.
func TestForloopBindsUnderBothLoopTags(t *testing.T) {
	// The engine's numbers for a two-element collection, not the schema's guess.
	fields := []struct {
		name         string
		iter0, iter1 string
	}{
		{"index", "1", "2"},
		{"index0", "0", "1"},
		{"rindex", "2", "1"},
		{"rindex0", "1", "0"},
		{"first", "true", "false"},
		{"last", "false", "true"},
		{"length", "2", "2"},
	}

	// The table covers the schema exactly — no field drifts in unasserted.
	named := make([]string, 0, len(fields))
	for _, f := range fields {
		named = append(named, f.name)
	}
	slices.Sort(named)
	if got := sortedKeys(forloopShape.fields); !slices.Equal(got, named) {
		t.Fatalf("forloopShape fields = %v, test table covers %v", got, named)
	}

	for _, tag := range loopTags {
		for _, f := range fields {
			src := tag.src("{{ forloop." + f.name + " }}")
			p, err := load(t, src)
			if err != nil {
				t.Errorf("Load(%q) = %v, want ok", src, err)
				continue
			}
			out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
			if err != nil {
				t.Errorf("Render(%q): %v", src, err)
				continue
			}
			if want := tag.want(f.iter0, f.iter1); out != want {
				t.Errorf("Render(%q) = %q, want %q", src, out, want)
			}
		}
	}
}

// The other direction of the same agreement: the engine binds no field the
// schema is missing. Emitting the whole object renders Go's map formatting —
// `map[k:v k:v]`, key-sorted — which is enough to read the key set back out.
func TestForloopSchemaCoversEveryEngineField(t *testing.T) {
	for _, tag := range loopTags {
		src := tag.src("{{ forloop }}")
		p, err := load(t, src)
		if err != nil {
			t.Errorf("Load(%q) = %v, want ok", src, err)
			continue
		}
		out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
		if err != nil {
			t.Errorf("Render(%q): %v", src, err)
			continue
		}
		got, err := objectKeysFromDump(out)
		if err != nil {
			t.Errorf("Render(%q) = %q: %v", src, out, err)
			continue
		}
		if want := sortedKeys(forloopShape.fields); !slices.Equal(got, want) {
			t.Errorf("%s binds forloop fields %v, schema has %v", tag.name, got, want)
		}
	}
}

// untilMatchingBracket returns s up to the ']' closing an already-open '['.
func untilMatchingBracket(s string) (string, bool) {
	depth := 1
	for i, c := range s {
		switch c {
		case '[':
			depth++
		case ']':
			if depth--; depth == 0 {
				return s[:i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits map entries on spaces outside any nested brackets.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '[':
			depth++
		case ']':
			depth--
		case ' ':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func sortedKeys(m map[string]shape) []string {
	return slices.Sorted(maps.Keys(m))
}

// forloop is body-scoped and the only spelling: a tag-named variant is not in
// the closed set, and neither name survives the loop.
func TestLoadRejectsForloopOutOfScopeAndTagNamedVariants(t *testing.T) {
	for _, tc := range []struct {
		src     string
		wantRef string
	}{
		// The tag-named variant the walk used to bind. Refused at load now,
		// rather than loading and failing on the render backstop.
		{"{% tablerow l in issue.labels %}{{ tablerowloop.index }}{% endtablerow %}", "tablerowloop.index"},
		{"{% tablerow l in issue.labels %}{{ tablerowloop }}{% endtablerow %}", "tablerowloop"},
		{"{% for l in issue.labels %}{{ forlooploop.index }}{% endfor %}", "forlooploop.index"},
		// Unknown fields, both tags.
		{"{% tablerow l in issue.labels %}{{ forloop.bogus }}{% endtablerow %}", "forloop.bogus"},
		{"{% tablerow l in issue.labels cols: 2 %}{{ forloop.col }}{% endtablerow %}", "forloop.col"},
		// Body-scoped: gone after the loop closes, for either tag.
		{"{% tablerow l in issue.labels %}{% endtablerow %}{{ forloop.index }}", "forloop.index"},
		{"{% for l in issue.labels %}{% endfor %}{{ forloop.index }}", "forloop.index"},
		// The {% else %} clause runs on an empty collection, outside the body.
		{"{% for l in issue.labels %}{% else %}{{ forloop.index }}{% endfor %}", "forloop.index"},
	} {
		_, err := load(t, tc.src)
		var uv *UnknownVariableError
		if !errors.As(err, &uv) {
			t.Errorf("Load(%q) = %v, want UnknownVariableError", tc.src, err)
			continue
		}
		if uv.Ref != tc.wantRef {
			t.Errorf("Load(%q) flagged ref %q, want %q", tc.src, uv.Ref, tc.wantRef)
		}
	}
}

// forloop is the engine's to bind. A prompt that binds it either loses the
// metadata (the engine sets the loop variable after forloop, so the binder
// wins) or, with {% cycle %} in the body, panics the engine outright. Both are
// refused at load instead.
func TestLoadRejectsBindingForloop(t *testing.T) {
	for _, src := range []string{
		// Loop binder, both tags. Loads-then-fails-at-render without the guard:
		// {{ forloop.index }} checks against the metadata shape, but renders
		// against the collection element.
		"{% for forloop in issue.labels %}{{ forloop.index }}{% endfor %}",
		"{% tablerow forloop in issue.labels %}{{ forloop.index }}{% endtablerow %}",
		"{% for forloop in issue.labels %}{{ forloop }}{% endfor %}",
		"{% tablerow forloop in issue.labels cols: 2 %}{{ forloop }}{% endtablerow %}",
		// The panic reproducer, both tags (see TestRenderContainsEnginePanicOnReboundForloop).
		"{% for forloop in issue.labels %}{% cycle 'x', 'y' %}{% endfor %}",
		"{% tablerow forloop in issue.labels %}{% cycle 'x', 'y' %}{% endtablerow %}",
		// The other two ways a template can bind a name — same hazard, since
		// {% cycle %} reads whatever forloop currently holds.
		"{% assign forloop = issue.title %}{{ forloop }}",
		"{% capture forloop %}x{% endcapture %}{{ forloop }}",
		"{% for l in issue.labels %}{% assign forloop = 'x' %}{% cycle 'a', 'b' %}{% endfor %}",
	} {
		_, err := load(t, src)
		if !errors.Is(err, ErrReservedName) {
			t.Errorf("Load(%q) = %v, want ErrReservedName", src, err)
		}
		var rn *ReservedNameError
		if !errors.As(err, &rn) {
			t.Errorf("Load(%q) = %v, want ReservedNameError", src, err)
			continue
		}
		if rn.Name != "forloop" {
			t.Errorf("Load(%q) flagged name %q, want \"forloop\"", src, rn.Name)
		}
	}
}

// Reading forloop is still fine — only binding it is reserved.
func TestLoadAcceptsReadingForloopAlongsideCycle(t *testing.T) {
	for _, src := range []string{
		"{% for l in issue.labels %}{{ forloop.index }}{% cycle 'x', 'y' %}{% endfor %}",
		"{% tablerow l in issue.labels %}{{ forloop.index }}{% cycle 'x', 'y' %}{% endtablerow %}",
	} {
		if _, err := load(t, src); err != nil {
			t.Errorf("Load(%q) = %v, want ok", src, err)
		}
	}
}

// The reservation is not speculative: with forloop rebound, {% cycle %}
// type-asserts it back to the loop record the engine wrote and panics
// (tags/iteration_tags.go:59-60). Load refuses the source, so this bypasses
// validation and proves Prompt.Render contains the real engine panic. If a
// future engine version returns an ordinary error instead, this fails and
// reservedBindings can be revisited.
func TestRenderContainsEnginePanicOnReboundForloop(t *testing.T) {
	p := mustParseUnchecked(t, "{% for forloop in issue.labels %}{% cycle 'x', 'y' %}{% endfor %}")
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
	if out != "" {
		t.Errorf("Render output = %q, want empty after engine panic", out)
	}
	if !errors.Is(err, ErrEnginePanic) {
		t.Fatalf("Render error = %v, want ErrEnginePanic", err)
	}
	var panicErr *EnginePanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("Render error = %T, want *EnginePanicError", err)
	}
	if panicErr.Value == nil {
		t.Error("EnginePanicError.Value = nil, want recovered panic value")
	}
	stack := string(panicErr.Stack)
	if !strings.Contains(stack, "iteration_tags.go") || !strings.Contains(stack, "(*Prompt).Render") {
		t.Errorf("EnginePanicError.Stack lacks engine and Render frames:\n%s", stack)
	}
	if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "iteration_tags.go") {
		t.Errorf("render error summary contains stack details: %q", err)
	}
}

func TestEnginePanicErrorDoesNotUnwrapPanicValue(t *testing.T) {
	payload := errors.New("engine payload")
	err := &EnginePanicError{Value: payload, Stack: []byte("stack")}
	if !errors.Is(err, ErrEnginePanic) {
		t.Fatal("EnginePanicError does not unwrap to ErrEnginePanic")
	}
	if errors.Is(err, payload) {
		t.Fatal("EnginePanicError unexpectedly unwraps its panic value")
	}
}

func TestLoadRejectsUnsupportedTags(t *testing.T) {
	_, err := load(t, "{% include 'other.liquid' %}")
	if !errors.Is(err, ErrUnsupportedTag) {
		t.Errorf("Load(include) = %v, want ErrUnsupportedTag", err)
	}
	var ut *UnsupportedTagError
	if !errors.As(err, &ut) || ut.Tag != "include" {
		t.Fatalf("Load(include) = %v, want UnsupportedTagError{Tag: include}", err)
	}
}

func TestLoadRejectsParseErrors(t *testing.T) {
	// Note: an unclosed `{{ ...` is not here — the engine's scanner treats it
	// as literal text (only complete {{ }} chunks are objects), so there is
	// nothing to reject; it renders as written.
	for _, src := range []string{
		"{% bogustag %}",
		"{% if attempt %}unclosed",
		// A word in loop-modifier position that is not a modifier: the engine's
		// own loop production refuses it before the walk sees the tag.
		"{% for x in issue.labels bogusmod %}{{ x }}{% endfor %}",
		"{% tablerow x in issue.labels bogusmod %}{{ x }}{% endtablerow %}",
	} {
		if _, err := load(t, src); err == nil {
			t.Errorf("Load(%q) = ok, want parse error", src)
		}
	}
}

func TestLoadErrorLineNumbers(t *testing.T) {
	src := "line one\nline two\n{{ issue.titel }}\n"
	_, err := Load(src, "sub/WORKFLOW.md", 10)
	var uv *UnknownVariableError
	if !errors.As(err, &uv) {
		t.Fatalf("Load = %v, want UnknownVariableError", err)
	}
	if uv.File != "sub/WORKFLOW.md" || uv.Line != 12 {
		t.Errorf("error located at %s:%d, want sub/WORKFLOW.md:12", uv.File, uv.Line)
	}
}

// --- the canonical publish snippet (SPEC §5.6), three ways ---

const canonicalSnippet = `## Publishing

When — and only when — the task is complete:

1. Commit all changes. Work only on the branch already checked out in this
   workspace; never create, switch, or force-update branches.
2. Push it: ` + "`git push origin HEAD`" + `.
3. Open a pull request against ` + "`{{ target_branch }}`" + ` with ` + "`gh pr create --base {{ target_branch | shellescape }}`" + `, and put
   ` + "`Fixes #{{ issue.identifier }}`" + ` in the PR body so the issue closes on merge.
4. Do not merge the pull request. Do not close the issue.

{% if attempt %}
This is attempt {{ attempt }}.
{% if run.previous_outcome == "succeeded" %}Your previous session ended cleanly
but without a published pull request — inspect the workspace, finish the
remaining work, and publish.{% elsif run.previous_outcome %}Your previous session failed
({{ run.previous_outcome }}) — inspect the workspace, recover, and continue.
{% else %}This branch already carries work, but the previous run outcome did
not survive the claim boundary — inspect the workspace and continue.
{% endif %}
{% endif %}`

func TestCanonicalSnippetFirstAttempt(t *testing.T) {
	p := mustLoad(t, canonicalSnippet)
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", TargetBranch: "main", Run: Run{ID: "r-1"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "Fixes #123") {
		t.Errorf("output missing issue identifier:\n%s", out)
	}
	if !strings.Contains(out, `gh pr create --base 'main'`) {
		t.Errorf("output missing trusted target branch:\n%s", out)
	}
	for _, absent := range []string{"This is attempt", "previous session"} {
		if strings.Contains(out, absent) {
			t.Errorf("first attempt output should not contain %q:\n%s", absent, out)
		}
	}
}

func TestCanonicalSnippetContinuation(t *testing.T) {
	p := mustLoad(t, canonicalSnippet)
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 2, Workspace: "/w", TargetBranch: "main", Run: Run{ID: "r-2", PreviousOutcome: "succeeded"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"This is attempt 2.", "ended cleanly"} {
		if !strings.Contains(out, want) {
			t.Errorf("continuation output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "session failed") {
		t.Errorf("continuation output took the failure branch:\n%s", out)
	}
}

func TestCanonicalSnippetFailureRetry(t *testing.T) {
	p := mustLoad(t, canonicalSnippet)
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 3, Workspace: "/w", TargetBranch: "main", Run: Run{ID: "r-3", PreviousOutcome: string(core.FailureStalled)}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"This is attempt 3.", "Your previous session failed", "(stalled)"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure-retry output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ended cleanly") {
		t.Errorf("failure-retry output took the continuation branch:\n%s", out)
	}
}

func TestCanonicalSnippetEvidenceFloor(t *testing.T) {
	p := mustLoad(t, canonicalSnippet)
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 2, Workspace: "/w", TargetBranch: "main", Run: Run{ID: "r-2"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"This is attempt 2.", "previous run outcome did", "not survive the claim boundary"} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence-floor output missing %q:\n%s", want, out)
		}
	}
	for _, invented := range []string{"ended cleanly", "previous session failed"} {
		if strings.Contains(out, invented) {
			t.Errorf("evidence-floor output invented %q:\n%s", invented, out)
		}
	}
}

// --- schema honesty: what the walk admits, the engine actually renders ---

func TestSchemaMatchesEngineBehavior(t *testing.T) {
	p := mustLoad(t, "{{ issue.labels.size }}|{{ issue.labels.first }}|{{ issue.labels[1] }}|{{ issue.state.size }}|{{ issue.created_at }}|{{ issue.blockers.first.open }}")
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "2|ben|bug|4|2026-08-06T12:00:00Z|false"
	if out != want {
		t.Errorf("Render = %q, want %q", out, want)
	}
}

func TestShellEscapePreservesGitValidTargetAsOneArgument(t *testing.T) {
	const target = `release/$USER-$(id)-"quoted"-'single';false`
	p := mustLoad(t, canonicalSnippet)
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, TargetBranch: target}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	const commandPrefix = "gh pr create --base "
	start := strings.Index(out, commandPrefix)
	if start < 0 {
		t.Fatalf("rendered canonical snippet has no publish command:\n%s", out)
	}
	argument := out[start+len(commandPrefix):]
	end := strings.Index(argument, "`, and put")
	if end < 0 {
		t.Fatalf("rendered canonical command has no argument terminator:\n%s", out)
	}
	argument = argument[:end]

	cmd := exec.Command("sh", "-c", `set -- `+argument+`; test "$#" -eq 1 || exit 91; printf '%s' "$1"`)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "USER=expanded"}
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell round trip: %v: %s\nrendered argument: %s", err, got, argument)
	}
	if string(got) != target {
		t.Fatalf("shell round trip = %q, want %q; rendered argument: %s", got, target, argument)
	}
}

func TestRenderBindsWorkspaceTargetAndRun(t *testing.T) {
	p := mustLoad(t, "{{ workspace }} {{ target_branch }} {{ run.id }}")
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/data/ws/issues/123", TargetBranch: "release/stable", Run: Run{ID: "run-7"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "/data/ws/issues/123 release/stable run-7" {
		t.Errorf("Render = %q", out)
	}
}

// --- the render-time strict backstop (SPEC §5.6) ---

func TestBackstopCatchesPropertyAccessOnFilterOutput(t *testing.T) {
	// z is filter output — the walk's checking frontier — so z.bogus loads,
	// evaluates to nil at render, and the strict backstop rejects it there.
	p := mustLoad(t, "{% assign z = issue.state | split: ' ' %}{{ z.bogus }}")
	_, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
	if err == nil {
		t.Fatal("Render = ok, want strict-backstop error on nil emission")
	}
	if errors.Is(err, ErrEnginePanic) {
		t.Errorf("ordinary render error = %v, unexpectedly marked ErrEnginePanic", err)
	}
}

func TestNullEmissionSemantics(t *testing.T) {
	// Known-but-null values emitted unguarded fail the render: the engine
	// cannot tell null from undefined. Guarded emission is the contract.
	p := mustLoad(t, "{{ attempt }}")
	if _, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{}); err == nil {
		t.Fatal("Render({{ attempt }}, first attempt) = ok, want strict error on null emission")
	}
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 2, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
	if err != nil {
		t.Fatalf("Render({{ attempt }}, attempt 2): %v", err)
	}
	if out != "2" {
		t.Errorf("Render = %q, want \"2\"", out)
	}
}

func TestGuardedNullsRenderCleanly(t *testing.T) {
	p := mustLoad(t, "{% if attempt %}a{{ attempt }}{% endif %}{% if run.previous_outcome %}o{{ run.previous_outcome }}{% endif %}done")
	out, err := p.Render(Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "done" {
		t.Errorf("Render = %q, want \"done\"", out)
	}
}

// --- filter probe honesty against the live engine ---

func TestFilterProbeDistinguishesArgumentErrorsFromUndefined(t *testing.T) {
	// divided_by errors on a nil receiver — but it exists, so the probe must
	// say yes; the load then succeeds.
	if _, err := load(t, "{{ attempt | divided_by: 2 }}"); err != nil {
		t.Errorf("Load(divided_by) = %v, want ok", err)
	}
}
