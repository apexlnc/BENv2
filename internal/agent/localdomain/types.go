package localdomain

import (
	"errors"
	"io"
	"os"
	"time"
)

const (
	// EvidenceScheme is the only durable local-domain evidence version.
	EvidenceScheme  = "linux-domain-v1"
	attemptPrefix   = "run-"
	supervisorArg   = "__ben_localdomain_supervisor_v1"
	providerArg     = "__ben_localdomain_provider_v1"
	canaryArg       = "__ben_localdomain_canary_v1"
	nestedCgroupArg = "__ben_localdomain_nested_cgroup_v1"
	nestedProcArg   = "__ben_localdomain_nested_proc_v1"
	canaryKeeperArg = "__ben_localdomain_canary_keeper_v1"
)

var (
	// ErrUnavailable is returned when the host cannot prove every primitive
	// required by the local execution-domain contract.
	ErrUnavailable = errors.New("local execution domain unavailable")
	// ErrCleanupDegraded refuses new attempts after bounded cleanup retries
	// demonstrate that this manager would otherwise leak cgroups indefinitely.
	ErrCleanupDegraded = errors.New("local execution-domain cleanup degraded")
	// ErrInvalidEvidence identifies noncanonical or incomplete durable evidence.
	ErrInvalidEvidence = errors.New("invalid local execution-domain evidence")
	// ErrContainment identifies the impossible-looking state where the recorded
	// PID-namespace init exited while its matching attempt cgroup is populated.
	ErrContainment = errors.New("local execution-domain containment invariant violated")
)

// ObjectID is a kernel object's stable statx identity for one boot.
type ObjectID struct {
	DevMajor uint32
	DevMinor uint32
	Inode    uint64
}

// Identity is the decoded linux-domain-v1 payload. No caller should derive a
// verdict from individual fields; they are exposed so marker stores can retain
// and report the opaque evidence without importing Linux implementation types.
type Identity struct {
	Boot       string
	Delegate   ObjectID
	Root       ObjectID
	Name       string
	Leaf       ObjectID
	PID        uint32
	StartTicks uint64
	PIDNS      ObjectID
	CgroupNS   ObjectID
}

// Evidence is the substrate-owned value copied opaquely into core.RunEvidence.
// This package alone parses ID.
type Evidence struct {
	Scheme string
	Boot   string
	ID     string
}

// Termination answers only whether the execution domain is positively quiet.
type Termination uint8

const (
	TerminationUnconfirmed Termination = iota
	TerminationConfirmed
)

// StopMode selects the cooperative grace without changing bounded teardown.
type StopMode uint8

const (
	StopInterrupt StopMode = iota
	StopDiscard
)

// Timings bounds local-domain teardown and janitor retry work.
type Timings struct {
	InterruptGrace  time.Duration
	DiscardGrace    time.Duration
	KillGrace       time.Duration
	PollInterval    time.Duration
	CleanupRetry    time.Duration
	CleanupPass     time.Duration
	CleanupNodes    int
	CleanupFailures int
}

func (t Timings) withDefaults() Timings {
	if t.InterruptGrace <= 0 {
		t.InterruptGrace = 2 * time.Second
	}
	if t.DiscardGrace <= 0 {
		t.DiscardGrace = 200 * time.Millisecond
	}
	if t.KillGrace <= 0 {
		t.KillGrace = 2 * time.Second
	}
	if t.PollInterval <= 0 {
		t.PollInterval = 25 * time.Millisecond
	}
	if t.CleanupRetry <= 0 {
		t.CleanupRetry = 250 * time.Millisecond
	}
	if t.CleanupPass <= 0 {
		t.CleanupPass = 250 * time.Millisecond
	}
	if t.CleanupNodes <= 0 {
		t.CleanupNodes = 1024
	}
	if t.CleanupFailures <= 0 {
		t.CleanupFailures = 8
	}
	return t
}

// Options configures one process-lifetime manager. DelegatedRoot is an
// explicit real-test seam; production leaves it empty and discovery starts at
// the daemon's unified-v2 membership.
type Options struct {
	Executable    string
	DelegatedRoot string
	Timings       Timings
	Random        io.Reader
}

// Launch describes the untrusted provider process. The three stream files are
// passed explicitly; no other daemon descriptor crosses the supervisor exec.
type Launch struct {
	Argv   []string
	Env    []string
	Dir    string
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File

	// OnDomain durably upgrades the existing run marker while the supervisor is
	// trusted and the provider is still gated. A failure aborts before untrusted
	// exec and tears down the empty domain.
	OnDomain func(Evidence) error
}

// ProviderExit describes the direct provider child independently of the
// supervisor's later execution-domain exit.
type ProviderExit struct {
	Code       int
	Signal     int
	StartError string
}

// Success reports the advisory direct-process exit status.
func (s ProviderExit) Success() bool {
	return s.StartError == "" && s.Signal == 0 && s.Code == 0
}
