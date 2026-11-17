#!/bin/sh
# Builds the mixednet reference harness from the Voice Incubator firmware's
# executor at a pinned commit, sha256-checked, into $1 (default
# /tmp/mixednet_ref), and packs real wake word models with the app's own
# packer (also pinned) into $1/blobs. Nothing from it is vendored into the
# firmware; it only produces internal/mixednet/testdata:
#
#   PYTHON=<python with numpy, tflite> tools/mixednet_ref/build.sh /tmp/mixednet_ref
#   MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/mixednet -update
set -eu
REPO=https://github.com/OHF-Voice/firmware-voice-incubator.git
COMMIT=64ce6c2aa9f17b779293626a76d36fd9169e06d8
out=${1:-/tmp/mixednet_ref}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"
src="$out/firmware-voice-incubator"
if [ ! -d "$src/.git" ]; then
	git init -q "$src"
	git -C "$src" remote add origin "$REPO"
fi
git -C "$src" fetch -q --depth 1 origin "$COMMIT"
git -C "$src" checkout -q "$COMMIT"
dir="$src/components/voice_incubator"
(cd "$dir" && sha256sum -c - <<SUMS
e69b5fb66b823a0a8470e6c4a5bba450fba0d0cf1717bd6206ae5387ceb9642b  mixednet.h
2326d004f6a189d8b785769f6b3c4fb29f9d7123752db4387c318378a75a22c7  mixednet.cpp
59523a2ed06fc6b462dd844da063013d14ecd18281b502f0c86d06c26c80cded  mixednet_blob.h
eeed5a26e6fe264bc6f7f0a52e1b31a124fac942aab8312bf79a6000b916ce8a  mixednet_blob.cpp
SUMS
)
# -fwrapv: signed overflow wraps, as Go's int32 arithmetic does, so a synthetic
# model that overflows an accumulator has one answer to be exact with.
g++ -std=c++17 -O2 -fwrapv -DMIXEDNET_DEBUG -I"$dir" -o "$out/harness" \
	"$here/harness.cpp" "$dir/mixednet.cpp" "$dir/mixednet_blob.cpp"
echo "built $out/harness from $COMMIT"

# The front end harness: internal/microfeatures' vendored C (byte-identical to
# the component release the Voice Incubator firmware builds against), and the
# detector's own configuration and scaling lines, cut verbatim from its
# wake_detector.cpp, whose whole text is pinned here.
(cd "$dir" && echo "5316b6bf8a0e887084f06cfd35baf11cd0a5134919d6e137cd88a4aa98ab05c1  wake_detector.cpp" | sha256sum -c -)
gen="$out/gen"
mkdir -p "$gen"
grep -E '^[[:space:]]*this->frontend_config_\.' "$dir/wake_detector.cpp" > "$gen/config.inc"
awk '/constexpr int32_t value_scale = 256;/,/features\[i\] = static_cast<int8_t>/' "$dir/wake_detector.cpp" > "$gen/scale.inc"
[ "$(wc -l < "$gen/config.inc")" -eq 15 ] && [ "$(wc -l < "$gen/scale.inc")" -eq 5 ] ||
	{ echo "the detector's lines moved: re-check gen/*.inc" >&2; exit 1; }
msf="$here/../../internal/microfeatures/libmsf"
objs=""
for c in "$msf"/src/*.c; do
	o="$gen/$(basename "${c%.c}").o"
	gcc -std=c11 -O2 -w -I"$msf/include" -I"$msf/src" -c "$c" -o "$o"
	objs="$objs $o"
done
# shellcheck disable=SC2086
g++ -std=c++17 -O2 -I"$msf/include" -I"$msf/src" -I"$gen" -o "$out/features_harness" \
	"$here/features_harness.cpp" $objs -lm
echo "built $out/features_harness"

# The stream harness: the firmware's ModelStream, declaration and methods cut
# verbatim from wake_detector.h and wake_detector.cpp (both pinned here).
(cd "$dir" && echo "45a7da96fa71491b71c39d70e9dca3197945e6a120de36db4d6c1cc77c8e4787  wake_detector.h" | sha256sum -c -)
awk '/^struct ModelStream \{/,/^\};/' "$dir/wake_detector.h" > "$gen/stream_decl.inc"
awk '/^bool ModelStream::setup/{p=1} /^bool WakeDetector::setup/{p=0} p' "$dir/wake_detector.cpp" > "$gen/stream_impl.inc"
[ "$(wc -l < "$gen/stream_decl.inc")" -eq 19 ] && [ "$(wc -l < "$gen/stream_impl.inc")" -eq 54 ] ||
	{ echo "ModelStream moved: re-check gen/stream_*.inc" >&2; exit 1; }
g++ -std=c++17 -O2 -fwrapv -I"$dir" -I"$gen" -o "$out/stream_harness" \
	"$here/stream_harness.cpp" "$dir/mixednet.cpp" "$dir/mixednet_blob.cpp"
echo "built $out/stream_harness"

# The detector harness: the firmware's own loading, stop-word arming,
# process_features_ and take_report, cut verbatim from wake_detector.h and .cpp.
awk '/^static constexpr uint8_t FEATURE_SIZE = 40;/{p=1} p{print} p&&/^struct WakeModel \{/{w=1} w&&/^\};/{exit}' \
	"$dir/wake_detector.h" > "$gen/detector_decls.inc"
awk '/^bool WakeDetector::begin_model/{p=1} /^size_t WakeDetector::total_arena_size/{p=0} p' \
	"$dir/wake_detector.cpp" > "$gen/loading_impl.inc"
awk '/^void WakeDetector::process_features_/{p=1} p{print} p&&/^}/{exit}' \
	"$dir/wake_detector.cpp" > "$gen/process_impl.inc"
awk '/^void WakeDetector::take_report/{p=1} p{print} p&&/^}/{exit}' \
	"$dir/wake_detector.cpp" > "$gen/report_impl.inc"
[ "$(wc -l < "$gen/detector_decls.inc")" -eq 93 ] && [ "$(wc -l < "$gen/loading_impl.inc")" -eq 109 ] &&
	[ "$(wc -l < "$gen/process_impl.inc")" -eq 60 ] && [ "$(wc -l < "$gen/report_impl.inc")" -eq 17 ] ||
	{ echo "the detector moved: re-check gen/detector_*.inc" >&2; exit 1; }
g++ -std=c++17 -O2 -fwrapv -I"$dir" -I"$gen" -o "$out/detector_harness" \
	"$here/detector_harness.cpp" "$dir/mixednet.cpp" "$dir/mixednet_blob.cpp"
echo "built $out/detector_harness"

# Real models, packed by the app exactly as it would push them: its own test
# fixture ("okay nabu" as the app trains it, so with max pooling) and three of the
# built-in models it downloads, all Apache-2.0 (the models repo's README).
APP_REPO=https://github.com/OHF-Voice/voice-incubator.git
APP_COMMIT=3a6d7e7aba0520aa43f84c23f2fdd450c5fdf93e
MODELS=https://huggingface.co/rhasspy/voice-incubator-models/resolve/e359089e2dfe430415b24410a089fdcf36fd5bf9/micro-wake-word
app="$out/voice-incubator"
if [ ! -d "$app/.git" ]; then
	git init -q "$app"
	git -C "$app" remote add origin "$APP_REPO"
fi
git -C "$app" fetch -q --depth 1 origin "$APP_COMMIT"
git -C "$app" checkout -q "$APP_COMMIT"
mkdir -p "$out/models" "$out/blobs"
cp "$app/tests/fixtures/pooled.tflite" "$out/models/pooled.tflite"
for m in okay_nabu hey_jarvis stop; do
	curl -fsSL -o "$out/models/$m.tflite" "$MODELS/$m.tflite"
done
(cd "$out" && sha256sum -c - <<SUMS
bda4af967baa1d18898f3b3478f741675c62e363d44d3de8255cd478afdad9d4  voice-incubator/voice_incubator/mixednet.py
4a74678297f5b56396b88a4d63e3b1d3492547d2736d8af4afcfb33c71da5647  voice-incubator/voice_incubator/_mixednet_compiler.py
d67397da43b1de3b5beb5fa1d229258a341b9451608013fb6e453cb9e6e06dbe  models/pooled.tflite
d89128c4d16a72de429119fb2254ce46649553c2a24f5dd840175c80d7b9d094  models/okay_nabu.tflite
21a7976add39ee24ec96c63d96b7aaa18e24d1d9824b963e451da8feb4b78b77  models/hey_jarvis.tflite
b5a18c4ad681a89950dfade31011e1631bdcb333e93c84519a1a63ff4f071146  models/stop.tflite
SUMS
)
for m in pooled okay_nabu hey_jarvis stop; do
	"${PYTHON:-python3}" -I "$here/pack.py" "$app" "$out/models/$m.tflite" "$out/blobs/$m.blob"
done
