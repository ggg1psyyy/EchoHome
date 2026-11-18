# EchoHome — Early Access

This is the **Early Access** channel: the next controller release,
before it is general. It is the same program as the stable add-on
and the same settings; only the version differs.

**Install this instead of the stable add-on, not alongside it.**
Both use host networking and the same ports (8767, 8768, 8770), so
whichever starts second will fail to bind.

**Switching channels is a migration, not a toggle.** Home Assistant
add-ons do not share storage, so this starts with an empty database
and a newly generated certificate authority. Existing devices hold
the stable add-on's CA, and they take `wss` from the mDNS record
rather than from `require_device_tls` — so they will fail
verification and will not connect at all until you copy the stable
add-on's `/data` across, including all four files in `tls/`.

Report anything you find against the EchoHome repository, saying
which channel you are on.

---

# EchoHome

The EchoHome setup tool: it provisions rooted Amazon Echoes over USB, pairs
them, pushes their configuration and updates their firmware and emOS. Each
Echo is then a Voice Incubator satellite and an ESPHome device of its own,
and keeps working with this add-on stopped.

## Installation

1. Install the add-on and start it. **Controller LAN IP address** can be
   left empty: the address Echoes are told to dial is detected from this
   host and logged at startup. Set it only if this machine has more than one
   network interface.
2. Open the dashboard from **Open Web UI**. You are signed in as your Home
   Assistant user. The first person to open it is the admin; anyone after
   that is read-only until an admin promotes them.
3. Use the dashboard's **Provision Device** wizard (USB, Chrome or Edge) to
   set up a rooted Echo. It finds this add-on by itself.
4. Approve the Echo once it appears as pending.
5. Add the Echo in Home Assistant (**ESPHome**, the Echo's address, port
   6053) and in the Voice Incubator app as a satellite.

The add-on can be stopped afterwards; start it again for updates, a WiFi
change or another Echo.

### The provisioning wizard needs a secure browser context

WebUSB, which the wizard uses to talk to the Echo over the cable, is only
available on a secure origin. If you reach Home Assistant over plain
`http://`, the wizard's first step says so and names the exact origin to
allow. Either serve Home Assistant over HTTPS, or add that origin to
`chrome://flags/#unsafely-treat-insecure-origin-as-secure` and relaunch the
browser. The allowlist matches scheme, host **and port** exactly.

### Privacy

This add-on hears nothing of the room: an Echo's calls go to the Voice
Incubator app, and what the dashboard says about listening is what the Echo
reports.

## Configuration

Every option is explained on the Configuration tab; restart the add-on after
changing one. More in the project's docs:

- [Quickstart](https://github.com/ggg1psyyy/EchoHome/blob/main/docs/quickstart.md)
- [Configuration](https://github.com/ggg1psyyy/EchoHome/blob/main/docs/configuration.md)
- [Rooting a device](https://github.com/ggg1psyyy/EchoHome/blob/main/docs/rooting.md)
