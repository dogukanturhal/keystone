# syntax=docker/dockerfile:1.7
#
# Keystone Manager
# SPDX-License-Identifier: AGPL-3.0-or-later

# Go toolchain. Bumped 1.26.3 -> 1.26.5 on 2026-08-01: `vuln-scan-image`
# failed on two HIGH stdlib advisories compiled into the binary --
#   CVE-2026-27145  fixed in 1.25.11 / 1.26.4
#   CVE-2026-39822  fixed in 1.25.12 / 1.26.5 / 1.27.0-rc.2
# 1.26.5 is the lowest 1.26.x that clears BOTH (1.26.4 clears only the
# first) and is the highest stable 1.26 patch.
#
# Bumped 1.26.5 -> 1.26.8 on 2026-09-21: `vuln-scan-image` (pipeline
# 16870) failed on 8 HIGH stdlib advisories compiled into the binary --
# CVE-2026-33818, 39821, 46600, 56853, 56858, 56859, 56860, 56862,
# all fixed in 1.25.13 / 1.26.6 / 1.27.0-rc.3. 1.26.8 is the current
# highest stable 1.26 patch (go.dev/dl as of 2026-09-21).
#
# Keep this current: the stdlib is linked into the binary, so a stale
# toolchain fails the image scan even when every go.mod dependency is
# clean -- and because sign-image runs after scan, a stale toolchain
# means NO signed image can be produced at all, which in turn means the
# operator can never be upgraded past the Kyverno verifyImages gate.
# Bumped 1.26.8 -> 1.26.9 on 2026-10-11: CVE-2026-78667,
# CVE-2026-78669 and CVE-2026-97031 are HIGH and fixed in 1.26.9.
ARG GO_VERSION=1.26.10

FROM mirror.gcr.io/library/golang:${GO_VERSION}-alpine AS build
WORKDIR /src

RUN apk add --no-cache git

# Optional checksum-verified module proxy for offline/local builds. Defaults
# preserve CI's direct authentication path for private modules. No credentials
# or module cache are copied into either image stage.
ARG GOPROXY=https://proxy.golang.org,direct
ARG GONOPROXY=
ENV GOPROXY=${GOPROXY} GONOPROXY=${GONOPROXY} GOMAXPROCS=2

# Copy go.mod/go.sum AND the api submodule first for layer caching.
# go.mod has `replace github.com/dogukanturhal/keystone/api => ./api`,
# so `go mod download` needs ./api/go.mod to validate the replace target.
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum ./api/
RUN go mod download

# Copy source.
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -p 2 \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.date=${BUILD_DATE}" \
    -o /out/keystone-manager ./cmd/manager

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="keystone-manager" \
      org.opencontainers.image.source="https://github.com/dogukanturhal/keystone" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
USER 65532:65532
COPY --from=build /out/keystone-manager /keystone-manager
ENTRYPOINT ["/keystone-manager"]
