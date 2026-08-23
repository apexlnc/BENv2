package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// agentFixtures gives every registered agent.kind a provider block its adapter
// accepts and one it refuses, so `config effective` is asserted against the
// real adapters. The completeness check in
// TestRunConfigEffectiveValidatesEveryRegisteredAgentKind is what makes a new
// runner kind land with that coverage rather than silently without it.
var agentFixtures = map[string]struct {
	valid, invalid, wantRefusal string
}{
	"claude-code": {
		valid: "    permission_mode: bypassPermissions\n",
		// One 's' short — the typo class the loader cannot see, because the
		// provider block is opaque to it (SPEC §5.2.5).
		invalid:     "    permission_mode: bypassPermisions\n",
		wantRefusal: "agent.provider.permission_mode",
	},
	"codex-exec": {
		valid:       "    sandbox_mode: workspace-write\n",
		invalid:     "    sandbox_mode: workspace-writ\n",
		wantRefusal: "agent.provider.sandbox_mode",
	},
}

// workflowWithAgent renders a valid WORKFLOW.md around one agent kind and
// provider block.
func workflowWithAgent(kind, provider string) string {
	return fmt.Sprintf(`---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: %s
  provider:
%sdeployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`, kind, provider)
}

// validWorkflow is a config every registered adapter accepts. Its
// agent.provider block is not decoration: `config effective` runs the runner
// kind's Structural too (#55), and claude-code requires a stated permission
// posture (SPEC §7.7).
var validWorkflow = workflowWithAgent("claude-code", agentFixtures["claude-code"].valid)

// writeWorkflow drops a valid WORKFLOW.md into a temp dir and returns its
// absolute path (run has no cwd of its own to resolve against).
func writeWorkflow(t *testing.T) string {
	t.Helper()
	return writeWorkflowContent(t, validWorkflow)
}

func writeWorkflowContent(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunArgumentsAndExitCodes(t *testing.T) {
	valid := writeWorkflow(t)
	missing := filepath.Join(t.TempDir(), "WORKFLOW.md")

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // required substring of stdout ("" = don't care)
		wantErr  string // required substring of stderr ("" = don't care)
	}{
		{"no arguments", nil, 2, "", "Usage:"},
		{"unknown command", []string{"frobnicate"}, 2, "", `unknown command "frobnicate"`},
		{"help goes to stdout", []string{"help"}, 0, "Usage:", ""},
		{"-h goes to stdout", []string{"-h"}, 0, "Usage:", ""},
		{"bare config", []string{"config"}, 2, "", "ben config effective"},
		{"unknown config subcommand", []string{"config", "defective"}, 2, "", "ben config effective"},
		// `run` reaches the loader now, so its refusal is the workflow's. It
		// cannot get further here: a real run needs a config that loads and
		// adapters that are Ready, which is what daemon_test.go drives.
		{"run with no workflow", []string{"run"}, 1, "", "refusing to start"},
		{"run takes at most one path", []string{"run", "a", "b"}, 2, "", "at most one path"},
		// No state dir for this path is an *answer*, not a failure: a daemon
		// writes the directory before it does anything else, so its absence is
		// positive evidence that none has run here.
		{"status with no state", []string{"status"}, 0, "No BEN state", ""},
		{"status takes at most one path", []string{"status", "a", "b"}, 2, "", "at most one path"},
		{"status flag after path hints placement", []string{"status", "a", "--json"}, 2, "", "flags must come before the path"},
		{"status unknown flag", []string{"status", "--jsn"}, 2, "", "flag provided but not defined"},
		{"effective with path", []string{"config", "effective", valid}, 0, "workflow_key:", ""},
		{"effective flag before path", []string{"config", "effective", "--json", valid}, 0, `"workflow_key"`, ""},
		{"effective flag after path", []string{"config", "effective", valid, "--json"}, 2, "", "at most one path"},
		{"effective flag after path hints placement", []string{"config", "effective", valid, "--json"}, 2, "", "flags must come before the path"},
		{"effective two paths", []string{"config", "effective", valid, valid}, 2, "", "at most one path"},
		{"effective unknown flag", []string{"config", "effective", "--jsn", valid}, 2, "", "flag provided but not defined"},
		{"effective flag -h", []string{"config", "effective", "-h"}, 0, "", "-json"},
		{"effective load failure", []string{"config", "effective", missing}, 1, "", "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.wantCode, stderr.String())
			}
			if tc.wantOut != "" && !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout %q missing %q", stdout.String(), tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr %q missing %q", stderr.String(), tc.wantErr)
			}
		})
	}
}

// `config effective` calls Structural only, never New or Ready (SPEC §5.8):
// a workflow whose token is omitted must validate with no credentials in the
// environment. This is exactly what `make workflow-check` relies on in CI.
func TestRunConfigEffectiveIsCredentialFree(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "effective", writeWorkflow(t)}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
}

// The provider block is opaque to the loader, so the adapter's Structural
// check is the only place a typo inside it can be refused (SPEC §5.7, §5.8).
func TestRunConfigEffectiveSurfacesAdapterRefusals(t *testing.T) {
	bad := strings.Replace(validWorkflow, "repo: acme/widgets", "repo: acme/widgets\n    tokn: oops", 1)
	path := writeWorkflowContent(t, bad)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown key in tracker.provider") {
		t.Errorf("stderr %q does not carry the adapter's named refusal", stderr.String())
	}
}

// claim_assignee is adapter-owned but not a credential: Structural validates
// it, while effective output preserves the loader-resolved spelling an operator
// wrote. Lowercasing belongs only to the ready identity publication, where it
// cannot rewrite the configuration being inspected (SPEC §5.8, §8.4).
func TestRunConfigEffectivePreservesAndValidatesClaimAssignee(t *testing.T) {
	configured := strings.Replace(validWorkflow, "repo: acme/widgets",
		"repo: acme/widgets\n    claim_assignee: Ben-Bot", 1)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "effective", writeWorkflowContent(t, configured)}, &stdout, &stderr); code != 0 {
		t.Fatalf("configured claim_assignee: exit code = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim_assignee: Ben-Bot") {
		t.Errorf("effective output changed or hid the configured spelling:\n%s", stdout.String())
	}

	blank := strings.Replace(validWorkflow, "repo: acme/widgets",
		"repo: acme/widgets\n    claim_assignee: \"   \"", 1)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "effective", writeWorkflowContent(t, blank)}, &stdout, &stderr); code != 1 {
		t.Fatalf("blank claim_assignee: exit code = %d, want 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "claim_assignee must not be blank") {
		t.Errorf("blank refusal = %q, want the named claim_assignee error", stderr.String())
	}
}

// `config effective` must run the *runner* kind's Structural over
// agent.provider, not the tracker's alone: validating half the config and
// printing it as effective is how a typo'd permission_mode reached a green
// `make workflow-check` (#55). Every registered kind, accepted and refused.
func TestRunConfigEffectiveValidatesEveryRegisteredAgentKind(t *testing.T) {
	fixtured := slices.Sorted(maps.Keys(agentFixtures))
	if registered := registry.RunnerNames(); !slices.Equal(fixtured, registered) {
		t.Fatalf("agentFixtures covers %v but the registry registers %v — a new runner kind needs a fixture here", fixtured, registered)
	}

	for kind, fx := range agentFixtures {
		t.Run(kind+"/valid", func(t *testing.T) {
			path := writeWorkflowContent(t, workflowWithAgent(kind, fx.valid))
			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
			}
		})
		t.Run(kind+"/invalid", func(t *testing.T) {
			path := writeWorkflowContent(t, workflowWithAgent(kind, fx.invalid))
			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), fx.wantRefusal) {
				t.Errorf("stderr %q does not name %s", stderr.String(), fx.wantRefusal)
			}
			// A refused config must not also be printed as effective: the
			// operator would read a valid-looking dump of a config that is not.
			if stdout.Len() != 0 {
				t.Errorf("stdout is not empty on a refusal: %q", stdout.String())
			}
		})
	}
}

// AGENTS.md presents `make workflow-check` as the guarantee the dogfood
// WORKFLOW.md cannot rot, and workflow-check is exactly `config effective` over
// that file. So assert the real file, both directions: as committed it validates
// credential-free, and a typo inside the opaque agent.provider block is refused
// — the case that used to pass (#55).
func TestWorkflowCheckValidatesTheDogfoodWorkflow(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "") // credential-free, as in CI
	path := filepath.Join(moduleRoot(t), "WORKFLOW.md")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("the committed WORKFLOW.md does not validate: exit code = %d, stderr: %s", code, stderr.String())
	}

	dogfood, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Fail loudly rather than assert nothing: a dogfood config that no longer
	// spells this posture needs a new mutation, not a silently passing test.
	const posture = "permission_mode: bypassPermissions"
	if !strings.Contains(string(dogfood), posture) {
		t.Fatalf("WORKFLOW.md no longer contains %q — point this test at the posture the dogfood agent block now states", posture)
	}
	typoed := strings.Replace(string(dogfood), posture, "permission_mode: bypassPermisions", 1)

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "effective", writeWorkflowContent(t, typoed)}, &stdout, &stderr); code != 1 {
		t.Fatalf("a typo'd permission_mode passed: exit code = %d, want 1 (stdout: %s)", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "agent.provider.permission_mode") {
		t.Errorf("stderr %q does not name agent.provider.permission_mode", stderr.String())
	}
}

// #94's evidence-derived attempt floor introduces the third prompt state:
// numbered attempt 2, but no previous run outcome in this fresh record. BEN's
// own workflow must render that state as an inspection instruction rather than
// entering the failure branch and trying to emit null.
func TestDogfoodWorkflowRendersAnUnknownOutcomeAttemptFloor(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "WORKFLOW.md")
	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}

	prompt, err := def.RenderPrompt(template.Vars{
		Issue: core.Issue{
			Identifier: "94",
			Title:      "attempt floor",
			Body:       "derive it from git evidence",
		},
		Attempt:   2,
		Workspace: "/tmp/ben-94",
		Run:       template.Run{ID: "94-2"},
	})
	if err != nil {
		t.Fatalf("rendering the evidence-floored dogfood prompt: %v", err)
	}
	normalized := strings.Join(strings.Fields(prompt), " ")
	if !strings.Contains(normalized, "previous run outcome did not survive the claim boundary") {
		t.Errorf("prompt does not explain the evidence-floored unknown outcome:\n%s", prompt)
	}
	if strings.Contains(prompt, "Your previous session failed") {
		t.Errorf("prompt invented a failure for an unknown prior outcome:\n%s", prompt)
	}
}

// #150: the signed contract changes the canonical instruction itself. Each
// committed source is named here so omission cannot turn into a partial schema,
// and the independent workflow walk below catches a newly shipped config that
// the closed table does not know yet. Exact lines matter: a bare push, another
// remote/ref, or --force would all avoid -u while violating the contract.
func TestCanonicalPublishSources(t *testing.T) {
	root := moduleRoot(t)
	for _, tc := range []struct {
		path string
		want string
	}{
		{"SPEC.md", "2. Push it: `git push origin HEAD`."},
		{"WORKFLOW.md", "2. Push it: `git push origin HEAD`."},
		{"scripts/smoke-workflow.md", "2. Push it: `git push origin HEAD`."},
		{"internal/template/template_test.go", "2. Push it: ` + \"`git push origin HEAD`\" + `."},
	} {
		t.Run(filepath.ToSlash(tc.path), func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.path)))
			if err != nil {
				t.Fatal(err)
			}
			got := publishInstructionLines(string(body))
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("canonical publish lines = %q, want [%q]", got, tc.want)
			}
		})
	}

	files := workflowMarkdownFiles(t, root)
	// Fail loudly rather than pass over an empty set: a walk that stopped
	// finding workflow files would otherwise report this invariant as held.
	if len(files) < 2 {
		t.Fatalf("found %d workflow files (%v), want at least the dogfood WORKFLOW.md and "+
			"scripts/smoke-workflow.md — the walk no longer recognizes what it is looking for",
			len(files), files)
	}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := publishInstructionLines(string(body))
		if len(got) != 1 || got[0] != "2. Push it: `git push origin HEAD`." {
			t.Errorf("%s canonical publish lines = %q, want the exact explicit origin/HEAD command",
				path, got)
		}
	}
}

func publishInstructionLines(body string) []string {
	var found []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "2. Push it:") {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// workflowMarkdownFiles finds every workflow config this repository ships: a
// markdown file whose YAML front matter declares a tracker (SPEC §5.2).
//
// Recognized by shape, not by name, so a real workflow is covered wherever it
// is added. testdata is skipped for the reason the go tool skips it: those files
// exist to be malformed.
func workflowMarkdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		front, _, ok := strings.Cut(strings.TrimPrefix(string(body), "---\n"), "\n---")
		if !ok || !strings.HasPrefix(string(body), "---\n") {
			return nil
		}
		for _, line := range strings.Split(front, "\n") {
			if strings.HasPrefix(line, "tracker:") {
				found = append(found, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// moduleRoot walks up from the test's working directory to the go.mod, so the
// repo's own WORKFLOW.md is found by relation to the module rather than by a
// path relative to wherever the test was invoked.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

// secretShapes are the value shapes that defeated scrub-by-replacement in the
// PR #34 review, kept in one place because every adapter family's Structural
// path owes the same guarantee.
var secretShapes = []struct {
	name      string
	secret    string
	fragments []string // none of these may appear on stderr
}{
	{"plain", "supersecret-not-a-value", []string{"supersecret-not-a-value"}},
	// Round 2's repro: quotes, backslashes, and newlines diverge under %q.
	{"escaped", `supersecret\line` + "\nsecond-line", []string{"supersecret", "second-line"}},
	// 'o' occurs throughout the refusal text; replacement-based redaction
	// corrupted the message instead of hiding the secret.
	{"short", "o", nil},
}

// P1 regressions (PR #34 review, both rounds): an env-resolved provider
// value is a secret (SPEC §5.8) and must not reach CI logs through the
// Structural error path — not raw, not %q-escaped, and a short one must not
// corrupt the named refusal around it.
func TestRunConfigEffectiveRedactsSecretsInAdapterRefusals(t *testing.T) {
	for _, tc := range secretShapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEN_TEST_REPO", tc.secret)
			bad := strings.Replace(validWorkflow, "repo: acme/widgets", "repo: $BEN_TEST_REPO", 1)
			path := writeWorkflowContent(t, bad)

			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
			}
			for _, fragment := range tc.fragments {
				if strings.Contains(stderr.String(), fragment) {
					t.Fatalf("stderr leaked %q: %q", fragment, stderr.String())
				}
			}
			// The refusal arrives intact, with the marker in the value's
			// place — asserted as one phrase so substring corruption of the
			// message cannot pass.
			want := "tracker.provider.repo must be owner/name: got [redacted $BEN_TEST_REPO]"
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr %q missing the intact refusal %q", stderr.String(), want)
			}
		})
	}
}

// The operator renderer is where this guarantee is actually owed, and provenance
// alone does not deliver it: `api_url` is an address the output deliberately
// prints in full (github Kind.SensitiveFields), and a URL's authority is
// `[userinfo@]host[:port]` — so a credential written as a *file literal* passed
// every provenance check on its way to stderr (PR #115 review, P1).
//
// Both refusals, because they are one keystroke apart: dropping the `//` moves
// the same password from the endpoint check to the parse check, and a fix that
// covered only the one that was reported would leave the other printing it.
func TestRunConfigEffectiveNeverPrintsAnAPIURLCredential(t *testing.T) {
	const password = "hunter2-not-a-value"
	for _, tc := range []struct {
		name, apiURL, want string
	}{
		{
			name:   "endpoint refusal",
			apiURL: "https://ben:" + password + "@ghe.example.com/",
			want:   "tracker.provider.api_url must name only an endpoint: no userinfo, query, or fragment: it carries userinfo: got [redacted]",
		},
		{
			// No `//`, so there is no authority to find: the value cannot be
			// shown to be credential-free either, and is not shown.
			name:   "invalid-url refusal",
			apiURL: "ben:" + password + "@ghe.example.com",
			want:   "tracker.provider.api_url is not a valid URL: it names no host: got [redacted]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Replace(validWorkflow, "repo: acme/widgets", "repo: acme/widgets\n    api_url: "+tc.apiURL, 1)
			path := writeWorkflowContent(t, bad)

			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
			}
			if strings.Contains(stderr.String(), password) {
				t.Fatalf("stderr leaked the credential: %q", stderr.String())
			}
			// One phrase: the refusal must stay diagnosable, naming the field and
			// what was wrong with it, while carrying no part of the value.
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr %q missing the intact refusal %q", stderr.String(), tc.want)
			}
		})
	}
}

// The same guarantee for `agent.provider`, which this PR made reachable: the
// runner kinds' Structural refusals were the one adapter family whose values
// travelled as *text*, so `permission_mode: $SECRET` printed the resolved secret
// verbatim (PR #71 review, P1).
//
// Redaction naming the variable is also the end-to-end proof that each refusal's
// field path matches the loader's provenance key: a mis-spelled path still
// redacts, but as a bare marker, because the renderer found no origin.
func TestRunConfigEffectiveRedactsSecretsInRunnerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, kind, provider, want string
	}{
		{
			name:     "permission_mode",
			kind:     "claude-code",
			provider: "    permission_mode: $BEN_TEST_SECRET\n",
			want:     "agent.provider.permission_mode unusable: unknown mode (one of acceptEdits, auto, bypassPermissions, dontAsk): got [redacted $BEN_TEST_SECRET]",
		},
		{
			name:     "sandbox_mode",
			kind:     "codex-exec",
			provider: "    sandbox_mode: $BEN_TEST_SECRET\n",
			want:     "agent.provider.sandbox_mode unusable: unknown mode (one of workspace-write, danger-full-access): got [redacted $BEN_TEST_SECRET]",
		},
		{
			// A list entry: its provenance is indexed, so the refusal has to be
			// anchored at the entry rather than at the list.
			name:     "add_dirs entry",
			kind:     "codex-exec",
			provider: "    sandbox_mode: workspace-write\n    add_dirs:\n      - $BEN_TEST_SECRET\n",
			want:     "(a relative writable root would resolve against the agent's own workspace): got [redacted $BEN_TEST_SECRET]",
		},
	} {
		for _, shape := range secretShapes {
			t.Run(tc.name+"/"+shape.name, func(t *testing.T) {
				t.Setenv("BEN_TEST_SECRET", shape.secret)
				path := writeWorkflowContent(t, workflowWithAgent(tc.kind, tc.provider))

				var stdout, stderr bytes.Buffer
				if code := run([]string{"config", "effective", path}, &stdout, &stderr); code != 1 {
					t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
				}
				for _, fragment := range shape.fragments {
					if strings.Contains(stderr.String(), fragment) {
						t.Fatalf("stderr leaked %q: %q", fragment, stderr.String())
					}
				}
				// One phrase, so a corrupted message cannot pass.
				if !strings.Contains(stderr.String(), tc.want) {
					t.Errorf("stderr %q missing the intact refusal %q", stderr.String(), tc.want)
				}
			})
		}
	}
}

func TestRunConfigEffectiveJSONIsValid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "effective", "--json", writeWorkflow(t)}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--json output is not valid JSON: %v", err)
	}
	for _, key := range []string{"workflow", "workflow_key", "config", "provenance"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON output missing top-level %q", key)
		}
	}
}

// The refusal reaches `ben config effective`, not only `ben run`. That is the
// point of failing Load: a command whose whole job is saying what is in force
// must not print a configuration that cannot start (SPEC §5.2.9).
func TestConfigEffectiveRefusesAWorkflowWithNoDeploymentDeclaration(t *testing.T) {
	// Written rather than loaded: the harness's def() loads too, and this is a
	// file the loader is supposed to refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n"+
		"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n"+
		"---\nWork {{ issue.identifier }}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errs bytes.Buffer
	if code := run([]string{"config", "effective", path}, &out, &errs); code != 1 {
		t.Fatalf("exit %d, want 1; stdout:\n%s", code, out.String())
	}
	for _, want := range []string{"deployment.mode", "protected, risk-accepted, attended"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("refusal does not mention %q: %s", want, errs.String())
		}
	}
	if out.Len() != 0 {
		t.Errorf("printed a configuration it had just refused:\n%s", out.String())
	}
}
