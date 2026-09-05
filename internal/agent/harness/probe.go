package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/localdomain"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// EvidenceGone answers SPEC §9.10's workspace precondition for a run this
// process never started. New Linux evidence delegates to the same read-only
// predicate as a live handle; legacy pgid evidence is retained on the same boot
// because process-group absence cannot account for a setsid descendant.
func EvidenceGone(e core.RunEvidence) (bool, error) {
	switch e.Scheme {
	case localdomain.EvidenceScheme:
		status, err := processLocalDomain.manager.Recover(context.Background(), localdomain.Evidence{
			Scheme: e.Scheme,
			Boot:   e.Boot,
			ID:     e.ID,
		})
		return status == localdomain.TerminationConfirmed, err
	case core.RunEvidenceLocal:
		return legacyEvidenceGone(e)
	default:
		return false, fmt.Errorf("run evidence scheme %q is not one this build can probe", e.Scheme)
	}
}

func legacyEvidenceGone(e core.RunEvidence) (bool, error) {
	here := bootID()
	switch {
	case here == "":
		return false, errors.New("this host reports no boot identity, so legacy process-group evidence cannot be matched")
	case e.Boot == "":
		return false, errors.New("the legacy run marker carries no boot identity")
	case e.Boot != here:
		return true, nil
	}
	pgid, err := strconv.Atoi(e.ID)
	if err != nil || pgid <= 0 {
		return false, fmt.Errorf("legacy run evidence id %q is not a usable process group id", e.ID)
	}
	// Deliberately no kill(0) probe: ESRCH for the recorded group cannot prove
	// that no member escaped it before the previous daemon disappeared.
	return false, nil
}
