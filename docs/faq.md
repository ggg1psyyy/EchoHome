# FAQ

Quick answers and workarounds for the things that come up most. Where an
answer has a longer version, it links to it.

Issue numbers link to EchoMuse's tracker, where most of these were worked
out. If your problem isn't here, [support-bundle.md](support-bundle.md)
explains what to attach to an EchoHome issue, and
[Voice Incubator](voice-incubator.md#when-it-doesnt-answer) covers an Echo
that does not answer.

---

## Rooting, the setup wizard, emOS and hardware

Unlocking an Echo, the setup wizard's steps and failures, emOS, and which
hardware works are EchoMuse's: EchoHome uses its unlock path, its wizard and
its emOS releases. Their questions and answers are in
**[EchoMuse's FAQ](https://github.com/wilbowes/EchoMuse/blob/main/docs/faq.md)**: rooting and unlocking, the provisioning wizard,
emOS, and hardware and scope.

EchoHome is built on [EchoMuse](https://github.com/wilbowes/EchoMuse) by Wil Bowes and the EchoMuse contributors.

## Controller and dashboard

### Add-on or Docker container — which should I run?
Whichever suits you. **Both are first-class** and neither is improved at the
other's expense. The add-on is easier if you already run Home Assistant OS;
the container is the answer if the controller lives elsewhere.

### I'm read-only in the dashboard under the add-on and can't get admin back.
Fixed — **update the controller**. The rule now counts Home Assistant admins
who can sign in through ingress, so the first HA user gets admin. On the first
page load after updating, the dashboard re-reads your role from the server and
corrects itself; you shouldn't need to do anything else.

If you're stranded on an older build, in the browser console:

```js
['em_token','em_role','em_auth_via'].forEach(k => localStorage.removeItem(k));
location.reload();
```

Background: [#235](https://github.com/wilbowes/EchoMuse/issues/235).

### How do I update the controller?
```
docker compose pull && docker compose up -d
```
or update the add-on from Home Assistant. **The dashboard's update notice is
advisory and always will be** — the controller is your container, and a
process cannot restart itself mid-request and then tell you how it went.

### Home Assistant offers an update, but installing it fails.
Wait a few minutes and try again. Home Assistant can see a new version shortly
before its image has finished publishing, and until then the install fails
with `manifest unknown`. Nothing is wrong with your setup.

### The add-on won't start: "NumPy was built with baseline optimizations: (X86_V2)".
Fixed in **2.23.1**; update the add-on. It affected Proxmox VMs using the
`kvm64` CPU type, which lacks instructions NumPy 2.4 needed.
[#496](https://github.com/wilbowes/EchoMuse/issues/496).

### The update notice shows no release notes.
Notes come from the tag annotation. If they're empty, that's ours to fix —
please file it with the version you're on.

### The dashboard shows my device as Online but nothing works.
Check **Device → Status → Voice assistant**. *Waiting for the Voice
Incubator app* means the app has not connected to the Echo's satellite: is
the app running, and can it reach the Echo on port 6055? See
[Voice Incubator](voice-incubator.md#when-it-doesnt-answer).

### Where is my data?
SQLite beside the controller — in `/data` for the add-on, in the mounted
volume for the container. Back that directory up; it holds devices, users,
config, hourly stats and the TLS CA.

### Can I move the controller to a different IP?
Yes. Devices find it over mDNS and the TLS certificate deliberately identifies
it by name, not address, so it can move freely. **Static endpoints for routed
or tunnelled networks are in progress** —
[#106](https://github.com/wilbowes/EchoMuse/issues/106) /
[#166](https://github.com/wilbowes/EchoMuse/issues/166).

---

## Audio

### The audio distorts when it's loud.
Check **Config → Playback**: the limiter and bass guard should be on. If it
still distorts, report the volume percentage — the number is the diagnostic.

### The 3.5mm jack is very quiet, or only one channel plays.
Fixed in firmware v2.18.0 (Early Access from v2.18.0-ea.2). A codec switch
stock turns on with a plug in was never set
([#566](https://github.com/wilbowes/EchoMuse/issues/566),
[#669](https://github.com/wilbowes/EchoMuse/issues/669)). On older firmware,
`tinymix -D 0 "Audio_DacMux_Setting" On` from a shell brings it back until
the next restart.

### The internal speaker is quieter than stock.
Reported on some Dots and not others of the same model
([#630](https://github.com/wilbowes/EchoMuse/issues/630)); there is no
setting for it yet. Changing `HP Driver Gain` with `tinymix` lasts a few
seconds, because the firmware keeps writing its own speaker routing back.

### It sounds worse than stock Alexa did.
Partly true and partly fixed. A DAC clipping fault above unity gain was found
and corrected, which was most of it. What remains is that stock applies a
substantial driver correction we don't yet —
[#247](https://github.com/wilbowes/EchoMuse/issues/247).

## Devices and the fleet

### A firmware update failed.
Fixed in `controller-v2.18.0` — **update the controller first**, then retry.
Two faults were behind it: the transfer deleted the fallback slot before
sending anything, and one error message covered five different outcomes.

If you're on an older controller, the workaround that worked: update one
device at a time, with the device close to an access point.

**Never power-cycle a device mid-update.**

### A device stopped responding after an update.
Device → **Updates** offers a rollback to the previous slot. If that doesn't
help, a support bundle plus the time it happened is the right report.

### I deleted a device and it kept working.
Fixed — update the controller. A deleted device now drops its connection and
comes back as pending.

### One device settings, or all of them?
Config is scoped per section. A device follows the fleet for every section it
doesn't override, so changing one device's Ring settings leaves its
microphone and playback settings tracking fleet changes. **Device → Status → Config** tells you which
it is.

### Can I swap a dead Echo for a new one and keep its history?
Not yet — [#133](https://github.com/wilbowes/EchoMuse/issues/133).

### The device won't reconnect to WiFi after a reboot.
If your network has no internet access, Android decides it is bad and stops
auto-joining it. **Fixed for devices provisioned by the current wizard**
([#317](https://github.com/wilbowes/EchoMuse/issues/317)). A device provisioned
before that fix can still hit it, and clearing it on a fielded device is
[#439](https://github.com/wilbowes/EchoMuse/issues/439).

### Can it use 5 GHz WiFi?
**Yes.** It runs on a combined 2.4 and 5 GHz network. One owner's ISP router
(a BT Smart Hub 2, which cannot split its bands) let the Echo join on 5 GHz
and never gave it an address. Turning 5 GHz off until the Echo joined on
2.4 GHz fixed it. WPA3 is not supported; see the
[README](../README.md).

### There's no ambient light sensor on my device.
Some Dots ship a second-source sensor that binds a different driver. Known,
and the fix is unverifiable on our hardware —
[#90](https://github.com/wilbowes/EchoMuse/issues/90) — so a report from
affected hardware is genuinely useful.

---

## Privacy

### Is a support bundle safe to attach to a public issue?
Yes, by design. It's an allowlist: no transcripts, no saved audio, no WiFi
SSIDs, no IP addresses, no device labels you wrote, no credentials, no file
paths. It's plain JSON — [open it first](support-bundle.md) rather than take
our word for it.

### Is the link between device and controller encrypted?
Yes, once the Echo is paired. Approving a new Echo pairs it. An Echo already
on `plain ws` (its Status tab Link row) pairs when you hold its action button
for 7 seconds (5 on firmware before v2.19.0) and then approve it in the
dashboard; on firmware too old to ask,
the Status tab offers **Pair** instead. The Echo's ESPHome connection to Home
Assistant is encrypted with a key Home Assistant sets when it adds the Echo.
The Voice Incubator satellite port (6055) has no authentication, as the
app's own satellites have none.

---

## About EchoHome

### Does it depend on Amazon's software?
Barely, and keeping it that way is deliberate. It's a Linux daemon using ALSA,
i2c, evdev and sysfs that happens to run on Android because that's what
shipped on the box. A change that puts an Amazon blob back in the audio path is going
the wrong way, even when it sounds better.

### I want to help. Where do I start?
[CONTRIBUTING.md](../CONTRIBUTING.md) covers building from source and the
tests.
