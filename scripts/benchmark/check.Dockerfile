# syntax=docker/dockerfile:1

# This image is built from the trusted controller checkout, never from the
# agent-authored published head. The digest matches BEN's build image so the
# benchmark and repository gate use the repository's recorded Go toolchain.
ARG GO_IMAGE=golang:1.26.1-bookworm@sha256:ab3d6955bbc813a0f3fdf220c1d817dd89c0b3f283777db8ece4a32fe7858edd
FROM ${GO_IMAGE}

# Every v1 case pins the same module graph and staticcheck version. Preload both
# while the image build has network access; the untrusted runtime gets none.
WORKDIR /seed
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=local go mod download && \
    GOBIN=/usr/local/bin GOTOOLCHAIN=local go install honnef.co/go/tools/cmd/staticcheck@2026.1 && \
    command -v bash git make gcc >/dev/null && \
    rm -rf /seed && \
    install -d -o 10001 -g 10001 /home/check /cache/go-build /cache/xdg
COPY scripts/benchmark/check-go.sh /usr/local/bin/go

USER 10001:10001
WORKDIR /work
