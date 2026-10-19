// Host unit tests for flight_sim.h and telemetry_frames.h.
//
//   esp/test/run_tests.sh      # build with coverage, run, report
//
// No test framework: the board's toolchain is not here, and a dependency-free
// harness keeps these runnable on any machine with g++. Frames are decoded the
// way the receiver decodes them -- validateFrameChecksum, then the packed struct
// -- so a pass here means esp.ino would accept and display the same bytes.

#include <cmath>
#include <cstdio>
#include <cstring>
#include <vector>

#include "../flight_sim.h"

using namespace flightsim;

static int g_checks = 0;
static int g_failures = 0;

#define CHECK(cond)                                                         \
  do {                                                                      \
    ++g_checks;                                                             \
    if (!(cond)) {                                                          \
      ++g_failures;                                                         \
      std::printf("  FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond);         \
    }                                                                       \
  } while (0)

#define CHECK_NEAR(a, b, tol)                                               \
  do {                                                                      \
    ++g_checks;                                                             \
    double _a = (a), _b = (b);                                              \
    if (!(std::fabs(_a - _b) <= (tol))) {                                   \
      ++g_failures;                                                         \
      std::printf("  FAIL %s:%d: %s=%g vs %s=%g (tol %g)\n", __FILE__,      \
                  __LINE__, #a, _a, #b, _b, (double)(tol));                 \
    }                                                                       \
  } while (0)

template <typename T>
static T decode(const uint8_t* buf) {
  T out;
  std::memcpy(&out, buf, sizeof(T));
  return out;
}

static bool isFiniteValue(double v) { return std::isfinite(v); }

// Drives a simulator through `ms` of time in `stepMs` increments of millis(),
// collecting every frame that arrives, exactly as the sketch's loop() would.
struct Drive {
  std::vector<std::vector<uint8_t>> frames;
  uint32_t now;
  explicit Drive(uint32_t start = 0) : now(start) {}
  void run(FlightSim& sim, uint32_t ms, uint32_t stepMs = 10) {
    uint8_t buf[FRAME_SIZE];
    for (uint32_t t = 0; t < ms; t += stepMs) {
      now += stepMs;
      while (sim.nextFrame(now, buf)) {
        frames.emplace_back(buf, buf + FRAME_SIZE);
      }
    }
  }
};

// Advances physics only, sampling state every `sampleMs`.
template <typename F>
static void sampleFor(FlightSim& sim, uint32_t& now, uint32_t ms, uint32_t sampleMs, F fn) {
  for (uint32_t t = 0; t < ms; t += sampleMs) {
    now += sampleMs;
    sim.update(now);
    fn(sim.state());
  }
}

// ---------------------------------------------------------------- helpers

static void test_wrap_deg() {
  CHECK_NEAR(wrapDeg(0), 0, 1e-9);
  CHECK_NEAR(wrapDeg(359.5), 359.5, 1e-9);
  CHECK_NEAR(wrapDeg(360), 0, 1e-9);
  CHECK_NEAR(wrapDeg(-0.5), 359.5, 1e-9);
  CHECK_NEAR(wrapDeg(-360), 0, 1e-9);
  CHECK_NEAR(wrapDeg(720.25), 0.25, 1e-9);
  CHECK_NEAR(wrapDeg(-1e6), wrapDeg(-1e6 + 360 * 3000), 1e-6);
  CHECK(wrapDeg(1e9) >= 0 && wrapDeg(1e9) < 360);
  CHECK_NEAR(wrapDeg(NAN), 0, 0);  // never propagate NaN into a heading

  CHECK_NEAR(wrapDeg180(0), 0, 1e-9);
  CHECK_NEAR(wrapDeg180(180), 180, 1e-9);
  CHECK_NEAR(wrapDeg180(181), -179, 1e-9);
  CHECK_NEAR(wrapDeg180(-181), 179, 1e-9);
  CHECK_NEAR(wrapDeg180(359), -1, 1e-9);
  CHECK_NEAR(wrapDeg180(-180), 180, 1e-9);
}

static void test_saturating_casts() {
  // NaN must not reach an integer cast: that is undefined behaviour in C++ and
  // on the board produces whatever the FPU leaves behind.
  CHECK(toI16(NAN) == 0);
  CHECK(toU16(NAN) == 0);
  CHECK(toU8(NAN) == 0);
  CHECK(toI32(NAN) == 0);

  CHECK(toI16(INFINITY) == 32767);
  CHECK(toI16(-INFINITY) == -32768);
  CHECK(toI16(32767.4) == 32767);
  CHECK(toI16(32767.6) == 32767);
  CHECK(toI16(1e12) == 32767);
  CHECK(toI16(-32768.6) == -32768);
  CHECK(toI16(1.5) == 2);
  CHECK(toI16(-1.5) == -2);
  CHECK(toI16(0.49) == 0);

  CHECK(toU16(-1) == 0);
  CHECK(toU16(65535.4) == 65535);
  CHECK(toU16(70000) == 65535);
  CHECK(toU16(INFINITY) == 65535);

  CHECK(toU8(-5) == 0);
  CHECK(toU8(255.4) == 255);
  CHECK(toU8(300) == 255);
  CHECK(toU8(127.5) == 128);

  CHECK(toI32(2147483647.0) == 2147483647);
  CHECK(toI32(1e18) == 2147483647);
  CHECK(toI32(-1e18) == (-2147483647 - 1));
  CHECK(toI32(128437360.4) == 128437360);
}

static void test_lipo_curve() {
  CHECK_NEAR(lipoCellOcv(1.0), 4.20, 1e-9);
  CHECK_NEAR(lipoCellOcv(0.0), 3.30, 1e-9);
  CHECK_NEAR(lipoCellOcv(2.0), 4.20, 1e-9);   // clamped above
  CHECK_NEAR(lipoCellOcv(-1.0), 3.30, 1e-9);  // clamped below
  CHECK_NEAR(lipoCellOcv(NAN), 3.30, 1e-9);   // unknown charge reads as empty

  // Monotonic: more charge never means less voltage. A dip anywhere would show
  // the battery "recovering" mid-flight.
  double prev = lipoCellOcv(0.0);
  bool monotonic = true;
  for (int i = 1; i <= 1000; i++) {
    double v = lipoCellOcv(i / 1000.0);
    if (v < prev) monotonic = false;
    prev = v;
  }
  CHECK(monotonic);

  // The flat middle of a LiPo is what makes a real pack read ~3.8V/cell for most
  // of a flight.
  CHECK(lipoCellOcv(0.5) > 3.75 && lipoCellOcv(0.5) < 3.85);
}

static void test_rng() {
  // xorshift has a fixed point at 0: a zero seed must not lock the generator.
  Rng zero(0);
  uint32_t a = zero.next(), b = zero.next();
  CHECK(a != 0 && b != 0 && a != b);

  Rng r1(42), r2(42), r3(43);
  bool same = true, differ = false;
  for (int i = 0; i < 100; i++) {
    uint32_t x = r1.next(), y = r2.next(), z = r3.next();
    if (x != y) same = false;
    if (x != z) differ = true;
  }
  CHECK(same);
  CHECK(differ);

  Rng u(7);
  bool inRange = true;
  double sum = 0, sumSq = 0;
  const int n = 20000;
  for (int i = 0; i < n; i++) {
    double v = u.uniform();
    if (!(v >= 0.0 && v < 1.0)) inRange = false;
  }
  CHECK(inRange);
  Rng g(9);
  for (int i = 0; i < n; i++) {
    double v = g.gauss();
    sum += v;
    sumSq += v * v;
  }
  double mean = sum / n, sd = std::sqrt(sumSq / n - mean * mean);
  CHECK_NEAR(mean, 0.0, 0.05);
  CHECK_NEAR(sd, 1.0, 0.05);
}

static void test_phase_names() {
  const Phase all[] = {Phase::Boot,       Phase::Standby,    Phase::Arming,
                       Phase::Takeoff,    Phase::Waypoints,  Phase::ReturnHome,
                       Phase::Landing,    Phase::Landed};
  for (size_t i = 0; i < sizeof(all) / sizeof(all[0]); i++) {
    CHECK(phaseName(all[i]) != nullptr);
    for (size_t j = 0; j < i; j++) {
      CHECK(std::strcmp(phaseName(all[i]), phaseName(all[j])) != 0);
    }
  }
  CHECK(std::strcmp(phaseName(static_cast<Phase>(200)), "?") == 0);
}

// ---------------------------------------------------------------- frames

static void test_checksum_helpers() {
  uint8_t buf[FRAME_SIZE] = {0};
  buf[0] = PACKET_MAGIC;
  buf[5] = 0x5A;
  buf[FRAME_SIZE - 1] = calculateChecksum(buf, FRAME_SIZE - 1);
  CHECK(validateFrameChecksum(buf, FRAME_SIZE));
  CHECK(!validateFrameChecksum(buf, FRAME_SIZE - 1));  // wrong size
  CHECK(!validateFrameChecksum(buf, FRAME_SIZE + 1));
  CHECK(calculateChecksum(buf, 0) == 0);
}

static void test_every_frame_validates_and_any_bit_flip_fails() {
  FlightSim sim;
  uint32_t now = 0;
  sampleFor(sim, now, 60000, 100, [](const State&) {});  // mid-flight

  uint8_t buf[FRAME_SIZE];
  for (int type = FRAME_BASIC; type <= FRAME_MOTORS; type++) {
    std::memset(buf, 0xEE, sizeof(buf));  // prove the builder writes every byte
    sim.buildFrame(static_cast<uint8_t>(type), 77, buf);
    CHECK(validateFrameChecksum(buf, FRAME_SIZE));
    CHECK(buf[0] == PACKET_MAGIC);
    CHECK(buf[1] == 77);
    CHECK(buf[2] == type);

    bool allCaught = true;
    for (int byte = 0; byte < FRAME_SIZE - 1; byte++) {
      for (int bit = 0; bit < 8; bit++) {
        uint8_t copy[FRAME_SIZE];
        std::memcpy(copy, buf, FRAME_SIZE);
        copy[byte] ^= (uint8_t)(1u << bit);
        if (validateFrameChecksum(copy, FRAME_SIZE)) allCaught = false;
      }
    }
    CHECK(allCaught);
  }

  // An unknown frame type still produces a valid, typed, otherwise empty frame,
  // which the receiver then ignores. It must not write past the buffer.
  uint8_t guard[FRAME_SIZE + 4];
  std::memset(guard, 0x11, sizeof(guard));
  sim.buildFrame(99, 3, guard);
  CHECK(validateFrameChecksum(guard, FRAME_SIZE));
  CHECK(guard[2] == 99);
  CHECK(guard[FRAME_SIZE] == 0x11 && guard[FRAME_SIZE + 3] == 0x11);
}

static void test_power_frame_layout_quirk() {
  // PowerSbcFrame is 30 bytes. Bytes 30 and 31 of the payload are outside the
  // struct: 30 is zero and 31 is the checksum the receiver actually checks.
  FlightSim sim;
  uint32_t now = 0;
  sampleFor(sim, now, 45000, 100, [](const State&) {});
  uint8_t buf[FRAME_SIZE];
  sim.buildFrame(FRAME_POWER_SBC, 1, buf);
  CHECK(buf[30] == 0);
  CHECK(validateFrameChecksum(buf, FRAME_SIZE));

  PowerSbcFrame p = decode<PowerSbcFrame>(buf);
  const State& s = sim.state();
  CHECK(p.cell_mv[4] == 0 && p.cell_mv[5] == 0);  // 4S pack: cells 5 and 6 unused
  uint16_t lo = 65535, hi = 0;
  for (int i = 0; i < 4; i++) {
    CHECK(p.cell_mv[i] > 3000 && p.cell_mv[i] < 4300);
    if (p.cell_mv[i] < lo) lo = p.cell_mv[i];
    if (p.cell_mv[i] > hi) hi = p.cell_mv[i];
  }
  CHECK(p.cell_delta_mv == hi - lo);
  CHECK_NEAR(p.battery_current_ca / 100.0, s.currentA, 0.006);
  CHECK(p.ram_percent > 0 && p.ram_percent < 100);
  CHECK(p.disk_percent > 0 && p.disk_percent < 100);
}

static void test_frames_decode_to_state() {
  FlightSim sim;
  uint32_t now = 0;
  // Into the waypoint leg, so every field is non-trivial.
  sampleFor(sim, now, 70000, 50, [](const State&) {});
  const State& s = sim.state();
  CHECK(s.phase == Phase::Waypoints);

  uint8_t buf[FRAME_SIZE];
  sim.buildFrame(FRAME_BASIC, 9, buf);
  BasicFrame b = decode<BasicFrame>(buf);
  CHECK_NEAR(b.bat_mv / 1000.0, s.packV, 0.0011);
  CHECK_NEAR(b.alt_cm / 100.0, s.altRel, 0.011);
  CHECK_NEAR(b.lat_e7 / 1e7, s.lat, 2e-7);
  CHECK_NEAR(b.lon_e7 / 1e7, s.lon, 2e-7);
  CHECK_NEAR(b.heading_cd / 100.0, s.heading, 0.011);
  CHECK(b.satellites == s.satellites);
  CHECK(b.gps_fix == s.gpsFix);
  CHECK(b.battery_remaining == s.batteryRemaining);
  CHECK(b.flight_mode == s.flightMode);
  CHECK(b.system_status == s.systemStatus);
  CHECK(b.cell_count == 4);
  CHECK(b.rssi == s.rssi);
  CHECK(b.link_quality == s.linkQuality);
  CHECK(b.mission_progress == s.missionProgress);
  CHECK((b.flags & FRAME_FLAG_ARMED) != 0);
  CHECK((b.flags & FRAME_FLAG_MAVLINK) != 0);

  sim.buildFrame(FRAME_ATTITUDE, 10, buf);
  AttitudeFrame a = decode<AttitudeFrame>(buf);
  CHECK_NEAR(a.roll_cd / 100.0, s.roll, 0.011);
  CHECK_NEAR(a.pitch_cd / 100.0, s.pitch, 0.011);
  CHECK_NEAR(a.yaw_cd / 100.0, s.yaw, 0.011);
  CHECK_NEAR(a.target_roll_cd / 100.0, s.targetRoll, 0.011);
  CHECK_NEAR(a.error_roll_cd / 100.0, s.targetRoll - s.roll, 0.021);
  CHECK_NEAR(a.climb_cms / 100.0, s.climbRate, 0.011);
  CHECK_NEAR(a.altitude_msl_dm / 10.0, s.altMsl, 0.051);
  CHECK_NEAR(a.gyro_z_cdps / 100.0, s.gyroZ, 0.011);

  sim.buildFrame(FRAME_MOTORS, 11, buf);
  MotorsFrame m = decode<MotorsFrame>(buf);
  CHECK(m.mission_total_items == 7);
  CHECK(m.mission_current_seq == s.missionSeq);
  CHECK(m.mission_state == s.missionState);
  CHECK_NEAR(m.ground_speed_cms / 100.0, s.groundSpeed, 0.011);
  CHECK_NEAR(m.accel_z_centi / 100.0, s.accelZ, 0.011);
  CHECK_NEAR(m.dist_to_target_wp_dm / 10.0, s.distToTarget, 0.051);
  for (int i = 0; i < 4; i++) {
    CHECK(m.motor_percent[i] == s.motorPercent[i]);
    // An ESC command and its percent must agree, as on a real flight controller.
    CHECK(std::abs((int)m.motor_pwm[i] - (1000 + 10 * (int)m.motor_percent[i])) <= 5);
  }
}

// ---------------------------------------------------------------- scheduler

static void test_link_comes_up_after_delay() {
  Config cfg;
  cfg.linkDelayMs = 3000;
  FlightSim sim(cfg);
  Drive d(1000);
  d.run(sim, 2900);
  CHECK(d.frames.empty());  // "Waiting NRF link" on the OLED
  d.run(sim, 500);
  CHECK(!d.frames.empty());
}

static void test_frame_rates_match_a_real_link() {
  Config cfg;
  cfg.lossBase = 0;
  cfg.lossPerMeter = 0;
  FlightSim sim(cfg);
  Drive d;
  d.run(sim, cfg.linkDelayMs);  // past the link-up delay
  d.frames.clear();
  d.run(sim, 10000);
  int count[4] = {0, 0, 0, 0};
  for (auto& f : d.frames) {
    CHECK(f[2] <= FRAME_MOTORS);
    if (f[2] <= FRAME_MOTORS) count[f[2]]++;
  }
  CHECK_NEAR(count[FRAME_BASIC], 50, 1);     // 5 Hz
  CHECK_NEAR(count[FRAME_ATTITUDE], 50, 1);  // 5 Hz
  CHECK_NEAR(count[FRAME_MOTORS], 20, 1);    // 2 Hz
  CHECK_NEAR(count[FRAME_POWER_SBC], 10, 1); // 1 Hz
  CHECK(sim.framesLost() == 0);
}

static void test_sequence_increments_and_wraps() {
  Config cfg;
  cfg.lossBase = 0;
  cfg.lossPerMeter = 0;
  FlightSim sim(cfg);
  Drive d;
  d.run(sim, cfg.linkDelayMs + 30000);
  CHECK(d.frames.size() > 300);
  bool consecutive = true, wrapped = false;
  for (size_t i = 1; i < d.frames.size(); i++) {
    uint8_t prev = d.frames[i - 1][1], cur = d.frames[i][1];
    if ((uint8_t)(prev + 1) != cur) consecutive = false;
    if (prev == 255 && cur == 0) wrapped = true;
  }
  CHECK(consecutive);
  CHECK(wrapped);
}

static void test_loss_shows_as_sequence_gaps() {
  FlightSim sim;  // default loss model
  Drive d;
  d.run(sim, sim.config().linkDelayMs + 120000);
  uint32_t sent = sim.framesSent(), lost = sim.framesLost();
  CHECK(sent > 1000);
  CHECK(lost > 0);
  CHECK((double)lost / sent < 0.15);
  CHECK(d.frames.size() == sent - lost);

  // A lost frame still consumed a sequence number, so the receiver sees gaps --
  // which is how a real ground station counts link loss.
  int gaps = 0;
  for (size_t i = 1; i < d.frames.size(); i++) {
    if ((uint8_t)(d.frames[i - 1][1] + 1) != d.frames[i][1]) gaps++;
  }
  CHECK(gaps > 0);
}

static void test_stall_does_not_dump_a_backlog() {
  // setupWifiApi() can block for 15s while joining a network. When loop() runs
  // again the radio would simply have missed those packets; the simulator must
  // not replay 200 of them in one burst.
  FlightSim sim;
  uint8_t buf[FRAME_SIZE];
  uint32_t now = 0;
  for (; now < 6000; now += 10) {
    while (sim.nextFrame(now, buf)) {}
  }
  uint64_t before = sim.state().simMs;
  now += 16000;  // stall
  int burst = 0;
  while (sim.nextFrame(now, buf)) burst++;
  CHECK(burst <= 1);
  // Simulated time moved, but by at most the catch-up cap.
  CHECK(sim.state().simMs - before <= sim.config().maxCatchUpMs);
  CHECK(sim.state().simMs > before);

  // Cadence resumes normally afterwards.
  Drive d(now);
  d.run(sim, 1000);
  CHECK(d.frames.size() >= 10 && d.frames.size() <= 14);
}

static void test_millis_wraparound() {
  // millis() overflows after ~49.7 days. Unsigned subtraction must carry the
  // simulation straight across it.
  Config cfg;
  cfg.lossBase = 0;
  cfg.lossPerMeter = 0;
  FlightSim sim(cfg);
  uint8_t buf[FRAME_SIZE];
  uint32_t now = 0xFFFF0000u;
  uint32_t lastFrameAt = now;
  uint32_t worstGap = 0;
  int frames = 0;
  for (uint32_t i = 0; i < 0x20000u / 10; i++) {
    now += 10;  // wraps through 0
    while (sim.nextFrame(now, buf)) {
      frames++;
      uint32_t gap = now - lastFrameAt;
      if (frames > 1 && gap > worstGap) worstGap = gap;
      lastFrameAt = now;
    }
  }
  CHECK(now < 0x00010000u);  // we really did cross the wrap
  CHECK_NEAR((double)sim.state().simMs, 0x20000 - 10, 30);
  CHECK(frames > 1000);
  CHECK(worstGap <= 200);  // no hole at the wrap
}

static void test_same_millis_twice_is_a_no_op() {
  FlightSim sim;
  uint32_t now = 0;
  sampleFor(sim, now, 40000, 100, [](const State&) {});
  State before = sim.state();
  sim.update(now);
  sim.update(now);
  const State& after = sim.state();
  CHECK(after.simMs == before.simMs);
  CHECK(after.altRel == before.altRel);
  CHECK(isFiniteValue(after.climbRate));
}

static void test_zero_interval_cannot_hang() {
  Config cfg;
  cfg.frameIntervalMs = 0;  // would make "while frame is due" spin for ever
  FlightSim sim(cfg);
  CHECK(sim.config().frameIntervalMs > 0);
  Drive d;
  d.run(sim, cfg.linkDelayMs + 2000);
  CHECK(!d.frames.empty());
}

static void test_determinism() {
  FlightSim a, b;
  Config other;
  other.seed = 0x1234;
  FlightSim c(other);
  Drive da, db, dc;
  da.run(a, 20000);
  db.run(b, 20000);
  dc.run(c, 20000);
  CHECK(da.frames == db.frames);
  CHECK(da.frames != dc.frames);
}

// ---------------------------------------------------------------- flight

static void test_full_mission_cycle() {
  FlightSim sim;
  uint32_t now = 0;
  std::vector<Phase> order;
  double maxAlt = 0, maxDist = 0, maxSpeed = 0, maxTilt = 0;
  bool allFinite = true, rangesOk = true, groundOk = true, progressOk = true;
  bool sagSeen = false, sagViolated = false;
  int lastRemaining = 101;
  bool remainingMonotonic = true;
  uint8_t lastProgress = 0;
  double tempAtArm = -1, tempAfterFlight = -1;
  double rssiNearSum = 0, rssiFarSum = 0;
  int rssiNear = 0, rssiFar = 0;
  double climbTakeoff = 0, climbLanding = 0;
  int nTakeoff = 0, nLanding = 0;

  sampleFor(sim, now, 520000, 50, [&](const State& s) {
    if (order.empty() || order.back() != s.phase) {
      if (s.phase == Phase::Arming && tempAtArm < 0) tempAtArm = s.cpuTemp;
      if (s.phase == Phase::ReturnHome && tempAfterFlight < 0) tempAfterFlight = s.cpuTemp;
      order.push_back(s.phase);
      if (s.phase == Phase::Standby && order.size() > 2) lastRemaining = 101;  // battery swap
      if (s.phase == Phase::Arming) lastProgress = 0;
    }

    const double vals[] = {s.east,    s.north,    s.altRel,     s.climbRate, s.groundSpeed,
                           s.heading, s.roll,     s.pitch,      s.yaw,       s.targetRoll,
                           s.gyroX,   s.gyroY,    s.gyroZ,      s.accelX,    s.accelY,
                           s.accelZ,  s.soc,      s.currentA,   s.packV,     s.lat,
                           s.lon,     s.altMsl,   s.cpuTemp,    s.cpuLoad,   s.distToTarget};
    for (double v : vals) if (!isFiniteValue(v)) allFinite = false;

    double dist = std::hypot(s.east, s.north);
    if (s.altRel > maxAlt) maxAlt = s.altRel;
    if (dist > maxDist) maxDist = dist;
    if (s.groundSpeed > maxSpeed) maxSpeed = s.groundSpeed;
    if (std::fabs(s.roll) > maxTilt) maxTilt = std::fabs(s.roll);
    if (std::fabs(s.pitch) > maxTilt) maxTilt = std::fabs(s.pitch);

    if (s.altRel < -0.5) rangesOk = false;
    if (!(s.heading >= 0 && s.heading < 360)) rangesOk = false;
    if (!(s.yaw >= 0 && s.yaw < 360)) rangesOk = false;
    if (s.rssi < 0 || s.rssi > 100) rangesOk = false;
    if (s.linkQuality > 100) rangesOk = false;
    if (s.satellites > 20) rangesOk = false;
    if (s.gpsFix != 0 && s.gpsFix != 2 && s.gpsFix != 3) rangesOk = false;
    if (s.soc < 0 || s.soc > 1) rangesOk = false;
    if (s.batteryRemaining > 100) rangesOk = false;
    if (s.packV > 4.2 * 4 + 0.05 || s.packV < 3.0 * 4) rangesOk = false;

    bool onGround = s.phase == Phase::Boot || s.phase == Phase::Standby ||
                    s.phase == Phase::Arming || s.phase == Phase::Landed;
    if (onGround && std::fabs(s.altRel) > 0.5) groundOk = false;
    if ((s.phase == Phase::Boot || s.phase == Phase::Standby) && s.armed) groundOk = false;
    if (s.phase == Phase::Arming && !s.armed) groundOk = false;

    if (!s.armed) {
      for (int i = 0; i < 4; i++) if (s.motorPercent[i] != 0) groundOk = false;
    }
    bool flying = s.phase == Phase::Takeoff || s.phase == Phase::Waypoints ||
                  s.phase == Phase::ReturnHome || s.phase == Phase::Landing;
    if (flying) {
      for (int i = 0; i < 4; i++) {
        if (s.motorPercent[i] <= 20 || s.motorPercent[i] > 100) rangesOk = false;
      }
      if (s.missionProgress < lastProgress) progressOk = false;
      lastProgress = s.missionProgress;
    }
    if (s.phase == Phase::Landed && s.missionProgress != 100) progressOk = false;

    if (s.batteryRemaining > lastRemaining) remainingMonotonic = false;
    lastRemaining = s.batteryRemaining;

    if (s.phase == Phase::Takeoff) {
      sagSeen = true;
      if (!(s.packV < 4 * lipoCellOcv(s.soc))) sagViolated = true;
      climbTakeoff += s.climbRate;
      nTakeoff++;
    }
    if (s.phase == Phase::Landing) {
      climbLanding += s.climbRate;
      nLanding++;
    }
    if (s.gpsFix >= 2) {
      if (dist < 20) { rssiNearSum += s.rssi; rssiNear++; }
      if (dist > 100) { rssiFarSum += s.rssi; rssiFar++; }
    }
  });

  const Phase expected[] = {Phase::Boot,     Phase::Standby,    Phase::Arming,
                            Phase::Takeoff,  Phase::Waypoints,  Phase::ReturnHome,
                            Phase::Landing,  Phase::Landed,     Phase::Standby};
  const size_t n = sizeof(expected) / sizeof(expected[0]);
  CHECK(order.size() >= n);
  bool orderOk = true;
  for (size_t i = 0; i < n && i < order.size(); i++) {
    if (order[i] != expected[i]) orderOk = false;
  }
  CHECK(orderOk);
  CHECK(sim.state().cycles >= 1);

  CHECK(allFinite);
  CHECK(rangesOk);
  CHECK(groundOk);
  CHECK(progressOk);
  CHECK(remainingMonotonic);
  CHECK(sagSeen && !sagViolated);
  CHECK(maxAlt > 25 && maxAlt < 60);
  CHECK(maxDist < sim.config().missionRadius + 40);
  CHECK(maxDist > sim.config().missionRadius * 0.6);
  CHECK(maxSpeed <= sim.config().cruiseSpeed + 1.5);
  CHECK(maxSpeed > sim.config().cruiseSpeed * 0.8);
  CHECK(maxTilt < 30);
  CHECK(maxTilt > 3);  // it does bank and pitch, not slide around level
  CHECK(nTakeoff > 0 && climbTakeoff / nTakeoff > 0.5);
  CHECK(nLanding > 0 && climbLanding / nLanding < -0.3);
  CHECK(rssiNear > 0 && rssiFar > 0);
  CHECK(rssiFarSum / rssiFar < rssiNearSum / rssiNear);
  CHECK(tempAtArm > 0 && tempAfterFlight > tempAtArm);
}

static void test_motors_follow_arm_state_on_the_same_step() {
  // The props must stop on the very step the aircraft disarms, and spin up on
  // the step it arms -- not one step late.
  FlightSim sim;
  uint32_t now = 0;
  bool prevArmed = false;
  int disarmEdges = 0, armEdges = 0;
  bool coherent = true;
  for (uint32_t t = 0; t < 230000; t += FlightSim::kStepMs) {
    now += FlightSim::kStepMs;
    sim.update(now);
    const State& s = sim.state();
    bool anySpin = false;
    for (int i = 0; i < 4; i++) if (s.motorPercent[i] != 0) anySpin = true;
    if (s.armed != anySpin) coherent = false;
    if (prevArmed && !s.armed) disarmEdges++;
    if (!prevArmed && s.armed) armEdges++;
    prevArmed = s.armed;
  }
  CHECK(armEdges >= 2);
  CHECK(disarmEdges >= 1);
  CHECK(coherent);
}

static void test_boot_sequence() {
  FlightSim sim;
  uint32_t now = 0;
  bool sawNoHeartbeat = false, sawNoFix = false, sawFix = false;
  bool latZeroWithoutFix = true;
  sampleFor(sim, now, 14000, 50, [&](const State& s) {
    if (s.phase != Phase::Boot) return;
    if (!s.mavlink && s.flightMode == 10) sawNoHeartbeat = true;  // "NO HEARTBEAT"
    if (s.gpsFix == 0) {
      sawNoFix = true;
      if (s.lat != 0.0 || s.lon != 0.0) latZeroWithoutFix = false;  // OLED shows "NO FIX"
    } else {
      sawFix = true;
    }
  });
  CHECK(sawNoHeartbeat);
  CHECK(sawNoFix);
  CHECK(sawFix);
  CHECK(latZeroWithoutFix);

  // Once fixed, the position is the lab origin to within the mission area.
  const State& s = sim.state();
  CHECK(s.gpsFix == 3);
  CHECK_NEAR(s.lat, sim.config().homeLat, 0.01);
  CHECK_NEAR(s.lon, sim.config().homeLon, 0.01);
  CHECK(s.mavlink);
}

static void test_battery_swap_between_cycles() {
  FlightSim sim;
  uint32_t now = 0;
  int remainingAtLanding = -1, remainingAfterSwap = -1;
  Phase prev = Phase::Boot;
  int cycleSeen = 0;
  sampleFor(sim, now, 520000, 100, [&](const State& s) {
    if (s.phase == Phase::Landed && prev != Phase::Landed) remainingAtLanding = s.batteryRemaining;
    if (s.phase == Phase::Standby && prev == Phase::Landed && cycleSeen == 0) {
      remainingAfterSwap = s.batteryRemaining;
      cycleSeen = 1;
    }
    prev = s.phase;
  });
  CHECK(remainingAtLanding > 0 && remainingAtLanding < 95);
  CHECK(remainingAfterSwap >= 97);
}

static void test_imu_vibration_only_when_armed() {
  FlightSim sim;
  uint32_t now = 0;
  double sumG = 0, sumG2 = 0, sumA = 0, sumA2 = 0;
  int nG = 0, nA = 0;
  sampleFor(sim, now, 70000, 50, [&](const State& s) {
    if (s.phase == Phase::Standby) { sumG += s.accelX; sumG2 += s.accelX * s.accelX; nG++; }
    if (s.phase == Phase::Waypoints) { sumA += s.accelX; sumA2 += s.accelX * s.accelX; nA++; }
    if (s.phase == Phase::Standby) {
      CHECK_NEAR(s.accelZ, -9.81, 0.15);  // gravity, sitting on the pad
    }
  });
  CHECK(nG > 10 && nA > 10);
  double sdGround = std::sqrt(sumG2 / nG - (sumG / nG) * (sumG / nG));
  double sdAir = std::sqrt(sumA2 / nA - (sumA / nA) * (sumA / nA));
  CHECK(sdAir > 5 * sdGround);
}

static void test_low_battery_goes_critical_and_never_negative() {
  Config cfg;
  cfg.capacityAh = 0.05;  // drains in seconds
  FlightSim sim(cfg);
  uint32_t now = 0;
  bool critical = false, bad = false;
  sampleFor(sim, now, 120000, 100, [&](const State& s) {
    if (s.armed && s.batteryRemaining < 15 && s.systemStatus == 5) critical = true;
    if (s.soc < 0 || !isFiniteValue(s.packV) || s.packV < 0) bad = true;
  });
  CHECK(critical);
  CHECK(!bad);
}

static void test_battery_failsafe_aborts_the_survey_and_lands() {
  // A pack too small for the mission: the flight controller's battery failsafe
  // must abandon the survey part-way and bring the aircraft home, not fly the
  // remaining waypoints into the ground.
  Config cfg;
  cfg.capacityAh = 0.45;
  FlightSim sim(cfg);
  uint32_t now = 0;
  Phase prev = Phase::Boot;
  uint16_t seqWhenAborted = 0;
  int remainingWhenAborted = -1;
  bool aborted = false, landedAfter = false;
  uint16_t lastSurveySeq = 0;
  for (uint32_t t = 0; t < 300000; t += FlightSim::kStepMs) {
    now += FlightSim::kStepMs;
    sim.update(now);
    const State& s = sim.state();
    if (s.phase == Phase::Waypoints) lastSurveySeq = s.missionSeq;
    if (!aborted && prev == Phase::Waypoints && s.phase == Phase::ReturnHome) {
      aborted = true;
      seqWhenAborted = lastSurveySeq;
      remainingWhenAborted = s.batteryRemaining;
    }
    if (aborted && s.phase == Phase::Landed) {
      landedAfter = true;
      break;
    }
    prev = s.phase;
  }
  CHECK(aborted);
  CHECK(remainingWhenAborted >= 0 && remainingWhenAborted < 15);
  CHECK(seqWhenAborted >= 1 && seqWhenAborted < 5);  // did not reach the last waypoint
  CHECK(landedAfter);
}

static void test_config_is_clamped() {
  Config cfg;
  cfg.cellCount = 0;
  cfg.capacityAh = 0;
  cfg.cruiseSpeed = -3;
  cfg.missionRadius = 0;
  cfg.lossBase = 5;
  cfg.lossPerMeter = -1;
  cfg.maxCatchUpMs = 0;
  FlightSim sim(cfg);
  const Config& c = sim.config();
  CHECK(c.cellCount == 1);
  CHECK(c.capacityAh > 0);
  CHECK(c.cruiseSpeed > 0);
  CHECK(c.missionRadius > 0);
  CHECK(c.lossBase <= 1 && c.lossBase >= 0);
  CHECK(c.lossPerMeter >= 0);
  CHECK(c.maxCatchUpMs >= 20);

  Config big;
  big.cellCount = 9;
  FlightSim six(big);
  CHECK(six.config().cellCount == 6);
  uint32_t now = 0;
  sampleFor(six, now, 20000, 100, [](const State&) {});
  uint8_t buf[FRAME_SIZE];
  six.buildFrame(FRAME_POWER_SBC, 0, buf);
  PowerSbcFrame p = decode<PowerSbcFrame>(buf);
  for (int i = 0; i < 6; i++) CHECK(p.cell_mv[i] > 3000);

  // A fully lossy link sends but delivers nothing, and does not hang.
  Config lossy;
  lossy.lossBase = 1.0;
  FlightSim dead(lossy);
  Drive d;
  d.run(dead, lossy.linkDelayMs + 3000);
  CHECK(d.frames.empty());
  CHECK(dead.framesSent() > 0);

  // Zero-capacity battery must not divide by zero.
  FlightSim zero(cfg);
  uint32_t t = 0;
  bool ok = true;
  sampleFor(zero, t, 60000, 100, [&](const State& s) {
    if (!isFiniteValue(s.soc) || !isFiniteValue(s.packV)) ok = false;
  });
  CHECK(ok);
}

int main() {
  struct { const char* name; void (*fn)(); } tests[] = {
      {"wrap_deg", test_wrap_deg},
      {"saturating_casts", test_saturating_casts},
      {"lipo_curve", test_lipo_curve},
      {"rng", test_rng},
      {"phase_names", test_phase_names},
      {"checksum_helpers", test_checksum_helpers},
      {"every_frame_validates_and_any_bit_flip_fails", test_every_frame_validates_and_any_bit_flip_fails},
      {"power_frame_layout_quirk", test_power_frame_layout_quirk},
      {"frames_decode_to_state", test_frames_decode_to_state},
      {"link_comes_up_after_delay", test_link_comes_up_after_delay},
      {"frame_rates_match_a_real_link", test_frame_rates_match_a_real_link},
      {"sequence_increments_and_wraps", test_sequence_increments_and_wraps},
      {"loss_shows_as_sequence_gaps", test_loss_shows_as_sequence_gaps},
      {"stall_does_not_dump_a_backlog", test_stall_does_not_dump_a_backlog},
      {"millis_wraparound", test_millis_wraparound},
      {"same_millis_twice_is_a_no_op", test_same_millis_twice_is_a_no_op},
      {"zero_interval_cannot_hang", test_zero_interval_cannot_hang},
      {"determinism", test_determinism},
      {"full_mission_cycle", test_full_mission_cycle},
      {"motors_follow_arm_state_on_the_same_step", test_motors_follow_arm_state_on_the_same_step},
      {"boot_sequence", test_boot_sequence},
      {"battery_swap_between_cycles", test_battery_swap_between_cycles},
      {"imu_vibration_only_when_armed", test_imu_vibration_only_when_armed},
      {"low_battery_goes_critical_and_never_negative", test_low_battery_goes_critical_and_never_negative},
      {"battery_failsafe_aborts_the_survey_and_lands", test_battery_failsafe_aborts_the_survey_and_lands},
      {"config_is_clamped", test_config_is_clamped},
  };
  for (auto& t : tests) {
    int before = g_failures;
    t.fn();
    std::printf("%s %s\n", g_failures == before ? "ok  " : "FAIL", t.name);
  }
  std::printf("\n%d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
