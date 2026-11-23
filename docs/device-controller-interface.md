# The Device ↔ Controller Interface

This is the contract between the firmware on an Echo and the EchoHome setup
tool (the controller). Since EchoHome the Echo does its own voice work: it is
a Voice Incubator satellite and an ESPHome device of its own, and it works
with no controller. The link described here carries setup: registration,
configuration, state reports, WiFi changes, the shell that OTA rides, and the
controller's request that the Echo run its satellite.

The authoritative source for every message and field is the code cited
inline; where this document and the code disagree, the code wins and this
document is the bug. Any firmware can meet any controller, including firmware
from before EchoHome (`controller/tests/test_capabilities.py`).

## The two rules everything else serves

- **Negotiate by capability, not version.** The device announces what it
  implements in its `register` message; the controller reads
  `Device.capabilities` and gates each feature on the relevant string. Never
  compare version strings. A UI control whose capability the device lacks is
  shown **disabled with the reason**, never as a control that silently does
  nothing.
- **Degrade to old behaviour, never to a wrong answer.** Unknown JSON fields
  and message types are ignored in both directions. Where a field records a
  measurement, absence stores as **NULL, not 0**.

## The three planes

The controller is discovered by mDNS (`_emcontroller._tcp.local`, the name
every firmware looks for); the device dials out.

| Path | Payload | Purpose |
|------|---------|---------|
| `/control` | JSON text | Registration, config, state reports, WiFi, LEDs, shell control |
| `/data` | — | Idle. The device dials it and sends its identify line; nothing else flows either way. Firmware from before EchoHome sends audio and BLE frames on it, which the controller drops |
| `/shell/{device_id}` | raw binary | Root shell proxy, dialled only after `shell_open`; OTA transfers ride it |

All three exist in plain (`ws://`) and TLS (`wss://`) form; see
[Link auth & TLS](#link-auth--tls).

**Keepalive and loss.** The controller pings every 20s on each plane and
closes a connection after **30s** with no pong (`WS_PING_TIMEOUT_S`); never
block the goroutine that reads the socket. A device should set
`TCP_THIN_LINEAR_TIMEOUTS` on its sockets; the controller does on its end.
Losing the link changes nothing an Echo does for its owner.

## Registration and capabilities

Immediately after the `/control` socket opens, the device sends one `register`
message (`device/internal/client/control.go`):

```json
{
  "type": "register",
  "device_id": "<stable id>",
  "version": "<firmware version, from build ldflags>",
  "capabilities": ["speaker", "leds", ...],
  "ip": "<local ip, omitted if 127.0.0.1 or unresolved>",
  "ambient_light_status": { "...": "..." },
  "base_os": "emos | fireos | unknown",
  "pairing": true,  // only during a pairing window (capability `pairing`)
  "board": "<pkg/board id, or unknown>",
  "kernel_arch": "<uname -m, e.g. aarch64>",
  "kernel_release": "<uname -r, e.g. 3.18.19+>",
  "boot_id": "<the kernel's /proc/sys/kernel/random/boot_id>",
  "boot_reason": "<androidboot.bootreason, e.g. power_key, wdt_by_pass_pwk>",
  "emmc": { "rev": 7, "preEol": 1, "lifeA": 1, "lifeB": 2,
            "name": "FJ25AB", "date": "08/2017", "manfid": "0x000015" }
}
```

`base_os`, `board` and the two `kernel_*` fields describe the boot: the
controller stores and displays them, gates Android-only payloads on
`base_os`, and names the Echo's model from `board` (`em_boards`). The kernel
pair is omitted if `uname` fails.

`boot_id`, `boot_reason` and `emmc` are boot-time health, each omitted when it
cannot be read. The controller keeps one row per `boot_id` for the reason and
one row per day for the wear, which the device also sends on the `stats` tick.
`emmc` carries the eMMC's EXT_CSD bytes (JEDEC JESD84-B51): `rev` is
EXT_CSD_REV [192], and below 7 the other three mean nothing; `preEol` is
PRE_EOL_INFO [267] (1 normal, 2 warning, 3 urgent); `lifeA`/`lifeB` are
DEVICE_LIFE_TIME_EST_TYP_A/B [268]/[269] (1–10 in 10% steps, 11 past rated
life). Find the eMMC by type (`/sys/bus/mmc/devices/*/type` = `MMC`), not by
number.

### Capabilities

`capabilities()` in `control.go`:

| Capability | Condition | Meaning |
|------------|-----------|---------|
| `speaker`, `leds`, `buttons` | always | The hardware is driven by this firmware |
| `led_anim` | always | Renders animations locally from one `led_anim` spec per scene change |
| `aec_hw_ref` | always | Takes the AEC reference from a playback loopback where the board has one (`aecRef` on `stats` says which: `hw`, `sw`, `off`; absent from firmware too old to say, which must not read as `sw`) |
| `oww_local_only` | always | Listens privately; what it is doing is `listen_state` ([listening.md](listening.md)) |
| `output_chain` | always | Runs the output chain (EQ → bass guard → limiter) itself. EchoHome firmware runs it always; the capability stays for controllers that hand it over |
| `stock_sound` | where the board has a voicing | Voices its own speaker (volume law, driver correction, compressor bands), the bass guard held on |
| `volume_cue` | always | Plays a preview tone after a button volume change (`volumeButtonSound`) |
| `remote_volume_arc` | always | Can show the level arc for remote volume changes; the setting is the Echo's own (HA switch) |
| `mute_ring_off` | where the mute button lights itself | Can leave the ring dark while muted; the setting is the Echo's own (HA switch) |
| `response_level` | always | Boosts the voice plane by `responseLevel` (0 / +6 / +12 dB), capped at unity with the volume |
| `wake_mic` | always | Hears on the microphone `wakeMic` names (0 the board's default, 1..N an edge mic); the Echo's own select in HA, which a push only seeds |
| `identify_ring` | always | Takes the whole ring for 30s on `identify` |
| `pairing` | always | Asks to pair when its owner holds the action button 7s with no controller link or one over plain |
| `sendspin` | always | Can be a Sendspin player for Music Assistant (port 8928, `_sendspin._tcp`); nothing of it crosses the controller |
| `sendspin_owned` | always | Owns `sendspinEnabled`, also an HA switch on its own ESPHome device (refused there until paired), and plays with no controller. A pushed value counts when it differs from the last one pushed: the last change of the two wins. Pairing, the token and `sendspinUnpaired` stay the controller's |
| `esphome_server` | always | Serves the ESPHome native API on 6053 from boot, always: Home Assistant connects to the Echo directly. Messages generated from aioesphomeapi's descriptors (`tools/esphome_gen`), proven against it (`tools/esphome_interop`) |
| `esphome_noise` | always | That server is encrypted (Noise, NNpsk0_25519_ChaChaPoly_SHA256) with a key Home Assistant sets; handed out only on `esphome_key_request`. It also takes Home Assistant's time (`GetTimeRequest`), so the Echo has a clock with no controller |
| `incubator_satellite` | always | Is a Voice Incubator satellite (`internal/incubator`, protocol 3) on TCP 6055, run while `incubator_satellite` enables it |
| `incubator_standalone` | always | That satellite outlives the controller: saved on the Echo (`incubator.json`) and started from it at boot |
| `incubator_mdns` | always | The satellite announces itself (`_voice-incubator._tcp`) while it listens; the controller announces it as well, at the Echo's address, for networks where mDNS does not cross VLANs |
| `mic` | with a readable microphone | The board's capture works. Without it the Echo is speaker-only and `listen_state` is `degraded` |
| `incubator_calls` | with `mic` | Takes calls the action button starts, decided on the Echo in the reference firmware's order |
| `incubator_wake` | with `mic` | Runs the wake word models the app pushes (`mixednet`, 4 slots, 1.5s pre-roll); nothing leaves the Echo until a detection |
| `incubator_ring` | with `mic` | Shows the app's state on the ring, with the specs pushed as `incubatorRing` |
| `incubator_monitor` | with `mic` | Lets the app listen live outside a call (its Test page) while HA's "live listening" switch is on; reported as `monitored` |
| `ambient_light` | only if the sensor is readable | Reports light readings |
| `thermal` | only if the kernel exposes a thermal zone | `cpuTempC` and `maxTempC` on `stats` |

Retired with the controller's voice path, and announced only by firmware from
before EchoHome: `oww_shadow`, `oww_trigger`, `button_hold`, `audio_mix`,
`wake_cue`, `wake_word_off`, `ble_connect`. The controller reads none of them
any more.

The controller reads capabilities through `@property` gates on `Device`
(`controller/em_controller.py`) and plain membership checks. Every string it
checks must be one the device can send, and the reverse:
`test_capabilities.py` pins both directions, because a typo on either side
fails silently.

## `/control` — JSON messages

Dispatched on the device by the type switch in `control.go`. Unknown types are
ignored.

**Device → Controller**

| `type` | Payload | Meaning |
|--------|---------|---------|
| `register` | see above | Handshake, sent once at connect |
| `listen_state` | `state` (`local`/`degraded`; `stream` from older firmware), `reason?` | What the device does with its microphone, for the privacy line (`em_listen.resolve`). Sent after every `ack` and on change |
| `incubator_state` | `listening`, `port`, `appConnected`, `inCall`, `wakeModels`, `monitored`, `intercom`, `error`, `mac` | The satellite's state as it changes, and in answer to every `incubator_satellite`. `monitored`: the microphone is streaming to the app outside a call now; the dashboard's privacy line follows it. `intercom`: `ringing` (called, waiting for a tap on the Echo) or `connected` (answered there; `monitored` too); absent otherwise. `mac`: the one the satellite announces (the Echo's WiFi chip's, as its ESPHome device reports), which the controller's record then carries; absent from older firmware, which keeps the stored one |
| `mute_state` | `muted` | The button's mute or Software Mute changed (mute is the device's own) |
| `volume_state` | `level` | Volume changed |
| `ambient_light` | `lux` | Light reading on a real change (only with `ambient_light`) |
| `stats` | `internal/client/stats.go`, `DeviceStats` | Every ~30s and once on connect. Every field optional; absence means **not measured**, never zero |
| `wifi_scan_result` | `networks[]` of `{ssid, ssid_hex, signal}`, or `error` | Answer to `wifi_scan` |
| `wifi_result` | `ok`, `ssid`, `error?` | Outcome of a `wifi_change`, re-sent until `wifi_commit` |
| `pair_request` | — | The owner held the action button on a connected device (only with `pairing`). The controller shows **Approve pairing** |
| `sendspin_status` | `status`, or null when the player stopped | The Sendspin player's state as it changes. No secrets |
| `esphome_key` | `key`, or `error` | Answer to `esphome_key_request`; handed to the waiting request, never logged or stored |
| `sendspin_token` | `token`, `clientId`, or `error` | Answer to `sendspin_token_request`; never logged or stored |
| `esphome_mdns` | `name`, `port`, `txt[]` | The ESPHome record the Echo announces itself (`_esphomelib._tcp`): node name, API port, TXT strings. The controller announces the same record at the Echo's address on its own network while the Echo is connected (`em_esphome_mdns`), for a Home Assistant the Echo's multicast does not reach; a record outside RFC 1035/6763's limits or without `mac` is refused. Sent on connect and when the record changes |
| `log` | `level`, `message` | A device log line for the dashboard and support bundles |
| `pong` | `id`, `mono` when answering a `ping` that carried an `id` | Keepalive reply and RTT sample |

Firmware from before EchoHome also sends `button`, `oww_wake`,
`oww_shadow_cross`, `listen_end` and `playback_stats` (the Home Assistant
voice mode), and early EchoHome firmware `settings_changed`. The controller
ignores them (logged at debug).

**Controller → Device**

| `type` | Payload | Meaning |
|--------|---------|---------|
| `ack` | `device_id`, `features[]`, `time_ms` | Registration accepted. `features` is the controller's capability list: `ble_adverts_data` and `listen_session`, both read only by firmware from before EchoHome (adverts on the data plane, where they are dropped; private listening rather than streaming). `time_ms` sets the device clock, stepped only past 30s |
| `refused` | — | Link auth refused this device's credentials. Firmware with `pairing` shows the refused ring |
| `pending` | `pairing?` | Unapproved, or a pairing request waiting for an admin. Keep redialling |
| `config` | `ConfigMessage` fields | Push configuration (below) |
| `incubator_satellite` | `enabled`, `name`, `mac`, `port`, `monitor`, `standalone` | Run the satellite under the identity the app knows the Echo by, on `port` (outside 1-65535: 6055). Sent to firmware announcing `incubator_satellite`, on connect and on change; an identical message keeps the running satellite and its app. EchoHome sends `monitor` false (live listening is the Echo's own switch) and `standalone` true wherever the firmware announces `incubator_standalone` |
| `led_anim` / `leds` | `{pattern, colors, periodMs, ttlSec}` / `leds[]` | A scene's animation spec (only with `led_anim`); a frame, used to clear the ring |
| `identify` | — | Take the whole ring for 30s (only with `identify_ring`); a second one extends it |
| `wifi_scan` | — | Scan; answered with `wifi_scan_result` |
| `wifi_change` / `wifi_commit` | `ssid`, `ssid_hex?`, `psk` / — | Switch WiFi with automatic rollback; commit finalises |
| `shell_open` / `shell_close` | `pty?` | Dial `/shell` (`pty:true` interactive) / close it |
| `esphome_key_request` | — | Ask for the ESPHome key, for the dashboard on an admin's request |
| `sendspin_token_request` | — | Ask for the Sendspin pairing token, likewise |
| `ping` | `id?` | Keepalive; answered with `pong` |

The firmware still handles `volume_set`, `music_flush`, `speaker_flush` and
`incubator_press` from controllers before EchoHome, which sent them; an
EchoHome controller sends none.

**An SSID is 0–32 arbitrary bytes**, so a name alone cannot always address
one. `ssid` is for display (invalid UTF-8 shown as U+FFFD); `ssid_hex` is the
exact bytes, reported in each scan result and sent back in `wifi_change` when
the network came from a scan. A `wifi_change` without it means the UTF-8 of
`ssid`. `psk` is empty for an open network, 8–63 printable ASCII characters,
or a raw 64-hex PSK.

## Config push — `ConfigMessage`

The controller sends `config` on connect and on any change. Fields are
camelCase (`device/internal/config/config.go`). **Partial update:
non-zero/non-nil fields are applied; zero/nil are ignored**, so pointer types
exist where `false`/`0` must be distinguishable from unset. Changes take
effect at once.

Every key, its section, default, type, whether the Echo applies it and which
modules read it: [config-keys.md](config-keys.md), generated by
`controller/tools/config_map.py`. Keys retired with the voice path or now
owned by the Echo (`em_config_types.RETIRED`) stay in stored configs and are
**never pushed**. The settings Home Assistant changes on the Echo are kept
there (`settings.json`); a push only seeds one the Echo has no value of. The
Echo saves the last push (`config.json`) and applies it at boot, before any
controller.

## Link auth & TLS

- All three planes carry an `X-EM-Token` header, read from the device's
  credential file on **every dial** (`device/internal/client/tlscreds.go`), so
  a pushed credential takes effect on the next reconnect without a restart.
  A device holding a CA **never** sends its token on a plain dial.
- A device with a CA on disk dials **only** `wss://`, at the `tls_port` from
  mDNS TXT or its endpoint file; with no `tls_port` it does not dial at all.
  The one exception is a pairing window: wss first, then plain without the
  token, both registering with `"pairing": true`. A device with no CA dials
  plain. The server identity is the fixed DNS SAN `echomuse-controller`,
  never an IP.
- Certs are backdated and long-lived **and** the device clamps its
  verification clock to the firmware build time, because an Echo boots with a
  bogus clock and a device that cannot connect cannot fix its clock. Do not
  "normalise" either half.
- Enforcement is `em_linkauth.decide`: a wrong token always rejects; a stored
  token with none presented is allowed only until the device has presented it
  once, and refused after that; a token for a device with nothing on record
  is ignored. `REQUIRE_DEVICE_TLS=1` makes TLS+token mandatory. **A device
  must send its token on all three planes.**
- `/data` and `/shell` are admitted only from the address, and with the
  scheme, of the device's live `/control` connection
  (`em_linkauth.follows_control`).

## What the device binary owns

Everything an Echo does for its owner, with or without a controller:

- **Voice**: the Voice Incubator satellite (wake word, calls, the app's
  replies) and the microphone path that feeds it (`device/CLAUDE.md`).
- **Home Assistant**: its own ESPHome device (media player, LED ring light,
  mute, action button, sensors, the Bluetooth proxy, its settings).
- **Playback**: the output chain, volume, Sendspin.
- **Buttons and the ring**, resolved by device **name**, never by number.

It does not depend on Amazon HALs or libraries to do any of it (the direction
note in the repo-root `CLAUDE.md`). A new board is a fresh set of bindings
behind `pkg/board`; see [EchoMuse's board guide](https://github.com/wilbowes/EchoMuse/blob/main/docs/boards.md).

## Invariants (what the tests pin)

`controller/tests/test_capabilities.py` is the guard rail for this contract:

- The device announces the expected baseline capabilities.
- Every capability string the controller checks is one the device can send,
  and vice versa.
- A disabled UI control does not silently write when its capability is absent.
- Private listening is negotiated both ways for firmware from before
  EchoHome: `oww_local_only` from the device, `listen_session` on the ack.

The privacy line's states are pinned by `controller/tests/test_listen.py`;
the push's retired keys by `controller/tests/test_config_types.py`.

A board or protocol change that breaks one of these must update the test with
the reason, not route around it.
