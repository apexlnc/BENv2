package harness

import (
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Legacy boot identity remains solely for pre-#234 pgid marker migration. New
// local evidence is minted and decoded by localdomain.

// bootID returns this boot's identity, resolved once.
//
// Empty means the platform would not tell us. That is deliberately *not* treated
// as "matches anything" by the reader: evidence with no boot identity cannot
// prove a legacy domain quiet, and §9.10 grants freedom only to proof.
var bootID = sync.OnceValue(bootIdentity)

// BindEvidence adapts a per-workspace sink to one Launch's identity-free sink by
// closing over the RunSpec the attempt is for (SPEC §9.10).
//
// Shared so both adapters spell the binding once. It is the whole of what keeps
// core.RunEvidenceSink's addressing intact: a runner serves every issue, so the
// spec has to be captured per attempt — hoisting this to construction would
// record every run against whichever workspace happened to be first.
//
// Nil in, nil out: a caller with no marker to upgrade (a probe, a test) must not
// be turned into one with a sink that fails.
func BindEvidence(sink core.RunEvidenceSink, spec core.RunSpec) func(core.RunEvidence) error {
	if sink == nil {
		return nil
	}
	return func(e core.RunEvidence) error { return sink(spec, e) }
}
