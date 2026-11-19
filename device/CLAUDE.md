# CLAUDE.md — `device/`

The Go binary that runs on the Echo. Project-wide direction, the
device/controller compatibility rules, the wire protocol and the release
scheme are in the repo-root `CLAUDE.md`; the setup tool is in
`controller/CLAUDE.md`.

The why behind these rules — incidents, measurements, worked examples — is in
`docs/history/device.md` under the same headings. Read the matching section
there before changing that area.

Since EchoHome the Echo is a **Voice Incubator satellite** (`internal/incubator`)
and **its own ESPHome device** (`internal/esphome`), and works with no
controller. The old Home Assistant voice mode (the wake stream to the
controller, the shadow scorer, listening sessions, the speech gate, AGC,
bounded `lock_mic` turns, the Bluetooth connection stack) is gone.

## Building the device binary

FireOS 5 (API 22) needs the custom compiler image; plain Go cross-compilation won't work.

```bash
git submodule update --init                       # GoTinyAlsa: the wilbowes fork, NOT upstream Binozo
cd device && docker build -t echomuse-compiler compiler/
./compile.sh                                      # → build/server
go test ./internal/... ./pkg/...                  # host, pure-Go logic only
```

- `cmd/` is `//go:build server`: vet it in the compiler image (`go vet -tags
  server ./cmd/...`, as CI's device-firmware-build job does).
- **GoTinyAlsa stays on the fork** until upstream has its playback fix
  (WriteFrames blocks signals around `pcm_write`, #707). It also carries
  `GetAudioStreamChunked` (fork #4, #1139). **Merge fork PRs with a merge
  commit, never squash**.
- **The compiler base (`compiler/Dockerfile`: NDK 21.4.7075529, with Go
  1.26.11 swapped in, sha256-checked) is pinned by DIGEST and must stay so**;
  **moving the pin, or its Go, needs a real device in the loop**.
- `compile.sh` embeds the version via `-ldflags "-X .../client.Version=..."`;
  dirty trees get `YYYYMMDD-HHMM-dev`.
- **Release**: a `v*` tag runs `release.yml`. **Tag with `git tag -a
  --cleanup=verbatim`** — the annotation is the release body the dashboard
  shows beside an update.

## The microphone path (the satellite's)

```
raw frame (the board's MicArray) → the wake mic (beamformer unlocked) + fixed mic gain (micGainDb, on the 24-bit samples) → mono S16_LE → [AEC] → 80 ms frames → the satellite's tap
```

- **One microphone, the wake mic (`wakeMic`)**, for the wake word and calls
  alike: 0 is the board's default (its centre mic, or its first edge mic),
  1..N an edge mic. An escape hatch for a dead centre mic (0SE7). It is
  Home Assistant's "Wake word microphone" select on the Echo (key 28, kept in
  `settings.json`); the controller's push only seeds it.
- **`streamMic` (internal/client/data.go) runs only while the satellite holds
  the microphone** (`OpenCallMic`/`CloseCallMic`), sends nothing anywhere
  else, and survives a lost link. **Never reorder the AEC, echoref or
  beamformer steps**; a wake word that suddenly scores low is the first sign.
- **Batches are 160ms (2560 samples)**: `GetAudioStream` reads the whole ALSA
  buffer (512 × 5).
- **AEC** (`internal/aec/`, vendored SpeexDSP-1.2.1), **on by default**:
  - **Its FFT is pffft** (`src/pffft/`, byte-identical by `vendor.sh`).
  - **Hardware reference (#385)**: ch8 is an in-frame loopback of our
    playback. Promoted only when BIT-EXACT ZERO while silent AND carrying
    audio while playing (`noteEchoRef`).
  - **One filter per microphone (#814)**: `aec.ProcessMicsWithRef`; one saved
    file, `aec_echo_path.v3.bin`. v2 stays on /data for the other A/B slot.
  - **De-aliased (#975) — never "simplify" back to raw ch8.**
  - **No volume scalar on the reference; unity gain on its extraction; never
    raise the DAC above unity (127).**
  - **Loopback on its own PCM (donut, #1140)**: `internal/echoref`, used only
    while locked, never latched; **test the lead against first principles**.
  - **That loopback is read a period at a time** (`GetAudioStreamChunked`).
- Mute: the satellite closes its microphone (`incubatorMute`), and the ADCs
  are muted in hardware. Software Mute (HA) zeroes the mic channels
  (`internal/softmute`). Nothing remote lifts the button's mute.

## The Voice Incubator satellite (`internal/incubator`)

Runs from the controller's `incubator_satellite` message or, standalone, from
its saved copy (`incubator.json`) at boot. Its wake words are the app's
(`internal/mixednet`, `internal/microfeatures`); the button goes to it
locally (`incubatorLocalPress`), the ring is its own (`ring.go`; repainted
after any clear, `Repaint`). A stream's end goes to the media player or the
satellite (`incubatorStreamEnded`: its ring and duck end there). Its
`incubator_state` report carries `monitored` for the privacy line.

## The Echo's ESPHome device (`internal/esphome`, `cmd/esphome.go`)

The native API on 6053, **always on from boot**, plaintext until Home
Assistant provisions a key (`api_encryption_provisionable`, `esphome.key`),
then Noise. **Its MAC is the WiFi chip's** (`platform.WiFiMAC`, kept in
`esphome.mac`), shared with the satellite: Home Assistant keys the device
on it and follows a changed IP by it. Messages are generated from aioesphomeapi's descriptors
(`tools/esphome_gen`). **Entity keys are append-only**: media player 1, event
2, ambient 3, mic muted 4, CPU 5, hottest zone 6, Software Mute 20, mute ring
21, remote volume arc 22, live listening 23, LED Ring 24, start call 25,
multi-tap 26, Bluetooth proxy 27, wake word mic 28, Sendspin player 29,
ring while music plays 32 (30 and 31 were on a development build only: not
reused). **Timers read Go's
monotonic clock**, never the wall clock HA's time may step.

### The Echo's own settings (`internal/owned`, `cmd/owned.go`)

What HA changes on the Echo (the switches, the wake mic select) is **kept on
the Echo** (`settings.json`, values and ints), applied at boot, **never
reported**; a controller push only seeds a key the Echo has no value of
(`Seed`, `SeedInt`). **Sendspin's player switch is the exception**: the
dashboard's switch also sets it, a push counting when it differs from the
last pushed (`sendspinPushed`), and HA may turn it on only once the Echo is
paired (pairing stays the setup tool's).

### The kept config (`config.LastPath`)

The last config push is saved (`config.json`) and applied at boot before any
controller, the volume with it (`keepVolumeSoon`). **The output chain always
runs** (EQ, bass guard, limiter, the voicing): every sound is the Echo's own.

## The speaker's voicing (stock sound, #229, 2026-10)

- **Firmware announcing `stock_sound` always voices its speaker; no key turns
  it off.** The voicing is board data (`board.Speaker.Voicing`).
- Order, with no plug: volume → user EQ → correction → bass guard + upper
  compressor bands → limiter (`speaker.refreshStock`). The law is on only with
  a voicing (`stockLawOn`).
- **The bass guard is held on** whatever `bassGuardEnabled` says.
- Python (`em_eq`, `em_mbc`, `em_volume`) is the reference; Go is bit-exact
  to `outchain/testdata` — regenerate with `gen_vectors.py` after changing
  either.

## Key Go packages

| Package | Role |
|---------|------|
| `cmd/server.go`; `internal/client/` | Entry point; `/control` and `/data` clients, the satellite's mic path |
| `internal/incubator/` | The Voice Incubator satellite, its wake word and ring |
| `internal/esphome/` | The ESPHome native API server and entities (Switch, Select, Light, MediaPlayer, …) |
| `internal/owned/` | Settings the Echo owns |
| `internal/mediaplayer/` | HA's media player: WAV from HA's proxy, announcements on the voice plane |
| `internal/server/`; `internal/config/` | Mute/volume/LED state; runtime config (env defaults, the kept config, the push) |
| `internal/bindings/` | Hardware drivers; each opens what `pkg/board` resolved, never a number |
| `pkg/board/` | Board identity, parts BY NAME, `Resolve`. `guard_test.go` fails on hardware numbers anywhere else |
| `internal/outchain/` | EQ → bass guard → limiter at the ALSA write, bit-exact to `testdata/gen_vectors.py` |
| `internal/sendspin/` | Sendspin player (#89); re-run `tools/sendspin_interop/run.sh` before trusting an MA upgrade |
| `internal/bluetooth/` | Passive HCI scan for the ESPHome proxy, its emission gate and yields |
| `internal/wifi/` | WiFi change with rollback. **SSIDs are 0–32 BYTES**; never in a shell command |
| `internal/opus/` | libopus 1.5.2 decoder, vendored byte-identical |
| `pkg/led/`, `pkg/mic/`, `pkg/speaker/`, `pkg/buttons/` | Hardware interfaces |

## Boards: hardware is found by name (#541, 2026-10-04)

EchoMuse's `docs/boards.md` is the porter's guide; these must stay true. Parts are named
in `pkg/board/hardware.go`, found once by `board.Resolve`.

- **Only biscuit has a numeric FALLBACK; a new board never gets one.**
- **An unidentified device gets biscuit's layout incl. gpio444**, so ADD a
  board before running firmware on it.
- **Ambiguous is refused, never first-match. Inputs match by NAME.**
- **Board differences are data in `Hardware`, never a branch in a driver**
  (`TestNothingBranchesOnBoardIdentity`).
- **`server board`** prints the layout and changes nothing.
- **Start-up is amp, DAC, then mute released once RUNNING.**
- **Donut**: **`Loopback` must be open AND delivering before the mics open**,
  or the TAS2770 loses its clock until reboot.

## The external audio jack

- **`Init()` runs `stop media` and waits on the speaker substream before
  opening.** **Never make `stop media` permanent.**
- **The speaker is card 0 device 23, the mic 24.**
- **`SetJackRouting` applies BOTH positions and stays idempotent.**
- **Routing is PHYSICAL**: build no mux/destination layer.
- **Unresolved (#117, #141): a plug degrades the whole audio subsystem.** Read
  #117 first; **do not look at codec, SoC or mixer state again**.
- **MIXER CONTROL IDS ARE NOT STABLE ACROSS KERNELS — resolve by NAME**
  (`internal/bindings/mixer`). **Never `tinyplay` while the server runs.**

## The Bluetooth proxy, and what it costs the Echo

Passive HCI scan, each batch to the Echo's own ESPHome device
(`cmd/bleproxy.go`), on while HA's "Bluetooth proxy" switch is. Passive only:
no connection slots. **Scanning costs its own Echo's WiFi.**

- Settled, do not repeat: the chip ignores scan interval/window; cost is
  proportional to scan time; **do not rebuild the kernel for power save**.
- **`Scanner.Yield`**, polled every 100ms, stops the scan for a satellite
  call, a reply arriving and any shell session; music gets bursts
  (`bluetooth.MusicDuty`).
- **The emission gate (`emit.go`)** targets ~one advert per device per
  second; **a first sighting is NOT an arrival**; never `filter_duplicates=1`.
- Each Echo's own Bluetooth address is loaded every session (#1136).

## CPU topology, thermals and why `cpuPct` lies

- HPS parks idle cores. **`num_base_perf_serv` is raised to 2 every start.**
  **Never write `cpu1/online`.**
- **emOS board tuning**: `server platform-init` applies stock's
  thermal/hotplug values, board identified POSITIVELY.
- **`cpuPct` is a share of ONLINE capacity.**

## Volume / mute persistence

- **Volume is software (`PcmSpeaker.SetVolume`); the DAC sits at unity 127.**
  0.5dB/step law; level 0 silent.
- **Never above codec unity.** Buttons step over `volumeButtonFloor` (57)…127.
- The volume is kept in the kept config and restored at boot; HA's media
  player and Sendspin percentages map onto the same grid (`PercentToLevel`).
- **Mute is device-sovereign** in `state.json`, restored pre-connect, the
  write synced. Software Mute is independent of the button and cleared by
  any press.

## LED ring

The satellite paints its own states; idle is the LED Ring light (`cmd/ledring.go`,
`ledring.json`), repainted after anything that clears the ring. With a
controller, `led_anim` scenes animate locally (`internal/server/animator.go`);
a controller "off" is idle while the satellite runs. Mute red and the volume
arc are device-local. The link-state rings (orange pulse no controller,
orange alternating refused, white pending) still show while a controller is
expected.

## Where the serial comes from

`GetSerialNo` (and emOS `init.c`, same order): **`/proc/idme/serial`**, then
`getprop ro.serialno` (FireOS only), then `androidboot.serialno` on the
cmdline. **Non-printable ASCII is rejected.**

## The emOS console password

`consolePassword` is written to `/data/local/etc/echomuse/console.pw`
(`config.WriteConsolePassword`), never checked; emOS's init enforces it.
**A POINTER**; **written only when the content changes**; **temp file +
rename**.

## The DHCP hostname (`internal/dhcpname`, `cmd/dhcpname.go`)

emOS's init starts busybox udhcpc by path with no hostname and restarts it
within ~5 s when it exits. The firmware replaces the `/sbin/udhcpc` symlink
on the RAM-disk root with a wrapper adding `-x hostname:<dhcpHostname>`
(the controller's `em_labels.hostname`, kept in the kept config) and stops
the client so init restarts it through the wrapper. **The wrapper is run
before it goes in, only a DNS label is ever written into it, and if no
client comes back through it in 30 s the symlink is restored**: a DHCP
client that never returns takes the Echo offline at its lease's end.
Redone at each start; boards on Amazon's `dhcpcd` are left alone.

## Files the firmware keeps on the Echo

All in `/data/local/etc/echomuse/` (a path emOS shares: never rename it):
`config.json` (kept config), `settings.json` (owned settings), `state.json`
(mute), `incubator.json` (standalone satellite), `ledring.json`,
`esphome.key`, `esphome.mac`, `sendspin.json`, `controller.json`, `ca.pem`, `token`,
`console.pw`, `console.timeout`, `wifi_pending`, `aec_echo_path.v3.bin`.

## cgo dependency

SpeexDSP C source (AEC) is vendored in `device/internal/aec/`. **Go's build
cache does not see C files `#include`d from a cgo preamble**: after editing
one, test with `go test -a -count=1`. `internal/bindings/mixer` links the
device's own `libtinyalsa.so`: **call only functions both devices' library
export** (`llvm-nm -D`).
