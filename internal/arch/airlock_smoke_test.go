package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// scripts/airlock-smoke.sh has one job: make a real Airlock run happen and
// report whether it did. Two exit-0 paths used to defeat it, and neither is
// visible in a green pipeline because the wrapper is not part of `make check`
// (#244):
//
//   - `go test -run TestAirlockSmoke` was a substring match. It kept matching a
//     renamed TestAirlockSmokeAgainstStaging, and matched *nothing* once the
//     test was renamed past that — and "no tests to run" exits 0.
//   - The test skips itself when a variable is empty. The wrapper checked three
//     variable names of its own, pinned to nothing, so a rename on either side
//     turned every run into a silent skip that also exits 0.
//
// The script's own rule is invoked here rather than restated in Go, for the
// reason worktree_test.go gives: a copy would prove the copy correct and leave
// the script free to drift.

const (
	airlockSmokeScript   = "scripts/airlock-smoke.sh"
	airlockSmokeTestFile = "internal/airlock/smoke_test.go"
)

var (
	scriptSmokeTest = regexp.MustCompile(`(?m)^smoke_test=([A-Za-z0-9_]+)\s*$`)
	scriptSmokeVars = regexp.MustCompile(`(?m)^smoke_vars=\(([^)]*)\)\s*$`)
	airlockEnvVar   = regexp.MustCompile(`"(BEN_AIRLOCK_[A-Z0-9_]+)"`)

	// The invocation, not the word: the script's own diagnostics talk about
	// `-run` patterns, so the first bare occurrence is not the command that runs.
	scriptRunFlag = regexp.MustCompile(`go test \./internal/airlock/[^\n]*?-run (\S+)`)
)

// runRepoScript invokes one of the repository's scripts through bash and returns
// its combined output and exit code. Both smoke wrappers expose their guard as a
// credential-free self-check mode, and this is how those modes are driven.
func runRepoScript(t *testing.T, script string, args ...string) (combined string, exitCode int) {
	t.Helper()
	root := moduleRoot(t)
	cmd := exec.Command("bash", append([]string{filepath.Join(root, filepath.FromSlash(script))}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("running %s %v: %v\n%s", script, args, err, out)
	}
	return string(out), exit.ExitCode()
}

func airlockSmokeTestName(t *testing.T) string {
	t.Helper()
	script := read(t, filepath.Join(moduleRoot(t), filepath.FromSlash(airlockSmokeScript)))
	m := scriptSmokeTest.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s declares no smoke_test=<name>; its -run pattern and its verdict are both read from that one line",
			airlockSmokeScript)
	}
	return m[1]
}

// The wrapper must name a test that exists, and must ask for it anchored.
// Unanchored is the defect: `-run X` matches every test whose name contains X
// and exits 0 when it matches none of them.
func TestTheAirlockSmokeWrapperNamesATestThatExists(t *testing.T) {
	root := moduleRoot(t)
	name := airlockSmokeTestName(t)

	suite := read(t, filepath.Join(root, filepath.FromSlash(airlockSmokeTestFile)))
	if !regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`).MatchString(suite) {
		t.Errorf("%s runs %s, which %s does not declare", airlockSmokeScript, name, airlockSmokeTestFile)
	}

	script := read(t, filepath.Join(root, filepath.FromSlash(airlockSmokeScript)))
	m := scriptRunFlag.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s passes no -run flag", airlockSmokeScript)
	}
	pattern := m[1]
	if !strings.HasPrefix(pattern, `"^`) || !strings.HasSuffix(pattern, `\$"`) {
		t.Errorf("%s runs %s, which is a substring match; anchor it as \"^${smoke_test}\\$\"",
			airlockSmokeScript, pattern)
	}
	if !strings.Contains(pattern, "${smoke_test}") {
		t.Errorf("%s runs %s rather than the smoke_test it declares, so the two can disagree",
			airlockSmokeScript, pattern)
	}
}

// The variables the wrapper refuses to run without must be exactly the ones the
// test decides to skip on. They are two independent lists in two languages, and
// nothing but this says so: with them out of step every invocation is a skip,
// which the wrapper used to report as success.
func TestTheAirlockSmokeWrapperRequiresTheVariablesTheTestReads(t *testing.T) {
	root := moduleRoot(t)
	script := read(t, filepath.Join(root, filepath.FromSlash(airlockSmokeScript)))

	m := scriptSmokeVars.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s declares no smoke_vars=(...)", airlockSmokeScript)
	}
	declared := strings.Fields(m[1])
	sort.Strings(declared)

	suite := read(t, filepath.Join(root, filepath.FromSlash(airlockSmokeTestFile)))
	seen := map[string]bool{}
	for _, v := range airlockEnvVar.FindAllStringSubmatch(suite, -1) {
		seen[v[1]] = true
	}
	wanted := make([]string, 0, len(seen))
	for v := range seen {
		wanted = append(wanted, v)
	}
	sort.Strings(wanted)

	if len(wanted) == 0 {
		t.Fatalf("%s names no BEN_AIRLOCK_* variable; the parse that reads them has broken", airlockSmokeTestFile)
	}
	if strings.Join(declared, " ") != strings.Join(wanted, " ") {
		t.Errorf("%s requires %v; %s reads %v. Out of step, every run skips and the wrapper has nothing to check",
			airlockSmokeScript, declared, airlockSmokeTestFile, wanted)
	}

	// The declaration must be the thing the preflight enforces. A correct list
	// nobody loops over is the same defect with a tidier spelling (AGENTS.md, on
	// tests driven by the declaration they check).
	if !strings.Contains(script, `for var in "${smoke_vars[@]}"`) {
		t.Errorf("%s does not run its preflight over smoke_vars, so the pinned list is not the enforced one",
			airlockSmokeScript)
	}
}

// The verdict, exercised through the script itself. Only an explicit PASS line
// for the named test is success; every other shape of `go test` output is a
// refusal, including the two that are exit 0.
func TestTheAirlockSmokeWrapperRefusesEveryOutcomeButAPass(t *testing.T) {
	name := airlockSmokeTestName(t)
	dir := t.TempDir()

	cases := []struct {
		desc, log string
		wantPass  bool
		wantSays  string
	}{
		{
			desc:     "pass",
			log:      fmt.Sprintf("=== RUN   %s\n--- PASS: %s (3.21s)\nPASS\nok\tairlock\t3.3s\n", name, name),
			wantPass: true,
		},
		{
			// go test exits 0 here, and this is the shape a rename produces.
			desc:     "the -run pattern matched nothing",
			log:      "testing: warning: no tests to run\nPASS\nok\tairlock\t0.1s [no tests to run]\n",
			wantSays: "never ran",
		},
		{
			// go test exits 0 here too: a skip is not a failure to Go, but it is
			// to a wrapper that already refused an empty variable.
			desc:     "the test skipped itself",
			log:      fmt.Sprintf("=== RUN   %s\n--- SKIP: %s (0.00s)\nPASS\nok\tairlock\t0.1s\n", name, name),
			wantSays: "skipped itself",
		},
		{
			desc:     "the test failed",
			log:      fmt.Sprintf("=== RUN   %s\n--- FAIL: %s (1.00s)\nFAIL\n", name, name),
			wantSays: "failed against the real control plane",
		},
		{
			// What an unanchored pattern buys: a near-miss name passing in place
			// of the one the wrapper was asked for.
			desc:     "a differently named test passed",
			log:      fmt.Sprintf("=== RUN   %sAgainstStaging\n--- PASS: %sAgainstStaging (1.00s)\nPASS\n", name, name),
			wantSays: "never ran",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			log := filepath.Join(dir, strings.ReplaceAll(tc.desc, " ", "-")+".log")
			writeFile(t, log, tc.log)

			out, code := runRepoScript(t, airlockSmokeScript, "--check-verdict", log)
			if tc.wantPass {
				if code != 0 {
					t.Fatalf("exit %d, want 0\n%s", code, out)
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

	// A log the wrapper cannot read says nothing about the run, which is not the
	// same as saying the run was fine.
	out, code := runRepoScript(t, airlockSmokeScript, "--check-verdict", filepath.Join(dir, "absent.log"))
	if code == 0 {
		t.Errorf("an unreadable log exited 0\n%s", out)
	}
}

// The verdict must govern the normal execution path, not only the credential-
// free --check-verdict entrance the table above drives. A fake go reproduces
// the dangerous exit-0/no-test result without a credential or network. Removing
// the live verdict call then makes this script exit 0 and this test fail.
func TestTheAirlockSmokeLivePathAppliesTheVerdict(t *testing.T) {
	fakeBin := t.TempDir()
	fakeGo := filepath.Join(fakeBin, "go")
	writeFile(t, fakeGo, "#!/bin/sh\nprintf '%s\\n' 'testing: warning: no tests to run' 'PASS' 'ok airlock 0.1s [no tests to run]'\n")
	if err := os.Chmod(fakeGo, 0o755); err != nil {
		t.Fatal(err)
	}

	root := moduleRoot(t)
	cmd := exec.Command("/bin/bash", filepath.Join(root, filepath.FromSlash(airlockSmokeScript)))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"BEN_AIRLOCK_URL=https://airlock.invalid",
		"BEN_AIRLOCK_TOKEN=not-a-real-token",
		"BEN_AIRLOCK_PROFILE=test-profile",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("live wrapper exited 0 although go reported no tests run\n%s", out)
	}
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("running live wrapper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "never ran") {
		t.Errorf("live refusal = %q, want the verdict to report that the smoke never ran", out)
	}
}
