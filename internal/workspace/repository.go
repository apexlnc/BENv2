package workspace

import (
	"context"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// RepositoryFrom asks a tracker for the repository this strategy clones from
// and the credential that fetches it (SPEC §6.2, §10.2). It is the wiring half
// of core.RepositorySource: assembly calls it after the tracker's Ready has
// succeeded — readiness is what resolves the credential and probes the world
// (SPEC §5.7) — and hands the result to Options.Repository unchanged.
//
// The tracker is asked because only the tracker can answer. The repository
// lives in the adapter-owned provider block (SPEC §5.2.2) and the credential
// resolves the adapter's way (SPEC §5.8); a wirer that parsed either would be
// re-implementing an adapter in the core, which §5.2.5 forbids.
//
// A tracker that does not implement the contract is a named refusal rather
// than a default: the v1 strategy exists only as a clone of some repository,
// and guessing which would be inventing a fact.
func RepositoryFrom(ctx context.Context, tracker core.TrackerAdapter) (core.Repository, error) {
	src, ok := tracker.(core.RepositorySource)
	if !ok {
		return core.Repository{}, fmt.Errorf("%w: %T does not implement core.RepositorySource", ErrNoRepositorySource, tracker)
	}
	repo, err := src.Repository(ctx)
	if err != nil {
		return core.Repository{}, fmt.Errorf("resolving the tracker's repository: %w", err)
	}
	return repo, nil
}
