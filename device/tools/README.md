# Device tools

EchoHome's own tools for the firmware: generators and interop checks for the
protocols it speaks, and probes for its wake word stack and echo canceller.

| Tool | What it does |
|---|---|
| `esphome_gen` | Generates the ESPHome native API messages from aioesphomeapi's descriptors (`internal/esphome/api`) |
| `esphome_interop` | Drives the Echo's ESPHome server with aioesphomeapi, the library Home Assistant uses |
| `incubator_interop` | Interop vectors for `internal/incubator` against the Voice Incubator app's own `protocol.py` (below) |
| `sendspin_interop` | Runs the Sendspin player against aiosendspin, the library Music Assistant ships |
| `mixednet_ref` | Builds the reference the wake word vectors come from (below) |
| `mixednet_probe` | The Voice Incubator wake word stack on an Echo: parity with the host, and its CPU cost (below) |
| `aec_replay` | Runs a recorded capture through the firmware's microphone front end and echo canceller on the host; `tools/audio_bench` scores it |

`build_tools.sh` builds the on-Echo ones in the pinned compiler image.

The hardware probes that worked out the Echo itself (`capture_mics`,
`bf_capture`, `mixer_probe`, `speaker_profile`, `ble_probe`, the porting
scripts, ...) are EchoMuse's, by Wil Bowes and the EchoMuse contributors:
[wilbowes/EchoMuse device/tools](https://github.com/wilbowes/EchoMuse/tree/main/device/tools)
and [porting](https://github.com/wilbowes/EchoMuse/tree/main/porting).

# mixednet_probe — the Voice Incubator wake word stack on an Echo

Answers what the host tests cannot: do `internal/microfeatures` and
`internal/mixednet` give the host's answers on the real 32-bit ARM target,
and what do they cost? It touches no microphone, speaker, LEDs or mixer and
leaves the server running: it reads a model blob (and optionally a PCM file)
and prints.

- **Phase 1:** 10 s of seeded audio, generated in integers so the input is
  the same bytes on every CPU, through the front end and one stream; prints a
  hash of every frame and stream output. `-expect <host hash>` exits 1 on a
  mismatch; `-pcm` also runs a 16 kHz mono file; `-trace` prints per-second
  front end and stream hashes, and `-frame N` one frame's values (`-frame -N`
  a hash per frame for the first N), which is how a difference is located.
- **Phase 2:** the same chain paced in real time, 80 ms of audio a tick, with
  1, 2 and 4 streams: process CPU from `getrusage` as a share of one core,
  and the longest tick.

Built by `build_tools.sh` with the device module mounted (no `-tags
server`). The phase 1 hash is not expected to match between the host
(`4d99a062`) and an Echo (00LQ: `2b79ae70`): see the known difference below.
To compare, run `-pcm` with a speech recording on both (identical so far),
and `-trace -frame -998` on both and diff the per-frame hashes to count the
frames that differ; `-expect` is for comparing two runs of one platform. Push the binary and
`okay_nabu.blob` to `/data/local/tmp` as for `oww_probe`, and start phase 2
detached (`nohup ... > cost.out &`): the shell plane drops after about 50 s
of load.

On 00LQ (emOS, v2.19.0, 2 cores online, 42 °C, the server running),
2026-11-17: 4.2% of one core with one model, 7.1% with two, 12.3% with four,
no late tick. A speech recording matched the host on every frame; the
probe's broadband audio differed in 2 of 998 frames, both channel 38
(~7 kHz), one int8 step (`docs/history/device.md`).

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
    go test ./internal/mixednet -update
```

It also builds `features_harness`, the reference for `internal/microfeatures`:
that package's vendored front end C, with the Voice Incubator detector's own
configuration and scaling lines cut verbatim from its `wake_detector.cpp` (the
file sha256-checked). Regenerate those vectors with
`MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/microfeatures -update`.
And `stream_harness`, the reference for `internal/mixednet`'s `Stream`: the
detector's `ModelStream`, declaration and methods cut verbatim from
`wake_detector.h` and `.cpp` (both sha256-checked), over the firmware's
executor, driven per frame as `process_features_` drives one wake word
model. And `detector_harness`, the reference for `internal/mixednet`'s
`Detector`: the detector's model loading (`begin_model` to `clear_models`),
`set_stop_armed` and `process_features_`, with the declarations they use,
all cut verbatim from `wake_detector.h` and `.cpp`. Not the firmware's: the
stand-ins for its ESP32 allocator, mutex and logging, the `WakeDetector`
class shell (member list and initial values), `queue_detection_` (which
prints) and `main`'s per-frame printing. The C builds come first in `build.sh`; only
the model packing needs
`PYTHON`.

The harness prints, per invoke, the probability, the logit and an FNV-1a
hash of the whole working arena (built with `MIXEDNET_DEBUG`), so a one-step
difference anywhere in the net fails the vectors, not only one that reaches
the logit. It is built with `-fwrapv`, matching Go's wrapping int32
arithmetic on the synthetic cases that overflow. `pack.py` prints each
blob's op mix and shift range.

# incubator_interop — `internal/incubator` against the app's own protocol.py

`gen.py` reads the frames the Go codec writes, parses them the way the Voice
Incubator app does (its `voice_incubator/protocol.py`: `HEADER`, `Message`,
`Hello.parse`), and writes what the app made of them; it also writes every
app-to-satellite message encoded by the app's own `encode`/`encode_json`. The
app checkout is the one `tools/mixednet_ref/build.sh` makes, and `gen.py`
checks `protocol.py`'s sha256. Plain `python3`; the module needs nothing
installed.

```bash
cd device && MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/incubator -update
```

It holds the one trap that matters: the app reads a HELLO without
`protocol` as protocol 1, a Voice PE with a second capture and mixednet wake
words. With the constant set to 1 the interop test fails on five fields.
