# Third-party components

EchoMuse is MIT licensed (see `LICENSE`). It vendors and links the components
below, each of which keeps its own licence. All are permissive and compatible
with redistribution under MIT, and each requires that its copyright notice
travels with the software — which is what this file is for.

The device binary published on the releases page is a **combined work**: it
links SpeexDSP and GoTinyAlsa. BSD-3-Clause asks that binary redistributions
reproduce the copyright notice "in the documentation or other materials
provided with the distribution", and this file is that material. If the binary
is ever distributed somewhere other than alongside this repository, this notice
needs to travel with it.

## Vendored into this repository

| Component | Where | Licence | Copyright |
|---|---|---|---|
| SpeexDSP (acoustic echo canceller) | `device/internal/aec/` | BSD-3-Clause | Xiph.Org Foundation, Jean-Marc Valin, Analog Devices, CSIRO |
| ONNX Runtime C API header | `device/internal/wakeword/ort/include/` | MIT | Microsoft |
| pffft (the echo canceller's FFT) | `device/internal/aec/src/pffft/` | FFTPACK licence (BSD-style), in its source headers | Julien Pommier; University Corporation for Atmospheric Research |
| libopus 1.5.2 (Sendspin's decoder) | `device/internal/opus/libopus/` | BSD-3-Clause | Xiph.Org Foundation, Skype Limited, Octasic, Jean-Marc Valin, Timothy B. Terriberry, CSIRO, Gregory Maxwell, Mark Borgerding, Erik de Castro Lopo |
| Voice Incubator firmware's mixednet executor, model parser and wake word detector, ported to Go | `device/internal/mixednet/` | Apache-2.0 (`LICENSE` there); changed: ported from C++ | OHF-Voice (firmware-voice-incubator) |
| ESPMicroSpeechFeatures (the wake word front end; TensorFlow Lite Micro's microfrontend), with KISS FFT | `device/internal/microfeatures/libmsf/` | Apache-2.0 (`LICENSE` there); KISS FFT BSD-3-Clause, in its source headers | The TensorFlow Authors; Kevin Ahrendt (microWakeWord); Mark Borgerding (KISS FFT) |
| ESPHome native API definitions (aioesphomeapi 46.25.0's compiled `api.proto` and `api_options.proto`), and the Go code generated from them | `device/tools/esphome_gen/descriptors/`, `device/internal/esphome/api/` | MIT (`device/tools/esphome_gen/descriptors/LICENSE`) | Otto Winter |
| Wake word models and a test recording, as test data | `device/internal/mixednet/testdata/`, `device/internal/incubator/testdata/` | Apache-2.0 (the `README.md` there names each source) | OHF-Voice, Kevin Ahrendt (microWakeWord), ESPHome |

Full licence texts sit beside the code, in `COPYING` or `*_LICENSE` files. Do
not remove them — they are the attribution the licences require.

## Linked as a submodule

| Component | Where | Licence | Copyright |
|---|---|---|---|
| GoTinyAlsa (`wilbowes/GoTinyAlsa` fork) | `GoTinyAlsa/` | BSD-3-Clause | binozoworks |

The fork exists to carry a `GetAudioStream` defer-in-loop leak fix; see
`device/CLAUDE.md` before repointing it upstream.

## Go modules compiled into the device binary

Fetched by `go` at build time (`device/go.mod`, pinned by `device/go.sum`) and
linked into the firmware, so they travel with it. All permissive:

| Module | Licence |
|---|---|
| `github.com/gorilla/websocket` | BSD-2-Clause |
| `github.com/flynn/noise` | BSD-3-Clause |
| `github.com/grandcat/zeroconf` (and its `github.com/miekg/dns`, BSD-3-Clause; `github.com/cenkalti/backoff`, MIT) | MIT |
| `github.com/gvalkov/golang-evdev` | BSD-3-Clause |
| `github.com/mewkiz/flac` (and its `mewkiz/pkg`, `mewpkg/term`) | Unlicense (public domain) |
| `github.com/icza/bitio` (used by `mewkiz/flac`) | Apache-2.0 |
| `golang.org/x/crypto`, `golang.org/x/sys`, `golang.org/x/net`, `golang.org/x/sync` | BSD-3-Clause |
| `google.golang.org/protobuf` (the ESPHome API's messages; its generator, in `device/tools/esphome_gen`) | BSD-3-Clause |

## Installed at build or run time

These are dependencies rather than vendored code — they are fetched by the
Dockerfiles and `requirements.txt`, and are not redistributed as source here.
Listed because they end up in the published container image:

- The Python dependencies in `controller/requirements.txt`, all permissive.
