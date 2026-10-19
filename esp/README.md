# ESP32 ground unit

Receives drone telemetry over an NRF24L01+ link and shows it on a 1.3" SH1106
OLED, with the same data served as JSON over Wi-Fi.

It can also run with **no drone at all**: in simulation mode the board flies a
believable survey mission itself and feeds the frames through the normal receive
path, so the display and the API look exactly as they would on a live link.

| File | What it is |
|---|---|
| `esp.ino` | The receiver sketch. `DRONE_SIM_MODE` picks the frame source. |
| `telemetry_frames.h` | The wire format: frame structs, checksum. Shared by both. |
| `flight_sim.h` | The simulated aircraft. Plain C++, no Arduino headers. |
| `test/` | Host unit tests for the two headers, with coverage. |

## Simulation mode

```c
#define DRONE_SIM_MODE 1   // top of esp.ino; 1 = simulated, 0 = real radio
```

**Set it to 0 before flying anything real.** In sim mode the screen shows a
healthy aircraft regardless of what the radio is doing, so an operator would not
see a lost link or a flat battery.

The OLED carries no "SIM" marking. The simulation is still named in two places:

- `GET /telemetry.json` reports `"source": "esp32-sim"` (`esp32-nrf` on a real link).
- The serial log prints `[SIM]` lines and a banner at boot.

In sim mode the NRF24 is not initialised, so the board works with no radio fitted.

### The simulated flight

About 3 minutes 10 seconds per mission, plus a 12 s boot the first time, around
the VIT lab origin (`12.843736, 80.154725`, the same as `MAV_ORIGIN_*` in
`.env.example`):

| Phase | Time | What the display shows |
|---|---|---|
| Link up | 3 s | `Waiting NRF link` |
| Boot | 12 s | `NO HEARTBEAT`, GPS `NO FIX` -> 2D -> 3D, satellites climbing |
| Standby | 8 s | Disarmed on the pad, 16.7 V, RSSI ~99% |
| Arming | 3 s | Armed, props at idle, AUTO |
| Takeoff | ~14 s | Climb to 30 m at 2.5 m/s, voltage sags under load |
| Survey | ~95 s | Pentagon, 120 m radius, 30-45 m, 8 m/s cruise |
| RTL | ~19 s | Back over the pad |
| Landing | ~33 s | 1.5 m/s, then 0.6 m/s below 10 m |
| Landed | 20 s | Disarms after 3 s, then a fresh battery and round again |

The values are coupled, not independently random:

- A 4S 3300 mAh pack follows a LiPo discharge curve. Voltage sags with current
  through internal resistance, and the charge drains with current drawn.
- Climb rate is the derivative of altitude, and heading follows the ground track.
- The airframe banks into turns (`atan(v * omega / g)`) and pitches to accelerate.
  Measured attitude lags its setpoint, and the attitude frame reports the gap.
- Motor commands come from a quad-X mixer of thrust and attitude.
- RSSI falls with range, and packet loss rises with it. A lost frame still
  uses up a sequence number, so the receiver sees real gaps.
- The companion computer's CPU load and temperature rise in flight.
- Below 15% charge the battery failsafe aborts the survey and flies home.

Frames go out at real-link rates: BASIC and ATTITUDE at 5 Hz, MOTORS at 2 Hz,
POWER_SBC at 1 Hz.

## Build and flash

The sketch builds against `esp32:esp32` 3.1.x, with the **RF24** and **U8g2**
libraries. Board: *ESP32 Dev Module* (`esp32:esp32:esp32`).

```bash
CLI="arduino-cli --config-file ~/.arduinoIDE/arduino-cli.yaml"  # use the IDE's setup
$CLI lib install RF24 U8g2
$CLI compile --fqbn esp32:esp32:esp32 esp/
$CLI upload  --fqbn esp32:esp32:esp32 -p /dev/ttyUSB0 esp/
$CLI monitor -p /dev/ttyUSB0 -c baudrate=115200
```

Real-radio build without editing the file:

```bash
$CLI compile --fqbn esp32:esp32:esp32 \
  --build-property "compiler.cpp.extra_flags=-DDRONE_SIM_MODE=0" esp/
```

Or open `esp/esp.ino` in Arduino IDE. The two headers sit next to it and are
picked up automatically.

**If upload fails with "port busy"**, something else has the serial port open.
QGroundControl's USB auto-connect will grab it, because it treats the board's
USB-serial bridge as a possible telemetry radio. Check with
`fuser -v /dev/ttyUSB0`, then quit QGC or untick the serial types under
*Application Settings -> General -> AutoConnect*. QGC reaches the swarm over UDP
and does not need this port.

## Tests

```bash
esp/test/run_tests.sh
```

Needs only `g++` and `gcov`. It builds the headers as C++11 (what arduino-esp32
2.x compiles) and C++17, runs the suite, and fails if either header drops below
95% line coverage.

What the suite pins, beyond normal behaviour:

- Every frame validates, and flipping any single bit fails the checksum.
- Frames decode, through the receiver's own structs, to the simulated state.
- `millis()` wrapping after 49.7 days, a 15 s stall in `loop()` (no backlog
  burst), and two calls in the same millisecond.
- NaN and infinity never reach an integer cast. A zero seed, a zero frame
  interval and a zero battery capacity cannot hang or divide by zero.
- Motors stop on the very step the aircraft disarms.
- The battery failsafe aborts a survey and lands.

## Known quirk in the wire format

`PowerSbcFrame` is **30 bytes**, not 32 like the other frames. Its `checksum`
member sits at byte 29, while the receiver validates byte 31 of the 32-byte
payload. Fields up to byte 28 still decode correctly, so nothing is broken
today, but the layout disagrees with itself. It has been left as found, because
changing it would change how a real transmitter's bytes are read, and that
transmitter's code is not in this repo. A `static_assert` in
`telemetry_frames.h` pins the size, so any change has to be deliberate.
