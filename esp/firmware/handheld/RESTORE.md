# Handheld ground unit -- restore

ESP-WROOM-32 (ESP32-D0WD-V3), 4 MB flash, CP2102 on `/dev/ttyUSB0`. Built from
`esp/` at commit a1bfa71 (branch `oled-boot-anim`), core `esp32:esp32` 3.1.3.

| Image | Source |
|---|---|
| `build/litewing/esp.ino.merged.bin` | LiteWing mode (`DRONE_SOURCE=2`, the default) |
| `build/sim/esp.ino.merged.bin` | Flight simulator (`DRONE_SOURCE=1`) |
| `build/nrf/esp.ino.merged.bin` | NRF24 radio (`DRONE_SOURCE=0`) |

No full-flash dump of the board was taken: everything on it is built from this
repo, so a merged image restores it.

## Before flashing

```bash
stat -c '%a %G' /dev/ttyUSB0      # needs 666; else: sudo chmod 666 /dev/ttyUSB0 (resets on replug)
fuser -v /dev/ttyUSB0             # must be empty: stop litewing-relay and QGroundControl
sha256sum -c SHA256SUMS
```

## Flash a merged image

```bash
ET=~/.arduino15/packages/esp32/tools/esptool_py/4.9.dev3/esptool
$ET --chip esp32 --port /dev/ttyUSB0 --baud 921600 write_flash 0x0 build/litewing/esp.ino.merged.bin
```

## Rebuild from source instead

```bash
arduino-cli --config-file ~/.arduinoIDE/arduino-cli.yaml compile --fqbn esp32:esp32:esp32 esp/
arduino-cli --config-file ~/.arduinoIDE/arduino-cli.yaml upload  --fqbn esp32:esp32:esp32 -p /dev/ttyUSB0 esp/
```

Add `--build-property "compiler.cpp.extra_flags=-DDRONE_SOURCE=1"` (or `=0`) to
the compile for the other sources. See `esp/README.md`.

## After flashing

The serial log should show `[OLED] found at 0x3C` within a second of boot. If it
says `not responding`, check the OLED wiring (SDA GPIO 21, SCL GPIO 22). If
`write_flash` fails with "No serial data received", retry with `--baud 115200`.
