"""Packs a .tflite wake word model into an MNB1 blob with the Voice Incubator
app's own packer (voice_incubator/mixednet.py), so the blob is exactly what
the app would push to a satellite. Used by build.sh; needs numpy and tflite.

    python pack.py <voice-incubator checkout> <model.tflite> <out.blob>

Prints the blob's op mix and requantization shift range, which the Go
parser's limits are checked against.
"""
import collections
import struct
import sys
from pathlib import Path

app, model, out = (Path(a) for a in sys.argv[1:4])
sys.path.insert(0, str(app))
from voice_incubator.mixednet import pack  # noqa: E402

blob = pack(model)
out.write_bytes(blob)

nbuf, nops, nshift = struct.unpack_from("<HHH", blob, 8)
pos = 32 + nbuf * 10
tail = pos + nops * 52 + nshift
data = ((tail + 3) & ~3) + 256
names = ["RING_PUSH", "DENSE", "DEPTHWISE", "COPY", "MAX_POOL"]
kinds = collections.Counter()
shifts = []
for i in range(nops):
    op = blob[pos + i * 52 : pos + (i + 1) * 52]
    kinds[names[op[0]] if op[0] < len(names) else op[0]] += 1
    if op[0] in (1, 2):
        out_ch = struct.unpack_from("<H", op, 16)[0]
        s_off = struct.unpack_from("<I", op, 48)[0]
        shifts += struct.unpack_from(f"<{out_ch}i", blob, data + s_off)
print(f"{out.name}: {len(blob)} bytes, {nbuf} buffers, {nops} ops {dict(kinds)}, "
      f"input_frames={blob[14]} feature_size={blob[16]}, "
      f"shift range {min(shifts)}..{max(shifts)}")
