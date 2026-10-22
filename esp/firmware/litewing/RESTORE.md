# LiteWing firmware backup -- restore guide

This is a read-only dump of the stock firmware on the LiteWing drone.
The source code is NOT available anywhere. These files are the only copy.
Do not delete them.

## What was on the drone

| Fact | Value |
| --- | --- |
| Chip | ESP32-S3 (QFN56) rev v0.2, MAC 80:45:6b:54:aa:54 |
| USB bridge | CH340 (1a86:7522) |
| Physical flash | 8 MB (manufacturer 0x20, device 0x4017) |
| Image header | DIO, 80 MHz, 2MB (bootloader switches to QIO at runtime) |
| ESP-IDF | v4.4.8 |
| Project / version | ESPDrone, 4d2928b-dirty |
| Compiled | Sep 17 2026 01:15:11 |
| softAP | LiteWing_80456B54AA55, password 12345678 (public default) |
| Extras | custom ESP-NOW telemetry broadcast (`espnow_tlm`) |

Partition table (`partitions.csv`):

```
nvs,      data, nvs,     0x9000,  24K
phy_init, data, phy,     0xf000,  4K
factory,  app,  factory, 0x10000, 1M
```

Flash above 0x110000 is fully erased (all 0xFF).

## Files

| File | Flash offset | Notes |
| --- | --- | --- |
| `litewing-full-flash.bin` | 0x0 | whole 8 MB chip, byte-for-byte |
| `bootloader.bin` | 0x0 | 0x0..0x8000 |
| `partition-table.bin` | 0x8000 | 4 KB |
| `nvs.bin` | 0x9000 | 24 KB (Wi-Fi AP config, PHY cal) |
| `phy_init.bin` | 0xf000 | 4 KB |
| `espdrone-app.bin` | 0x10000 | factory app trimmed to its image length (836912 bytes) |
| `app-image-info.txt` | -- | `esptool image_info` output for the app |
| `SHA256SUMS` | -- | checksums for every .bin |

Note: the factory partition also holds ~12 KB of stale bytes past the end of
the app image (leftovers from an older, larger flash). They are kept in the
full dump but not in `espdrone-app.bin`. The app does not use them.

## Before you start

```sh
cd esp/firmware/litewing
sha256sum -c SHA256SUMS                  # every line must say OK
fuser -v /dev/ttyUSB1                    # port must be free (close QGC, monitors)
sudo chmod 666 /dev/ttyUSB1              # resets on every replug
ET=~/.arduino15/packages/esp32/tools/esptool_py/4.9.dev3/esptool
```

Check that the port really is the LiteWing (the CH340 numbering can change
after a replug): `esptool --port /dev/ttyUSBx flash_id` must report
ESP32-S3 with MAC 80:45:6b:54:aa:54.

## (a) Restore the full dump (preferred)

Byte-for-byte copy of the whole chip, including NVS and calibration.

```sh
$ET --chip esp32s3 --port /dev/ttyUSB1 --baud 921600 --after hard_reset \
    write_flash --flash_mode keep --flash_freq keep --flash_size keep \
    0x0 litewing-full-flash.bin
```

`keep` is required: it stops esptool from rewriting the image headers, so the
written bytes match the dump exactly.

## (b) Alternative: bootloader + partition table + app

Smaller and faster. If NVS is blank, the firmware recreates it on first boot (with the
default AP settings). Add `0x9000 nvs.bin 0xf000 phy_init.bin`
to also restore the saved NVS and PHY data.

```sh
$ET --chip esp32s3 --port /dev/ttyUSB1 --baud 921600 --after hard_reset \
    write_flash --flash_mode keep --flash_freq keep --flash_size keep \
    0x0     bootloader.bin \
    0x8000  partition-table.bin \
    0x10000 espdrone-app.bin
```

## (c) Verify

```sh
sha256sum -c SHA256SUMS
$ET --chip esp32s3 --port /dev/ttyUSB1 verify_flash --flash_size keep \
    0x0 litewing-full-flash.bin
# or, after (b):
$ET --chip esp32s3 --port /dev/ttyUSB1 verify_flash --flash_size keep \
    0x0 bootloader.bin 0x8000 partition-table.bin 0x10000 espdrone-app.bin
```

Expect `-- verify OK (digest matched)`. Then open the serial port at 115200
and look for `Project name: ESPDrone`, `selftestPassed = 1` and `espnow_tlm`
in the boot log.

## If auto-reset fails

If esptool prints `Failed to connect` / `Wrong boot mode detected`:

1. Hold the BOOT button.
2. Tap RESET (EN) once.
3. Release BOOT, then rerun the esptool command.

After flashing in this mode, press RESET once to start the firmware.

## The ground unit does not need this firmware changed

The ground unit talks to the stock CRTP log service over UDP port 2390 on the
LiteWing softAP. Restoring (or keeping) this stock image is all it needs.
