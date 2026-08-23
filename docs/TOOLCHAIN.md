# The Go toolchain floor (AGENTS.md, "Toolchain")

The rule lives in [AGENTS.md](../AGENTS.md#toolchain): `go.mod` declares a *minor-level* floor,
raising it to a new minor is ordinary, and naming a patch — or adding a `toolchain` preference —
needs an argument written where the next blocked contributor will look. This document is the
measurement behind it, and the alternative that was considered and rejected (#110).

## How the two directives resolve

`go 1.26` is a floor every 1.26.x toolchain satisfies, so the repository builds and checks
offline and under `GOTOOLCHAIN=local`. CI resolves its version from that same line
(`go-version-file: go.mod`); the pinned `actions/setup-go@v5` reads only the `go` directive, so
it installs the latest 1.26.x rather than a fixed patch. That float is what the floor costs, and
it is the cheap side of the trade.

Measured against an installed go1.26.1:

| `go.mod` says | `GOTOOLCHAIN=local` | `GOTOOLCHAIN=auto` (the default) |
|---|---|---|
| `go 1.26` | builds | builds, on the installed 1.26.1 |
| `go 1.26.2` | `go: go.mod requires go >= 1.26.2 (running go 1.26.1; GOTOOLCHAIN=local)` | downloads a whole toolchain and silently switches |
| `go 1.26` + `toolchain go1.26.9` | builds — the toolchain line is ignored | tries to download 1.26.9 |
| `go 1.26` + `toolchain go1.26.0` | builds | builds on 1.26.1 — never downgrades |

## Why a patch level needs an argument

A patch-level directive is a toolchain download for a contributor on `auto` and a hard refusal on
`local` — sandboxed, offline, or locked-down machines. CI is structurally incapable of reporting
either, because the one machine that always satisfies the pin is the one that installs exactly
what the pin names. `go mod init` writes the running toolchain's patch level, so the artifact
arrives silently and nothing about the resulting file looks wrong; that is why the policy is a
test (`internal/arch/gomod_test.go`) rather than something left to review, and why it survived
unnoticed from B01 to #110.

The test's negative control matters as much as the rule: driven by the real `go.mod` alone, it
would pass just as happily if the directive scan silently missed its target.

## Why no `toolchain` preference

An explicit `toolchain go1.26.1` was considered and rejected. It would **not** pin this CI:
`setup-go` v5 ignores `toolchain` lines. `GOTOOLCHAIN=local` ignores the preference too, while
`auto` downloads it for a contributor on 1.26.0 and never downgrades a newer installation. With
no known compiler or runtime fix requiring 1.26.1, that would impose the contributor download
without changing either CI or the local floor. The repository therefore carries no `toolchain`
directive, and `internal/arch` enforces its absence.

## Changing either

Raising the floor to a new **minor** is ordinary: edit `go.mod` and the tests stay green. Naming
a patch, or adding a `toolchain` preference, fails `internal/arch` until the test changes with
it — which is the point. Say which compiler or runtime fix requires it, in `go.mod` and in
AGENTS.md, in the same change.

A related trap on the other side of the same variable: a `GOTOOLCHAIN` persisted with `go env -w`
applies to every `go` invocation on the machine, including the ones no shell of yours configures.
See [GO-ENV.md](GO-ENV.md).
