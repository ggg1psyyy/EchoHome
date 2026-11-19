# Contributing to EchoHome

Issues and pull requests go to
[ggg1psyyy/EchoHome](https://github.com/ggg1psyyy/EchoHome). Something that
is EchoMuse's as much as EchoHome's (the unlock, emOS, a hardware fact) is
often better raised with [EchoMuse](https://github.com/wilbowes/EchoMuse),
which EchoHome follows for emOS.

With a bug, attach a [support bundle](docs/support-bundle.md) (dashboard →
Settings → Support): versions, device state and logs, with network and
account names removed.

## Layout

- `device/` — the Go firmware on the Echo: the Voice Incubator satellite,
  the Echo's ESPHome device, the hardware bindings (`device/CLAUDE.md`).
- `controller/` — the Python setup tool, its add-on and the dashboard
  (`controller/CLAUDE.md`).
- emOS, the Echo's userspace, is [EchoMuse's](https://github.com/wilbowes/EchoMuse/tree/main/emos) and not in
  this repository; `tools/fetch_emos_ref.py` fetches the few emOS files the
  tests check EchoHome against, at the commit pinned in
  `tools/emos_ref.json`.

`CLAUDE.md` holds the project's rules (portability, the device/controller
compatibility rules, privacy, releases); `docs/history/` holds the reasons
behind them.

## Building and testing

```bash
git submodule update --init          # GoTinyAlsa (the wilbowes fork)
cd device && docker build -t echomuse-compiler compiler/
cd device && ./compile.sh            # → build/server
cd device && go test ./internal/... ./pkg/...
cd controller && python -m pytest tests/   # needs: pytest numpy scipy pyyaml
BABEL_STANDALONE=/tmp/babel.min.js node controller/tests/dashboard_globals.test.mjs
python3 tools/fetch_emos_ref.py      # before the tests: emOS's files, pinned
```

`cmd/` builds only with `-tags server`, in the compiler image. CI runs both
suites, `go vet`, a headless-Chromium walk of every dashboard tab and the
image's CPU baseline checks; a change goes in green.

A change that touches the hardware gets the [acceptance checks](docs/uat.md)
on a real Echo, with the setup tool stopped.

## Pull requests

- One change per pull request, with a test for what it fixes or adds; where
  a standard or spec exists, a test written from its edges.
- **Negotiate by capability, never by version**: any firmware can meet any
  setup tool, including firmware from before EchoHome.
- Echoes are named by the last four characters of their serial in anything
  committed; no addresses, names or full serials.
- AI-assisted pull requests are welcome; EchoHome itself is written largely
  with Claude. Changes for OHF-Voice's projects (Voice Incubator) follow
  their AI policy and are posted there by a person.

By contributing you agree your contribution is licensed under the
[MIT licence](LICENSE).
