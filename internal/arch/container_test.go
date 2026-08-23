package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runtime's PID 1 is part of BEN's process-lifecycle contract: agent
// grandchildren can outlive their immediate parent, and a process that only
// forwards SIGTERM but does not reap adopted descendants leaves zombies that
// every §7.5 liveness probe must conservatively read as alive (#184).
func TestRuntimeImageUsesAReapingInit(t *testing.T) {
	dockerfile := runtimeDockerfile(t)

	const entrypoint = `ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/ben"]`
	if got := strings.Count(dockerfile, "ENTRYPOINT "); got != 1 {
		t.Fatalf("Dockerfile has %d ENTRYPOINT declarations, want exactly one", got)
	}
	if !strings.Contains(dockerfile, entrypoint) {
		t.Errorf("runtime entrypoint does not exec tini directly in front of BEN; want %s", entrypoint)
	}
	// Anchored independently from ENTRYPOINT: naming a binary there does not put
	// it in the image, and a missing reaper is a startup failure rather than a
	// graceful degradation.
	if !strings.Contains(dockerfile, "\n      tini && \\\n") {
		t.Error("runtime package set does not install tini")
	}
}

// BEN's own workflow runs make check inside the image. The process-discipline
// suite asks ps for a process state because signal 0 cannot distinguish a live
// process from a zombie; a slim image without ps turns that missing executable
// into a false death verdict and strands the test's gated cleanup (#186).
func TestRuntimeImageIncludesProcessInspection(t *testing.T) {
	dockerfile := runtimeDockerfile(t)
	if !strings.Contains(dockerfile, "\n      procps \\\n") {
		t.Error("runtime package set does not install procps")
	}
}

func runtimeDockerfile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
