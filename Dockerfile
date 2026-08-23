# syntax=docker/dockerfile:1

# Multi-architecture indexes, pinned so a rebuild cannot silently take a newer
# toolchain or runtime. Go stays in the final image because BEN's dogfood hook
# and the agent both run the repository's canonical `make check`.
ARG GO_IMAGE=golang:1.26.1-bookworm@sha256:ab3d6955bbc813a0f3fdf220c1d817dd89c0b3f283777db8ece4a32fe7858edd
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

FROM ${GO_IMAGE} AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ben ./cmd/ben

FROM ${NODE_IMAGE}

ARG CLAUDE_CODE_VERSION=2.1.221

# build-essential is runtime tooling here, not a build-stage convenience: Go's
# race-enabled test command invokes a C compiler, and the process-discipline
# conformance suite uses procps' ps to distinguish a live process from a zombie.
# The remaining tools are the reviewed BEN workflow's own publish and inspection
# surface.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      git \
      gh \
      jq \
      openssh-client \
      procps \
      ripgrep \
      tini && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/go/ /usr/local/go/
COPY --from=builder /out/ben /usr/local/bin/ben

ENV PATH="/usr/local/go/bin:${PATH}"

# This is the version BEN's adapter fixtures were measured against. The
# package has no unpinned regular dependencies; its platform package is pinned
# to the same version by the published optional-dependency table.
RUN npm install --global --omit=dev "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && \
    claude --version | grep -F "Claude Code" && \
    npm cache clean --force

RUN groupadd --gid 10001 ben && \
    useradd --uid 10001 --gid 10001 --create-home --home-dir /home/ben ben && \
    install -d -o ben -g ben \
      /etc/ben \
      /var/lib/ben/cache/go-build \
      /var/lib/ben/cache/go-mod \
      /var/lib/ben/data \
      /var/lib/ben/home \
      /var/lib/ben/state \
      /var/lib/ben/workspaces

# Keep commit metadata after the expensive runtime layers: changing VCS_REF
# must not invalidate apt and npm caches on every source commit.
ARG VCS_REF=unknown
LABEL org.opencontainers.image.source="https://github.com/srhg-ai-7cef3f93/ben" \
      org.opencontainers.image.revision="${VCS_REF}"

ENV HOME=/var/lib/ben/home \
    XDG_DATA_HOME=/var/lib/ben/data \
    XDG_STATE_HOME=/var/lib/ben/state \
    GOCACHE=/var/lib/ben/cache/go-build \
    GOMODCACHE=/var/lib/ben/cache/go-mod

USER 10001:10001
WORKDIR /var/lib/ben

# The agent may orphan grandchildren that only PID 1 can adopt and reap. tini
# owns that init duty and forwards Kubernetes' SIGTERM directly to BEN, whose
# ordered §9.8 drain remains authoritative. A shell entrypoint provides neither
# guarantee.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/ben"]
CMD ["run", "/etc/ben/WORKFLOW.md"]
