# EchoHome

**Turn an old Amazon Echo into a Home Assistant voice satellite that works
like a Voice PE on the Voice Incubator firmware: wake word on the Echo,
answers from [Voice Incubator](https://github.com/OHF-Voice/voice-incubator),
and an ESPHome device of its own in Home Assistant.**

[![CI](https://github.com/ggg1psyyy/EchoHome/actions/workflows/ci.yml/badge.svg)](https://github.com/ggg1psyyy/EchoHome/actions/workflows/ci.yml)
[![Firmware](https://img.shields.io/github/v/release/ggg1psyyy/EchoHome?include_prereleases&label=firmware)](https://github.com/ggg1psyyy/EchoHome/releases)
[![License: MIT](https://img.shields.io/github/license/ggg1psyyy/EchoHome)](LICENSE)

EchoHome is a fork of [EchoMuse](https://github.com/wilbowes/EchoMuse) by Wil
Bowes, and keeps its unlock and setup path. The Echo runs
[emOS](https://github.com/wilbowes/EchoMuse/tree/main/emos), EchoMuse's small Linux userspace, which EchoHome
installs and updates but does not develop. Where
EchoMuse runs the voice through a controller, EchoHome puts it on the Echo:
the controller becomes a setup tool you can stop once your Echoes are set up.
Not affiliated with or endorsed by Amazon, the Open Home Foundation or the
EchoMuse project.

> **Early Access.** 3.0.0-ea.1 is the first EchoHome release. Voice Incubator
> itself is marked experimental by its authors, and its protocol may change.

## What an Echo does

- **Voice through the Voice Incubator app.** The Echo runs the app's wake
  words itself (Okay Nabu, Hey Jarvis, Hey Mycroft, or your own) and sends
  nothing until one is heard or the action button is pressed. Then the call
  goes straight to the app.
- **An ESPHome device in Home Assistant**, discovered by itself and named as
  on the dashboard: a media
  player (Music Assistant, radio, announcements), the LED ring as a light,
  mute and Software Mute, the action button as an event, ambient light and
  temperatures, a passive Bluetooth proxy, and its settings as switches and a
  select.
- **No controller needed once it is set up.** Its configuration, its
  satellite and its settings are kept on the Echo; Home Assistant sets its
  clock.
- **Sendspin** player for synchronised multi-room audio from Music Assistant.

The setup tool (a Home Assistant add-on or a Docker container) provisions
Echoes over USB, pairs them, pushes configuration, updates firmware and emOS
over WiFi with automatic rollback, and collects support bundles. It hears
nothing of the room.

## Supported hardware

The Echo Dot 2nd gen (biscuit), the Echo 2nd gen (radar) and the Echo Dot 3rd
gen (donut), unlocked first. [EchoMuse's board guide](https://github.com/wilbowes/EchoMuse/blob/main/docs/boards.md) says how a new one is
described.

## Getting started

1. **Unlock the Echo** with [firebreak](https://github.com/bboe/firebreak)
   ([EchoMuse's rooting guide](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md)). **You do this at your own risk**: a
   step gone wrong can soft-brick the Echo, and the XDA threads for
   [the Dot](https://xdaforums.com/t/unlock-root-twrp-unbrick-amazon-echo-dot-2nd-gen-2016-biscuit.4761416/)
   and [the Echo](https://xdaforums.com/t/unlock-root-twrp-unbrick-amazon-echo-2nd-gen-2017-radar.4801290/)
   cover recovering it.
2. **Install the setup tool**: add this repository to Home Assistant and
   install **EchoHome (Early Access)**.

   [![Add the EchoHome repository to Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fggg1psyyy%2FEchoHome)
3. **Run the setup wizard** from its dashboard with the Echo on USB, in
   Chrome or Edge. It installs emOS and the firmware and joins your WiFi.
4. **Add the Echo to Home Assistant**: it appears under Settings → Devices &
   services → **Discovered** as an ESPHome device; press **Add**. Then add it
   **to the Voice Incubator app** as a satellite.

The [quickstart](docs/quickstart.md) walks through all four.

## Privacy

- **The Echo listens for its wake words itself.** Nothing leaves it until one
  fires or you press the button; then the call goes to the Voice Incubator
  app, with 1.5 s of audio from before the wake word so the app can check it,
  and stops when you stop speaking.
- **Live listening** (the app's Test page) happens only while the Echo's
  *Voice Incubator live listening* switch in Home Assistant is on; the ring
  pulses red meanwhile. Off by default.
- **The setup tool hears nothing.** Every statement it makes about listening
  is what the Echo reports. [Listening and privacy](docs/listening.md).
- **No telemetry.** The setup tool connects out only to GitHub, for releases.
- **Mute is a software mute** in the audio chip; the Echo refuses to listen
  while it is on, and nothing remote lifts it.

## Known issues

- Both 3.5 mm jack channels can be silent on emOS (EchoMuse #999); on FireOS
  unplugging can stall the microphone (EchoMuse #117, #141).
- emOS on FireOS 6 cannot join WPA3 networks; use WPA2 (EchoMuse #536).

## Documentation

| | |
|---|---|
| [Quickstart](docs/quickstart.md) | From an unlocked Echo to talking to it. |
| [Rooting](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md) | Unlocking the Echo, and which target to use (EchoMuse's guide). |
| [Voice Incubator](docs/voice-incubator.md) | The satellite: what works, privacy, troubleshooting. |
| [Configuration](docs/configuration.md) | The setup tool's options and the Echo's settings. |
| [Listening and privacy](docs/listening.md) | What the microphone does, in every state. |
| [emOS](https://github.com/wilbowes/EchoMuse/tree/main/emos) | EchoMuse's small Linux userspace, which the Echo runs (in EchoMuse). |
| [Device ↔ controller protocol](docs/device-controller-interface.md) | The setup link, for porting. |
| [Boards](https://github.com/wilbowes/EchoMuse/blob/main/docs/boards.md) | Describing a new board's hardware (EchoMuse's guide). |
| [Contributing](CONTRIBUTING.md) | Building from source and tests. |

## How it's built

EchoHome, like EchoMuse, is written largely with
[Claude](https://www.anthropic.com/claude), Anthropic's AI model, directed by
the maintainer; its commits carry a `Co-Authored-By: Claude` line.

## Credits

- **Wil Bowes and the EchoMuse contributors**, for EchoMuse and emOS, which
  EchoHome is built on.
- **The Open Home Foundation**, for
  [Voice Incubator](https://github.com/OHF-Voice/voice-incubator), Home
  Assistant and ESPHome; **microWakeWord** and **TensorFlow Lite Micro**
  (via esp-micro-speech-features) for the wake word stack.
- **R0rt1z2**, for amonet-biscuit and amonet-radar, the unlocks everything
  depends on; **bboe**, for [firebreak](https://github.com/bboe/firebreak).
- **Binozo**, for [EchoGo](https://github.com/Binozo/EchoGo) and
  [GoTinyAlsa](https://github.com/Binozo/GoTinyAlsa); **Dragon863**, for
  [EchoCLI](https://github.com/Dragon863/EchoCLI).
- **Xiph.Org**, for [SpeexDSP](https://gitlab.xiph.org/xiph/speexdsp) (the
  echo canceller) and Opus.

## Licence

[MIT](LICENSE), as EchoMuse. Third-party components keep their own licences,
listed with their copyright notices in [NOTICE.md](NOTICE.md). emOS, which the
setup tool installs from EchoMuse's releases, carries its own licences
(busybox, GPL-2.0, with its source attached to each EchoMuse release;
wpa_supplicant, BSD): see [EchoMuse](https://github.com/wilbowes/EchoMuse).

Amazon, Echo, Echo Dot, Alexa and FireOS are trademarks of Amazon.com, Inc. or
its affiliates. EchoHome is an independent project.
