//go:build !linux

package localdomain

import (
	"context"
	"fmt"
	"runtime"
)

// Manager is a fail-closed placeholder on platforms without the approved
// Linux execution domain. It keeps build/config/status and remote execution
// available without silently restoring process-group authority.
type Manager struct{}

func New(Options) *Manager { return &Manager{} }

func (m *Manager) Ready(context.Context) error {
	return fmt.Errorf("%w: %s has no local execution-domain provider", ErrUnavailable, runtime.GOOS)
}

func (m *Manager) Start(context.Context, Launch) (*Run, error) {
	return nil, fmt.Errorf("%w: %s has no local execution-domain provider", ErrUnavailable, runtime.GOOS)
}

func (m *Manager) Recover(context.Context, Evidence) (Termination, error) {
	return TerminationUnconfirmed, fmt.Errorf("%w: %s cannot observe Linux evidence", ErrUnavailable, runtime.GOOS)
}

func (m *Manager) Close() error { return nil }

// InternalMain recognizes no runnable local-domain mode on an unsupported OS.
func InternalMain(args []string) (bool, int) {
	if len(args) == 2 && (args[1] == supervisorArg || args[1] == providerArg || args[1] == canaryArg ||
		args[1] == nestedCgroupArg || args[1] == nestedProcArg || args[1] == canaryKeeperArg) {
		return true, 125
	}
	return false, 0
}
