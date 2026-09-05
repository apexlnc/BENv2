//go:build linux

package localdomain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type linuxTarget struct {
	manager *Manager
	name    string
	leaf    *os.File
	leafID  ObjectID

	mu           sync.Mutex
	pidfd        int
	noSupervisor bool
	exitedKnown  bool
	removed      bool
	closed       bool
}

func newLinuxTarget(manager *Manager, name string, leaf *os.File, leafID ObjectID) *linuxTarget {
	return &linuxTarget{manager: manager, name: name, leaf: leaf, leafID: leafID, pidfd: -1}
}

func (t *linuxTarget) attachPidfd(pidfd int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.noSupervisor || t.pidfd >= 0 {
		return fmt.Errorf("cleanup target %q cannot attach supervisor", t.name)
	}
	t.pidfd = pidfd
	return nil
}

func (t *linuxTarget) markNoSupervisor() {
	t.mu.Lock()
	t.noSupervisor = true
	t.exitedKnown = true
	t.mu.Unlock()
}

func (t *linuxTarget) exited() (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exitedKnown || t.noSupervisor {
		return true, nil
	}
	if t.pidfd < 0 {
		return false, nil
	}
	exited, err := pidfdExited(t.pidfd)
	if err == nil && exited {
		t.exitedKnown = true
	}
	return exited, err
}

func (t *linuxTarget) empty() (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.removed {
		return true, nil
	}
	state, err := t.cgroupStateLocked()
	if err != nil {
		return false, err
	}
	return state == cgroupEmpty || state == cgroupAbsent || state == cgroupReplaced, nil
}

func (t *linuxTarget) observe(ctx context.Context) observation {
	if ctx.Err() != nil {
		return observation{supervisor: supervisorUnknown, cgroup: cgroupUnknown, err: ctx.Err()}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	supervisor := supervisorUnknown
	var observedErr error
	if t.exitedKnown || t.noSupervisor {
		supervisor = supervisorExited
	} else if t.pidfd >= 0 {
		exited, err := pidfdExited(t.pidfd)
		if err != nil {
			observedErr = errors.Join(observedErr, err)
		} else if exited {
			t.exitedKnown = true
			supervisor = supervisorExited
		} else {
			supervisor = supervisorLive
		}
	}
	cgroup, err := t.cgroupStateLocked()
	return observation{supervisor: supervisor, cgroup: cgroup, err: errors.Join(observedErr, err)}
}

func (t *linuxTarget) cgroupStateLocked() (cgroupState, error) {
	if t.removed {
		return cgroupAbsent, nil
	}
	current, err := openAtDir(int(t.manager.runs.Fd()), attemptPrefix+t.name, false)
	if err != nil {
		if isNotExist(err) {
			return cgroupAbsent, nil
		}
		return cgroupUnknown, err
	}
	currentID, err := objectIDFD(int(current.Fd()))
	current.Close()
	if err != nil {
		return cgroupUnknown, err
	}
	if currentID != t.leafID {
		return cgroupReplaced, nil
	}
	populated, err := readPopulation(int(t.leaf.Fd()))
	if err != nil {
		return cgroupUnknown, err
	}
	if populated {
		return cgroupPopulated, nil
	}
	return cgroupEmpty, nil
}

func (t *linuxTarget) term() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exitedKnown {
		return nil
	}
	if t.pidfd < 0 {
		return fmt.Errorf("supervisor pidfd unavailable")
	}
	return unix.PidfdSendSignal(t.pidfd, unix.SIGTERM, nil, 0)
}

func (t *linuxTarget) kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.removed {
		return nil
	}
	return writeAt(int(t.leaf.Fd()), "cgroup.kill", "1")
}

func (t *linuxTarget) remove(ctx context.Context, nodes int) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.removed {
		return true, nil
	}
	removed, err := t.manager.removeAttempt(ctx, t.name, t.leafID, nodes)
	if removed {
		t.removed = true
	}
	return removed, err
}

func (t *linuxTarget) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.leaf != nil {
		_ = t.leaf.Close()
	}
	closeFD(&t.pidfd)
}

func (m *Manager) removeAttempt(ctx context.Context, nonce string, leafID ObjectID, nodes int) (bool, error) {
	if nodes <= 0 {
		return false, fmt.Errorf("cleanup node budget exhausted")
	}
	name := attemptPrefix + nonce
	leaf, err := openAtDir(int(m.runs.Fd()), name, true)
	if err != nil {
		if isNotExist(err) {
			return true, nil
		}
		return false, err
	}
	defer leaf.Close()
	identity, err := objectIDFD(int(leaf.Fd()))
	if err != nil {
		return false, err
	}
	if identity != leafID {
		return false, fmt.Errorf("attempt %q identity changed", name)
	}
	remaining := nodes
	if err := removeChildren(ctx, int(leaf.Fd()), &remaining); err != nil {
		return false, err
	}
	populated, err := readPopulation(int(leaf.Fd()))
	if err != nil {
		return false, err
	}
	if populated {
		return false, nil
	}
	check, err := openAtDir(int(m.runs.Fd()), name, false)
	if err != nil {
		if isNotExist(err) {
			return true, nil
		}
		return false, err
	}
	checkID, checkErr := objectIDFD(int(check.Fd()))
	check.Close()
	if checkErr != nil {
		return false, checkErr
	}
	if checkID != leafID {
		return false, fmt.Errorf("attempt %q changed before removal", name)
	}
	if err := unix.Unlinkat(int(m.runs.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true, nil
		}
		if errors.Is(err, unix.EBUSY) || errors.Is(err, unix.ENOTEMPTY) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func removeChildren(ctx context.Context, parentFD int, remaining *int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dup, err := unix.Dup(parentFD)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "cgroup-cleanup")
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if *remaining <= 0 {
			return fmt.Errorf("cleanup node budget exhausted")
		}
		*remaining--
		name := entry.Name()
		if name == "." || name == ".." || strings.ContainsRune(name, '/') {
			return fmt.Errorf("invalid cgroup child name %q", name)
		}
		child, err := openAtDir(parentFD, name, true)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		childID, err := objectIDFD(int(child.Fd()))
		if err == nil {
			err = removeChildren(ctx, int(child.Fd()), remaining)
		}
		if err == nil {
			populated, popErr := readPopulation(int(child.Fd()))
			if popErr != nil {
				err = popErr
			} else if populated {
				err = unix.EBUSY
			}
		}
		child.Close()
		if err != nil {
			if errors.Is(err, unix.EBUSY) || errors.Is(err, unix.ENOTEMPTY) {
				return nil
			}
			return err
		}
		check, err := openAtDir(parentFD, name, false)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		checkID, err := objectIDFD(int(check.Fd()))
		check.Close()
		if err != nil {
			return err
		}
		if checkID != childID {
			return fmt.Errorf("cgroup child %q changed before removal", name)
		}
		if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			if errors.Is(err, unix.EBUSY) || errors.Is(err, unix.ENOTEMPTY) {
				return nil
			}
			return err
		}
	}
	return nil
}
