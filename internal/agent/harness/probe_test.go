package harness

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// EvidenceGone is the one function in BEN whose wrong answer puts a second agent
// in a live worktree, so it is tested as an asymmetry rather than as a mapping:
// `true` must require proof, and every other input must yield `false`.
//
// SPEC §7.5 and §9.10: only ESRCH proves disappearance, and a boot identity that
// cannot describe a live process is the one other proof.

func TestEvidenceGoneReturnsTrueOnlyOnProof(t *testing.T) {
	here := bootID()
	if here == "" {
		t.Skip("this platform reports no boot identity; the match cases are unreachable")
	}

	for _, tc := range []struct {
		name     string
		evidence core.RunEvidence
		wantGone bool
		wantErr  bool
	}{
		{
			// The proof that needs no signal at all. A pgid is unique within one boot
			// and freely reused after a reboot, so a marker from an earlier boot
			// cannot be describing a process running now — which is what lets a
			// machine power-cycled mid-run converge with no human.
			name:     "a boot mismatch is proof the run is gone",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "1", Boot: here + "-different"},
			wantGone: true,
		},
		{
			name:     "an unrecognized scheme is unanswerable, not gone",
			evidence: core.RunEvidence{Scheme: "some-remote-substrate", ID: "abc", Boot: here},
			wantErr:  true,
		},
		{
			// Written by a build that could not read one. Absence of the identity is
			// not evidence about the process: without it a match cannot be
			// established, and §9.10 grants freedom only to proof.
			name:     "evidence with no boot identity cannot be matched",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "1"},
			wantErr:  true,
		},
		{
			name:     "a malformed id is not a process group",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "not-a-pid", Boot: here},
			wantErr:  true,
		},
		{
			// Signal 0 to group 0 addresses *our own* process group, and -1 addresses
			// everything this process may signal. Either would answer about the daemon
			// rather than about the run, and the second would also deliver a signal.
			name:     "group 0 is refused rather than probed",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "0", Boot: here},
			wantErr:  true,
		},
		{
			name:     "a negative id is refused rather than probed",
			evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "-1", Boot: here},
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone, err := EvidenceGone(tc.evidence)
			if gone != tc.wantGone {
				t.Errorf("gone = %v, want %v", gone, tc.wantGone)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}

// A live group reads as live, and the same group reads as gone once it is reaped.
// Driven against a real process because that is the whole question: a table over
// invented pids could not tell ESRCH from EPERM, and EPERM read as "gone" is the
// mistake that shares a workspace.
func TestEvidenceGoneTracksARealProcessGroup(t *testing.T) {
	here := bootID()
	if here == "" {
		t.Skip("this platform reports no boot identity")
	}

	cmd := exec.Command("sleep", "60")
	// Its own group, as every attempt gets (run.go, Setpgid) — so the probe is
	// asking about the group rather than about this test binary's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the probe target: %v", err)
	}
	pgid := cmd.Process.Pid
	evidence := core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: strconv.Itoa(pgid), Boot: here}

	gone, err := EvidenceGone(evidence)
	if err != nil {
		t.Fatalf("probing a live group: %v", err)
	}
	if gone {
		t.Fatal("a live process group reported as gone; this is the answer that shares a workspace")
	}

	// Kill the group and reap it. Reaping matters: a zombie is still a group member
	// on Linux, so a probe before Wait would legitimately answer "alive".
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the probe target: %v", err)
	}
	_ = cmd.Wait()

	gone, err = EvidenceGone(evidence)
	if err != nil {
		t.Fatalf("probing a reaped group: %v", err)
	}
	if !gone {
		t.Error("a reaped process group did not report as gone; recovery would retain the claim forever")
	}
}

// Only ESRCH proves disappearance. EPERM says the group exists and we may not
// signal it, and every unknown error fails closed the same way (SPEC §7.5) — the
// posture handle.groupAlive already has, restated here because a restart asks the
// same question from a process that owns nothing.
//
// Injected rather than provoked: a group this process genuinely may not signal
// cannot be arranged portably, and the one candidate — init's group — is `kill(-1)`,
// which means "every process you may signal" rather than "group 1".
func TestEvidenceGoneFailsClosedOnEveryErrorButESRCH(t *testing.T) {
	here := bootID()
	if here == "" {
		t.Skip("this platform reports no boot identity")
	}
	evidence := core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242", Boot: here}

	for _, tc := range []struct {
		name     string
		err      error
		wantGone bool
		wantErr  bool
	}{
		{name: "ESRCH is the proof", err: syscall.ESRCH, wantGone: true},
		{name: "EPERM means it is still there", err: syscall.EPERM, wantErr: true},
		{name: "EINVAL is unknown, so still there", err: syscall.EINVAL, wantErr: true},
		{name: "no error means alive", err: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone, err := evidenceGone(evidence, func(int, syscall.Signal) error { return tc.err })
			if gone != tc.wantGone {
				t.Errorf("gone = %v, want %v — a wrong `true` puts a second agent in a live worktree",
					gone, tc.wantGone)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}

// The probe addresses the process *group*, not its leader.
//
// Asserted on the argument, because the two spellings answer identically for every
// group whose leader and members die together — which is every group a test can
// easily build, and not the one that matters: a leader that exits while a
// descendant keeps the worktree open is the #79 shape, and a leader-only probe
// reports that workspace free.
func TestEvidenceGoneProbesTheGroupNotTheLeader(t *testing.T) {
	here := bootID()
	if here == "" {
		t.Skip("this platform reports no boot identity")
	}
	var got int
	_, err := evidenceGone(
		core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242", Boot: here},
		func(pgid int, _ syscall.Signal) error { got = pgid; return nil },
	)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// EvidenceGone negates before calling syscall.Kill, so the seam sees the
	// positive id and the negation is asserted at the call it wraps.
	if got != 4242 {
		t.Fatalf("the seam was handed pgid %d, want 4242", got)
	}

	// And the wrapper negates it, asserted against a real group whose leader is
	// gone and whose member is not. The group is built explicitly — a second
	// process joined to the first's group id — rather than through a shell's
	// backgrounding, because whether a job control-less shell puts `&` in its own
	// group is a property of the shell, and the fixture must not depend on it.
	leader := exec.Command("sleep", "60")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("starting the group leader: %v", err)
	}
	pgid := leader.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	member := exec.Command("sleep", "60")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := member.Start(); err != nil {
		t.Fatalf("starting the group member: %v", err)
	}

	// Kill and reap *only the leader*. This is the #79 shape: the process BEN
	// launched is gone, and something it spawned still holds the worktree.
	if err := leader.Process.Kill(); err != nil {
		t.Fatalf("killing the leader: %v", err)
	}
	_ = leader.Wait()
	if processAlive(pgid) {
		t.Fatal("the leader is still alive; the fixture is not exercising the case")
	}

	evidence := core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: strconv.Itoa(pgid), Boot: here}
	gone, err := EvidenceGone(evidence)
	if err != nil {
		t.Fatalf("probing a group whose leader is gone: %v", err)
	}
	if gone {
		t.Error("a group with a surviving member reported as gone; recovery would reattach a live worktree")
	}

	// And once the member goes too, the group is genuinely gone.
	if err := member.Process.Kill(); err != nil {
		t.Fatalf("killing the member: %v", err)
	}
	_ = member.Wait()
	gone, err = EvidenceGone(evidence)
	if err != nil {
		t.Fatalf("probing an empty group: %v", err)
	}
	if !gone {
		t.Error("an empty process group did not report as gone; recovery would retain the claim forever")
	}
}

// processAlive reports whether the leader pid itself is still there, so the test
// above can say it is asserting about the group rather than about the leader.
func processAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
