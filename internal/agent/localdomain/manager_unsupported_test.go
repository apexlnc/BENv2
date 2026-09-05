//go:build !linux

package localdomain

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestUnsupportedPlatformRefusesEveryLocalEntry(t *testing.T) {
	m := New(Options{})
	defer m.Close()
	if err := m.Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ready error = %v", err)
	}
	if _, err := m.Start(context.Background(), Launch{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start error = %v", err)
	}
	if got, err := m.Recover(context.Background(), Evidence{}); got != TerminationUnconfirmed || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Recover = (%v, %v)", got, err)
	}
	if handled, _ := InternalMain([]string{os.Args[0]}); handled {
		t.Fatal("normal argv was claimed as an internal mode")
	}
	if handled, code := InternalMain([]string{os.Args[0], supervisorArg}); !handled || code == 0 {
		t.Fatalf("supervisor mode = (%v, %d)", handled, code)
	}
}
