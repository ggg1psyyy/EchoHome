#!/bin/bash
# Builds the on-Echo tools that are EchoHome's own, in the pinned compiler
# image. The hardware probes EchoMuse used to work out the Echo (capture_mics,
# bf_capture, mixer_probe, ble_probe, ...) are EchoMuse's:
# https://github.com/wilbowes/EchoMuse/tree/main/device/tools
set -e

BUILD_DIR="$(pwd)/build"
REPO_ROOT=$(git rev-parse --show-toplevel)

# A module tool imports the device module's internal packages, which Go's
# internal-package rule only permits from inside the module, so it builds
# with the whole module mounted.
build_module_tool() {
    local name=$1

    echo "Building $name..."
    mkdir -p "$BUILD_DIR"
    docker run --rm \
        --entrypoint bash \
        -e CGO_LDFLAGS="-Wl,--hash-style=both" \
        -v "$(pwd)":/sdk \
        -v "$REPO_ROOT/GoTinyAlsa":/GoTinyAlsa \
        echomuse-compiler \
        -c "cd /sdk && go build $2 -o build/$name ./tools/$name"

    echo "Output: $BUILD_DIR/$name"
}

build_module_tool mixednet_probe  # Voice Incubator wake word: parity with the host + paced CPU
