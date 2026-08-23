package orchestrator

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// The loop declares narrow, consumer-defined dependencies rather than taking
// the full adapter contracts (SPEC §8.2, §7.1, §6.1). That is only worth
// anything if the real adapters still satisfy them, so assert it here — at
// compile time, against the core interfaces the shipped adapters implement.
//
// Production code deliberately programs against core.AgentRunner and
// core.TrackerAdapter rather than importing a concrete adapter. A
// package-internal integration test may bind one to those public seams;
// claim_assignee_reload_test.go does so to cover the config-to-identity join.
// That exception does not enter the production import graph, and internal/arch
// does not enforce test-only adapter imports.
var (
	_ Tracker = (core.TrackerAdapter)(nil)
	_ Runner  = (core.AgentRunner)(nil)
	// The loop also consumes the atomic pre-hook local-facts prepare,
	// deliberately absent from the three-method strategy interface. Assert the
	// shipped provider itself.
	_ Workspaces = (*workspace.Provider)(nil)

	_ Tracker    = (*fake.Tracker)(nil)
	_ Runner     = (*fake.Runner)(nil)
	_ Workspaces = (*fake.Workspaces)(nil)
	_ Clock      = (*fake.Clock)(nil)
)

// TestSeamsAcceptTheRealContracts exists so the assertions above are reported
// as a test rather than only as a build failure.
func TestSeamsAcceptTheRealContracts(t *testing.T) {
	t.Log("the shipped tracker, runner and workspace contracts satisfy the loop's consumer-defined seams")
}
