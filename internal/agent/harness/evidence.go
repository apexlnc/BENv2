package harness

import (
	"strconv"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Run evidence for the local process substrate (SPEC §9.10).
//
// A run marker exists so that a *later daemon* can ask whether the previous
// tenure's run is still going. Within one process that question is answered by
// the handle; across a restart there is no handle, only whatever was written
// down before the crash — so what is written down has to be enough to identify
// the group, and enough to know when it no longer means anything.
//
// A process group id alone is not enough. It is unique within one boot of one
// host and freely reused after a reboot, so a marker carrying a bare pgid can
// name an unrelated process and report a dead run as live in perpetuity — which
// under §9.10 retains a claim nothing will ever release. Pairing it with the
// boot identity makes the mismatch detectable, and a mismatch is proof: a marker
// from a previous boot cannot describe a live process.

// bootID returns this boot's identity, resolved once.
//
// Empty means the platform would not tell us. That is deliberately *not* treated
// as "matches anything" by the reader: evidence with no boot identity cannot
// prove a group is gone, and §9.10 grants freedom only to proof.
var bootID = sync.OnceValue(bootIdentity)

// localEvidence describes a live process group so a later process can find it.
func localEvidence(pgid int) core.RunEvidence {
	return core.RunEvidence{
		Scheme: core.RunEvidenceLocal,
		ID:     strconv.Itoa(pgid),
		Boot:   bootID(),
	}
}

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
