//go:build linux

package localdomain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	supervisorCgroup = "supervisor"
	runsCgroup       = "ben-runs"
)

// Manager owns the delegated cgroup descriptors and process-lifetime janitor.
type Manager struct {
	options Options
	timings Timings
	random  io.Reader

	readyOnce sync.Once
	readyErr  error
	ready     bool

	mu             sync.Mutex
	closed         bool
	delegate       *os.File
	runs           *os.File
	proc           *os.File
	delegateID     ObjectID
	runsID         ObjectID
	delegatePath   string
	daemonPIDNS    ObjectID
	daemonCgroupNS ObjectID
	boot           string
	queue          *cleanupQueue
	canaryReport   capabilityReport
	closeOnce      sync.Once
}

// New constructs a dormant manager. Kernel discovery and the capability
// canary occur on Ready so normal commands do not mutate cgroups merely by
// importing or assembling the package.
func New(options Options) *Manager {
	options.Timings = options.Timings.withDefaults()
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &Manager{
		options: options,
		timings: options.Timings,
		random:  options.Random,
		queue:   newCleanupQueue(options.Timings),
	}
}

// Ready performs discovery, startup cleanup, and a disposable launch canary
// once. Start repeats the health check so later janitor degradation fails shut.
func (m *Manager) Ready(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.readyOnce.Do(func() {
		m.readyErr = m.setup(ctx)
		m.mu.Lock()
		m.ready = m.readyErr == nil
		m.mu.Unlock()
	})
	if m.readyErr != nil {
		return m.readyErr
	}
	if err := m.queue.health(); err != nil {
		return err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: manager is closed", ErrUnavailable)
	}
	return nil
}

func (m *Manager) setup(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	executable := m.options.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return unavailable("resolve supervisor executable", err)
		}
		m.options.Executable = executable
	}
	if !filepath.IsAbs(executable) {
		return unavailable("supervisor executable is not absolute", nil)
	}
	boot, err := readBootID()
	if err != nil {
		return unavailable("read boot identity", err)
	}
	procFD, err := unix.Open("/proc", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return unavailable("open procfs", err)
	}
	proc := os.NewFile(uintptr(procFD), "procfs")
	if err := requireFilesystem(procFD, unix.PROC_SUPER_MAGIC); err != nil {
		proc.Close()
		return unavailable("validate procfs", err)
	}
	selfProc, err := openProcPID(procFD, uint32(os.Getpid()))
	if err != nil {
		proc.Close()
		return unavailable("open daemon proc identity", err)
	}
	daemonPIDNS, pidNSErr := procNamespaceID(int(selfProc.Fd()), "pid")
	daemonCgroupNS, cgroupNSErr := procNamespaceID(int(selfProc.Fd()), "cgroup")
	selfProc.Close()
	if pidNSErr != nil || cgroupNSErr != nil {
		proc.Close()
		return unavailable("identify daemon namespaces", errors.Join(pidNSErr, cgroupNSErr))
	}
	root, rootID, delegatePath, err := discoverDelegatedRoot(m.options.DelegatedRoot)
	if err != nil {
		proc.Close()
		return unavailable("discover delegated cgroup", err)
	}
	supervisor, err := ensureCgroup(root, supervisorCgroup, false)
	if err != nil {
		root.Close()
		proc.Close()
		return unavailable("create supervisor cgroup", err)
	}
	runs, err := ensureCgroup(root, runsCgroup, true)
	if err != nil {
		supervisor.Close()
		root.Close()
		proc.Close()
		return unavailable("create attempt root", err)
	}
	runsID, err := objectIDFD(int(runs.Fd()))
	if err != nil {
		runs.Close()
		supervisor.Close()
		root.Close()
		proc.Close()
		return unavailable("identify attempt root", err)
	}
	if err := writeAt(int(supervisor.Fd()), "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		runs.Close()
		supervisor.Close()
		root.Close()
		proc.Close()
		return unavailable("move daemon to supervisor cgroup", err)
	}
	rootProcs, err := readAt(int(root.Fd()), "cgroup.procs")
	if err != nil || strings.TrimSpace(string(rootProcs)) != "" {
		runs.Close()
		supervisor.Close()
		root.Close()
		proc.Close()
		if err == nil {
			err = fmt.Errorf("delegated root still contains processes")
		}
		return unavailable("enforce no-internal-process layout", err)
	}
	supervisor.Close()
	m.delegate = root
	m.runs = runs
	m.proc = proc
	m.delegateID = rootID
	m.runsID = runsID
	m.delegatePath = delegatePath
	m.daemonPIDNS = daemonPIDNS
	m.daemonCgroupNS = daemonCgroupNS
	m.boot = boot
	if err := m.startupSweep(ctx); err != nil {
		return unavailable("startup cgroup sweep", err)
	}
	if err := m.capabilityCanary(ctx); err != nil {
		return unavailable("execution-domain canary", err)
	}
	return nil
}

func discoverDelegatedRoot(explicit string) (*os.File, ObjectID, string, error) {
	cgroupData, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, ObjectID{}, "", err
	}
	membership, err := parseUnifiedCgroup(string(cgroupData))
	if err != nil {
		return nil, ObjectID{}, "", err
	}
	mountData, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, ObjectID{}, "", err
	}
	records, parseErr := parseMountInfo(mountData)
	mountData.Close()
	if parseErr != nil {
		return nil, ObjectID{}, "", parseErr
	}
	type candidate struct {
		record mountRecord
		path   string
	}
	var candidates []candidate
	for _, record := range records {
		if record.Filesystem != "cgroup2" {
			continue
		}
		relative, ok := beneathCgroupRoot(record.Root, membership)
		if !ok {
			continue
		}
		path := record.Target
		if relative != "" {
			path = filepath.Join(path, filepath.FromSlash(relative))
		}
		candidates = append(candidates, candidate{record: record, path: path})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return len(candidates[i].record.Root) > len(candidates[j].record.Root)
	})
	for _, candidate := range candidates {
		fd, err := unix.Open(candidate.path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		if err := requireFilesystem(fd, unix.CGROUP2_SUPER_MAGIC); err != nil {
			unix.Close(fd)
			continue
		}
		if !hasMountOption(candidate.record, "nsdelegate") {
			unix.Close(fd)
			return nil, ObjectID{}, "", fmt.Errorf("cgroup2 mount lacks nsdelegate")
		}
		id, err := objectIDFD(fd)
		if err != nil {
			unix.Close(fd)
			return nil, ObjectID{}, "", err
		}
		if explicit != "" {
			if !filepath.IsAbs(explicit) || filepath.Clean(explicit) != explicit {
				unix.Close(fd)
				return nil, ObjectID{}, "", fmt.Errorf("noncanonical explicit root %q", explicit)
			}
			explicitFD, openErr := unix.Open(explicit, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if openErr != nil {
				unix.Close(fd)
				return nil, ObjectID{}, "", openErr
			}
			explicitID, identityErr := objectIDFD(explicitFD)
			unix.Close(explicitFD)
			if identityErr != nil || explicitID != id {
				unix.Close(fd)
				if identityErr == nil {
					identityErr = fmt.Errorf("explicit root is not current delegated membership")
				}
				return nil, ObjectID{}, "", identityErr
			}
		}
		return os.NewFile(uintptr(fd), "delegated-cgroup"), id, membership, nil
	}
	return nil, ObjectID{}, "", fmt.Errorf("no matching unified cgroup2 mount")
}

func beneathCgroupRoot(root, membership string) (string, bool) {
	if root == "/" {
		return strings.TrimPrefix(membership, "/"), true
	}
	if membership == root {
		return "", true
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(membership, prefix) {
		return "", false
	}
	return strings.TrimPrefix(membership, prefix), true
}

func ensureCgroup(parent *os.File, name string, readable bool) (*os.File, error) {
	err := unix.Mkdirat(int(parent.Fd()), name, 0o755)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	return openAtDir(int(parent.Fd()), name, readable)
}

func (m *Manager) startupSweep(ctx context.Context) error {
	dup, err := unix.Dup(int(m.runs.Fd()))
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "attempt-root-sweep")
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !validAttemptDir(name) {
			return fmt.Errorf("unexpected cgroup directory %q", name)
		}
		leaf, err := openAtDir(int(m.runs.Fd()), name, true)
		if err != nil {
			return err
		}
		leafID, err := objectIDFD(int(leaf.Fd()))
		if err != nil {
			leaf.Close()
			return err
		}
		target := newLinuxTarget(m, strings.TrimPrefix(name, attemptPrefix), leaf, leafID)
		populated, err := readPopulation(int(leaf.Fd()))
		if err != nil {
			target.close()
			return err
		}
		if !populated {
			removed, removeErr := target.remove(ctx, m.timings.CleanupNodes)
			if removeErr != nil || !removed {
				target.close()
				if removeErr == nil {
					removeErr = fmt.Errorf("empty attempt %q survived cleanup", name)
				}
				return removeErr
			}
			target.close()
			continue
		}
		if err := m.queue.register(name, target, false); err != nil {
			target.close()
			return err
		}
	}
	return nil
}

func validAttemptDir(name string) bool {
	return strings.HasPrefix(name, attemptPrefix) && validNonce(strings.TrimPrefix(name, attemptPrefix))
}

func (m *Manager) createAttempt() (*linuxTarget, error) {
	if err := m.queue.health(); err != nil {
		return nil, err
	}
	nonce, leaf, leafID, err := m.createAttemptDirectory()
	if err != nil {
		return nil, err
	}
	name := attemptPrefix + nonce
	target := newLinuxTarget(m, nonce, leaf, leafID)
	if err := m.queue.register(name, target, true); err != nil {
		target.markNoSupervisor()
		target.close()
		_ = unix.Unlinkat(int(m.runs.Fd()), name, unix.AT_REMOVEDIR)
		return nil, err
	}
	return target, nil
}

func (m *Manager) createAttemptDirectory() (string, *os.File, ObjectID, error) {
	for tries := 0; tries < 8; tries++ {
		var nonceBytes [16]byte
		if _, err := io.ReadFull(m.random, nonceBytes[:]); err != nil {
			return "", nil, ObjectID{}, err
		}
		nonce := hex.EncodeToString(nonceBytes[:])
		name := attemptPrefix + nonce
		if err := unix.Mkdirat(int(m.runs.Fd()), name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", nil, ObjectID{}, err
		}
		leaf, err := openAtDir(int(m.runs.Fd()), name, true)
		if err != nil {
			_ = unix.Unlinkat(int(m.runs.Fd()), name, unix.AT_REMOVEDIR)
			return "", nil, ObjectID{}, err
		}
		leafID, err := objectIDFD(int(leaf.Fd()))
		if err != nil {
			leaf.Close()
			_ = unix.Unlinkat(int(m.runs.Fd()), name, unix.AT_REMOVEDIR)
			return "", nil, ObjectID{}, err
		}
		return nonce, leaf, leafID, nil
	}
	return "", nil, ObjectID{}, fmt.Errorf("eight random attempt names collided")
}

// Close stops the janitor and releases the manager's retained descriptors.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.queue.Close()
		for _, file := range []*os.File{m.runs, m.delegate, m.proc} {
			if file != nil {
				_ = file.Close()
			}
		}
	})
	return nil
}

func unavailable(action string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, action)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, action, err)
}
