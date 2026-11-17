# Using the Voice Incubator instead of Home Assistant's voice pipeline

> **Experimental.** [Voice Incubator](https://github.com/OHF-Voice/voice-incubator)
> itself is marked "experimental and not supported" by its authors, and its
> satellite protocol may change. Discussion: #1207.

An Echo can be answered by OHF-Voice's **Voice Incubator** app instead of
Home Assistant's own voice pipeline. You choose this **per Echo**; Home
Assistant stays the default, and you can switch back at any time.

The Echo keeps doing what it does today — its wake word, its ring, its
buttons, barge-in, music — and the Incubator app does the listening and
answering: it recognises commands in the audio itself and runs its own
timers, alarms and announcements.

## Before you start

- **The Voice Incubator app**, installed as a Home Assistant app (add-on) on
  the **same machine** as EchoMuse, together with its integration (HACS). See
  its [install steps](https://github.com/OHF-Voice/voice-incubator#install).
  It needs Home Assistant 2025.10 or later.
- The app finds Echoes the way it finds its own satellites, over mDNS. With
  both apps on one host that stays on the host, so it works even where mDNS
  does not cross your VLANs. Running the app on another machine needs mDNS
  between the two.
- Nothing changes on the Echo: no firmware update, no reflash.

## Switching an Echo over

1. Dashboard → the Echo → **Config** → **05 — Advanced**. To switch only
   this Echo, set the section's switch to **Device** first; on **Fleet** the
   section shows the fleet's values read-only. Then **Voice assistant** →
   **Voice Incubator (experimental)**, and save. (Or set it in the fleet
   settings to switch every Echo that follows the fleet's Advanced section.)

   With Advanced on **Device**, this Echo also keeps its own copy of the rest
   of that section — button tap settings, speech gate, and the emOS console
   password and idle timeout — and stops following fleet changes to them
   (see [Where settings live](configuration.md#where-settings-live)).

   > **If you use an emOS console password**, an Echo whose Advanced
   > section is set to Device can be left with none. Switching the section
   > from the dashboard does not carry the fleet's password across, and a
   > password typed into that Echo's own Advanced section is not stored in
   > the form the Echo checks. This is true of any Advanced section set to
   > Device, not only for this setting. Setting **Voice assistant** in the
   > fleet settings instead avoids it, but switches every Echo that follows
   > the fleet's Advanced section.
2. Within a few seconds the Voice Incubator app shows the Echo as a
   satellite, and Home Assistant links it to the Echo's existing ESPHome
   device as a separate *Voice Incubator* device.
3. The Echo's **Status** tab shows **Voice assistant: Voice Incubator
   connected**.

Home Assistant reconnects to the Echo once when you switch, and its device
page loses the **Assist satellite** and **wake word** entities. The media
player, the action button, the light and mute sensors and the Bluetooth proxy
stay as they were.

**Switching back** is the same setting set to **Home Assistant**: the Assist
entities come back and Home Assistant answers again. If you disabled the
Assist satellite entity by hand at some point, enable it again — a disabled
one answers nothing.

## What works

- Wake word, then a question or a command; follow-up questions the app asks.
- Speaking straight after the wake word, without a pause — though the app
  starts the command where the Echo *detected* the wake word (it cannot
  re-check an Echo's wake word, below), so the first syllable of a command
  said very quickly can be lost.
- **Barge-in**: the wake word over a reply cuts it off and starts a new
  question (turn barge-in off in **02 — Wake word** to have the reply play
  to its end).
- The action button: a tap asks a question, a tap during one cancels it.
- The app's **timers and alarms** ring on the Echo that set them; the wake
  word or a tap stops them.
- The app's **announcements**, to and from other Echoes using it.
- Music keeps playing, ducked under replies, rings and announcements.

## What is different

- **Which wake word.** The Echo listens for the wake word set in EchoMuse
  (**02 — Wake word**), as before. The app's own *Wake words* page (Okay
  Nabu and the rest) does not apply to Echoes: the app cannot run its models
  on an Echo, and accepts the Echo's detection as it is.
- **Replies sound slightly duller.** The app's protocol carries 16 kHz audio,
  so replies stop at about 7 kHz, where Home Assistant's replies reach the
  Echo at full bandwidth. Music is unaffected.
- **Settings that do nothing on an Incubator Echo**, because what they
  control is part of Home Assistant's pipeline: *Noise suppression* (03 —
  Microphones), *Save utterances*, *Speak while the reply is written*. The
  Activity tab records each turn, but not what was said — the app keeps that
  in its own Sessions page.
- **Turning the wake word off** is done in Home Assistant's wake word
  picker, which an Incubator Echo no longer has. An Echo whose wake word was
  switched off there stays off; switch it back on before moving it over.
- **The app does not re-check the wake word.** It double-checks its own
  satellites' wake words against what was actually said; an Echo sends too
  little audio from before the wake word for that, so the app accepts the
  Echo's detection as it is.
- **Speaker identification**, as the app's code reads, falls back to the
  one capture an Echo sends (its own satellite sends a second,
  echo-cancelled one) and uses voices enrolled elsewhere. It has not been
  tried on an Echo.
- **The ring** shows listening, thinking and the reply as usual. The app's
  timers ring by sound only: the ring does not light for them, nor show the
  app's running-timer progress or night mode.
- **Not supported**: the app's live microphone test and intercom, enrolling
  a voice from an Echo, and recording wake word samples on one. All of them
  stream the microphone outside a question, which an Echo does not do
  ([Listening and privacy](listening.md)).

## Privacy

Nothing about what leaves the Echo changes: it detects its own wake word and
sends audio only from just before it until you stop speaking
([Listening and privacy](listening.md)). That audio goes from the controller
to the Voice Incubator app on your own machine instead of to Home Assistant.
What the app keeps, and for how long, is the app's — see its documentation.

The satellite port (the Echo's voice port + 2000) has no authentication, as
the app's own satellites have none: whichever client connects first receives
the Echo's questions. Keep it reachable only from where the app runs.

## When it doesn't answer

- **Status tab, Voice assistant row.** *Voice Incubator connected* (green)
  is what you want. *Waiting for the Voice Incubator app* (amber): the app
  is not connected to this Echo — check that it is running. *Not listening*:
  the Echo is offline.
- **Without the app the Echo has no voice assistant.** A wake gives the
  short orange "no assistant" cue and nothing answers. It never falls back
  to Home Assistant on its own.
- A support bundle (**Settings → Support**) records whether the app was
  connected; the project's `bundle_check` tool reports *"the Voice Incubator
  app is not connected"* from it.
- **Very long answers** (more than about half a minute) make the app log
  *"Timed out waiting for playback to drain"*. The answer still plays to its
  end; the message is the app giving up waiting for the Echo to finish.

## Tested on

On hardware: two Echo Dot 2 (biscuit) on emOS with firmware v2.19.0, Voice
Incubator 0.3.1 as a Home Assistant app on the same host as EchoMuse, Home
Assistant 2026.11.1, both Echoes detecting their wake word on the device
(the default). Wake word calls, barge-in, timers (stopped by the wake word
and by the button), announcements between the two Echoes, music, switching
backends back and forth, and the app being stopped. On one of the two:
asking with the action button, and a follow-up question the app asked (which
of two timers).

Against the real controller with a simulated Echo and app only (the
project's scenario tests), not yet on hardware: a tap cancelling a call
(while the app listens and during its reply).

Not tested: speaker identification on an Echo, several Incubator Echoes
hearing the same wake word (EchoMuse's arbitration should still pick one),
detecting the wake word on the controller (*Wake word detection: On the
controller*), and the app running on another machine.
