# Pinned by DIGEST, not by tag, and that is load-bearing rather than tidiness.
#
# This image carries the Go toolchain and the NDK that compile the firmware, so
# it is the layer sitting directly on top of FireOS 5 — Android 5.1, API 22,
# armv7a, a 2015 platform we cannot upgrade. `:latest` is a third party's
# floating tag: a rebuild upstream would change the compiler under us with no
# PR, no review and nothing in CI to notice, and the first symptom would be a
# binary that no longer runs on the hardware. Everything else in this project
# can be rolled back from the dashboard; a device that will not boot its new
# slot is recovered by hand.
#
# The digest below is the image that built every release through 3.0.0-ea.1
# (its own Go 1.24.0, NDK 21.4.7075529). It was `:latest` when pinned, so this
# froze a known-good state rather than choosing a new one. Its Go is replaced
# below; the NDK, sysroot and tinyalsa stay the image's.
#
# To move it: pull the new digest deliberately, build, and put the binary on a
# real Dot before the pin changes. `go vet` and the host tests prove nothing
# here — they run on amd64 with the host toolchain, and this image is only ever
# exercised by compile.sh and release.yml.
FROM ghcr.io/binozo/echogo@sha256:c9456551c639290b1cc3e1df891177beadb5d0d7621a5c2ec81079a276019147

# Go 1.26.11 in place of the image's 1.24.0 (2026-11-19): the device module's
# dependencies (golang.org/x/crypto 0.59, x/sys 0.50) need go 1.26. Go 1.25
# and 1.26 change nothing for linux/android arm (their release notes' Ports).
# Moved as this file says a pin moves: built, then run on an Echo (00LQ, emOS,
# whose firmware runs against the FireOS system partition's bionic and
# linker): the wake word and a call, the button, both mutes, Home Assistant
# media, the ring and the Bluetooth proxy. The tarball is go.dev's, checked
# against the sha256 go.dev publishes for it.
ARG GO_VERSION=1.26.11
ARG GO_SHA256=9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec
RUN curl -sfL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
 && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
 && rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz \
 && go version | grep -q "go${GO_VERSION} "

ENV API=22
ENV CC=/opt/android/ndk/21.4.7075529/toolchains/llvm/prebuilt/linux-x86_64/bin/armv7a-linux-androideabi22-clang
ENV CXX=/opt/android/ndk/21.4.7075529/toolchains/llvm/prebuilt/linux-x86_64/bin/armv7a-linux-androideabi22-clang++
ENV AS=/opt/android/ndk/21.4.7075529/toolchains/llvm/prebuilt/linux-x86_64/bin/armv7a-linux-androideabi22-clang
ENV NDK_LIBS=/opt/android/ndk/21.4.7075529/toolchains/llvm/prebuilt/linux-x86_64/sysroot/usr/lib/arm-linux-androideabi/22
ENV CGO_LDFLAGS="-Wl,--hash-style=both"

# Recompile tinyalsa for API 22
WORKDIR /tinyalsa/src
RUN make clean && \
    make CC=$CC LD=$LD AR=$AR AS=$AS libtinyalsa.a libtinyalsa.so && \
    cp /tinyalsa/src/libtinyalsa.so $NDK_LIBS/libtinyalsa.so

WORKDIR /sdk
