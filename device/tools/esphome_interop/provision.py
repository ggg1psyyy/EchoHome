"""Sets the key of tools/esphome_interop's server (run with `provision`) the
way Home Assistant's ESPHome integration does (homeassistant/components/
esphome/manager.py, 2026.11): connect with no key, read
api_encryption_provisionable, connect with the all-zero key and send
NoiseEncryptionSetKeyRequest; then only the new key gets in. Last, clear it
(an entry removed) and plaintext is back.

    python provision.py [port]      (aioesphomeapi as HA 2026.11 pins: 46.25.0)
"""
import asyncio
import base64
import secrets
import sys

from aioesphomeapi import APIClient

ZERO = base64.b64encode(bytes(32)).decode()


async def connect(port, key):
    cli = APIClient("127.0.0.1", port, password="", noise_psk=key)
    await cli.connect(login=True)
    return cli


async def refused(port, key):
    try:
        cli = await connect(port, key)
    except Exception as e:
        return type(e).__name__
    await cli.disconnect()
    return "connected"


async def main(port):
    cli = await connect(port, None)
    info = await cli.device_info()
    assert info.api_encryption_supported and info.api_encryption_provisionable, info
    await cli.disconnect()
    print("no key: supported and provisionable")

    new_key = base64.b64encode(secrets.token_bytes(32))
    cli = await connect(port, ZERO)
    assert await cli.noise_encryption_set_key(new_key) is True
    await asyncio.sleep(0.5)
    await cli.disconnect()
    print("set over the all-zero key")

    assert await refused(port, None) == "RequiresEncryptionAPIError"
    assert await refused(port, ZERO) == "InvalidEncryptionKeyAPIError"
    cli = await connect(port, new_key.decode())
    info = await cli.device_info()
    assert info.api_encryption_supported and not info.api_encryption_provisionable, info
    print("after: no key and the zero key refused, the new key in, not provisionable")

    assert await cli.noise_encryption_set_key(b"") is True
    await asyncio.sleep(0.5)
    await cli.disconnect()
    cli = await connect(port, None)
    info = await cli.device_info()
    assert info.api_encryption_provisionable, info
    await cli.disconnect()
    print("cleared: plaintext and provisionable again")
    print("ok")


asyncio.run(main(int(sys.argv[1]) if len(sys.argv) > 1 else 16053))
