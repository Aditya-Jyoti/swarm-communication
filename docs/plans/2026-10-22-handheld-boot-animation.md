# Handheld boot animation -- implementation plan

Spec: `docs/specs/2026-10-22-handheld-boot-animation-design.md`. Branch `oled-boot-anim`.

Run every suite as `(ulimit -v 4000000; timeout 250 bash esp/test/run_tests.sh)`.

## Task 1 -- `tocRead()` on the CRTP client

- Test first in `esp/test/test_crtp_log.cpp`: 0 after construction, counts up
  one per TOC reply during READ_TOC, back to 0 after a restart.
- Add `uint16_t tocRead() const { return nextItem_; }` to `esp/crtp_log.h`.
- Commit `feat(esp): expose TOC read progress`.

## Task 2 -- `esp/boot_anim.h` with `esp/test/test_boot_anim.cpp`

API, namespace `bootanim`:

- `enum Stage { STG_WIFI, STG_GET_INFO, STG_READ_TOC, STG_SETUP, STG_STREAMING, STG_UNSUPPORTED }`
- `Stage stageOf(bool wifiUp, crtplog::State s)` -- no Wi-Fi wins over any state.
- `uint8_t progressPct(Stage, uint16_t tocRead, uint16_t tocCount)` -- bands from
  the spec, TOC clamped (read > count, count 0).
- `const char* stageLabel(Stage)`.
- `void stageDots(Stage, char out[6])` -- 5 dots: `o` done, `*` current, `.` pending;
  unsupported marks TOC as `x`.
- `char spinnerFrame(uint32_t ms)` -- `|/-\`, 125 ms each; wraps at 2^32.
- `uint8_t sweepX(uint32_t ms, uint8_t width)` -- triangle wave over 0..width-1,
  width 0 and 1 return 0.
- `class PanelWatch` -- `bool due(uint32_t now)` every 1000 ms (first call due),
  `Event update(bool ack)` returns `EV_NONE`, `EV_FOUND` (absent to present, and
  the first ack), `EV_LOST`; `present()`.

Tests: every stage and state combination, band edges, clamping, label and dot
strings, spinner and sweep across wrap, PanelWatch schedule across `millis()`
wrap and each transition. Add the suite to `run_tests.sh` (C++11 smoke and the
coverage list). Commit `feat(esp): boot animation model for the ground unit`.

## Task 3 -- sketch

- `drawLinkScreen()` (LiteWing): header `LITEWING LINK` plus spinner, stage label
  (TOC as `Reading TOC n/N`), framed bar plus percent, dots, IP.
- Splash becomes an animated spinner frame; disconnect screen gets the spinner.
- Telemetry screen gets the sweep under the header line.
- `panelPoll()` in `loop()`: probe 0x3C when due, on `EV_FOUND` call `u8g2.begin()`
  and `setContrast(255)`, log both events.
- Compile all three `DRONE_SOURCE` builds. Commit `feat(esp): animated link screen and OLED re-init`.

## Task 4 -- verify on hardware, docs, images

Flash LiteWing build, read the serial log for `[OLED]` lines and TLM. Update
`esp/README.md`, refresh `esp/firmware/handheld` images and SHA256SUMS, append
WORKLOG. Commit each separately.
