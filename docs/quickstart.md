# EchoHome Quickstart

From an Echo to a Voice Incubator satellite in Home Assistant. You need:

| | |
|---|---|
| An Echo Dot 2nd gen, Echo 2nd gen or Echo Dot 3rd gen | The hardware being repurposed |
| Home Assistant 2025.10 or later | With the [Voice Incubator](https://github.com/OHF-Voice/voice-incubator#install) app and its integration installed |
| A computer, a USB cable and Chrome or Edge, once | To unlock the Echo and run the setup wizard |

## 1. Unlock the Echo (once per Echo)

[firebreak](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md#unlocking-with-firebreak) runs R0rt1z2's amonet
unlock from one command in about ten minutes. On the Dot 2nd gen either
target works (`unlock fireos6`, the default, or `unlock fireos5`); the
[rooting guide](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md) (EchoMuse's) explains the choice. **You do this at your own
risk.**

firebreak and the rooting guide are EchoMuse's, credited and linked here.
**Questions about EchoHome, the setup tool or anything after the unlock go to
[EchoHome's issues](https://github.com/ggg1psyyy/EchoHome/issues)**, not to
EchoMuse.

## 2. Install the setup tool

<details open>
<summary><b>As a Home Assistant add-on</b></summary>

Settings → Apps → **Install App** → ⋮ → **Repositories** → **Add**, paste
`https://github.com/ggg1psyyy/EchoHome`, then install **EchoHome (Early
Access)**. (The plain **EchoHome** add-on has no EchoHome release yet.)

Its dashboard is a sidebar panel, reached only through Home Assistant. The
first person to open it is its admin; anyone after that is read-only until
promoted (Settings → Users). Options that would go in `.env` are on its
Configuration tab; leave `server_ip` empty unless the wrong address is
picked.

**Coming from EchoMuse**: copy the old controller's `data/tls/` (all four
files) into the add-on's data first, or Echoes that hold its certificate
authority cannot connect. [Migrating](migrate-to-addon.md) has the steps.

</details>

<details>
<summary><b>With Docker</b></summary>

```bash
mkdir echohome && cd echohome
curl -O https://raw.githubusercontent.com/ggg1psyyy/EchoHome/main/controller/docker-compose.deploy.yml
curl -o .env https://raw.githubusercontent.com/ggg1psyyy/EchoHome/main/controller/.env.example
# Early Access: pin the release, since :latest moves only with GA releases.
sed -i 's/echohome-controller:latest/echohome-controller:3.0.0-ea.2/' docker-compose.deploy.yml
docker compose -f docker-compose.deploy.yml up -d
```

Open `http://<this computer>:8768` and claim it with the setup token printed
in `docker logs echohome-controller`.

</details>

## 3. Run the setup wizard

**Start with the Echo in TWRP recovery.** Booted normally, the wizard's first
step can stop at "Authenticating ADB…" with nothing happening on the Echo;
from TWRP it goes straight on:

1. Unplug the Echo.
2. Hold **mute** down (**+**, volume up, if firebreak installed amonet
   v2.0.0) and keep holding it. The action button gives fastboot, with a
   green ring, not TWRP.
3. Plug in the USB cable with the button still held.
4. Let go once the ring changes: the Echo is in TWRP, and shows up as
   "Echo" in the browser's USB picker.

**Echo on another network (VLAN) than the setup tool?** Set the setup
tool's address first: Dashboard → **Config** → **Advanced** → **Controller
address**, the setup tool host's IP with ports 8767 and 8770 (the field
starts with them). The wizard writes the list onto the Echo, which then dials
the setup tool directly: mDNS, which finds it otherwise, does not cross
VLANs. Let the Echo's network reach the setup tool's host on TCP 8767 and
8770, and Home Assistant reach the Echo on TCP 6053 (ESPHome) and 6055 (the
Voice Incubator app), and Music Assistant on 8928 for Sendspin.

Already provisioned without it, and the Echo never registers? Set the
address, then run the wizard again from TWRP. The Echo keeps its WiFi, so
press **Skip** at **Configure WiFi**.

Then Dashboard → **Provision Device**, with the Echo on USB. The wizard
installs emOS and the firmware, keeps a copy of the original boot image, and
joins the WiFi. If a step fails it says why and offers **Download
diagnostics**; the cable is the Echo's only power, so unplugging reboots it,
and **Reconnect** is on every step.

The Echo then finds the setup tool on its own. Approve it in the dashboard
(**Approve & Add to Fleet**): nothing joins without you.

## 4. Add it to Home Assistant and the app

1. **Settings → Devices & Services**: the Echo is under **Discovered** as an
   ESPHome device, named as on the dashboard. Press **Add**; Home Assistant
   sets its encryption key itself. The Echo announces itself on its own
   network, and the setup tool announces it on its network too, so it is
   found across VLANs as long as the setup tool runs and Home Assistant can
   reach the Echo on port 6053. Not listed? Add **ESPHome** by hand with the
   Echo's address and port 6053.

   **Give each Echo a DHCP reservation in your router** if Home Assistant is
   on another network (VLAN) than the Echo, as for any ESPHome device there:
   mDNS does not cross VLANs, so a changed address would not reach Home
   Assistant. The Echo reports its WiFi chip's own MAC, the one in your
   router's leases, so the reservation and Home Assistant name the same
   device. It also gives DHCP a hostname, its label plus the last four
   characters of its serial (`living-room-00lq`). Where your router puts
   DHCP names in its DNS, use that name instead of an address (in Home
   Assistant's ESPHome entry, and Music Assistant's Sendspin provider), and
   no reservation is needed.
2. In the **Voice Incubator app**, add the Echo as a satellite; it is
   announced on the network as `echohome-<serial>`. Choose its wake words on
   the app's *Wake words* page.

Say a wake word, or tap the action button, and ask.

## Afterwards

- **The setup tool can be stopped.** The Echo keeps its configuration, its
  satellite and its Home Assistant settings, and starts them at boot. Start
  the tool again to update firmware or emOS, change WiFi, or provision
  another Echo.
- **Settings on the Echo** are its own, in Home Assistant: the LED ring,
  Software Mute, the red ring while muted, the volume arc, the Bluetooth
  proxy, live listening and the wake word microphone.
- **Updates**: the dashboard shows an update badge with the release notes;
  an update that fails to boot rolls back by itself.
- **Something wrong?** The dashboard's **Support** tab downloads a
  [support bundle](support-bundle.md) to attach to an issue: versions,
  device state and logs, with network and account names removed.
  [Voice Incubator](voice-incubator.md#when-it-doesnt-answer) covers an Echo
  that does not answer.
