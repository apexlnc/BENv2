package claudecode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var fakeSandboxHelper struct {
	sync.Once
	dir  string
	path string
	err  error
}

var (
	// Captured before any test can narrow HOME or PATH. The helper build is
	// shared by tests whose subject is precisely those process-global values,
	// so inheriting whichever test happened to build it first would make its
	// success order-dependent.
	fakeSandboxBuildEnv                       = os.Environ()
	fakeSandboxBuildGo, fakeSandboxBuildGoErr = exec.LookPath("go")
)

func fakeSandboxBinary(t *testing.T) string {
	t.Helper()
	fakeSandboxHelper.Do(func() {
		if fakeSandboxBuildGoErr != nil {
			fakeSandboxHelper.err = fmt.Errorf("locate go binary: %w", fakeSandboxBuildGoErr)
			return
		}
		fakeSandboxHelper.dir, fakeSandboxHelper.err = os.MkdirTemp("", "ben-fake-srt-")
		if fakeSandboxHelper.err != nil {
			return
		}
		fakeSandboxHelper.path = filepath.Join(fakeSandboxHelper.dir, "srt")
		_, source, _, ok := runtime.Caller(0)
		if !ok {
			fakeSandboxHelper.err = fmt.Errorf("locate fake sandbox source")
			return
		}
		cmd := exec.Command(fakeSandboxBuildGo, "build", "-o", fakeSandboxHelper.path, "./testdata/fakesrtcmd")
		cmd.Dir = filepath.Dir(source)
		cmd.Env = fakeSandboxBuildEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeSandboxHelper.err = fmt.Errorf("build fake sandbox runtime: %w\n%s", err, out)
		}
	})
	if fakeSandboxHelper.err != nil {
		t.Fatal(fakeSandboxHelper.err)
	}
	return fakeSandboxHelper.path
}

func cleanupFakeSandboxBinary() {
	if fakeSandboxHelper.dir != "" {
		_ = os.RemoveAll(fakeSandboxHelper.dir)
	}
}
