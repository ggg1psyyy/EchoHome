# EchoHome Documentation

| Document | What it covers |
|---|---|
| [Quickstart](quickstart.md) | From an unlocked Echo to talking to it: the setup tool, the wizard, Home Assistant and the Voice Incubator app. |
| [Voice Incubator](voice-incubator.md) | The Echo as the app's satellite: what works, privacy, and what to check when it does not answer. |
| [Configuration](configuration.md) | The Echo's own settings in Home Assistant, the dashboard's, and the setup tool's options. Ends with [what leaves your network](configuration.md#what-leaves-your-network). |
| [Listening and privacy](listening.md) | Exactly when audio leaves an Echo, and what each status means. Also the specification the code is held to. |
| [FAQ](faq.md) | Rooting refusals, wizard failures, emOS, updates and audio. |
| [Acceptance testing](uat.md) | The checks to run on hardware, with and without the setup tool. |
| [Moving to the Home Assistant add-on](migrate-to-addon.md) | Moving a Docker install to the add-on without losing devices or settings. Read the part about `tls/` first. |

Deeper references:

- [rooting.md](rooting.md) — unlocking with firebreak, and what the wizard
  does after it. The exploit is R0rt1z2's work on XDA Forums.
- [support-bundle.md](support-bundle.md) — what a support bundle contains
  and excludes.
- [device-controller-interface.md](device-controller-interface.md) — the
  setup link between the firmware and the setup tool: planes, capabilities,
  messages, config push, link auth.
- [config-keys.md](config-keys.md) — every pushed key, generated from the
  code.
- [boards.md](boards.md) — how the firmware finds each part by name, and how
  to describe a new board.
- [CLAUDE.md](../CLAUDE.md) — codebase orientation for developers (and AI
  assistants); [history/](history/) holds the reasons behind its rules.

Kept from EchoMuse for their hardware facts, with a banner saying what no
longer applies: [audio-states.md](audio-states.md),
[led-ring-states.md](led-ring-states.md), [SETUP.md](../SETUP.md), and the
engineering journal [JOURNAL.md](../JOURNAL.md) (to 2026-10-25).
[archived/](archived/) holds documents that are no longer current.
