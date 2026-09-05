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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Start creates and records a domain before releasing any untrusted provider
// instruction. Once OnDomain succeeds, later launch failure is reported on
// ProviderDone alongside the still-owned Handle.
func (m *Manager) Start(ctx context.Context, launch Launch) (*Run, error) {
	if err := m.Ready(ctx); err != nil {
		return nil, err
	}
	return m.startAttempt(ctx, launch)
}

func (m *Manager) startAttempt(ctx context.Context, launch Launch) (*Run, error) {
	if err := validateLaunch(launch); err != nil {
		return nil, err
	}
	target, err := m.createAttempt()
	if err != nil {
		return nil, err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		target.markNoSupervisor()
		m.queue.signal()
		return nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), "supervisor-control-parent")
	child := os.NewFile(uintptr(pair[1]), "supervisor-control-child")
	cmd := exec.Command(m.options.Executable, supervisorArg)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{child, launch.Stdin, launch.Stdout, launch.Stderr}
	pidfd := -1
	cmd.SysProcAttr = supervisorSysProcAttr(int(target.leaf.Fd()), &pidfd)
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		target.markNoSupervisor()
		m.queue.signal()
		return nil, fmt.Errorf("start trusted supervisor: %w", err)
	}
	child.Close()
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
		m.queue.signal()
	}()
	if pidfd < 0 {
		_ = target.kill()
		<-waited
		target.markNoSupervisor()
		parent.Close()
		m.queue.signal()
		return nil, unavailable("clone returned no pidfd", nil)
	}
	if err := target.attachPidfd(pidfd); err != nil {
		_ = target.kill()
		<-waited
		closeFD(&pidfd)
		parent.Close()
		return nil, err
	}
	decoder := json.NewDecoder(parent)
	encoder := json.NewEncoder(parent)
	message, err := readControlMessage(ctx, decoder, parent)
	if err != nil {
		return nil, m.abortBeforeRelease(target, encoder, waited, parent, fmt.Errorf("supervisor handshake: %w", err))
	}
	if message.Kind == "error" {
		return nil, m.abortBeforeRelease(target, encoder, waited, parent, errors.New(message.Error))
	}
	if message.Kind != "ready" || message.Ready == nil {
		return nil, m.abortBeforeRelease(target, encoder, waited, parent, fmt.Errorf("invalid supervisor ready message"))
	}
	identity, err := m.verifySupervisor(cmd.Process.Pid, target, *message.Ready)
	if err != nil {
		return nil, m.abortBeforeRelease(target, encoder, waited, parent, err)
	}
	evidence, err := EncodeEvidence(identity)
	if err != nil {
		return nil, m.abortBeforeRelease(target, encoder, waited, parent, err)
	}
	if launch.OnDomain != nil {
		if err := launch.OnDomain(evidence); err != nil {
			return nil, m.abortBeforeRelease(target, encoder, waited, parent, err)
		}
	}
	exit := make(chan ProviderExit, 1)
	termAck := make(chan struct{})
	started := make(chan struct{})
	run := &Run{
		Evidence: evidence, Handle: newHandle(target, m.timings), ProviderDone: exit,
		termAck: termAck, started: started, mounts: message.Ready.Mounts,
	}
	request := supervisorRequest{
		Argv: launch.Argv, Env: launch.Env, Dir: launch.Dir, HostUID: os.Getuid(), HostGID: os.Getgid(),
	}
	if err := encoder.Encode(controlMessage{Kind: "release", Request: &request}); err != nil {
		exit <- ProviderExit{StartError: fmt.Sprintf("release provider: %v", err)}
		close(exit)
		parent.Close()
		return run, nil
	}
	go readSupervisorMessages(decoder, parent, exit, termAck, started)
	return run, nil
}

func validateLaunch(launch Launch) error {
	if len(launch.Argv) == 0 || !filepath.IsAbs(launch.Argv[0]) {
		return fmt.Errorf("provider argv[0] must be absolute")
	}
	if !filepath.IsAbs(launch.Dir) || filepath.Clean(launch.Dir) != launch.Dir {
		return fmt.Errorf("provider directory must be canonical and absolute")
	}
	if launch.Stdin == nil || launch.Stdout == nil || launch.Stderr == nil {
		return fmt.Errorf("provider streams must be explicit files")
	}
	return nil
}

func supervisorSysProcAttr(cgroupFD int, pidfd *int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Unshareflags: syscall.CLONE_NEWCGROUP,
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
		UseCgroupFD:                true,
		CgroupFD:                   cgroupFD,
		PidFD:                      pidfd,
	}
}

func readControlMessage(ctx context.Context, decoder *json.Decoder, control *os.File) (controlMessage, error) {
	type result struct {
		message controlMessage
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		var message controlMessage
		err := decoder.Decode(&message)
		resultCh <- result{message: message, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.message, result.err
	case <-ctx.Done():
		control.Close()
		return controlMessage{}, ctx.Err()
	}
}

func (m *Manager) abortBeforeRelease(target *linuxTarget, encoder *json.Encoder, waited <-chan struct{}, control *os.File, cause error) error {
	_ = encoder.Encode(controlMessage{Kind: "abort"})
	control.Close()
	ctx, cancel := context.WithTimeout(context.Background(), m.timings.InterruptGrace+m.timings.KillGrace)
	defer cancel()
	status, stopErr := newHandle(target, m.timings).Stop(ctx, StopDiscard)
	if status != TerminationConfirmed {
		return errors.Join(cause, fmt.Errorf("trusted supervisor teardown unconfirmed: %w", stopErr))
	}
	select {
	case <-waited:
	case <-ctx.Done():
		return errors.Join(cause, ctx.Err())
	}
	return cause
}

func (m *Manager) verifySupervisor(hostPID int, target *linuxTarget, ready supervisorReady) (Identity, error) {
	if ready.PID != 1 || ready.Cgroup != "/" {
		return Identity{}, fmt.Errorf("trusted child reports pid/cgroup (%d, %q), want (1, /)", ready.PID, ready.Cgroup)
	}
	procPID, err := openProcPID(int(m.proc.Fd()), uint32(hostPID))
	if err != nil {
		return Identity{}, err
	}
	defer procPID.Close()
	cgroupData, err := readAt(int(procPID.Fd()), "cgroup")
	if err != nil {
		return Identity{}, err
	}
	membership, err := parseUnifiedCgroup(string(cgroupData))
	if err != nil {
		return Identity{}, err
	}
	wantMembership := path.Join(m.delegatePath, runsCgroup, attemptPrefix+target.name)
	if membership != wantMembership {
		return Identity{}, fmt.Errorf("supervisor host cgroup = %q, want %q", membership, wantMembership)
	}
	state, start, err := readProcStat(int(procPID.Fd()))
	if err != nil || state == 'Z' || state == 'X' {
		if err == nil {
			err = fmt.Errorf("supervisor already exited with state %c", state)
		}
		return Identity{}, err
	}
	pidns, err := procNamespaceID(int(procPID.Fd()), "pid")
	if err != nil {
		return Identity{}, err
	}
	cgns, err := procNamespaceID(int(procPID.Fd()), "cgroup")
	if err != nil {
		return Identity{}, err
	}
	if pidns == m.daemonPIDNS || cgns == m.daemonCgroupNS {
		return Identity{}, fmt.Errorf("supervisor did not enter fresh PID and cgroup namespaces")
	}
	if err := m.verifyNoControlDescriptors(hostPID, target.leafID, pidns, cgns); err != nil {
		return Identity{}, err
	}
	if err := verifyChildMountReport(hostPID, ready.Mounts, wantMembership); err != nil {
		return Identity{}, err
	}
	return Identity{
		Boot: m.boot, Delegate: m.delegateID, Root: m.runsID, Name: target.name,
		Leaf: target.leafID, PID: uint32(hostPID), StartTicks: start, PIDNS: pidns, CgroupNS: cgns,
	}, nil
}

func (m *Manager) verifyNoControlDescriptors(pid int, leafID, pidns, cgns ObjectID) error {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return err
	}
	forbidden := map[ObjectID]string{
		m.delegateID:     "delegated root",
		m.runsID:         "attempt parent",
		leafID:           "attempt leaf",
		m.daemonPIDNS:    "daemon PID namespace",
		m.daemonCgroupNS: "daemon cgroup namespace",
		pidns:            "supervisor PID namespace",
		cgns:             "supervisor cgroup namespace",
	}
	for _, entry := range entries {
		id, err := objectIDAt(unix.AT_FDCWD, fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()), 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		if name, found := forbidden[id]; found {
			return fmt.Errorf("trusted supervisor inherited %s descriptor %s", name, entry.Name())
		}
	}
	return nil
}

func verifyChildMountReport(pid int, report mountSetupReport, cgroupRoot string) error {
	if len(report.Aliases) == 0 || report.ProcMountID == 0 || report.CgroupMountID == 0 {
		return fmt.Errorf("supervisor mount report is incomplete")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if err != nil {
		return err
	}
	records, err := parseMountInfo(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	for _, canonical := range []struct {
		id         uint64
		target     string
		filesystem string
		magic      int64
		root       string
		access     string
	}{{report.ProcMountID, "/proc", "proc", unix.PROC_SUPER_MAGIC, "/", "rw"},
		{report.CgroupMountID, canonicalCgroup, "cgroup2", unix.CGROUP2_SUPER_MAGIC, cgroupRoot, "ro"}} {
		var observed *mountRecord
		for i := range records {
			record := &records[i]
			if record.ID == canonical.id && record.Target == canonical.target &&
				record.Filesystem == canonical.filesystem && record.Root == canonical.root {
				observed = record
				break
			}
		}
		if observed == nil {
			return fmt.Errorf("child canonical %s mount not observed", canonical.filesystem)
		}
		for _, option := range []string{canonical.access, "nosuid", "nodev", "noexec"} {
			if !hasMountOption(*observed, option) {
				return fmt.Errorf("child canonical %s mount lacks %s", canonical.filesystem, option)
			}
		}
		probePath := fmt.Sprintf("/proc/%d/root%s", pid, canonical.target)
		id, err := mountIDAt(unix.AT_FDCWD, probePath, 0)
		if err != nil {
			return fmt.Errorf("resolve child canonical %s mount: %w", canonical.filesystem, err)
		}
		if id != canonical.id {
			return fmt.Errorf("child canonical %s mount id = %d, want %d", canonical.filesystem, id, canonical.id)
		}
		fd, err := unix.Open(probePath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open child canonical %s mount: %w", canonical.filesystem, err)
		}
		filesystemErr := requireFilesystem(fd, canonical.magic)
		unix.Close(fd)
		if filesystemErr != nil {
			return fmt.Errorf("verify child canonical %s mount: %w", canonical.filesystem, filesystemErr)
		}
	}
	for _, alias := range report.Aliases {
		magic, knownFilesystem := filesystemMagic(alias.Filesystem)
		if alias.ID == 0 || alias.CoveredBy == 0 || alias.CoveredBy == alias.ID ||
			!validMountRoot(alias.Root) || !knownFilesystem || alias.Magic != magic {
			return fmt.Errorf("invalid cover report for %q", alias.Target)
		}
		probePath := fmt.Sprintf("/proc/%d/root%s", pid, alias.Target)
		id, err := mountIDAt(unix.AT_FDCWD, probePath, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("observe child alias %q: %w", alias.Target, err)
		}
		if id == alias.ID || id != alias.CoveredBy {
			return fmt.Errorf("child alias %q resolves to mount %d; old=%d cover=%d", alias.Target, id, alias.ID, alias.CoveredBy)
		}
	}
	return nil
}

func readSupervisorMessages(decoder *json.Decoder, control *os.File, exits chan ProviderExit, termAck, started chan struct{}) {
	defer control.Close()
	defer close(exits)
	var startOnce, ackOnce sync.Once
	delivered := false
	lastError := ""
	for {
		var message controlMessage
		if err := decoder.Decode(&message); err != nil {
			if !errors.Is(err, io.EOF) && lastError == "" {
				lastError = err.Error()
			}
			break
		}
		switch message.Kind {
		case "started":
			startOnce.Do(func() { close(started) })
		case "term_ack":
			ackOnce.Do(func() { close(termAck) })
		case "error":
			lastError = message.Error
		case "exit":
			if message.Exit != nil && !delivered {
				exits <- *message.Exit
				delivered = true
			}
		}
	}
	if !delivered {
		if lastError == "" {
			lastError = "supervisor exited before reporting provider status"
		}
		exits <- ProviderExit{StartError: lastError}
	}
}

func (m *Manager) capabilityCanary(ctx context.Context) (retErr error) {
	siblingNonce, sibling, siblingID, err := m.createAttemptDirectory()
	if err != nil {
		return err
	}
	sibling.Close()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), m.timings.CleanupPass)
		defer cancel()
		removed, cleanupErr := m.removeAttempt(cleanupCtx, siblingNonce, siblingID, m.timings.CleanupNodes)
		if cleanupErr != nil || !removed {
			if cleanupErr == nil {
				cleanupErr = fmt.Errorf("canary sibling remained populated")
			}
			retErr = errors.Join(retErr, fmt.Errorf("remove canary sibling: %w", cleanupErr))
		}
	}()
	nullIn, err := os.Open("/dev/null")
	if err != nil {
		return err
	}
	defer nullIn.Close()
	nullOut, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer nullOut.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdoutR.Close()
	run, err := m.startAttempt(ctx, Launch{
		Argv: []string{m.options.Executable, canaryArg}, Dir: "/",
		Env: []string{
			canarySiblingEnv + "=" + attemptPrefix + siblingNonce,
			fmt.Sprintf("%s=%d", canaryDaemonEnv, os.Getpid()),
		},
		Stdin: nullIn, Stdout: stdoutW, Stderr: nullOut,
	})
	stdoutW.Close()
	if err != nil {
		return err
	}
	// Readiness is allowed to fail at every later observation. None of those
	// failures transfers ownership of the already-created canary away from the
	// provider, so always drive its bounded teardown before returning.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(),
			m.timings.InterruptGrace+m.timings.KillGrace+time.Second)
		defer cancel()
		status, stopErr := run.Handle.Stop(stopCtx, StopDiscard)
		if status != TerminationConfirmed {
			retErr = errors.Join(retErr, fmt.Errorf("canary cleanup unconfirmed: %w", stopErr))
		}
	}()
	select {
	case <-run.started:
	case exit := <-run.ProviderDone:
		select {
		case <-run.started:
			// The ordered control stream can make both channels ready at
			// once; started was still observed before direct completion.
		default:
			return fmt.Errorf("canary provider did not start: %+v", exit)
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.timings.InterruptGrace):
		return fmt.Errorf("canary provider start timed out")
	}
	identity, err := ParseEvidence(run.Evidence)
	if err != nil {
		return err
	}
	type reportResult struct {
		report canaryProviderReport
		err    error
	}
	reportCh := make(chan reportResult, 1)
	go func() {
		var report canaryProviderReport
		err := json.NewDecoder(io.LimitReader(stdoutR, 64*1024)).Decode(&report)
		reportCh <- reportResult{report: report, err: err}
	}()
	var providerReport canaryProviderReport
	select {
	case result := <-reportCh:
		if result.err != nil {
			return fmt.Errorf("decode canary report: %w", result.err)
		}
		providerReport = result.report
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.timings.InterruptGrace):
		return fmt.Errorf("canary report timed out")
	}
	nestedCgroup, nestedProc, migrationRejected, err := m.inspectCanary(identity, providerReport)
	if err != nil {
		return err
	}
	if err := m.verifyDaemonMembership(); err != nil {
		return err
	}
	opened, err := unix.PidfdOpen(int(identity.PID), 0)
	if err != nil {
		return fmt.Errorf("pidfd_open: %w", err)
	}
	unix.Close(opened)
	stopCtx, cancel := context.WithTimeout(ctx, m.timings.InterruptGrace+m.timings.KillGrace+time.Second)
	defer cancel()
	status, err := run.Handle.Stop(stopCtx, StopDiscard)
	if status != TerminationConfirmed {
		return fmt.Errorf("canary teardown unconfirmed: %w", err)
	}
	select {
	case <-run.termAck:
	case <-time.After(max(m.timings.PollInterval, 100*time.Millisecond)):
		return fmt.Errorf("pidfd SIGTERM was not acknowledged by supervisor handler")
	}
	m.queue.signal()
	deadline := time.NewTimer(m.timings.CleanupPass + 5*m.timings.CleanupRetry)
	defer deadline.Stop()
	ticker := time.NewTicker(m.timings.PollInterval)
	defer ticker.Stop()
	for {
		leaf, err := openAtDir(int(m.runs.Fd()), attemptPrefix+identity.Name, false)
		if isNotExist(err) {
			report := capabilityReport{
				UnifiedV2: true, NSDelegate: true, WritableDelegate: true, CgroupKill: true,
				Openat2: true, Statx: true, Clone3Placement: true, CgroupUnshare: true,
				UserPIDMountNS: true, MountCover: true, PidfdOpen: true, PidfdSignal: true,
				MigrationRejected: migrationRejected, Cleanup: true,
				NestedCgroupMount: nestedCgroup, NestedProcMount: nestedProc,
			}
			if err := validateCapabilityReport(report); err != nil {
				return err
			}
			m.mu.Lock()
			m.canaryReport = report
			m.mu.Unlock()
			return nil
		}
		if err == nil {
			leaf.Close()
		} else {
			return err
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("canary janitor did not remove attempt leaf")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) verifyDaemonMembership() error {
	procPID, err := openProcPID(int(m.proc.Fd()), uint32(os.Getpid()))
	if err != nil {
		return err
	}
	defer procPID.Close()
	data, err := readAt(int(procPID.Fd()), "cgroup")
	if err != nil {
		return err
	}
	membership, err := parseUnifiedCgroup(string(data))
	if err != nil {
		return err
	}
	want := path.Join(m.delegatePath, supervisorCgroup)
	if membership != want {
		return fmt.Errorf("canary moved daemon to %q, want %q", membership, want)
	}
	return nil
}

func (m *Manager) inspectCanary(identity Identity, report canaryProviderReport) (nestedMountResult, nestedMountResult, bool, error) {
	if report.CgroupMount < nestedMountDenied || report.CgroupMount > nestedMountExposed ||
		report.ProcMount < nestedMountDenied || report.ProcMount > nestedMountExposed {
		return nestedMountUnknown, nestedMountUnknown, false, fmt.Errorf("canary reported invalid nested outcome")
	}
	if !report.MigrationAttempt {
		return nestedMountUnknown, nestedMountUnknown, false, fmt.Errorf(
			"canary migration proof incomplete: canonical=%v aliases=%v nested=%v guessed=%v keeper=%v cgroup=%v (%s) proc=%v (%s)",
			report.CanonicalRejected, report.AliasesRejected, report.NestedRejected,
			report.GuessedHostAttempted, report.KeeperStarted, report.CgroupMount, report.CgroupError,
			report.ProcMount, report.ProcError)
	}
	expected := path.Join(m.delegatePath, runsCgroup, attemptPrefix+identity.Name)
	cgroupFound := false
	procFound := false
	sawSupervisor := false
	err := m.forEachPIDDomainProcess(identity.PIDNS, func(pid uint32, procPID *os.File) error {
		membershipData, err := readAt(int(procPID.Fd()), "cgroup")
		if err != nil {
			return fmt.Errorf("read cgroup membership: %w", err)
		}
		membership, err := parseUnifiedCgroup(string(membershipData))
		if err != nil {
			return fmt.Errorf("parse cgroup membership: %w", err)
		}
		mountData, err := readAt(int(procPID.Fd()), "mountinfo")
		if err != nil {
			return fmt.Errorf("read mountinfo: %w", err)
		}
		records, err := parseMountInfo(strings.NewReader(string(mountData)))
		if err != nil {
			return fmt.Errorf("parse mountinfo: %w", err)
		}
		if membership != expected && !strings.HasPrefix(membership, expected+"/") {
			return fmt.Errorf("canary process %d migrated to %q outside %q", pid, membership, expected)
		}
		if pid == identity.PID {
			sawSupervisor = true
		}
		for _, mount := range records {
			switch {
			case mount.Filesystem == "cgroup2" && strings.HasPrefix(mount.Target, "/tmp/.ben-localdomain-nested-cgroup2-"):
				if mount.Root != expected && !strings.HasPrefix(mount.Root, expected+"/") {
					return fmt.Errorf("nested cgroup2 root = %q, want %q or descendant", mount.Root, expected)
				}
				cgroupNS, namespaceErr := procNamespaceID(int(procPID.Fd()), "cgroup")
				userNS, userErr := procNamespaceID(int(procPID.Fd()), "user")
				owner, ownerErr := namespaceRelatedID(int(procPID.Fd()), "cgroup", unix.NS_GET_USERNS)
				if namespaceErr != nil || userErr != nil || ownerErr != nil {
					return fmt.Errorf("observe nested cgroup namespaces: %w",
						errors.Join(namespaceErr, userErr, ownerErr))
				}
				if cgroupNS == identity.CgroupNS || owner != userNS {
					return fmt.Errorf("nested cgroup namespace was not freshly owned: cgroup=%v owner=%v user=%v", cgroupNS, owner, userNS)
				}
				cgroupFound = true
			case mount.Filesystem == "proc" && strings.HasPrefix(mount.Target, "/tmp/.ben-localdomain-nested-proc-"):
				if mount.Root != "/" {
					return fmt.Errorf("nested proc root = %q", mount.Root)
				}
				pidNS, namespaceErr := procNamespaceID(int(procPID.Fd()), "pid")
				userNS, userErr := procNamespaceID(int(procPID.Fd()), "user")
				parent, parentErr := namespaceRelatedID(int(procPID.Fd()), "pid", unix.NS_GET_PARENT)
				owner, ownerErr := namespaceRelatedID(int(procPID.Fd()), "pid", unix.NS_GET_USERNS)
				if namespaceErr != nil || userErr != nil || parentErr != nil || ownerErr != nil {
					return fmt.Errorf("observe nested PID namespaces: %w",
						errors.Join(namespaceErr, userErr, parentErr, ownerErr))
				}
				if parentErr != nil || parent != identity.PIDNS {
					return fmt.Errorf("nested PID namespace parent = %v, want %v", parent, identity.PIDNS)
				}
				if pidNS == identity.PIDNS || owner != userNS {
					return fmt.Errorf("nested PID namespace was not freshly owned: pid=%v owner=%v user=%v", pidNS, owner, userNS)
				}
				procFound = true
			}
		}
		return nil
	})
	if err != nil {
		return nestedMountExposed, nestedMountExposed, false, err
	}
	if !sawSupervisor {
		return nestedMountUnknown, nestedMountUnknown, false, fmt.Errorf("canary supervisor was absent from its recorded PID namespace")
	}
	if report.CgroupMount == nestedMountContained && !cgroupFound || report.CgroupMount == nestedMountDenied && cgroupFound {
		return nestedMountUnknown, nestedMountUnknown, false, fmt.Errorf("nested cgroup2 report/host observation disagree")
	}
	if report.ProcMount == nestedMountContained && !procFound || report.ProcMount == nestedMountDenied && procFound {
		return nestedMountUnknown, nestedMountUnknown, false, fmt.Errorf("nested proc report/host observation disagree")
	}
	return report.CgroupMount, report.ProcMount, report.MigrationAttempt, nil
}

func (m *Manager) forEachPIDDomainProcess(root ObjectID, visit func(uint32, *os.File) error) error {
	fd, err := openAt(int(m.proc.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), "proc-domain-scan")
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		pid64, parseErr := parsePositiveDecimal(entry.Name(), 32)
		if parseErr != nil {
			continue
		}
		procPID, openErr := openProcPID(int(m.proc.Fd()), uint32(pid64))
		if errors.Is(openErr, unix.ENOENT) {
			continue
		}
		if openErr != nil {
			// Unrelated processes can make their proc namespace links
			// unreadable. Every process in this provider domain retains the
			// service UID and is independently found below.
			continue
		}
		inside, relationErr := pidNamespaceAtOrBelow(int(procPID.Fd()), root)
		if errors.Is(relationErr, unix.ENOENT) || errors.Is(relationErr, unix.EPERM) || errors.Is(relationErr, unix.EACCES) {
			procPID.Close()
			continue
		}
		if relationErr != nil {
			procPID.Close()
			return fmt.Errorf("classify pid %d namespace: %w", pid64, relationErr)
		}
		if inside {
			if err := visit(uint32(pid64), procPID); err != nil {
				procPID.Close()
				return fmt.Errorf("inspect pid %d: %w", pid64, err)
			}
		}
		procPID.Close()
	}
	return nil
}

func pidNamespaceAtOrBelow(procPIDFD int, root ObjectID) (bool, error) {
	current, err := unix.Openat(procPIDFD, "ns/pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	for depth := 0; depth <= 32; depth++ {
		identity, identityErr := objectAnyIDFD(current)
		if identityErr != nil {
			unix.Close(current)
			return false, identityErr
		}
		if identity == root {
			unix.Close(current)
			return true, nil
		}
		parent, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(current), uintptr(unix.NS_GET_PARENT), 0)
		unix.Close(current)
		if errors.Is(errno, unix.EPERM) {
			return false, nil
		}
		if errno != 0 {
			return false, errno
		}
		current = int(parent)
	}
	unix.Close(current)
	return false, fmt.Errorf("PID namespace nesting exceeds kernel maximum")
}

func namespaceRelatedID(procPIDFD int, kind string, operation uint) (ObjectID, error) {
	fd, err := unix.Openat(procPIDFD, "ns/"+kind, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ObjectID{}, err
	}
	defer unix.Close(fd)
	related, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(operation), 0)
	if errno != 0 {
		return ObjectID{}, errno
	}
	relatedFD := int(related)
	defer unix.Close(relatedFD)
	return objectAnyIDFD(relatedFD)
}
