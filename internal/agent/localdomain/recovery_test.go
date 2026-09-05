package localdomain

import (
	"context"
	"errors"
	"testing"
)

type scriptedRecoveryReader struct {
	boot            string
	bootErr         error
	supervisorState supervisorState
	supervisorErr   error
	cgroupState     cgroupState
	cgroupErr       error
	supervisorCalls int
	cgroupCalls     int
}

func (s *scriptedRecoveryReader) bootID() (string, error) { return s.boot, s.bootErr }
func (s *scriptedRecoveryReader) supervisor(Identity) (supervisorState, error) {
	s.supervisorCalls++
	return s.supervisorState, s.supervisorErr
}
func (s *scriptedRecoveryReader) cgroup(Identity) (cgroupState, error) {
	s.cgroupCalls++
	return s.cgroupState, s.cgroupErr
}

func TestRecoveryUsesTheSharedQuietPredicate(t *testing.T) {
	evidence, err := EncodeEvidence(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("torn observation")
	cases := []struct {
		name       string
		reader     scriptedRecoveryReader
		want       Termination
		wantErr    error
		wantProbes bool
	}{
		{name: "old boot", reader: scriptedRecoveryReader{boot: "fedcba98-7654-3210-fedc-ba9876543210", supervisorState: supervisorLive, cgroupState: cgroupPopulated}, want: TerminationConfirmed},
		{name: "exact live supervisor", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorLive, cgroupState: cgroupEmpty}, want: TerminationUnconfirmed, wantProbes: true},
		{name: "exited and empty", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorExited, cgroupState: cgroupEmpty}, want: TerminationConfirmed, wantProbes: true},
		{name: "exited and replaced leaf", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorExited, cgroupState: cgroupReplaced}, want: TerminationConfirmed, wantProbes: true},
		{name: "populated veto", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorExited, cgroupState: cgroupPopulated}, want: TerminationUnconfirmed, wantErr: ErrContainment, wantProbes: true},
		{name: "supervisor observation error", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorUnknown, supervisorErr: sentinel, cgroupState: cgroupEmpty}, want: TerminationUnconfirmed, wantErr: sentinel, wantProbes: true},
		{name: "cgroup observation error", reader: scriptedRecoveryReader{boot: testIdentity().Boot, supervisorState: supervisorExited, cgroupState: cgroupUnknown, cgroupErr: sentinel}, want: TerminationUnconfirmed, wantErr: sentinel, wantProbes: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader
			got, err := recoverEvidence(context.Background(), evidence, &reader)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("recover = (%v, %v), want (%v, %v)", got, err, tc.want, tc.wantErr)
			}
			probed := reader.supervisorCalls == 1 && reader.cgroupCalls == 1
			if probed != tc.wantProbes {
				t.Fatalf("probe calls = (%d, %d), want probes %v", reader.supervisorCalls, reader.cgroupCalls, tc.wantProbes)
			}
		})
	}
}

func TestRecoveryRejectsEvidenceBeforeObservation(t *testing.T) {
	reader := &scriptedRecoveryReader{boot: testIdentity().Boot}
	got, err := recoverEvidence(context.Background(), Evidence{Scheme: EvidenceScheme}, reader)
	if got != TerminationUnconfirmed || !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("recover = (%v, %v)", got, err)
	}
	if reader.supervisorCalls != 0 || reader.cgroupCalls != 0 {
		t.Fatal("invalid evidence reached observation")
	}
}

func TestRecoveryCancellationIsUnconfirmedAndReadOnly(t *testing.T) {
	evidence, err := EncodeEvidence(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &scriptedRecoveryReader{boot: testIdentity().Boot}
	got, err := recoverEvidence(ctx, evidence, reader)
	if got != TerminationUnconfirmed || !errors.Is(err, context.Canceled) {
		t.Fatalf("recover = (%v, %v)", got, err)
	}
	if reader.supervisorCalls != 0 || reader.cgroupCalls != 0 {
		t.Fatal("cancelled recovery reached observation")
	}
}

func TestRecoveredProcessIdentityClassification(t *testing.T) {
	id := testIdentity()
	exact := processSnapshot{
		StartTicks: id.StartTicks,
		PIDNS:      id.PIDNS,
		CgroupNS:   id.CgroupNS,
		State:      'S',
	}
	cases := []struct {
		name    string
		mutate  func(*processSnapshot)
		want    supervisorState
		wantErr bool
	}{
		{name: "exact live", want: supervisorLive},
		{name: "pidfd readable", mutate: func(s *processSnapshot) { s.PidfdExited = true }, want: supervisorExited},
		{name: "zombie", mutate: func(s *processSnapshot) { s.State = 'Z' }, want: supervisorExited},
		{name: "dead state", mutate: func(s *processSnapshot) { s.State = 'X' }, want: supervisorExited},
		{name: "start-time reuse", mutate: func(s *processSnapshot) { s.StartTicks++ }, want: supervisorExited},
		{name: "pid namespace reuse", mutate: func(s *processSnapshot) { s.PIDNS.Inode++ }, want: supervisorExited},
		{name: "cgroup namespace only mismatch", mutate: func(s *processSnapshot) { s.CgroupNS.Inode++ }, want: supervisorUnknown, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := exact
			if tc.mutate != nil {
				tc.mutate(&snapshot)
			}
			got, err := classifyProcess(id, snapshot)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("classify = (%v, %v), want state %v error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestRecoveredCgroupIdentityClassification(t *testing.T) {
	id := testIdentity()
	cases := []struct {
		name      string
		delegate  ObjectID
		root      ObjectID
		leaf      *ObjectID
		populated bool
		want      cgroupState
	}{
		{name: "matching empty", delegate: id.Delegate, root: id.Root, leaf: objectPointer(id.Leaf), want: cgroupEmpty},
		{name: "matching populated", delegate: id.Delegate, root: id.Root, leaf: objectPointer(id.Leaf), populated: true, want: cgroupPopulated},
		{name: "missing beneath matching root", delegate: id.Delegate, root: id.Root, want: cgroupAbsent},
		{name: "delegated root reused", delegate: changedObject(id.Delegate), root: id.Root, leaf: objectPointer(id.Leaf), want: cgroupReplaced},
		{name: "attempt root reused", delegate: id.Delegate, root: changedObject(id.Root), leaf: objectPointer(id.Leaf), want: cgroupReplaced},
		{name: "leaf reused", delegate: id.Delegate, root: id.Root, leaf: objectPointer(changedObject(id.Leaf)), want: cgroupReplaced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCgroup(id, tc.delegate, tc.root, tc.leaf, tc.populated); got != tc.want {
				t.Fatalf("classifyCgroup = %v, want %v", got, tc.want)
			}
		})
	}
}

func objectPointer(id ObjectID) *ObjectID { return &id }

func changedObject(id ObjectID) ObjectID {
	id.Inode++
	return id
}
