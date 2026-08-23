package claudecode

import "github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode/testdata/fakesrt"

// The fake sandbox runtime models the observable contract measured from
// @anthropic-ai/sandbox-runtime 0.0.73:
//
//   - argv elements reach the inner command literally;
//   - a missing or malformed settings file is a refusal, never an unbounded run;
//   - CLAUDE_CODE_TMPDIR overrides the runtime's /tmp/claude default;
//   - stdio and the inner exit code are inherited;
//   - read and write denials can be exercised independently.
//
// The implementation lives under testdata/fakesrt so the readiness tests can
// build one small helper and avoid re-executing the race-and-coverage-
// instrumented package test binary for every wrapper probe. The same code still
// backs the package binary's fallback below, so the helper and the fake cannot
// drift, and testdata keeps `go test ./...`'s package set unchanged.
//
// This is deliberately not a general sandbox model. Its path check sees only
// absolute paths named in argv; the real-runtime integration test is what
// establishes that the composed posture holds at the syscall boundary.

const (
	sandboxSettingsFlag   = fakesrt.SettingsFlag
	fakeSandboxEnforceEnv = fakesrt.EnforceEnv
)

func isSandboxInvocation(args []string) bool { return fakesrt.IsInvocation(args) }

func runFakeSandbox(args []string) { fakesrt.Run(args) }

func fakeSandboxInner(argv []string) []string { return fakesrt.Inner(argv) }
