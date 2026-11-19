# Changelog

## 3.0.0-ea.1 (Early Access)

The first EchoHome release: the controller becomes a setup tool. Each Echo is now a Voice Incubator satellite and an ESPHome device of its own, and works with the add-on stopped. Pair with firmware v3.0.0-ea.1, which this controller offers; emOS still comes from EchoMuse.

**The database is unchanged (still v33)**, so going back to an EchoMuse controller keeps your devices and settings. Firmware from before EchoHome cannot be its own satellite and has no voice under this controller until it is updated.

### New

- **Every Echo is its own Voice Incubator satellite**, answering the app on its own wake word, with or without the controller.
- **Each Echo is an ESPHome device in Home Assistant**, added by its address with a key Home Assistant sets: media player, LED ring, mute, action button, ambient light, temperatures, the Bluetooth proxy, and its settings as switches and a select (the wake word microphone).
- **Settings changed in Home Assistant are the Echo's own** and survive reboots with no controller; the dashboard no longer has controls for them.
- **Firmware comes from EchoHome's releases, emOS from EchoMuse's**, each from its own repository (Settings → Release source).

### Removed

- The controller's own voice path: Home Assistant's voice pipeline through the controller, its wake word, barge-in, timers, announcements and media player, the emulated ESPHome satellites and Bluetooth proxy, the Activity tab and its recordings, and the custom wake word models and their trainer. Their settings are retired and no longer sent to an Echo.
- The add-on options for the wake word and the satellite ports.
## 2.27.0

EchoMuse's last release, which this add-on still names until EchoHome's first
general release. EchoHome starts at 3.0.0, in Early Access (the **EchoHome
(Early Access)** add-on), and publishes no image for 2.27.0.

EchoHome is built on [EchoMuse](https://github.com/wilbowes/EchoMuse) by Wil Bowes and the EchoMuse
contributors. The notes for 2.27.0 and every release before it are
EchoMuse's: [https://github.com/wilbowes/EchoMuse/blob/main/controller/CHANGELOG.md](https://github.com/wilbowes/EchoMuse/blob/main/controller/CHANGELOG.md).
