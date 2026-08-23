package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// workflowWith builds a workflow whose tracker and agent provider blocks are
// spelled by the caller, which is the whole variable in these tests.
// The agent block carries permission_mode because without it the block is not
// structurally valid, and a bypass that only ever reached an unloadable file
// would not be a bypass. Load does not run Structural, so its absence would not
// have failed the *load* — which is exactly the kind of accident that lets a
// negative test pass for the wrong reason.
func workflowWith(trackerProvider, agentProvider string) string {
	return "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
		trackerProvider +
		"  required_labels: [\"ben\"]\nagent:\n  kind: claude-code\n  provider:\n" +
		"    permission_mode: bypassPermissions\n" +
		agentProvider +
		"deployment:\n  mode: attended\n" +
		"---\nDo the work described in {{ issue.title }}.\n"
}

// SPEC §10.2 names exactly two credentials and §6.7 gives publishing to the
// agent, so the tracker credential — which authorizes issue writes, assignment
// and labels — must never reach an agent process. Nothing enforced that until
// #47: an agent holding it can rewrite the queue that dispatched it.
//
// The rows are the four shapes that matter, and the third is the bug as it
// actually stood in BEN's own WORKFLOW.md.
func TestLoadRefusesASharedCredential(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracker string
		agent   string
		// wantVar is the collision; "" means the file must load.
		wantVar string
		// wantTrackerField and wantAgentField are the paths the refusal cites,
		// "" where a side named the variable rather than a field.
		wantTrackerField, wantAgentField string
	}{
		{
			name:             "the same variable on both sides, by field and by name",
			tracker:          "    token: $SHARED_TOKEN\n",
			agent:            "    env_passthrough: [SHARED_TOKEN]\n",
			wantVar:          "SHARED_TOKEN",
			wantTrackerField: "tracker.provider.token",
		},
		{
			// The spelling a check on the *child's* key names would miss, and
			// the one an operator reaches for first: `gh` wants GH_TOKEN and the
			// secret manager exports something else.
			name:             "renamed on the way into the child",
			tracker:          "    token: $SHARED_TOKEN\n",
			agent:            "    env:\n      GH_TOKEN: $SHARED_TOKEN\n",
			wantVar:          "SHARED_TOKEN",
			wantTrackerField: "tracker.provider.token",
			wantAgentField:   "agent.provider.env.GH_TOKEN",
		},
		{
			// #47 exactly: the tracker names no variable at all, so the
			// collision is invisible unless the kind declares the fallback its
			// Ready reads.
			name:    "the tracker's undeclared fallback",
			tracker: "",
			agent:   "    env_passthrough: [GITHUB_TOKEN]\n",
			wantVar: "GITHUB_TOKEN",
		},
		{
			// The split working: two variables, one child name. This must load,
			// or the check has made the intended configuration unexpressible.
			name:    "two distinct sources under one child name",
			tracker: "    token: $TRACKER_TOKEN\n",
			agent:   "    env:\n      GH_TOKEN: $PUBLISH_TOKEN\n",
			wantVar: "",
		},
		{
			// A literal secret has no variable identity, so there is nothing to
			// collide on — and §5.5 is what makes a name the thing the file says.
			name:    "a literal tracker token cannot collide",
			tracker: "    token: ghp_literal_not_a_reference\n",
			agent:   "    env_passthrough: [GITHUB_TOKEN]\n",
			wantVar: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range []string{"SHARED_TOKEN", "TRACKER_TOKEN", "PUBLISH_TOKEN"} {
				t.Setenv(v, "value-of-"+v)
			}

			_, err := Load(writeWorkflow(t, workflowWith(tc.tracker, tc.agent)))
			if tc.wantVar == "" {
				if err != nil {
					t.Fatalf("Load = %v, want the split to be expressible", err)
				}
				return
			}

			var shared *CredentialSharedError
			if !errors.As(err, &shared) {
				t.Fatalf("Load = %v, want CredentialSharedError", err)
			}
			if shared.Var != tc.wantVar {
				t.Errorf("Var = %q, want %q", shared.Var, tc.wantVar)
			}
			if shared.TrackerField != tc.wantTrackerField {
				t.Errorf("TrackerField = %q, want %q", shared.TrackerField, tc.wantTrackerField)
			}
			if shared.AgentField != tc.wantAgentField {
				t.Errorf("AgentField = %q, want %q", shared.AgentField, tc.wantAgentField)
			}
			// The refusal carries the variable *name*, which is not a secret,
			// and never the value, which is (SPEC §5.5, §5.8).
			if msg := shared.Error(); strings.Contains(msg, "value-of-") {
				t.Errorf("the refusal leaked a resolved secret: %s", msg)
			}
		})
	}
}

// The adapter's own credential keys are on the agent side of the comparison
// too: `token: $ANTHROPIC_API_KEY` is the tracker authenticating as the agent's
// own credential, which is one secret doing both jobs however it is spelled.
func TestLoadRefusesTheTrackerBorrowingTheAgentCredential(t *testing.T) {
	t.Setenv("SHARED_KEY", "sk-shared")
	_, err := Load(writeWorkflow(t, workflowWith(
		"    token: $SHARED_KEY\n",
		"    api_key: $SHARED_KEY\n")))

	var shared *CredentialSharedError
	if !errors.As(err, &shared) {
		t.Fatalf("Load = %v, want CredentialSharedError", err)
	}
	if shared.AgentField != "agent.provider.api_key" {
		t.Errorf("AgentField = %q, want agent.provider.api_key", shared.AgentField)
	}
}

// A malformed block still has to answer the credential question. Load never
// runs Structural — the assembly does (SPEC §5.7) — so a reference reader that
// gave up on an unrelated type error would report no credentials at all for a
// block that plainly has one, and the collision would load.
func TestSharedCredentialIsFoundInABlockStructuralWouldRefuse(t *testing.T) {
	t.Setenv("SHARED_TOKEN", "sekret")
	_, err := Load(writeWorkflow(t, workflowWith(
		"    token: $SHARED_TOKEN\n",
		"    binary: 17\n    env_passthrough: [SHARED_TOKEN]\n")))

	var shared *CredentialSharedError
	if !errors.As(err, &shared) {
		t.Fatalf("Load = %v, want CredentialSharedError despite the malformed binary key", err)
	}
}

// Three bypasses of the first cut of this check, each of which loaded a workflow
// whose agent process received the tracker credential. All three came from the
// same mistake: trusting an enumeration of the routes a secret can take.
func TestSharedCredentialSurvivesEveryRouteIntoTheChild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracker string
		agent   string
		wantVar string
	}{
		{
			// The provenance entry recorded only the *last* variable a value
			// interpolated, so appending anything to a reference hid it.
			name:    "an interpolated value naming more than one variable",
			tracker: "    token: $SHARED_TOKEN\n",
			agent:   "    env:\n      GH_TOKEN: $SHARED_TOKEN-$SUFFIX\n",
			wantVar: "SHARED_TOKEN",
		},
		{
			// §7.6 copies the allowlist into every child unconditionally
			// (harness.Environ), so the agent block need not mention it at all.
			name:    "a tracker credential sourced from the §7.6 allowlist",
			tracker: "    token: $TERM\n",
			agent:   "",
			wantVar: "TERM",
		},
		{
			// Not an environment variable at all: `model` reaches the child as
			// `--model <secret>`. Every value in an agent block becomes argv, child
			// environment, or a file the child reads — there is no inert key.
			name:    "an argv-bound field",
			tracker: "    token: $SHARED_TOKEN\n",
			agent:   "    model: $SHARED_TOKEN\n",
			wantVar: "SHARED_TOKEN",
		},
		{
			// The same, one level down: a settings *path* built from the tracker
			// credential is still that secret on the command line.
			name:    "a nested argv-bound field",
			tracker: "    token: $SHARED_TOKEN\n",
			agent:   "    add_dirs: [/tmp/$SHARED_TOKEN]\n",
			wantVar: "SHARED_TOKEN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHARED_TOKEN", "tracker-secret")
			t.Setenv("SUFFIX", "-v2")
			t.Setenv("TERM", "tracker-secret")

			_, err := Load(writeWorkflow(t, workflowWith(tc.tracker, tc.agent)))
			if !errors.Is(err, ErrCredentialShared) {
				t.Fatalf("Load = %v, want ErrCredentialShared", err)
			}
			var shared *CredentialSharedError
			if !errors.As(err, &shared) || shared.Var != tc.wantVar {
				t.Errorf("refused on %+v, want the collision to name %q", shared, tc.wantVar)
			}
		})
	}
}

// The §7.6 allowlist is the agent side's floor, so it holds with no agent
// provider block at all — and it is core's list, not an adapter's, so no kind
// can forget to declare it.
func TestEveryAllowlistedVariableIsTreatedAsReachingTheChild(t *testing.T) {
	for _, name := range core.EnvAllowlist {
		t.Run(name, func(t *testing.T) {
			// The file first: TMPDIR is on this list, and overwriting it before
			// t.TempDir() runs breaks the helper rather than the check.
			path := writeWorkflow(t, workflowWith("    token: $"+name+"\n", ""))
			t.Setenv(name, "tracker-secret")
			_, err := Load(path)
			if !errors.Is(err, ErrCredentialShared) {
				t.Fatalf("a tracker credential from $%s loaded: %v; §7.6 copies it into every child", name, err)
			}
		})
	}
}

// A value naming several variables records all of them, which is what the check
// above depends on. Asserted on provenance directly, because the shape is a
// property of §5.5 resolution rather than of the credential rule reading it.
func TestProvenanceRecordsEveryInterpolatedVariable(t *testing.T) {
	t.Setenv("FIRST", "a")
	t.Setenv("SECOND", "b")
	def, err := Load(writeWorkflow(t, workflowWith("    token: $FIRST-$SECOND-$FIRST\n", "")))
	if err != nil {
		t.Fatal(err)
	}
	origin := def.Provenance["tracker.provider.token"]
	if !slices.Equal(origin.EnvVars, []string{"FIRST", "SECOND"}) {
		t.Errorf("EnvVars = %v, want [FIRST SECOND] — every variable once, in order", origin.EnvVars)
	}
	if got := origin.EnvVarLabel(); got != "$FIRST, $SECOND" {
		t.Errorf("EnvVarLabel = %q, want both variables named", got)
	}
}
