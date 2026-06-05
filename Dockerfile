# Build on the native build platform and cross-compile to the target platform.
# buildx injects BUILDPLATFORM/TARGETOS/TARGETARCH automatically.
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS builder

# Provided automatically by `docker buildx build`.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
# The cache mount keeps the module cache across builds even when they do change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VIEW
# CGO is disabled (pure-Go project), so Go's built-in cross-compiler handles
# every target arch without an external C toolchain or QEMU emulation.
# The module cache is arch-independent (shared); the build cache is arch-specific
# (keyed by TARGETARCH) so parallel multi-platform builds don't contend.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    sh -c 'if [ "$VIEW" = "1" ]; then \
        go build -tags view -o gigapipe cmd/gigapipe/main.go ; \
    else \
        go build -o gigapipe cmd/gigapipe/main.go ; \
    fi'

FROM alpine:3.21
COPY --from=builder /src/gigapipe /gigapipe
ENV PORT=3100
EXPOSE 3100
CMD ["/gigapipe"]
