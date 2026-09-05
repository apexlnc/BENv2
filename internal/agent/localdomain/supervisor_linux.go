//go:build linux

package localdomain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	controlFD = 3
	stdinFD   = 4
	stdoutFD  = 5
	stderrFD  = 6

	canarySiblingEnv = "BEN_LOCALDOMAIN_CANARY_SIBLING"
	canaryDaemonEnv  = "BEN_LOCALDOMAIN_CANARY_DAEMON_PID"
)

type controlMessage struct {
	Kind    string             `json:"kind"`
	Ready   *supervisorReady   `json:"ready,omitempty"`
	Request *supervisorRequest `json:"request,omitempty"`
	Exit    *ProviderExit      `json:"exit,omitempty"`
	Error   string             `json:"error,omitempty"`
}

type supervisorReady struct {
	PID    int              `json:"pid"`
	Cgroup string           `json:"cgroup"`
	Mounts mountSetupReport `json:"mounts"`
}

type supervisorRequest struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env"`
	Dir     string   `json:"dir"`
	HostUID int      `json:"host_uid"`
	HostGID int      `json:"host_gid"`
}

type providerMessage struct {
	Request *supervisorRequest `json:"request,omitempty"`
	Error   string             `json:"error,omitempty"`
}

type lockedEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *lockedEncoder) send(message controlMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(message)
}

// InternalMain is the only entry to the same-binary supervisor and provider
// trampoline. cmd/ben calls it before normal command parsing.
func InternalMain(args []string) (bool, int) {
	if len(args) != 2 {
		return false, 0
	}
	switch args[1] {
	case supervisorArg:
		if err := runSupervisor(); err != nil {
			return true, 125
		}
		return true, 0
	case providerArg:
		if err := runProviderTrampoline(); err != nil {
			return true, 126
		}
		return true, 0
	case canaryArg:
		runCanaryProvider()
		return true, 0
	case nestedCgroupArg:
		runNestedMountHelper("cgroup2")
		return true, 0
	case nestedProcArg:
		runNestedMountHelper("proc")
		return true, 0
	case canaryKeeperArg:
		runCanaryKeeper()
		return true, 0
	default:
		return false, 0
	}
}

func runSupervisor() (retErr error) {
	control := os.NewFile(controlFD, "localdomain-control")
	if control == nil {
		return fmt.Errorf("supervisor control descriptor unavailable")
	}
	defer control.Close()
	encoder := &lockedEncoder{encoder: json.NewEncoder(control)}
	decoder := json.NewDecoder(control)
	fail := func(err error) error {
		_ = encoder.send(controlMessage{Kind: "error", Error: err.Error()})
		return err
	}
	mounts, err := setupMountNamespace()
	if err != nil {
		return fail(err)
	}
	cgroupData, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return fail(err)
	}
	cgroup, err := parseUnifiedCgroup(string(cgroupData))
	if err != nil {
		return fail(err)
	}
	ready := supervisorReady{PID: unix.Getpid(), Cgroup: cgroup, Mounts: mounts}
	if err := encoder.send(controlMessage{Kind: "ready", Ready: &ready}); err != nil {
		return err
	}
	var release controlMessage
	if err := decoder.Decode(&release); err != nil {
		return fail(fmt.Errorf("read release: %w", err))
	}
	if release.Kind == "abort" {
		return nil
	}
	if release.Kind != "release" || release.Request == nil {
		return fail(fmt.Errorf("invalid release message"))
	}

	term := make(chan os.Signal, 1)
	stopSignals := make(chan struct{})
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	go func() {
		acked := false
		for {
			select {
			case <-term:
				if !acked {
					_ = encoder.send(controlMessage{Kind: "term_ack"})
					acked = true
				}
				_ = unix.Kill(-1, unix.SIGTERM)
			case <-stopSignals:
				return
			}
		}
	}()
	defer close(stopSignals)

	exit, err := superviseProvider(*release.Request, func() error {
		return encoder.send(controlMessage{Kind: "started"})
	})
	if err != nil && exit.StartError == "" {
		exit.StartError = err.Error()
	}
	if sendErr := encoder.send(controlMessage{Kind: "exit", Exit: &exit}); sendErr != nil && err == nil {
		err = sendErr
	}
	if reapErr := reapNamespace(); reapErr != nil && err == nil {
		err = reapErr
	}
	return err
}

func superviseProvider(request supervisorRequest, onStarted func() error) (ProviderExit, error) {
	if err := validateSupervisorRequest(request); err != nil {
		return ProviderExit{StartError: err.Error()}, err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return ProviderExit{StartError: err.Error()}, err
	}
	parent := os.NewFile(uintptr(pair[0]), "provider-config-parent")
	child := os.NewFile(uintptr(pair[1]), "provider-config-child")
	defer parent.Close()
	executable, err := os.Executable()
	if err != nil {
		child.Close()
		return ProviderExit{StartError: err.Error()}, err
	}
	stdin := os.NewFile(stdinFD, "provider-stdin")
	stdout := os.NewFile(stdoutFD, "provider-stdout")
	stderr := os.NewFile(stderrFD, "provider-stderr")
	if stdin == nil || stdout == nil || stderr == nil {
		closeSupervisorStreams(stdin, stdout, stderr)
		child.Close()
		return ProviderExit{StartError: "provider stream descriptor unavailable"}, fmt.Errorf("provider stream descriptor unavailable")
	}
	cmd := exec.Command(executable, providerArg)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{child, stdin, stdout, stderr}
	cmd.SysProcAttr = providerSysProcAttr(request.HostUID, request.HostGID)
	if err := cmd.Start(); err != nil {
		closeSupervisorStreams(stdin, stdout, stderr)
		child.Close()
		return ProviderExit{StartError: err.Error()}, err
	}
	// Start duplicated these descriptors into the provider trampoline. The
	// trusted supervisor must not keep a transcript pipe open while it remains
	// PID 1 to reap an otherwise detached descendant.
	closeSupervisorStreams(stdin, stdout, stderr)
	child.Close()
	if err := json.NewEncoder(parent).Encode(providerMessage{Request: &request}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ProviderExit{StartError: err.Error()}, err
	}
	var setup providerMessage
	decodeErr := json.NewDecoder(parent).Decode(&setup)
	if decodeErr != nil && !errors.Is(decodeErr, io.EOF) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ProviderExit{StartError: decodeErr.Error()}, decodeErr
	}
	if setup.Error != "" {
		_ = cmd.Wait()
		return ProviderExit{StartError: setup.Error}, fmt.Errorf("provider setup: %s", setup.Error)
	}
	if err := onStarted(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ProviderExit{StartError: err.Error()}, err
	}
	waitErr := cmd.Wait()
	exit := providerExit(cmd.ProcessState)
	return exit, waitErr
}

func closeSupervisorStreams(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func validateSupervisorRequest(request supervisorRequest) error {
	if len(request.Argv) == 0 || !filepath.IsAbs(request.Argv[0]) {
		return fmt.Errorf("provider argv[0] must be absolute")
	}
	if !filepath.IsAbs(request.Dir) || filepath.Clean(request.Dir) != request.Dir {
		return fmt.Errorf("provider directory must be canonical and absolute")
	}
	if request.HostUID < 0 || request.HostGID < 0 {
		return fmt.Errorf("provider identity is invalid")
	}
	for _, value := range append(append([]string(nil), request.Argv...), request.Env...) {
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("provider value contains NUL")
		}
	}
	return nil
}

func providerSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{
			ContainerID: uid,
			HostID:      0,
			Size:        1,
		}},
		GidMappings: []syscall.SysProcIDMap{{
			ContainerID: gid,
			HostID:      0,
			Size:        1,
		}},
		GidMappingsEnableSetgroups: false,
		Credential:                 &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
}

func runProviderTrampoline() error {
	control := os.NewFile(controlFD, "provider-config")
	if control == nil {
		return fmt.Errorf("provider config descriptor unavailable")
	}
	encoder := json.NewEncoder(control)
	fail := func(err error) error {
		_ = encoder.Encode(providerMessage{Error: err.Error()})
		return err
	}
	var message providerMessage
	if err := json.NewDecoder(control).Decode(&message); err != nil {
		return fail(err)
	}
	if message.Request == nil {
		return fail(fmt.Errorf("provider request missing"))
	}
	request := *message.Request
	if err := validateSupervisorRequest(request); err != nil {
		return fail(err)
	}
	// no_new_privs and capability state are per-thread. Never unlock: Exec must
	// replace this same hardened thread rather than a different runtime thread.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fail(fmt.Errorf("set no_new_privs: %w", err))
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fail(fmt.Errorf("clear ambient capabilities: %w", err))
	}
	capabilities := [2]unix.CapUserData{}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capset(&header, &capabilities[0]); err != nil {
		return fail(fmt.Errorf("clear capabilities: %w", err))
	}
	if err := unix.Chdir(request.Dir); err != nil {
		return fail(fmt.Errorf("provider chdir: %w", err))
	}
	for source, target := range map[int]int{stdinFD: 0, stdoutFD: 1, stderrFD: 2} {
		if err := unix.Dup3(source, target, 0); err != nil {
			return fail(fmt.Errorf("provider stream %d: %w", target, err))
		}
	}
	unix.CloseOnExec(controlFD)
	if err := unix.CloseRange(stdinFD, ^uint(0), 0); err != nil {
		return fail(fmt.Errorf("close provider descriptors: %w", err))
	}
	if err := unix.Exec(request.Argv[0], request.Argv, request.Env); err != nil {
		return fail(fmt.Errorf("exec provider: %w", err))
	}
	return nil
}

func providerExit(state *os.ProcessState) ProviderExit {
	if state == nil {
		return ProviderExit{StartError: "provider has no process state"}
	}
	exit := ProviderExit{Code: state.ExitCode()}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exit.Signal = int(status.Signal())
	}
	return exit
}

func reapNamespace() error {
	for {
		var status unix.WaitStatus
		_, err := unix.Wait4(-1, &status, 0, nil)
		switch {
		case err == nil:
			continue
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			return nil
		default:
			return err
		}
	}
}

type nestedHelperReport struct {
	Outcome              nestedMountResult `json:"outcome"`
	MigrationRejected    bool              `json:"migration_rejected"`
	GuessedHostAttempted bool              `json:"guessed_host_attempted"`
	Error                string            `json:"error,omitempty"`
}

type canaryProviderReport struct {
	CgroupMount          nestedMountResult `json:"cgroup_mount"`
	ProcMount            nestedMountResult `json:"proc_mount"`
	MigrationAttempt     bool              `json:"migration_attempt"`
	CanonicalRejected    bool              `json:"canonical_rejected"`
	AliasesRejected      bool              `json:"aliases_rejected"`
	NestedRejected       bool              `json:"nested_rejected"`
	GuessedHostAttempted bool              `json:"guessed_host_attempted"`
	KeeperStarted        bool              `json:"keeper_started"`
	CgroupError          string            `json:"cgroup_error,omitempty"`
	ProcError            string            `json:"proc_error,omitempty"`
}

func runCanaryProvider() {
	cgroup := startNestedHelper(nestedCgroupArg)
	proc := startNestedHelper(nestedProcArg)
	sibling := os.Getenv(canarySiblingEnv)
	canonicalRejected, _ := cgroupEscapePathsRejected(canonicalCgroup, sibling)
	canonicalRejected = validAttemptDir(sibling) && canonicalRejected
	aliasRejected := true
	aliases, aliasErr := canaryCoveredAliasTargets()
	if aliasErr != nil {
		aliasRejected = false
	}
	for _, alias := range aliases {
		rejected, _ := cgroupEscapePathsRejected(alias, sibling)
		aliasRejected = aliasRejected && rejected
	}
	guessedHostAttempted := attemptGuessedHostPID(canonicalCgroup)
	nestedRejected := cgroup.Outcome == nestedMountDenied || cgroup.Outcome == nestedMountContained && cgroup.MigrationRejected
	cmd := exec.Command(os.Args[0], canaryKeeperArg)
	cmd.Env = []string{}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	keeperStarted := false
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
		keeperStarted = true
	} else {
		canonicalRejected = false
	}
	_ = json.NewEncoder(os.Stdout).Encode(canaryProviderReport{
		CgroupMount:          cgroup.Outcome,
		ProcMount:            proc.Outcome,
		CanonicalRejected:    canonicalRejected,
		AliasesRejected:      aliasRejected,
		NestedRejected:       nestedRejected,
		GuessedHostAttempted: guessedHostAttempted,
		KeeperStarted:        keeperStarted,
		CgroupError:          cgroup.Error,
		ProcError:            proc.Error,
		MigrationAttempt: canonicalRejected && aliasRejected && nestedRejected &&
			guessedHostAttempted && (cgroup.Outcome != nestedMountContained || cgroup.GuessedHostAttempted),
	})
}

func startNestedHelper(argument string) nestedHelperReport {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nestedHelperReport{Outcome: nestedMountUnknown, Error: err.Error()}
	}
	cmd := exec.Command(os.Args[0], argument)
	cmd.Env = canaryHelperEnvironment()
	cmd.Dir = "/"
	cmd.Stdout = writeEnd
	cmd.Stderr = os.Stderr
	flags := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS)
	if argument == nestedCgroupArg {
		flags |= syscall.CLONE_NEWCGROUP
	} else {
		flags |= syscall.CLONE_NEWPID
	}
	cmd.SysProcAttr = nestedNamespaceSysProcAttr(flags)
	if err := cmd.Start(); err != nil {
		readEnd.Close()
		writeEnd.Close()
		outcome := nestedMountUnknown
		if nestedPolicyDenied(err) {
			outcome = nestedMountDenied
		}
		return nestedHelperReport{Outcome: outcome, Error: err.Error()}
	}
	writeEnd.Close()
	var report nestedHelperReport
	err = json.NewDecoder(io.LimitReader(readEnd, 4096)).Decode(&report)
	readEnd.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nestedHelperReport{Outcome: nestedMountUnknown, Error: err.Error()}
	}
	if report.Outcome == nestedMountContained || report.Outcome == nestedMountExposed {
		_ = cmd.Process.Release()
		return report
	}
	_ = cmd.Wait()
	return report
}

func canaryHelperEnvironment() []string {
	var result []string
	for _, key := range []string{canarySiblingEnv, canaryDaemonEnv} {
		if value, found := os.LookupEnv(key); found {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func canaryCoveredAliasTargets() ([]string, error) {
	records, err := currentMountInfo()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var targets []string
	for _, record := range records {
		if strings.Contains(record.Target, "/.ben-localdomain-alias-") && !seen[record.Target] {
			seen[record.Target] = true
			targets = append(targets, record.Target)
		}
	}
	return targets, nil
}

func nestedNamespaceSysProcAttr(flags uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: flags,
		UidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0,
			HostID:      os.Getuid(),
			Size:        1,
		}},
		GidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0,
			HostID:      os.Getgid(),
			Size:        1,
		}},
		GidMappingsEnableSetgroups: false,
	}
}

func runNestedMountHelper(filesystem string) {
	target, err := os.MkdirTemp("/tmp", ".ben-localdomain-nested-"+filesystem+"-")
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{Outcome: nestedMountDenied, Error: err.Error()})
		return
	}
	source := filesystem
	if filesystem == "cgroup2" {
		source = "none"
	}
	if err := unix.Mount(source, target, filesystem, unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		outcome := nestedMountUnknown
		if nestedPolicyDenied(err) {
			outcome = nestedMountDenied
		}
		_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{Outcome: outcome, Error: err.Error()})
		_ = os.Remove(target)
		return
	}
	if filesystem == "cgroup2" {
		nested := filepath.Join(target, "provider-a", "provider-b")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{Outcome: nestedMountUnknown, Error: err.Error()})
			return
		}
		sibling := os.Getenv(canarySiblingEnv)
		migrationRejected, escapedPath := cgroupEscapePathsRejected(target, sibling)
		migrationRejected = validAttemptDir(sibling) && migrationRejected
		guessedHostAttempted := attemptGuessedHostPID(nested)
		if !migrationRejected {
			_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{
				Outcome: nestedMountExposed, GuessedHostAttempted: guessedHostAttempted,
				Error: "migration write succeeded: " + escapedPath,
			})
			runCanaryKeeper()
			return
		}
		_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{
			Outcome: nestedMountContained, MigrationRejected: true, GuessedHostAttempted: guessedHostAttempted,
		})
		runCanaryKeeper()
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(nestedHelperReport{Outcome: nestedMountContained})
	runCanaryKeeper()
}

func cgroupEscapePathsRejected(root, sibling string) (bool, string) {
	self := []byte(fmt.Sprintf("%d", os.Getpid()))
	for _, candidate := range []string{
		filepath.Join(root, "..", "cgroup.procs"),
		filepath.Join(root, "..", sibling, "cgroup.procs"),
		filepath.Join(root, "..", "..", "cgroup.procs"),
		filepath.Join(root, "..", "..", supervisorCgroup, "cgroup.procs"),
		filepath.Join(root, runsCgroup, sibling, "cgroup.procs"),
		filepath.Join(root, supervisorCgroup, "cgroup.procs"),
	} {
		if err := writeCgroupMembership(candidate, self); err == nil {
			return false, candidate
		}
	}
	return true, ""
}

func attemptGuessedHostPID(root string) bool {
	value := os.Getenv(canaryDaemonEnv)
	if _, err := parsePositiveDecimal(value, 32); err != nil {
		return false
	}
	_ = writeCgroupMembership(filepath.Join(root, "cgroup.procs"), []byte(value))
	return true
}

func writeCgroupMembership(name string, value []byte) error {
	fd, err := unix.Open(name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := requireFilesystem(fd, unix.CGROUP2_SUPER_MAGIC); err != nil {
		return err
	}
	n, err := file.Write(value)
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	return nil
}

func nestedPolicyDenied(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}

func runCanaryKeeper() {
	signal.Ignore(syscall.SIGTERM)
	for {
		_ = unix.Pause()
	}
}
