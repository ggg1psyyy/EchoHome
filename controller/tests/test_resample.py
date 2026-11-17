"""em_resample: 16 kHz → 48 kHz for the Voice Incubator's replies, judged
on the signal (pitch kept, level kept, images removed), not on the code."""

import numpy as np
import pytest

import em_resample as rs


def _tone(hz, seconds=0.5, amp=0.5, rate=rs.IN_RATE):
    t = np.arange(int(seconds * rate)) / rate
    return (np.sin(2 * np.pi * hz * t) * amp * 32767).astype("<i2")


def _spectrum_db(pcm_bytes, rate):
    y = np.frombuffer(pcm_bytes, dtype="<i2").astype(np.float64)
    y = y[len(y) // 4:]  # past the start
    win = np.hanning(len(y))
    mag = np.abs(np.fft.rfft(y * win))
    freqs = np.fft.rfftfreq(len(y), 1 / rate)
    return freqs, 20 * np.log10(mag / mag.max() + 1e-12)


def _level_db(pcm_bytes):
    y = np.frombuffer(pcm_bytes, dtype="<i2").astype(np.float64)
    y = y[len(y) // 4: -len(y) // 4]
    return 20 * np.log10(np.sqrt(np.mean(y ** 2)) / 32768)


def test_three_output_samples_per_input_sample():
    up = rs.Upsampler()
    assert len(up.process(b"\x00\x00" * 160)) == 160 * 2 * 3


@pytest.mark.parametrize("hz", [200, 1000, 3000, 6000])
def test_pitch_is_kept(hz):
    out = rs.Upsampler().process(_tone(hz).tobytes())
    freqs, db = _spectrum_db(out, rs.OUT_RATE)
    assert abs(freqs[np.argmax(db)] - hz) < 5


@pytest.mark.parametrize("hz", [200, 1000, 3000, 6000])
def test_level_is_kept_in_the_speech_band(hz):
    pcm = _tone(hz).tobytes()
    assert abs(_level_db(rs.Upsampler().process(pcm)) - _level_db(pcm)) < 0.5


@pytest.mark.parametrize("hz", [1000, 3000, 6000])
def test_the_images_are_removed(hz):
    # 3x zero-stuffing mirrors a tone at f to 16k-f, 16k+f, 32k-f...; a
    # sample-and-hold would play them. They must sit far under the tone.
    out = rs.Upsampler().process(_tone(hz).tobytes())
    freqs, db = _spectrum_db(out, rs.OUT_RATE)
    for image in (16000 - hz, 16000 + hz):
        near = (freqs > image - 50) & (freqs < image + 50)
        assert db[near].max() < -40, (hz, image, db[near].max())


def test_chunked_is_identical_to_one_shot_with_odd_cuts():
    pcm = _tone(440, seconds=0.2).tobytes()
    whole = rs.Upsampler()
    expected = whole.process(pcm) + whole.flush()
    for cut in (1, 3, 7, 160, 999, 3201):
        up = rs.Upsampler()
        got = b"".join(up.process(pcm[i:i + cut]) for i in range(0, len(pcm), cut)) + up.flush()
        assert got == expected, cut


def test_silence_stays_silent():
    up = rs.Upsampler()
    assert set(up.process(b"\x00\x00" * 500) + up.flush()) == {0}


def test_full_scale_does_not_wrap():
    square = np.where(np.arange(1600) % 40 < 20, 32767, -32768).astype("<i2").tobytes()
    out = np.frombuffer(rs.Upsampler().process(square), dtype="<i2")
    # Overshoot is clipped, never wrapped round to the other sign.
    assert out.max() <= 32767 and out.min() >= -32768
    assert not np.any((out[:-1] > 30000) & (out[1:] < -30000))


def test_flush_drains_the_filter_delay():
    up = rs.Upsampler()
    body = up.process(_tone(1000, seconds=0.1).tobytes())
    tail = up.flush()
    assert len(tail) == (rs.TAPS_PER_PHASE // 2) * 2 * 3
    assert len(body) + len(tail) == (1600 + rs.TAPS_PER_PHASE // 2) * 2 * 3


def test_an_empty_or_single_byte_call_returns_nothing_yet():
    up = rs.Upsampler()
    assert up.process(b"") == b""
    assert up.process(b"\x01") == b""
    assert len(up.process(b"\x00")) == 6
