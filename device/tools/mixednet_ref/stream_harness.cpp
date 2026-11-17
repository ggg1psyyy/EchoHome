// Reference harness for internal/mixednet's Stream: the Voice Incubator
// firmware's own ModelStream, its declaration and methods cut verbatim from
// wake_detector.h and wake_detector.cpp at the pinned commit (both files
// sha256-checked by build.sh) into gen/stream_decl.inc and
// gen/stream_impl.inc, over its own executor.
//
//   stream_harness <blob> <features> <cutoff> <window>
// features: int8 frames of FEATURE_SIZE. Per frame it does what
// WakeDetector::process_features_ does for one enabled, non-VAD model:
// feed; if a fresh inference came, ask detected(), and reset on a detection.
// Prints per frame: "<latest probability> <ignore_windows> <detected>
// <average> <highest>", the last two summarize() taken before the reset, as
// queue_detection_ takes them (0 0 on a frame with no detection).
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <vector>

#include "mixednet.h"
#include "mixednet_blob.h"

namespace esphome::voice_incubator {
static constexpr uint8_t FEATURE_SIZE = 40;                  // wake_detector.h
static constexpr uint8_t MIN_SLICES_BEFORE_DETECTION = 100;  // wake_detector.h
#include "stream_decl.inc"
#include "stream_impl.inc"
}  // namespace esphome::voice_incubator

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
  if (argc != 5) { std::fprintf(stderr, "usage: stream_harness <blob> <features> <cutoff> <window>\n"); return 2; }
  std::vector<uint8_t> raw = slurp(argv[1]);
  std::vector<uint32_t> aligned((raw.size() + 3) / 4);
  std::copy(raw.begin(), raw.end(), reinterpret_cast<uint8_t *>(aligned.data()));
  MixedNetBlob blob;
  if (!blob.parse(reinterpret_cast<const uint8_t *>(aligned.data()), raw.size())) {
    std::printf("refused: %s\n", blob.error());
    return 3;
  }
  const uint8_t cutoff = static_cast<uint8_t>(std::atoi(argv[3]));
  const uint8_t window = static_cast<uint8_t>(std::atoi(argv[4]));
  ModelStream stream;
  if (!stream.setup(blob, window)) { std::fprintf(stderr, "setup failed\n"); return 2; }
  std::vector<uint8_t> feats = slurp(argv[2]);
  for (size_t off = 0; off + FEATURE_SIZE <= feats.size(); off += FEATURE_SIZE) {
    stream.feed(reinterpret_cast<const int8_t *>(feats.data() + off), blob.input_frames(), cutoff, window);
    bool det = false;
    uint8_t average = 0, highest = 0;
    if (stream.fresh) {
      det = stream.detected(cutoff, window);
      if (det) {
        stream.summarize(&average, &highest);
        stream.reset_probabilities();
      }
    }
    std::printf("%u %d %d %u %u\n", stream.recent[stream.last_n_index], stream.ignore_windows, det, average,
                highest);
  }
  return 0;
}
