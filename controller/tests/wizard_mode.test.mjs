// Tests for the wizard's mode checks in dashboard.jsx: which mode a device's
// ADB banner says it is in (_bannerMode), which mode each step needs
// (_EMOS_STEPS), and which errors are a disconnect rather than a provisioning
// failure (_isDisconnectError).
//
//     node controller/tests/wizard_mode.test.mjs
//
// Source extraction rather than import: the dashboard compiles to a single
// classic script with no module boundary, so the alternative is a second copy
// that drifts. (Until 2026-11-19 this lived in pm_verdict.test.mjs beside the
// FireOS flow's package-manager checks, which went with that flow.)

import { readFileSync } from "fs";
import { fileURLToPath } from "url";
import { dirname, join } from "path";

const HERE = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(HERE, "..", "static", "dashboard.jsx"), "utf8");

// Handles both arrow forms in this file: `= (x) => { ... };` with a body, and
// `= (x) => expr || expr;` without one. Scanning for a matching brace only
// works for the first, and silently swallows the following declaration for the
// second, which is how this test first failed with "already declared".
function liftArrow(name) {
  const start = src.indexOf(`const ${name} = (`);
  if (start < 0) {
    throw new Error(`dashboard.jsx no longer defines ${name}() — if it was `
                  + `renamed or moved, update this test to match`);
  }
  let depth = 0;
  for (let i = start; i < src.length; i++) {
    const ch = src[i];
    if (ch === "{" || ch === "(" || ch === "[") depth++;
    else if (ch === "}" || ch === ")" || ch === "]") depth--;
    else if (ch === ";" && depth === 0) return src.slice(start, i + 1);
  }
  throw new Error(`could not find the end of ${name}`);
}

let failures = 0;
function check(name, cond, detail) {
  if (cond) return;
  failures++;
  console.error(`FAIL: ${name}${detail ? `\n      ${detail}` : ""}`);
}

// ─── Step mode / banner classification ────────────────────────────────────────
//
// Pulling the cable powers the Dot off, and `reboot recovery` is a one-shot,
// so a replug is a cold boot into Android whatever phase the wizard is in.
// Reconnecting during the TWRP phase hands back an Android device that looks
// healthy. In Android `/dev/block/other-boot` points at boot_b, which holds
// amonet's unlock payload, so a retried Patch Boot Image there would write
// over the unlock.

function liftConstObject(name) {
  const start = src.indexOf(`const ${name} = {`);
  if (start < 0) throw new Error(`dashboard.jsx no longer defines ${name}`);
  const end = src.indexOf("};", start);
  return src.slice(start, end + 2);
}

function liftFunctionDecl(name) {
  const start = src.indexOf(`function ${name}(`);
  if (start < 0) throw new Error(`dashboard.jsx no longer defines ${name}()`);
  let depth = 0, seen = false, i = src.indexOf("{", start);
  for (; i < src.length; i++) {
    if (src[i] === "{") { depth++; seen = true; }
    else if (src[i] === "}") { depth--; if (seen && depth === 0) break; }
  }
  return src.slice(start, i + 1);
}

function liftConstArray(name) {
  const start = src.indexOf(`const ${name} = [`);
  if (start < 0) throw new Error(`dashboard.jsx no longer defines ${name}`);
  const end = src.indexOf("\n];", start);
  return src.slice(start, end + 3);
}

const { _bannerMode, _EMOS_STEPS } = await import(
  "data:text/javascript;base64," + Buffer.from(
    liftFunctionDecl("_bannerMode") + "\n" + liftConstArray("_EMOS_STEPS")
    + "\nexport { _bannerMode, _EMOS_STEPS };"
  ).toString("base64"));
const mode = id => (_EMOS_STEPS.find(s => s.id === id) || {}).mode;

// Real banners, from provisioning transcripts.
check("TWRP is recognised", _bannerMode("omni_biscuit") === "twrp",
      _bannerMode("omni_biscuit"));
check("Android is recognised", _bannerMode("csm_biscuit") === "android",
      _bannerMode("csm_biscuit"));

// The ordering trap: "omni_biscuit" contains "biscuit", so an Android-first
// test calls every TWRP device Android — which is the exact direction that
// lets a TWRP step run against Android.
check("TWRP is not mistaken for Android", _bannerMode("omni_biscuit") !== "android");

check("an unknown banner is not guessed at",
      _bannerMode("something_else") === "unknown", _bannerMode("something_else"));
check("an empty banner is not guessed at", _bannerMode("") === "unknown");
check("a missing banner is not guessed at", _bannerMode(undefined) === "unknown");

// Every step that writes the boot partition or reads it must run in TWRP: in
// Android /dev/block/other-boot is amonet's unlock payload, not a kernel.
for (const id of ["escrow_boot", "install_em", "build_emos", "flash_emos"]) {
  check(`${id} is a TWRP step`, mode(id) === "twrp", String(mode(id)));
}
check("connect device starts in Android", mode("connect_android") === "android");
check("the console steps run over serial",
  mode("reboot_watch") === "serial" && mode("wifi_register") === "serial");
// Every step must have a mode, or the check silently does nothing for it.
for (const s of _EMOS_STEPS) {
  check(`${s.id} has a mode`, ["android", "twrp", "serial"].includes(s.mode), String(s.mode));
}

// ─── Disconnect-shaped errors ─────────────────────────────────────────────────
//
// The in-flight transfer can throw BEFORE the WebUSB disconnect event lands —
// the two race, and on a real run the throw won. That put "Failed to execute
// 'transferOut' on 'USBDevice'" in the transcript as though it were a
// provisioning failure, and then sent the diagnostics probes at a device that
// was no longer plugged in.

const { _isDisconnectError } = await import(
  "data:text/javascript;base64," + Buffer.from(
    liftArrow("_isDisconnectError") + "\nexport { _isDisconnectError };"
  ).toString("base64"));

// The real one, from the transcript that prompted this.
check("the observed transferOut error is recognised",
  _isDisconnectError(new Error(
    "Failed to execute 'transferOut' on 'USBDevice': The device was disconnected.")));
check("the transferIn direction is recognised",
  _isDisconnectError(new Error(
    "Failed to execute 'transferIn' on 'USBDevice': The device was disconnected.")));
check("a NetworkError is recognised",
  _isDisconnectError(new Error("NetworkError: A transfer error has occurred.")));

// Provisioning failures must NOT be swallowed as disconnects: taking the
// abandon path on a genuine failure hides the real message and skips the
// diagnostics capture, which is the whole point of #87.
for (const msg of [
  "Hash mismatch — expected 18e46b16b25e… (Magisk-v17.3.zip)",
  "Did not associate to the network within 20s",
  "The package manager rejected 11 of 11 calls and disabled none.",
  "Install verification failed: /data/local/bin/server points to \"\"",
]) {
  check(`a real failure is not treated as a disconnect: ${msg.slice(0, 34)}`,
        !_isDisconnectError(new Error(msg)), msg);
}

check("a missing message does not throw", _isDisconnectError(undefined) === false);
check("an error with no message does not throw", _isDisconnectError({}) === false);

if (failures) {
  console.error(`\n${failures} failure(s)`);
  process.exit(1);
}
console.log("wizard_mode: all checks passed.");
