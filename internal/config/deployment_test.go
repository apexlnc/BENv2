package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// SPEC §5.2.9 and §10.1: the declaration BEN refuses to run without.
//
// The point of every case here is that the refusal is a *load* refusal. §10.1
// says a deployment MUST NOT arrive in an unattended mode "by default or by
// omission", and a refusal at `ben run` alone would leave `ben config effective`
// printing a configuration that cannot start — the one command whose whole job
// is saying what is in force.

func deploymentWorkflow(block string) string {
	return "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
		"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
		block +
		"---\nDo the work described in {{ issue.title }}.\n"
}

func TestDeploymentDeclarationIsRequiredAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		field string
		// want is a fragment of the message an operator has to act on.
		want string
	}{
		{
			name:  "omitted entirely",
			block: "",
			field: "deployment.mode",
			want:  "protected, risk-accepted, attended",
		},
		{
			// A written block with no mode is as unstated as no block at all, and
			// the message must be the same one: an operator who wrote `deployment:`
			// and stopped has made the same omission.
			name:  "written with no mode",
			block: "deployment:\n  accepted_because: because\n",
			field: "deployment.mode",
			want:  "required",
		},
		{
			name:  "unknown mode",
			block: "deployment:\n  mode: yolo\n",
			field: "deployment.mode",
			want:  "the set is closed",
		},
		{
			// The closed set is closed in both directions: an empty string is not
			// "unset means safe".
			name:  "empty mode",
			block: "deployment:\n  mode: \"\"\n",
			field: "deployment.mode",
			want:  "required",
		},
		{
			name:  "risk-accepted with no reason",
			block: "deployment:\n  mode: risk-accepted\n",
			field: "deployment.accepted_because",
			want:  "non-blank",
		},
		{
			// Whitespace is not a record.
			name:  "risk-accepted with a blank reason",
			block: "deployment:\n  mode: risk-accepted\n  accepted_because: \"   \"\n",
			field: "deployment.accepted_because",
			want:  "non-blank",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeWorkflow(t, deploymentWorkflow(tc.block)))
			if err == nil {
				t.Fatal("Load accepted a workflow with no usable deployment declaration")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Load = %v, want a *ValidationError", err)
			}
			if ve.Field != tc.field {
				t.Errorf("field = %q, want %q", ve.Field, tc.field)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestDeploymentDeclarationAccepted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		block      string
		mode       DeploymentMode
		reason     string
		unattended bool
	}{
		{
			name:       "protected needs no reason",
			block:      "deployment:\n  mode: protected\n",
			mode:       DeploymentProtected,
			unattended: true,
		},
		{
			name:       "risk-accepted records one",
			block:      "deployment:\n  mode: risk-accepted\n  accepted_because: canary repo\n",
			mode:       DeploymentRiskAccepted,
			reason:     "canary repo",
			unattended: true,
		},
		{
			// §10.1's on-ramp exemption. Not unattended, so its requirements do
			// not govern — which is exactly why it is declared rather than sensed.
			name:  "attended is the exemption",
			block: "deployment:\n  mode: attended\n",
			mode:  DeploymentAttended,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Load(writeWorkflow(t, deploymentWorkflow(tc.block)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := def.Config.Deployment
			if got.Mode != tc.mode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.mode)
			}
			if got.AcceptedBecause != tc.reason {
				t.Errorf("accepted_because = %q, want %q", got.AcceptedBecause, tc.reason)
			}
			if got.Unattended() != tc.unattended {
				t.Errorf("Unattended = %v, want %v", got.Unattended(), tc.unattended)
			}
		})
	}
}

// The declaration is rendered, and `accepted_because` is not redacted: it is the
// record §10.1 asks for, and a record nobody can read is not a record.
func TestDeploymentIsRenderedAndNotRedacted(t *testing.T) {
	const reason = "single-tenant canary; PR review is the gate"
	def, err := Load(writeWorkflow(t, deploymentWorkflow(
		"deployment:\n  mode: risk-accepted\n  accepted_because: "+reason+"\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	text := EffectiveText(def)
	if !strings.Contains(text, "mode: risk-accepted") || !strings.Contains(text, reason) {
		t.Errorf("EffectiveText does not carry the declaration:\n%s", text)
	}
	if strings.Contains(text, Redacted) && !strings.Contains(text, reason) {
		t.Error("accepted_because was redacted; it is the record, not a secret")
	}

	raw, err := EffectiveJSON(def)
	if err != nil {
		t.Fatalf("EffectiveJSON: %v", err)
	}
	if !strings.Contains(string(raw), `"mode": "risk-accepted"`) || !strings.Contains(string(raw), reason) {
		t.Errorf("EffectiveJSON does not carry the declaration:\n%s", raw)
	}
}

// The closed set, anchored independently of the declaration.
//
// AGENTS.md's rule: a test driven by the thing it checks proves the declared
// entries behave, never that the declaration is complete. So this names all
// three literally — an entry deleted from deploymentModes fails here, and one
// added fails here too, because §10.1 has exactly three and a fourth is a SPEC
// change rather than a code change.
//
// The second anchor is the refusal *message*: it lists what an operator may
// write, and a set that drifted from the message would leave someone reading a
// list of values that no longer load.
func TestTheDeploymentModeSetIsExactlyThree(t *testing.T) {
	want := []DeploymentMode{"protected", "risk-accepted", "attended"}

	got := DeploymentModes()
	if len(got) != len(want) {
		t.Fatalf("the closed set has %d entries (%v), want exactly %d — §10.1 names three, and a "+
			"fourth is a SPEC amendment rather than a code change", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("entry %d = %q, want %q", i, got[i], w)
		}
	}

	// Every literal loads, read off the file rather than off the constant.
	for _, m := range want {
		block := "deployment:\n  mode: " + string(m) + "\n"
		if m == "risk-accepted" {
			block += "  accepted_because: because\n"
		}
		def, err := Load(writeWorkflow(t, deploymentWorkflow(block)))
		if err != nil {
			t.Errorf("mode %q is in the set and does not load: %v", m, err)
			continue
		}
		if def.Config.Deployment.Mode != m {
			t.Errorf("loaded mode = %q, want %q", def.Config.Deployment.Mode, m)
		}
	}

	// And the refusal names each one, so the set and the message cannot drift.
	_, err := Load(writeWorkflow(t, deploymentWorkflow("deployment:\n  mode: nope\n")))
	if err == nil {
		t.Fatal("an unknown mode loaded")
	}
	for _, m := range want {
		if !strings.Contains(err.Error(), string(m)) {
			t.Errorf("the refusal does not offer %q: %s", m, err)
		}
	}
}

// The returned set is a copy. An exported slice would have let any importer
// widen or empty §10.1's closed set at run time, from anywhere, with no compile
// error — which is what validation compares against.
func TestTheDeploymentModeSetCannotBeMutatedByACaller(t *testing.T) {
	DeploymentModes()[0] = "anything-goes"

	if got := DeploymentModes()[0]; got != DeploymentProtected {
		t.Fatalf("a caller rewrote the closed set: entry 0 is now %q", got)
	}
	// And validation still refuses what the caller tried to inject.
	if _, err := Load(writeWorkflow(t, deploymentWorkflow("deployment:\n  mode: anything-goes\n"))); err == nil {
		t.Error("a mode a caller wrote into the set was accepted")
	}
}

// Every validation refusal names the file it came from — including the ones
// raised *before* the main validation pass.
//
// `make workflow-check` validates this repo's own workflow and the §12.4 smoke
// profile in one run, and "invalid deployment.mode" identifies neither. The
// first version of this wrapped two call sites and missed `rejectExplicitNulls`,
// which is why the wrapper now sits at Load's boundary and why this table covers
// the early paths rather than the one that was reported.
func TestEveryValidationRefusalNamesTheWorkflowFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		field   string
	}{
		{
			// The reported case: an explicit null, refused before validate runs.
			name: "explicit null publish",
			content: "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
				"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
				"deployment:\n  mode: attended\npublish: null\n---\nWork {{ issue.identifier }}.\n",
			field: "publish",
		},
		{
			name: "explicit null credential sources",
			content: "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n    token: $GITHUB_TOKEN\n" +
				"credential_sources: null\nagent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
				"deployment:\n  mode: attended\n---\nWork {{ issue.identifier }}.\n",
			field: "credential_sources",
		},
		{
			name: "explicit null provider",
			content: "---\ntracker:\n  kind: github\n  provider: null\n" +
				"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
				"deployment:\n  mode: attended\n---\nWork {{ issue.identifier }}.\n",
			field: "tracker.provider",
		},
		{
			name: "explicit null workspace root",
			content: "---\ntracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
				"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
				"workspace:\n  root: null\ndeployment:\n  mode: attended\n---\nWork {{ issue.identifier }}.\n",
			field: "workspace.root",
		},
		{
			// And the late path, so the table spans both sides of the pass.
			name:    "the deployment declaration",
			content: deploymentWorkflow(""),
			field:   "deployment.mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeWorkflow(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted the fixture")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Load = %v, want a *ValidationError", err)
			}
			if ve.Field != tc.field {
				t.Errorf("field = %q, want %q", ve.Field, tc.field)
			}
			var we *WorkflowError
			if !errors.As(err, &we) {
				t.Fatalf("Load = %v, want it wrapped in a *WorkflowError", err)
			}
			if we.Path != path {
				t.Errorf("WorkflowError.Path = %q, want %q", we.Path, path)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("rendered refusal does not name the file:\n  %v", err)
			}
		})
	}
}

// Every refusal Load returns names the file — not only the ValidationError class.
//
// The first version of this claimed "every refusal" while wrapping only
// *ValidationError, so ErrEmptyPrompt and a version mismatch stayed anonymous
// and the comment overstated the code. This table is the claim, checked: it
// spans the file read, the front-matter split, the YAML decode, the version
// gate, the prompt body, the template pass and validation, because those are
// every stage that can refuse.
//
// Two of them name the file *themselves* and must not be double-named — asserted
// here too, since the wrapper skipping them is the half a mutation would remove.
func TestEveryLoadRefusalNamesTheWorkflowFileExactlyOnce(t *testing.T) {
	valid := "tracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
		"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
		"deployment:\n  mode: attended\n"

	for _, tc := range []struct {
		name string
		// content empty means "do not create the file at all".
		content string
		// wrapped says the refusal is named by Load's boundary rather than by
		// the error itself.
		wrapped bool
	}{
		{name: "front matter is not a map", content: "---\n- not a map\n---\nWork.\n", wrapped: true},
		{name: "yaml syntax", content: "---\ntracker: [\n---\nWork.\n", wrapped: true},
		{name: "unsupported version", content: "---\nversion: 99\n---\nWork.\n", wrapped: true},
		{name: "empty prompt", content: "---\n" + valid + "---\n", wrapped: true},
		{name: "validation", content: "---\n" + valid + "publish: null\n---\nWork.\n", wrapped: true},
		// Names itself, with a line number the wrapper could not add.
		{name: "template", content: "---\n" + valid + "---\nWork {{ issue.titel }}.\n"},
		// Names itself: the message *is* the path.
		{name: "missing file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted the fixture")
			}
			if n := strings.Count(err.Error(), path); n != 1 {
				t.Errorf("the refusal names the file %d times, want exactly 1:\n  %v", n, err)
			}

			var we *WorkflowError
			if got := errors.As(err, &we); got != tc.wrapped {
				t.Fatalf("wrapped in *WorkflowError = %v, want %v: %v", got, tc.wrapped, err)
			}
			if tc.wrapped && we.Path != path {
				t.Errorf("WorkflowError.Path = %q, want %q", we.Path, path)
			}
		})
	}
}

// Wrapping must not break the sentinels and typed errors callers match on —
// startupRefusal switches on several of them (cmd/ben), and the watcher's
// blocked state is read with errors.Is.
func TestWrappingPreservesUnwrapping(t *testing.T) {
	path := writeWorkflow(t, "---\nversion: 99\n---\nWork.\n")
	_, err := Load(path)

	var ue *UnsupportedVersionError
	if !errors.As(err, &ue) || ue.Version != 99 {
		t.Errorf("errors.As lost the typed error: %v", err)
	}

	path = writeWorkflow(t, deploymentWorkflow(""))
	_, err = Load(path)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "deployment.mode" {
		t.Errorf("errors.As lost the ValidationError: %v", err)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "WORKFLOW.md")); !errors.Is(err, ErrMissingWorkflowFile) {
		t.Errorf("errors.Is lost ErrMissingWorkflowFile: %v", err)
	}
}

// The full closed set of *located* template refusals, through Load, each naming
// the file exactly once.
//
// The reported bug: withPath skipped only UnknownVariableError, so its four
// siblings — which carry file and line just as it does — were double-named. The
// fix asks template.Located rather than listing types, and this is the table
// that would have caught the original.
func TestLocatedTemplateRefusalsAreNamedOnce(t *testing.T) {
	valid := "tracker:\n  kind: github\n  provider:\n    repo: acme/widgets\n" +
		"agent:\n  kind: claude-code\n  provider:\n    permission_mode: bypassPermissions\n" +
		"deployment:\n  mode: attended\n"

	for _, tc := range []struct {
		name   string
		prompt string
		// want is the template error the prompt provokes, so a prompt that
		// stopped provoking it fails here rather than passing vacuously.
		want func(error) bool
	}{
		{
			name:   "unknown variable",
			prompt: "Work {{ issue.titel }}.",
			want:   func(e error) bool { var x *template.UnknownVariableError; return errors.As(e, &x) },
		},
		{
			name: "unknown filter",
			// Filtered on a trusted leaf: an untrusted one is refused for being
			// reshaped before the filter name is ever looked up.
			prompt: "Work {{ workspace | shout }}.",
			want:   func(e error) bool { var x *template.UnknownFilterError; return errors.As(e, &x) },
		},
		{
			name: "reserved name",
			// forloop is the one engine-owned name a prompt may read and not bind.
			prompt: "{% assign forloop = 1 %}Work.",
			want:   func(e error) bool { var x *template.ReservedNameError; return errors.As(e, &x) },
		},
		{
			name:   "untrusted use",
			prompt: "Work {{ issue.title | upcase }}.",
			want:   func(e error) bool { var x *template.UntrustedUseError; return errors.As(e, &x) },
		},
		{
			name:   "unsupported tag",
			prompt: "{% include \"other\" %}Work.",
			want:   func(e error) bool { var x *template.UnsupportedTagError; return errors.As(e, &x) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeWorkflow(t, "---\n"+valid+"---\n"+tc.prompt+"\n")
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted the fixture")
			}
			if !tc.want(err) {
				t.Fatalf("the prompt no longer provokes the refusal this case is about: %v", err)
			}
			if n := strings.Count(err.Error(), path); n != 1 {
				t.Errorf("names the file %d times, want exactly 1:\n  %v", n, err)
			}
			// Named by the template error, which carries the line, not by the
			// loader's boundary — that is what the skip buys.
			var we *WorkflowError
			if errors.As(err, &we) {
				t.Errorf("wrapped in *WorkflowError, so the file is named twice or the line is lost: %v", err)
			}
			var located template.Located
			if !errors.As(err, &located) {
				t.Fatalf("not a located error: %v", err)
			}
			if file, line := located.Location(); file != path || line == 0 {
				t.Errorf("Location() = %q:%d, want %s with a real line", file, line, path)
			}
		})
	}
}

// fakeLocated stands in for a template error at the withPath seam.
//
// It **renders its location**, because every real Located error does — the AST
// anchor in internal/template holds each of them to it. An earlier version
// returned a constant string, and the test below then accepted an unwrapped
// error naming no file at all: the fake had quietly dropped the guarantee the
// real component makes, so the case that was supposed to prove the invariant
// proved nothing (AGENTS.md, on fakes that do not model what they stand in for).
type fakeLocated struct {
	file string
	line int
}

func (f *fakeLocated) Error() string {
	if f.file == "" || f.line <= 0 {
		// A type that claims a location and cannot give one has nothing to put
		// here either. That is exactly the shape withPath must not skip.
		return "refused, location unknown"
	}
	return fmt.Sprintf("refused at %s:%d", f.file, f.line)
}

func (f *fakeLocated) Location() (string, int) { return f.file, f.line }

// The invariant, at the one seam that decides it: **every refusal names the
// file**, whichever mechanism does the naming.
//
// A real location is left to name itself, because it carries the line too. A
// claimed location that cannot say where is wrapped anyway — skipping it would
// make it the only refusal naming no file at all, which is worse than the
// double-naming the skip prevents. So the assertion is the same for every row:
// the result names the path, exactly once.
func TestEveryRefusalNamesTheFileWhicheverMechanismNamesIt(t *testing.T) {
	const path = "/tmp/w/WORKFLOW.md"
	for _, tc := range []struct {
		name string
		err  error
		// wrapped says the boundary named it rather than the error itself.
		wrapped bool
	}{
		{"no file", &fakeLocated{line: 7}, true},
		{"no line", &fakeLocated{file: path}, true},
		{"neither", &fakeLocated{}, true},
		// The one row where the error names itself — and it must still name the
		// file, which is what the previous version of this test never checked.
		{"a usable location", &fakeLocated{file: path, line: 7}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withPath(tc.err, path)

			var we *WorkflowError
			if errors.As(got, &we) != tc.wrapped {
				t.Errorf("wrapped in *WorkflowError = %v, want %v", !tc.wrapped, tc.wrapped)
			}
			if n := strings.Count(got.Error(), path); n != 1 {
				t.Fatalf("names the file %d times, want exactly 1:\n  %v", n, got)
			}
		})
	}
}
