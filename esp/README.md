# ESP32 ground unit

A handheld telemetry display: an ESP32 with a 1.3" SH1106 OLED. It shows a
drone's telemetry from one of three sources, picked at build time:

| `DRONE_SOURCE` | Source | Notes |
|---|---|---|
| `2` (default) | A **LiteWing** quadcopter, over the drone's own Wi-Fi | Read only. Also feeds node 9 in QGroundControl |
| `1` | The on-board **flight simulator** | No drone needed |
| `0` | An **NRF24L01+** radio link | Serves the same data as JSON over Wi-Fi |

| File | What it is |
|---|---|
| `esp.ino` | The receiver sketch. `DRONE_SOURCE` picks the frame source. |
| `crtp_log.h` | The LiteWing link: CRTP codec and log-protocol client. Plain C++. |
| `telemetry_frames.h` | The NRF wire format: frame structs, checksum. |
| `flight_sim.h` | The simulated aircraft. Plain C++, no Arduino headers. |
| `test/` | Host unit tests for the three headers, with coverage. |

The old `-DDRONE_SIM_MODE=1` / `=0` still works and means SIM / NRF.

## LiteWing mode

The LiteWing (ESP32-S3, ESP-Drone firmware) runs its own access point and serves
the Crazyflie log protocol (CRTP) over UDP. The handheld joins that access point
and asks the drone to stream the values it displays. Nothing is reflashed on the
drone.

```mermaid
flowchart LR
  D[LiteWing] -- "CRTP log, UDP 2390" --> H[Handheld + OLED]
  H -- "USB serial, TLM lines" --> R[litewing-relay]
  R -- "UDP 14560" --> B[MAVLink bridge]
  B -- "sysid 9" --> Q[QGroundControl]
```

**On the OLED:** battery volts and percent, the Wi-Fi RSSI in dBm, altitude from
the downward range sensor (`---` when nothing is in range), roll / pitch / yaw,
thrust, and `ARMED` (inverted) or `DISARMED`. Armed means thrust or a motor above
zero. A short bar slides along the header line, so a live board never looks hung.

**Link screen.** From boot until the first sample, and whenever no sample arrives
for 2 s, the OLED shows the handshake instead:

| Element | Shows |
|---|---|
| Header | `LITEWING LINK`, or `LINK LOST 12s`, and a spinner |
| Stage | `Joining Wi-Fi`, `Asking drone`, `Reading TOC 142/316`, `Setting up` |
| Bar | Progress through the real handshake, never a timer |
| Dots | Wi-Fi, info, TOC, setup, streaming: filled done, blinking current |

The bar jumps from part-way to 90% when the TOC scan stops early, once all 11
variables are found. The logic is in `boot_anim.h`.

**OLED check.** The panel has its own task, so a slow or missing screen never
delays the drone link. Once a second it probes I2C address 0x3C at 400, 100 and
50 kHz and draws at the fastest clock that answers 5 probes out of 5. It drops a
clock at once, and climbs back one step only after 5 clean seconds. A panel that
keeps answering gets its setup re-sent each second, which heals one garbled by a
glitch, with no visible blink. Serial shows each change:

```text
[OLED] found at 0x3C, bus 50 kHz
[OLED] not responding at 0x3C (SDA 21, SCL 22)
```

`not responding` for good means the wiring: SDA is GPIO 21, SCL is GPIO 22. A
panel found only at 50 kHz means a poor contact on SDA or SCL. It works, but
reseat or solder the wire.

**Read only.** The handheld sends only log requests and a keepalive. It never
sends a setpoint, so it cannot spin the motors. This is enforced in `crtp_log.h`
and tested: the firmware reads any 7-byte datagram starting with `now` as a stick
command, and no datagram the client sends can start with `n`.

What the client does, in order:

1. Reads the drone's log table (316 entries on this unit), keeping the 11 it wants
   by name.
2. Builds two log blocks: attitude, thrust, range and battery voltage at 10 Hz,
   and motors and battery percent at 2 Hz.
3. Sends a keepalive every 250 ms. With no packet for 1 s, the drone drops every
   log block.
4. Restarts from step 1 after 2 s without data, which covers a drone that was
   switched off or rebooted.

A different LiteWing has a different access point name:

```bash
$CLI compile --fqbn esp32:esp32:esp32 \
  --build-property 'compiler.cpp.extra_flags=-DDRONE_LITEWING_SSID="LiteWing_xxxxxxxxxxxx"' esp/
```

**Serial output.** Each sample is printed as one line, and
`backend/cmd/litewing-relay` forwards those lines to the swarm's MAVLink bridge:

```text
TLM {"src":"litewing","t":3258573,"roll":2.30,"pitch":1.91,"yaw":-116.73,"thrust":0,"alt":0.027,"vbat":4.052,"pct":80,"armed":0,"rssi":-29,"m":[0,0,0,0]}
```

In this mode the board's Wi-Fi is used for the drone link, so the HTTP API is
not started.

### Showing it in QGroundControl

```bash
# .env: MAV_ENABLED=true and MAV_LIVE_NODE=swarm-net-node-9
docker compose up -d
cd backend && go run ./cmd/litewing-relay -port /dev/ttyUSB0
```

Node 9 then reports the drone's real attitude, battery, altitude, throttle and
armed state. Its map position stays the swarm's, because the drone has no GPS.
If the feed stops for 1.5 s, QGC shows `LiteWing link lost` and the drone's
health bits go red. It does not fall back to simulated values. See
`docs/architecture/mavlink-bridge.md` section 11.

## Simulation mode

```bash
$CLI compile --fqbn esp32:esp32:esp32 \
  --build-property "compiler.cpp.extra_flags=-DDRONE_SOURCE=1" esp/
```

**Never use it with anything real.** In sim mode the screen shows a healthy
aircraft regardless of what the radio is doing, so an operator would not see a
lost link or a flat battery.

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

The default build is LiteWing mode. For the other sources, without editing the
file:

```bash
$CLI compile --fqbn esp32:esp32:esp32 \
  --build-property "compiler.cpp.extra_flags=-DDRONE_SOURCE=0" esp/   # NRF radio
```

Or open `esp/esp.ino` in Arduino IDE. The headers sit next to it and are picked
up automatically.

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
2.x compiles) and C++17, runs all three suites, and fails if any header drops below
95% line coverage.

The LiteWing suite runs the client against a fake drone written from the
ESP-Drone source: the same reply layouts, checksum, 32-byte limit and block
timers. It pins:

- The handshake end to end, and decoded values that track the drone.
- Retries of a lost request or reply, restarts when retries run out or the drone
  goes silent, and recovery.
- A missing variable is left out, not fatal, and nothing usable backs off for 5 s.
- No datagram it sends can be a setpoint.
- No sample is published until the 2 Hz block has reported. Before this, the
  real drone showed 0% battery for the first half second.
- A drone rejecting every block cannot spin the client inside one `loop()`.
- `millis()` wrap, NaN floats, out-of-range altitude and clamped percent.

The boot animation suite (`test_boot_anim.cpp`) pins the stage for every Wi-Fi
and client state, a bar that never goes backwards through the handshake and
never divides by zero, the spinner and sweep across `millis()` wrap, and the
OLED probe: once a second, the first probe always logged, then only changes,
including a wire flapping on every probe.

What the flight-sim suite pins, beyond normal behaviour:

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
