# capture_mics — Biscuit Mic Array Channel Mapping Tool

Standalone tool for mapping the 9 ALSA channels to physical mic positions
on the Echo Dot Gen 2 (biscuit) mic PCB.

## What this is for

The biscuit has a 7-mic array (6 perimeter + 1 centre) on 4× TLV320ADC3101
ADCs giving 8 channels, and the capture is 9 wide. Amazon's firmware mapping
is closed-source. This tool captures raw 9-channel audio and an analysis
script identifies which channel corresponds to which physical mic by playing
a tone from known angles.

**Ch7 and Ch8 are not mics.** They are a stereo loopback of the device's own
playback — Ch7 left, Ch8 right — a hardware echo reference that is always
present and needs no mixer change (measured 2026-08-29; see the Mic Array
section of SETUP.md). Four stereo ADCs account for Ch0–Ch7, so one of those
is an unused ADC input and the ninth channel has no ADC behind it at all.

That is also why this tool's own procedure below cannot see them: it uses an
external tone source with the Dot's speaker silent, and under that condition
both channels read bit-exact zero. **If you are mapping mics, ignore Ch7/Ch8;
if you are measuring the speaker, they are the reference signal, sample-aligned
with the mics because they arrive in the same TDM frame.**

## Build

Inside the `echomuse-compiler` Docker container:

```bash
docker run --rm \
  -v "$(pwd)":/capture \
  -v "$(pwd)/../GoTinyAlsa":/GoTinyAlsa \
  echomuse-compiler \
  bash -c "cd /capture && go build -tags server -o capture_mics ."
```

## Deploy

```bash
adb push capture_mics /sdcard/capture_mics
adb shell "su -c 'cp /sdcard/capture_mics /data/local/bin/capture_mics && chmod 755 /data/local/bin/capture_mics'"
```

**Note:** Stop EchoMuse first — both tools need exclusive access to ALSA device 24:

```bash
adb shell "su -c 'stop echomuse'"
# ... run capture ...
adb shell "su -c 'start echomuse'"
```

## Test procedure

You need a consistent tone source — a phone playing a 440Hz sine wave works well.
Mark the front of the Echo Dot as the action button side. That's 0°.

For each of the 6 positions (0°, 60°, 120°, 180°, 240°, 300°):

1. Place tone source ~50cm directly in front of the device at the current angle
2. Capture:
   ```bash
   adb shell "su -c '/data/local/bin/capture_mics 5'"
   adb pull /data/local/tmp/capture.raw capture_0deg.raw   # adjust name per angle
   ```
3. Analyse:
   ```bash
   python3 analyse_capture.py capture_0deg.raw --label "0deg_action_button"
   ```
4. Note the loudest channel

After 3–4 positions the mapping is unambiguous. The centre mic is **ch6** and
is consistently loud regardless of angle. (This line used to guess "ch8 most
likely"; ch8 is the right-channel echo reference and is silent for the whole
of this procedure.)

## Output

`capture.raw` — raw interleaved S24_3LE, 9 channels, 16kHz.

Format: `[ch0_b0][ch0_b1][ch0_b2][ch1_b0]...[ch8_b2]` per frame, 16000 frames/sec.

File size for 5 seconds: `5 × 16000 × 9 × 3 = 2,160,000 bytes` (~2.1MB)

## Analysis

```bash
# Basic RMS table
python3 analyse_capture.py capture_0deg.raw

# With waveform plot (requires matplotlib)
python3 analyse_capture.py capture_0deg.raw --plot --label "0deg"
```

Install deps if needed:
```bash
pip3 install numpy matplotlib
```

---

# oww_probe — on-device wake word verification and cost

Answers the two questions the host tests cannot: does the ONNX Runtime
binding actually work on this hardware, and what does it cost in the shape it
will really run?

It touches no microphone, no LEDs and not the running server — it reads three
model files, a fixture and `libonnxruntime.so`, and prints. Safe on a live
device, though phase 2 competes for CPU with whatever else is running, which
is deliberate.

## What it checks

**Phase 1 — correctness.** Runs `fixture.Verify`, the *same* comparison the
host test runs, against the same golden capture of openWakeWord's Python
output (`internal/wakeword/testdata/ort_fixture.bin`). Compares
melspectrogram output, embeddings and scores per 80ms chunk, plus a probe
block covering the classifier with a wide-range tensor — necessary because
the fixture's synthetic audio scores 0.0000 everywhere, so the audio path
alone would also pass with a classifier stuck at zero. A pass means the real
pipeline reproduces Python **on ARM**, not merely on x86.

**Phase 2 — cost.** Paces frames at the real 12.5Hz duty cycle and reports
process CPU from `getrusage`. Flat-out latency is the misleading number here:
ORT's thread pool can burn several times the inference cost spin-waiting in
the 60ms gaps, and this also captures what a C benchmark cannot — cgo call
overhead and Go GC pressure from the buffering.

Exit status is 0 only if every stage matched Python.

## Deploy

Unlike the other tools it is not a standalone module (it imports
`internal/wakeword`, which Go's internal rule only allows from inside the
module), so `build_tools.sh` builds it with the whole device module mounted.

Needs four things on the device besides the binary: `melspectrogram.onnx`,
`embedding_model.onnx`, a classifier `.onnx`, `ort_fixture.bin`, and the
armeabi-v7a `libonnxruntime.so` (12.3MB, from the Maven
`onnxruntime-android` AAR under `jni/armeabi-v7a/`).

Without adb, push over the controller shell plane:

```bash
docker cp controller/tools/push_file.py echomuse-controller:/tmp/
docker cp device/build/oww_probe echomuse-controller:/tmp/
docker exec echomuse-controller python /tmp/push_file.py \
    <device_id> /tmp/oww_probe /data/local/tmp/oww_probe --chmod 755
```

## Run

```bash
oww_probe -classifier /data/local/tmp/hey_mycroft_v0.1.onnx -seconds 30
```

Flags exist for `-threads`, `-xnnpack` and `-spinning` so the measured
optimum can be re-derived on the hardware rather than trusted: on an Echo Dot
Gen 2, one thread with XNNPACK and spinning **off** cost 36.2% of one core,
where ORT's defaults cost 243%.

# mixednet_ref — the reference for `internal/mixednet`'s vectors

Builds a small harness around the Voice Incubator firmware's own C++
mixednet executor (`OHF-Voice/firmware-voice-incubator`, fetched at a pinned
commit and sha256-checked, never vendored), so the expected values in
`internal/mixednet/testdata` come from upstream's code and never from the Go
port. It also packs real wake word models with the app's own packer
(`OHF-Voice/voice-incubator` at a pinned commit, through `pack.py`): the
app's pooled test fixture and three built-in models from its models
repository, each sha256-checked. Host only; needs `g++`, `git`, `curl`, and a
Python with `numpy` and `tflite` (the app's pins: `numpy>=2,<3`,
`tflite>=2.18,<3`).

```bash
PYTHON=<python with numpy, tflite> device/tools/mixednet_ref/build.sh /tmp/mixednet_ref
cd device && MIXEDNET_REF=/tmp/mixednet_ref \
    go test ./internal/mixednet -run Vectors -update
```

It also builds `features_harness`, the reference for `internal/microfeatures`:
that package's vendored front end C, with the Voice Incubator detector's own
configuration and scaling lines cut verbatim from its `wake_detector.cpp` (the
file sha256-checked). Regenerate those vectors with
`MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/microfeatures -update`.
The C builds come first in `build.sh`; only the model packing needs
`PYTHON`.

The harness prints, per invoke, the probability, the logit and an FNV-1a
hash of the whole working arena (built with `MIXEDNET_DEBUG`), so a one-step
difference anywhere in the net fails the vectors, not only one that reaches
the logit. It is built with `-fwrapv`, matching Go's wrapping int32
arithmetic on the synthetic cases that overflow. `pack.py` prints each
blob's op mix and shift range.
