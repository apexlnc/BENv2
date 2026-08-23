# The persisted Go environment (AGENTS.md, "Go on a dev machine")

The rule and the audit that enforces it live in
[AGENTS.md](../AGENTS.md#go-on-a-dev-machine): do not persist a Go setting you also export, and
check what is persisted with `env -i`. This document is why that is a rule at all — which file
`go env -w` writes, who reads it, and the dogfood run it cost.

## Which file, and who reads it

`go env -w` writes to a file under `os.UserConfigDir()` — `~/Library/Application Support/go/env`
on macOS, `~/.config/go/env` on Linux — and that file governs **every** `go` invocation on the
machine. An `export` in `~/.zshrc` overrides it, but only where that dotfile is read: an
interactive shell. Everything else — cron, launchd, systemd, a CI runner, an editor's language
server, and BEN's own hooks — sees the file.

So the two can disagree for years while every shell you look at says the setting is fine. BEN's
first dogfood run died on exactly that: `GO111MODULE=off` written in 2023, masked by an
`export GO111MODULE=on` in `.zshrc`, and visible only to a process that does not read dotfiles.

## Reading the audit

```sh
cat "$(go env GOENV)"                       # what is persisted, if anything
env -i PATH="$PATH" HOME="$HOME" go env GO111MODULE GOFLAGS GOPROXY GOTOOLCHAIN
```

The second is the one that matters: `env -i` is roughly what a daemon gets, so it answers "what
does Go do when nothing of mine is loaded?" — which is the only question a hook, a service or CI
ever asks.

## What belongs in that file, and what does not

**Belongs:** machine-wide policy with no per-shell variation — `GOPRIVATE` for private module
paths, a corporate `GOPROXY`.

**Does not:** `GO111MODULE` (default since 1.16; setting it is legacy and `off` is what bit us),
and anything you would rather state per-project. `GOFLAGS` is the sharpest of these — a persisted
`-mod=vendor` or `-tags` silently changes what every build in every context compiles.

## Why BEN detects this rather than overriding it

`WORKFLOW.md`'s `after_create` hook runs `go mod download`, and that is where the failure above
surfaced. The hook could set `GO111MODULE=on` for itself, and deliberately does not: an override
would fix that one command and move the failure to the agent, which inherits the same `HOME` and
runs `make check` with the same result — later, after tokens are spent, and reported as an agent
failure rather than as the machine's misconfiguration.

Setting it in `agent.provider.env` too would close the gap for one variable and leave the class
open: `GOFLAGS=-mod=vendor`, `GOPROXY=off`, and `GOTOOLCHAIN=local` on an older toolchain all
reach the hook and the agent by the same route. So the hook stays a detector, and the remedy is
the audit — see [TOOLCHAIN.md](TOOLCHAIN.md) for what a persisted `GOTOOLCHAIN` does to this
repository's version floor in particular.
