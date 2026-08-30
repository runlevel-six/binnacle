# Build
# Pinned to go.mod's exact version. golang:1.26 could be any patch release, and
# a builder older than go.mod's target makes Go download a toolchain mid-build —
# slower, and a network dependency the build does not otherwise have.
FROM golang:1.26.1 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. client-go is most of it and it is not small.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
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
USER nonroot:nonroot
ENTRYPOINT ["/binnacle"]
