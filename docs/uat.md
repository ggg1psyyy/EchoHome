# Acceptance testing

The checks to run on real hardware after a firmware or setup tool change,
and the order to run them in. Each is pass or fail; a failure goes in an
issue with a [support bundle](support-bundle.md) and the step's letter and
number.

## Before you start

- One Echo provisioned and approved, added to Home Assistant (it is offered
  under Discovered, named as on the dashboard) and to the Voice Incubator
  app, with at least one wake word chosen in the app.
- The firmware under test installed by OTA from the dashboard (Device →
  Update), and the device page showing that version.

## A — With the setup tool running

1. **Status reads true.** The Status tab shows the firmware, WiFi, *Voice
   Incubator connected*, and **Microphone** as the Echo reports it.
2. **Settings push.** Change the EQ or the ring scene; it takes effect at
   once.
3. **Update and rollback.** An OTA to the same build is refused as already
   running; an OTA to another build boots it, and a build that fails to start
   is rolled back by itself.

## B — With the setup tool stopped

Stop the add-on (or the container), then reboot the Echo (`kill -TERM 1`
from the emOS console, which has no `reboot`; on FireOS pull the power).
Everything here must work with nothing but Home Assistant and the app.

1. **Wake word and a call**: say a wake word, ask something, hear the reply.
2. **The action button**: a tap asks; a tap during the call cancels it.
3. **Both mutes**: the button mutes (red ring) and the wake word does
   nothing; Software Mute in Home Assistant does the same, and a button press
   clears it.
4. **Home Assistant media**: play something to the Echo's media player;
   pause, volume and an announcement over it.
5. **The ring**: the LED Ring light changes colour and brightness between
   calls; the satellite's states show during a call.
6. **The Echo's switches and select**: toggle each, reboot again, and each
   keeps its value: red ring while muted, volume arc for remote changes,
   Bluetooth proxy, Voice Incubator live listening, wake word microphone.
7. **Live listening**: with the switch on, the app's Test page hears the
   Echo and the ring pulses red; with it off the Test page hears nothing.

## C — After starting the setup tool again

1. The Echo reconnects by itself, shows as online, and nothing it kept on
   the Echo has changed (B6's values, the volume).
2. Firmware from before EchoHome (if you have one) is offered the update and
   its listening is shown as the Echo reports it.

## Already known

- Both 3.5 mm jack channels can be silent on emOS (EchoMuse #999).
- emOS on FireOS 6 cannot join WPA3 networks (EchoMuse #536).
