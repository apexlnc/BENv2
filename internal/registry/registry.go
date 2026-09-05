// Package registry is the single source of BEN's supported adapter kinds
// (SPEC §5.7, §11; BUILD.md assembly decision 13): one table per adapter
// family, mapping the `tracker.kind` / `agent.kind` name an operator writes
// onto the package-level registration that owns it.
//
// One table, two callers, deliberately. The loader asks it whether a kind name
// is supported (SPEC §5.7); `ben config effective` asks it for the kind whose
// pure `Structural` to run (SPEC §5.8). Because both get the same answer, a
// config the loader accepts can never name an adapter the CLI then fails to
// find — the drift a name list in the loader plus a kind map in the CLI
// invited (#55).
//
// The dependency runs one way: loader → registry → adapters. Adapters take
// core-owned config values (core.TrackerConfig, core.RunnerOptions), never
// internal/config, which is what keeps that true.
//
// `ben run` performs the other half of the wiring — `Structural` → `New` →
// `Ready` — over these same tables.
package registry

import (
	"maps"
	"slices"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/codexexec"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/tracker/github"
)

// The closed v1 kind sets (SPEC §2.2, §7.7, §8.4). Unexported so the tables
// cannot be mutated at runtime: a kind set that changes under a running daemon
// would make the loader's answer and the CLI's answer disagree by construction.
var (
	trackers = map[string]core.TrackerKind{
		github.KindName: github.Kind{},
	}
	runners = map[string]core.RunnerKind{
		claudecode.KindName: claudecode.Kind{},
		codexexec.KindName:  codexexec.Kind{},
	}
	// The `credential_sources` kinds (SPEC §5.2, amendment 1). Here beside the
	// other two because they answer the same question — a name an operator wrote
	// against the registration that owns it — and because the loader and
	// `ben config effective` must agree about it for the same reason: a source
	// kind the loader accepted and the CLI could not find is the drift #55 was
	// about.
	sources = map[string]core.SourceKind{
		credential.OctoSTSKindName:       credential.OctoSTSKind{},
		credential.ProjectedOIDCKindName: credential.ProjectedOIDCKind{},
		credential.StaticKindName:        credential.StaticKind{},
	}
)

// Tracker returns the registration for a `tracker.kind` name. The bool is
// false for an unsupported name, and the kind is then nil — a caller that
// ignores it gets a nil interface rather than something to call Structural on.
func Tracker(name string) (core.TrackerKind, bool) {
	kind, ok := trackers[name]
	if !ok {
		return nil, false
	}
	return kind, true
}

// Runner returns the registration for an `agent.kind` name, with the same
// contract as Tracker.
func Runner(name string) (core.RunnerKind, bool) {
	kind, ok := runners[name]
	if !ok {
		return nil, false
	}
	return kind, true
}

// Source returns the registration for a `credential_sources` kind, with the
// same contract as Tracker.
func Source(name string) (core.SourceKind, bool) {
	kind, ok := sources[name]
	if !ok {
		return nil, false
	}
	return kind, true
}

// TrackerNames lists the supported `tracker.kind` values, sorted, for the
// loader's refusal message. Each call returns a fresh slice: a caller holding
// the table's own backing array could reorder the closed set.
func TrackerNames() []string { return slices.Sorted(maps.Keys(trackers)) }

// RunnerNames lists the supported `agent.kind` values, on the same contract.
func RunnerNames() []string { return slices.Sorted(maps.Keys(runners)) }

// SourceNames lists the supported `credential_sources` kinds, on the same
// contract.
func SourceNames() []string { return slices.Sorted(maps.Keys(sources)) }
