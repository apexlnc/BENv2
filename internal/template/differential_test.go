package template

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/expressions"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Differential testing of the load-time scanner against the engine it stands
// in for (#64).
//
// The scanner lexes each node's expression itself and reimplements enough of
// Liquid's grammar to pull out variable references and filter names, so it can
// disagree with the engine at the margins. Neither direction of disagreement
// is harmless: in the strict direction it is a false refusal at load, in the
// lax direction it is an unknown variable reaching render — the failure
// SPEC §5.6 exists to prevent. So: generate a template, ask both, require the
// same verdict.
//
// What the engine can actually be asked. Its only definedness signals are
// StrictVariables — which fires in exactly one place, emitting a nil from an
// {{ … }} object node (osteele/liquid render/render.go) — and UndefinedFilter,
// raised when an unknown filter is evaluated. Two consequences shape every
// case below:
//
//   - A reference the engine never *emits* is invisible to it. It says nothing
//     about a condition, a tag argument, or an untaken branch. Every generated
//     case therefore emits its generated path somewhere the engine evaluates.
//   - A filter destroys the signal: `nil | append: '!'` renders "!", so an
//     undefined variable under a filter chain is something the engine cannot
//     report. The variable half of the differential is therefore asked of a
//     filter-free *control* template, and the filtered variant is held against
//     that control instead — a filter chain must neither hide a reference the
//     control caught nor invent one it did not.

// --- verdicts -------------------------------------------------------------

type loadVerdict int

const (
	loadAccepts loadVerdict = iota
	loadUnknownVariable
	loadUnknownFilter
	// loadOtherRefusal is a refusal the differential does not speak to: a
	// parse error (the engine refused it first) or one of the contract's
	// deliberate extra strictnesses — {% include %}, binding forloop, reading
	// an untrusted variable anywhere but a whole emission (SPEC §5.6). The
	// engine has no notion of any of them, so there is no verdict to compare.
	loadOtherRefusal
)

func (v loadVerdict) String() string {
	return [...]string{"accepts", "unknown variable", "unknown filter", "other refusal"}[v]
}

func scanVerdict(src string) (loadVerdict, error) {
	_, err := Load(src, "WORKFLOW.md", 1)
	var uv *UnknownVariableError
	var uf *UnknownFilterError
	switch {
	case err == nil:
		return loadAccepts, nil
	case errors.As(err, &uv):
		return loadUnknownVariable, err
	case errors.As(err, &uf):
		return loadUnknownFilter, err
	default:
		return loadOtherRefusal, err
	}
}

type engineVerdict int

const (
	engineRenders engineVerdict = iota
	engineUndefinedVariable
	engineUndefinedFilter
	// engineOtherFailure is a render failure that is not about definedness —
	// a filter complaining about an argument type, say. It is a legitimate
	// contained run failure (SPEC §5.7) and tells the differential nothing.
	engineOtherFailure
)

func (v engineVerdict) String() string {
	return [...]string{"renders", "undefined variable", "undefined filter", "other failure"}[v]
}

// undefinedVariableMessage is the engine's strict-variables signal. It is an
// unexported errors.New value with no type to match on, so its text is the
// only handle; TestEngineSignalsAreStillRecognized pins it against the live
// engine so a version bump cannot quietly make this classifier blind.
const undefinedVariableMessage = "undefined variable"

// engineOutcome renders src with the strictness backstop, deliberately
// bypassing Load: the question is what the engine does with a template the
// walk may have refused.
func engineOutcome(t *testing.T, src string) engineVerdict {
	t.Helper()
	renderEng, _ := engines()
	tpl, err := renderEng.ParseString(src)
	if err != nil {
		// Unreachable for the cases the differential keeps: Load parses with
		// this same engine, so a parse failure is already a loadOtherRefusal.
		t.Fatalf("engine failed to parse %q: %v", src, err)
	}
	_, rerr := tpl.RenderString(bindings(totalVars()))
	if rerr == nil {
		return engineRenders
	}
	var cause error = rerr
	for cause != nil {
		if _, undefined := cause.(expressions.UndefinedFilter); undefined {
			return engineUndefinedFilter
		}
		if cause.Error() == undefinedVariableMessage {
			return engineUndefinedVariable
		}
		se, ok := cause.(liquid.SourceError)
		if !ok {
			return engineOtherFailure
		}
		cause = se.Cause()
	}
	return engineOtherFailure
}

// totalVars binds every variable in the closed set to a non-null value, with
// arrays long enough that every static index the generator emits is in range.
// The differential reads the engine's nils as "undefined", so no legitimately
// null value (SPEC §5.6: numbered attempt 1, or no prior run outcome) may be
// in play.
func totalVars() Vars {
	return Vars{
		Issue: core.Issue{
			Identifier: "123",
			Title:      "Fix the flux capacitor",
			Body:       "It fluxes when it should capacitate.",
			Labels:     []string{"ben", "bug", "queue"},
			State:      "open",
			Assignees:  []string{"ben-bot", "someone", "else"},
			Blockers: []core.Blocker{
				{Identifier: "122", State: "closed", Open: false},
				{Identifier: "121", State: "open", Open: true},
				{Identifier: "120", State: "open", Open: true},
			},
			URL:       "https://github.com/o/r/issues/123",
			CreatedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC),
		},
		Attempt:      2,
		Workspace:    "/w/123",
		TargetBranch: "main",
		Run: Run{
			ID:              "r-2",
			PreviousOutcome: "succeeded",
			PreviousAttempt: "attempt 1 ended: stalled",
		},
	}
}

// --- the one deliberate deviation ----------------------------------------

// The engine binds each described object as a Go map, and its values package
// answers `size` on any map with the key count. That number is an artifact of
// BEN's projection rather than a member of the closed set (SPEC §5.6) — it
// would change under a workflow author's feet the next time the issue model
// grows a field — so the walk refuses it. Consulted only where the engine
// renders, which is what makes the suffix test sound: a rejected `bogus.size`
// never reaches it, because the engine has no map to size there. Pinned by
// TestObjectSizeIsRefusedDeliberately.
func deliberatelyStricter(err error) bool {
	var uv *UnknownVariableError
	return errors.As(err, &uv) && strings.HasSuffix(uv.Ref, ".size")
}

// --- the agreement --------------------------------------------------------

// checkAgreement is the property under test, over one generated case.
func checkAgreement(t *testing.T, c genCase) {
	t.Helper()
	control, controlErr := scanVerdict(c.control)
	if control != loadOtherRefusal {
		switch outcome := engineOutcome(t, c.control); {
		case control == loadAccepts && outcome == engineUndefinedVariable && !c.frontier:
			t.Errorf("lax scanner: Load(%q) = ok, but an unknown variable reached render", c.control)
		case control != loadAccepts && outcome == engineRenders && !deliberatelyStricter(controlErr):
			t.Errorf("false refusal: Load(%q) = %v, but the engine renders it", c.control, controlErr)
		}
	}
	if c.filtered == "" {
		return
	}

	// The filtered variant. Its filter names are still answerable by the
	// engine directly; its references are held against the control, whose own
	// verdict the engine has just confirmed.
	filtered, filteredErr := scanVerdict(c.filtered)
	if filtered == loadOtherRefusal {
		return
	}
	switch outcome := engineOutcome(t, c.filtered); {
	case filtered == loadAccepts && outcome == engineUndefinedFilter:
		t.Errorf("lax scanner: Load(%q) = ok, but the engine has no such filter", c.filtered)
	case filtered == loadUnknownFilter && outcome == engineRenders:
		t.Errorf("false refusal: Load(%q) = %v, but the engine renders it", c.filtered, filteredErr)
	}
	switch {
	case control == loadUnknownVariable && filtered == loadAccepts:
		t.Errorf("filter chain hid a reference: Load(%q) = %v, but Load(%q) = ok",
			c.control, controlErr, c.filtered)
	case control == loadAccepts && filtered == loadUnknownVariable:
		t.Errorf("filter chain invented a reference: Load(%q) = ok, but Load(%q) = %v",
			c.control, c.filtered, filteredErr)
	}
}

// --- the generator --------------------------------------------------------

// gen draws generator choices from a finite byte stream; past its end every
// choice is zero, so generation always terminates.
type gen struct {
	src []byte
	i   int
	// frontier records that the path left the walk's static model — a
	// computed index, whose result the walk cannot know and admits by design,
	// leaving the residue to the render-time backstop.
	frontier bool
}

func (g *gen) next() byte {
	if g.i >= len(g.src) {
		return 0
	}
	b := g.src[g.i]
	g.i++
	return b
}

func (g *gen) pick(n int) int { return int(g.next()) % n }

func (g *gen) choose(from []string) string { return from[g.pick(len(from))] }

// The pools mix material that is in the closed set with material that is not,
// and with the loop-modifier and filter spellings whose grammar position, not
// spelling, decides their meaning (#18). Which of them is valid is for the
// scanner and the engine to say — that is the whole question.
var (
	// Roots nothing binds — at least not where the generator writes them: `v`
	// and `forloop` exist only inside a loop body.
	genUnboundRoots = []string{
		"bogus", "isue", "size", "reversed", "limit", "offset", "cols",
		"v", "forloop",
	}

	genProps = []string{
		"identifier", "title", "body", "labels", "state", "assignees",
		"blockers", "url", "created_at", "updated_at", // issue
		"id", "previous_outcome", "previous_attempt", // run
		"open",                  // blocker
		"size", "first", "last", // string and array pseudo-properties
		"index", "index0", "rindex", "length", // forloop
		"titel", "bogus", "dispatchable", "push", "col", "cycles",
	}

	genFilters = []string{
		"upcase", "downcase", "size", "strip", "sort", "reverse",
		"join: ', '", "split: ' '", "append: '!'", "prepend: '>'",
		"replace: 'a', 'b'", "truncate: 5", "default: 'd'", "plus: 1",
		"divided_by: 2", "map: 'identifier'", "date: '%Y'",
		// Not defined by the engine. `reversed`, `limit` and friends are the
		// loop-modifier spellings, which mean nothing in filter position.
		"bogusfilter", "reversed", "limit: 1", "offset: 2", "cols: 3", "push",
	}

	// The modifier run the walk parses by grammar position.
	genLoopModifiers = []string{
		"", "reversed", "limit: 2", "offset: 1", "limit: 2 offset: 1 reversed",
	}
)

// admissible lists the names a shape admits, for steering.
func admissible(s shape) []string {
	switch s.kind {
	case kindObject:
		return sortedKeys(s.fields)
	case kindString:
		return []string{"size"}
	case kindArray:
		return []string{"size", "first", "last"}
	default:
		return nil
	}
}

// path generates one variable reference: a root plus an access chain of
// properties, bracketed keys, static indices, and computed indices.
//
// It walks the descriptor while it generates, so most steps are steps the
// descriptor allows and the interesting cases — one wrong step at the end of a
// long valid prefix — are common instead of vanishingly rare. Steering by the
// descriptor is not the same as judging by it: the engine remains the oracle,
// so a descriptor that is wrong changes which templates get generated, never
// who is held to be right about one.
func (g *gen) path(scope map[string]shape) string {
	var b strings.Builder
	root := g.choose(genUnboundRoots)
	if g.pick(4) != 0 {
		root = g.choose(sortedKeys(scope))
	}
	b.WriteString(root)

	s, tracking := scope[root]
	for n := g.pick(4); n > 0; n-- {
		name := g.choose(genProps)
		if names := admissible(s); tracking && len(names) > 0 && g.pick(4) != 0 {
			name = g.choose(names)
		}
		switch g.pick(5) {
		case 0, 1:
			b.WriteString("." + name)
			s, tracking = s.property(name)
		case 2:
			b.WriteString(`["` + name + `"]`)
			s, tracking = s.keyIndex(name)
		case 3:
			// In range for every array in totalVars, so the walk is still
			// expected to model a static index exactly.
			fmt.Fprintf(&b, "[%d]", g.pick(3))
			s, tracking = s.index()
		case 4:
			// A computed index can land anywhere, including out of range and
			// on a key no map has.
			b.WriteString("[" + g.choose(genUnboundRoots) + "]")
			s, tracking = s.index()
			g.frontier = true
		}
	}
	return b.String()
}

// filtered wraps a path in a filter chain, either as the chain's receiver or
// as an argument to one — the position that most easily hides a reference from
// a scanner that lexes filter arguments itself.
func (g *gen) filtered(path string) string {
	var b strings.Builder
	if g.pick(4) == 0 {
		b.WriteString("issue.title | append: " + path)
	} else {
		b.WriteString(path)
	}
	for n := 1 + g.pick(2); n > 0; n-- {
		b.WriteString(" | " + g.choose(genFilters))
	}
	return b.String()
}

// caseScope is what is in scope where a case shape puts its expression: the
// closed set, plus the loop binder and the engine's forloop for the two shapes
// that emit inside a loop body.
func caseScope(shape int) map[string]shape {
	scope := rootScope()
	switch shape {
	case 5: // {% for v in issue.labels %}
		scope["v"] = *issueBinding.fields["labels"].elem
		scope["forloop"] = forloopShape
	case 6: // {% tablerow v in issue.blockers %}
		scope["v"] = *issueBinding.fields["blockers"].elem
		scope["forloop"] = forloopShape
	}
	return scope
}

// A genCase is one generated template in two forms: the filter-free control
// the engine can answer for, and — when the byte stream asked for filters —
// the same case with a filter chain, which is held against the control.
type genCase struct {
	control  string
	filtered string
	frontier bool
}

// generate builds one case. Every shape emits the expression on the path the
// engine takes: at the top level, inside a loop body over a collection that is
// never empty, or inside a branch the total binding always takes.
func generate(data []byte) genCase {
	g := &gen{src: data}
	shape := g.pick(11)
	path := g.path(caseScope(shape))
	mods := g.choose(genLoopModifiers)

	build := func(e string) string {
		switch shape {
		case 0:
			return "{{ " + e + " }}"
		case 1:
			return "{% assign v = " + e + " %}{{ v }}"
		case 2:
			return "{% capture v %}{{ " + e + " }}{% endcapture %}{{ v }}"
		case 3:
			return "{{ " + e + " }}{% if " + e + " %}taken{% endif %}"
		case 4:
			return "{{ " + e + " }}{% unless " + e + " %}taken{% endunless %}"
		case 5:
			return "{% for v in issue.labels " + mods + " %}{{ " + e + " }}{% endfor %}"
		case 6:
			return "{% tablerow v in issue.blockers " + mods + " %}{{ " + e + " }}{% endtablerow %}"
		case 7:
			return "{{ " + e + " }}{% for v in " + e + " " + mods + " %}{{ v }}{% endfor %}"
		case 8:
			return "{{ " + e + " }}{% for v in issue.labels limit: " + e + " %}{{ v }}{% endfor %}"
		case 9:
			return "{{ " + e + " }}{% case issue.state %}{% when " + e + " %}m{% else %}n{% endcase %}"
		default:
			return "{% if attempt %}{{ " + e + " }}{% endif %}"
		}
	}

	c := genCase{control: build(path), frontier: g.frontier}
	if g.pick(2) == 0 {
		c.filtered = build(g.filtered(path))
	}
	return c
}

// corpusBytes expands a seed into generator input. It is a splitmix64 written
// out rather than a math/rand call so the corpus is fixed forever and a
// reported failing seed replays exactly.
func corpusBytes(seed uint64) []byte {
	b := make([]byte, 24)
	x := seed
	for i := range b {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		b[i] = byte(z >> 31)
	}
	return b
}

// --- the tests ------------------------------------------------------------

// The sweep is what puts the differential in `make check`, rather than leaving
// it to something only an explicit fuzzing run would ever exercise.
func TestScannerAgreesWithEngineOverGeneratedCorpus(t *testing.T) {
	const seeds = 5000
	for seed := range uint64(seeds) {
		checkAgreement(t, generate(corpusBytes(seed)))
		if t.Failed() {
			t.Fatalf("first disagreement at seed %d; replay it with corpusBytes(%d)", seed, seed)
		}
	}
}

func FuzzScannerAgreesWithEngine(f *testing.F) {
	for seed := range uint64(64) {
		f.Add(corpusBytes(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkAgreement(t, generate(data))
	})
}

// The differential is only as good as its reading of the engine's failures.
// Both signals are pinned against the live engine here: if a version bump
// renames the strict-variables error or retypes the undefined-filter one, this
// fails loudly instead of silently classifying every disagreement as
// engineOtherFailure and passing everything.
func TestEngineSignalsAreStillRecognized(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want engineVerdict
	}{
		{"{{ bogus }}", engineUndefinedVariable},
		{"{{ issue.titel }}", engineUndefinedVariable},
		{"{{ issue.title | bogusfilter }}", engineUndefinedFilter},
		{"{{ issue.title | divided_by: 2 }}", engineOtherFailure},
		{"{{ issue.title }}", engineRenders},
	} {
		if got := engineOutcome(t, tc.src); got != tc.want {
			t.Errorf("engineOutcome(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
	// The blindness the control/filtered split exists for: under a filter the
	// engine has no undefined variable to report.
	if got := engineOutcome(t, "{{ bogus | append: '!' }}"); got != engineRenders {
		t.Errorf("engineOutcome(filtered undefined) = %v, want renders", got)
	}
}

// The one place the walk is deliberately stricter than the engine about a
// variable path, held still so it stays deliberate.
func TestObjectSizeIsRefusedDeliberately(t *testing.T) {
	for _, src := range []string{
		"{{ issue.size }}",
		"{{ run.size }}",
		"{% for v in issue.blockers %}{{ v.size }}{% endfor %}",
	} {
		lv, lerr := scanVerdict(src)
		if lv != loadUnknownVariable {
			t.Errorf("Load(%q) = %v (%v), want unknown variable", src, lv, lerr)
			continue
		}
		if !deliberatelyStricter(lerr) {
			t.Errorf("Load(%q) refusal %v is not recognized as the deliberate one", src, lerr)
		}
		if got := engineOutcome(t, src); got != engineRenders {
			t.Errorf("engineOutcome(%q) = %v, want renders — the deviation has gone away", src, got)
		}
	}
	// It is `size` on an object only: the rest of a map stays closed, and the
	// pseudo-properties the schema does admit agree with the engine.
	for _, src := range []string{"{{ issue.first }}", "{{ issue.last }}", "{{ run.bogus }}"} {
		if got := engineOutcome(t, src); got != engineUndefinedVariable {
			t.Errorf("engineOutcome(%q) = %v, want undefined variable", src, got)
		}
	}
}
