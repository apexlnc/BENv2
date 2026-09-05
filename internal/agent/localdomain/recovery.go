package localdomain

import (
	"context"
	"errors"
)

type recoveryReader interface {
	bootID() (string, error)
	supervisor(Identity) (supervisorState, error)
	cgroup(Identity) (cgroupState, error)
}

func recoverEvidence(ctx context.Context, evidence Evidence, reader recoveryReader) (Termination, error) {
	identity, err := ParseEvidence(evidence)
	if err != nil {
		return TerminationUnconfirmed, err
	}
	boot, err := reader.bootID()
	if err != nil {
		return TerminationUnconfirmed, err
	}
	if identity.Boot != boot {
		return TerminationConfirmed, nil
	}
	if ctx.Err() != nil {
		return TerminationUnconfirmed, ctx.Err()
	}
	supervisor, supervisorErr := reader.supervisor(identity)
	cgroup, cgroupErr := reader.cgroup(identity)
	return decide(observation{
		supervisor: supervisor,
		cgroup:     cgroup,
		err:        errors.Join(supervisorErr, cgroupErr),
	})
}

type processSnapshot struct {
	StartTicks  uint64
	PIDNS       ObjectID
	CgroupNS    ObjectID
	State       byte
	PidfdExited bool
}

func classifyCgroup(identity Identity, delegate, root ObjectID, leaf *ObjectID, populated bool) cgroupState {
	if delegate != identity.Delegate || root != identity.Root {
		return cgroupReplaced
	}
	if leaf == nil {
		return cgroupAbsent
	}
	if *leaf != identity.Leaf {
		return cgroupReplaced
	}
	if populated {
		return cgroupPopulated
	}
	return cgroupEmpty
}

func classifyProcess(identity Identity, snapshot processSnapshot) (supervisorState, error) {
	if snapshot.PidfdExited || snapshot.State == 'Z' || snapshot.State == 'X' {
		return supervisorExited, nil
	}
	if snapshot.StartTicks != identity.StartTicks || snapshot.PIDNS != identity.PIDNS {
		return supervisorExited, nil
	}
	if snapshot.CgroupNS != identity.CgroupNS {
		return supervisorUnknown, errors.New("recorded supervisor changed cgroup namespace")
	}
	return supervisorLive, nil
}
