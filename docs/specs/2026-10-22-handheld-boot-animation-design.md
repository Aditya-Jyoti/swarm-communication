# Handheld boot and link animation -- design

Date: 2026-10-22. Branch `oled-boot-anim`. Approved by the user (style: stages + progress).

## Problem

The ground unit's OLED shows a static "Initializing..." splash, then a static
"NO CONNECTION" screen until LiteWing samples arrive. Nothing on screen moves, so
a frozen board, a slow handshake and a dead panel all look the same. The panel
is also on a flaky SDA wire (2026-10-22 probe: GPIO21 floats), so a panel that
misses its init stays blank until the next reboot.

## What it does

1. **Link screen (LiteWing mode).** Replaces the splash and the "NO CONNECTION"
   screen until the first sample. A spinner, the current stage, a progress bar
   driven by the real handshake, a row of stage dots, and the board's IP.
2. **Activity.** A one-pixel sweep under the header on the telemetry screen, so
   the screen visibly moves even when the numbers do not. The disconnect screen
   (all modes) gets a spinner.
3. **Panel watch.** Once a second the sketch probes I2C address 0x3C. Absent to
   present re-inits the panel; each change is logged as `[OLED] found` or
   `[OLED] not responding`.

```mermaid
flowchart LR
    W["Joining Wi-Fi"] --> I["get-info"]
    I --> T["Reading TOC n of N"]
    T --> S["Setting up blocks"]
    S --> L["Streaming: telemetry screen"]
    L -- "link lost" --> W
```

## Progress bands

| Stage | Bar range | Driven by |
|---|---|---|
| Joining Wi-Fi | 0 to 20 | `WiFi.status()` |
| get-info | 20 to 30 | client state |
| Reading TOC | 30 to 85 | `tocRead / tocCount` |
| Setting up | 85 to 95 | client state |
| Streaming | 100 | client state |

TOC reading stops early once all wanted variables are found, so the bar jumps
from part-way to 85. That is honest: the remaining entries are never read.
`unsupported` shows its own label and an empty bar.

## Units

- `esp/boot_anim.h` -- plain C++, no Arduino: stage from (Wi-Fi, client state),
  percent, labels, stage dots, spinner frame, sweep position, and `PanelWatch`
  (probe schedule and present/absent transitions). Host-tested, >= 95% lines.
- `crtp_log.h` -- one new accessor, `tocRead()`.
- `esp.ino` -- drawing and the I2C probe only.

## Not doing

No timer-driven fake progress, no animation in NRF or SIM link stages beyond
the disconnect spinner, no change to the telemetry layout.
