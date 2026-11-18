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
