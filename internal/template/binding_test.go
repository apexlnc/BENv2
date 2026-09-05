package template

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// The binding descriptor is meant to be the single spelling of the closed set
// (#64): what the walk checks a reference against and what the engine is
// handed at render come from the same declaration. These tests hold the two
// ends of that claim — the descriptor still says what SPEC §5.6 says, and
// every path it describes really does render.

// The closed set as SPEC §5.6 writes it. Held here as a literal so the
// descriptor is pinned to the spec by something other than itself.
var (
	specRoots      = []string{"attempt", "issue", "run", "target_branch", "workspace"}
	specIssueProps = []string{
		"assignees", "blockers", "body", "created_at", "identifier",
		"labels", "state", "title", "updated_at", "url",
	}
	specRunProps = []string{"id", "previous_attempt", "previous_outcome"}
	// The blocker element comes from the normalized issue model (SPEC §8.3,
	// core.Blocker); §5.6 exposes the object whole.
	specBlockerProps = []string{"identifier", "open", "state"}
)

func TestDescriptorSpellsTheSpecClosedSet(t *testing.T) {
	for _, tc := range []struct {
		what  string
		shape shape
		want  []string
	}{
		{"the closed variable set", rootBinding, specRoots},
		{"issue", issueBinding, specIssueProps},
		{"run", runBinding, specRunProps},
		{"a blocker", blockerBinding, specBlockerProps},
	} {
		if got := sortedKeys(tc.shape.fields); !slices.Equal(got, tc.want) {
			t.Errorf("descriptor for %s = %v, SPEC says %v", tc.what, got, tc.want)
		}
	}
}

// describedPaths enumerates every path a prompt can write against a described
// shape, including the pseudo-properties the walk admits on strings and
// arrays. Nothing here is a list of names — it is derived from the descriptor,
// which is the point: a field added to the descriptor is covered the moment it
// exists.
func describedPaths(prefix string, s shape) []string {
	switch s.kind {
	case kindObject:
		var out []string
		for _, name := range sortedKeys(s.fields) {
			out = append(out, describedPaths(prefix+"."+name, s.fields[name])...)
		}
		return out
	case kindArray:
		out := []string{prefix, prefix + ".size"}
		// first and the static index reach the same element two ways; both
		// are paths the walk admits, so both are checked.
		out = append(out, describedPaths(prefix+".first", *s.elem)...)
		out = append(out, describedPaths(prefix+"[0]", *s.elem)...)
		return out
	case kindString:
		if s.untrusted {
			// An untrusted value may only be emitted whole (SPEC §5.6), so
			// the pseudo-property is not a path a prompt can write.
			return []string{prefix}
		}
		return []string{prefix, prefix + ".size"}
	default:
		return []string{prefix}
	}
}

// Every path the descriptor describes loads and renders. This is the drift the
// descriptor exists to make impossible, asserted end to end: a shape the walk
// admits but the projection never binds would load and then fail on the
// render-time backstop.
func TestEveryDescribedPathLoadsAndRenders(t *testing.T) {
	var paths []string
	for _, root := range sortedKeys(rootBinding.fields) {
		paths = append(paths, describedPaths(root, rootBinding.fields[root])...)
	}
	// Guard against the enumeration silently collapsing to nothing.
	if len(paths) < len(specIssueProps) {
		t.Fatalf("enumerated only %d paths: %v", len(paths), paths)
	}
	for _, path := range paths {
		src := "{{ " + path + " }}"
		p, err := load(t, src)
		if err != nil {
			t.Errorf("Load(%q) = %v, want ok", src, err)
			continue
		}
		out, err := p.Render(totalVars(), Limits{})
		if err != nil {
			t.Errorf("Render(%q) = %v, want ok", src, err)
			continue
		}
		if out == "" {
			t.Errorf("Render(%q) = \"\"; every value in totalVars is non-empty", src)
		}
	}
}

// The descriptor makes a name impossible to spell twice, but not a getter: two
// entries could still project the same field of core.Issue. Every scalar the
// descriptor describes on `issue` therefore has to render a distinct value —
// which is checkable without writing the expected values down anywhere, and so
// without reintroducing the second list the descriptor exists to remove.
func TestDescribedScalarsProjectDistinctValues(t *testing.T) {
	seen := map[string]string{}
	for _, name := range sortedKeys(issueBinding.fields) {
		if issueBinding.fields[name].kind != kindString {
			continue
		}
		src := "{{ issue." + name + " }}"
		out, err := mustLoad(t, src).Render(totalVars(), Limits{})
		if err != nil {
			t.Errorf("Render(%q): %v", src, err)
			continue
		}
		if other, dup := seen[out]; dup {
			t.Errorf("issue.%s and issue.%s both render %q — one projection is mis-wired", other, name, out)
		}
		seen[out] = name
	}
}

// The other direction: the engine is handed exactly the fields the descriptor
// describes, and no more. Read off the binding itself rather than a rendered
// map dump — liquid.Bindings *is* what the engine is given, and a fenced value
// carries the spaces and newlines that make Go's unquoted map formatting
// unparsable.
func TestBoundObjectsCarryExactlyTheDescribedFields(t *testing.T) {
	bound := bindings(totalVars())
	issue, ok := bound["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue binds as %T, want map[string]any", bound["issue"])
	}
	run, ok := bound["run"].(map[string]any)
	if !ok {
		t.Fatalf("run binds as %T, want map[string]any", bound["run"])
	}
	blockers, ok := issue["blockers"].([]any)
	if !ok || len(blockers) == 0 {
		t.Fatalf("issue.blockers binds as %T, want a non-empty []any", issue["blockers"])
	}
	blocker, ok := blockers[0].(map[string]any)
	if !ok {
		t.Fatalf("a blocker binds as %T, want map[string]any", blockers[0])
	}

	for _, tc := range []struct {
		what  string
		bound map[string]any
		shape shape
	}{
		{"issue", issue, issueBinding},
		{"run", run, runBinding},
		{"a blocker", blocker, blockerBinding},
	} {
		got := slices.Sorted(maps.Keys(tc.bound))
		if want := sortedKeys(tc.shape.fields); !slices.Equal(got, want) {
			t.Errorf("%s binds fields %v, the descriptor describes %v", tc.what, got, want)
		}
	}
}

// Arrays bind element-wise through the descriptor rather than leaking the Go
// slice, so the engine's array operations still work on them.
func TestDescribedArraysBindAsArrays(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{"{{ issue.labels | join: '-' }}", "ben-bug-queue"},
		{"{{ issue.labels.size }}", "3"},
		{"{{ issue.labels[2] }}", "queue"},
		{"{{ issue.blockers.last.identifier }}", "120"},
		{"{% for b in issue.blockers %}{{ b.state }},{% endfor %}", "closed,open,open,"},
	} {
		p := mustLoad(t, tc.src)
		out, err := p.Render(totalVars(), Limits{})
		if err != nil {
			t.Errorf("Render(%q): %v", tc.src, err)
			continue
		}
		if out != tc.want {
			t.Errorf("Render(%q) = %q, want %q", tc.src, out, tc.want)
		}
	}
}

// The two nullable members of the closed set (SPEC §5.6) still bind null on a
// first attempt, now that the projection is the descriptor's.
func TestDescriptorBindsFirstAttemptNulls(t *testing.T) {
	first := Vars{Issue: testIssue(), Attempt: 1, Workspace: "/w", Run: Run{ID: "r"}}
	p := mustLoad(t, "{% if attempt %}a{% endif %}{% if run.previous_outcome %}o{% endif %}{{ run.id }}")
	out, err := p.Render(first, Limits{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "r" {
		t.Errorf("Render = %q, want %q — a nullable bound non-null", out, "r")
	}
	for _, src := range []string{"{{ attempt }}", "{{ run.previous_outcome }}"} {
		p := mustLoad(t, src)
		if _, err := p.Render(first, Limits{}); err == nil {
			t.Errorf("Render(%q, first attempt) = ok, want the strict backstop to refuse a null emission", src)
		}
	}
}

// objectKeysFromDump reads the key set out of the first `map[...]` in a
// rendered object, dropping engine internals — forloop's ".cycles" backs
// {% cycle %} and is not a reachable Liquid identifier. Bracket depth is
// tracked because ".cycles" is itself a map.
func objectKeysFromDump(out string) ([]string, error) {
	i := strings.Index(out, "map[")
	if i < 0 {
		return nil, fmt.Errorf(`no "map[" in %q; the engine changed how it emits an object`, out)
	}
	body, ok := untilMatchingBracket(out[i+len("map["):])
	if !ok {
		return nil, fmt.Errorf("unterminated map in %q", out)
	}
	var keys []string
	for _, entry := range splitTopLevel(body) {
		k, _, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("unparsable map entry %q", entry)
		}
		if strings.HasPrefix(k, ".") {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no fields parsed out of the map dump %q", out)
	}
	slices.Sort(keys)
	return keys, nil
}
