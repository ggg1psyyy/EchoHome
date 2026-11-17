# internal/mixednet test data

- `synth_NN.expected`: what the Voice Incubator firmware's own executor
  (firmware-voice-incubator at `64ce6c2a`) makes of 24 seeded synthetic
  models; the models and their inputs are rebuilt from their seeds by the
  test.
- `pooled.blob`, `okay_nabu.blob`, `hey_jarvis.blob`, `stop.blob`: wake word
  models packed into MNB1 by the Voice Incubator app's own packer
  (voice-incubator at `3a6d7e7a`), exactly as it would push them. `pooled` is
  the app's test fixture (`tests/fixtures/pooled.tflite`, "okay nabu" as
  the app trains it, by its `tests/test_wake_words.py`); the others are
  from the app's models repository (rhasspy/voice-incubator-models at
  `e359089e`, `micro-wake-word/`): okay_nabu and stop as released by
  kahrendt/microWakeWord, hey_jarvis as published by
  esphome/micro-wake-word-models. All Apache-2.0 (`../LICENSE`).
- `<model>.expected`: that executor's output for each over 2000 seeded
  invokes.

Regenerate everything with `tools/mixednet_ref/build.sh` (see
`device/tools/README.md`).
