// Package fakesrt is the measured sandbox-runtime fake used by the claude-code
// adapter tests. It lives under testdata so the helper binary does not add a
// package to `go test ./...`'s production surface.
package fakesrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	SettingsFlag = "-s"
	EnforceEnv   = "FAKE_SRT_ENFORCE"
	tmpDirEnv    = "CLAUDE_CODE_TMPDIR"
)

type settings struct {
	Filesystem struct {
		AllowRead  []string `json:"allowRead"`
		DenyRead   []string `json:"denyRead"`
		AllowWrite []string `json:"allowWrite"`
		DenyWrite  []string `json:"denyWrite"`
	} `json:"filesystem"`
	Network struct {
		AllowedDomains []string `json:"allowedDomains"`
	} `json:"network"`
}

// IsInvocation reports the wrapper shape the adapter builds:
// `-s <settings> -- <harness argv...>`.
func IsInvocation(args []string) bool {
	return len(args) >= 4 && args[0] == SettingsFlag && args[2] == "--"
}

// Run behaves like the subset of sandbox-runtime the adapter observes. It
// never returns: the wrapper either refuses or exits with the inner command.
func Run(args []string) {
	settingsPath, inner := args[1], args[3:]
	enforced := strings.Split(os.Getenv(EnforceEnv), ",")
	if path, class, ok := denied(settingsPath, inner); ok && slices.Contains(enforced, class) {
		fmt.Fprintf(os.Stderr, "%s: Operation not permitted\n", path)
		os.Exit(1)
	}
	if err := load(settingsPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not load settings from %s (missing, unreadable, or invalid): %v\n",
			settingsPath, err)
		os.Exit(1)
	}

	cmd := exec.Command(inner[0], inner[1:]...)
	cmd.Env = append(os.Environ(), "SANDBOX_RUNTIME=1", "TMPDIR="+tmpDir())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to execute command: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func tmpDir() string {
	if dir := os.Getenv(tmpDirEnv); dir != "" {
		return dir
	}
	return "/tmp/claude"
}

func load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var got settings
	if err := json.Unmarshal(b, &got); err != nil {
		return err
	}
	if len(got.Network.AllowedDomains) == 0 {
		return fmt.Errorf("no allowed domains")
	}
	return nil
}

func denied(settingsPath string, argv []string) (path, class string, ok bool) {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return "", "", false
	}
	var got settings
	if err := json.Unmarshal(b, &got); err != nil {
		return "", "", false
	}
	for _, arg := range argv {
		for _, field := range strings.Fields(arg) {
			field = strings.Trim(field, "'\"")
			if !strings.HasPrefix(field, "/") {
				continue
			}
			allowed := longestPrefix(slices.Concat(got.Filesystem.AllowWrite, got.Filesystem.AllowRead), field)
			for _, list := range []struct {
				class   string
				entries []string
			}{{"write", got.Filesystem.DenyWrite}, {"read", got.Filesystem.DenyRead}} {
				if longestPrefix(list.entries, field) > allowed {
					return field, list.class, true
				}
			}
		}
	}
	return "", "", false
}

func longestPrefix(entries []string, target string) int {
	best := -1
	for _, entry := range entries {
		if target == entry || strings.HasPrefix(target, entry+string(filepath.Separator)) {
			best = max(best, len(entry))
		}
	}
	return best
}

// Inner returns the wrapped command from a recorded invocation.
func Inner(argv []string) []string {
	i := slices.Index(argv, "--")
	if i < 0 {
		return nil
	}
	return argv[i+1:]
}
