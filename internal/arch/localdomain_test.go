package arch

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const localDomainPkg = "internal/agent/localdomain"
const localDomainOwner = "internal/agent/harness"

// #234 activates the #274 mechanism through one owner. Keeping the importer
// exact prevents an adapter, the orchestrator, or assembly from growing a
// second interpretation of local-domain evidence or teardown.
func TestLocalDomainProviderHasOneProductionOwner(t *testing.T) {
	graph, err := importGraph(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graph[localDomainPkg]; !ok {
		t.Fatalf("%s is absent from the import graph; this test is checking nothing", localDomainPkg)
	}
	var importers []string
	for pkg, imports := range graph {
		if pkg == localDomainPkg {
			continue
		}
		for _, imported := range imports {
			if imported == localDomainPkg {
				importers = append(importers, pkg)
				break
			}
		}
	}
	sort.Strings(importers)
	if want := []string{localDomainOwner}; !slices.Equal(importers, want) {
		t.Errorf("packages importing %s = %v, want its single owner %v", localDomainPkg, importers, want)
	}
	if route := pathTo(graph, daemonPkg, localDomainPkg); strings.Join(route, " -> ") !=
		"cmd/ben -> internal/agent/harness -> internal/agent/localdomain" {
		t.Errorf("production local-domain route = %v", route)
	}
}

func TestLocalDomainOwnershipAuditHasANegativeControl(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/activation\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "cmd/ben/main.go"),
		"package main\n\nimport _ \"example.com/activation/internal/agent/harness\"\n")
	writeFile(t, filepath.Join(root, "internal/agent/harness/domain.go"),
		"package harness\n\nimport _ \"example.com/activation/internal/agent/localdomain\"\n")
	writeFile(t, filepath.Join(root, "internal/agent/localdomain/doc.go"), "package localdomain\n")
	graph, err := importGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	route := pathTo(graph, daemonPkg, localDomainPkg)
	if strings.Join(route, " -> ") != "cmd/ben -> internal/agent/harness -> internal/agent/localdomain" {
		t.Fatalf("activation route = %v", route)
	}
}

// The POSIX process group used by adapter contract fixtures must never become
// a production fallback. It lives in agenttest, and no daemon import path may
// reach that package.
func TestProcessGroupTestDomainIsOutsideProduction(t *testing.T) {
	reachable, err := productionReachablePackages(moduleRoot(t), daemonPkg)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(reachable, "internal/agent/agenttest") {
		t.Error("production reaches the process-group test domain in internal/agent/agenttest")
	}
}
