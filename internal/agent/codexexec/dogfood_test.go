// An external test package, because this file imports the loader: internal/config
// asks internal/registry which kinds are supported, and the registry imports this
// adapter, so an in-package test reaching for the loader is an import cycle (#55).
package codexexec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/codexexec"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// BEN's own WORKFLOW.md must satisfy this adapter, not just the config loader:
// `make workflow-check` validates the core schema, but the provider block is
// opaque to it, so only the adapter can say whether the dogfood config would
// actually dispatch. It skips while the dogfood workflow names another kind —
// and stops skipping the day someone points BEN at this one, which is exactly
// when a required provider key would silently rot it (AGENTS.md, Dogfooding).
func TestDogfoodWorkflowProviderBlock(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "WORKFLOW.md")
	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	if def.Config.Agent.Kind != codexexec.KindName {
		t.Skipf("dogfood workflow uses agent kind %q, not this adapter", def.Config.Agent.Kind)
	}
	// Through the kind, exactly as `ben config effective` reaches it.
	var kind core.RunnerKind = codexexec.Kind{}
	// The whole agent configuration, since that is what the kind is asked
	// (SPEC §5.7): the publish credential is core-owned but the §7.6 reservation
	// between it and this block is the adapter's to refuse, so a dogfood file that
	// respelled it would fail here rather than at `ben run`.
	cfg := core.AgentConfig{Provider: def.Config.Agent.Provider, Publish: def.Config.Publish.Credential()}
	if err := kind.Structural(cfg); err != nil {
		t.Errorf("WORKFLOW.md agent configuration is not valid for this adapter: %v", err)
	}
}

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
