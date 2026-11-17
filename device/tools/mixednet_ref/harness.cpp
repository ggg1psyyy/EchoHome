// Reference harness: runs the Voice Incubator firmware's own mixednet
// executor (fetched by build.sh at a pinned commit, never vendored) over a
// blob and feature frames, so the Go port's vectors come from upstream's
// code and never from the port itself.
//
//   harness <blob> <features>      features: int8 frames, input_frames x
//                                  feature_size per invoke
// Prints one line per invoke: "<probability 0-255> <logit> <arena hash>",
// the hash being FNV-1a 32 over every working buffer in order after the
// invoke, so a one-step difference anywhere in the net shows, not only one
// that reaches the logit. On a blob the parser refuses, prints
// "refused: <reason>" and exits 3. Built with MIXEDNET_DEBUG for peek().
#include "mixednet.h"
#include "mixednet_blob.h"

#include <cstdio>
#include <cstdlib>
#include <vector>

static std::vector<uint8_t> slurp(const char *path) {
  std::FILE *f = std::fopen(path, "rb");
  if (f == nullptr) { std::perror(path); std::exit(2); }
  std::vector<uint8_t> out;
  int c;
  while ((c = std::fgetc(f)) != EOF) out.push_back(static_cast<uint8_t>(c));
  std::fclose(f);
  return out;
}

int main(int argc, char **argv) {
  using namespace esphome::voice_incubator;
  if (argc != 3) { std::fprintf(stderr, "usage: harness <blob> <features>\n"); return 2; }
  std::vector<uint8_t> raw = slurp(argv[1]);
  // The parser needs a 4-byte aligned blob; copy into int32 storage.
  std::vector<uint32_t> aligned((raw.size() + 3) / 4);
  std::copy(raw.begin(), raw.end(), reinterpret_cast<uint8_t *>(aligned.data()));
  MixedNetBlob blob;
  if (!blob.parse(reinterpret_cast<const uint8_t *>(aligned.data()), raw.size())) {
    std::printf("refused: %s\n", blob.error());
    return 3;
  }
  MixedNetExecutor exec;
  if (!exec.setup(blob.model())) { std::fprintf(stderr, "setup failed\n"); return 2; }
  std::vector<uint8_t> feats = slurp(argv[2]);
  const size_t step = static_cast<size_t>(blob.input_frames()) * blob.feature_size();
  for (size_t off = 0; off + step <= feats.size(); off += step) {
    const uint8_t p = exec.invoke(reinterpret_cast<const int8_t *>(feats.data() + off));
    uint32_t h = 2166136261u;
    const MixedNetModel *m = blob.model();
    for (uint8_t b = 0; b < m->num_buffers; b++) {
      const int8_t *buf = exec.peek(b);
      const size_t n = static_cast<size_t>(m->buffers[b].frames) * m->buffers[b].channels;
      for (size_t i = 0; i < n; i++) { h ^= static_cast<uint8_t>(buf[i]); h *= 16777619u; }
    }
    std::printf("%u %d %08x\n", p, exec.last_logit(), h);
  }
  return 0;
}
