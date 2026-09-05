package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const ticketprepPkg = "internal/ticketprep"

const (
	ticketprepSkillProcedure   = "docs/runbooks/agent-skills/prep-ticket.md"
	ticketprepSkillDescription = "Produce a bounded, read-only readiness packet for one GitHub issue using exact issue/repository binding, committed Git facts, trust-separated advice, and visible freshness. Use only for an explicit invocation of the repository's prep-ticket skill; do not auto-select it for general planning, implementation, issue-editing, or approval work."
	ticketprepSkillStubBody    = "Read `docs/runbooks/agent-skills/prep-ticket.md` and execute it in the current\n" +
		"session.\n\n" +
		"That file is the canonical, harness-neutral procedure for this skill. This stub\n" +
		"exists only to register the skill; never restate or fork the steps here."
)

var ticketprepDeps = map[string]string{
	"internal/gitcmd":    "every Git invocation carries BEN's no-maintenance argv and isolated environment (#154, #222)",
	"internal/gitremote": "origin identity rejects credentials and transport helpers before capture (#52, #222)",
}

func TestDaemonCannotReachTicketprep(t *testing.T) {
	graph, err := importGraph(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graph[ticketprepPkg]; !ok {
		t.Fatalf("%s is not in the import graph; this test is checking nothing", ticketprepPkg)
	}
	if path := pathTo(graph, daemonPkg, ticketprepPkg); path != nil {
		t.Errorf("%s reaches %s: %s; #222 artifacts are developer advice and cannot become daemon authority",
			daemonPkg, ticketprepPkg, strings.Join(path, " -> "))
	}
}

func TestOnlyTicketprepCommandImportsKernel(t *testing.T) {
	graph, err := importGraph(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	got := importersOf(graph, ticketprepPkg)
	want := []string{"cmd/ticketprep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("packages importing %s = %v, want %v", ticketprepPkg, got, want)
	}
}

func TestTicketprepCommandStaysOfflineAndDeveloperOnly(t *testing.T) {
	imports, err := packageImports(moduleRoot(t), "cmd/ticketprep", false)
	if err != nil {
		t.Fatal(err)
	}
	seenKernel := false
	for _, imported := range imports {
		if relative, local := moduleRelative(imported); local {
			if relative != ticketprepPkg {
				t.Errorf("cmd/ticketprep imports %s; the developer command may bind only its kernel", relative)
			}
			seenKernel = seenKernel || relative == ticketprepPkg
			continue
		}
		if !isStdlib(imported) {
			t.Errorf("cmd/ticketprep imports third-party package %q", imported)
		}
		if imported == "os/exec" || imported == "net" || strings.HasPrefix(imported, "net/") {
			t.Errorf("cmd/ticketprep imports %q; model, forge, and network execution stay outside the offline command", imported)
		}
	}
	if !seenKernel {
		t.Fatal("cmd/ticketprep does not import internal/ticketprep; the command boundary test is inert")
	}
}

func TestTicketprepImportsOnlyGitFactBoundaries(t *testing.T) {
	imports, err := packageImports(moduleRoot(t), ticketprepPkg, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, imported := range imports {
		if imported == "net" || strings.HasPrefix(imported, "net/") && imported != "net/url" {
			t.Errorf("%s imports network-capable standard package %q; only net/url syntax is permitted", ticketprepPkg, imported)
		}
		relative, ok := moduleRelative(imported)
		if !ok {
			continue
		}
		seen[relative] = true
		if _, allowed := ticketprepDeps[relative]; !allowed {
			t.Errorf("%s imports %s, outside its deterministic Git fact surface %v", ticketprepPkg, relative, sortedKeys(ticketprepDeps))
		}
	}
	for dependency, reason := range ticketprepDeps {
		if !seen[dependency] {
			t.Errorf("permitted dependency %s is not imported; its rule is inert", dependency)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("permitted dependency %s has no recorded reason", dependency)
		}
		if _, err := os.Stat(filepath.Join(moduleRoot(t), filepath.FromSlash(dependency))); err != nil {
			t.Errorf("permitted dependency %s is not a package here: %v", dependency, err)
		}
	}
}

func TestDaemonCannotNameTicketprepArtifacts(t *testing.T) {
	root := moduleRoot(t)
	packages, err := productionReachablePackages(root, daemonPkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) == 0 || packages[0] != daemonPkg {
		t.Fatalf("production package walk = %v; daemon root is missing", packages)
	}
	for _, pkg := range packages {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			filename := filepath.Join(root, filepath.FromSlash(pkg), entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Errorf("unquote %s string literal: %v", filename, err)
					return true
				}
				if isTicketprepArtifactReference(value) {
					t.Errorf("daemon-reachable %s/%s names ticketprep artifact %q", pkg, entry.Name(), value)
				}
				return true
			})
		}
	}
}

func productionReachablePackages(root, start string) ([]string, error) {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imports, err := packageImports(root, pkg, false)
		if err != nil {
			return nil, err
		}
		for _, imported := range imports {
			local, ok := moduleRelative(imported)
			if !ok || seen[local] {
				continue
			}
			seen[local] = true
			queue = append(queue, local)
		}
	}
	packages := make([]string, 0, len(seen))
	for pkg := range seen {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages, nil
}

func isTicketprepArtifactReference(value string) bool {
	for _, fragment := range []string{
		"docs/TICKETPREP.md",
		"docs/ticketprep",
		".agents/skills/prep-ticket",
		".claude/skills/prep-ticket",
	} {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func TestTicketprepBoundaryChecksHaveSyntheticTriggers(t *testing.T) {
	graph := map[string][]string{
		"cmd/ben":               {"internal/orchestrator"},
		"internal/orchestrator": {ticketprepPkg},
		ticketprepPkg:           nil,
		"cmd/ticketprep":        {ticketprepPkg},
		"internal/accidental":   {ticketprepPkg},
	}
	if path := pathTo(graph, daemonPkg, ticketprepPkg); strings.Join(path, " -> ") != "cmd/ben -> internal/orchestrator -> internal/ticketprep" {
		t.Fatalf("reachability trigger returned %v", path)
	}
	if got := importersOf(graph, ticketprepPkg); strings.Join(got, ",") != "cmd/ticketprep,internal/accidental,internal/orchestrator" {
		t.Fatalf("importer trigger returned %v", got)
	}
	if !isTicketprepArtifactReference("docs/ticketprep/pilot-152/packet.json") || isTicketprepArtifactReference("docs/REMOTE.md") {
		t.Fatal("ticketprep artifact reference detector does not distinguish its synthetic trigger")
	}
}

func TestTicketprepSkillHasNativeExplicitOnlyRegistrations(t *testing.T) {
	root := moduleRoot(t)
	procedure, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ticketprepSkillProcedure)))
	if err != nil {
		t.Fatalf("read canonical skill procedure: %v", err)
	}
	if strings.TrimSpace(string(procedure)) == "" {
		t.Fatal("canonical skill procedure is empty")
	}

	registrations := []struct {
		name       string
		path       string
		wantFields map[string]string
	}{
		{
			name: "Codex",
			path: ".agents/skills/prep-ticket/SKILL.md",
			wantFields: map[string]string{
				"name":        "prep-ticket",
				"description": ticketprepSkillDescription,
			},
		},
		{
			name: "Claude Code",
			path: ".claude/skills/prep-ticket/SKILL.md",
			wantFields: map[string]string{
				"name":                     "prep-ticket",
				"description":              ticketprepSkillDescription,
				"disable-model-invocation": "true",
				"argument-hint":            "[issue number or URL]",
			},
		},
	}

	for _, registration := range registrations {
		t.Run(registration.name, func(t *testing.T) {
			fields, body := readSkillRegistration(t, filepath.Join(root, filepath.FromSlash(registration.path)))
			if !reflect.DeepEqual(fields, registration.wantFields) {
				t.Errorf("frontmatter = %#v, want %#v", fields, registration.wantFields)
			}
			if body != ticketprepSkillStubBody {
				t.Errorf("stub body drifted from the canonical pointer registration:\n%s", body)
			}
		})
	}

	openAI, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "prep-ticket", "agents", "openai.yaml"))
	if err != nil {
		t.Fatalf("read Codex skill policy: %v", err)
	}
	if got, ok := nestedYAMLScalar(string(openAI), "policy", "allow_implicit_invocation"); !ok || got != "false" {
		t.Errorf("Codex allow_implicit_invocation = %q, %v; want false, true", got, ok)
	}
}

func readSkillRegistration(t *testing.T, path string) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skill registration: %v", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) < 3 || lines[0] != "---" {
		t.Fatalf("skill registration must open with YAML frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatalf("skill registration has no closing frontmatter delimiter")
	}

	fields := make(map[string]string, end-1)
	for _, line := range lines[1:end] {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			t.Fatalf("invalid scalar frontmatter line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if _, duplicate := fields[key]; duplicate {
			t.Fatalf("duplicate frontmatter key %q", key)
		}
		fields[key] = value
	}
	return fields, strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
}

func nestedYAMLScalar(raw, section, key string) (string, bool) {
	inSection := false
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := len(line) > len(strings.TrimLeft(line, " \t"))
		if !indented {
			inSection = trimmed == section+":"
			continue
		}
		if !inSection {
			continue
		}
		foundKey, value, ok := strings.Cut(trimmed, ":")
		if ok && foundKey == key {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func importersOf(graph map[string][]string, target string) []string {
	var importers []string
	for pkg, dependencies := range graph {
		if pkg == target {
			continue
		}
		for _, dependency := range dependencies {
			if dependency == target {
				importers = append(importers, pkg)
				break
			}
		}
	}
	sort.Strings(importers)
	return importers
}
