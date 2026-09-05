package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// docs/SMOKE.md's Kubernetes canary runtime preflight (#189), held to the facts
// it reports on.
//
// The preflight is the check the canary profile does not otherwise have:
// `make smoke` runs scripts/smoke.sh, which validates its own tools before it
// creates anything, and a label-dispatched pod validates nothing — so the
// section *is* that step, and a stale command in it fails as a burned agent
// attempt half an hour later rather than as a wrong string on a screen.
//
// Per AGENTS.md, a table of markers proves only that the listed markers are
// present. So each assertion below is anchored where the doc's claim actually
// comes from, and none of those anchors is edited by editing the section: the
// Dockerfile's ENTRYPOINT and the image smoke test for the PID 1 and `ps`
// contract, the publish workflow's `IMAGE_REPOSITORY` for the registry,
// internal/registry for the adapter kinds an operator might have configured,
// this module's own source for every log line the section tells an operator to
// grep for, and the section's own commands for the read-only rule.

const preflightHeading = "Kubernetes canary runtime preflight"

// preflightSection returns docs/SMOKE.md's preflight section, code blocks
// included: a command in a fenced block is most of what this section is.
func preflightSection(t *testing.T) []docLine {
	t.Helper()
	var out []docLine
	in := false
	for _, line := range readDoc(t, filepath.Join(moduleRoot(t), "docs", "SMOKE.md")) {
		text := strings.TrimSpace(line.text)
		if !line.fenced && strings.HasPrefix(text, "## ") {
			in = strings.TrimSpace(strings.TrimPrefix(text, "##")) == preflightHeading
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("docs/SMOKE.md has no %q section", preflightHeading)
	}
	return out
}

// preflightText is that section flattened to one string, for containment
// checks that must survive a rewrap.
func preflightText(t *testing.T) string {
	t.Helper()
	var lines []string
	for _, line := range preflightSection(t) {
		lines = append(lines, line.text)
	}
	return flatten(strings.Join(lines, " "))
}

var dockerfileEntrypointPath = regexp.MustCompile(`"(/[^"]+)"`)

// The section names the runtime image's PID 1 and the binary underneath it, and
// it names them as the Dockerfile spells them.
//
// Anchored on the ENTRYPOINT rather than on "tini": the operator's check is
// worth running only if it compares against what the image is actually built
// with, and #185 could have chosen a different init. TestRuntimeImageUsesAReapingInit
// holds the other end — that the entrypoint is a reaping init at all.
func TestCanaryPreflightNamesTheEntrypointTheImageDeclares(t *testing.T) {
	dockerfile := runtimeDockerfile(t)
	section := preflightText(t)

	var entrypoint string
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ENTRYPOINT ") {
			entrypoint = line
			break
		}
	}
	if entrypoint == "" {
		t.Fatal("the Dockerfile declares no ENTRYPOINT; the preflight has nothing to be checked against")
	}

	found := 0
	for _, m := range dockerfileEntrypointPath.FindAllStringSubmatch(entrypoint, -1) {
		found++
		if !strings.Contains(section, m[1]) {
			t.Errorf("docs/SMOKE.md's %q section does not name %s, which the image's ENTRYPOINT runs; "+
				"an operator comparing `ps` output against this section would not see a changed init",
				preflightHeading, m[1])
		}
	}
	if found == 0 {
		t.Errorf("no path parsed out of ENTRYPOINT line %q, so this test checked nothing", entrypoint)
	}
}

// The exact `ps` query BEN's process-discipline suite depends on (#186), used
// by the image's CI smoke test and by the operator's preflight alike.
const psStateProbe = "ps -o stat= -p 1"

// The `ps` half of the runtime contract is asserted in both places, and it is
// the same query in both.
//
// Two-sided on purpose: the preflight exists to tell an operator that procps is
// present *before* a run needs it, and the statement is only worth anything if
// what the operator runs is what the suite runs. CI proves the image can answer
// the query; this proves the section asks it.
func TestCanaryPreflightAsksTheProcessProbeCIProves(t *testing.T) {
	root := moduleRoot(t)
	workflow := read(t, filepath.Join(root, ".github", "workflows", "publish-daemon-image.yml"))
	if !strings.Contains(workflow, psStateProbe) {
		t.Errorf("the daemon image workflow no longer runs %q; the preflight's `ps` check is being held "+
			"to a query nothing else proves the image can answer", psStateProbe)
	}
	if !strings.Contains(preflightText(t), psStateProbe) {
		t.Errorf("docs/SMOKE.md's %q section does not run %q; a missing ps reads as a false death verdict "+
			"in every §7.5 liveness probe (#186)", preflightHeading, psStateProbe)
	}
}

var imageRepositoryEnv = regexp.MustCompile(`(?m)^\s*IMAGE_REPOSITORY:\s*(\S+)\s*$`)

// The registry the section tells an operator to compare a digest against is the
// one the publish workflow pushes to.
//
// Anchored on the workflow because the account, region and repository path are
// its to change, and a preflight naming the old one reads as a pass while the
// pod runs an image from somewhere else entirely.
func TestCanaryPreflightNamesThePublishedImageRepository(t *testing.T) {
	root := moduleRoot(t)
	workflow := read(t, filepath.Join(root, ".github", "workflows", "publish-daemon-image.yml"))
	m := imageRepositoryEnv.FindStringSubmatch(workflow)
	if m == nil {
		t.Fatalf("the daemon image workflow declares no IMAGE_REPOSITORY, so %q matched nothing", imageRepositoryEnv)
	}
	if !strings.Contains(preflightText(t), m[1]) {
		t.Errorf("docs/SMOKE.md's %q section does not name %s, the repository the daemon image is published to",
			preflightHeading, m[1])
	}
}

// Every agent kind BEN registers is accounted for in the section.
//
// The criterion is that an operator checks the harness the *active workflow*
// names rather than assuming the image carries both, and the failure that
// wording guards against is a third adapter arriving with nothing said about
// which binary it needs. So this is driven by the registry — the one table the
// loader and `ben config effective` both answer from — and not by a list here.
func TestCanaryPreflightAccountsForEveryRegisteredAgentKind(t *testing.T) {
	section := preflightText(t)
	kinds := registry.RunnerNames()
	if len(kinds) < 2 {
		t.Fatalf("registry reports %d agent kinds; with fewer than two there is no assumption to guard against", len(kinds))
	}
	for _, kind := range kinds {
		if !strings.Contains(section, kind) {
			t.Errorf("docs/SMOKE.md's %q section says nothing about agent.kind %q; an operator cannot check "+
				"the configured harness against a table that does not list it", preflightHeading, kind)
		}
	}
	// The kind is what the workflow declares; the binary is what the operator
	// looks for. Reading the first is the step that makes the second specific.
	for _, want := range []string{"ben config effective", "agent.provider.binary"} {
		if !strings.Contains(section, want) {
			t.Errorf("docs/SMOKE.md's %q section does not mention %q; without it the harness check is a guess "+
				"about which binary this pod's workflow needs", preflightHeading, want)
		}
	}
}

// Any email address in the section, in the form a GitHub App's commits carry.
var (
	anyEmail       = regexp.MustCompile(`[\w.+\[\]-]+@[\w-]+(?:\.[\w-]+)+`)
	appBotNoreply  = regexp.MustCompile(`^\d+\+[\w-]+\[bot\]@users\.noreply\.github\.com$`)
	gitIdentityCmd = []string{"printenv GIT_AUTHOR_EMAIL", "git config --global user.email"}
)

// Every address the section holds out as the publishing identity is an app-bot
// noreply address, and both halves of the identity are checked.
//
// Anchored on the addresses the section actually contains rather than on the
// one it contains today: a personal `…@users.noreply.github.com` pasted in as
// an example is indistinguishable from the App's at a glance, and it is the
// identity half of §10.1's forge control that it silently disables.
//
// Both commands are required because §7.6 composes the agent's environment
// instead of inheriting it: a pod-level GIT_AUTHOR_* reaches the daemon and not
// the run, so the environment check alone passes on a pod whose agent cannot
// commit at all.
func TestCanaryPreflightVerifiesTheAppBotIdentity(t *testing.T) {
	section := preflightText(t)

	emails := anyEmail.FindAllString(section, -1)
	if len(emails) == 0 {
		t.Fatalf("docs/SMOKE.md's %q section names no email address, so it states no identity to verify", preflightHeading)
	}
	for _, email := range emails {
		if !appBotNoreply.MatchString(email) {
			t.Errorf("docs/SMOKE.md's %q section names %q, which is not an app-bot noreply address "+
				"(<id>+<app>[bot]@users.noreply.github.com); a commit authored as anything else is not "+
				"attributed to the publishing App", preflightHeading, email)
		}
	}
	for _, cmd := range gitIdentityCmd {
		if !strings.Contains(section, cmd) {
			t.Errorf("docs/SMOKE.md's %q section does not run `%s`; the environment and the config file are "+
				"different answers, and the agent's composed §7.6 environment only sees the second",
				preflightHeading, cmd)
		}
	}
}

// kubectlValueFlags are the global flags whose value is a separate token, so a
// namespace is not mistaken for a subcommand.
var kubectlValueFlags = map[string]bool{
	"-n": true, "--namespace": true,
	"-o": true, "--output": true,
	"-c": true, "--container": true,
	"-l": true, "--selector": true,
	"--context": true, "--kubeconfig": true,
}

// kubectlSubcommand returns the subcommand of a kubectl invocation — the first
// token after `kubectl` that is neither a flag nor a flag's value.
func kubectlSubcommand(command string) (string, bool) {
	fields := strings.Fields(command)
	for i := 0; i < len(fields); i++ {
		if fields[i] != "kubectl" {
			continue
		}
		for j := i + 1; j < len(fields); j++ {
			tok := fields[j]
			if strings.HasPrefix(tok, "-") {
				if kubectlValueFlags[tok] {
					j++
				}
				continue
			}
			return tok, true
		}
	}
	return "", false
}

// readOnlyKubectl is every subcommand the preflight may use. GitOps owns the pod
// contract, so a mutating command here is reverted by Argo self-heal at exit 0 —
// it changes nothing and reads as though it did.
var readOnlyKubectl = map[string]bool{
	"get": true, "describe": true, "logs": true, "exec": true, "top": true, "version": true,
}

// Every kubectl command in the section is read-only and points at this canary.
//
// Driven by the commands present rather than by a list of the ones written
// today: the failure is somebody pasting a new command into the section, which
// is exactly what a marker table cannot see. The namespace and pod are asserted
// here for the same reason — an example that names another deployment is a
// command an operator runs against the wrong pod and believes.
func TestCanaryPreflightCommandsAreReadOnlyAndNameThisCanary(t *testing.T) {
	const (
		namespace = "-n ben"
		pod       = "ben-daemon-0"
	)

	found := 0
	for _, line := range joinedCommands(preflightSection(t)) {
		if !strings.Contains(line.text, "kubectl") {
			continue
		}
		found++
		sub, ok := kubectlSubcommand(line.text)
		if !ok {
			t.Errorf("docs/SMOKE.md:%d: cannot read a subcommand out of %q", line.number, line.text)
			continue
		}
		if !readOnlyKubectl[sub] {
			t.Errorf("docs/SMOKE.md:%d: `kubectl %s` mutates the pod contract GitOps owns; Argo self-heal "+
				"reverts it silently, so the preflight stays read-only", line.number, sub)
		}
		if !strings.Contains(line.text, namespace) {
			t.Errorf("docs/SMOKE.md:%d: kubectl command names no namespace (%q): %q",
				line.number, namespace, line.text)
		}
		if strings.Contains(line.text, "ben-daemon") && !strings.Contains(line.text, pod) {
			t.Errorf("docs/SMOKE.md:%d: kubectl command names a pod other than %s: %q", line.number, pod, line.text)
		}
	}
	if found < 5 {
		t.Errorf("only %d kubectl command(s) in the %q section; the preflight covers the pod, its process "+
			"tree, its harness, its git identity and its state", found, preflightHeading)
	}
}

// The read-only rule is only as good as the parse, and the parse has one real
// trap: a flag's value is not a subcommand. `kubectl -n ben get …` read
// naively yields `ben`, an unknown name that a lookup against readOnlyKubectl
// then rejects — the check would fail every command for the wrong reason, and a
// later fix that returned the first token unconditionally would pass every
// command for the wrong reason too. Both answers are asserted here.
func TestKubectlSubcommandReadsPastFlagsAndTheirValues(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"kubectl -n ben get pod ben-daemon-0 -o jsonpath='{.status}'", "get"},
		{"kubectl -n ben exec ben-daemon-0 -- ps -o pid=,args= -e", "exec"},
		{"kubectl --namespace ben logs ben-daemon-0", "logs"},
		{"kubectl -n ben apply -f -", "apply"},
		{"kubectl -n ben scale statefulset/ben-daemon --replicas=0", "scale"},
		{"kubectl -n ben get pod ben-daemon-0 | jq .", "get"},
	} {
		t.Run(tc.want+"/"+tc.command, func(t *testing.T) {
			got, ok := kubectlSubcommand(tc.command)
			if !ok || got != tc.want {
				t.Errorf("kubectlSubcommand(%q) = %q, %v; want %q", tc.command, got, ok, tc.want)
			}
			if readOnlyKubectl[got] != readOnlyKubectl[tc.want] {
				t.Errorf("%q is classified against the wrong subcommand", tc.command)
			}
		})
	}
}

// joinedCommands returns the fenced lines of a section with backslash
// continuations joined onto the line that started them, so a command wrapped
// for width is read as the one command it is.
func joinedCommands(section []docLine) []docLine {
	var out []docLine
	for _, line := range section {
		if !line.fenced {
			continue
		}
		text := strings.TrimSpace(line.text)
		if n := len(out); n > 0 && strings.HasSuffix(out[n-1].text, `\`) {
			out[n-1].text = strings.TrimSuffix(out[n-1].text, `\`) + " " + text
			continue
		}
		out = append(out, docLine{text: text, number: line.number})
	}
	return out
}

// Every daemon output the section tells an operator to look for still exists in
// this module's source.
//
// This is the anchor the status and recovery step rests on. A log message is
// prose to the program and a contract to the person grepping for it: renaming
// one breaks the preflight silently, and the daemon's own tests have no reason
// to notice. `recovery did not complete` is the sharpest of them — the section's
// instruction is that its *absence* is the pass, and a marker that can no longer
// appear is absent on every pod forever.
func TestCanaryPreflightGrepsForOutputTheDaemonStillEmits(t *testing.T) {
	sources := goSources(t, moduleRoot(t))
	section := preflightText(t)

	for _, tc := range []struct {
		marker string
		why    string
	}{
		{"deployment declares attended mode", "the §10.1 mode this canary is allowed to run under"},
		{"deployment declares risk-accepted mode", "the mode that is a stop on a pod sharing one process identity"},
		{"recovering", "§9.10 reconstructing the claims this principal holds"},
		{"recovery did not complete", "the line whose absence is the pass"},
		{"last heartbeat", "what `ben status` says about a daemon that is still writing"},
		{"state dir", "where `ben status` read its answer from"},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			if !strings.Contains(section, tc.marker) {
				t.Errorf("docs/SMOKE.md's %q section does not mention %q — %s", preflightHeading, tc.marker, tc.why)
			}
			if !strings.Contains(sources, tc.marker) {
				t.Errorf("no source file in this module emits %q, but docs/SMOKE.md's %q section tells an "+
					"operator to look for it — %s", tc.marker, preflightHeading, tc.why)
			}
		})
	}
}

// goSources concatenates every non-test Go file in the module, under the same
// scoping rules the markdown walk uses. Deliberately not the import walks':
// an operator-facing marker has to come from source `go test ./...` builds, not
// from a testdata helper.
func goSources(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (ignoredPackageDir(d.Name()) || isModuleRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Len() == 0 {
		t.Fatal("no Go source read; the walk is checking nothing")
	}
	return b.String()
}
