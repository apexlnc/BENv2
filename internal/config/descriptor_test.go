package config

import (
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// AgentDescriptor (#60): what an attempt-outcome record says ran, resolved where
// the configuration, the kind registry and the provenance already meet.

func TestAgentDescriptorNamesTheKindAndTheModel(t *testing.T) {
	content := strings.Replace(validMinimal,
		"agent:\n  kind: claude-code",
		"agent:\n  kind: claude-code\n  provider:\n    model: opus", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := AgentDescriptor(def), (core.AgentDescriptor{Kind: "claude-code", Model: "opus"}); got != want {
		t.Errorf("descriptor = %+v, want %+v", got, want)
	}
}

// An omitted model is an answer, not a gap: the block names none and the
// harness's own default applies. Naming one here would put a model in the
// record that nothing ran.
func TestAgentDescriptorLeavesAnUnnamedModelEmpty(t *testing.T) {
	def, err := Load(writeWorkflow(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := AgentDescriptor(def); got.Model != "" {
		t.Errorf("model = %q, want empty — the block names none", got.Model)
	}
}

// The model goes through `config effective`'s redactor, so an env-resolved value
// is hidden here exactly as it is there. Otherwise `ben status` would acquire a
// redaction policy of its own, and it would be the one nobody maintains.
func TestAgentDescriptorRedactsAnEnvResolvedModel(t *testing.T) {
	t.Setenv("BEN_TEST_MODEL", "some-value-from-the-environment")
	content := strings.Replace(validMinimal,
		"agent:\n  kind: claude-code",
		"agent:\n  kind: claude-code\n  provider:\n    model: $BEN_TEST_MODEL", 1)
	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := AgentDescriptor(def)
	if got.Model != Redacted {
		t.Errorf("model = %q, want %q", got.Model, Redacted)
	}
	if strings.Contains(got.Model, "some-value-from-the-environment") {
		t.Error("the descriptor carries a resolved environment value")
	}
	// The rendering the redactor already governs agrees, which is the point of
	// routing through it rather than reimplementing the rule.
	if !strings.Contains(EffectiveText(def), Redacted) {
		t.Error("config effective does not redact the same value")
	}
}

// The descriptor is what the attempt log groups by (#62), so a workflow that
// names a different adapter has to produce a different descriptor.
func TestAgentDescriptorDistinguishesTheAdapters(t *testing.T) {
	one := strings.Replace(validMinimal,
		"agent:\n  kind: claude-code",
		"agent:\n  kind: claude-code\n  provider:\n    model: opus", 1)
	two := strings.Replace(validMinimal,
		"agent:\n  kind: claude-code",
		"agent:\n  kind: codex-exec\n  provider:\n    model: gpt-5", 1)

	defOne, err := Load(writeWorkflow(t, one))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defTwo, err := Load(writeWorkflow(t, two))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if AgentDescriptor(defOne) == AgentDescriptor(defTwo) {
		t.Fatal("two adapters produced one descriptor; the comparison #62 wants is not a query")
	}
}

// A kind no registry knows is unreachable through Load, and stated anyway
// because definitions are also built by hand in tests.
func TestAgentDescriptorToleratesAnUnknownKind(t *testing.T) {
	def := &WorkflowDefinition{Config: Config{Agent: AgentConfig{
		Kind:     "not-a-kind",
		Provider: map[string]any{"model": "whatever"},
	}}}
	if got, want := AgentDescriptor(def), (core.AgentDescriptor{Kind: "not-a-kind"}); got != want {
		t.Errorf("descriptor = %+v, want %+v — no kind to ask, so no model", got, want)
	}
}
