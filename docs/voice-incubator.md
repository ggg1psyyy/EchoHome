# Using the Voice Incubator instead of Home Assistant's voice pipeline

> **Experimental.** [Voice Incubator](https://github.com/OHF-Voice/voice-incubator)
> itself is marked "experimental and not supported" by its authors, and its
> satellite protocol may change. Discussion: #1207.

An Echo can be answered by OHF-Voice's **Voice Incubator** app instead of
Home Assistant's own voice pipeline. You choose this **per Echo**; Home
Assistant stays the default, and you can switch back at any time.

There are two ways to do it:

- **Voice Incubator**: the controller is the app's satellite on the Echo's
  behalf. Any firmware. The Echo keeps its own wake word, ring, buttons,
  barge-in and music, and the controller passes each question to the app.
- **Voice Incubator, on the Echo**: the Echo's own firmware is the app's
  satellite, the Echo announces itself on the network (mDNS), and the app
  talks to the Echo directly. It needs firmware newer than v2.19.0 (the
  option is greyed out otherwise), and it does more: the app's own wake
  words, its ring, its questions back, and more (below).

Either way the app does the listening and answering: it recognises commands
in the audio itself and runs its own timers, alarms and announcements.

## Before you start

- **The Voice Incubator app**, installed as a Home Assistant app (add-on) on
  the **same machine** as EchoMuse, together with its integration (HACS). See
  its [install steps](https://github.com/OHF-Voice/voice-incubator#install).
  It needs Home Assistant 2025.10 or later.
- The app finds Echoes the way it finds its own satellites, over mDNS. With
  both apps on one host that stays on the host, so it works even where mDNS
  does not cross your VLANs. Running the app on another machine needs mDNS
  between the two, and for **on the Echo** the app must also be able to
  reach the Echo itself (TCP port 6055). The Echo announces itself over
  mDNS, and the controller announces it too, on its own network, so where
  mDNS does not cross your VLANs it is enough for the app and the controller
  to share one.

## Switching an Echo over

1. Dashboard → the Echo → **Config** → **05 — Advanced**. To switch only
   this Echo, set the section's switch to **Device** first; on **Fleet** the
   section shows the fleet's values read-only. Then **Voice assistant** →
   **Voice Incubator (experimental)** or **Voice Incubator, on the Echo
   (experimental)**, and save. (Or set it in the fleet settings to switch
   every Echo that follows the fleet's Advanced section.)

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
   connected** (with *on the Echo* for the second way).

Home Assistant reconnects to the Echo once when you switch, and its device
page loses the **Assist satellite** and **wake word** entities. The media
player, the action button, the light and mute sensors and the Bluetooth proxy
stay as they were.

**Switching back** is the same setting set to **Home Assistant**: the Assist
entities come back and Home Assistant answers again. If you disabled the
Assist satellite entity by hand at some point, enable it again — a disabled
one answers nothing.

## What works, both ways

- A wake word, then a question or a command; follow-up questions the app
  asks.
- The action button: a tap asks a question, a tap during one cancels it.
  Cancelling takes one tap; asking again takes another.
- **Barge-in** (the controller's satellite): the wake word over a reply cuts
  it off and starts a new question (turn barge-in off in **02 — Wake word**
  to have the reply play to its end).
- The app's **timers and alarms** ring on the Echo that set them, and its
  **announcements** play on it, to and from other Echoes using it.
- Music keeps playing, ducked under questions, replies, rings and
  announcements.

## On the Echo: what it adds and changes

- **The app's wake words.** The app sends the Echo the wake words chosen for
  it on its *Wake words* page (Okay Nabu, Hey Jarvis, Hey Mycroft, or your
  own trained ones), up to three at once, and the Echo runs them itself.
  While it holds any, EchoMuse's own wake word (**02 — Wake word**) stands
  down. Home Assistant's wake word switch does not apply; what the Echo
  listens for is set in the app.
- **The app re-checks the wake word** against the 1.5 s of audio before it,
  as it does for its own satellites (see Privacy). The first check after the
  Echo connects can take a second or two while the app loads its recogniser;
  after that it takes a fraction of a second.
- **"Stop"** stops a ringing timer, as does a tap, even while muted.
- **The ring** shows the app's state with your ring scene: listening, the
  spinner while it works, the meter while it answers, a short flash when a
  command needed no reply (or failed), an amber pulse while a timer rings,
  the meter during an announcement, and **running timers** between calls,
  each in its colour, the ring counting down to the next. In the app's
  **night mode** (its per-area switch in Home Assistant) the ring is dim and
  amber, a ringing timer aside, and shows how a command went as a green
  pulse or two red blinks. A wake word said during an answer is ignored; a
  tap cancels.
- **Questions back** ("Which room?") are answered without the wake word:
  the Echo listens again once the question has played.
- **Without the controller (optional)**: with **Keep working without the
  controller** switched on, the Echo saves its satellite, ring and volume,
  starts them at boot and keeps answering the app when the controller is
  down, its action and volume buttons its own. The app must still find it:
  the controller keeps announcing it while it runs, and the Echo announces
  itself on its own network. Firmware updates still need the controller.
- **Live listening (optional)**: the app's Test page can listen to the Echo
  only if you switch on **Let the app listen live**, under the voice
  backend. It is off by default (see Privacy).
- **Voice enrolment and wake word recording** need a second, echo-cancelled
  capture the Echo does not send, and the app refuses both for it. Speaker
  identification, as the app's code reads, falls back to the one capture and
  uses voices enrolled elsewhere; it has not been tried on an Echo. The
  app's **intercom** is not offered.
- **The Activity tab does not list these calls**: they go between the Echo
  and the app without passing the controller. The app's own Sessions page
  has them.

## The controller's satellite: what is different

- **Which wake word.** The Echo listens for the wake word set in EchoMuse
  (**02 — Wake word**), as before. The app's *Wake words* page does not
  apply, and the app accepts the Echo's detection as it is: it sends too
  little audio from before the wake word for the app to re-check it.
- **Turning the wake word off** is done in Home Assistant's wake word
  picker, which an Incubator Echo no longer has. An Echo whose wake word was
  switched off there stays off; switch it back on before moving it over.
- **The ring** shows listening, thinking and the reply as usual. The app's
  timers ring by sound only: the ring does not light for them.
- **Not supported**: the app's live microphone test and intercom, enrolling
  a voice from an Echo, and recording wake word samples on one. All of them
  stream the microphone outside a question, which this satellite never does
  ([Listening and privacy](listening.md)).

## Both ways

- **Replies sound slightly duller.** The app's protocol carries 16 kHz audio,
  so replies stop at about 7 kHz, where Home Assistant's replies reach the
  Echo at full bandwidth. Music is unaffected.
- **Settings that do nothing on an Incubator Echo**, because what they
  control is part of Home Assistant's pipeline: *Noise suppression* (03 —
  Microphones), *Save utterances*, *Speak while the reply is written*.
- **Questions the app does not understand** go to the conversation agent
  chosen in the app's settings. To have it answer about your home ("how
  warm is the living room?"), expose those entities to voice in Home
  Assistant (Settings → Voice assistants → Expose).
- **Timer progress and night mode** show on the ring only with the Echo's
  own satellite.

## Privacy

**The controller's satellite**: nothing about what leaves the Echo changes.
It detects its own wake word and sends audio only from just before it until
you stop speaking ([Listening and privacy](listening.md)), and the controller
passes it to the app on your own machine instead of to Home Assistant.

**On the Echo**: nothing leaves the Echo until one of the app's wake words
fires, then it goes straight to the app until you stop speaking. Each wake
call starts with **1.5 s of audio from before the detection** (where the
controller's satellite sends about a quarter of a second), because the app
checks the wake word again on it. A tap sends nothing from before it. With
**Let the app listen live** switched on, the Echo streams its microphone to
the app for as long as the app's Test page listens (never during a
question, never muted); the ring pulses red the whole time and the dashboard
says it streams.

What the app keeps, and for how long, is the app's — see its documentation.

The satellite port has no authentication, as the app's own satellites have
none: whichever client connects first receives the Echo's questions. It is
the Echo's voice port + 2000 for the controller's satellite, and port 6055
on the Echo itself for the second way. Keep it reachable only from where the
app runs.

## When it doesn't answer

- **Status tab, Voice assistant row.** *Voice Incubator connected* (green)
  is what you want. *Waiting for the Voice Incubator app* (amber): the app
  is not connected to this Echo — check that it is running. *Not listening*:
  the Echo is offline.
- **Without the app the Echo has no voice assistant.** A wake gives the
  short orange "no assistant" cue, a tap the error flash, and nothing
  answers. It never falls back to Home Assistant on its own.
- **On the Echo, a wake word that starts a call and hangs up at once** is
  the app rejecting it on its second check; its Sessions page says why.
- A support bundle (**Settings → Support**) records whether the app was
  connected; the project's `bundle_check` tool reports *"the Voice Incubator
  app is not connected"* from it.
- **On firmware v2.19.0 and older, after a question is cancelled before its
  answer began, the next answer can end early for the app.** The Echo keeps
  playing it, but the app and EchoMuse treat it as finished as soon as it
  has been sent. Newer firmware fixes it.
- **Very long answers** (more than about half a minute) can make the app log
  *"Timed out waiting for playback to drain"* with the controller's
  satellite. The answer still plays to its end.

## Tested on

The controller's satellite, on hardware: two Echo Dot 2 (biscuit) on emOS
with firmware v2.19.0, Voice Incubator 0.3.1 as a Home Assistant app on the
same host as EchoMuse, Home Assistant 2026.11.1, both Echoes detecting their
wake word on the device (the default). Wake word calls, barge-in, timers
(stopped by the wake word and by the button), announcements between the two
Echoes, music, switching backends back and forth, and the app being stopped.
On one of the two: asking with the action button, a follow-up question the
app asked (which of two timers), and a tap cancelling a call, both while the
app listened and during its reply.

On the Echo, on hardware: one Echo Dot 2 (00LQ) on emOS with firmware built
from `feat/integration` (2026-11-17), same app and Home Assistant. Button
calls and cancels, long answers, the app's three built-in wake words and
"stop" on a ringing timer, the ring in every state above, questions back,
mute (and a muted tap stopping a timer), music ducked through the answer,
live listening switched on and off, a dropped data link (it recovers in
about 30 s and the wake words work after it), and switching back to the
controller's satellite. The three wake words cost the Echo a few percent of
its CPU.

Not tested: speaker identification on an Echo, several Incubator Echoes
hearing the same wake word, detecting the wake word on the controller
(*Wake word detection: On the controller*), and the app running on another
machine.
