# The Echo as a Voice Incubator satellite

> **Experimental.** [Voice Incubator](https://github.com/OHF-Voice/voice-incubator)
> is marked "experimental and not supported" by its authors, and its
> satellite protocol may change.

Every EchoHome Echo is a satellite of OHF-Voice's **Voice Incubator** app,
as a Voice PE on the Voice Incubator firmware is. The app does the listening
and answering: it recognises commands in the audio and runs its own timers,
alarms and announcements. The Echo runs the app's wake words itself, talks
to the app directly on TCP port 6055, and keeps doing so with the setup tool
stopped.

## Before you start

- **The Voice Incubator app** and its integration, installed as its
  [install steps](https://github.com/OHF-Voice/voice-incubator#install) say.
  It needs Home Assistant 2025.10 or later.
- The app finds satellites over mDNS. The Echo announces itself
  (`_voice-incubator._tcp`, as `echomuse-<serial>`) while it listens, and
  the setup tool announces it too, at the Echo's address, on its own
  network: where mDNS does not cross your VLANs it is enough for the app and
  the setup tool to share one, as long as the app can reach the Echo on port
  6055. The setup tool keeps announcing an Echo while it is offline.

## Adding an Echo

1. Approve the Echo in the setup tool's dashboard. Within a few seconds its
   satellite listens, and the **Status** tab shows **Voice assistant: Voice
   Incubator**, waiting for the app or connected.
2. In the app, add the Echo as a satellite, and choose its wake words on the
   *Wake words* page (Okay Nabu, Hey Jarvis, Hey Mycroft, or your own; up to
   three at once). Home Assistant links it to the Echo's ESPHome device.

## What works

- A wake word, then a question or a command; questions the app asks back
  ("Which room?") are answered without the wake word.
- **The action button**, in the Voice PE's order: a tap stops a ringing
  timer, cancels a call, pauses music that is playing (Home Assistant's, or
  the whole Sendspin group), resumes what a tap paused, and otherwise asks.
  Muted, it still stops a timer and pauses music, and asks nothing. Over
  music, talk with the wake word. A tap that stops a timer or cancels a
  call also stops what Home Assistant is announcing on the Echo.
  **"Stop"**, said alone, stops a ringing timer, and said while the Echo
  answers it ends the answer, as a tap does. It is listened for only then.
- **A Home Assistant voice**: with a Home Assistant TTS entity as the Echo's
  voice in the app, replies play on the Echo's media player, and the Echo
  tells the app when each starts and ends.
- The app's **timers and alarms** ring on the Echo that set them, and its
  **announcements** play on it.
- **The ring** shows the app's state with your ring scene: listening, the
  spinner while it works (in the speaker's color, from the app's People
  page, once their voice is enrolled and recognized), the meter while it
  answers, a short flash when a
  command needed no reply (or failed), an amber pulse while a timer rings,
  and running timers between calls, the ring counting down to the next. In
  the app's **night mode** the ring is dim and amber, a ringing timer aside.
- **Music** keeps playing, ducked under calls, replies, rings and
  announcements.
- **Live listening**: the app's Test page can listen to the Echo only while
  its **Voice Incubator live listening** switch in Home Assistant is on (off
  by default).

- **Voice enrolment** (the app's People page) and **wake word recording**:
  switch the Echo's *Voice Incubator live listening* on, enrol or record on
  the app's page while the ring pulses red, then switch it off again. The
  app records from the Echo's second capture while monitored, which the
  Echo offers only with that switch on. Recognising who speaks in a call
  keeps working with the switch off: calls send one capture, and the app
  identifies speakers on it.

Not offered: the app's **intercom**. Replies are 16 kHz audio, so they sound
slightly duller than music.

## Privacy

Nothing leaves the Echo until one of the app's wake words fires or you tap
the button; then the call goes straight to the app until you stop speaking.
A wake call starts with **1.5 s of audio from before the detection**, which
the app uses to check the wake word again; a tap sends nothing from before
it. With live listening on, the Echo streams its microphone to the app for
as long as the app monitors it: its Test page, or enrolling a voice or
recording a wake word. Never during a call, never muted. While monitored each
frame goes twice, as the first and the second capture, which the app's
enrolment and recording read. The ring pulses red the whole time and the
dashboard says it streams. The setup tool receives none of it
([Listening and privacy](listening.md)).

The satellite port has no authentication, as the app's own satellites have
none: whichever client connects first receives the Echo's calls. Keep port
6055 reachable only from where the app runs.

## When it doesn't answer

- **Status tab, Voice assistant row**: *connected* is what you want;
  *waiting for the Voice Incubator app* means the app is not connected to
  this Echo (is it running, can it reach port 6055?); *not listening*: the
  Echo is offline.
- **Without the app the Echo has no voice assistant**: a tap gives the error
  flash and nothing answers.
- **A wake word that starts a call and hangs up at once** is the app
  rejecting it on its second check; its Sessions page says why.
- Firmware from before EchoHome cannot be its own satellite and is not
  served: update it from the dashboard.
- A support bundle (**Settings → Support**) records whether the app was
  connected.

## Tested on

One Echo Dot 2 (00LQ) on emOS, Voice Incubator 0.3.1 as a Home Assistant app,
Home Assistant 2026.11.1: button calls and cancels, long answers, the app's
three built-in wake words and "stop" on a ringing timer, the ring in every
state above, questions back, mute, music ducked through the answer, live
listening on and off, a dropped link, a cold boot with the setup tool
stopped. The three wake words cost the Echo a few percent of its CPU.

Since then, on 00LQ: the spinner in a recognized speaker's color, voice
enrolment, and the button pausing Home Assistant's music.

Not tested: "stop" over an answer, a Home Assistant voice, pausing a
Sendspin group with the button, several Echoes hearing the same wake word,
the app on another machine than Home Assistant.
