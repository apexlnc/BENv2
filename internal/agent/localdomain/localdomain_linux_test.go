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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	realFixtureEnv         = "BEN_LOCALDOMAIN_DELEGATED_ROOT"
	unrestrictedFixtureEnv = "BEN_LOCALDOMAIN_UNRESTRICTED_NESTED_MOUNTS"
	realSetsidArg          = "__ben_localdomain_test_setsid_v1"
	realNestedArg          = "__ben_localdomain_test_nested_pid_v1"
	realKeeperArg          = "__ben_localdomain_test_keeper_v1"
	realPeelArg            = "__ben_localdomain_test_peel_v1"
	realNestedPeelArg      = "__ben_localdomain_test_nested_peel_v1"
)

func TestMain(m *testing.M) {
	if handled, code := InternalMain(os.Args); handled {
		os.Exit(code)
	}
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case realSetsidArg:
			os.Exit(runDescendantFixture(false))
		case realNestedArg:
			os.Exit(runDescendantFixture(true))
		case realKeeperArg:
			runRealKeeper()
			os.Exit(0)
		case realPeelArg:
			os.Exit(runPeelFixture())
		case realNestedPeelArg:
			runNestedPeeler()
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// TestLinuxDelegatedExecutionDomain is intentionally gated. Run the test
// binary as a non-root process in a delegated cgroup-v2 service with a private
// disposable mount namespace, and set BEN_LOCALDOMAIN_DELEGATED_ROOT to that
// exact current cgroup directory. The fixture may retain CAP_SYS_ADMIN only for
// hostile mount construction; production does not require it. Set
// BEN_LOCALDOMAIN_UNRESTRICTED_NESTED_MOUNTS=1 for the fixture that
// proves both provider-created mount success branches. The checked-in systemd
// unit is not this fixture; its proof belongs to #234.
func TestLinuxDelegatedExecutionDomain(t *testing.T) {
	root := os.Getenv(realFixtureEnv)
	if root == "" {
		t.Skipf("set %s inside a caller-supplied delegated cgroup fixture", realFixtureEnv)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	timings := Timings{
		InterruptGrace:  500 * time.Millisecond,
		DiscardGrace:    150 * time.Millisecond,
		KillGrace:       2 * time.Second,
		PollInterval:    10 * time.Millisecond,
		CleanupRetry:    20 * time.Millisecond,
		CleanupPass:     time.Second,
		CleanupNodes:    1024,
		CleanupFailures: 5,
	}
	startup := prepareStartupResidue(t, root, executable)
	defer startup.cleanup()
	manager := New(Options{Executable: executable, DelegatedRoot: root, Timings: timings})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := manager.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if _, err := os.Stat(startup.emptyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty startup residue still exists: %v", err)
	}
	if _, err := os.Stat(startup.populatedPath); err != nil {
		t.Fatalf("populated startup residue was removed: %v", err)
	}
	startup.stop(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := os.Stat(startup.populatedPath)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("startup janitor did not remove residue after it became empty")
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Run("clean natural exit", func(t *testing.T) {
		run := startRealRun(t, ctx, manager, []string{"/bin/true"})
		exit := awaitProvider(t, run)
		if !exit.Success() {
			t.Fatalf("provider exit = %+v", exit)
		}
		awaitTermination(t, func() (Termination, error) { return run.Handle.Probe(ctx) }, TerminationConfirmed)
	})

	for _, tc := range []struct {
		name string
		arg  string
	}{{"setsid descendant", realSetsidArg}, {"nested PID namespace descendant", realNestedArg}} {
		t.Run(tc.name, func(t *testing.T) {
			run := startRealRun(t, ctx, manager, []string{executable, tc.arg})
			exit := awaitProvider(t, run)
			if !exit.Success() {
				t.Fatalf("provider exit = %+v", exit)
			}
			if got, err := run.Handle.Probe(ctx); got != TerminationUnconfirmed {
				t.Fatalf("Probe with descendant = (%v, %v), want unconfirmed", got, err)
			}
			if got, err := manager.Recover(ctx, run.Evidence); got != TerminationUnconfirmed {
				t.Fatalf("Recover with descendant = (%v, %v), want unconfirmed", got, err)
			}
			got, err := run.Handle.Stop(ctx, StopInterrupt)
			if got != TerminationConfirmed || err != nil {
				t.Fatalf("Stop = (%v, %v), want confirmed", got, err)
			}
			got, err = manager.Recover(ctx, run.Evidence)
			if got != TerminationConfirmed || err != nil {
				t.Fatalf("Recover after stop = (%v, %v), want confirmed", got, err)
			}
		})
	}

	fixture := installInheritedHostileMountFixture(t)
	t.Run("hostile migration remains in the PID domain", func(t *testing.T) {
		if err := manager.capabilityCanary(ctx); err != nil {
			t.Fatal(err)
		}
		manager.mu.Lock()
		report := manager.canaryReport
		manager.mu.Unlock()
		if !report.MigrationRejected {
			t.Fatal("hostile migration canary was not positively rejected")
		}
		if os.Getenv(unrestrictedFixtureEnv) == "1" &&
			(report.NestedCgroupMount != nestedMountContained || report.NestedProcMount != nestedMountContained) {
			t.Fatalf("unrestricted nested mounts = (%v, %v), want both contained", report.NestedCgroupMount, report.NestedProcMount)
		}
	})
	t.Run("locked mount aliases are covered", func(t *testing.T) {
		run := startRealRun(t, ctx, manager, []string{executable, realKeeperArg})
		awaitStarted(t, run)
		fixture.propagate(t)
		assertNoLatePropagation(t, run, fixture.lateTarget)
		var sawDirectory, sawFile, sawPropagation, sawNested bool
		found := make(map[string]bool)
		for _, alias := range run.mounts.Aliases {
			oldID, wanted := fixture.aliases[alias.Target]
			if !wanted {
				continue
			}
			found[alias.Target] = true
			if oldID == 0 {
				t.Errorf("parent alias %s has no mount ID", alias.Target)
			}
			wantMagic, knownFilesystem := filesystemMagic(alias.Filesystem)
			if !validMountRoot(alias.Root) || !knownFilesystem || alias.Magic != wantMagic {
				t.Errorf("alias %s identity root=%q magic=%#x filesystem=%q", alias.Target, alias.Root, alias.Magic, alias.Filesystem)
			}
			if alias.Directory {
				sawDirectory = true
			} else {
				sawFile = true
			}
			sawPropagation = sawPropagation || alias.Propagating
			if strings.HasSuffix(alias.Target, "/proc-dir/sys") {
				sawNested = true
			}
			if alias.ID == alias.CoveredBy || alias.CoveredBy == 0 {
				t.Errorf("alias %+v was not replaced by a distinct cover", alias)
			}
		}
		if !sawDirectory || !sawFile || !sawPropagation || !sawNested {
			t.Fatalf("fixture coverage dir=%v file=%v propagation=%v nested=%v", sawDirectory, sawFile, sawPropagation, sawNested)
		}
		for target := range fixture.aliases {
			if !found[target] {
				t.Errorf("inherited alias %s was absent from supervisor snapshot", target)
			}
		}
		if got, err := run.Handle.Stop(ctx, StopInterrupt); got != TerminationConfirmed || err != nil {
			t.Fatalf("Stop = (%v, %v)", got, err)
		}
	})

	t.Run("provider cannot reveal covered aliases", func(t *testing.T) {
		input, err := os.Open("/dev/null")
		if err != nil {
			t.Fatal(err)
		}
		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		run, err := manager.Start(ctx, Launch{
			Argv: []string{executable, realPeelArg}, Env: []string{}, Dir: "/",
			Stdin: input, Stdout: stdoutW, Stderr: stderr,
		})
		input.Close()
		stdoutW.Close()
		stderr.Close()
		if err != nil {
			t.Fatal(err)
		}
		var report peelFixtureReport
		if err := json.NewDecoder(io.LimitReader(stdoutR, 4096)).Decode(&report); err != nil {
			t.Fatal(err)
		}
		stdoutR.Close()
		if report.Error != "" {
			t.Fatalf("peel fixture: %s", report.Error)
		}
		if !report.OuterRejected || !report.NestedReady {
			t.Fatalf("peel report = %+v", report)
		}
		if exit := awaitProvider(t, run); !exit.Success() {
			t.Fatalf("provider exit = %+v", exit)
		}
		if got, err := run.Handle.Probe(ctx); got != TerminationUnconfirmed {
			t.Fatalf("Probe with nested peeler = (%v, %v)", got, err)
		}
		assertOldAliasesUnreachable(t, manager, run)
		if got, err := run.Handle.Stop(ctx, StopInterrupt); got != TerminationConfirmed || err != nil {
			t.Fatalf("Stop = (%v, %v)", got, err)
		}
	})
}

func startRealRun(t *testing.T, ctx context.Context, manager *Manager, argv []string) *Run {
	t.Helper()
	input, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	run, err := manager.Start(ctx, Launch{
		Argv: argv, Env: []string{}, Dir: "/", Stdin: input, Stdout: os.Stderr, Stderr: os.Stderr,
		OnDomain: func(evidence Evidence) error {
			_, parseErr := ParseEvidence(evidence)
			return parseErr
		},
	})
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func awaitProvider(t *testing.T, run *Run) ProviderExit {
	t.Helper()
	select {
	case exit := <-run.ProviderDone:
		return exit
	case <-time.After(10 * time.Second):
		t.Fatal("provider completion timed out")
		return ProviderExit{}
	}
}

func awaitStarted(t *testing.T, run *Run) {
	t.Helper()
	select {
	case <-run.started:
	case exit := <-run.ProviderDone:
		select {
		case <-run.started:
			return
		default:
			t.Fatalf("provider exited before start observation: %+v", exit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provider start timed out")
	}
}

func awaitTermination(t *testing.T, probe func() (Termination, error), want Termination) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := probe()
		if got == want {
			return
		}
		if err != nil && !errors.Is(err, ErrContainment) {
			t.Fatalf("probe error: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("termination remained %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runDescendantFixture(nested bool) int {
	cmd := exec.Command(os.Args[0], realKeeperArg)
	cmd.Env = []string{}
	if nested {
		cmd.SysProcAttr = nestedNamespaceSysProcAttr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS)
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "start descendant: %v\n", err)
		return 1
	}
	if err := cmd.Process.Release(); err != nil {
		return 1
	}
	return 0
}

func runRealKeeper() {
	signal.Ignore(syscall.SIGTERM)
	for {
		_ = unix.Pause()
	}
}

type peelFixtureReport struct {
	OuterRejected bool   `json:"outer_rejected"`
	NestedReady   bool   `json:"nested_ready"`
	Error         string `json:"error,omitempty"`
}

func runPeelFixture() int {
	fail := func(err error) int {
		_ = json.NewEncoder(os.Stdout).Encode(peelFixtureReport{Error: err.Error()})
		return 1
	}
	targets, err := fixtureAliasTargets()
	if err != nil {
		return fail(fmt.Errorf("list alias targets: %w", err))
	}
	if len(targets) == 0 {
		return fail(fmt.Errorf("no alias targets found"))
	}
	outerRejected := true
	for _, target := range targets {
		if err := unix.Unmount(target, 0); err == nil {
			outerRejected = false
		}
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("create nested peeler pipe: %w", err))
	}
	cmd := exec.Command(os.Args[0], realNestedPeelArg)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stdout = writeEnd
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = nestedNamespaceSysProcAttr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS)
	if err := cmd.Start(); err != nil {
		readEnd.Close()
		writeEnd.Close()
		return fail(fmt.Errorf("start nested peeler: %w", err))
	}
	writeEnd.Close()
	var ready struct {
		Ready bool `json:"ready"`
	}
	err = json.NewDecoder(io.LimitReader(readEnd, 4096)).Decode(&ready)
	readEnd.Close()
	if err != nil || !ready.Ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fail(fmt.Errorf("nested peeler readiness: ready=%v: %w", ready.Ready, err))
	}
	if err := cmd.Process.Release(); err != nil {
		return fail(fmt.Errorf("release nested peeler: %w", err))
	}
	if err := json.NewEncoder(os.Stdout).Encode(peelFixtureReport{OuterRejected: outerRejected, NestedReady: true}); err != nil {
		return 1
	}
	return 0
}

func runNestedPeeler() {
	targets, _ := fixtureAliasTargets()
	for _, target := range targets {
		_ = unix.Unmount(target, 0)
	}
	_, procErr := os.ReadFile("/proc/self/status")
	_, cgroupErr := os.ReadFile(filepath.Join(canonicalCgroup, "cgroup.events"))
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Ready bool `json:"ready"`
	}{Ready: procErr == nil && cgroupErr == nil})
	runRealKeeper()
}

func fixtureAliasTargets() ([]string, error) {
	records, err := currentMountInfo()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var targets []string
	for _, record := range records {
		if strings.Contains(record.Target, "/.ben-localdomain-alias-") && !seen[record.Target] {
			seen[record.Target] = true
			targets = append(targets, record.Target)
		}
	}
	return targets, nil
}

func assertOldAliasesUnreachable(t *testing.T, manager *Manager, run *Run) {
	t.Helper()
	identity, err := ParseEvidence(run.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = manager.forEachPIDDomainProcess(identity.PIDNS, func(pid uint32, _ *os.File) error {
		seen++
		for _, alias := range run.mounts.Aliases {
			if !strings.Contains(alias.Target, "/.ben-localdomain-alias-") {
				continue
			}
			mountID, err := mountIDAt(unix.AT_FDCWD, fmt.Sprintf("/proc/%d/root%s", pid, alias.Target), 0)
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				return err
			}
			if mountID == alias.ID {
				return fmt.Errorf("process %d revealed old mount %d at %s", pid, alias.ID, alias.Target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 2 {
		t.Fatalf("observed %d PID-domain members, want supervisor and nested peeler", seen)
	}
}

type inheritedMountFixture struct {
	root       string
	procDir    string
	peerDir    string
	lateTarget string
	aliases    map[string]uint64
}

func installInheritedHostileMountFixture(t *testing.T) *inheritedMountFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", ".ben-localdomain-alias-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &inheritedMountFixture{root: root, aliases: make(map[string]uint64)}
	t.Cleanup(func() { fixture.cleanup(t) })
	fixture.procDir = filepath.Join(root, "proc-dir")
	fixture.peerDir = filepath.Join(root, "proc-peer")
	fileAlias := filepath.Join(root, "proc-version")
	cgroupAlias := filepath.Join(root, "cgroup-dir")
	for _, directory := range []string{fixture.procDir, fixture.peerDir, cgroupAlias} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.OpenFile(fileAlias, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o400)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	fixture.mount(t, "/proc", fixture.procDir, unix.MS_BIND|unix.MS_REC)
	nested := filepath.Join(fixture.procDir, "sys")
	fixture.mount(t, "/proc/sys", nested, unix.MS_BIND)
	fixture.mount(t, "/proc/version", fileAlias, unix.MS_BIND)
	fixture.mount(t, canonicalCgroup, cgroupAlias, unix.MS_BIND|unix.MS_REC)
	if err := unix.Mount("", fixture.procDir, "", unix.MS_SHARED|unix.MS_REC, ""); err != nil {
		t.Fatalf("make inherited proc fixture shared: %v", err)
	}
	fixture.mount(t, fixture.procDir, fixture.peerDir, unix.MS_BIND|unix.MS_REC)
	for _, target := range []string{fixture.procDir, nested, fileAlias, cgroupAlias, fixture.peerDir} {
		mountID, err := mountIDAt(unix.AT_FDCWD, target, 0)
		if err != nil {
			t.Fatal(err)
		}
		fixture.aliases[target] = mountID
	}
	return fixture
}

func (f *inheritedMountFixture) mount(t *testing.T, source, target string, flags uintptr) {
	t.Helper()
	if err := unix.Mount(source, target, "", flags, ""); err != nil {
		t.Fatalf("mount inherited fixture %s at %s: %v (the caller-supplied fixture needs a private mount namespace with CAP_SYS_ADMIN)", source, target, err)
	}
}

func (f *inheritedMountFixture) propagate(t *testing.T) {
	t.Helper()
	f.lateTarget = filepath.Join(f.procDir, "uptime")
	f.mount(t, "/proc/uptime", f.lateTarget, unix.MS_BIND)
}

func assertNoLatePropagation(t *testing.T, run *Run, target string) {
	t.Helper()
	identity, err := ParseEvidence(run.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", identity.PID))
	if err != nil {
		t.Fatal(err)
	}
	records, err := parseMountInfo(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Target == target {
			t.Fatalf("late parent mount propagated back into private supervisor namespace: %+v", record)
		}
	}
}

func (f *inheritedMountFixture) cleanup(t *testing.T) {
	t.Helper()
	for _, target := range []string{
		filepath.Join(f.peerDir, "uptime"),
		f.lateTarget,
		filepath.Join(f.peerDir, "sys"),
		filepath.Join(f.procDir, "sys"),
		f.peerDir,
		f.procDir,
		filepath.Join(f.root, "proc-version"),
		filepath.Join(f.root, "cgroup-dir"),
	} {
		if target == "" {
			continue
		}
		if err := unix.Unmount(target, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			t.Errorf("unmount hostile fixture %s: %v", target, err)
		}
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Errorf("remove hostile fixture: %v", err)
	}
}

type startupResidue struct {
	emptyPath     string
	populatedPath string
	cmd           *exec.Cmd
	pidfd         int
	stopped       bool
}

func prepareStartupResidue(t *testing.T, root, executable string) *startupResidue {
	t.Helper()
	runs := filepath.Join(root, runsCgroup)
	if err := os.Mkdir(runs, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	empty := filepath.Join(runs, attemptPrefix+strings.Repeat("a", 32))
	populated := filepath.Join(runs, attemptPrefix+strings.Repeat("b", 32))
	if err := os.MkdirAll(filepath.Join(empty, "nested-a", "nested-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(populated, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(populated, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	pidfd := -1
	cmd := exec.Command(executable, realKeeperArg)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd, PidFD: &pidfd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if pidfd < 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("startup residue clone returned no pidfd")
	}
	return &startupResidue{emptyPath: empty, populatedPath: populated, cmd: cmd, pidfd: pidfd}
}

func (s *startupResidue) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	if err := unix.PidfdSendSignal(s.pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		t.Fatal(err)
	}
	_ = s.cmd.Wait()
	unix.Close(s.pidfd)
	s.pidfd = -1
}

func (s *startupResidue) cleanup() {
	if !s.stopped {
		_ = unix.PidfdSendSignal(s.pidfd, unix.SIGKILL, nil, 0)
		_ = s.cmd.Wait()
		unix.Close(s.pidfd)
		s.stopped = true
	}
	for _, path := range []string{
		filepath.Join(s.emptyPath, "nested-a", "nested-b"),
		filepath.Join(s.emptyPath, "nested-a"),
		s.emptyPath,
		s.populatedPath,
	} {
		_ = os.Remove(path)
	}
}
