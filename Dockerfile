# Build
# Pinned to go.mod's exact version. golang:1.26 could be any patch release, and
# a builder older than go.mod's target makes Go download a toolchain mid-build —
# slower, and a network dependency the build does not otherwise have.
#
# --platform=$BUILDPLATFORM pins the builder to the machine doing the building,
# not to the image being built. Without it, buildx runs the whole arm64 leg —
# including the Go compile — under QEMU emulation, which took twenty minutes of
# every push for a binary that cross-compiles in under one. CGO is off, so there
# is nothing to emulate for: Go targets arm64 from an amd64 host natively, and
# GOARCH below is what makes it.
FROM --platform=$BUILDPLATFORM golang:1.26.1 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. client-go is most of it and it is not small.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# TARGETOS and TARGETARCH are supplied by buildx per platform being built.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/binnacle ./cmd/binnacle

# Run
#
# Static binary, no shell, no package manager, nonroot. Binnacle reads two
# clusters with credentials of its own; the blast radius of a compromise is
# every cluster in the fleet, so there is no reason to ship anything it does
# not execute.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/binnacle /binnacle
EXPOSE 8080
# Numeric, not the "nonroot" name distroless ships. A kubelet enforcing
# runAsNonRoot cannot verify a name — it has no way to resolve it to a UID —
# and refuses to start the container with "image has non-numeric user".
# 65532 is what distroless means by nonroot.
USER 65532:65532
ENTRYPOINT ["/binnacle"]
