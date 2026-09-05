package arch

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// The real smoke run is credentialed and deliberately outside make check, but
// its path assembly is not. Drive the script's own layout function, the real
// workflow loader, the selected adapter's writable-set report and the real
// workspace constructor together so a future overlap fails before a canary run.
func TestSmokeRuntimeKeepsScratchOutsideTheAgentWriteSet(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "smoke.sh"), "--check-runtime-layout", work)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke layout self-check: %v: %s", err, out)
	}
	paths := make(map[string]string)
	scan := bufio.NewScanner(strings.NewReader(string(out)))
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), "=")
		if ok {
			paths[key] = value
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workspace", "state_home", "agent_tmp"} {
		if !filepath.IsAbs(paths[key]) {
			t.Fatalf("smoke %s = %q, want an absolute path", key, paths[key])
		}
	}

	t.Setenv("BEN_SMOKE_REPO", "acme/canary")
	t.Setenv("BEN_SMOKE_WORKSPACE", paths["workspace"])
	t.Setenv("ANTHROPIC_API_KEY", "inert-smoke-key")
	def, err := config.Load(filepath.Join(root, "scripts", "smoke-workflow.md"))
	if err != nil {
		t.Fatalf("load smoke workflow: %v", err)
	}
	kind, ok := registry.Runner(def.Config.Agent.Kind)
	if !ok {
		t.Fatalf("smoke agent kind %q is not registered", def.Config.Agent.Kind)
	}
	home := t.TempDir()
	writeScope, err := kind.LocalWrites(def.Config.AgentBinding(), core.LocalRuntimePaths{DaemonHomeDir: home})
	if err != nil {
		t.Fatalf("smoke agent write scope: %v", err)
	}

	t.Setenv("XDG_STATE_HOME", paths["state_home"])
	p, err := workspace.New(workspace.Options{
		Root:          paths["workspace"],
		WorkflowKey:   def.Key,
		ScratchRoot:   state.For(def.Key).Root(),
		AgentTempRoot: paths["agent_tmp"],
		AgentWrites:   writeScope,
		Repository:    core.Repository{RemoteURL: "/canonical.git"},
	})
	if err != nil {
		t.Fatalf("smoke runtime path assembly: %v", err)
	}
	if p == nil {
		t.Fatal("smoke runtime path assembly returned no workspace provider")
	}
}
