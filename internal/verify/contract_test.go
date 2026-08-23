package verify

import (
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
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
