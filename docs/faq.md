# FAQ

Quick answers and workarounds for the things that come up most. Where an
answer has a longer version, it links to it.

Issue numbers link to EchoMuse's tracker, where most of these were worked
out. If your problem isn't here, [support-bundle.md](support-bundle.md)
explains what to attach to an EchoHome issue, and
[Voice Incubator](voice-incubator.md#when-it-doesnt-answer) covers an Echo
that does not answer.

---

## Rooting and unlocking

### Should I unlock my Dot to FireOS 5 or FireOS 6?
**Either works.** firebreak's `unlock fireos5` leaves the Dot on FireOS 5,
where you can install emOS or keep FireOS with root. `unlock fireos6`, the
default, leaves it on FireOS 6, where EchoMuse offers emOS. An Echo 2nd gen
has FireOS 6 only. [firebreak](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md#unlocking-with-firebreak) moves a Dot
between them.

More in [EchoMuse's rooting guide](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md).

### The unlock won't run on my Mac.
**Use [firebreak](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md#unlocking-with-firebreak).** It runs on macOS,
Linux and Windows. Everything after it, including the
provisioning wizard, runs from a Chromium-based browser on any OS.

### `brick.sh` refuses with "restricted on locked hw", or `fastbrick.sh` sits at "Sending payload...".
**Use [firebreak](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md#unlocking-with-firebreak) instead.** amonet's
scripts pick the exploit image from the Dot's LK, which fastboot reports as
`lk_build_desc`, and know only one FireOS 6 LK, `63cb91b-20221007_072309`.
FireOS 6 4310, 4313 and 4315 ship `41fb3ce-20221007_151724`, which then gets
the wrong image and refuses it. amonet 2.0.0's `fastbrick.sh` hides the
refusal: its retry loop swallows the error, so it waits at "Sending
payload..." instead of failing.

firebreak sends the right image for every LK in
[its table](https://github.com/bboe/firebreak#bootloaders), so the Dot does
not need a FireOS update first. With amonet's scripts, letting Amazon update
the Dot to FireOS 6574.1 moves it to `63cb91b`
([#567](https://github.com/wilbowes/EchoMuse/issues/567)).

Turn **off** any file manager that auto-mounts USB devices. It grabs the
handshake and makes later adb steps fail with unhelpful errors.

### fastboot sees the Dot, but `getvar` never answers (Linux).
**Check TLP's USB autosuspend.** On a laptop running TLP, the Dot's fastboot
device can be suspended between commands, so a query that answered once then
times out. Add the Dot's fastboot USB ID to TLP's denylist in
`/etc/tlp.conf` and reboot:

```
USB_DENYLIST+="0bb4:0c01"
```

`0bb4:0c01` is what one Dot reported in fastboot; check yours with `lsusb`.
`systemctl is-active tlp` reading `inactive` does not rule this out, because
TLP's udev helper still applies the policy
([#697](https://github.com/wilbowes/EchoMuse/issues/697)).

### The wizard stops at Escrow Boot Image and mentions `boot_a_x`.
**The Echo has amonet v1's partition table under a v2 unlock.** Installing
v2 is meant to put the original table back, and on this Echo it has not. The
wizard has read and written nothing. Reinstalling FireOS does not change the
table, so it won't help.

Two owners got past it by installing amonet v2 again, as a zip from TWRP
([#712](https://github.com/wilbowes/EchoMuse/issues/712)). **We have not run
that ourselves and can't tell you it is safe:** it rewrites the partition
table and the bootloader, and an Echo on which it fails partway may not
start. Ask on the XDA thread first and quote the wizard's error line.

### Sideloading FireOS 5.5.5.4 fails with a red flash of the ring.
Sideload it again. It generally works on the second attempt.

### Deeper questions about the unlock itself.
For a failed firebreak run, run `uvx firebreak@latest report` and paste what it
prints in `#support-device-unlocking` on the Discord. amonet itself is
R0rt1z2's work, and the [XDA thread](https://xdaforums.com/t/unlock-root-twrp-unbrick-amazon-echo-dot-2nd-gen-2016-biscuit.4761416/) is the reference for it.

### Did I brick it?
**Probably not permanently.** A Dot stuck in preloader is usually
recoverable — see the recovery notes on the XDA thread. Don't flash anything
else at it until you've asked.

### The Dot boot-loops on one computer but not on another.
**The USB port can't power it.** The Dot draws the most as it enters TWRP,
when the ring turns bright white, and a weak port lets it brown out and
restart. Its own adapter supplies 9 W (5.2 V at 1.8 A); a computer's
USB 2.0 port need supply only 2.5 W (0.5 A).

To confirm a lack of power, boot into TWRP on a wall charger rated 1.8 A
or more. A charger carries no data, so it cannot finish the unlock, but a Dot that
stops looping on it has a power problem. Then finish from another computer.
One owner's Dot looped on a desktop's ports, reached TWRP at once on a
charger, and finished from a different computer.

---

## The provisioning wizard

### The wizard can't see my device, or says it's in use by another program.
**Run `adb kill-server` first.** A running adb server on your machine holds
the device and the browser can't claim it. This is the single most common
provisioning failure.

### The device picker shows nothing.
Use **Chrome or Edge**. The wizard talks to the device over WebUSB and other
browsers vary. Brave in particular is unconfirmed.

### The wizard says it needs a secure context.
If you reach Home Assistant over plain `http://`, the browser blocks WebUSB, and
the wizard's first step names the exact origin to allow. Either serve Home
Assistant over HTTPS, or add that origin at
`chrome://flags/#unsafely-treat-insecure-origin-as-secure` and restart the
browser. The entry has to match scheme, host **and port** exactly, so one for
some other address does not cover it.

### The USB connection drops and re-enumerates every few seconds.
`persist.sys.usb.config` is set to `mtp,adb`, and the composite gadget is what
drops the bus — roughly every six seconds, with `device firmware changed` in
`dmesg` each time. Forcing it to `adb` alone fixes it. Nothing EchoMuse does
needs MTP.

**On a Linux desktop, try the computer side first.** GNOME's MTP monitor
probes the Echo's MTP interface, and that is what resets the link. Stop it,
then unplug and replug the Echo:

```sh
systemctl --user stop gvfs-mtp-volume-monitor
systemctl --user mask --runtime gvfs-mtp-volume-monitor
```

The monitor restarts itself on demand, so `stop` alone does not hold; the
mask lasts until the computer reboots. This fixed it for two owners on
Ubuntu, both at the end of the unlock and in the wizard. Windows is not
affected.

**`setprop` from a booted device will not work.** Android's property service
refuses that specific property whatever `ro.secure` and `ro.debuggable` say
(`init: sys_prop: permission denied uid:2000 name:sys.usb.config`), so it has
to be changed before that layer applies. From TWRP, unpack the boot image, edit
`default.prop` to read `persist.sys.usb.config=adb`, add
`persist.sys.usb.state=adb`, repack and write it back — the full commands are
in [#79](https://github.com/wilbowes/EchoMuse/issues/79).

Root-caused by @kylegordon and confirmed independently by @midiland, who found
`setprop` did work on his device — worth trying first, since it costs nothing.

*(This entry previously advised setting `mtp,adb`, which is the cause rather
than the fix. Corrected 2026-09-05.)*

### A wizard step failed and I don't know why.
Every failed step offers diagnostics. Grab those before retrying — the state
the device is in is the diagnostic, and retrying destroys it.

### The wizard says `/data` is not mounted in TWRP.
**The userdata partition may have no filesystem yet.** The unlock can leave
it blank, and a device that goes straight from the unlock into TWRP never
boots Android, which is what would normally format it. One TWRP build has
also been seen with no filesystem type on its `/data` line.

The fix is TWRP's **Wipe → Format Data**. It **erases everything on `/data`**,
which on a freshly unlocked Dot is nothing. On a Dot you have been using, stop
and ask on [#598](https://github.com/wilbowes/EchoMuse/issues/598) first.

### The Build emOS step fails.
Check anything between your browser and Home Assistant. This step sends your
boot image (about 9 MB) to the controller, and a reverse proxy with a small
upload limit refuses it. NGINX's default is 1 MB; raise `client_max_body_size`
([#556](https://github.com/wilbowes/EchoMuse/issues/556)).

### The Echo boot-loops straight after provisioning.
In both reports so far, the unlock hadn't finished. On amonet 2.0.0, check
FireOS 6 was flashed to **both** slots, and redo that step from R0rt1z2's
thread if not ([#619](https://github.com/wilbowes/EchoMuse/issues/619),
[#603](https://github.com/wilbowes/EchoMuse/issues/603)). Your escrowed boot
image puts the boot partition back from TWRP if you need to start over.

### Connect Console fails on Linux.
"Failed to open serial port" in the wizard means the browser was refused the
Echo's serial port by your computer; the Echo has booted emOS and is waiting
for WiFi. Three Linux users with Chromium have hit it
([#605](https://github.com/wilbowes/EchoMuse/issues/605),
[#767](https://github.com/wilbowes/EchoMuse/issues/767),
[#810](https://github.com/wilbowes/EchoMuse/issues/810)), and one on Fedora
got past it by joining the group that owns the port:

```
sudo usermod -aG dialout $USER     # then log out and back in
```

ModemManager also probes new USB serial devices on many distributions, so
run `sudo systemctl stop ModemManager` and `adb kill-server` before clicking
Connect Console. If it still fails, open the console yourself (below) and
finish the WiFi step there.

### After the reboot into emOS the ring sits blue, or part of it turns red.
**The Echo has booted emOS and is waiting for WiFi.** A blue ring with a
brighter segment throbbing means it has no network saved yet, which is where
every new install stops until the wizard's WiFi step. Red means a saved
network did not connect within two minutes. Neither is a failed flash.

If the wizard can't open the console to finish, open it yourself (below) and
run `em-wifi`: it scans, you pick a network by number (or `h` to type a
hidden network's name) and type the password
([#810](https://github.com/wilbowes/EchoMuse/issues/810),
[#825](https://github.com/wilbowes/EchoMuse/issues/825)).

---

## emOS

[emOS](https://github.com/wilbowes/EchoMuse/tree/main/emos) is EchoMuse's, by Wil Bowes and the EchoMuse
contributors; EchoHome installs and updates its releases. Questions about
emOS itself belong there.

### emOS or FireOS: which should I pick in the wizard?
The wizard offers **emOS** first. It replaces Android on the Echo entirely,
keeping only Amazon's kernel, and it is why the 3.5mm jack behaves properly.
**FireOS** is one labelled click away and is what most fielded devices run.
Both flows save your original boot image before writing anything (the FireOS
flow since controller 2.24.0), and restoring it from TWRP takes about ten
seconds. emOS works after
either amonet version; the FireOS flow needs **v1.1.0**. See the first
question on this page.

### How do I open the emOS console myself?
Plug the Echo in by USB and open its serial port in a terminal, at 115200
baud:

- **Linux:** `screen /dev/ttyACM0 115200`. The number varies; `ls /dev/ttyACM*`
  lists the candidates.
- **macOS:** `screen /dev/tty.usbmodem* 115200`
- **Windows:** a serial terminal such as PuTTY, on the COM port Device Manager
  shows for the Echo.

`screen` on Linux is confirmed by users; the macOS and Windows lines are the
usual names for a USB serial device and haven't been confirmed on emOS yet. If
you set a console password, it asks for that first.

The banner it prints gives the Echo's address, the controller it is connected
to (or `not connected`), and where the logs are. `/tmp/server.log` is
EchoMuse's own.

### The Echo is on WiFi but never connects to the controller.
The Echo finds the controller over mDNS, which does not cross subnets or
VLANs. Put them on the same network, or give the Echo a
[static controller endpoint](configuration.md#static-controller-endpoint).
To check, open the console and run `tail -n 40 /tmp/server.log`: repeated
`mDNS: no server found` lines mean discovery is what's failing.

With a static endpoint the firewall still has to let the Echo open TCP 8767
and 8770 on the controller (the default ports). An mDNS relay between the VLANs was not enough
for one owner ([#827](https://github.com/wilbowes/EchoMuse/issues/827)).

### No sound on emOS with FireOS 6.
Update the controller to **2.24.1** and the device to **v2.16.0**. Earlier
firmware left part of the speaker path switched off on FireOS 6's kernel
([#587](https://github.com/wilbowes/EchoMuse/issues/587)).

### How do I update emOS?
Open the Echo in the dashboard and go to **Updates**. An Echo on emOS has an
**emOS** panel there with the version it runs and the newest release; press
**Update**. This needs controller 2.26.0 or later and installs emOS 0.10
or later. On an older controller, re-run the wizard's build and flash steps
instead.

The Echo restarts and is back in about a minute, with its WiFi, settings and
console password untouched. If the new image does not reach the controller,
the Echo restarts itself every three minutes and, after three tries, puts the
previous image back and shows an amber ring. That takes about eleven minutes,
and the Echo's log then says it rolled back.

**Keep the Echo powered while it updates.** A power cut during the second or
so in which the boot partition is written leaves an image that cannot start,
and recovering from that needs TWRP, a USB cable and the provisioning wizard.
Nothing after that write can leave it in that state.

### How do I run the wizard again on an emOS device?
emOS has no adb, so the wizard cannot see it directly. Open the USB console,
run `/init recovery`, and the Echo reboots into TWRP, where the wizard's first
step accepts it. This needs emOS 0.4 or later. If the wizard then says the
device is already registered, let it delete the old entry.

---

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

## Hardware and scope

### Will this work on an Echo Show or Studio?
The Echo Dot 2nd gen (biscuit), the Echo 2nd gen (radar) and the Echo Dot
3rd gen (donut) are supported. Community ports have been started in EchoMuse
for the Echo Show 8 ([#358](https://github.com/wilbowes/EchoMuse/pull/358))
and the Echo Show 5 ([#36](https://github.com/wilbowes/EchoMuse/issues/36)).
Only the original Gen 3 has a known unlock: the refreshed Gen 3, the Echo Dot
Gen 5 and the Echo Studio have none. Check XDA before buying one for this.
Other boards are welcome — the Android-specific surface is about twenty call
sites, so a new board is mostly a mic/speaker/LED/button binding.

### Does it depend on Amazon's software?
Barely, and keeping it that way is deliberate. It's a Linux daemon using ALSA,
i2c, evdev and sysfs that happens to run on Android because that's what
shipped on the box. A change that puts an Amazon blob back in the audio path is going
the wrong way, even when it sounds better.

### I want to help. Where do I start?
[CONTRIBUTING.md](../CONTRIBUTING.md) covers building from source and the
tests.
