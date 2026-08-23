# Canonical entry points for humans, agents, and CI. `make check` is the
# definition of green — CI runs exactly that target (see AGENTS.md).

GO ?= go
# Pinned so agents, laptops, and CI agree; bump deliberately.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2026.1

.PHONY: build dist test race cover fmt fmt-check vet lint workflow-check worktree-check check smoke

build:
	$(GO) build ./...

# The single static binary of SPEC §11 — the artifact that gets copied to a host
# and named in deploy/ben.service.
#
# **The target is named, not inherited.** `dist` produces the thing that goes
# under a supervisor, and the supervisor in that unit is systemd; on a Mac this
# was producing a dynamically linked Mach-O under comments describing a static
# ELF. Cross-compile by overriding:
#
#	make dist DIST_GOARCH=arm64
#	make dist DIST_GOOS=darwin   # a local binary; `make build` is usually what you want
#
# DIST_-prefixed rather than GOOS/GOARCH, so setting one on the command line
# cannot silently retarget `make check` in the same invocation. The output
# carries its platform, so two builds cannot overwrite each other.
DIST_GOOS ?= linux
DIST_GOARCH ?= amd64

# CGO_ENABLED=0 is what makes it static, and on Linux that changes behaviour and
# not only linkage: a cgo build resolves users and hostnames through the host's
# libc NSS, so a binary built on one distribution can fail to look anything up
# on another. BEN reads the hostname at startup — it is half of the daemon
# identity on every §9.11 entry — so that failure would land in the state files
# rather than in an error anyone reads.
#
# -trimpath keeps build-machine paths out of the binary: two builds of one commit
# then match, and a panic in the journal names package paths rather than
# somebody's home directory.
dist:
	CGO_ENABLED=0 GOOS=$(DIST_GOOS) GOARCH=$(DIST_GOARCH) $(GO) build -trimpath -o bin/ben-$(DIST_GOOS)-$(DIST_GOARCH) ./cmd/ben
	@echo "built bin/ben-$(DIST_GOOS)-$(DIST_GOARCH)"

test:
	$(GO) test ./...

# -cover rides on the canonical run rather than living in a target of its own.
# Coverage instrumentation changes what the tests observe — the instrumented
# child writes a GOCOVERDIR warning to stderr, which a transcript then records —
# so a suite only ever run without it can be green and still unusable for
# finding untested paths. Here that can never regress unnoticed, and CI pays for
# one instrumented build instead of a second full run.
race:
	$(GO) test -race -cover ./...

# A cross-package profile, for reading which paths the tests actually reach.
# `make check` is already instrumented, but its per-package percentages
# under-report any package driven from elsewhere: internal/agent/harness reads
# 26% against its own tests and ~94% once the adapters' conformance suites are
# counted, and only the second number answers whether the lifecycle is tested.
cover:
	$(GO) test -coverpkg=./... -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1
	@echo "per-function: $(GO) tool cover -func=coverage.out"
	@echo "annotated:    $(GO) tool cover -html=coverage.out"

fmt:
	gofmt -w cmd internal

fmt-check:
	@unformatted=$$(gofmt -l cmd internal); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	$(GO) vet ./...

lint:
	$(GO) run $(STATICCHECK) ./...

# Load-validate BEN's dogfood, real-integration smoke, and benchmark profiles
# with BEN's own loader (AGENTS.md, "Dogfooding"; docs/SMOKE.md; docs/BENCH.md).
# The supplied values are inert validation fixtures: config effective calls
# Structural only, with no credentials, network, or harness. Output discarded:
# green/red is the signal.
workflow-check:
	@$(GO) run ./cmd/ben config effective WORKFLOW.md >/dev/null
	@BEN_SMOKE_REPO=acme/canary BEN_SMOKE_WORKSPACE=/tmp/ben-smoke-workspaces \
		$(GO) run ./cmd/ben config effective scripts/smoke-workflow.md >/dev/null
	@BEN_BENCH_REPO=acme/canary BEN_BENCH_WORKSPACE=/tmp/ben-bench-workspaces BEN_BENCH_MODEL=test-model \
		$(GO) run ./cmd/ben config effective scripts/benchmark/claude-code-default.md >/dev/null
	@BEN_BENCH_REPO=acme/canary BEN_BENCH_WORKSPACE=/tmp/ben-bench-workspaces BEN_BENCH_MODEL=test-model \
		$(GO) run ./cmd/ben config effective scripts/benchmark/claude-code-model.md >/dev/null
	@BEN_BENCH_REPO=acme/canary BEN_BENCH_WORKSPACE=/tmp/ben-bench-workspaces BEN_BENCH_MODEL=test-model \
		$(GO) run ./cmd/ben config effective scripts/benchmark/codex-exec-default.md >/dev/null
	@BEN_BENCH_REPO=acme/canary BEN_BENCH_WORKSPACE=/tmp/ben-bench-workspaces BEN_BENCH_MODEL=test-model \
		$(GO) run ./cmd/ben config effective scripts/benchmark/codex-exec-model.md >/dev/null
	@echo "workflow configs valid"

# A dev-machine detector (AGENTS.md, "One branch, one worktree"). Git already
# refuses to check a branch out twice, so a duplicate here means the refusal was
# overridden — and the cost lands in the *other* worktree, whose index and tree
# git never updated. It rides `check` rather than living in a target nobody runs,
# because the failure it detects leaves a coherent older tree that tests green:
# the one signal that would reassure you is the one that cannot see it.
# A no-op in CI, which has a single worktree.
# Enumeration is checked before it is parsed, and a failure to look is a failure:
# piping git straight into sed would hide a broken or absent repository behind an
# empty result, and report the all-clear for the one input it never read.
worktree-check:
	@list=$$(git worktree list --porcelain 2>&1) || { \
		echo "worktree-check: cannot enumerate worktrees:"; \
		printf '%s\n' "$$list" | sed 's/^/  /'; \
		exit 1; \
	}; \
	dupes=$$(printf '%s\n' "$$list" | sed -n 's/^branch //p' | sort | uniq -d); \
	if [ -n "$$dupes" ]; then \
		echo "the same branch is checked out in more than one worktree:"; \
		printf '%s\n' "$$dupes" | sed 's/^/  /'; \
		git worktree list; \
		echo "see AGENTS.md, \"Working in worktrees\" — the other worktree's tree may have skipped a commit"; \
		exit 1; \
	fi

check: fmt-check vet lint race workflow-check worktree-check
	@echo "check: all green"

# SPEC §12.4's real-integration profile: one scripted issue end to end on a
# canary repository, with a real agent harness and a real GitHub.
#
# Deliberately not part of `check`, and not a CI job. It needs two credentials,
# spends agent tokens and writes to a repository, none of which belongs in a
# target that runs on every push — while `check` must stay something anyone can
# run offline. What it catches is the class `check` structurally cannot: harness
# and API drift where CI must model or replay the outside world.
#
#	BEN_SMOKE_REPO=<owner>/<canary> make smoke
#
# See docs/SMOKE.md.
smoke:
	@./scripts/smoke.sh
