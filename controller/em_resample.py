"""
16 kHz → 48 kHz, streamed: the Voice Incubator app's replies (16-bit mono at
16 kHz, its only format) onto the Echo's 48 kHz speaker plane.

An integer factor of 3, so a polyphase FIR: each input sample becomes three
output samples, each the dot product of the recent input with one phase of a
windowed-sinc low-pass. The cut-off sits below the input's 8 kHz Nyquist so
the images 3x upsampling folds up (8-16 kHz and above) are taken out rather
than played -- a plain sample-and-hold would put them straight through the
tweeter. Chunks of any size, odd byte counts included, give exactly the
output one call over the whole stream gives: the filter's history and a
trailing odd byte are carried between calls.

Latency is the filter's half-length, TAPS_PER_PHASE // 2 input samples
(~0.75 ms); `flush()` drains it at the end of a stream.

Pure numpy, no scipy (lazily imported elsewhere for a reason: em_eq), so
the suite exercises it directly.
"""

from __future__ import annotations

import numpy as np

FACTOR = 3
IN_RATE = 16000
OUT_RATE = IN_RATE * FACTOR

TAPS_PER_PHASE = 24
CUTOFF_HZ = 7200.0
"""Pass to ~7 kHz, gone by 8 kHz: speech's useful band is well inside it, and
the first image (8-16 kHz) is attenuated by the transition and stop band."""


def _design() -> np.ndarray:
    """The prototype low-pass at the output rate, gain FACTOR (zero-stuffing
    divides the level by it), split into FACTOR phases of TAPS_PER_PHASE."""
    n = TAPS_PER_PHASE * FACTOR
    t = np.arange(n) - (n - 1) / 2.0
    fc = CUTOFF_HZ / OUT_RATE
    h = 2 * fc * np.sinc(2 * fc * t) * np.kaiser(n, 8.0)
    h *= FACTOR / h.sum()
    # Phase p holds taps p, p+FACTOR, ...; reversed so a dot product with the
    # history (oldest first) is the convolution.
    return np.stack([h[p::FACTOR][::-1] for p in range(FACTOR)])


_PHASES = _design()


class Upsampler:
    """One stream's state. Feed S16LE bytes, get S16LE bytes back."""

    def __init__(self) -> None:
        self._history = np.zeros(TAPS_PER_PHASE - 1, dtype=np.float64)
        self._odd = b""

    def process(self, pcm: bytes) -> bytes:
        data = self._odd + bytes(pcm)
        whole = len(data) - (len(data) % 2)
        self._odd = data[whole:]
        if not whole:
            return b""
        x = np.frombuffer(data[:whole], dtype="<i2").astype(np.float64)
        buf = np.concatenate([self._history, x])
        self._history = buf[-(TAPS_PER_PHASE - 1):]
        # windows[i] is the TAPS_PER_PHASE samples ending at input i.
        windows = np.lib.stride_tricks.sliding_window_view(buf, TAPS_PER_PHASE)
        out = windows @ _PHASES.T  # (len(x), FACTOR): the three outputs per input
        return _to_s16(out.reshape(-1))

    def flush(self) -> bytes:
        """The filter's tail: what the last input samples still owe."""
        tail = self.process(b"\x00\x00" * (TAPS_PER_PHASE // 2))
        self._odd = b""
        return tail


def _to_s16(y: np.ndarray) -> bytes:
    return np.clip(np.rint(y), -32768, 32767).astype("<i2").tobytes()
