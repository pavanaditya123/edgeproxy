# syntax=docker/dockerfile:1
#
# Builds edgeproxy (default) or the demo backend:
#   docker build -t edgeproxy .
#   docker build -t demobackend --build-arg TARGET=demobackend .

FROM golang:1.26 AS build
WORKDIR /src
# Download modules in their own layer so code changes don't refetch them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGET=edgeproxy
# A static binary (no cgo) runs on an image with no libc at all.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

# Distroless: no shell, no package manager, nothing but the binary and CA
# certificates, so there's almost nothing for an attacker to use. Runs as an
# unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
