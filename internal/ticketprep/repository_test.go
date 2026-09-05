package ticketprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCaptureReadsOnlyTheRecordedCommit(t *testing.T) {
	repo := fixtureRepository(t)
	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        7,
		URL:           "https://github.com/acme/widget/issues/7",
		Title:         "Change `UniqueThing` without changing `config.value`",
		Body: strings.Join([]string{
			"Inspect `pkg/thing.go` and `thing.go:3`.",
			"Also consider `Dockerfile`, `Duplicate`, `BrokenSymbol`, `WorkingTreeOnly`, `missing.go`, `untracked.go`, and `ben/<workspace_key>`.",
		}, "\n"),
	}

	clean, err := CaptureIssue(context.Background(), repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "pkg", "thing.go"), "package thing\n\nfunc WorkingTreeOnly() {}\n")
	writeTestFile(t, filepath.Join(repo, "untracked.go"), "package untracked\nfunc WorkingTreeOnly() {}\n")
	dirty, err := CaptureIssue(context.Background(), repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clean, dirty) {
		t.Fatalf("working-tree bytes changed the committed capture:\nclean: %#v\ndirty: %#v", clean, dirty)
	}

	assertPathFact(t, dirty.Facts.Paths, "pkg/thing.go", FactExists, "pkg/thing.go")
	assertPathFact(t, dirty.Facts.Paths, "thing.go:3", FactExists, "pkg/thing.go")
	assertPathFact(t, dirty.Facts.Paths, "Dockerfile", FactExists, "Dockerfile")
	assertPathFact(t, dirty.Facts.Paths, "missing.go", FactAbsent, "")
	assertPathFact(t, dirty.Facts.Paths, "untracked.go", FactAbsent, "")
	assertSymbolFact(t, dirty.Facts.Symbols, "UniqueThing", FactExists, "pkg/thing.go")
	assertSymbolFact(t, dirty.Facts.Symbols, "Duplicate", FactUnknown, "")
	assertSymbolFact(t, dirty.Facts.Symbols, "BrokenSymbol", FactUnknown, "")
	assertSymbolFact(t, dirty.Facts.Symbols, "WorkingTreeOnly", FactAbsent, "")

	if got := instructionPaths(dirty.Facts.InstructionFiles); !reflect.DeepEqual(got, []string{"AGENTS.md", "pkg/AGENTS.md"}) {
		t.Fatalf("instruction files = %v", got)
	}
	if got := commandNames(dirty.Facts.ValidationCommands); !reflect.DeepEqual(got, []string{"make check", "make test", "go test ./pkg"}) {
		t.Fatalf("validation commands = %v", got)
	}
	for _, command := range dirty.Facts.ValidationCommands {
		if command.Blob == "" || command.Line <= 0 || command.Source == "" {
			t.Fatalf("command lacks committed evidence: %+v", command)
		}
	}
	if got := unknownReferences(dirty.Facts.Unknown); !reflect.DeepEqual(got, []string{"ben/<workspace_key>", "config.value"}) {
		t.Fatalf("unknown references = %v", got)
	}
	if dirty.Repository.Commit == "" || dirty.Repository.Tree == "" || dirty.Repository.Identity != "github.com/acme/widget" {
		t.Fatalf("repository binding = %+v", dirty.Repository)
	}
}

func TestCaptureRefusesIssueFromAnotherRepository(t *testing.T) {
	repo := fixtureRepository(t)
	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        9,
		URL:           "https://github.com/acme/other/issues/9",
		Title:         "Mismatch",
		Body:          "",
	}
	if _, err := CaptureIssue(context.Background(), repo, issue); err == nil || !strings.Contains(err.Error(), "binding mismatch") {
		t.Fatalf("error = %v, want repository binding mismatch", err)
	}
}

// The expected prefix is independent of gitcmd.Argv: shrinking the production
// declaration cannot shrink the test's expectation with it (AGENTS.md,
// Conventions). Capture is developer-only, but it is still Git BEN starts — and
// it is pointed at a developer-selected repository, so #228's neutralization of
// the hook, fsmonitor and replace-ref surfaces is the half of this prefix that
// answers for a repository nobody here vouched for.
var wantTicketprepInvocationArgv = []string{
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "core.hooksPath=",
	"-c", "core.fsmonitor=",
	"-c", "core.useReplaceRefs=false",
}

func TestEveryCaptureGitUsesTheSharedInvocationBoundary(t *testing.T) {
	repo := fixtureRepository(t)
	recorded := installTicketprepGitRecorder(t)
	// A repository-local variable inherited from an unrelated caller must not
	// redirect capture away from the repository named by -repo.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong.git"))
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "0")
	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        7,
		URL:           "https://github.com/acme/widget/issues/7",
		Title:         "Inspect `UniqueThing`",
		Body:          "Inspect `pkg/thing.go`.",
	}
	if _, err := CaptureIssue(context.Background(), repo, issue); err != nil {
		t.Fatal(err)
	}
	record := recorded()
	invocations := record.argv
	if len(invocations) == 0 {
		t.Fatal("no git invocation recorded; the boundary test is inert")
	}
	for _, argv := range invocations {
		if len(argv) < len(wantTicketprepInvocationArgv) || !slices.Equal(argv[:len(wantTicketprepInvocationArgv)], wantTicketprepInvocationArgv) {
			t.Errorf("git %q does not begin with %q", argv, wantTicketprepInvocationArgv)
		}
	}
	for _, subcommand := range []string{"rev-parse", "config", "ls-tree", "grep", "cat-file"} {
		if !slices.ContainsFunc(invocations, func(argv []string) bool { return slices.Contains(argv, subcommand) }) {
			t.Errorf("capture recorded no git %s; that invocation seam is uncovered", subcommand)
		}
	}
	if len(record.guards) != len(invocations) {
		t.Fatalf("recorded %d argv and %d environment rows", len(invocations), len(record.guards))
	}
	for i, guards := range record.guards {
		if guards != "1\x1f1" {
			t.Errorf("git invocation %d offline/object guards = %q, want lazy-fetch=1 and replacements=1", i, guards)
		}
	}
}

func TestCommittedExtensionlessPathIsNotReinterpretedAsSymbol(t *testing.T) {
	entry := treeEntry{path: "Dockerfile", blob: strings.Repeat("b", 40), kind: "blob"}
	r := newRepositoryReader("", strings.Repeat("a", 40), strings.Repeat("c", 40), map[string]treeEntry{"Dockerfile": entry})
	facts, err := r.observe(context.Background(), "Inspect `Dockerfile`", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Symbols) != 0 || len(facts.Paths) != 1 {
		t.Fatalf("paths = %+v, symbols = %+v", facts.Paths, facts.Symbols)
	}
	assertPathFact(t, facts.Paths, "Dockerfile", FactExists, "Dockerfile")
}

func TestSymbolFactBoundRefusesBeforeAnyPerSymbolGitSearch(t *testing.T) {
	repo := fixtureRepository(t)
	recorded := installTicketprepGitRecorder(t)
	var body strings.Builder
	for i := 0; i <= maxFactCount; i++ {
		fmt.Fprintf(&body, "`Symbol%d` ", i)
	}
	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        7,
		URL:           "https://github.com/acme/widget/issues/7",
		Title:         "Oversized symbol frontier",
		Body:          body.String(),
	}
	if _, err := CaptureIssue(context.Background(), repo, issue); !errors.Is(err, ErrBoundExceeded) {
		t.Fatalf("error = %v, want bound refusal before git search", err)
	}
	for _, argv := range recorded().argv {
		if slices.Contains(argv, "grep") {
			t.Fatalf("oversized symbol frontier launched git grep: %q", argv)
		}
	}
}

func TestCaptureIgnoresReplacementRefs(t *testing.T) {
	repo := fixtureRepository(t)
	original := runFixtureGit(t, repo, "rev-parse", "HEAD")
	rawTree := runFixtureGit(t, repo, "--no-replace-objects", "rev-parse", original+"^{tree}")
	writeTestFile(t, filepath.Join(repo, "replacement.go"), "package replacement\nfunc ReplacementOnly() {}\n")
	runFixtureGit(t, repo, "add", "replacement.go")
	replacementTree := runFixtureGit(t, repo, "write-tree")
	replacement := runFixtureGit(t, repo, "commit-tree", replacementTree, "-p", original, "-m", "replacement")
	runFixtureGit(t, repo, "replace", original, replacement)
	if replacementAware := runFixtureGit(t, repo, "rev-parse", original+"^{tree}"); replacementAware == rawTree {
		t.Fatal("replacement fixture is inert")
	}

	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        7,
		URL:           "https://github.com/acme/widget/issues/7",
		Title:         "Inspect `UniqueThing` and `ReplacementOnly`",
		Body:          "Check `replacement.go`.",
	}
	capture, err := CaptureIssue(context.Background(), repo, issue)
	if err != nil {
		t.Fatal(err)
	}
	if capture.Repository.Commit != original || capture.Repository.Tree != rawTree {
		t.Fatalf("capture binding = commit %s tree %s, want raw %s %s", capture.Repository.Commit, capture.Repository.Tree, original, rawTree)
	}
	assertPathFact(t, capture.Facts.Paths, "replacement.go", FactAbsent, "")
	assertSymbolFact(t, capture.Facts.Symbols, "ReplacementOnly", FactAbsent, "")
	assertSymbolFact(t, capture.Facts.Symbols, "UniqueThing", FactExists, "pkg/thing.go")
}

func TestPathBasenameAmbiguityIsUnknown(t *testing.T) {
	r := newRepositoryReader("", "", strings.Repeat("a", 40), map[string]treeEntry{
		"one/shared.go": {path: "one/shared.go", blob: strings.Repeat("b", 40)},
		"two/shared.go": {path: "two/shared.go", blob: strings.Repeat("c", 40)},
	})
	fact, ok := r.pathFact("shared.go:1")
	if !ok || fact.Status != FactUnknown || !strings.Contains(fact.Reason, "ambiguous") {
		t.Fatalf("fact = %+v, recognized = %v", fact, ok)
	}
}

func TestPathBasenameNonBlobIsUnknown(t *testing.T) {
	r := newRepositoryReader("", "", strings.Repeat("a", 40), map[string]treeEntry{
		"module.go": {path: "module.go", blob: strings.Repeat("b", 40), kind: "commit"},
	})
	fact, ok := r.pathFact("module.go")
	if !ok || fact.Status != FactUnknown || !strings.Contains(fact.Reason, "non-blob") {
		t.Fatalf("fact = %+v, recognized = %v", fact, ok)
	}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runFixtureGit(t, repo, "init", "-q")
	runFixtureGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widget.git")
	writeTestFile(t, filepath.Join(repo, "AGENTS.md"), strings.Join([]string{
		"# Agent guide",
		"",
		"## Canonical commands",
		"",
		"```sh",
		"make check # full validation",
		"```",
		"",
		"## Other",
	}, "\n"))
	writeTestFile(t, filepath.Join(repo, "pkg", "AGENTS.md"), strings.Join([]string{
		"## Canonical commands",
		"",
		"```sh",
		"go test ./pkg",
		"```",
	}, "\n"))
	writeTestFile(t, filepath.Join(repo, "Makefile"), "check:\n\tgo test ./...\n\ntest:\n\tgo test ./...\n")
	writeTestFile(t, filepath.Join(repo, "Dockerfile"), "FROM scratch\n")
	writeTestFile(t, filepath.Join(repo, "pkg", "thing.go"), strings.Join([]string{
		"package thing",
		"",
		"func UniqueThing() {}",
		"func Duplicate() {}",
	}, "\n"))
	writeTestFile(t, filepath.Join(repo, "other.go"), "package widget\nfunc Duplicate() {}\n")
	writeTestFile(t, filepath.Join(repo, "broken.go"), "package widget\nfunc BrokenSymbol(\n")
	runFixtureGit(t, repo, "add", ".")
	runFixtureGit(t, repo, "commit", "-q", "-m", "fixture")
	return repo
}

func runFixtureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	argv := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-C", repo}, args...)
	cmd := exec.Command("git", argv...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ticketprep Test", "GIT_AUTHOR_EMAIL=ticketprep@example.invalid",
		"GIT_COMMITTER_NAME=Ticketprep Test", "GIT_COMMITTER_EMAIL=ticketprep@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, body)
	}
	return strings.TrimSpace(string(body))
}

type ticketprepGitRecord struct {
	argv   [][]string
	guards []string
}

func installTicketprepGitRecorder(t *testing.T) func() ticketprepGitRecord {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argvRecord := filepath.Join(dir, "argv")
	guardRecord := filepath.Join(dir, "guards")
	script := "#!/bin/sh\nprintf '%s\\n' \"$(printf '%s\\037' \"$@\")\" >> " + testShellQuote(argvRecord) +
		"\nprintf '%s\\037%s\\n' \"$GIT_NO_LAZY_FETCH\" \"$GIT_NO_REPLACE_OBJECTS\" >> " + testShellQuote(guardRecord) +
		"\nexec " + testShellQuote(real) + " \"$@\"\n"
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil { //nolint:gosec // executable recorder is the test boundary
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() ticketprepGitRecord {
		raw, err := os.ReadFile(argvRecord)
		if err != nil {
			t.Fatal(err)
		}
		var record ticketprepGitRecord
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if line != "" {
				record.argv = append(record.argv, strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f"))
			}
		}
		raw, err = os.ReadFile(guardRecord)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if line != "" {
				record.guards = append(record.guards, line)
			}
		}
		return record
	}
}

func testShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertPathFact(t *testing.T, facts []PathFact, reference string, status FactStatus, resolved string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Reference == reference {
			if fact.Status != status || fact.ResolvedPath != resolved {
				t.Fatalf("path %s = %+v", reference, fact)
			}
			return
		}
	}
	t.Fatalf("no path fact for %q in %+v", reference, facts)
}

func assertSymbolFact(t *testing.T, facts []SymbolFact, reference string, status FactStatus, path string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Reference == reference {
			if fact.Status != status || fact.Path != path {
				t.Fatalf("symbol %s = %+v", reference, fact)
			}
			return
		}
	}
	t.Fatalf("no symbol fact for %q in %+v", reference, facts)
}

func instructionPaths(facts []InstructionFact) []string {
	out := make([]string, len(facts))
	for i, fact := range facts {
		out[i] = fact.Path
	}
	return out
}

func commandNames(facts []CommandFact) []string {
	out := make([]string, len(facts))
	for i, fact := range facts {
		out[i] = fact.Command
	}
	return out
}

func unknownReferences(facts []UnknownFact) []string {
	out := make([]string, len(facts))
	for i, fact := range facts {
		out[i] = fact.Reference
	}
	return out
}
