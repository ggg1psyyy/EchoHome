// Reference harness for internal/microfeatures: the same vendored front end
// C, configured and scaled by the Voice Incubator firmware's own lines.
// build.sh cuts them verbatim from wake_detector.cpp at the pinned commit
// (sha256-checked) into gen/config.inc and gen/scale.inc, so neither is a
// re-reading of that code.
//
//   features_harness <pcm s16le 16 kHz> <step ms> <chunk samples>
// Feeds the audio chunk by chunk, as the detector's read loop does, and
// prints one line per feature frame: FEATURE_SIZE int8 values.
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <vector>

#include "frontend.h"
#include "frontend_util.h"

using std::clamp;  // esphome::clamp, as wake_detector.cpp calls it

static constexpr uint8_t FEATURE_SIZE = 40;         // wake_detector.h
static constexpr uint8_t FEATURE_DURATION_MS = 30;  // wake_detector.h
static constexpr uint32_t SAMPLE_RATE_HZ = 16000;   // wake_detector.cpp

struct Detector {
  struct FrontendConfig frontend_config_ {};  // as wake_detector.h declares it
  uint8_t feature_step_size_{10};

  void configure() {
#include "config.inc"
  }

  static void generate(const FrontendOutput &output, int8_t *features) {
    for (size_t i = 0; i < output.size && i < FEATURE_SIZE; i++) {
#include "scale.inc"
    }
  }
};

int main(int argc, char **argv) {
  if (argc != 4) { std::fprintf(stderr, "usage: features_harness <pcm> <step ms> <chunk>\n"); return 2; }
  std::FILE *f = std::fopen(argv[1], "rb");
  if (f == nullptr) { std::perror(argv[1]); return 2; }
  std::vector<int16_t> pcm;
  int16_t s;
  while (std::fread(&s, sizeof s, 1, f) == 1) pcm.push_back(s);
  std::fclose(f);

  Detector d;
  d.feature_step_size_ = static_cast<uint8_t>(std::atoi(argv[2]));
  d.configure();
  struct FrontendState state {};
  if (!FrontendPopulateState(&d.frontend_config_, &state, SAMPLE_RATE_HZ)) { std::fprintf(stderr, "populate failed\n"); return 2; }
  const size_t chunk = static_cast<size_t>(std::atoi(argv[3]));

  int8_t features[FEATURE_SIZE];
  for (size_t base = 0; base < pcm.size(); base += chunk) {
    const size_t end = std::min(pcm.size(), base + chunk);
    size_t offset = base;
    while (offset < end) {  // the detector's read loop over one chunk
      size_t read = 0;
      FrontendOutput out = FrontendProcessSamples(&state, pcm.data() + offset, end - offset, &read);
      if (read == 0) break;
      offset += read;
      if (out.size == 0) continue;
      Detector::generate(out, features);
      for (int i = 0; i < FEATURE_SIZE; i++) std::printf(i ? " %d" : "%d", features[i]);
      std::printf("\n");
    }
  }
  FrontendFreeStateContents(&state);
  return 0;
}
