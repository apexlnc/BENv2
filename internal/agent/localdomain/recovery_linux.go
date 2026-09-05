//go:build linux

package localdomain

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Recover performs the restart-safe, read-only supervisor and cgroup
// observation. It never signals, removes, or registers cleanup work.
func (m *Manager) Recover(ctx context.Context, evidence Evidence) (Termination, error) {
	return recoverEvidence(ctx, evidence, managerRecoveryReader{manager: m})
}

type managerRecoveryReader struct{ manager *Manager }

func (r managerRecoveryReader) bootID() (string, error) { return readBootID() }

func (r managerRecoveryReader) supervisor(identity Identity) (supervisorState, error) {
	r.manager.mu.Lock()
	ready := r.manager.ready && !r.manager.closed
	r.manager.mu.Unlock()
	if !ready {
		return supervisorUnknown, fmt.Errorf("%w: manager is not ready", ErrUnavailable)
	}
	return r.manager.recoverSupervisor(identity)
}

func (r managerRecoveryReader) cgroup(identity Identity) (cgroupState, error) {
	r.manager.mu.Lock()
	ready := r.manager.ready && !r.manager.closed
	r.manager.mu.Unlock()
	if !ready {
		return cgroupUnknown, fmt.Errorf("%w: manager is not ready", ErrUnavailable)
	}
	return r.manager.recoverCgroup(identity)
}

func (m *Manager) recoverSupervisor(identity Identity) (supervisorState, error) {
	pidfd, err := unix.PidfdOpen(int(identity.PID), 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return supervisorExited, nil
		}
		return supervisorUnknown, fmt.Errorf("pidfd_open: %w", err)
	}
	defer unix.Close(pidfd)
	exited, err := pidfdExited(pidfd)
	if err != nil {
		return supervisorUnknown, err
	}
	if exited {
		return supervisorExited, nil
	}
	procPID, err := openProcPID(int(m.proc.Fd()), identity.PID)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return supervisorExited, nil
		}
		return supervisorUnknown, err
	}
	defer procPID.Close()
	state, start, err := readProcStat(int(procPID.Fd()))
	if err != nil {
		return supervisorUnknown, err
	}
	pidns, err := procNamespaceID(int(procPID.Fd()), "pid")
	if err != nil {
		return supervisorUnknown, err
	}
	cgns, err := procNamespaceID(int(procPID.Fd()), "cgroup")
	if err != nil {
		return supervisorUnknown, err
	}
	exitedAfter, err := pidfdExited(pidfd)
	if err != nil {
		return supervisorUnknown, err
	}
	return classifyProcess(identity, processSnapshot{
		StartTicks: start, PIDNS: pidns, CgroupNS: cgns, State: state,
		PidfdExited: exited || exitedAfter,
	})
}

func (m *Manager) recoverCgroup(identity Identity) (cgroupState, error) {
	delegateID, err := objectIDFD(int(m.delegate.Fd()))
	if err != nil {
		return cgroupUnknown, err
	}
	rootID, err := objectIDFD(int(m.runs.Fd()))
	if err != nil {
		return cgroupUnknown, err
	}
	if delegateID != identity.Delegate || rootID != identity.Root {
		return classifyCgroup(identity, delegateID, rootID, nil, false), nil
	}
	leaf, err := openAtDir(int(m.runs.Fd()), attemptPrefix+identity.Name, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return classifyCgroup(identity, delegateID, rootID, nil, false), nil
		}
		return cgroupUnknown, err
	}
	defer leaf.Close()
	leafID, err := objectIDFD(int(leaf.Fd()))
	if err != nil {
		return cgroupUnknown, err
	}
	if state := classifyCgroup(identity, delegateID, rootID, &leafID, false); state == cgroupReplaced {
		return state, nil
	}
	populated, err := readPopulation(int(leaf.Fd()))
	if err != nil {
		return cgroupUnknown, err
	}
	return classifyCgroup(identity, delegateID, rootID, &leafID, populated), nil
}
