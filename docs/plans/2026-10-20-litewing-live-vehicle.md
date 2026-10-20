# LiteWing live vehicle -- implementation plan

> For agentic workers: use superpowers:executing-plans. Steps use checkbox
> syntax for tracking.

**Goal:** the real LiteWing's telemetry on the handheld OLED and on swarm node 9
in QGroundControl, read-only.

**Architecture:** see `docs/specs/2026-10-20-litewing-live-vehicle-design.md`.

- The handheld speaks CRTP log over the drone's Wi-Fi and prints `TLM` JSON lines.
- A host relay forwards those lines over UDP.
- The bridge overrides one vehicle's instruments with them.

**Tech stack:**

- Arduino C++ on esp32 core 3.1.3, host-tested with g++ and gcov.
- Go 1.27, standard library only.

**Branch:** `litewing-live`, off `esp-flight-sim`.

## File map

| File | Responsibility |
|---|---|
| `esp/crtp_log.h` | CRTP datagram codec, log-protocol client state machine, sample decoding. No Arduino headers |
| `esp/test/test_crtp_log.cpp` | Host tests for `crtp_log.h`, with a scripted fake drone |
| `esp/test/run_tests.sh` | Builds and runs both test binaries, gates coverage on all three headers |
| `esp/esp.ino` | `DRONE_SOURCE` switch, Wi-Fi join, UDP pump, LiteWing screen, `TLM` printing |
| `backend/pkg/litewing/sample.go` | `Sample` type and `Parse` -- the one definition of a valid `TLM` line, shared by relay and bridge |
| `backend/pkg/litewing/sample_test.go` | Parser tests |
| `backend/cmd/litewing-relay/main.go` | Flags, run loop, reconnect |
| `backend/cmd/litewing-relay/serial_linux.go` | Raw termios open |
| `backend/cmd/litewing-relay/main_test.go` | Relay tests against fake ports |
| `backend/pkg/mavlink/live.go` | Live feed: socket reader, loop-owned state, freshness, overrides |
| `backend/pkg/mavlink/live_test.go` | Override tests |
| `backend/pkg/mavlink/bridge.go` | Hooks: config, socket, goroutine, loop case, message builders consult live |
| `backend/pkg/mavlink/health.go`, `instruments.go` | Builders consult live |
| `backend/cmd/control-center/config.go` | `MAV_LIVE_NODE`, `MAV_LIVE_BIND` |
| `docker-compose.yml`, `.env.example` | Env passthrough, publish `127.0.0.1:14560/udp` |
| `esp/README.md`, `docs/architecture/mavlink-bridge.md`, `docs/WORKLOG.md` | Docs |

## Task 1: `crtp_log.h` codec (TDD)

- [ ] Write the tests:
  - The checksum is the 8-bit sum.
  - `encode` then `decode` round-trips.
  - `decode` rejects a size of 0, a size of 1 (checksum only, no header), a bad
    checksum, and a size over 32 (30 data bytes + header + checksum, `WIFI_RX_TX_PACKET_SIZE`).
  - `encode` refuses a payload over 30 bytes.
- [ ] Run `esp/test/run_tests.sh` and expect a compile failure.
- [ ] Implement:
  - `uint8_t checksum(const uint8_t*, size_t)`
  - `size_t encode(uint8_t header, const uint8_t* data, size_t n, uint8_t* out, size_t cap)`
  - `bool decode(const uint8_t* dg, size_t n, Packet* p)`, where `Packet` is
    `{port, channel, data[30], size}`.
- [ ] Run and expect a pass. Commit `feat(esp): CRTP datagram codec for the ESP-Drone Wi-Fi link`.

## Task 2: `crtp_log.h` log client (TDD)

Interface:

```cpp
namespace crtplog {
struct Telemetry { float roll, pitch, yaw, thrust; float altM; bool altValid;
  float vbat; uint8_t pct; bool armed; uint16_t motor[4]; uint32_t droneMs; };
class Client {
 public:
  // Next datagram to send, or 0. Call until it returns 0.
  size_t poll(uint32_t nowMs, uint8_t* out, size_t cap);
  // Feed one received datagram.
  void onDatagram(uint32_t nowMs, const uint8_t* dg, size_t n);
  State state() const;           // GetInfo, ReadToc, CreateBlocks, Streaming
  bool hasSample() const;  uint32_t lastSampleMs() const;
  uint32_t samples() const;  uint32_t restarts() const;
  const Telemetry& telemetry() const;
  int missing() const;           // wanted variables absent from the TOC
  bool takeFreshSample();        // true once per new block-1 sample
};
}
```

- [ ] Write the tests against a `FakeDrone` that implements the firmware
  behaviour from `log.c`: the TOC reply formats, the create/start reply
  (`data[2] = errno`), and a data packet with a block id and a 3-byte timestamp.
  - Happy path to Streaming.
  - Decoded values match what the fake sent.
  - Lost info reply: retry after 300 ms.
  - Lost item reply: re-request the same index.
  - Truncated item (size under 6): ignored, then retried.
  - A missing variable is left out of its block and counted in `missing`.
  - Armed is derived from thrust and motors (`sys.armed` defaults true, not used).
  - A create reply with EEXIST: send a reset (CONTROL_RESET) and redo.
  - Any other create error: restart.
  - No data for 2 s: back to GetInfo.
  - Keepalive `0xF2` every 250 ms in Streaming.
  - No output ever starts with `n`, `o`, `w`.
  - `millis()` wrap.
  - Non-finite float: the previous value is kept.
  - zrange over 8000: not valid.
  - pct over 100: clamped.
  - A short data packet: ignored.
  - Data for an unknown block: ignored.
- [ ] Implement, then run. Coverage must be 95% or more. Commit
  `feat(esp): CRTP log client that streams LiteWing telemetry`.

## Task 3: sketch integration

- [ ] `DRONE_SOURCE` with `DRONE_SIM_MODE` back-compat. Default LITEWING.
  `-DDRONE_LITEWING_SSID` and `-DDRONE_LITEWING_PASSWORD` override the AP.
- [ ] LiteWing mode:
  - `WiFi.mode(WIFI_STA)`, `begin`.
  - `WiFiUDP` bound on local port 2399.
  - The peer is `WiFi.gatewayIP()`, port 2390.
  - In `loop`: drain `parsePacket` into `client.onDatagram`, then send every
    datagram `client.poll` gives.
  - Reconnect Wi-Fi when it drops.
- [ ] `drawLiteWingScreen()`. Disconnect timeout 2 s in this mode, with the text
  `Waiting LiteWing`.
- [ ] On each fresh sample, print the `TLM` JSON line. Link events print as `[LW]`.
- [ ] Compile all three sources at `--warnings all`, with zero warnings in our files.
  Commit `feat(esp): LiteWing source for the ground unit`.

## Task 4: `pkg/litewing` sample parser (TDD)

- [ ] Tests:
  - A valid line round-trips.
  - The `TLM ` prefix is optional for `Parse` (the bridge receives no prefix).
  - Rejected: a missing field, `src` other than `litewing`, roll 181, NaN (as a
    string or as an absurd number), alt -1 and alt 11, alt null accepted, vbat 7,
    pct 101, armed 2, rssi 1, `m` of length 3, m 70000, trailing garbage, a line
    over 512 bytes.
- [ ] Implement `Parse(b []byte) (Sample, error)`, then run it with `-cover`. Commit
  `feat(litewing): TLM sample type and validation`.

## Task 5: litewing-relay (TDD)

- [ ] `run(ctx, cfg, open func() (io.ReadCloser, error), send func([]byte) error, log)`.
- [ ] Tests:
  - It forwards valid lines and drops invalid ones, counted.
  - It skips non-TLM lines.
  - An over-long line is discarded up to the next newline.
  - It reopens after EOF and after a read error.
  - It keeps retrying an open error at 1 s intervals, using a fake clock or a
    short interval.
  - A permission error logs a hint.
  - It stops on ctx cancel.
  - A send error is counted and doesn't end the loop.
  - Flag validation.
- [ ] `serial_linux.go`: open with `O_RDWR|O_NOCTTY`, then `TCGETS` and `TCSETS`
  to raw 8N1 at the baud.
- [ ] Commit `feat(relay): forward the ground unit's telemetry to the bridge`.

## Task 6: bridge live override (TDD)

- [ ] `BridgeConfig.Live LiveConfig{Node protocol.NodeID; Bind string; StaleAfter time.Duration}`,
  with StaleAfter defaulting to 1.5 s. A second socket is opened only when Node
  is set, and a `liveLoop` goroutine forwards parsed samples on `liveIn`.
- [ ] Loop-owned `liveState{have, up bool; s, prev litewing.Sample; at, prevAt time.Time}`.
- [ ] Hooks:
  - `heartbeat`: armed.
  - `sysStatus`: voltage, remaining, and health with gyro, accel, attitude and
    battery withdrawn when stale.
  - `batteryStatus`: 1 cell.
  - `attitude`: from the drone, with rates differentiated across samples.
  - `globalPosition`: Alt and RelativeAlt.
  - `vfrHud`: alt, throttle and climb.
  - `tickStatus`: freshness transitions, with STATUSTEXT up and lost once each.
- [ ] Stats: `LiveAccepted`, `LiveRejected`.
- [ ] Tests:
  - Off by default: no socket, output identical.
  - Each overridden field.
  - A stale feed withdraws bits and keeps the last values.
  - Up and lost each produce exactly one STATUSTEXT.
  - Recovery.
  - A sample before the vehicle exists.
  - Garbage is counted.
  - Shutdown joins the goroutine.
- [ ] Run `go test ./... -race -cover`, then commit
  `feat(mavlink): drive one vehicle from a live LiteWing feed`.

## Task 7: config and compose

- [ ] Add `mav-live-node` and `mav-live-bind` to the settings, validate the bind,
  and pass them into `Bridge()`. Tests in `config_test.go`.
- [ ] Compose:
  - Add `MAV_LIVE_NODE: "${MAV_LIVE_NODE-}"` and `MAV_LIVE_BIND`.
  - Add the port `${BIND_ADDR:-127.0.0.1}:${MAV_LIVE_PORT:-14560}:14560/udp`.
  - Document the new settings in `.env.example`.
- [ ] Commit `feat(control-center): MAV_LIVE_NODE and MAV_LIVE_BIND`.

## Task 8: hardware end to end

1. Flash the handheld, then read serial and expect `TLM` lines with plausible
   values: around 3.7-4.2 V, with roll and pitch near 0 when flat.
2. Set `.env` with `MAV_LIVE_NODE=swarm-net-node-9`, rebuild the control center,
   and run `docker compose up -d`.
3. Run `go run ./cmd/litewing-relay -port /dev/ttyUSB0`.
4. Decode node 9's MAVLink on 14555 with a scratch client, and compare its
   ATTITUDE and SYS_STATUS against the `TLM` lines.
5. The user tilts the drone and powers it off and on, and checks the OLED and QGC.

## Task 9: docs

- [ ] `esp/README.md` (LiteWing section), `docs/architecture/mavlink-bridge.md`
  (live vehicle section), and WORKLOG section 15. Run the ASCII and Mermaid
  checks, then commit.
