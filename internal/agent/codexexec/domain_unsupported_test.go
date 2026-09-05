//go:build !linux

package codexexec

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultDomainRefusesUnsupportedPlatform(t *testing.T) {
	parallel(t)
	runner, err := New(Options{Provider: map[string]any{"sandbox_mode": "workspace-write"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Ready(context.Background()); !errors.Is(err, ErrExecutionDomain) {
		t.Fatalf("Ready = %v, want ErrExecutionDomain", err)
	}
}
