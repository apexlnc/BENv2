package reviewctl_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #11's deployment was a GitHub Actions workflow; #204 replaced it with daemon
// reconciliation. This file asserts the replacement is a *removal* rather than
// an addition beside it.
//
// It is held here, in the package that owns the controller, for the reason
// `cmd/benreview` held the workflow's security posture before it: the file was
// the deployment, so everything the code refuses is worth nothing if a job
// running it still exists holding a token that can do it anyway. The same
// sentence with the file deleted is this test.
//
// The failure it exists to catch is not "somebody re-added a workflow on
// purpose". It is the half-migration — a retired path left installable, or a
// runbook still carrying the install step — because an obsolete deployment
// nobody removed is one an operator can still follow, and following it would
// put a repository credential back beside the model.

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
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
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}

// The reviewer's shipped Actions assets are gone from the repository.
//
// Driven by the paths #11 created rather than by a scan for "a workflow", so a
// reintroduction under the old names — the likely shape, since that is what
// every retired instruction still names — cannot pass.
func TestTheReviewerActionsAssetsAreGone(t *testing.T) {
	root := moduleRoot(t)
	for _, path := range []string{
		".github/reviewer",
		".github/reviewer/ben-review.yml",
		".github/reviewer/run.sh",
		".github/reviewer/PROMPT.md",
		".github/workflows/ben-review.yml",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("%s still exists; #204 retired the Actions review deployment, and a path left in place "+
				"is one an operator can still install", path)
		}
	}
}

// No installed workflow runs the review controller.
//
// The independent half of the check above, anchored at the directory that
// decides what actually runs rather than at the five names: a workflow called
// anything at all that invokes this controller is the thing #204 removed, and a
// test reading only those names could not see it arrive as `review.yml`.
func TestNoInstalledWorkflowRunsTheController(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	// The controller's own names. `internal/review` is deliberately absent: it
	// is the reducer, and `make check` compiles it in CI like every other
	// package.
	invocations := []string{"benreview", "reviewctl", "reviewrun", "ben-review"}

	var checked int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		checked++
		body := strings.ToLower(read(t, filepath.Join(dir, name)))
		for _, needle := range invocations {
			if strings.Contains(body, needle) {
				t.Errorf(".github/workflows/%s references %q; reviewer execution belongs to the daemon's "+
					"process backend, never to a GitHub runner holding a repository credential", name, needle)
			}
		}
	}
	// The scan is the whole test; an empty directory would pass it silently.
	if checked == 0 {
		t.Fatal(".github/workflows contains no workflow files, so the scan proved nothing")
	}
}

// installStep matches an instruction to put a workflow under `.github/workflows/`.
//
// The path with a trailing segment, not the bare directory: `ci.yml` and the
// image-publish workflow are legitimately described by name in the runbooks,
// and this is meant to catch the *review* deployment's step 0 returning in
// prose after the file itself is gone.
var installStep = regexp.MustCompile(`\.github/workflows/ben-review`)

// No document instructs an operator to install a review workflow.
//
// The docs half of the retirement, and the half that outlives the code change:
// the files can be gone from the tree while `docs/REVIEW.md` still opens with
// "copy this into `.github/workflows/`", and an operator following a runbook
// does not check whether the file it names still exists.
func TestNoRunbookCarriesTheWorkflowInstallStep(t *testing.T) {
	root := moduleRoot(t)

	var scanned int
	for _, dir := range []string{".", "docs", "scripts"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(dir, e.Name()))
			scanned++
			body := read(t, filepath.Join(root, dir, e.Name()))
			if loc := installStep.FindStringIndex(body); loc != nil {
				t.Errorf("%s carries a review-workflow installation step (%q); #204's deployment is a "+
					"`review:` section and a daemon, and a retired install step is one somebody follows",
					rel, body[loc[0]:loc[1]])
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no markdown files were scanned, so the audit proved nothing")
	}
}

// docs/REVIEW.md describes the deployment #204 actually has.
//
// The positive half. The three assertions above can all pass against a document
// that says nothing at all about how review runs, which is the state a
// deletion-only migration leaves — and a runbook with the old step removed and
// no new one is worse than the old step, because it strands the operator
// without telling them where to go.
func TestTheReviewRunbookDescribesDaemonReconciliation(t *testing.T) {
	body := read(t, filepath.Join(moduleRoot(t), "docs", "REVIEW.md"))

	for _, tc := range []struct {
		marker string
		why    string
	}{
		{"review:", "the configuration section that is now the whole deployment"},
		{"enabled: true", "the gate an operator sets, since omission cannot arrive at one"},
		{"auth_source", "how the controller's credential is named, indirectly"},
		{"reviewer_argv", "the operator's choice of model harness"},
		{"--json", "the Codex JSONL mode the verdict reader unwraps"},
		{"--skip-git-repo-check", "the flag required in the local reviewer's fresh non-git directory"},
		{"benreview", "the operator's dry-run and reconcile command"},
		{"-dry-run", "and the flag that reaches a real decision performing none of it"},
		{"sweep", "the daemon lifecycle that is the availability mechanism"},
		{"workspace cycle", "the identity that owns the sandbox across claim epochs"},
		{"claim epoch", "the identity that scopes verification and must not be collapsed into it"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			if !strings.Contains(body, tc.marker) {
				t.Errorf("docs/REVIEW.md does not mention %q — %s", tc.marker, tc.why)
			}
		})
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}
