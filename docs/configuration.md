# Configuration

What you can set, and where. The Echo's own settings live in Home Assistant;
the rest are pushed by the setup tool, which the Echo keeps and applies at
boot, so they hold with the setup tool stopped. Every key, its default and
whether the Echo applies it: [config-keys.md](config-keys.md).

## The Echo's own settings (Home Assistant)

On the Echo's ESPHome device page. Each is kept **on the Echo** and survives
reboots with no controller; the dashboard has no control for them.

| Entity | What it does |
|---|---|
| **LED Ring** (light) | The ring's colour and brightness between calls. |
| **Software Mute** (switch) | Zeroes the microphones at capture, the button's light untouched. Refused while the button's mute is on; any press of the button clears it. |
| **Red ring while muted** (switch) | Off leaves the ring dark while muted, the lit mute button alone showing it (Echoes whose button lights itself). |
| **Volume arc for remote changes** (switch) | Shows the cyan level arc for volume changes from Home Assistant, Music Assistant or an automation. Physical buttons always show it; zero, a repeated level and the boot restore never do. |
| **Voice Incubator live listening** (switch) | Lets the Voice Incubator app's Test page listen outside a call. Off by default; the ring pulses red while it listens. |
| **Bluetooth proxy** (switch) | A passive ESPHome Bluetooth proxy (advertisements only, no connections). Scanning costs some of the Echo's WiFi; the scan stands aside during calls. |
| **Wake word microphone** (select) | Which microphone the satellite hears on: the default (the centre one on a Dot 2), or one of the edge microphones, for an Echo whose centre microphone has failed. |

An Echo updated from firmware before EchoHome starts with these at their
defaults, except the wake word microphone, which carries over from the
dashboard. Which wake words it listens for is set in the Voice Incubator
app.

## Where settings live (the dashboard)

- **Fleet config** (gear icon → Fleet Config): the defaults every Echo uses.
- **Per-device config** (device page → Config tab): each section has its
  own **Fleet / Device** switch.

Scoping is **per section**. A section on *Fleet* follows the fleet's value,
including future changes, and is shown read-only; on *Device* it is this
Echo's own. Flipping a section back to *Fleet* discards the values it held.
Changes apply at once. The Config tab opens with the Echo's **WiFi**, always
per device, then **Playback**, **Microphones**, **Ring**, **Advanced** and
**Sendspin**.

The device's **Status** tab shows its address, firmware, WiFi, volume,
**Latency** (amber past 200 ms, red past 1 s), **Temp**, the CPU (as a share
of the cores awake, "27% · 2/4 cores") and **Microphone**: what the Echo
reports it does with it. Hourly hardware trends are kept for 180 days
(`/api/devices/{id}/activity?days=N`).

## Playback

How the Echo's speaker sounds. The output chain runs on the Echo.

### Equalizer (8 faders + presets)
Shapes the tone of everything the speaker plays. On firmware with the new
sound, the Echo already corrects its own speaker, so **Flat** is the
recommended setting and the presets lean it one way:

- **Flat**: the Echo's own voicing, no extra shaping.
- **Warm**: more body, softer top.
- **Clear**: leaner low end, more presence.
- Drag any fader for a custom curve.

Upgrading resets the old **Speech** and **Music** presets to Flat, because
on the new sound they would boost the bass twice. A custom curve is kept.

### Response level
The Voice Incubator app's replies relative to the Echo's volume: **Low**
keeps the level, **Medium** adds 6 dB, **High** 12 dB. The boost tapers away
near maximum volume, so it never drives the output past full scale.

### Speaker protection
Keeps bass the driver cannot deliver from muddying everything above it. Leave
it on.

It sounds backwards, and it is the right answer for a speaker this small.
Frequencies below about 115Hz still move the cone even though you cannot hear
them, and that movement smears the midrange — which is what people usually
describe as thin, boxy or "tin-can". Removing them makes the middle clearer,
and on loud material slightly *louder*, because the limiter no longer has to
hold everything down to contain bass peaks you were never going to hear.

Quiet passages keep their low end. Only loud content is affected, which is why
it is a guard rather than a filter.

This one switch covers two stages: the bass guard described above, and a
limiter that stops the equalizer distorting what it boosts. Turning any EQ
band up can push the audio past the maximum the hardware can represent, and
without a limiter that gets clipped — measured at nearly 5% of samples on an
ordinary response with a modest bass boost, audible as harshness or crackle,
and it only ever happened to people who touched the EQ to improve their sound.

**It used to be five controls and now it is one, deliberately.** None of the
five could be judged by ear. The two stages cancel out each other's most
obvious effect — at a flat EQ, switching the guard on is a 7.7dB change in
overall level, and with every band at +12dB it is 0.2dB, because the limiter
simply gives back what the guard takes. The depth slider moved the overall
level by 0.14dB across its entire range. Controls whose effect ranges from
"large" to "nothing" depending on where the others sit are not a tuning
surface; they are a way to conclude the feature is broken, which is what kept
happening.

The individual values still exist and still apply — if you had tuned the
ceiling, release or depth, your settings are unchanged. They are reachable
through the API for anyone who wants them, just not on the dashboard.

The crossover frequency and the shape of the curve come from measurements of
Amazon's own firmware on this same speaker, so they are not guesses. Our
default depth is gentler than Amazon's, because theirs sits in front of an
equalizer curve we have not yet measured — see issue #247.

### Duck depth
How far music drops under a call, a reply, a ringing timer or an
announcement. Music keeps playing underneath.

### Volume
The volume is the Echo's own state, kept across reboots and shown read-only
on the Status tab; change it with the buttons, Home Assistant or Music
Assistant. The buttons step 1 dB per press from barely audible to the
codec's unity gain (never above it), and Home Assistant's percentage and
Sendspin's map onto the same steps.

**Volume button sound** plays a short beep at the new level after a button
press while the speaker is idle, and again for Volume Up at maximum.

Mute is the Echo's too: a muted Echo stays muted through reboots, power cuts
and updates, and nothing remote lifts it.

## Microphones

- **MICPGA / Digital gain**: the audio chips' hardware gain, at Amazon's
  factory values. Leave them alone; a wrong value distorts every microphone.
- **Mic gain (dB)**: software gain on the raw 24-bit signal, default 24 dB.
  Lower it if the device log reports clipping.
- **Echo cancel (AEC)**: subtracts the Echo's own playback from what the
  microphone hears, so it hears you over music and its own replies. On by
  default. **AEC delay**: leave it at 0, the measured value for this
  hardware. **AEC tail**: the room echo modelled, default 300 ms. **AEC
  reference**: where the far-end signal comes from; *auto* uses the board's
  hardware loopback where it has one.
- **Wake word microphone**: on EchoHome firmware this only seeds the Echo's
  own select in Home Assistant, which then decides; firmware before EchoHome
  reads it directly.

## Ring

The ring scenes the satellite paints its states with: listening, the
spinner while the app works, the meter while it answers.

- **Standard** (green), **Airy** (pale blue), **Malevolent** (crimson with an
  ember spinner), **Pride** (a rotating rainbow), or **Custom** with your own
  **Listening** and **Thinking** colours.

The red mute ring and the cyan volume arc are the same in every scene.

### Meter response (Advanced)
While a response plays the ring throbs with the live speaker level. The
**Advanced** panel here shapes how hard it throbs — the device renders it
locally, so changes apply on the next response with no restart:

- **Decay** — how fast it falls. Higher tracks individual syllables; lower
  reads as a slow swell.
- **Attack** — how fast it rises on a peak.
- **Gamma** — contrast. Higher makes the swing more visible.
- **Floor** — brightness during silence. `0` goes fully dark between words.
- **Reference** — the speaker level mapped to full brightness. Lower is more
  sensitive.
- **Curve** — below `1` lifts quiet consonants into view.

These are taste settings, which is exactly why they're adjustable here
rather than baked into firmware. The defaults are tuned for speech; if the
ring looks too static, raise **Decay** and **Gamma** first.

## Advanced

- **emOS console password** and **idle timeout**: guard the USB console of
  an Echo on emOS. A nod to security, not a lock: anyone with the Echo in
  hand can reflash it.
- **Controller endpoints** (fleet only): see below.

### Static controller endpoint
Devices normally find the controller with link-local mDNS. If a device
reaches the controller through a routed tunnel or an isolated VLAN where mDNS
cannot cross, list the controller's address under **Config → Advanced →
Controller address**. It applies to the whole fleet, with no per-device
override:

- Each entry is an IP address or a host name, with the device port and the
  encrypted (TLS) port. They start as this controller's own; a TLS port of `0`
  means that address has no encrypted listener.
- Echos try the addresses in order, twice each, then look for the controller
  by mDNS once before starting again. A wrong address therefore slows a
  reconnect down; it cannot leave an Echo unable to find its controller.
- Saving writes the list to every connected Echo straight away; the rest get
  it when they next connect. An Echo uses it from its next reconnect, with no
  restart. The provisioning wizard writes it to a new Echo as it stands when
  the wizard runs.
- Needs firmware v2.16.0 or later. Older firmware ignores it and uses mDNS.

The list lands on the device as `/data/local/etc/echomuse/controller.json`,
which the firmware re-reads on every reconnect attempt. You can still write
that file by hand, for example to turn the mDNS fallback off for a test fleet
that must never reach another controller:

```json
{
  "endpoints": [
    {"host": "10.20.40.110", "port": 8767, "tls_port": 8770},
    {"host": "controller.example.internal", "port": 8767, "tls_port": 8770}
  ],
  "mdns": false
}
```

The dashboard list wins. While it is empty the controller leaves a
hand-written file alone; once you set a list, it replaces the file on every
Echo, including any hand edits. The provisioning wizard goes further: it
writes the list as set, and with none set it removes any `controller.json`
the device already has, so a device from a previous setup cannot carry
another controller's addresses.

## Sendspin (Early Access)
**Sendspin player** makes the Echo a [Sendspin](https://github.com/Sendspin/spec)
player, so Music Assistant can put it in a group with other speakers and play
to all of them in sync. Music Assistant connects to the Echo directly; the
controller only turns the player on and shows its status.

To set it up:

1. Turn on **Sendspin player** for the Echo. Music Assistant finds it on the
   network within a few seconds, listed under the Echo's name.
2. Press **Show pairing token** under the switch, then **Copy**.
3. In Music Assistant, open the player and pair it with the token.

The token pairs the Echo with a server, so treat it like a password. Anyone
who has it can pair with that Echo. Pairing is remembered on both sides, so it
is a one-time step per Music Assistant install.

**Play without pairing** skips the token: any server that Music Assistant's
operator approves can play. It is off by default, because without pairing the
Echo cannot tell a real server from anything else on your network claiming to
be one.

Things to know:

- **Music from Home Assistant wins.** Asking the Echo for music by voice, or
  playing to its Home Assistant media player, takes the speaker. The Echo
  leaves its Sendspin group, and Music Assistant does not put it back by
  itself. Start the group again from Music Assistant.
- **Voice still ducks the music.** A wake word lowers synced music exactly as
  it lowers other music, and the other speakers in the group carry on.
- **One volume.** Music Assistant's volume slider for the Echo is the Echo's
  own volume: moving it moves Home Assistant's slider too, and the Echo's
  buttons move both. Music Assistant's mute silences only the synced music;
  the Echo's mute button still mutes the microphone.
- **It needs the controller.** When the Echo loses its controller (the ring
  pulses orange) it stops playing and leaves its group, and Music Assistant
  reconnects it once the link is back.
- **Mono.** The Echo asks for one channel, which is what its speaker plays.
- The player listens on port **8928** and advertises itself over mDNS as
  `_sendspin._tcp`. The connection is encrypted.
- Decoding and decryption cost about 4% of one CPU core while playing,
  measured on a Dot.

## WiFi (device page → Config tab, top section)
Move a device to a different WiFi network without touching ADB. The section
at the top of the Config tab shows the current network, signal, and IP, lets
you scan for visible networks, and switches with a confirmation step.

The switch is designed to be **unbrickable**: the device applies the change
itself and must pass three checks — join the network, get an IP, and
**reconnect to this controller** — before the change is kept. Fail any of
them (wrong passphrase, DHCP trouble, or a network that works but can't
reach the controller, like an isolated guest VLAN) and it automatically
restores the previous network and tells you why. Even a power cut
mid-switch recovers: an unconfirmed change is rolled back on boot. Allow
about two minutes for the device to drop off and come back.

## Setup tool settings (add-on options or `.env`)

Set once, and a restart applies them. On the add-on they are the
Configuration tab's options; in a container, `.env`
([`.env.example`](../controller/.env.example) has them all with comments).

| Setting | What it is |
|---|---|
| `SERVER_IP` | The address Echoes are told to dial. Empty detects it from this host; the tool refuses to start rather than advertise a guess, and warns about a container bridge address. mDNS is advertised on that interface only. |
| `DB_PATH` | The database; the certificates sit beside it. The image defaults to `/app/data/echomuse.db`. |
| `DEVICE_APPROVAL` | `strict` (you approve every new Echo, recommended) or `auto`. |
| `SERVER_TLS_PORT` | The encrypted device link's port, default 8770. Do not set `0` once Echoes are paired: an Echo holding credentials never falls back to plain. |
| `REQUIRE_DEVICE_TLS` | `1` only after every Echo shows `wss (TLS)` on its Status tab; then unencrypted or tokenless connections are refused. |
| `EM_EXTRA_CA_CERT` | A PEM CA certificate to trust, for a sign-in provider served with your own certificate authority. |
| `DEBUG`, `LOG_LEVELS` | Log detail. |

### Single sign-on (OpenID Connect)
The dashboard can sign people in through an OpenID Connect provider such as
Authentik, Keycloak or Authelia, instead of (or alongside) its own
passwords. Roles follow group membership at the provider, and an account is
created the first time someone signs in.

Set these on the controller, and restart it:

| Setting | What it is |
|---|---|
| `OIDC_ISSUER` | The provider's issuer URL, which has a `/.well-known/openid-configuration` beneath it. Must be `https` (plain `http` only for `localhost`). Setting it turns single sign-on on. |
| `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | The client you registered for EchoMuse. Register it as a confidential client using the authorization-code flow. |
| `OIDC_REDIRECT_URI` | `https://<where you open the dashboard>/api/auth/oidc/callback`. Register exactly this at the provider. |
| `OIDC_ADMIN_GROUPS` | Comma-separated group names whose members become **admin**. |
| `OIDC_READONLY_GROUPS` | Comma-separated group names whose members become **read-only**. |
| `OIDC_GROUPS_CLAIM` | The ID token claim that lists a person's groups. Default `groups`. |
| `OIDC_SCOPES` | Default `openid profile email groups`. |
| `OIDC_PASSWORD_LOGIN` | `false` removes the password form and refuses password sign-in. Default `true`. |
| `OIDC_SESSION_HOURS` | How long a single sign-on session lasts. Default 12. A change to someone's groups takes effect when their session next signs in, so this is the longest a removed person keeps access. |
| `OIDC_BUTTON_LABEL` | The text on the sign-in button. Default "Sign in with SSO". |

What to know before turning it on:

- **A person in none of the listed groups cannot sign in.** There is no
  default role. Removing someone from the group at the provider is how you
  remove their access.
- **An admin group wins** over a read-only group when someone is in both.
- **The role is set by the provider at every sign-in.** It cannot be edited
  on the Users tab for these accounts, which are marked "SSO".
- **Accounts are tied to the provider's stable user id**, not the name, so a
  rename at the provider does not create a second account.
- **Keep a local admin.** Leave `OIDC_PASSWORD_LOGIN` on, or at least keep one
  password account, until you have signed in by single sign-on and seen it
  work. If the provider is down, that account is the way back in, but only
  while `OIDC_PASSWORD_LOGIN` is on. With it `false`, the way back is to
  unset it (or `OIDC_ISSUER`) and restart the controller.
- **Start sign-in from the address in `OIDC_REDIRECT_URI`.** The browser is
  sent back to exactly that host, and the cookie that ties the return trip to
  the sign-in belongs to the host you started from. Starting from a different
  address (the controller's bare LAN IP, say) ends in "That sign-in expired".
- A half-set configuration is logged and ignored, so a typo leaves you with
  the password form rather than locked out.

### Encrypted device link
The controller generates its own certificate authority on first start
(stored in `tls/` next to the database) and listens for encrypted device
connections alongside the plain ones. Each device gets two credentials —
the CA certificate and a private token — installed automatically by the
provisioning wizard, or pushed to an existing device with the **Secure
link** button on its Status tab. A device with credentials connects
encrypted from its next reconnect; the Status tab's **Link** row shows
which mode each device is using. Once the whole fleet shows `wss (TLS)`,
set `REQUIRE_DEVICE_TLS=1` to lock out unencrypted connections entirely.

## What leaves your network

EchoHome has **no telemetry**: no usage reporting, analytics, crash reports
or install counter.

### The one outbound connection
The controller contacts `api.github.com` once an hour to ask what the newest
release is, so the dashboard can tell you an update is available and show its
notes. When you choose to update a device, the firmware binary is downloaded
from `github.com` at that moment.

That is the whole of it. The request carries no identifiers — it is an
ordinary unauthenticated API call — but like any request it does reveal your
public IP to GitHub, the same exposure as a `git clone` or opening the repo
in a browser.

Set `update_check_interval` (seconds, default `3600`) in the system config to
change how often it runs. A long interval, say `86400`, reduces it to once a
day, and `0` turns it off entirely — the controller then makes no outbound
connection at all, and the dashboard shows whatever it last knew about
releases rather than nothing.

Re-enabling takes effect without a restart. Turning it off does not disable
the **Check now** button on the Updates tab: that is a deliberate request, so
it still asks GitHub when you press it.

### What never leaves

- **Anything the Echo hears.** The setup tool receives no audio. An Echo's
  calls go from the Echo to the Voice Incubator app on your network; where
  the app sends them is the app's and your pipeline's
  ([listening.md](listening.md)).
- **Device serials, WiFi credentials, network names and your fleet's
  configuration**, which live in the setup tool's database.
- **Support bundles** are built only when you ask, and exclude network and
  account names ([support-bundle.md](support-bundle.md)).
