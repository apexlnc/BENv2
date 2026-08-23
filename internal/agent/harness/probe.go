package harness

import (
	"errors"
	"fmt"
	"strconv"
	"syscall"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// EvidenceGone answers SPEC §9.10's workspace precondition for a run *this
// process never started*: is the run this evidence identifies confirmed gone?
//
// It is the restart-side counterpart of handle.groupAlive, and it keeps that
// function's posture exactly. `true` is returned only on proof; a probe that
// errored for any reason but ESRCH, a scheme this build does not recognize, or a
// boot identity that cannot be established all return `false`, with an error
// where there is something to say. The asymmetry is the contract: `false` costs a
// retained claim and another tick, `true` on a live group puts a second agent in
// a worktree.
//
// Wired into the orchestrator as Config.RunGone. It is a plain function rather
// than a method on anything because there is nothing to hold: the question is
// asked of the operating system about a group id read off disk, and the daemon
// that started the run is gone by construction.
func EvidenceGone(e core.RunEvidence) (bool, error) {
	return evidenceGone(e, func(pgid int, sig syscall.Signal) error {
		// Negated: the question is about the *group*, not its leader. A leader can
		// exit while a descendant it spawned keeps the worktree open — the #79 shape
		// — and a leader-only probe answers "gone" about a workspace that is still
		// being written to.
		return syscall.Kill(-pgid, sig)
	})
}

// evidenceGone is EvidenceGone with the signal call injected, mirroring the seam
// run.go gives the signal ladder (launch.Signal).
//
// It exists because the two mistakes worth testing are not reachable through the
// real syscall on a fixture we can build: a non-ESRCH error needs a group this
// process may not signal, and "probed the leader instead of the group" gives the
// *same* answer for every group whose leader and members die together. Both are
// silent in the dangerous direction, so both get a test.
func evidenceGone(e core.RunEvidence, signal func(pgid int, sig syscall.Signal) error) (bool, error) {
	if e.Scheme != core.RunEvidenceLocal {
		// A remote substrate's session id (#46) is answerable, but not here and not
		// by signalling a pid. Unrecognized means unanswerable, which §9.10 reads as
		// possibly live.
		return false, fmt.Errorf("run evidence scheme %q is not one this build can probe", e.Scheme)
	}

	// The boot check comes first, and it is the half that makes a pid meaningful
	// at all. A process group id is unique within one boot and freely reused after
	// a reboot, so probing a recorded pgid without it can name an unrelated
	// process and report a dead run as live in perpetuity — which retains a claim
	// nothing will ever release.
	here := bootID()
	switch {
	case here == "":
		// The platform would not say. Absence of our own identity is not evidence
		// about theirs: without it a match cannot be established, and §9.10 grants
		// freedom only to proof.
		return false, errors.New("this host reports no boot identity, so a recorded process group cannot be matched against it")
	case e.Boot == "":
		// Written by a build that could not read one. Same conclusion, other side.
		return false, errors.New("the run marker carries no boot identity, so its process group cannot be matched")
	case e.Boot != here:
		// A *mismatch is proof*, and the one place absence of a live process is
		// established without signalling anything: a marker from a previous boot
		// cannot describe a process running now. This is what lets a machine that
		// was power-cycled mid-run converge with no human.
		return true, nil
	}

	pgid, err := strconv.Atoi(e.ID)
	if err != nil {
		return false, fmt.Errorf("run evidence id %q is not a process group id: %w", e.ID, err)
	}
	if pgid <= 0 {
		// Signalling 0 addresses *our own* group and -1 addresses everything the
		// caller may signal. Either would answer "alive" about this daemon rather
		// than about the run, and the second would also deliver the signal.
		return false, fmt.Errorf("run evidence id %q is not a usable process group id", e.ID)
	}

	// Signal 0 delivers nothing and asks the kernel whether any member of the
	// group is still there. Only ESRCH proves disappearance: EPERM says the group
	// exists and we may not signal it, and every unknown error fails closed the
	// same way (SPEC §7.5).
	if err := signal(pgid, syscall.Signal(0)); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		return false, fmt.Errorf("probing process group %d: %w", pgid, err)
	}
	return false, nil
}
