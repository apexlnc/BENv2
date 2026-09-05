//go:build linux

package localdomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	systemdProofGate       = "BEN_LOCALDOMAIN_SYSTEMD_PROOF"
	systemdProofModeFile   = ".ben-systemd-proof-mode"
	systemdProofStateFile  = "proof-state.json"
	systemdProofMarkerFile = "run-marker.json"
	systemdProofReadyFile  = "phase-one-ready"
	systemdProofResultFile = "proof-result.json"
	systemdProofResidue    = "run-dddddddddddddddddddddddddddddddd"
)

type systemdProofState struct {
	Evidence               Evidence `json:"evidence"`
	UnitPID                uint32   `json:"unit_pid"`
	InvocationID           string   `json:"invocation_id"`
	SupervisorPID          uint32   `json:"supervisor_pid"`
	SupervisorStart        uint64   `json:"supervisor_start"`
	DescendantPID          uint32   `json:"descendant_pid"`
	DescendantStart        uint64   `json:"descendant_start"`
	CleanLeafRemoved       bool     `json:"clean_leaf_removed"`
	ReplacementUnconfirmed bool     `json:"replacement_unconfirmed"`
}

type systemdProofResult struct {
	Refused                  bool   `json:"refused,omitempty"`
	Error                    string `json:"error,omitempty"`
	RecoveryConfirmed        bool   `json:"recovery_confirmed,omitempty"`
	MarkerPresentBeforeReady bool   `json:"marker_present_before_ready,omitempty"`
	MarkerPresentAfterReady  bool   `json:"marker_present_after_ready,omitempty"`
	AttemptLeafRemoved       bool   `json:"attempt_leaf_removed,omitempty"`
	StartupResidueSurvived   bool   `json:"startup_residue_survived,omitempty"`
	StartupResiduePresent    bool   `json:"startup_residue_present_before_ready,omitempty"`
	StartupResidueRemoved    bool   `json:"startup_residue_removed,omitempty"`
	RestartPID               uint32 `json:"restart_pid,omitempty"`
	RestartInvocationID      string `json:"restart_invocation_id,omitempty"`
	SupervisorIdentityGone   bool   `json:"supervisor_identity_gone,omitempty"`
	DescendantIdentityGone   bool   `json:"descendant_identity_gone,omitempty"`
}

// TestShippedSystemdCrashRestart has two entry paths. Ordinary test runs skip
// it. The gated root invocation installs exact copies of deploy/ben.service
// with only its documented paths and startup gate adjusted. systemd then runs
// this same test binary as User=ben; the mode file in WorkingDirectory selects
// the participant path without a test-only delegation drop-in.
func TestShippedSystemdCrashRestart(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(workingDirectory, systemdProofModeFile)); err == nil {
		switch strings.TrimSpace(string(raw)) {
		case "nondelegated":
			runNondelegatedSystemdParticipant(t, workingDirectory)
		case "crash-restart":
			runCrashRestartSystemdParticipant(t, workingDirectory)
		default:
			t.Fatalf("unknown systemd proof mode %q", strings.TrimSpace(string(raw)))
		}
		return
	}

	if os.Getenv(systemdProofGate) != "1" {
		t.Skipf("set %s=1 and run as root on a supported shipped-systemd host", systemdProofGate)
	}
	if os.Geteuid() != 0 {
		t.Fatalf("%s=1 requires a root test launcher so it can install a disposable /run unit", systemdProofGate)
	}
	runShippedSystemdProof(t)
}

func runNondelegatedSystemdParticipant(t *testing.T, stateDirectory string) {
	manager := New(Options{})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := manager.Ready(ctx)
	result := systemdProofResult{Refused: err != nil}
	if err != nil {
		result.Error = err.Error()
	}
	writeSystemdProofJSON(t, filepath.Join(stateDirectory, systemdProofResultFile), result)
}

func runCrashRestartSystemdParticipant(t *testing.T, stateDirectory string) {
	statePath := filepath.Join(stateDirectory, systemdProofStateFile)
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		runSystemdPhaseOne(t, stateDirectory)
		return
	} else if err != nil {
		t.Fatal(err)
	}
	runSystemdPhaseTwo(t, stateDirectory)
}

func systemdProofTimings() Timings {
	return Timings{
		InterruptGrace:  500 * time.Millisecond,
		DiscardGrace:    150 * time.Millisecond,
		KillGrace:       2 * time.Second,
		PollInterval:    10 * time.Millisecond,
		CleanupRetry:    20 * time.Millisecond,
		CleanupPass:     time.Second,
		CleanupNodes:    1024,
		CleanupFailures: 5,
	}
}

func runSystemdPhaseOne(t *testing.T, stateDirectory string) {
	invocationID := os.Getenv("INVOCATION_ID")
	if invocationID == "" {
		t.Fatal("systemd did not provide INVOCATION_ID to the fixture service")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Executable: executable, Timings: systemdProofTimings()})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("shipped-unit Ready: %v", err)
	}

	clean := startRealRun(t, ctx, manager, []string{"/bin/true"})
	if exit := awaitProvider(t, clean); !exit.Success() {
		t.Fatalf("clean provider exit = %+v", exit)
	}
	awaitTermination(t, func() (Termination, error) { return clean.Handle.Probe(ctx) }, TerminationConfirmed)
	cleanIdentity, err := ParseEvidence(clean.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	waitSystemdLeafAbsent(t, manager, cleanIdentity.Name)

	markerPath := filepath.Join(stateDirectory, systemdProofMarkerFile)
	input, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, Launch{
		Argv: []string{executable, realSetsidArg}, Env: []string{}, Dir: "/",
		Stdin: input, Stdout: os.Stderr, Stderr: os.Stderr,
		OnDomain: func(evidence Evidence) error {
			return writeSystemdProofJSONError(markerPath, evidence)
		},
	})
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	if exit := awaitProvider(t, live); !exit.Success() {
		t.Fatalf("setsid provider exit = %+v", exit)
	}
	identity, err := ParseEvidence(live.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	descendantPID, descendantStart := waitSystemdDescendant(t, manager, identity)
	if got, err := live.Handle.Probe(ctx); got != TerminationUnconfirmed {
		t.Fatalf("Probe with live setsid descendant = (%v, %v), want unconfirmed", got, err)
	}

	// The identity-mismatch control represents a manually replaced attempt leaf
	// while the exact old PID-namespace supervisor remains live. Supervisor
	// liveness must veto the otherwise-empty/replaced cgroup observation.
	replaced := identity
	if replaced.Leaf.Inode == ^uint64(0) {
		replaced.Leaf.Inode--
	} else {
		replaced.Leaf.Inode++
	}
	replacementEvidence, err := EncodeEvidence(replaced)
	if err != nil {
		t.Fatal(err)
	}
	replacementStatus, _ := manager.Recover(ctx, replacementEvidence)
	if replacementStatus != TerminationUnconfirmed {
		t.Fatalf("Recover with live supervisor and replaced leaf = %v, want unconfirmed", replacementStatus)
	}
	if err := unix.Mkdirat(int(manager.runs.Fd()), systemdProofResidue, 0o700); err != nil {
		t.Fatalf("plant crash cleanup residue: %v", err)
	}

	state := systemdProofState{
		Evidence:               live.Evidence,
		UnitPID:                uint32(os.Getpid()),
		InvocationID:           invocationID,
		SupervisorPID:          identity.PID,
		SupervisorStart:        identity.StartTicks,
		DescendantPID:          descendantPID,
		DescendantStart:        descendantStart,
		CleanLeafRemoved:       true,
		ReplacementUnconfirmed: true,
	}
	writeSystemdProofJSON(t, filepath.Join(stateDirectory, systemdProofStateFile), state)
	if err := os.WriteFile(filepath.Join(stateDirectory, systemdProofReadyFile), []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The root harness SIGKILLs this unit's MainPID. Deliberately do not close
	// manager: its abrupt loss is the crash whose startup sweep is under test.
	for {
		time.Sleep(time.Hour)
	}
}

func runSystemdPhaseTwo(t *testing.T, stateDirectory string) {
	var state systemdProofState
	readSystemdProofJSON(t, filepath.Join(stateDirectory, systemdProofStateFile), &state)
	markerPath := filepath.Join(stateDirectory, systemdProofMarkerFile)
	result := systemdProofResult{}
	result.MarkerPresentBeforeReady = fileExists(markerPath)
	result.RestartPID = uint32(os.Getpid())
	result.RestartInvocationID = os.Getenv("INVOCATION_ID")
	result.StartupResidueSurvived = ensureSystemdStartupResidue(t)
	result.StartupResiduePresent = true

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Executable: executable, Timings: systemdProofTimings()})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("restart Ready: %v", err)
	}
	result.MarkerPresentAfterReady = fileExists(markerPath)
	identity, err := ParseEvidence(state.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	result.AttemptLeafRemoved = systemdLeafAbsent(manager, identity.Name)
	result.StartupResidueRemoved = systemdLeafAbsent(manager, strings.TrimPrefix(systemdProofResidue, attemptPrefix))
	status, recoverErr := manager.Recover(ctx, state.Evidence)
	result.RecoveryConfirmed = status == TerminationConfirmed && recoverErr == nil
	result.SupervisorIdentityGone = !systemdProcessMatches(manager, state.SupervisorPID, state.SupervisorStart)
	result.DescendantIdentityGone = !systemdProcessMatches(manager, state.DescendantPID, state.DescendantStart)
	if result.RecoveryConfirmed {
		if err := os.Remove(markerPath); err != nil {
			t.Fatal(err)
		}
	} else if recoverErr != nil {
		result.Error = recoverErr.Error()
	}
	writeSystemdProofJSON(t, filepath.Join(stateDirectory, systemdProofResultFile), result)
}

func waitSystemdDescendant(t *testing.T, manager *Manager, identity Identity) (uint32, uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pid uint32
		var start uint64
		err := manager.forEachPIDDomainProcess(identity.PIDNS, func(candidate uint32, proc *os.File) error {
			if candidate == identity.PID {
				return nil
			}
			state, ticks, err := readProcStat(int(proc.Fd()))
			if err == nil && state != 'Z' && state != 'X' {
				pid, start = candidate, ticks
			}
			return nil
		})
		if err == nil && pid != 0 {
			return pid, start
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("setsid descendant never appeared in the attempt PID namespace")
	return 0, 0
}

func waitSystemdLeafAbsent(t *testing.T, manager *Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if systemdLeafAbsent(manager, name) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("attempt leaf run-%s was not removed by the janitor", name)
}

func systemdLeafAbsent(manager *Manager, name string) bool {
	fd, err := unix.Openat(int(manager.runs.Fd()), attemptPrefix+name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == nil {
		unix.Close(fd)
		return false
	}
	return errors.Is(err, unix.ENOENT)
}

// ensureSystemdStartupResidue reports whether the phase-one residue survived
// systemd's own service-cgroup teardown. Some systemd releases prune delegated
// children and some leave them for the delegate. In the former case, recreate
// the same canonical empty crash artifact before Ready so this shipped-policy
// restart still exercises the provider's startup sweeper.
func ensureSystemdStartupResidue(t *testing.T) bool {
	t.Helper()
	root, _, _, err := discoverDelegatedRoot("")
	if err != nil {
		t.Fatalf("discover restarted delegated root: %v", err)
	}
	defer root.Close()
	runs, err := ensureCgroup(root, runsCgroup, true)
	if err != nil {
		t.Fatalf("open restarted attempt root: %v", err)
	}
	defer runs.Close()
	leaf, err := openAtDir(int(runs.Fd()), systemdProofResidue, false)
	if err == nil {
		leaf.Close()
		return true
	}
	if !isNotExist(err) {
		t.Fatalf("observe crash cleanup residue: %v", err)
	}
	if err := unix.Mkdirat(int(runs.Fd()), systemdProofResidue, 0o700); err != nil {
		t.Fatalf("restore crash cleanup residue: %v", err)
	}
	return false
}

func systemdProcessMatches(manager *Manager, pid uint32, start uint64) bool {
	proc, err := openProcPID(int(manager.proc.Fd()), pid)
	if err != nil {
		return false
	}
	defer proc.Close()
	state, gotStart, err := readProcStat(int(proc.Fd()))
	return err == nil && state != 'Z' && state != 'X' && gotStart == start
}

func runShippedSystemdProof(t *testing.T) {
	for _, command := range []string{"systemctl", "systemd-analyze", "uname"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("%s is required: %v", command, err)
		}
	}
	serviceUser, err := user.Lookup("ben")
	if err != nil {
		t.Fatalf("the checked-in unit's User=ben must exist: %v", err)
	}
	uid, err := strconv.Atoi(serviceUser.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(serviceUser.Gid)
	if err != nil {
		t.Fatal(err)
	}
	unitPath := findShippedUnit(t)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "Delegate=yes\n") {
		t.Fatal("checked-in deploy/ben.service is missing Delegate=yes")
	}
	if !strings.Contains(string(unit), "RestrictNamespaces=no\n") {
		t.Fatal("checked-in deploy/ben.service must leave clone3 namespace creation unrestricted")
	}
	t.Logf("systemd: %s", firstOutputLine(t, "systemctl", "--version"))
	t.Logf("kernel: %s", firstOutputLine(t, "uname", "-r"))

	t.Run("pre-change unit refuses without delegation", func(t *testing.T) {
		fixture := installSystemdProofUnit(t, unit, uid, gid, false, "nondelegated")
		fixture.start(t)
		var result systemdProofResult
		fixture.waitJSON(t, systemdProofResultFile, &result, 45*time.Second)
		if !result.Refused {
			t.Fatalf("nondelegated shipped-unit fixture did not refuse readiness: %+v", result)
		}
	})

	t.Run("abrupt MainPID restart classifies and sweeps", func(t *testing.T) {
		fixture := installSystemdProofUnit(t, unit, uid, gid, true, "crash-restart")
		fixture.start(t)
		fixture.waitFile(t, systemdProofReadyFile, 90*time.Second)
		var state systemdProofState
		readSystemdProofJSON(t, filepath.Join(fixture.stateDirectory, systemdProofStateFile), &state)
		if !state.CleanLeafRemoved || !state.ReplacementUnconfirmed {
			t.Fatalf("phase-one controls = %+v", state)
		}
		supervisorFD, err := unix.PidfdOpen(int(state.SupervisorPID), 0)
		if err != nil {
			t.Fatalf("pidfd_open supervisor: %v", err)
		}
		defer unix.Close(supervisorFD)
		descendantFD, err := unix.PidfdOpen(int(state.DescendantPID), 0)
		if err != nil {
			t.Fatalf("pidfd_open descendant: %v", err)
		}
		defer unix.Close(descendantFD)
		if exited, err := pidfdExited(supervisorFD); err != nil || exited {
			t.Fatalf("supervisor pidfd before crash = (exited=%v, err=%v)", exited, err)
		}
		if exited, err := pidfdExited(descendantFD); err != nil || exited {
			t.Fatalf("descendant pidfd before crash = (exited=%v, err=%v)", exited, err)
		}

		mainPID := fixture.mainPID(t)
		if uint32(mainPID) != state.UnitPID {
			t.Fatalf("systemd MainPID = %d, phase-one unit pid = %d", mainPID, state.UnitPID)
		}
		if uint32(mainPID) == state.SupervisorPID || uint32(mainPID) == state.DescendantPID {
			t.Fatalf("systemd MainPID %d aliases an attempt pid", mainPID)
		}
		fixture.systemctl(t, "kill", "--kill-whom=main", "--signal=SIGKILL", fixture.unitName)
		waitPidfdReadable(t, supervisorFD, 90*time.Second, "old supervisor")
		waitPidfdReadable(t, descendantFD, 90*time.Second, "old setsid descendant")

		var result systemdProofResult
		fixture.waitJSON(t, systemdProofResultFile, &result, 120*time.Second)
		if !result.RecoveryConfirmed || !result.MarkerPresentBeforeReady || !result.MarkerPresentAfterReady ||
			!result.AttemptLeafRemoved || !result.StartupResiduePresent || !result.StartupResidueRemoved ||
			!result.SupervisorIdentityGone || !result.DescendantIdentityGone {
			t.Fatalf("restart proof = %+v", result)
		}
		if result.RestartPID == 0 || result.RestartPID == state.UnitPID || result.RestartInvocationID == "" ||
			result.RestartInvocationID == state.InvocationID {
			t.Fatalf("replacement invocation = (pid=%d id=%q), phase one = (pid=%d id=%q)",
				result.RestartPID, result.RestartInvocationID, state.UnitPID, state.InvocationID)
		}
		t.Logf("restart proof: %+v", result)
		if fileExists(filepath.Join(fixture.stateDirectory, systemdProofMarkerFile)) {
			t.Fatal("run marker remained after positive recovery classification")
		}
		t.Logf("NRestarts after successful replacement exit: %s", fixture.property(t, "NRestarts"))
		if dropins := fixture.property(t, "DropInPaths"); dropins != "" {
			t.Fatalf("DropInPaths = %q, want no delegation drop-in", dropins)
		}
	})
}

type systemdProofUnit struct {
	unitName       string
	unitFile       string
	stateDirectory string
	cleaned        bool
}

func installSystemdProofUnit(t *testing.T, source []byte, uid, gid int, delegate bool, mode string) *systemdProofUnit {
	t.Helper()
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	stateName := "ben-234-systemd-proof-" + suffix
	stateDirectory := filepath.Join("/var/lib", stateName)
	unitName := stateName + ".service"
	unitFile := filepath.Join("/run/systemd/system", unitName)
	if err := os.Mkdir(stateDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(stateDirectory, uid, gid); err != nil {
		t.Fatal(err)
	}
	modePath := filepath.Join(stateDirectory, systemdProofModeFile)
	if err := os.WriteFile(modePath, []byte(mode+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(modePath, uid, gid); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(stateDirectory, "localdomain.test")
	copySystemdProofExecutable(t, executable, uid, gid)

	generated := string(source)
	generated = replaceSystemdProofLine(t, generated, "WorkingDirectory=/srv/ben", "WorkingDirectory="+stateDirectory)
	generated = replaceSystemdProofLine(t, generated, "ExecStartPre=/bin/false", "ExecStartPre=/bin/true")
	generated = replaceSystemdProofLine(t, generated,
		"ExecStart=/usr/local/bin/ben run /srv/ben/WORKFLOW.md",
		"ExecStart="+executable+" -test.run=^TestShippedSystemdCrashRestart$ -test.v")
	generated = replaceSystemdProofLine(t, generated, "EnvironmentFile=/etc/ben/env", "EnvironmentFile=-/dev/null")
	generated = replaceSystemdProofLine(t, generated, "StateDirectory=ben", "StateDirectory="+stateName)
	generated = replaceSystemdProofLine(t, generated, "ReadWritePaths=/srv/ben", "ReadWritePaths="+stateDirectory)
	if !delegate {
		generated = replaceSystemdProofLine(t, generated, "Delegate=yes", "")
	}
	if err := os.WriteFile(unitFile, []byte(generated), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := &systemdProofUnit{unitName: unitName, unitFile: unitFile, stateDirectory: stateDirectory}
	t.Cleanup(func() { fixture.cleanup(t) })
	fixture.command(t, "systemd-analyze", "verify", unitFile)
	fixture.systemctl(t, "daemon-reload")
	return fixture
}

func replaceSystemdProofLine(t *testing.T, source, old, replacement string) string {
	t.Helper()
	needle := old + "\n"
	if strings.Count(source, needle) != 1 {
		t.Fatalf("checked-in unit contains %q %d times, want exactly once", old, strings.Count(source, needle))
	}
	if replacement != "" {
		replacement += "\n"
	}
	return strings.Replace(source, needle, replacement, 1)
}

func copySystemdProofExecutable(t *testing.T, destination string, uid, gid int) {
	t.Helper()
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destinationFile, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destinationFile, source); err != nil {
		destinationFile.Close()
		t.Fatal(err)
	}
	if err := destinationFile.Sync(); err != nil {
		destinationFile.Close()
		t.Fatal(err)
	}
	if err := destinationFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(destination, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func (u *systemdProofUnit) start(t *testing.T) {
	t.Helper()
	u.systemctl(t, "start", u.unitName)
}

func (u *systemdProofUnit) mainPID(t *testing.T) int {
	t.Helper()
	raw := u.property(t, "MainPID")
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		t.Fatalf("MainPID = %q: %v", raw, err)
	}
	return pid
}

func (u *systemdProofUnit) property(t *testing.T, name string) string {
	t.Helper()
	out := u.systemctl(t, "show", "--property="+name, "--value", u.unitName)
	return strings.TrimSpace(out)
}

func (u *systemdProofUnit) waitFile(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	path := filepath.Join(u.stateDirectory, name)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; journal:\n%s", name, u.journal(t))
}

func (u *systemdProofUnit) waitJSON(t *testing.T, name string, value any, timeout time.Duration) {
	t.Helper()
	u.waitFile(t, name, timeout)
	readSystemdProofJSON(t, filepath.Join(u.stateDirectory, name), value)
}

func (u *systemdProofUnit) systemctl(t *testing.T, args ...string) string {
	t.Helper()
	return u.command(t, "systemctl", args...)
}

func (u *systemdProofUnit) command(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (u *systemdProofUnit) journal(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("journalctl", "--no-pager", "-n", "100", "-u", u.unitName)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func (u *systemdProofUnit) cleanup(t *testing.T) {
	t.Helper()
	if u.cleaned {
		return
	}
	u.cleaned = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"kill", "--kill-whom=all", "--signal=SIGKILL", u.unitName},
		{"stop", u.unitName},
		{"reset-failed", u.unitName},
	} {
		cmd := exec.CommandContext(ctx, "systemctl", args...)
		_, _ = cmd.CombinedOutput()
	}
	if err := os.Remove(u.unitFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove proof unit: %v", err)
	}
	_, _ = exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput()
	if !strings.HasPrefix(u.stateDirectory, "/var/lib/ben-234-systemd-proof-") {
		t.Errorf("refusing to clean unexpected state path %q", u.stateDirectory)
		return
	}
	if err := os.RemoveAll(u.stateDirectory); err != nil {
		t.Errorf("remove proof state: %v", err)
	}
}

func waitPidfdReadable(t *testing.T, pidfd int, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exited, err := pidfdExited(pidfd)
		if err != nil {
			t.Fatalf("poll %s pidfd: %v", what, err)
		}
		if exited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s pidfd did not become readable", what)
}

func writeSystemdProofJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := writeSystemdProofJSONError(path, value); err != nil {
		t.Fatal(err)
	}
}

func writeSystemdProofJSONError(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".systemd-proof-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := json.NewEncoder(temporary).Encode(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readSystemdProofJSON(t *testing.T, path string, value any) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func findShippedUnit(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		candidate := filepath.Join(directory, "deploy", "ben.service")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	t.Fatal("could not find deploy/ben.service from the test working directory")
	return ""
}

func firstOutputLine(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
