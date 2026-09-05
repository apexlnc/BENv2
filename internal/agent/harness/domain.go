package harness

import (
	"context"
	"errors"
	"os"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/localdomain"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ErrExecutionDomain identifies a launch refusal from the substrate boundary.
// Adapters wrap it in their own named readiness/start refusal.
var ErrExecutionDomain = errors.New("local execution domain refused launch")

// LocalEvidenceScheme is exported for the assembly's substrate router. Core
// deliberately treats every evidence scheme as opaque.
const LocalEvidenceScheme = localdomain.EvidenceScheme

// ExecutionDomain is the process-substrate half of one local adapter. The
// harness owns streams and lifecycle; the domain owns launch, quiet evidence,
// and teardown. Production uses one LocalDomain for the process lifetime.
type ExecutionDomain interface {
	Ready(context.Context) error
	Start(context.Context, DomainLaunch) (DomainRun, error)
}

// DomainLaunch is the descriptor-only launch boundary. The provider receives
// exactly these streams; marker evidence must be durable before it is released.
type DomainLaunch struct {
	Argv   []string
	Env    []string
	Dir    string
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File

	OnDomain func(core.RunEvidence) error
	Timings  Timings
}

// DomainRun separates direct-provider completion from execution-domain quiet.
// Done is only the phase edge used by the stream lifecycle; it grants no reuse.
type DomainRun interface {
	DirectDone() <-chan struct{}
	Probe(context.Context) core.Termination
	Stop(context.Context, core.StopMode) core.Termination
}

type linuxDomain struct {
	manager *localdomain.Manager
}

var processLocalDomain = &linuxDomain{manager: localdomain.New(localdomain.Options{})}

// LocalDomain returns the one production local-domain provider for this
// process. Runner reloads and both adapter kinds therefore share its readiness,
// retained descriptors, startup sweep, and janitor health.
func LocalDomain() ExecutionDomain { return processLocalDomain }

// InternalMain recognizes the same-binary trusted supervisor modes before the
// public CLI parses arguments.
func InternalMain(args []string) (bool, int) { return localdomain.InternalMain(args) }

func (d *linuxDomain) Ready(ctx context.Context) error { return d.manager.Ready(ctx) }

func (d *linuxDomain) Start(ctx context.Context, launch DomainLaunch) (DomainRun, error) {
	run, err := d.manager.Start(ctx, localdomain.Launch{
		Argv: launch.Argv, Env: launch.Env, Dir: launch.Dir,
		Stdin: launch.Stdin, Stdout: launch.Stdout, Stderr: launch.Stderr,
		OnDomain: func(e localdomain.Evidence) error {
			if launch.OnDomain == nil {
				return nil
			}
			return launch.OnDomain(core.RunEvidence{Scheme: e.Scheme, Boot: e.Boot, ID: e.ID})
		},
	})
	if err != nil {
		return nil, err
	}
	directDone := make(chan struct{})
	go func() {
		<-run.ProviderDone
		close(directDone)
	}()
	return &linuxRun{run: run, directDone: directDone}, nil
}

type linuxRun struct {
	run        *localdomain.Run
	directDone <-chan struct{}
}

func (r *linuxRun) DirectDone() <-chan struct{} { return r.directDone }

func (r *linuxRun) Probe(ctx context.Context) core.Termination {
	status, _ := r.run.Handle.Probe(ctx)
	return coreTermination(status)
}

func (r *linuxRun) Stop(ctx context.Context, mode core.StopMode) core.Termination {
	localMode := localdomain.StopInterrupt
	if mode == core.StopDiscard {
		localMode = localdomain.StopDiscard
	}
	status, _ := r.run.Handle.Stop(ctx, localMode)
	return coreTermination(status)
}

func coreTermination(status localdomain.Termination) core.Termination {
	if status == localdomain.TerminationConfirmed {
		return core.TerminationConfirmed
	}
	return core.TerminationUnconfirmed
}
