# syntax=docker/dockerfile:1

# The head as the deployment runs it: one static binary and the state
# directory it owns.
#
# The builder is pinned to go.mod's toolchain rather than to a tag of its own,
# so the image and CI cannot drift apart — a fork whose container is built by
# a different compiler than its tests is a fork with two answers to what it
# does.
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first: a source-only change then does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off. distroless/base carries no toolchain to link against, and nothing
# here asks for one: the sqlite driver is glebarez/modernc, which is pure Go.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /headscale ./cmd/headscale

# The state directory, made HERE because the runtime image has no shell to
# make it in. It holds the sqlite database, the noise key, and — the reason
# this is not merely a mount point — registry.cache_path, which the head
# rewrites every time the change stream moves. A head that cannot write it
# still runs, and full-refreshes on every boot instead, which is the kind of
# fault that looks like nothing until an installation is large.
#
# 65532 is distroless's nonroot uid/gid, and it is the user the head runs as.
RUN mkdir -p /state && chown 65532:65532 /state

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /headscale /headscale
COPY --from=build --chown=65532:65532 /state /var/lib/headscale

USER nonroot:nonroot

# Declared so a deployment that forgets to mount one still gets a writable
# directory rather than a head that cannot persist its cache.
VOLUME ["/var/lib/headscale"]

EXPOSE 8080

ENTRYPOINT ["/headscale", "serve"]
