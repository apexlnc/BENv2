package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
)

// scripts/smoke.sh must never run against BEN's own repository: the dogfood
// workflow already dispatches against it, and a smoke run there files throwaway
// issues into the real queue and races the daemon for the same claims.
//
// The refusal that says so had three ways of not happening, none of which any
// green pipeline could show, because the §12.4 profile needs credentials and a
// network and is deliberately outside `make check` (#244). It was skipped whole
// if WORKFLOW.md was missing or renamed; its `sed` printed nothing for a quoted
// or comment-suffixed value, and an empty `own_repo` compared equal to nothing;
// and it compared bytes, while GitHub repository names are case-insensitive, so
// a differently-spelled name for the same repository sailed through.
//
// The rule is exercised through the script's own self-check mode over the real
// `ben config effective` rendering — the rendering produced in-process here, so
// `make check` gains a real renderer and a real script without a `go build`
// inside a test.

const smokeScript = "scripts/smoke.sh"

// effectiveWorkflow renders BEN's own WORKFLOW.md exactly as `ben config
// effective` prints it, and returns the repository the guard must find in it —
// read from the parsed config rather than from the rendering, so the two cannot
// agree by sharing a mistake.
func effectiveWorkflow(t *testing.T) (rendered, repo string) {
	t.Helper()
	def, err := config.Load(filepath.Join(moduleRoot(t), "WORKFLOW.md"))
	if err != nil {
		t.Fatalf("loading BEN's own WORKFLOW.md: %v", err)
	}
	repo, ok := def.Config.Tracker.Provider["repo"].(string)
	if !ok || repo == "" {
		t.Fatalf("WORKFLOW.md declares no tracker.provider.repo: %#v", def.Config.Tracker.Provider)
	}
	return config.EffectiveText(def), repo
}

func TestTheSmokeRepoGuardRefusesBENsOwnRepository(t *testing.T) {
	rendered, own := effectiveWorkflow(t)
	dir := t.TempDir()

	full := filepath.Join(dir, "effective.txt")
	writeFile(t, full, rendered)

	// The same rendering with the repository's line taken out: what the old
	// `sed` produced from a value it could not match, and the case where the
	// guard has nothing to compare against.
	var kept []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "repo:") {
			continue
		}
		kept = append(kept, line)
	}
	unparsed := filepath.Join(dir, "no-repo.txt")
	writeFile(t, unparsed, strings.Join(kept, "\n"))

	// Two answers is not one answer. A rendering that grew a second
	// tracker.provider.repo is one this rule can no longer read.
	doubled := filepath.Join(dir, "two-repos.txt")
	writeFile(t, doubled, strings.TrimRight(rendered, "\n")+"\ntracker:\n  provider:\n    repo: someone/else\n")

	cases := []struct {
		desc, candidate, rendering string
		wantAllowed                bool
		wantSays                   string
	}{
		{
			desc:        "a distinct canary is allowed",
			candidate:   own + "-canary",
			rendering:   full,
			wantAllowed: true,
		},
		{
			desc:      "BEN's own repository, exactly",
			candidate: own,
			rendering: full,
			wantSays:  "own repository",
		},
		{
			// GitHub resolves this to the same repository. A byte comparison
			// does not.
			desc:      "BEN's own repository, differently spelled",
			candidate: strings.ToUpper(own),
			rendering: full,
			wantSays:  "case-insensitive",
		},
		{
			desc:      "a rendering with no repo to read",
			candidate: own + "-canary",
			rendering: unparsed,
			wantSays:  "want exactly one",
		},
		{
			desc:      "a rendering with two repos to read",
			candidate: own + "-canary",
			rendering: doubled,
			wantSays:  "want exactly one",
		},
		{
			// The missing-WORKFLOW.md case, at the boundary the guard owns:
			// no rendering, so no refusal can be made, so the run does not
			// proceed.
			desc:      "a rendering that cannot be read",
			candidate: own + "-canary",
			rendering: filepath.Join(dir, "absent.txt"),
			wantSays:  "cannot be read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			out, code := runRepoScript(t, smokeScript, "--check-repo-guard", tc.candidate, tc.rendering)
			if tc.wantAllowed {
				if code != 0 {
					t.Fatalf("exit %d, want the canary allowed\n%s", code, out)
				}
				return
			}
			if code == 0 {
				t.Fatalf("exit 0, want a refusal\n%s", out)
			}
			if !strings.Contains(out, tc.wantSays) {
				t.Errorf("refusal = %q, want it to say %q", out, tc.wantSays)
			}
		})
	}
}

// The self-check above proves the rule. This source audit pins the loader and
// its live call; the executed missing-workflow case below is the independent
// proof that the call is unconditional.
func TestTheSmokeRepoGuardIsOnTheLivePath(t *testing.T) {
	script := read(t, filepath.Join(moduleRoot(t), filepath.FromSlash(smokeScript)))

	if !strings.Contains(script, "\nrefuse_own_repository \"$BEN_SMOKE_REPO\"") {
		t.Errorf("%s does not call refuse_own_repository on BEN_SMOKE_REPO at the top level; a guard reached"+
			" only on some paths is the defect #244 found", smokeScript)
	}
	// The loader over BEN's *own* workflow specifically. The profile already ran
	// `config effective` on the smoke workflow long before #244, so the bare
	// phrase would have passed against the sed this replaced.
	if !strings.Contains(script, `config effective "$own_workflow"`) {
		t.Errorf("%s no longer reads its own repository through BEN's loader; a shell parse of WORKFLOW.md is"+
			" what missed a quoted value and left the guard comparing against nothing", smokeScript)
	}
	if strings.Contains(script, "sed -n 's/^ *repo:") {
		t.Errorf("%s parses repo: with sed again", smokeScript)
	}
}

// Run the normal preflight from a checkout with no WORKFLOW.md. This is the
// exact branch #244 found fail-open. The command fakes are reached only for
// presence checks and the credential-free go env audit; if the old
// `if [ -f WORKFLOW.md ]` guard returns, execution reaches fake gh and the
// required missing-workflow refusal disappears.
func TestTheSmokeRepoGuardRefusesMissingWorkflowOnTheLivePath(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "smoke.sh")
	writeFile(t, script, read(t, filepath.Join(moduleRoot(t), filepath.FromSlash(smokeScript))))

	fakeBin := filepath.Join(root, "bin")
	for _, name := range []string{"gh", "git", "claude", "srt"} {
		path := filepath.Join(fakeBin, name)
		writeFile(t, path, "#!/bin/sh\nexit 97\n")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakeGo := filepath.Join(fakeBin, "go")
	writeFile(t, fakeGo, "#!/bin/sh\nif [ \"${1-}\" = env ] && [ \"${2-}\" = GO111MODULE ]; then exit 0; fi\nexit 97\n")
	if err := os.Chmod(fakeGo, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/bash", script)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"HOME=" + root,
		"BEN_SMOKE_REPO=acme/canary",
		"GITHUB_TOKEN=tracker-fixture",
		"GH_TOKEN=publisher-fixture",
		"ANTHROPIC_API_KEY=harness-fixture",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("live smoke preflight exited 0 without WORKFLOW.md\n%s", out)
	}
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("running live smoke preflight: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "WORKFLOW.md is missing") {
		t.Errorf("live refusal = %q, want the missing WORKFLOW.md refusal", out)
	}
}
