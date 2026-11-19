"""
Which userspace a device booted on, and what follows from it.

EchoHome supports emOS only (2026-11-19). The device reports its base as
`base_os` on REGISTER (device/internal/platform): `emos`, `fireos`, or
`unknown`. A positive `fireos` is shown and not served (`unsupported`); an
absent or unknown base keeps today's behaviour, since firmware too old to say
must not lose a control. emOS still mounts the FireOS system partition, so
"FireOS" also names the kernel and donor userspace beneath emOS elsewhere.

Pure and dependency-free: the rules are one comparison each and getting them
backwards is silent, so they belong somewhere a test can reach without
aiohttp.
"""

# The three answers the device can give. Mirrored from
# device/internal/platform/platform.go and pinned by test — a typo here makes
# every device look like Android forever, which is exactly the failure the
# capability-string mirroring exists to prevent.
EMOS = "emos"
FIREOS = "fireos"
UNKNOWN = "unknown"

# The wire key on the REGISTER message. Snake case, matching its neighbours
# there (`ambient_light_status`, `firmware_ver`) rather than the camelCase of
# the stats report it briefly rode on.
REGISTER_KEY = "base_os"


def android_userspace(base_os) -> bool:
    """
    Whether this device may be on Android (anything but a positive emOS).

    True for everything except a device that has POSITIVELY said `emos`.
    Firmware too old to report the field, a device that has not sent stats
    yet, and any value we do not recognise all keep today's behaviour.

    That asymmetry is deliberate: an unrecognised device keeps the behaviour
    it had, and only a positive emOS gets the emOS-only steps (the crash log,
    the emOS update), which would act on paths no other base has.
    """
    return base_os != EMOS


# Why a FireOS Echo is shown but not served (2026-11-19): EchoHome supports
# emOS only. Its wizard installs emOS; FireOS was EchoMuse's other base.
UNSUPPORTED_REASON = ("FireOS is not supported by EchoHome: reprovision this "
                      "Echo with emOS (the setup wizard).")


def unsupported(base_os):
    """
    Why this Echo is not served, or None.

    Only a POSITIVE `fireos` is unsupported: firmware too old to report its
    base, or a value we do not know, keeps today's behaviour, for the reason
    android_userspace gives (degrade to old behaviour, and an absent field
    must not disable a control). Such an Echo is shown, named FireOS, and
    refused firmware updates and payload pushes, which assume emOS's layout.
    """
    return UNSUPPORTED_REASON if base_os == FIREOS else None


def label(base_os) -> str:
    """
    How to name the base in something a person reads.

    None becomes "unknown" rather than an empty string or "FireOS": a device
    whose firmware predates the field has not told us it is on Android, and a
    panel that says FireOS anyway is stating a fact nobody established.
    """
    if base_os == EMOS:
        return "emOS"
    if base_os == FIREOS:
        return "FireOS"
    return "unknown"
