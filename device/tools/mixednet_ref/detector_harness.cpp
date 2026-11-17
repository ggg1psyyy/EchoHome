// Reference harness for internal/mixednet's Detector: the Voice Incubator
// firmware's own wake word detector logic, compiled from its sources at the
// pinned commit (sha256-checked by build.sh). Cut verbatim into gen/:
//   detector_decls.inc  wake_detector.h, FEATURE_SIZE to the end of WakeModel
//                       (constants, ModelReport, Detection, ModelConfig,
//                       ModelStream, WakeModel)
//   stream_impl.inc     ModelStream's methods
//   loading_impl.inc    begin_model .. set_stop_armed
//   process_impl.inc    process_features_
//   report_impl.inc     take_report (what a monitored satellite's WAKE_REPORT says)
// What is not the firmware's: the stand-ins for its ESP32 pieces below (an
// allocator, a mutex, logging), the WakeDetector class shell (its member
// list and initial values, copied from wake_detector.h), queue_detection_
// (which prints), and main's per-frame printing.
//
//   detector_harness <features> <model>... [arm=N]... [disarm=N]...
//   model: id,is_vad,cutoff,window,blob     arm/disarm: set_stop_armed at frame N
// Prints per frame: "<frame> <latest probability of each model, or - when
// disabled>", and "D <frame> <index> <average> <max>" per detection; every
// 7th frame, take_report's "R <frame>" and " <id> <peak> <window>" per model.
#include <algorithm>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <string>
#include <vector>

#include "mixednet.h"
#include "mixednet_blob.h"

namespace esphome {
template <typename T> using RAMUniquePtr = std::unique_ptr<T>;
template <typename T> struct RAMAllocator {
  std::unique_ptr<T[]> make_unique_array_for_overwrite(size_t n) { return std::unique_ptr<T[]>(new T[n]); }
};
using std::make_unique;
struct Mutex {};
struct LockGuard {
  explicit LockGuard(Mutex &) {}
};
}  // namespace esphome
#define ESP_LOGE(...) ((void) 0)
#define ESP_LOGD(...) ((void) 0)
#define ESP_LOGV(...) ((void) 0)

namespace esphome::voice_incubator {
static const char *const TAG = "harness";
static constexpr size_t MAX_MODEL_BYTES = 512 * 1024;  // wake_detector.cpp
#include "detector_decls.inc"
#include "stream_impl.inc"

class WakeDetector {
 public:
  bool begin_model(const ModelConfig &config, size_t size);
  bool append_model(const uint8_t *data, size_t len);
  bool finish_model(std::string *error);
  void abort_model();
  void clear_models();
  void set_stop_armed(bool armed);
  void process_features_(const int8_t *features);
  void take_report(std::vector<ModelReport> *out);
  void queue_detection_(uint8_t index, const ModelStream &stream) {
    uint8_t average, max;
    stream.summarize(&average, &max);
    std::printf("D %u %u %u %u\n", this->slices_seen_, index, average, max);
  }

  Mutex models_lock_;
  std::vector<std::unique_ptr<WakeModel>> models_;
  size_t loaded_count_{0};
  bool stop_armed_{false};
  ModelConfig staging_config_;
  RAMUniquePtr<uint8_t[]> staging_blob_;
  size_t staging_size_{0};
  size_t staging_filled_{0};
  uint8_t feature_step_size_{10};
  uint32_t slices_seen_{0};
  int32_t audio_peak_{0};
};

#include "loading_impl.inc"
#include "process_impl.inc"
#include "report_impl.inc"
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
  if (argc < 3) { std::fprintf(stderr, "usage: detector_harness <features> <model>... [arm=N]... [disarm=N]...\n"); return 2; }
  WakeDetector det;
  std::vector<long> arm_at, disarm_at;
  for (int a = 2; a < argc; a++) {
    if (std::strncmp(argv[a], "arm=", 4) == 0) { arm_at.push_back(std::atol(argv[a] + 4)); continue; }
    if (std::strncmp(argv[a], "disarm=", 7) == 0) { disarm_at.push_back(std::atol(argv[a] + 7)); continue; }
    char id[64], path[512];
    int vad, cutoff, window;
    if (std::sscanf(argv[a], "%63[^,],%d,%d,%d,%511s", id, &vad, &cutoff, &window, path) != 5) return 2;
    ModelConfig cfg;
    cfg.id = id;
    cfg.is_vad = vad != 0;
    cfg.probability_cutoff = static_cast<uint8_t>(cutoff);
    cfg.sliding_window_size = static_cast<uint8_t>(window);
    std::vector<uint8_t> blob = slurp(path);
    std::string error;
    if (!det.begin_model(cfg, blob.size()) || !det.append_model(blob.data(), blob.size()) ||
        !det.finish_model(&error)) {
      std::printf("refused %s: %s\n", id, error.c_str());
      continue;
    }
  }
  std::vector<uint8_t> feats = slurp(argv[1]);
  long frame = 0;
  for (size_t off = 0; off + FEATURE_SIZE <= feats.size(); off += FEATURE_SIZE) {
    if (std::find(arm_at.begin(), arm_at.end(), frame) != arm_at.end()) det.set_stop_armed(true);
    if (std::find(disarm_at.begin(), disarm_at.end(), frame) != disarm_at.end()) det.set_stop_armed(false);
    det.process_features_(reinterpret_cast<const int8_t *>(feats.data() + off));
    frame++;
    std::printf("%ld", frame);
    for (auto &m : det.models_) {
      if (m->enabled)
        std::printf(" %u", m->stream.recent[m->stream.last_n_index]);
      else
        std::printf(" -");
    }
    std::printf("\n");
    if (frame % 7 == 0) {
      std::vector<ModelReport> reports;
      det.take_report(&reports);
      std::printf("R %ld", frame);
      for (auto &r : reports)
        std::printf(" %s %u %u", r.id.c_str(), r.peak, r.window);
      std::printf("\n");
    }
  }
  return 0;
}
