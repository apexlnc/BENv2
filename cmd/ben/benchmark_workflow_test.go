package main

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
)

// The four profiles differ only at the adapter boundary. This is the
// independent half of docs/BENCH.md's instruction to hold the prompt, limits,
// tracker and workspace posture fixed across cells.
func TestBenchmarkProfilesHoldEveryNonAgentInputConstant(t *testing.T) {
	t.Setenv("BEN_BENCH_REPO", "acme/canary")
	t.Setenv("BEN_BENCH_WORKSPACE", filepath.Join(t.TempDir(), "workspaces"))
	t.Setenv("BEN_BENCH_MODEL", "test-model")

	type profile struct {
		file, agent, model string
	}
	profiles := []profile{
		{"claude-code-default.md", "claude-code", ""},
		{"claude-code-model.md", "claude-code", "test-model"},
		{"codex-exec-default.md", "codex-exec", ""},
		{"codex-exec-model.md", "codex-exec", "test-model"},
	}

	var baseline config.Config
	var prompt string
	agentBaselines := map[string]config.AgentConfig{}
	for i, p := range profiles {
		path := filepath.Join(moduleRoot(t), "scripts", "benchmark", p.file)
		def, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", p.file, err)
		}
		if def.Config.Agent.Kind != p.agent || def.Config.Agent.Provider["model"] != p.model {
			t.Errorf("%s selects %s model %#v, want %s model %q", p.file,
				def.Config.Agent.Kind, def.Config.Agent.Provider["model"], p.agent, p.model)
		}
		agent := def.Config.Agent
		agent.Provider = maps.Clone(agent.Provider)
		agent.Provider["model"] = ""
		if want, ok := agentBaselines[p.agent]; ok {
			if !reflect.DeepEqual(agent, want) {
				t.Errorf("%s changes %s provider input other than model:\n got  %#v\n want %#v",
					p.file, p.agent, agent, want)
			}
		} else {
			agentBaselines[p.agent] = agent
		}

		cfg := def.Config
		cfg.Agent = config.AgentConfig{}
		if i == 0 {
			baseline, prompt = cfg, def.PromptTemplate
			continue
		}
		if !reflect.DeepEqual(cfg, baseline) {
			t.Errorf("%s changes a non-agent benchmark input:\n got  %#v\n want %#v", p.file, cfg, baseline)
		}
		if def.PromptTemplate != prompt {
			t.Errorf("%s carries a different prompt from %s", p.file, profiles[0].file)
		}
	}
}

// The documented benchmark owns the daemon process it stops. `go run` puts a
// compiler parent between the shell and BEN, so signaling/waiting for $! can
// leave the actual daemon behind to claim a later issue.
func TestBenchmarkProcedureOwnsTheDaemonProcess(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "docs", "BENCH.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{
		`go run "$CONTROL_ROOT/cmd/ben" run`,
		`go run "$CONTROL_ROOT/cmd/ben" status`,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("%s contains process-indirect launch %q", path, forbidden)
		}
	}
	for _, required := range []string{
		`go build -o "$BEN_BIN" "$CONTROL_ROOT/cmd/ben"`,
		`"$BEN_BIN" run "$WORKFLOW" &`,
		`DAEMON_PID=$!`,
		`kill -TERM "$DAEMON_PID"`,
		`wait "$DAEMON_PID"`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("%s lacks direct-process requirement %q", path, required)
		}
	}
}
