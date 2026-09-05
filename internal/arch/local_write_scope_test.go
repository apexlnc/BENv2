package arch

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// These checked-in profiles intentionally use Claude's unbounded local mode.
// Bind the real files to the real kind and workspace constructor: representing
// that posture as the literal root once made every one of them structurally
// incapable of starting, even though attended mode explicitly permits it.
func TestCanonicalUnboundedProfilesCanAssembleTheirWorkspace(t *testing.T) {
	root := moduleRoot(t)
	t.Setenv("BEN_BENCH_REPO", "acme/canary")
	t.Setenv("BEN_BENCH_WORKSPACE", t.TempDir())
	t.Setenv("BEN_BENCH_MODEL", "benchmark-model")

	for _, rel := range []string{
		"WORKFLOW.md",
		"scripts/benchmark/claude-code-default.md",
		"scripts/benchmark/claude-code-model.md",
	} {
		t.Run(rel, func(t *testing.T) {
			def, err := config.Load(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("load profile: %v", err)
			}
			kind, ok := registry.Runner(def.Config.Agent.Kind)
			if !ok {
				t.Fatalf("agent kind %q is not registered", def.Config.Agent.Kind)
			}
			scope, err := kind.LocalWrites(def.Config.AgentBinding(), core.LocalRuntimePaths{DaemonHomeDir: t.TempDir()})
			if err != nil {
				t.Fatalf("local write scope: %v", err)
			}
			if !scope.Unbounded || len(scope.Roots) != 0 {
				t.Fatalf("local write scope = %+v, want explicit unbounded access with no concrete root sentinel", scope)
			}

			p, err := workspace.New(workspace.Options{
				Root:          t.TempDir(),
				WorkflowKey:   def.Key,
				ScratchRoot:   t.TempDir(),
				AgentTempRoot: t.TempDir(),
				AgentWrites:   scope,
				Repository:    core.Repository{RemoteURL: "/canonical.git"},
			})
			if err != nil || p == nil {
				t.Fatalf("workspace assembly = (%v, %v), want accepted", p, err)
			}
		})
	}
}

// SRT grants these paths independently of its generated settings. Exercise
// the exact production composition from the review finding: the daemon state
// root selected by XDG_STATE_HOME sits beneath ~/.claude/debug and must fail
// before a credentialed scratch repository can be created there.
func TestSRTImplicitHomeGrantRejectsNestedDaemonState(t *testing.T) {
	root := moduleRoot(t)
	home := t.TempDir()
	t.Setenv("BEN_SMOKE_REPO", "acme/canary")
	t.Setenv("BEN_SMOKE_WORKSPACE", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "inert-smoke-key")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".claude", "debug", "state"))

	def, err := config.Load(filepath.Join(root, "scripts", "smoke-workflow.md"))
	if err != nil {
		t.Fatalf("load SRT profile: %v", err)
	}
	kind, ok := registry.Runner(def.Config.Agent.Kind)
	if !ok {
		t.Fatalf("agent kind %q is not registered", def.Config.Agent.Kind)
	}
	scope, err := kind.LocalWrites(def.Config.AgentBinding(), core.LocalRuntimePaths{DaemonHomeDir: home})
	if err != nil {
		t.Fatalf("local write scope: %v", err)
	}

	p, err := workspace.New(workspace.Options{
		Root:          t.TempDir(),
		WorkflowKey:   def.Key,
		ScratchRoot:   state.For(def.Key).Root(),
		AgentTempRoot: t.TempDir(),
		AgentWrites:   scope,
		Repository:    core.Repository{RemoteURL: "/canonical.git"},
	})
	if !errors.Is(err, workspace.ErrScratchRoot) {
		t.Fatalf("workspace assembly = (%v, %v), want ErrScratchRoot", p, err)
	}
}

// CLAUDE_CODE_TMPDIR redirects the child's TMPDIR but does not remove SRT's
// fixed /tmp/claude and /private/tmp/claude write grants. A daemon with its own
// custom TMPDIR can therefore still place state under an agent-writable tree;
// bind both runtime defaults to the real adapter and workspace refusal.
func TestSRTImplicitTempGrantsRejectNestedDaemonState(t *testing.T) {
	root := moduleRoot(t)
	t.Setenv("BEN_SMOKE_REPO", "acme/canary")
	t.Setenv("BEN_SMOKE_WORKSPACE", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "inert-smoke-key")

	def, err := config.Load(filepath.Join(root, "scripts", "smoke-workflow.md"))
	if err != nil {
		t.Fatalf("load SRT profile: %v", err)
	}
	kind, ok := registry.Runner(def.Config.Agent.Kind)
	if !ok {
		t.Fatalf("agent kind %q is not registered", def.Config.Agent.Kind)
	}
	scope, err := kind.LocalWrites(def.Config.AgentBinding(), core.LocalRuntimePaths{DaemonHomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("local write scope: %v", err)
	}

	for _, fixedRoot := range []string{"/tmp/claude", "/private/tmp/claude"} {
		t.Run(fixedRoot, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", filepath.Join(fixedRoot, "state"))
			p, err := workspace.New(workspace.Options{
				Root:          t.TempDir(),
				WorkflowKey:   def.Key,
				ScratchRoot:   state.For(def.Key).Root(),
				AgentTempRoot: t.TempDir(),
				AgentWrites:   scope,
				Repository:    core.Repository{RemoteURL: "/canonical.git"},
			})
			if !errors.Is(err, workspace.ErrScratchRoot) {
				t.Fatalf("workspace assembly = (%v, %v), want ErrScratchRoot", p, err)
			}
		})
	}
}
