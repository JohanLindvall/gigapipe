#!/usr/bin/env bash
#
# Build a multi-arch (arm64 + amd64) gigapipe Docker image with buildx.
#
# Usage:
#   ./build.sh [IMAGE[:TAG]]
#
# Environment variables:
#   IMAGE      Image name[:tag]            (default: gigapipe:latest)
#   PLATFORMS  Target platforms            (default: linux/amd64,linux/arm64)
#   VIEW       Build with the `view` tag   (default: 0)
#   PUSH       Push to the registry (1/0)  (default: 0)
#   LOAD       Load into the local docker  (default: 0, single-platform only)
#   BUILDER    buildx builder name         (default: gigapipe-builder)
#
# Examples:
#   ./build.sh                                   # build both arches, keep in cache
#   PUSH=1 ./build.sh myrepo/gigapipe:v1.2.3     # build both arches and push
#   LOAD=1 PLATFORMS=linux/amd64 ./build.sh      # build amd64 and load locally
set -euo pipefail

IMAGE="${1:-${IMAGE:-gigapipe:latest}}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
VIEW="${VIEW:-0}"
PUSH="${PUSH:-0}"
LOAD="${LOAD:-0}"
BUILDER="${BUILDER:-gigapipe-builder}"

cd "$(dirname "$0")"

# A dedicated buildx builder with the docker-container driver is required for
# cross-platform builds; create it once and reuse it on later runs.
if ! docker buildx inspect "$BUILDER" >/dev/null 2>&1; then
    echo ">> Creating buildx builder '$BUILDER'"
    docker buildx create --name "$BUILDER" --driver docker-container --bootstrap
fi

OUTPUT_ARGS=()
if [ "$PUSH" = "1" ]; then
    OUTPUT_ARGS+=(--push)
elif [ "$LOAD" = "1" ]; then
    case "$PLATFORMS" in
        *,*) echo "ERROR: LOAD=1 supports a single platform only; set PLATFORMS=linux/amd64 (or arm64)." >&2
             exit 1 ;;
    esac
    OUTPUT_ARGS+=(--load)
fi

echo ">> Building $IMAGE for $PLATFORMS (VIEW=$VIEW)"
docker buildx build \
    --builder "$BUILDER" \
    --platform "$PLATFORMS" \
    --build-arg VIEW="$VIEW" \
    --tag "$IMAGE" \
    "${OUTPUT_ARGS[@]}" \
    .

if [ "$PUSH" != "1" ] && [ "$LOAD" != "1" ]; then
    echo ">> Built into the buildx cache only. Re-run with PUSH=1 to push, or LOAD=1 (single platform) to load locally."
fi
