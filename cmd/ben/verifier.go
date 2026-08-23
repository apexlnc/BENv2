package main

import (
	"context"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/verify"
)

// The §9.7 evidence check and the loop that routes its answer are deliberately
// strangers. `verify` reports what the evidence says and refuses to name an
// action — an unpublished-but-clean run and an unpublished run out of turns
// produce identical evidence and route differently (§9.6), and only the
// orchestrator knows which it is holding — while the loop declares the narrow
// `Verifier` seam it consumes and imports no adapter at all (orchestrator
// deps.go). Neither may import the other without giving that up, so binding
// them is the assembly's job and the translation lives here, beside the kind
// registry, where `ben run` can assemble them.

// newVerifier builds the §9.7 checker and adapts it to the loop's seam.
func newVerifier(ws verify.Workspaces, tr verify.Tracker) (orchestrator.Verifier, error) {
	c, err := verify.New(ws, tr)
	if err != nil {
		return nil, err
	}
	return checkedVerifier{c}, nil
}

// checker is the half of *verify.Checker this adapter uses. Named here rather
// than taken concrete so the translation can be tested without standing up a
// workspace and a tracker to produce evidence it does not read.
type checker interface {
	Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (verify.Result, error)
}

type checkedVerifier struct{ checker checker }

func (v checkedVerifier) Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (orchestrator.VerifyResult, error) {
	res, err := v.checker.Verify(ctx, issue, ws)
	if err != nil {
		// The zero VerifyResult is VerdictUnknown, so a caller that ignored
		// this error still could not read the result as success (SPEC §9.7).
		return orchestrator.VerifyResult{}, err
	}
	verdict, err := routableVerdict(res.Verdict)
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	return orchestrator.VerifyResult{
		Verdict: verdict,
		PRURL:   res.PRURL,
		Detail:  res.Detail,
	}, nil
}

// routableVerdict translates one closed enum into the other.
//
// Exhaustively, and never by conversion. The two types are distinct on purpose
// — one says what the evidence is, the other says what the loop routes on —
// and today their members happen to line up numerically, so
// `orchestrator.Verdict(res.Verdict)` would compile and pass every test in
// this repo. It would also mistranslate in silence the first time either enum
// gains a member, turning `incomplete` into `contradicted` or worse, and
// nothing about the cast would look wrong when it did. That is the whole
// reason this function exists rather than a one-line conversion.
func routableVerdict(v verify.Verdict) (orchestrator.Verdict, error) {
	switch v {
	case verify.VerdictPublished:
		return orchestrator.VerdictPublished, nil
	case verify.VerdictIncomplete:
		return orchestrator.VerdictIncomplete, nil
	case verify.VerdictContradicted:
		return orchestrator.VerdictContradicted, nil
	}
	// verify.VerdictUnknown lands here with everything unrecognized, and both
	// fail closed. Unknown is "never an answer" and is only ever returned
	// alongside an error, so reaching this line with a nil error means the
	// checker broke its own contract — the same reading verify gives a tracker
	// that breaks FindPR's (ErrPRNotOpen): a broken contract has no correct
	// interpretation, so there is none to pick. Erroring parks the issue,
	// where guessing would spend an attempt, or at published be unrecoverable.
	return orchestrator.VerdictUnknown, fmt.Errorf("verification returned %s, which is not a routable verdict", v)
}
