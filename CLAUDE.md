# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

It holds the rules. The why behind them (incidents, measurements, worked
examples, dates) is in `docs/history/root.md` under the same headings; read
the matching section there before changing that area or a rule.

**Before diagnosing a fault, reopening a decision or changing a default, grep
`docs/history/` and `JOURNAL.md` for the symptom, setting or function name.**
The stories behind the rules are no longer loaded; this is how they are found.
JOURNAL.md is an archive to 2026-10-25, from EchoMuse. **A finding that
produces no commit** (a measurement, a cause ruled out, a hardware fact)
**goes in `docs/history/<area>.md` as a dated bullet under the matching
heading.**

## What this is

EchoHome (`ggg1psyyy/EchoHome`) is a fork of EchoMuse (`wilbowes/EchoMuse`)
that makes a rooted Amazon Echo behave like a Home Assistant Voice PE on the
Voice Incubator firmware:

- **`device/`** — the Go binary on the Echo (the "firmware"; strictly an
  application, started by emOS's or FireOS's init through
  `/data/local/bin/start_server.sh`). It is the Echo's **Voice Incubator
  satellite** (`internal/incubator`, wake word on the Echo,
  `internal/mixednet`) and its **own ESPHome device** (`internal/esphome`:
  media player, LED ring light, mute, action button, ambient light,
  temperatures, the passive Bluetooth proxy, and its settings as switches and
  a select). It works with no controller at all.
- **`controller/`** — the **setup tool**: provisioning (the wizard), pairing,
  configuration, firmware OTA and the emOS update, support bundles, the
  dashboard. It runs as a Home Assistant add-on or a container and can be
  stopped once the Echoes are set up. It has no voice path: it hears nothing
  of the room and sends no audio.
- **`emos/`** — emOS's init, from upstream. EchoHome does not change or
  release emOS: the setup tool installs EchoMuse's emOS releases.

Removed in EchoHome (2026-11-18), recorded in `docs/history/` and the commit
log: the controller's Home Assistant voice mode, its wake word and
arbitration, the emulated ESPHome satellites and BLE proxy, the wake word
asset sync, `oww_forge`, the Activity tab, and on the Echo the old voice
mode's streaming, scorer, speech gate and Bluetooth connections.

## Standing decisions (the owner's)

- **emOS stays upstream and its paths on the Echo are fixed.** Its init reads
  `/data/local/etc/echomuse/` (`console.pw`, the TLS files) and starts
  `/data/local/bin/start_server.sh`; renaming either strands every Echo. The
  same holds for `_emcontroller._tcp` (older firmware finds the controller by
  it), the add-on slugs, the `em_` module names and the Go module path. The
  rename to EchoHome is of what a person reads.
- **Releases**: firmware from EchoHome (`firmware_repo`, default
  `ggg1psyyy/EchoHome`), emOS from EchoMuse (`emos_repo`, default
  `wilbowes/EchoMuse`). A cached release counts only for the repository it
  came from (`em_releases`); a binary cache entry is keyed by tag and
  repository.
- **The Voice Incubator app's mDNS limit across VLANs** is answered by the
  controller's mDNS record for each Echo (`em_incubator`) until the app can
  be told the satellite's address. That change goes to OHF-Voice from the
  owner's fork (`ggg1psyyy/voice-incubator`), under OHF's AI policy: **no
  assistant opens or comments on an upstream PR or issue; the owner writes
  the description and replies in their own words.** Prepare code and notes;
  never post upstream.
- Echoes are named by the last four characters of their serial in anything
  committed or published (00LQ, 0SE7). The pre-commit check greps the diff
  AND the message for names, addresses and serials.

## Direction: portable, and not dependent on Amazon

**EchoHome should run on more than one piece of hardware, with minimal change
per platform, and should not depend on Amazon's software to work.** Judge a
change by that. The Android surface in `device/` is about twenty call sites
(`tinymix`, `stop <service>`, `svc wifi`, `getprop`); the rest is ALSA, i2c,
evdev, sysfs and wpa_supplicant.

- **Prefer the Linux interface to the Android one**; isolate an unavoidable
  Android call rather than spread it.
- **Resolve hardware by NAME, not by number** — the wrong `event*` opens
  silently and leaves the buttons dead. Boards state their parts in
  `pkg/board` (`device/CLAUDE.md` → "Boards", `docs/boards.md`).
- **A change that makes a vendor blob necessary is going the wrong way**:
  at most a terminal opt-in for one platform. **Decline the direction, keep
  the findings** (PR #168).
- No Android build (LineageOS included) is the target: same vendor blobs.

Keep `pkg/led`, `pkg/mic`, `pkg/speaker` and `pkg/buttons` honest as
interfaces.

## Where a standard exists, conform to it and prove it

**If what we are handling has a known standard or spec, abide by the spec, and
have a test that proves it**, written from the spec's edges (min/max lengths,
every permitted character class, forbidden forms, published test vectors) and,
where possible, run against the real implementation. Worked examples: #586,
GitHub's owner/name rules in `tests/test_release_sources.py`, the ESPHome
native API against aioesphomeapi's descriptors (`device/tools/esphome_gen`).

**Never interpolate user-supplied text into a shell command** (serial console,
`adb shell`, `sh -c`). Send hex, base64 or a file.

## Writing to people: bottom line first

Anything a **person** reads leads with the answer and stays short — PR
comments, issue replies, review feedback, release notes. Replies are the
owner's, in their own voice: **no project sign-off.**

- **The first line answers it** — the verdict, the decision, or the ask.
- **Three points, maximum.** Evidence is the *number*, not the derivation.
- **Match the recipient**; cut process narration, restating the question and
  hedging. **Never contrastive negation** ("not X, it's Y").
- **The exception is anything irreversible**, or anything asking someone to
  act on their own hardware — an OTA, rooting, a schema migration, a
  partition write. The caveat stays whatever it costs in length.

None of this applies to commit messages, this file, or code comments: those
are the record, and their density is load-bearing.

## Listening is private, and every claim about it is the Echo's own

**`docs/listening.md` is the spec.** EchoHome firmware sends nothing of the
room to the controller, in any state; the Voice Incubator app hears it only in
a call (after the Echo's own wake word or a press) or while the app's Test
page listens with HA's "live listening" switch on, which the Echo reports
(`incubator_state` `monitored`).

- **Privacy statements come from what the Echo REPORTS** (`listen_state` →
  `em_listen.resolve`, and `monitored`), never from configuration. For
  firmware that can listen privately its report outranks any stored
  `owwOnDevice`. Unknown shows as unknown.
- **Nothing ever falls back to streaming.** `owwOnDevice` stays pushed (no
  control) only so firmware from before EchoHome keeps listening privately.
- **The device enforces its own limits**: mute closes the satellite's
  microphone and nothing remote lifts it.

## Device/controller compatibility

Any device/controller pairing can occur in the field (`tests/test_capabilities.py`),
including firmware from before EchoHome (0SE7 runs v2.19.0 until it is
updated):

- **Negotiate by capability, never by version string.** The register message
  lists them (`internal/client/control.go`, `capabilities()`): `speaker`,
  `leds`, `led_anim`, `buttons`, `aec_hw_ref`, `oww_local_only`,
  `output_chain`, `volume_cue`, `remote_volume_arc`, `response_level`,
  `wake_mic`, `pairing`, `sendspin`, `identify_ring`, `incubator_satellite`,
  `incubator_mdns`, `incubator_standalone`, `esphome_server`, `esphome_noise`;
  with a readable mic `mic`, `incubator_calls`, `incubator_wake`,
  `incubator_ring`, `incubator_monitor`; and `stock_sound`, `ambient_light`,
  `mute_ring_off`, `thermal` where the hardware has them. A UI control whose
  feature is missing is **disabled with the reason**.
- **Both ways**: the `ack` carries the controller's `features`
  (`ble_adverts_data`, `listen_session` — both only for firmware from before
  EchoHome). Adding a message is safe unnegotiated; MOVING one never is.
- **A static boot property rides REGISTER, not the stats tick, and is
  persisted.** `base_os` (schema v21): absence means Android for payload
  gating, but must NOT disable a control.
- The controller sets the device clock via `time_ms` on the `ack`, stepped
  only past 30s; HA's `GetTimeRequest` sets it with no controller.
- **Degrade to old behaviour, never to a wrong answer.** Unknown fields and
  messages are ignored; a missing measurement stores **NULL, not 0**.
- **Before changing what is pushed, ask what the running firmware and the
  oldest fielded one do with it in the gap** — the settings the Echo now owns
  were once overwritten by the old firmware between an add-on update and its
  OTA (2026-11-18). Retired keys (`em_config_types.RETIRED`) are dropped from
  every push.

## Versioning / releases

Three tag namespaces, each `git tag -a --cleanup=verbatim` (the default strip
deletes `#` heading lines from the notes):

- **Firmware** `v*` → `release.yml` → a GitHub Release with a `server` asset;
  OTA compares the embedded tag to `firmware_ver` — don't change the scheme.
  EA is `vX.Y.Z-ea.N`, a prerelease GA controllers skip. EchoHome's line
  starts at 3.0.0, above every EchoMuse release.
- **Controller** `controller-v*` / `controller-ea-v*` →
  `controller-release.yml` → `ghcr.io/ggg1psyyy/echohome-controller`
  (amd64 + arm64). No GitHub Release: the annotation is the only copy of the
  notes. The release workflow **re-tags `:sha-<short>`, it does not build**,
  and refuses a commit whose `Controller Build (main)` failed: merge → green
  build on the merge commit → tag. If the tag raced the build, `gh run rerun
  <id>`; never re-cut the tag.
- **emOS** is EchoMuse's (`emos-v*` there). EchoHome tags none.

`config.yaml`'s `version:` pins the pulled image; the release refuses a
tag/pin mismatch. EA notes go in `controller-ea/CHANGELOG.md`; EA is
generated by `tools/sync_channels.py --set-version`, never edited.

**Pipeline hygiene**: actions pinned by SHA, runners by release, base images
by digest (`tests/test_workflow_pins.py`); workflows default to `permissions:
contents: read`; release workflows refuse a tag that is not a version.

Version resolution: `controller/version.py` (`EM_CONTROLLER_VERSION` → `git
describe --match 'controller-v*'` → `dev`).

## Architecture

### Device → Controller protocol

**The wire contract is `docs/device-controller-interface.md`**. Each device
opens `/control` (JSON), `/data` (idle since EchoHome: the controller sends
and takes no audio; old firmware's frames are dropped) and `/shell/{device_id}`
(on `shell_open`). The controller is found by mDNS (`_emcontroller._tcp.local`).

### Device-link TLS + token auth

Plain on 8767, TLS on 8770. The cert's identity is the DNS SAN
`echomuse-controller`, never an IP. Certs are backdated and the device clamps
its clock to the build time (`BuildUnix`) — don't "normalise" either. The
device re-reads `/data/local/etc/echomuse/{ca.pem,token}` on every dial; with a
CA it dials **wss only**. `em_linkauth.decide`: a wrong token always rejects; a
missing token is allowed only until the device has presented it once. A first
token comes only from a human (wizard over USB, or an approval). **Deleting a
device closes its control plane**, after the row is gone.

### The Echo's own ESPHome device

`device/internal/esphome` serves the native API on 6053 from boot, always,
encrypted with a key Home Assistant sets (`api_encryption_provisionable`).
Entity keys are **append-only** (1-6, 20-28). Settings HA changes there are
**the Echo's own** (`internal/owned`): kept on the Echo, never reported, and a
controller push only seeds a value the Echo has none of.

## Device config push

`config.ConfigMessage` (camelCase JSON) is sent on connect and on change.
Non-zero fields apply; zero/nil are ignored. Every key, its section, default,
type, whether the Echo applies it and which modules read it:
`docs/config-keys.md`, generated by `controller/tools/config_map.py` and held
current by `tests/test_config_map.py`. Regenerate it in the same change as any
key.

## Build and test quickref

```bash
git submodule update --init          # GoTinyAlsa fork — see device/CLAUDE.md
cd device && ./compile.sh            # needs the echomuse-compiler image
cd device && go test ./internal/... ./pkg/...   # cmd/ needs -tags server: vet it in the compiler image
cd controller && python -m pytest tests/   # needs: pytest numpy scipy pyyaml
BABEL_STANDALONE=/tmp/babel.min.js node controller/tests/dashboard_globals.test.mjs
cd emos/init && cc -O2 -o /tmp/ringsim ringsim.c -lm && /tmp/ringsim --check
```

CI runs both suites, `go vet`, a headless-Chromium walk of every dashboard tab
(`uat/`), and `test_design_tokens.py`. CI on a fork starts disabled; EchoHome's
was enabled 2026-11-18.

**A source guard finds code through `tests/_sources.py`** (`MODULES`,
`find_def`, `code_only`), never by reading one file by name.
**`tests/test_device_attrs.py`** fails on an attribute read off a Device that
no Device has — pyflakes cannot see it, and it bit twice in the strip.

**Three ratchets keep the module split**: `tests/test_layering.py`,
`tests/test_size_ratchet.py` (`controller/tools/size_ratchet.py --update`
after a shrink) and `tests/coverage_floor.json` (never lowered; a new module
needs a floor; `em_emos_*` need busybox on the host).

**Accept on hardware with no controller**: after an OTA, stop the add-on,
reboot the Echo (`kill -TERM 1` on emOS, which has no `reboot`), and check the
wake word and a call, the button, both mutes, HA media, the ring and the HA
switches.
