package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The checker runs agent-authored shell commands. Hold its isolation at an
// independent boundary: a fake Docker client records the exact container it
// would start while the parent deliberately carries credentials and a home.
func TestBenchmarkCheckerContainsUntrustedCode(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	script := filepath.Join(root, "scripts", "benchmark", "check.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable", script)
	}

	fakeBin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "docker.args")
	fakeDocker := filepath.Join(fakeBin, "docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		image   = "ben-bench-check:test"
		command = "make check"
		secret  = "must-not-reach-agent-code"
		home    = "/operator/private-home"
	)
	cmd := exec.Command(script, image, checkout, command)
	cmd.Env = []string{
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		"BENCH_DOCKER_ARGS=" + argsFile,
		"GH_TOKEN=" + secret,
		"GITHUB_TOKEN=" + secret,
		"HOME=" + home,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check.sh: %v\n%s", err, out)
	}
	body, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	joined := strings.Join(args, " ")
	for _, absent := range []string{secret, home, "GH_TOKEN", "GITHUB_TOKEN", "/var/run/docker.sock"} {
		if strings.Contains(joined, absent) {
			t.Errorf("Docker argv contains %q:\n%s", absent, joined)
		}
	}
	for _, required := range []string{
		"run", "--network=none", "--ipc=none", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=1024",
		"--memory=6g", "--cpus=4",
		fmt.Sprintf("--user=%d:%d", os.Getuid(), os.Getgid()),
		"--entrypoint=/usr/bin/env", "-i", "HOME=/home/check",
		"PATH=/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin", "GOTOOLCHAIN=local",
		"GIT_CONFIG_VALUE_0=/work", "/bin/bash", "--noprofile", "--norc", "-c",
		`if /bin/bash --noprofile --norc -c "$1"; then exit 0; fi; exit 1`,
		"benchmark-check", command,
	} {
		if !slices.Contains(args, required) {
			t.Errorf("Docker argv lacks %q:\n%s", required, joined)
		}
	}
	mount := "type=bind,source=" + checkout + ",target=/work,readonly"
	if !slices.Contains(args, mount) {
		t.Errorf("Docker argv lacks the one read-only checkout mount %q:\n%s", mount, joined)
	}
	if !slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--cidfile=") }) {
		t.Errorf("Docker argv lacks the container ID file needed for runtime inspection:\n%s", joined)
	}
	if slices.Contains(args, "--rm") {
		t.Errorf("Docker argv removes the container before its runtime state can be inspected:\n%s", joined)
	}
	for _, executableTmpfs := range []string{"/tmp:rw,exec", "/cache:rw,exec"} {
		if !strings.Contains(joined, executableTmpfs) {
			t.Errorf("Docker argv lacks executable ephemeral storage %q:\n%s", executableTmpfs, joined)
		}
	}
	imageAt := slices.Index(args, image)
	if imageAt < 0 || imageAt+1 >= len(args) || args[imageAt+1] != "-i" {
		t.Errorf("image is not immediately followed by env -i: %s", joined)
	}
}

// A failed acceptance check is benchmark evidence; a failure to execute the
// checker is not. check.sh exposes that distinction as a small status protocol
// so the recording loop can abort on Docker/setup/resource failures.
func TestBenchmarkCheckerSeparatesVerdictsFromInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name             string
		env              []string
		relativeCheckout bool
		want             int
	}{
		{name: "passing command", want: 0},
		{name: "completed failing command", env: []string{"BENCH_DOCKER_RUN_STATUS=1"}, want: 1},
		{name: "checker setup refusal", relativeCheckout: true, want: 2},
		{name: "Docker cannot create the container", env: []string{
			"BENCH_DOCKER_RUN_STATUS=125", "BENCH_DOCKER_CREATE=no",
		}, want: 2},
		{name: "container is OOM killed", env: []string{
			"BENCH_DOCKER_RUN_STATUS=1", "BENCH_DOCKER_OOM=true",
		}, want: 2},
		{name: "runtime records an error", env: []string{
			"BENCH_DOCKER_RUN_STATUS=1", "BENCH_DOCKER_STATE_ERROR=cgroup setup failed",
		}, want: 2},
		{name: "container cleanup fails", env: []string{"BENCH_DOCKER_RM_STATUS=1"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Clean(filepath.Join("..", ".."))
			script := filepath.Join(root, "scripts", "benchmark", "check.sh")
			fakeBin := t.TempDir()
			if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(fakeDockerScript), 0o755); err != nil {
				t.Fatal(err)
			}
			checkout := "relative-checkout"
			if !tc.relativeCheckout {
				checkout = t.TempDir()
				if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(script, "ben-bench-check:test", checkout, "make check")
			cmd.Env = append([]string{
				"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
				"BENCH_DOCKER_ARGS=" + filepath.Join(t.TempDir(), "docker.args"),
			}, tc.env...)
			out, err := cmd.CombinedOutput()
			got := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("check.sh: %v\n%s", err, out)
				}
				got = exit.ExitCode()
			}
			if got != tc.want {
				t.Fatalf("check.sh exit = %d, want %d\n%s", got, tc.want, out)
			}
		})
	}
}

func TestBenchmarkProcedureRecordsOnlyCompletedCheckVerdicts(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "BENCH.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`"$CHECK_IMAGE" "$CHECKOUT" "$run" || check_status=$?`,
		`1) passed=false; echo "FAIL $id" ;;`,
		`exit "$check_status" ;;`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("%s lacks checker-status boundary %q", path, required)
		}
	}
}

const fakeDockerScript = `#!/bin/sh
set -eu

case "$1" in
  run)
    printf '%s\n' "$@" >"$BENCH_DOCKER_ARGS"
    cidfile=
    for arg in "$@"; do
      case "$arg" in
        --cidfile=*) cidfile=${arg#--cidfile=} ;;
      esac
    done
    if [ -e "$cidfile" ]; then
      exit 125
    fi
    if [ "${BENCH_DOCKER_CREATE:-yes}" = yes ]; then
      printf '%s\n' fake-container >"$cidfile"
    fi
    exit "${BENCH_DOCKER_RUN_STATUS:-0}"
    ;;
  inspect)
    case "$*" in
      *OOMKilled*) printf '%s\n' "${BENCH_DOCKER_OOM:-false}" ;;
      *ExitCode*)
        status=${BENCH_DOCKER_CONTAINER_STATUS:-${BENCH_DOCKER_RUN_STATUS:-0}}
        printf '%s\n' "$status"
        ;;
      *State.Error*) printf '%s\n' "${BENCH_DOCKER_STATE_ERROR:-}" ;;
      *) exit 99 ;;
    esac
    exit "${BENCH_DOCKER_INSPECT_STATUS:-0}"
    ;;
  rm)
    exit "${BENCH_DOCKER_RM_STATUS:-0}"
    ;;
  *) exit 99 ;;
esac
`

func TestBenchmarkGoShimInterceptsOnlyPinnedStaticcheck(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "benchmark", "check-go.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`[ "$1" = "run" ]`,
		`[ "$2" = "honnef.co/go/tools/cmd/staticcheck@2026.1" ]`,
		`exec /usr/local/bin/staticcheck "$@"`,
		`exec /usr/local/go/bin/go "$@"`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("%s lacks %q", path, required)
		}
	}
}

// The runtime has no network, so its complete module/tool graph must be baked
// by a pinned trusted build. This source audit is deliberately not driven by the
// Dockerfile's own declarations: deleting one of these requirements stays red.
func TestBenchmarkCheckerImageIsPinnedAndPrewarmed(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "benchmark", "check.Dockerfile")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"golang:1.26.1-bookworm@sha256:",
		"GOTOOLCHAIN=local go mod download",
		"go install honnef.co/go/tools/cmd/staticcheck@2026.1",
		"COPY scripts/benchmark/check-go.sh /usr/local/bin/go",
		"USER 10001:10001",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("%s lacks %q", path, required)
		}
	}
	for _, forbidden := range []string{"COPY .", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("%s contains forbidden %q", path, forbidden)
		}
	}
}
