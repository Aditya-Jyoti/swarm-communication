// flight_sim.h -- a simulated drone for the ground unit, flying a repeating
// survey mission and transmitting the same telemetry frames a real one would.
//
// It produces 32-byte frames in the telemetry_frames.h wire format, at the rates
// a real 250 kbps NRF24 link carries them, and the sketch feeds them through its
// ordinary receive path. The OLED and /telemetry.json therefore cannot tell the
// difference -- which is the point, and also why the sketch keeps this behind an
// explicit DRONE_SIM_MODE switch and reports "source":"esp32-sim".
//
// What makes it read as a real aircraft is that the numbers are coupled, not
// independently random: battery voltage sags under throttle and the charge
// drains with current; climb rate is the derivative of altitude; heading follows
// the ground track; the airframe banks into turns and pitches to accelerate;
// motor commands follow thrust and attitude; RSSI falls off with distance and
// packet loss rises with it; the companion computer warms up in flight.
//
// Plain C++ with no Arduino headers, deterministic for a given seed, and driven
// only by the millis() values passed in, so esp/test/ checks it on a PC.

#ifndef FLIGHT_SIM_H
#define FLIGHT_SIM_H

#include <math.h>
#include <stdint.h>
#include <string.h>

#include "telemetry_frames.h"

namespace flightsim {

// ------------------------------------------------------------------ helpers

constexpr double kPi = 3.14159265358979323846;
constexpr double kGravity = 9.80665;
constexpr double kEarthRadius = 6378137.0;  // WGS-84 equatorial, metres

inline double deg2rad(double d) { return d * kPi / 180.0; }
inline double rad2deg(double r) { return r * 180.0 / kPi; }

inline double clampd(double v, double lo, double hi) {
  return v < lo ? lo : (v > hi ? hi : v);
}

// Wraps to [0, 360). Non-finite input becomes 0 rather than NaN, because a NaN
// heading would poison every derived value downstream.
inline double wrapDeg(double d) {
  if (!isfinite(d)) return 0.0;
  double r = fmod(d, 360.0);
  if (r < 0) r += 360.0;
  if (r >= 360.0) r -= 360.0;  // -1e-20 + 360 rounds to exactly 360.0
  return r;
}

// Wraps to (-180, 180]: the signed shortest turn.
inline double wrapDeg180(double d) {
  double r = wrapDeg(d);
  return r > 180.0 ? r - 360.0 : r;
}

// Saturating, rounding casts for packing engineering values into the frame.
// Casting NaN or an out-of-range double to an integer is undefined behaviour, so
// every value goes through one of these rather than a bare cast.
inline int16_t toI16(double v) {
  if (isnan(v)) return 0;
  double r = round(v);
  if (r >= 32767.0) return 32767;
  if (r <= -32768.0) return -32768;
  return static_cast<int16_t>(r);
}
inline uint16_t toU16(double v) {
  if (isnan(v)) return 0;
  double r = round(v);
  if (r >= 65535.0) return 65535;
  if (r <= 0.0) return 0;
  return static_cast<uint16_t>(r);
}
inline uint8_t toU8(double v) {
  if (isnan(v)) return 0;
  double r = round(v);
  if (r >= 255.0) return 255;
  if (r <= 0.0) return 0;
  return static_cast<uint8_t>(r);
}
inline int32_t toI32(double v) {
  if (isnan(v)) return 0;
  double r = round(v);
  if (r >= 2147483647.0) return 2147483647;
  if (r <= -2147483648.0) return -2147483647 - 1;
  return static_cast<int32_t>(r);
}

// Degrees to centidegrees in [0, 35999]. 359.996 rounds to 36000, which is the
// same direction as 0 and must be sent as 0.
inline uint16_t toCentiDeg360(double deg) {
  uint16_t cd = toU16(wrapDeg(deg) * 100.0);
  return cd >= 36000 ? static_cast<uint16_t>(cd - 36000) : cd;
}

// Resting (open-circuit) voltage of one LiPo cell at state of charge `soc`.
//
// Piecewise linear through typical discharge points. The shape matters more
// than the exact values: steep at both ends and flat through the middle, which
// is why a real pack reads ~3.8 V/cell for most of a flight and then falls away.
inline double lipoCellOcv(double soc) {
  static const double kSoc[] = {0.00, 0.05, 0.10, 0.20, 0.40, 0.60, 0.80, 0.95, 1.00};
  static const double kVolt[] = {3.30, 3.55, 3.65, 3.70, 3.76, 3.83, 3.97, 4.12, 4.20};
  const int n = sizeof(kSoc) / sizeof(kSoc[0]);
  if (!(soc > 0.0)) return kVolt[0];  // also catches NaN: unknown reads as empty
  if (soc >= 1.0) return kVolt[n - 1];
  for (int i = 1; i < n; i++) {
    if (soc <= kSoc[i]) {
      double f = (soc - kSoc[i - 1]) / (kSoc[i] - kSoc[i - 1]);
      return kVolt[i - 1] + f * (kVolt[i] - kVolt[i - 1]);
    }
  }
  return kVolt[n - 1];  // unreachable: soc < 1 always lands in a segment
}

// xorshift32. Small, fast on an ESP32, and reproducible: the same seed gives the
// same flight, which is what makes the unit tests deterministic.
class Rng {
 public:
  // xorshift has a fixed point at 0, so a zero seed would emit zeros for ever.
  explicit Rng(uint32_t seed) : s_(seed ? seed : 0x9E3779B9u) {}
  uint32_t next() {
    s_ ^= s_ << 13;
    s_ ^= s_ >> 17;
    s_ ^= s_ << 5;
    return s_;
  }
  // [0, 1), from the top 24 bits so it is exact in a double.
  double uniform() { return (next() >> 8) * (1.0 / 16777216.0); }
  // Standard normal by Box-Muller. 1 - uniform() is in (0, 1], so log never
  // sees zero.
  double gauss() {
    double u1 = 1.0 - uniform();
    double u2 = uniform();
    return sqrt(-2.0 * log(u1)) * cos(2.0 * kPi * u2);
  }

 private:
  uint32_t s_;
};

// ------------------------------------------------------------------ model

enum class Phase : uint8_t {
  Boot,        // power on: companion computer boots, GPS acquires
  Standby,     // disarmed on the pad, mission loaded
  Arming,      // armed, motors spinning at idle
  Takeoff,     // vertical climb to the first mission altitude
  Waypoints,   // flying the survey pattern
  ReturnHome,  // RTL: back over the launch point
  Landing,     // vertical descent
  Landed,      // on the ground; disarms, then the battery is swapped
};

inline const char* phaseName(Phase p) {
  switch (p) {
    case Phase::Boot: return "BOOT";
    case Phase::Standby: return "STANDBY";
    case Phase::Arming: return "ARMING";
    case Phase::Takeoff: return "TAKEOFF";
    case Phase::Waypoints: return "WAYPOINTS";
    case Phase::ReturnHome: return "RTL";
    case Phase::Landing: return "LANDING";
    case Phase::Landed: return "LANDED";
  }
  return "?";
}

// Codes the receiver turns into labels: see flightModeLabel, systemStatusLabel
// and missionStateLabel in esp.ino.
namespace code {
constexpr uint8_t kModeStabilize = 1;
constexpr uint8_t kModeAuto = 3;
constexpr uint8_t kModeRtl = 6;
constexpr uint8_t kModeLand = 7;
constexpr uint8_t kModeNoHeartbeat = 10;

constexpr uint8_t kStatusBoot = 1;
constexpr uint8_t kStatusCalibrating = 2;
constexpr uint8_t kStatusStandby = 3;
constexpr uint8_t kStatusActive = 4;
constexpr uint8_t kStatusCritical = 5;

constexpr uint8_t kMissionStandby = 0;
constexpr uint8_t kMissionReady = 1;
constexpr uint8_t kMissionTakeoff = 2;
constexpr uint8_t kMissionWaypoint = 3;
constexpr uint8_t kMissionRtl = 4;
constexpr uint8_t kMissionLanding = 5;
constexpr uint8_t kMissionComplete = 7;

constexpr uint8_t kFixNone = 0;
constexpr uint8_t kFix2d = 2;
constexpr uint8_t kFix3d = 3;
}  // namespace code

struct Config {
  uint32_t seed = 0xC0FFEE11u;
  // Launch point. The same VIT lab origin as MAV_ORIGIN_* in .env.example, so the
  // ground unit and QGroundControl show the same place.
  double homeLat = 12.843736;
  double homeLon = 80.154725;
  double homeAltMsl = 15.0;
  // How long the OLED shows "Waiting NRF link" before the first frame arrives.
  uint32_t linkDelayMs = 3000;
  // One radio slot. 20 slots make the one-second schedule in FlightSim::slotType.
  uint32_t frameIntervalMs = 50;
  uint8_t cellCount = 4;      // 4S LiPo
  double capacityAh = 3.3;    // 3300 mAh
  double cruiseSpeed = 8.0;   // m/s between waypoints
  double missionRadius = 120.0;  // m, radius of the survey pentagon
  // Packet loss probability: lossBase at the pad, plus lossPerMeter per metre of
  // range. The receiver sees it as gaps in the sequence counter.
  double lossBase = 0.01;
  double lossPerMeter = 0.0003;
  // Longest stretch simulated in one update(). A loop() that was blocked (Wi-Fi
  // join can take 15 s) resumes slightly later in the flight instead of
  // integrating the whole gap.
  uint32_t maxCatchUpMs = 5000;
};

// Everything the simulator knows, in engineering units. The frames are packed
// from this, and the tests compare the two.
struct State {
  Phase phase = Phase::Boot;
  uint32_t phaseMs = 0;  // time spent in the current phase
  uint64_t simMs = 0;    // simulated time since the first update
  uint32_t cycles = 0;   // completed missions

  bool armed = false;
  bool mavlink = false;  // companion computer is seeing flight-controller heartbeats

  // Position relative to launch, metres (east, north, up from the pad).
  double east = 0, north = 0;
  double altRel = 0;       // barometric altitude above launch, with sensor noise
  double altMsl = 0;
  double climbRate = 0;    // m/s, positive up
  double groundSpeed = 0;  // m/s
  double lat = 0, lon = 0; // 0,0 until the GPS has a fix
  double heading = 0;      // degrees true, [0, 360)

  double roll = 0, pitch = 0, yaw = 0;          // measured attitude, degrees
  double targetRoll = 0, targetPitch = 0, targetYaw = 0;  // controller setpoints
  double gyroX = 0, gyroY = 0, gyroZ = 0;       // deg/s
  double accelX = 0, accelY = 0, accelZ = 0;    // m/s^2, body frame; z is -g at rest

  double soc = 1.0;        // state of charge, 0..1
  double currentA = 0;
  double packV = 0;
  double cellV[6] = {0, 0, 0, 0, 0, 0};
  uint16_t cellDeltaMv = 0;
  uint8_t batteryRemaining = 100;

  uint8_t satellites = 0;
  uint8_t gpsFix = code::kFixNone;
  int16_t rssi = 0;        // FlySky receiver RSSI, percent
  uint8_t linkQuality = 100;

  uint8_t flightMode = code::kModeNoHeartbeat;
  uint8_t systemStatus = code::kStatusBoot;
  uint8_t missionState = code::kMissionStandby;
  uint16_t missionSeq = 0;
  uint8_t missionProgress = 0;
  double distToTarget = 0;  // metres to the current mission target

  uint8_t motorPercent[4] = {0, 0, 0, 0};  // FR, RL, FL, RR (quad X)
  uint16_t motorPwm[4] = {1000, 1000, 1000, 1000};

  double cpuLoad = 0, cpuTemp = 0;  // companion computer (Raspberry Pi)
  uint8_t ramPercent = 0, diskPercent = 0;
  uint32_t uptimeS = 0;
};

class FlightSim {
 public:
  static constexpr uint8_t kMissionItems = 7;  // takeoff, 5 waypoints, RTL
  static constexpr uint32_t kStepMs = 20;      // physics step, 50 Hz

  explicit FlightSim(const Config& cfg = Config())
      : cfg_(sanitize(cfg)), rng_(cfg_.seed), rngLink_(cfg_.seed ^ 0xA5A5F00Du) {
    layoutMission();
    for (int i = 0; i < 6; i++) cellOffset_[i] = (rng_.uniform() - 0.5) * 0.014;
    s_.heading = rng_.uniform() * 360.0;  // pointing wherever it was set down
    hdgTarget_ = s_.heading;
    s_.yaw = s_.heading;
    s_.targetYaw = s_.heading;
    s_.soc = 0.99;
    tempTrue_ = 47.0;
    s_.diskPercent = 41;
    s_.accelZ = -kGravity;
    refreshPower(0.45, 0.0);
    refreshDerived();
  }

  const Config& config() const { return cfg_; }
  const State& state() const { return s_; }
  uint32_t framesSent() const { return sent_; }
  uint32_t framesLost() const { return lost_; }

  // Advances the simulation to `nowMs`, a millis() reading.
  //
  // The elapsed time is unsigned subtraction, so it is correct straight across
  // millis() wrapping after ~49.7 days. Physics runs in fixed 20 ms steps with
  // the remainder carried, so the flight is the same however often loop() calls.
  void update(uint32_t nowMs) {
    if (!started_) {
      started_ = true;
      lastMs_ = nowMs;
      return;
    }
    uint32_t dt = nowMs - lastMs_;
    lastMs_ = nowMs;
    if (dt == 0) return;
    if (dt > cfg_.maxCatchUpMs) dt = cfg_.maxCatchUpMs;
    accMs_ += dt;
    while (accMs_ >= kStepMs) {
      accMs_ -= kStepMs;
      step(kStepMs / 1000.0);
      s_.simMs += kStepMs;
    }
  }

  // Returns the next frame that has arrived "over the radio" by `nowMs`.
  //
  // Call it in a loop until it returns false, exactly like draining
  // radio.available(). Frames go out on a fixed one-second slot schedule; a
  // lost frame still uses up its sequence number, so the receiver sees a gap.
  bool nextFrame(uint32_t nowMs, uint8_t out[FRAME_SIZE]) {
    update(nowMs);
    if (s_.simMs < cfg_.linkDelayMs) return false;
    if (!slotsStarted_) {
      slotsStarted_ = true;
      nextSlotMs_ = s_.simMs;
    }
    // A receiver that was busy just missed those packets. Replaying a backlog
    // would show a burst the real link never produces.
    if (s_.simMs > nextSlotMs_ + kBacklogMs) nextSlotMs_ = s_.simMs;

    while (s_.simMs >= nextSlotMs_) {
      uint8_t type = slotType(slot_);
      slot_++;
      nextSlotMs_ += cfg_.frameIntervalMs;
      if (type == kNoFrame) continue;

      uint8_t seq = seq_++;
      sent_++;
      if (rngLink_.uniform() < lossProbability()) {
        lost_++;
        continue;
      }
      buildFrame(type, seq, out);
      return true;
    }
    return false;
  }

  // Packs the current state into one 32-byte frame of the given type. Every
  // byte is written: unused ones are zero and byte 31 is the XOR checksum the
  // receiver validates. An unknown type yields a header-only frame.
  void buildFrame(uint8_t type, uint8_t seq, uint8_t out[FRAME_SIZE]) const {
    memset(out, 0, FRAME_SIZE);
    uint8_t flags = (s_.armed ? FRAME_FLAG_ARMED : 0) | (s_.mavlink ? FRAME_FLAG_MAVLINK : 0);

    switch (type) {
      case FRAME_BASIC: {
        BasicFrame f;
        memset(&f, 0, sizeof(f));
        f.bat_mv = toU16(s_.packV * 1000.0);
        f.rssi = s_.rssi;
        f.alt_cm = toI32(s_.altRel * 100.0);
        f.lat_e7 = toI32(s_.lat * 1e7);
        f.lon_e7 = toI32(s_.lon * 1e7);
        f.satellites = s_.satellites;
        f.heading_cd = toCentiDeg360(s_.heading);
        f.battery_remaining = s_.batteryRemaining;
        f.gps_fix = s_.gpsFix;
        f.flight_mode = s_.flightMode;
        f.system_status = s_.systemStatus;
        f.link_quality = s_.linkQuality;
        f.mission_progress = s_.missionProgress;
        f.cell_count = cfg_.cellCount;
        memcpy(out, &f, sizeof(f));
        break;
      }
      case FRAME_POWER_SBC: {
        PowerSbcFrame f;
        memset(&f, 0, sizeof(f));
        f.battery_current_ca = toI16(s_.currentA * 100.0);
        for (int i = 0; i < 6; i++) {
          f.cell_mv[i] = i < cfg_.cellCount ? toU16(s_.cellV[i] * 1000.0) : 0;
        }
        f.cell_delta_mv = s_.cellDeltaMv;
        f.cpu_load_percent = toU8(s_.cpuLoad);
        f.cpu_temp_dc = toI16(s_.cpuTemp * 10.0);
        f.ram_percent = s_.ramPercent;
        f.disk_percent = s_.diskPercent;
        f.uptime_minutes = toU16(s_.uptimeS / 60.0);
        memcpy(out, &f, sizeof(f));  // 30 bytes; 30 and 31 stay outside it
        break;
      }
      case FRAME_ATTITUDE: {
        AttitudeFrame f;
        memset(&f, 0, sizeof(f));
        f.roll_cd = toI16(s_.roll * 100.0);
        f.pitch_cd = toI16(s_.pitch * 100.0);
        f.yaw_cd = toCentiDeg360(s_.yaw);
        f.target_roll_cd = toI16(s_.targetRoll * 100.0);
        f.target_pitch_cd = toI16(s_.targetPitch * 100.0);
        f.target_yaw_cd = toCentiDeg360(s_.targetYaw);
        f.error_roll_cd = toI16((s_.targetRoll - s_.roll) * 100.0);
        f.error_pitch_cd = toI16((s_.targetPitch - s_.pitch) * 100.0);
        f.gyro_x_cdps = toI16(s_.gyroX * 100.0);
        f.gyro_y_cdps = toI16(s_.gyroY * 100.0);
        f.gyro_z_cdps = toI16(s_.gyroZ * 100.0);
        f.climb_cms = toI16(s_.climbRate * 100.0);
        f.altitude_msl_dm = toI16(s_.altMsl * 10.0);
        memcpy(out, &f, sizeof(f));
        break;
      }
      case FRAME_MOTORS: {
        MotorsFrame f;
        memset(&f, 0, sizeof(f));
        for (int i = 0; i < 4; i++) {
          f.motor_percent[i] = s_.motorPercent[i];
          f.motor_pwm[i] = s_.motorPwm[i];
        }
        f.mission_current_seq = s_.missionSeq;
        f.mission_total_items = kMissionItems;
        f.dist_to_target_wp_dm = toU16(s_.distToTarget * 10.0);
        f.ground_speed_cms = toU16(s_.groundSpeed * 100.0);
        f.accel_x_centi = toI16(s_.accelX * 100.0);
        f.accel_y_centi = toI16(s_.accelY * 100.0);
        f.accel_z_centi = toI16(s_.accelZ * 100.0);
        f.mission_state = s_.missionState;
        memcpy(out, &f, sizeof(f));
        break;
      }
      default:
        break;
    }

    out[0] = PACKET_MAGIC;
    out[1] = seq;
    out[2] = type;
    out[3] = flags;
    out[FRAME_SIZE - 1] = calculateChecksum(out, FRAME_SIZE - 1);
  }

 private:
  static constexpr uint8_t kNoFrame = 0xFF;
  static constexpr uint32_t kBacklogMs = 200;
  static constexpr int kSlotCount = 20;

  // One second of radio slots: BASIC and ATTITUDE at 5 Hz, MOTORS at 2 Hz,
  // POWER_SBC at 1 Hz, with gaps -- 13 frames a second, well inside 250 kbps.
  //
  // A function-local table rather than a static constexpr array member: indexing
  // that member odr-uses it, which needs an out-of-class definition before C++17,
  // and arduino-esp32 2.x compiles as gnu++11.
  static uint8_t slotType(uint32_t slot) {
    static const uint8_t kSlots[kSlotCount] = {
        FRAME_BASIC,    FRAME_MOTORS, FRAME_ATTITUDE,  kNoFrame,    FRAME_BASIC,
        kNoFrame,       FRAME_ATTITUDE, FRAME_POWER_SBC, FRAME_BASIC, kNoFrame,
        FRAME_ATTITUDE, FRAME_MOTORS, FRAME_BASIC,     kNoFrame,    FRAME_ATTITUDE,
        kNoFrame,       FRAME_BASIC,  kNoFrame,        FRAME_ATTITUDE, kNoFrame};
    return kSlots[slot % kSlotCount];
  }

  static constexpr int kWaypoints = 5;
  static constexpr double kTakeoffAlt = 30.0;
  static constexpr double kAccel = 2.5;     // m/s^2 horizontal
  static constexpr double kRint = 0.012;    // ohms per cell
  static constexpr uint32_t kPiBootMs = 95000;  // the Pi was up before the link

  static Config sanitize(Config c) {
    if (c.cellCount < 1) c.cellCount = 1;
    if (c.cellCount > 6) c.cellCount = 6;
    if (!(c.capacityAh >= 0.05)) c.capacityAh = 0.05;  // also NaN
    if (!(c.cruiseSpeed >= 1.0)) c.cruiseSpeed = 1.0;
    if (!(c.missionRadius >= 10.0)) c.missionRadius = 10.0;
    c.lossBase = isfinite(c.lossBase) ? clampd(c.lossBase, 0.0, 1.0) : 0.0;
    c.lossPerMeter = isfinite(c.lossPerMeter) ? clampd(c.lossPerMeter, 0.0, 1.0) : 0.0;
    if (c.maxCatchUpMs < kStepMs) c.maxCatchUpMs = kStepMs;
    if (c.frameIntervalMs == 0) c.frameIntervalMs = 50;  // 0 would spin nextFrame for ever
    // Keep cos(lat) well away from zero: the projection divides by it.
    c.homeLat = isfinite(c.homeLat) ? clampd(c.homeLat, -85.0, 85.0) : 0.0;
    c.homeLon = isfinite(c.homeLon) ? clampd(c.homeLon, -180.0, 180.0) : 0.0;
    if (!isfinite(c.homeAltMsl)) c.homeAltMsl = 0.0;
    return c;
  }

  // A pentagon around the launch point, each corner at a different height, so
  // the altitude readout moves during the survey and not only at takeoff.
  void layoutMission() {
    static const double kAlt[kWaypoints] = {30, 40, 35, 45, 32};
    for (int i = 0; i < kWaypoints; i++) {
      double a = deg2rad(18.0 + 72.0 * i);
      wpE_[i] = cfg_.missionRadius * sin(a);
      wpN_[i] = cfg_.missionRadius * cos(a);
      wpAlt_[i] = kAlt[i];
    }
  }

  double horizontalDistance() const { return hypot(posE_, posN_); }

  double lossProbability() const {
    return clampd(cfg_.lossBase + cfg_.lossPerMeter * horizontalDistance(), 0.0, 1.0);
  }

  bool airborne() const {
    return s_.phase == Phase::Takeoff || s_.phase == Phase::Waypoints ||
           s_.phase == Phase::ReturnHome || s_.phase == Phase::Landing;
  }

  void enter(Phase p) {
    s_.phase = p;
    s_.phaseMs = 0;
    switch (p) {
      case Phase::Arming:
        progressMax_ = 0;
        break;
      case Phase::Waypoints:
        wpIndex_ = 0;
        legLen_ = legLength();
        break;
      case Phase::ReturnHome:
        rtlAlt_ = posU_;
        rtlLen_ = horizontalDistance() > 1.0 ? horizontalDistance() : 1.0;
        break;
      case Phase::Landing:
        landStartAlt_ = posU_ > 0.5 ? posU_ : 0.5;
        break;
      case Phase::Landed:
        progressMax_ = 1.0;
        break;
      default:
        break;
    }
  }

  double legLength() const {
    double d = hypot(hypot(wpE_[wpIndex_] - posE_, wpN_[wpIndex_] - posN_),
                     wpAlt_[wpIndex_] - posU_);
    return d > 1.0 ? d : 1.0;
  }

  // ---------------------------------------------------------------- one step

  void step(double dt) {
    s_.phaseMs += kStepMs;
    advancePhase();
    // Before any physics: the motors, current draw and IMU of this step must all
    // see the same arm state. Deciding it afterwards left the props at idle for
    // one step after a disarm.
    updateArmed();

    // Where the mission wants the aircraft.
    double tgtE = 0, tgtN = 0, tgtAlt = 0;
    switch (s_.phase) {
      case Phase::Takeoff: tgtAlt = kTakeoffAlt; break;
      case Phase::Waypoints:
        tgtE = wpE_[wpIndex_];
        tgtN = wpN_[wpIndex_];
        tgtAlt = wpAlt_[wpIndex_];
        break;
      case Phase::ReturnHome: tgtAlt = rtlAlt_; break;
      default: break;
    }

    double prevSpeed = speed_;
    double prevVz = velU_;
    integrate(dt, tgtE, tgtN, tgtAlt);
    speed_ = hypot(velE_, velN_);
    double aVert = (velU_ - prevVz) / dt;
    double fwdAccel = (speed_ - prevSpeed) / dt;
    fwdFilt_ += (fwdAccel - fwdFilt_) * clampd(dt / 0.3, 0.0, 1.0);

    // Heading follows the ground track; a multirotor in AUTO yaws to face its
    // next waypoint. Below walking pace the track is noise, so hold.
    if (speed_ > 0.6) hdgTarget_ = wrapDeg(rad2deg(atan2(velE_, velN_)));
    double turn = clampd(wrapDeg180(hdgTarget_ - s_.heading), -90.0 * dt, 90.0 * dt);
    s_.heading = wrapDeg(s_.heading + turn);
    double turnRate = turn / dt;

    attitude(dt, turnRate);
    imu(aVert);
    motors(turnRate);

    double current;
    if (!s_.armed) {
      current = 0.45;  // flight controller, Pi and GPS
    } else if (!airborne()) {
      current = 2.8;   // props at idle
    } else {
      current = 12.5 + 2.8 * (velU_ > 0 ? velU_ : 0) + 0.05 * speed_ * speed_;
    }
    currentNoise_ += -currentNoise_ * dt / 2.0 + 0.25 * sqrt(dt) * rng_.gauss();
    current += currentNoise_ * (s_.armed ? 1.0 : 0.2);
    if (current < 0.05) current = 0.05;
    refreshPower(current, dt);

    sensors(dt);
    companion(dt);
    refreshDerived();
  }

  void advancePhase() {
    switch (s_.phase) {
      case Phase::Boot:
        if (s_.phaseMs >= 12000) enter(Phase::Standby);
        break;
      case Phase::Standby:
        if (s_.phaseMs >= 8000) enter(Phase::Arming);
        break;
      case Phase::Arming:
        if (s_.phaseMs >= 3000) enter(Phase::Takeoff);
        break;
      case Phase::Takeoff:
        if (s_.batteryRemaining < 15) enter(Phase::ReturnHome);  // battery failsafe
        else if (posU_ >= kTakeoffAlt - 0.5) enter(Phase::Waypoints);
        break;
      case Phase::Waypoints: {
        if (s_.batteryRemaining < 15) {
          enter(Phase::ReturnHome);
          break;
        }
        double d = hypot(hypot(wpE_[wpIndex_] - posE_, wpN_[wpIndex_] - posN_),
                         wpAlt_[wpIndex_] - posU_);
        if (d < 3.0) {
          if (wpIndex_ + 1 >= kWaypoints) {
            enter(Phase::ReturnHome);
          } else {
            wpIndex_++;
            legLen_ = legLength();
          }
        }
        break;
      }
      case Phase::ReturnHome:
        if (horizontalDistance() < 1.5 && speed_ < 1.5) enter(Phase::Landing);
        break;
      case Phase::Landing:
        if (posU_ <= 0.02) {
          posU_ = 0;
          velU_ = 0;
          enter(Phase::Landed);
        }
        break;
      case Phase::Landed:
        if (s_.phaseMs >= 20000) {
          // A fresh battery, and around again.
          s_.soc = 0.995;
          s_.cycles++;
          enter(Phase::Standby);
        }
        break;
    }
  }

  void integrate(double dt, double tgtE, double tgtN, double tgtAlt) {
    if (!airborne()) {
      velE_ = velN_ = velU_ = 0;
      posU_ = 0;
      return;
    }
    // Horizontal: head for the target at cruise speed, slowing so as to arrive
    // rather than overshoot. Takeoff and landing only station-keep.
    double dx = tgtE - posE_, dy = tgtN - posN_;
    double d = hypot(dx, dy);
    double vmax = (s_.phase == Phase::Takeoff || s_.phase == Phase::Landing) ? 1.0 : cfg_.cruiseSpeed;
    double vdes = sqrt(2.0 * kAccel * d) * 0.9;
    if (vdes > vmax) vdes = vmax;
    if (d < 0.2) vdes = 0;
    double desE = d > 1e-6 ? dx / d * vdes : 0.0;
    double desN = d > 1e-6 ? dy / d * vdes : 0.0;
    double dvE = desE - velE_, dvN = desN - velN_;
    double dv = hypot(dvE, dvN), maxDv = kAccel * dt;
    if (dv > maxDv) {
      dvE *= maxDv / dv;
      dvN *= maxDv / dv;
    }
    velE_ += dvE;
    velN_ += dvN;
    posE_ += velE_ * dt;
    posN_ += velN_ * dt;

    // Vertical: proportional climb, limited to 2.5 up and 1.5 down; landing
    // slows down near the ground.
    double vzDes;
    if (s_.phase == Phase::Landing) {
      vzDes = posU_ > 10.0 ? -1.5 : (posU_ > 1.5 ? -0.6 : -0.35);
    } else {
      vzDes = clampd((tgtAlt - posU_) * 0.8, -1.5, 2.5);
    }
    velU_ += clampd(vzDes - velU_, -3.0 * dt, 3.0 * dt);
    posU_ += velU_ * dt;
    // Defensive: landing descends at most 7 mm a step and touches down at 20 mm,
    // so this does not trigger today. It stays because a tuning change to the
    // descent rate must not be able to put the aircraft underground.
    if (posU_ < 0) {
      posU_ = 0;
      velU_ = 0;
    }
  }

  void attitude(double dt, double turnRate) {
    double tRoll = 0, tPitch = 0;
    if (airborne()) {
      // A coordinated turn banks by atan(v * omega / g). Pitching nose down is
      // how a multirotor accelerates and holds speed against drag.
      tRoll = clampd(rad2deg(atan(speed_ * deg2rad(turnRate) / kGravity)), -25.0, 25.0);
      tPitch = clampd(-rad2deg(atan((fwdFilt_ + 0.15 * speed_) / kGravity)), -25.0, 25.0);
    }
    s_.targetRoll = tRoll;
    s_.targetPitch = tPitch;
    s_.targetYaw = hdgTarget_;

    // The airframe follows its setpoint a beat late (tau 120 ms); the gap is the
    // controller error the attitude frame reports.
    double prevRoll = rollF_, prevPitch = pitchF_;
    double alpha = 1.0 - exp(-dt / 0.12);
    rollF_ += (tRoll - rollF_) * alpha;
    pitchF_ += (tPitch - pitchF_) * alpha;

    double noise = s_.armed ? 0.35 : 0.04;
    s_.roll = rollF_ + rng_.gauss() * noise;
    s_.pitch = pitchF_ + rng_.gauss() * noise;
    s_.yaw = wrapDeg(s_.heading + rng_.gauss() * noise);

    // Rates from the smooth attitude, plus gyro noise. Differentiating the noisy
    // angle would read as a gyro tens of times worse than a real one.
    double gyroNoise = s_.armed ? 0.8 : 0.05;
    s_.gyroX = (rollF_ - prevRoll) / dt + rng_.gauss() * gyroNoise;
    s_.gyroY = (pitchF_ - prevPitch) / dt + rng_.gauss() * gyroNoise;
    s_.gyroZ = turnRate + rng_.gauss() * gyroNoise;
  }

  void imu(double aVert) {
    // An accelerometer reads specific force: about -g on z at rest, more when the
    // thrust vector tilts or climbs. Spinning props add vibration on every axis.
    double vib = s_.armed ? 0.35 : 0.02;
    double tilt = cos(deg2rad(s_.roll)) * cos(deg2rad(s_.pitch));
    if (tilt < 0.5) tilt = 0.5;
    s_.accelX = rng_.gauss() * vib;
    s_.accelY = rng_.gauss() * vib;
    s_.accelZ = -(kGravity + aVert) / tilt + rng_.gauss() * vib;
  }

  void motors(double turnRate) {
    double pct[4] = {0, 0, 0, 0};
    if (s_.armed && !airborne()) {
      for (int i = 0; i < 4; i++) pct[i] = 12.0 + rng_.gauss() * 0.3;  // idle
    } else if (s_.armed) {
      // Quad X mixer. Roll right needs the left pair faster; nose up needs the
      // front pair faster; yaw is the diagonal pairs.
      double t = 50.0 + 5.0 * velU_ + 0.6 * speed_;
      double r = s_.roll * 0.35, p = s_.pitch * 0.35;
      double y = clampd(turnRate * 0.05, -3.0, 3.0);
      pct[0] = t - r + p - y;  // front right
      pct[1] = t + r - p - y;  // rear left
      pct[2] = t + r + p + y;  // front left
      pct[3] = t - r - p + y;  // rear right
      for (int i = 0; i < 4; i++) pct[i] += rng_.gauss() * 0.6;
    }
    for (int i = 0; i < 4; i++) {
      double c = clampd(pct[i], 0.0, 100.0);
      s_.motorPercent[i] = toU8(c);
      s_.motorPwm[i] = toU16(1000.0 + 10.0 * c);
    }
  }

  void refreshPower(double current, double dt) {
    s_.currentA = current;
    s_.soc -= current * dt / 3600.0 / cfg_.capacityAh;
    s_.soc = clampd(s_.soc, 0.0, 1.0);
    double ocv = lipoCellOcv(s_.soc);
    double pack = 0;
    uint16_t lo = 65535, hi = 0;
    for (int i = 0; i < 6; i++) {
      if (i < cfg_.cellCount) {
        s_.cellV[i] = ocv - current * kRint + cellOffset_[i] + rng_.gauss() * 0.0008;
        pack += s_.cellV[i];
        uint16_t mv = toU16(s_.cellV[i] * 1000.0);
        if (mv < lo) lo = mv;
        if (mv > hi) hi = mv;
      } else {
        s_.cellV[i] = 0;
      }
    }
    s_.packV = pack;
    s_.cellDeltaMv = static_cast<uint16_t>(hi - lo);
    s_.batteryRemaining = toU8(s_.soc * 100.0);
  }

  void sensors(double dt) {
    // Barometer: correlated noise, a few centimetres.
    baroNoise_ += -baroNoise_ * dt / 3.0 + 0.065 * sqrt(dt) * rng_.gauss();
    s_.altRel = posU_ + baroNoise_;
    s_.climbRate = velU_ + rng_.gauss() * 0.05;
    s_.groundSpeed = speed_;

    // GPS: no fix for the first seconds after power-on, then 2D, then 3D, with
    // the satellite count settling over the next half minute.
    if (s_.gpsFix != code::kFix3d) {
      if (s_.phaseMs < 6000 && s_.phase == Phase::Boot) {
        s_.gpsFix = code::kFixNone;
        s_.satellites = toU8(s_.phaseMs / 6000.0 * 5.0 - 0.5);
      } else if (s_.phaseMs < 9000 && s_.phase == Phase::Boot) {
        s_.gpsFix = code::kFix2d;
        s_.satellites = toU8(6.0 + (s_.phaseMs - 6000) / 3000.0);
      } else {
        s_.gpsFix = code::kFix3d;
        satF_ = 8.0;
      }
    }
    if (s_.gpsFix == code::kFix3d) {
      satF_ += (14.0 - satF_) * dt / 20.0 + 0.15 * sqrt(dt) * rng_.gauss();
      satF_ = clampd(satF_, 8.0, 18.0);
      s_.satellites = toU8(satF_);
    }
    gpsE_ += -gpsE_ * dt / 10.0 + 0.25 * sqrt(dt) * rng_.gauss();
    gpsN_ += -gpsN_ * dt / 10.0 + 0.25 * sqrt(dt) * rng_.gauss();

    // FlySky RSSI drops with range; the NRF link loses more packets with it.
    double dist = horizontalDistance();
    rssiNoise_ += -rssiNoise_ * dt / 1.5 + 1.7 * sqrt(dt) * rng_.gauss();
    s_.rssi = toI16(clampd(99.0 - dist / 10.0 + rssiNoise_, 0.0, 100.0));
    s_.linkQuality = toU8(clampd(100.0 - lossProbability() * 100.0 + rng_.gauss() * 0.8, 0.0, 100.0));
  }

  void companion(double dt) {
    // The Pi routing MAVLink works harder and runs hotter in flight.
    double loadMean = s_.armed ? 34.0 : 21.0;
    cpuNoise_ += -cpuNoise_ * dt / 4.0 + 2.1 * sqrt(dt) * rng_.gauss();
    s_.cpuLoad = clampd(loadMean + cpuNoise_, 3.0, 97.0);
    double tempTarget = airborne() ? 58.0 : (s_.armed ? 52.0 : 49.0);
    tempTrue_ += (tempTarget - tempTrue_) * dt / 90.0;
    s_.cpuTemp = tempTrue_ + rng_.gauss() * 0.15;
    ramNoise_ += -ramNoise_ * dt / 30.0 + 0.2 * sqrt(dt) * rng_.gauss();
    s_.ramPercent = toU8(37.0 + ramNoise_);
    s_.uptimeS = static_cast<uint32_t>((s_.simMs + kPiBootMs) / 1000);
  }

  // Arm state and companion link as a function of the phase. Landed stays armed
  // while the props spool down, then disarms.
  void updateArmed() {
    if (s_.phase == Phase::Arming || airborne()) {
      s_.armed = true;
    } else if (s_.phase == Phase::Landed) {
      s_.armed = s_.phaseMs < 3000;
    } else {
      s_.armed = false;
    }
    if (s_.phase == Phase::Boot && s_.phaseMs >= 2000) s_.mavlink = true;
  }

  // Modes, status, mission bookkeeping and the projected position: everything
  // that is a function of the phase and the physics rather than integrated.
  void refreshDerived() {
    s_.east = posE_;
    s_.north = posN_;
    s_.altMsl = cfg_.homeAltMsl + s_.altRel;

    if (s_.gpsFix >= code::kFix2d) {
      double n = posN_ + gpsN_, e = posE_ + gpsE_;
      s_.lat = cfg_.homeLat + rad2deg(n / kEarthRadius);
      s_.lon = cfg_.homeLon + rad2deg(e / (kEarthRadius * cos(deg2rad(cfg_.homeLat))));
    } else {
      s_.lat = 0.0;  // the receiver shows "NO FIX" for exactly 0.0
      s_.lon = 0.0;
    }

    double p = 0;
    switch (s_.phase) {
      case Phase::Boot:
        s_.flightMode = s_.mavlink ? code::kModeStabilize : code::kModeNoHeartbeat;
        s_.systemStatus = s_.phaseMs < 2000 ? code::kStatusBoot
                        : s_.phaseMs < 5000 ? code::kStatusCalibrating
                                            : code::kStatusStandby;
        s_.missionState = code::kMissionStandby;
        s_.missionSeq = 0;
        s_.distToTarget = 0;
        break;
      case Phase::Standby:
      case Phase::Arming:
        s_.flightMode = s_.phase == Phase::Arming ? code::kModeAuto : code::kModeStabilize;
        s_.systemStatus = s_.armed ? code::kStatusActive : code::kStatusStandby;
        s_.missionState = code::kMissionReady;
        s_.missionSeq = 0;
        s_.distToTarget = 0;
        break;
      case Phase::Takeoff:
        s_.flightMode = code::kModeAuto;
        s_.systemStatus = code::kStatusActive;
        s_.missionState = code::kMissionTakeoff;
        s_.missionSeq = 0;
        s_.distToTarget = fabs(kTakeoffAlt - posU_);
        p = clampd(posU_ / kTakeoffAlt, 0.0, 1.0) / kMissionItems;
        break;
      case Phase::Waypoints: {
        s_.flightMode = code::kModeAuto;
        s_.systemStatus = code::kStatusActive;
        s_.missionState = code::kMissionWaypoint;
        s_.missionSeq = static_cast<uint16_t>(wpIndex_ + 1);
        s_.distToTarget = hypot(hypot(wpE_[wpIndex_] - posE_, wpN_[wpIndex_] - posN_),
                                wpAlt_[wpIndex_] - posU_);
        double leg = clampd(1.0 - s_.distToTarget / legLen_, 0.0, 1.0);
        p = (1.0 + wpIndex_ + leg) / kMissionItems;
        break;
      }
      case Phase::ReturnHome:
        s_.flightMode = code::kModeRtl;
        s_.systemStatus = code::kStatusActive;
        s_.missionState = code::kMissionRtl;
        s_.missionSeq = kMissionItems - 1;
        s_.distToTarget = horizontalDistance();
        p = (6.0 + 0.5 * clampd(1.0 - s_.distToTarget / rtlLen_, 0.0, 1.0)) / kMissionItems;
        break;
      case Phase::Landing:
        s_.flightMode = code::kModeLand;
        s_.systemStatus = code::kStatusActive;
        s_.missionState = code::kMissionLanding;
        s_.missionSeq = kMissionItems - 1;
        s_.distToTarget = posU_;
        p = (6.5 + 0.5 * clampd(1.0 - posU_ / landStartAlt_, 0.0, 1.0)) / kMissionItems;
        break;
      case Phase::Landed:
        s_.flightMode = s_.armed ? code::kModeLand : code::kModeStabilize;
        s_.systemStatus = s_.armed ? code::kStatusActive : code::kStatusStandby;
        s_.missionState = code::kMissionComplete;
        s_.missionSeq = kMissionItems - 1;
        s_.distToTarget = 0;
        p = 1.0;
        break;
    }
    if (s_.armed && s_.batteryRemaining < 15) s_.systemStatus = code::kStatusCritical;

    // Progress only moves forward within a flight; a waypoint leg that bends
    // away for a moment must not make the bar jump backwards.
    if (p > progressMax_) progressMax_ = p;
    bool inFlight = airborne() || s_.phase == Phase::Landed;
    s_.missionProgress = inFlight ? toU8(progressMax_ * 100.0) : 0;
  }

  Config cfg_;
  Rng rng_;      // physics and sensor noise
  Rng rngLink_;  // packet loss only, so polling cadence cannot change the flight
  State s_;

  bool started_ = false;
  uint32_t lastMs_ = 0;
  uint32_t accMs_ = 0;

  bool slotsStarted_ = false;
  uint64_t nextSlotMs_ = 0;
  uint32_t slot_ = 0;
  uint8_t seq_ = 0;
  uint32_t sent_ = 0;
  uint32_t lost_ = 0;

  double wpE_[kWaypoints] = {}, wpN_[kWaypoints] = {}, wpAlt_[kWaypoints] = {};
  int wpIndex_ = 0;
  double legLen_ = 1.0, rtlLen_ = 1.0, rtlAlt_ = kTakeoffAlt, landStartAlt_ = 0.5;
  double progressMax_ = 0;

  // True kinematic state; State carries the measured view of it.
  double posE_ = 0, posN_ = 0, posU_ = 0;
  double velE_ = 0, velN_ = 0, velU_ = 0;
  double speed_ = 0, fwdFilt_ = 0, hdgTarget_ = 0;
  double rollF_ = 0, pitchF_ = 0;

  double cellOffset_[6] = {};
  double currentNoise_ = 0, baroNoise_ = 0, gpsE_ = 0, gpsN_ = 0, rssiNoise_ = 0;
  double cpuNoise_ = 0, ramNoise_ = 0, tempTrue_ = 0, satF_ = 8.0;
};

}  // namespace flightsim

#endif  // FLIGHT_SIM_H
