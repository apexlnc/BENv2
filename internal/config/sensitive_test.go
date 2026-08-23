package config

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// setPath plants v at a segment path inside a provider block, creating the
// intermediate maps a nested declaration needs.
func setPath(block map[string]any, segments []string, v any) {
	for i, seg := range segments {
		if i == len(segments)-1 {
			block[seg] = v
			return
		}
		nested, ok := block[seg].(map[string]any)
		if !ok {
			nested = map[string]any{}
			block[seg] = nested
		}
		block = nested
	}
}

// canaryBlock seeds a block for one kind and plants a distinct canary at every
// path the kind declares sensitive.
//
// Two rounds, because one declaration is dynamic: `env.*` reports whatever keys
// the block holds, so a block with no `env` reports none. The seed supplies one,
// the first round adds canaries for every static key the kind names, and the
// second round asks again over the enriched block. Anything a third round could
// add would be a declaration that grows from its own canaries, which no kind does.
// The seeded `env` keys include one that path syntax cannot spell plainly. A
// declaration is segments and provenance is a *joined* path, so the two are
// joined by the same helper or they are not the same path — and dot-joining a
// key containing a dot silently produces a path that matches nothing, which
// fails permissively: the value prints. A block of well-behaved identifiers
// never exercises the branch that prevents it.
func canaryBlock(prefix string, declare func(map[string]any) [][]string) (map[string]any, map[string]string) {
	block := map[string]any{"env": map[string]any{
		"CANARY_ENV_KEY":     prefix + "-env-canary",
		"CANARY.DOTTED.KEY":  prefix + "-dotted-canary",
		"CANARY[BRACKET]KEY": prefix + "-bracket-canary",
	}}
	canaries := map[string]string{}
	for round := range 2 {
		for _, segments := range declare(block) {
			joined := strings.Join(segments, ".")
			if _, planted := canaries[joined]; planted {
				continue
			}
			canary := fmt.Sprintf("%s-canary-%s-r%d", prefix, strings.ReplaceAll(joined, ".", "-"), round)
			setPath(block, segments, canary)
			canaries[joined] = canary
		}
	}
	return block, canaries
}

// Every value a kind calls sensitive is hidden in every `config effective`
// rendering, whatever its provenance (SPEC §5.8).
//
// Table-driven over internal/registry rather than over a list written here, and
// over the kinds' own declarations rather than over key names, so this covers a
// key added to a declaration and a *kind* added to the registry without being
// edited. A renderer that acquires a redaction path of its own fails in whichever
// half skipped the shared one — which is the drift this shape exists to prevent.
// `ben status` renders state only and deliberately never carries provider config.
//
// The canaries are literals, not `$VAR` references, which is the point: on the
// old provenance-only rule every one of them printed in the clear, on the
// reasoning that a literal is already visible to anyone who can read the repo —
// true of the repo, and not of output that gets pasted into a pull request.
func TestEveryDeclaredSensitiveFieldIsRedactedInEveryRendering(t *testing.T) {
	for _, trackerKind := range registry.TrackerNames() {
		for _, agentKind := range registry.RunnerNames() {
			t.Run(trackerKind+"/"+agentKind, func(t *testing.T) {
				tk, _ := registry.Tracker(trackerKind)
				ak, _ := registry.Runner(agentKind)
				trackerBlock, trackerCanaries := canaryBlock("tracker", tk.SensitiveFields)
				agentBlock, agentCanaries := canaryBlock("agent", ak.SensitiveFields)

				if len(trackerCanaries) == 0 || len(agentCanaries) == 0 {
					t.Fatalf("a kind declared nothing sensitive: tracker=%v agent=%v",
						trackerCanaries, agentCanaries)
				}

				def, err := Load(writeWorkflow(t, sensitiveWorkflow(t,
					trackerKind, trackerBlock, agentKind, agentBlock)))
				if err != nil {
					t.Fatal(err)
				}

				text := EffectiveText(def)
				raw, err := EffectiveJSON(def)
				if err != nil {
					t.Fatal(err)
				}
				jsonOut := string(raw)

				for _, side := range []struct {
					where    string
					canaries map[string]string
				}{{"tracker.provider", trackerCanaries}, {"agent.provider", agentCanaries}} {
					for path, canary := range side.canaries {
						if strings.Contains(text, canary) {
							t.Errorf("EffectiveText prints %s.%s in the clear", side.where, path)
						}
						if strings.Contains(jsonOut, canary) {
							t.Errorf("EffectiveJSON prints %s.%s in the clear", side.where, path)
						}
					}
				}
			})
		}
	}
}

// A provider value under a key nobody declared is still redacted when it came
// from the environment. Sensitivity joins provenance; it does not replace it,
// and reading the change the other way would un-redact every env-resolved value
// an adapter did not think to name.
func TestUndeclaredEnvResolvedValuesStayRedacted(t *testing.T) {
	t.Setenv("UNDECLARED_SECRET", "env-resolved-canary")
	def, err := Load(writeWorkflow(t, sensitiveWorkflow(t,
		"github", map[string]any{"api_url": "$UNDECLARED_SECRET"},
		"claude-code", map[string]any{"model": "$UNDECLARED_SECRET"})))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"text": EffectiveText(def), "json": string(raw)} {
		if strings.Contains(out, "env-resolved-canary") {
			t.Errorf("%s printed an env-resolved value under an undeclared key", name)
		}
	}
}

// An address is not a credential. Redacting one would cost an operator the two
// facts they most need from this output while protecting nothing.
func TestNonSensitiveProviderValuesStayReadable(t *testing.T) {
	def, err := Load(writeWorkflow(t, sensitiveWorkflow(t,
		"github", map[string]any{"repo": "acme/widgets"},
		"claude-code", map[string]any{"permission_mode": "bypassPermissions"})))
	if err != nil {
		t.Fatal(err)
	}
	text := EffectiveText(def)
	for _, want := range []string{"acme/widgets", "bypassPermissions"} {
		if !strings.Contains(text, want) {
			t.Errorf("EffectiveText redacted %q, which is an address, not a credential:\n%s", want, text)
		}
	}
}

// sensitiveWorkflow renders a loadable workflow around two provider blocks.
func sensitiveWorkflow(t *testing.T, trackerKind string, trackerProvider map[string]any,
	agentKind string, agentProvider map[string]any,
) string {
	t.Helper()
	if _, ok := trackerProvider["repo"]; !ok {
		trackerProvider["repo"] = "acme/widgets"
	}
	doc := map[string]any{
		"tracker": map[string]any{
			"kind": trackerKind, "provider": trackerProvider,
			"required_labels": []string{"ben"},
		},
		"agent": map[string]any{"kind": agentKind, "provider": agentProvider},
		// SPEC §5.2.9: required, no default. Not what this test is about; stated
		// because every workflow must state it.
		"deployment": map[string]any{"mode": "attended"},
	}
	front, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return "---\n" + string(front) + "---\nDo the work described in {{ issue.title }}.\n"
}

// The renderers must test sensitivity *before* descending, or a declaration
// naming a whole subtree is walked and printed leaf by leaf — each leaf
// undeclared on its own, and the subtree its parent named never hidden.
//
// Driven through a hand-built redactor rather than a registered kind, because no
// v1 kind declares a nested map today: the ordering is a property of the renderer,
// and a rule only reachable once some future adapter declares one is a rule
// nothing would notice breaking in the meantime.
func TestASensitiveSubtreeIsRedactedWholeRatherThanDescendedInto(t *testing.T) {
	block := map[string]any{
		"settings": map[string]any{
			"nested_secret": "subtree-canary",
			"deeper":        map[string]any{"also": "deep-canary"},
		},
	}
	red := redactor{
		prov:      Provenance{},
		sensitive: map[string]bool{"agent.provider.settings": true},
	}

	var b strings.Builder
	writeProviderEntries(&b, &WorkflowDefinition{Provenance: red.prov}, red, 1, "agent.provider", block)
	text := b.String()

	for _, canary := range []string{"subtree-canary", "deep-canary"} {
		if strings.Contains(text, canary) {
			t.Errorf("EffectiveText printed %q from inside a sensitive subtree:\n%s", canary, text)
		}
	}
	if !strings.Contains(text, Redacted) {
		t.Errorf("the subtree was not redacted at all:\n%s", text)
	}

	// And the JSON half, which descends through the same redactor.
	out := red.block("agent.provider", block)
	if got := out["settings"]; got != Redacted {
		t.Errorf("EffectiveJSON settings = %v, want the whole subtree redacted", got)
	}
}
