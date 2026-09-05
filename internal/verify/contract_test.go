package verify

import (
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/mirror"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// Consumer-defined seams cost nothing only if the real implementations satisfy
// them. These make that a compile-time fact rather than a claim B11's wiring
// would be the first to test.
//
// The tracker seam asserts against core.TrackerAdapter rather than the GitHub
// adapter: verification must not depend on a specific provider (SPEC §3.6).
// The workspace seam has no core interface to name — PublishFacts is
// deliberately off §6.1's closed provider seam — so it names the concrete
// provider, and does so in _test.go so the production import graph keeps the
// same independence.
var (
	_ Tracker    = (core.TrackerAdapter)(nil)
	_ Workspaces = (*workspace.Provider)(nil)
)

// The v2 seams, on the same terms. The fact source names the concrete mirror
// because there is no core interface for it — a daemon-side store is not a
// workspace strategy — and the forge seam names core.RemotePRSource rather than
// any adapter, because v2 verification must no more depend on a specific
// provider than v1 does (SPEC §3.6).
//
// The fake is asserted beside the real one deliberately. Every test of the v2
// checker is written against it, so "the fake satisfies the same seam" is the
// assumption those tests rest on, and internal/mirror/mirrortest is what holds
// the two to the same *behaviour* as well as the same shape.
var (
	_ RemoteFactSource = (*mirror.Mirror)(nil)
	_ RemoteFactSource = (*fake.Mirror)(nil)
	_ RemotePRs        = (core.RemotePRSource)(nil)
	_ RemotePRs        = (*fake.Forge)(nil)
)
