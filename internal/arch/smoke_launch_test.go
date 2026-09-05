package arch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
)

// The real smoke is credentialed, but the ordering that protects those
// credentials' forge is not. Drive the live script with command fakes: the
// fake forge refuses its first mutation unless BEN's own status command has
// already reported a daemon that systemd-run placed in the delegated scope.
func TestSmokeReadinessPrecedesTheFirstForgeWrite(t *testing.T) {
	result := runSmokeLaunchFixture(t, "ready")
	if result.err == nil {
		t.Fatalf("fixture unexpectedly completed a smoke run\n%s", result.output)
	}
	if !strings.Contains(result.output, "no open pull request") {
		t.Fatalf("fixture stopped before its intended post-write boundary: %v\n%s", result.err, result.output)
	}
	if !result.ready {
		t.Fatal("the script mutated the forge without observing BEN's ready publication")
	}
	for _, want := range []string{"--user", "--scope", "--collect", "--property=Delegate=yes", " run "} {
		if !strings.Contains(" "+strings.ReplaceAll(result.systemdArgs, "\n", " ")+" ", want) {
			t.Errorf("systemd-run argv does not contain %q:\n%s", want, result.systemdArgs)
		}
	}
	if !strings.Contains(result.writes, "label create\nissue create\n") {
		t.Fatalf("forge writes = %q, want the one-run label followed by its issue", result.writes)
	}
}

// A launch command existing is not readiness. Make the command exit before it
// can publish runs.json and prove that neither of the two forge mutations is
// attempted.
func TestSmokeReadinessRefusalLeavesTheForgeUnchanged(t *testing.T) {
	result := runSmokeLaunchFixture(t, "refuse")
	if result.err == nil {
		t.Fatal("a refused daemon made the smoke wrapper succeed")
	}
	if !strings.Contains(result.output, "no queue label or issue was created") {
		t.Fatalf("refusal did not state the write boundary: %v\n%s", result.err, result.output)
	}
	if result.ready {
		t.Fatal("the refusing fixture published ready")
	}
	if result.writes != "" {
		t.Fatalf("forge writes after readiness refusal = %q, want none", result.writes)
	}
}

// required_labels is not an environment-expanding field. Drive the renderer
// and the real loader together so the per-run route the script creates is
// exactly the literal route the daemon polls.
func TestSmokeWorkflowUsesTheOneRunAbsentRoute(t *testing.T) {
	root := moduleRoot(t)
	source := filepath.Join(root, "scripts", "smoke-workflow.md")
	workflow := read(t, source)
	const marker = "    - __BEN_SMOKE_QUEUE_LABEL__"
	if count := strings.Count(workflow, marker); count != 1 {
		t.Fatalf("smoke workflow queue-label markers = %d, want 1", count)
	}

	const label = "ben-smoke-0123456789abcdef0123456789abcdef"
	rendered := filepath.Join(t.TempDir(), "smoke-workflow.md")
	cmd := exec.Command("bash", filepath.Join(root, smokeScript), "--check-workflow-label", source, rendered, label)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render smoke workflow: %v: %s", err, out)
	}
	t.Setenv("BEN_SMOKE_REPO", "acme/canary")
	t.Setenv("BEN_SMOKE_WORKSPACE", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "inert-smoke-key")
	def, err := config.Load(rendered)
	if err != nil {
		t.Fatalf("load rendered smoke workflow: %v", err)
	}
	if got := def.Config.Tracker.RequiredLabels; len(got) != 1 || got[0] != label {
		t.Fatalf("rendered required_labels = %q, want [%q]", got, label)
	}

	script := read(t, filepath.Join(root, smokeScript))
	ready := strings.LastIndex(script, "await_delegated_readiness")
	write := strings.Index(script, "gh_tracker label create")
	if ready < 0 || write < 0 || ready > write {
		t.Fatalf("live smoke order = readiness at %d, first forge write at %d", ready, write)
	}
}

// BEN can exit after readiness while a supervisor or descendant keeps its
// delegated scope active. The scope, not BEN's PID, is the cleanup authority:
// closing the issue or deleting its route first would strand live work.
func TestSmokePostReadinessCrashWaitsForScopeBeforeForgeCleanup(t *testing.T) {
	result := runSmokeLaunchFixture(t, "crash")
	if result.err == nil {
		t.Fatalf("fixture unexpectedly completed a smoke run\n%s", result.output)
	}
	if !strings.Contains(result.output, "the daemon exited; waiting for its execution domain to become quiet") {
		t.Fatalf("post-readiness crash was not diagnosed: %v\n%s", result.err, result.output)
	}
	quiet := strings.Index(result.events, "scope quiet\n")
	if quiet < 0 {
		t.Fatalf("events contain no confirmed scope quiet:\n%s", result.events)
	}
	for _, mutation := range []string{"pr close\n", "issue close\n", "label delete\n"} {
		at := strings.Index(result.events, mutation)
		if at < 0 || at < quiet {
			t.Errorf("%q at %d, scope quiet at %d; events:\n%s", strings.TrimSpace(mutation), at, quiet, result.events)
		}
	}
}

type smokeLaunchResult struct {
	err         error
	output      string
	systemdArgs string
	writes      string
	events      string
	ready       bool
}

func runSmokeLaunchFixture(t *testing.T, mode string) smokeLaunchResult {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "smoke.sh")
	writeFile(t, script, read(t, filepath.Join(moduleRoot(t), smokeScript)))
	chmodExecutable(t, script)
	writeFile(t, filepath.Join(root, "WORKFLOW.md"), "fixture\n")
	writeFile(t, filepath.Join(root, "scripts", "smoke-workflow.md"), read(t, filepath.Join(moduleRoot(t), "scripts", "smoke-workflow.md")))

	fixtureDir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(fixtureDir, "daemon.pid")
	scopePIDFile := filepath.Join(fixtureDir, "scope.pid")
	readyFile := filepath.Join(fixtureDir, "ready")
	crashTriggerFile := filepath.Join(fixtureDir, "crash-trigger")
	daemonDeadFile := filepath.Join(fixtureDir, "daemon-dead")
	showCountFile := filepath.Join(fixtureDir, "show-count")
	argsFile := filepath.Join(fixtureDir, "systemd.args")
	writesFile := filepath.Join(fixtureDir, "writes")
	eventsFile := filepath.Join(fixtureDir, "events")
	fakeBen := filepath.Join(fixtureDir, "ben")
	writeFile(t, fakeBen, `#!/bin/sh
reported_pid=
finish() {
	if [ -n "$reported_pid" ]; then
		kill -TERM "$reported_pid" 2>/dev/null || true
		wait "$reported_pid" 2>/dev/null || true
	fi
	rm -f "$SMOKE_FIXTURE_PID" "$SMOKE_FIXTURE_SCOPE_PID"
	exit 0
}
case "${1-}" in
config)
	exit 0
	;;
run)
	if [ "$SMOKE_FIXTURE_MODE" = refuse ]; then
		echo "fixture readiness refusal" >&2
		exit 41
	fi
	printf '%s\n' "$$" >"$SMOKE_FIXTURE_SCOPE_PID"
	trap finish HUP INT TERM
	if [ "$SMOKE_FIXTURE_MODE" = crash ]; then
		sleep 300 &
		reported_pid=$!
		printf '%s\n' "$reported_pid" >"$SMOKE_FIXTURE_PID"
		while [ ! -f "$SMOKE_FIXTURE_CRASH_TRIGGER" ]; do sleep 0.01; done
		kill -TERM "$reported_pid" 2>/dev/null || true
		wait "$reported_pid" 2>/dev/null || true
		reported_pid=
		rm -f "$SMOKE_FIXTURE_PID"
		: >"$SMOKE_FIXTURE_DAEMON_DEAD"
		while :; do
			count=0
			[ ! -s "$SMOKE_FIXTURE_SHOW_COUNT" ] || count=$(sed -n '1p' "$SMOKE_FIXTURE_SHOW_COUNT")
			[ "$count" -ge 2 ] && break
			sleep 0.01
		done
		rm -f "$SMOKE_FIXTURE_SCOPE_PID"
		exit 0
	fi
	printf '%s\n' "$$" >"$SMOKE_FIXTURE_PID"
	while :; do sleep 1; done
	;;
status)
	if [ -s "$SMOKE_FIXTURE_PID" ]; then
		pid=$(sed -n '1p' "$SMOKE_FIXTURE_PID")
		: >"$SMOKE_FIXTURE_READY"
		printf 'status  running — pid %s, last heartbeat 0s ago\n' "$pid"
	else
		printf 'No BEN state\n'
	fi
	;;
*)
	exit 97
	;;
esac
`)
	chmodExecutable(t, fakeBen)

	fakeBin := filepath.Join(root, "bin")
	writeExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
if [ "${1-}" = env ] && [ "${2-}" = GO111MODULE ]; then
	exit 0
fi
if [ "${1-}" = run ]; then
	printf 'tracker:\n  provider:\n    repo: ben/own\n'
	exit 0
fi
if [ "${1-}" = build ] && [ "${2-}" = -o ] && [ -n "${3-}" ]; then
	cp "$SMOKE_FAKE_BEN" "$3"
	chmod 755 "$3"
	exit 0
fi
exit 97
`)
	writeExecutable(t, filepath.Join(fakeBin, "uname"), "#!/bin/sh\nprintf 'Linux\\n'\n")
	writeExecutable(t, filepath.Join(fakeBin, "git"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "claude"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "srt"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "systemd-run"), `#!/bin/sh
printf '%s\n' "$@" >"$SMOKE_FIXTURE_SYSTEMD_ARGS"
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
[ "${1-}" = -- ] || exit 98
shift
exec "$@"
`)
	writeExecutable(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
[ "${1-}" = --user ] && shift
case "${1-}" in
show-environment)
	exit 0
	;;
show)
	if [ "$SMOKE_FIXTURE_MODE" = crash ] && [ -f "$SMOKE_FIXTURE_DAEMON_DEAD" ]; then
		count=0
		[ ! -s "$SMOKE_FIXTURE_SHOW_COUNT" ] || count=$(sed -n '1p' "$SMOKE_FIXTURE_SHOW_COUNT")
		count=$((count + 1))
		printf '%s\n' "$count" >"$SMOKE_FIXTURE_SHOW_COUNT"
		if [ "$count" -ge 2 ]; then
			for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
				[ ! -s "$SMOKE_FIXTURE_SCOPE_PID" ] && break
				sleep 0.01
			done
		fi
	fi
	if [ -s "$SMOKE_FIXTURE_SCOPE_PID" ]; then
		printf 'scope active\n' >>"$SMOKE_FIXTURE_EVENTS"
		printf 'active\n'
	else
		printf 'scope quiet\n' >>"$SMOKE_FIXTURE_EVENTS"
		printf 'inactive\n'
	fi
	;;
stop)
	if [ -s "$SMOKE_FIXTURE_SCOPE_PID" ]; then kill -TERM "$(sed -n '1p' "$SMOKE_FIXTURE_SCOPE_PID")"; fi
	;;
*)
	exit 97
	;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
case "${1-}:${2-}" in
api:*)
	# Credential and label-absence reads all succeed; an empty paginated
	# label response is the absence the script needs.
	exit 0
	;;
label:create)
	[ -f "$SMOKE_FIXTURE_READY" ] || { echo "label write preceded readiness" >&2; exit 96; }
	printf 'label create\n' >>"$SMOKE_FIXTURE_WRITES"
	printf 'label create\n' >>"$SMOKE_FIXTURE_EVENTS"
	;;
label:delete)
	printf 'label delete\n' >>"$SMOKE_FIXTURE_EVENTS"
	exit 0
	;;
issue:create)
	[ -f "$SMOKE_FIXTURE_READY" ] || { echo "issue write preceded readiness" >&2; exit 96; }
	printf 'issue create\n' >>"$SMOKE_FIXTURE_WRITES"
	printf 'issue create\n' >>"$SMOKE_FIXTURE_EVENTS"
	if [ "$SMOKE_FIXTURE_MODE" = crash ]; then
		: >"$SMOKE_FIXTURE_CRASH_TRIGGER"
		for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
			[ -f "$SMOKE_FIXTURE_DAEMON_DEAD" ] && break
			sleep 0.01
		done
		[ -f "$SMOKE_FIXTURE_DAEMON_DEAD" ] || exit 95
	fi
	printf 'https://github.example/acme/canary/issues/7\n'
	;;
issue:close)
	printf 'issue close\n' >>"$SMOKE_FIXTURE_EVENTS"
	exit 0
	;;
pr:close)
	printf 'pr close\n' >>"$SMOKE_FIXTURE_EVENTS"
	exit 0
	;;
*)
	exit 97
	;;
esac
`)

	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", script)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + fakeBin + ":/usr/bin:/bin",
		"HOME=" + root,
		"TMPDIR=" + tmp,
		"BEN_SMOKE_REPO=acme/canary",
		"BEN_SMOKE_TIMEOUT=0",
		"BEN_SMOKE_READY_TIMEOUT=5",
		"GITHUB_TOKEN=tracker-fixture",
		"GH_TOKEN=publisher-fixture",
		"ANTHROPIC_API_KEY=harness-fixture",
		"SMOKE_FAKE_BEN=" + fakeBen,
		"SMOKE_FIXTURE_PID=" + pidFile,
		"SMOKE_FIXTURE_SCOPE_PID=" + scopePIDFile,
		"SMOKE_FIXTURE_READY=" + readyFile,
		"SMOKE_FIXTURE_CRASH_TRIGGER=" + crashTriggerFile,
		"SMOKE_FIXTURE_DAEMON_DEAD=" + daemonDeadFile,
		"SMOKE_FIXTURE_SHOW_COUNT=" + showCountFile,
		"SMOKE_FIXTURE_SYSTEMD_ARGS=" + argsFile,
		"SMOKE_FIXTURE_WRITES=" + writesFile,
		"SMOKE_FIXTURE_EVENTS=" + eventsFile,
		"SMOKE_FIXTURE_MODE=" + mode,
	}
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("smoke launch fixture hung:\n%s", out)
	}
	return smokeLaunchResult{
		err:         err,
		output:      string(out),
		systemdArgs: readOptional(t, argsFile),
		writes:      readOptional(t, writesFile),
		events:      readOptional(t, eventsFile),
		ready:       fileExistsForTest(readyFile),
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	writeFile(t, path, content)
	chmodExecutable(t, path)
}

func chmodExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func readOptional(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func fileExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
