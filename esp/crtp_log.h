// crtp_log.h -- the ground unit's side of the ESP-Drone (LiteWing) log link.
//
// The drone serves the Crazyflie Real-Time Protocol over UDP on its own access
// point: one CRTP packet per datagram, followed by one checksum byte. This file
// is the datagram codec and a client that reads the drone's log table of
// contents, asks for the variables the handheld displays, and decodes the
// samples the drone then streams.
//
// READ ONLY. The client sends only port 5 (log) and port 15 channel 2 (link
// sink, ignored by the drone) packets. It can never produce a setpoint -- and
// it must not, because the firmware also reads any 7-byte datagram starting
// with "now" as a stick command (detectEspNow in wifilink.c). Neither header
// this file emits (0x5X, 0xF2) is 'n' (0x6E), and a test holds that.
//
// Facts taken from the firmware source (espressif/esp-drone, db0f656):
//   - checksum: 8-bit sum of every preceding byte  (wifi_esp32.c)
//   - datagram limit: 32 bytes, header + 30 data + checksum  (wifi_esp32.h)
//   - a block carries at most 26 value bytes  (log.c, LOG_MAX_LEN)
//   - with no packet for 1000 ms the drone resets every log block (log.c ->
//     crtpIsConnected -> wifilinkIsConnected), hence the keepalive
//
// Plain C++11, no Arduino headers, driven only by the millis() values passed
// in, so esp/test/ checks it on a PC.

#ifndef CRTP_LOG_H
#define CRTP_LOG_H

#include <math.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

namespace crtplog {

// ------------------------------------------------------------------ codec

const uint8_t PORT_LOG = 0x05;
const uint8_t PORT_LINK = 0x0F;
const uint8_t CH_TOC = 0;
const uint8_t CH_CONTROL = 1;
const uint8_t CH_DATA = 2;
const uint8_t CH_LINK_SINK = 2;

const size_t MAX_DATA = 30;      // CRTP_MAX_DATA_SIZE
const size_t MAX_DATAGRAM = 32;  // WIFI_RX_TX_PACKET_SIZE: header + data + checksum

inline uint8_t header(uint8_t port, uint8_t channel) {
  return (uint8_t)(((port & 0x0F) << 4) | (channel & 0x03));
}

// Port 15 channel 2: the drone's link service ignores it. It exists only to
// keep the drone's one-second link watchdog fed.
const uint8_t KEEPALIVE_HEADER = 0xF2;

inline uint8_t checksum(const uint8_t* data, size_t n) {
  uint8_t sum = 0;
  for (size_t i = 0; i < n; ++i) sum = (uint8_t)(sum + data[i]);
  return sum;
}

struct Packet {
  uint8_t port;
  uint8_t channel;
  uint8_t size;  // data bytes, not counting header or checksum
  uint8_t data[MAX_DATA];
};

// Writes header, data and checksum into out. Returns the datagram length, or 0
// if the payload is too big for the link or does not fit in cap.
inline size_t encode(uint8_t hdr, const uint8_t* data, size_t n, uint8_t* out, size_t cap) {
  if (out == nullptr || n > MAX_DATA || cap < n + 2) return 0;
  out[0] = hdr;
  if (n > 0) memcpy(out + 1, data, n);
  out[n + 1] = checksum(out, n + 1);
  return n + 2;
}

// Validates and splits one received datagram. False for anything the firmware
// itself would not have sent: empty, header-less, oversize or a bad checksum.
inline bool decode(const uint8_t* dg, size_t n, Packet* p) {
  if (dg == nullptr || p == nullptr || n < 2 || n > MAX_DATAGRAM) return false;
  if (checksum(dg, n - 1) != dg[n - 1]) return false;
  p->port = (uint8_t)(dg[0] >> 4);
  p->channel = (uint8_t)(dg[0] & 0x03);
  p->size = (uint8_t)(n - 2);
  if (p->size > 0) memcpy(p->data, dg + 1, p->size);
  return true;
}

// ------------------------------------------------------------------ log client

// Firmware log type codes (log.h). Only the ones this client requests.
const uint8_t LOG_UINT8 = 1;
const uint8_t LOG_UINT16 = 2;
const uint8_t LOG_FLOAT = 7;

// Log port commands (log.c).
const uint8_t CMD_GET_ITEM_V2 = 2;
const uint8_t CMD_GET_INFO_V2 = 3;
const uint8_t CONTROL_START_BLOCK = 3;
const uint8_t CONTROL_RESET = 5;
const uint8_t CONTROL_CREATE_BLOCK_V2 = 6;

// Timing. RETRY_MS covers a lost request or reply on a busy 2.4 GHz channel;
// KEEPALIVE_MS is a quarter of the drone's 1000 ms watchdog, so three lost
// keepalives in a row still do not cost the log blocks.
const uint32_t RETRY_MS = 300;
const uint32_t MAX_RETRIES = 5;
const uint32_t KEEPALIVE_MS = 250;
const uint32_t STREAM_TIMEOUT_MS = 2000;
const uint32_t UNSUPPORTED_RETRY_MS = 5000;

// Block periods, in the firmware's unit of 10 ms.
const uint8_t BLOCK_FAST = 1;  // attitude, thrust, altitude, battery: 10 Hz
const uint8_t BLOCK_SLOW = 2;  // motors, battery percent: 2 Hz
const uint8_t FAST_PERIOD_10MS = 10;
const uint8_t SLOW_PERIOD_10MS = 50;

// VL53L0X reports 8190 mm (or more) when nothing is in range.
const uint16_t ZRANGE_MAX_VALID_MM = 8000;

enum VarIndex {
  V_ROLL,
  V_PITCH,
  V_YAW,
  V_THRUST,
  V_ZRANGE,
  V_VBAT,
  V_M1,
  V_M2,
  V_M3,
  V_M4,
  V_PCT,
  V_COUNT
};

struct VarSpec {
  const char* group;
  const char* name;
  uint8_t type;   // the type this client ASKS for; the drone converts
  uint8_t block;
};

// sys.armed is deliberately absent: ESP-Drone initialises it to true unless
// built with START_DISARMED, so it reads "armed" on a drone sitting on the
// bench. Armed is derived from thrust and motor output instead.
inline const VarSpec& varSpec(int i) {
  static const VarSpec specs[V_COUNT] = {
      {"stabilizer", "roll", LOG_FLOAT, BLOCK_FAST},
      {"stabilizer", "pitch", LOG_FLOAT, BLOCK_FAST},
      {"stabilizer", "yaw", LOG_FLOAT, BLOCK_FAST},
      {"stabilizer", "thrust", LOG_FLOAT, BLOCK_FAST},
      {"range", "zrange", LOG_UINT16, BLOCK_FAST},
      {"pm", "vbat", LOG_FLOAT, BLOCK_FAST},
      // Motor power is a uint32 in the firmware but never exceeds 65535.
      {"motor", "m1", LOG_UINT16, BLOCK_SLOW},
      {"motor", "m2", LOG_UINT16, BLOCK_SLOW},
      {"motor", "m3", LOG_UINT16, BLOCK_SLOW},
      {"motor", "m4", LOG_UINT16, BLOCK_SLOW},
      {"pm", "batteryLevel", LOG_UINT8, BLOCK_SLOW},
  };
  return specs[i];
}

inline uint8_t typeSize(uint8_t type) {
  return type == LOG_FLOAT ? 4 : (type == LOG_UINT16 ? 2 : 1);
}

struct Telemetry {
  float roll, pitch, yaw;  // degrees, as the drone's stabilizer reports them
  float thrust;            // 0..65535, the drone's thrust command
  float altM;              // range sensor, metres
  bool altValid;           // false when the sensor sees nothing in range
  float vbat;              // volts
  uint8_t pct;             // battery percent, 0..100
  bool armed;              // thrust or any motor above zero
  uint16_t motor[4];
  uint32_t droneMs;        // the sample's drone timestamp, 24 bits
};

enum State { ST_GET_INFO, ST_READ_TOC, ST_SETUP, ST_STREAMING, ST_UNSUPPORTED };

inline const char* stateName(State s) {
  switch (s) {
    case ST_GET_INFO: return "get-info";
    case ST_READ_TOC: return "read-toc";
    case ST_SETUP: return "setup";
    case ST_STREAMING: return "streaming";
    case ST_UNSUPPORTED: return "unsupported";
  }
  return "?";
}

// The log-protocol client. Single-threaded and allocation-free: the sketch
// calls onDatagram() for every received datagram and poll() until it returns
// 0, every loop.
//
//   GET_INFO   ask how many variables the drone logs
//   READ_TOC   read them one at a time, keeping the ids of the wanted ones
//   SETUP      reset the drone's blocks, create ours, start them
//   STREAMING  decode samples, send keepalives; silence for 2 s restarts
//
// Every request is retried after RETRY_MS. MAX_RETRIES failures in one step
// restart from GET_INFO, which is also where a rebooted drone lands us.
class Client {
 public:
  Client() { reset(0); restarts_ = 0; }

  // The next datagram to send, written into out, or 0 if there is nothing to
  // send right now. Call until it returns 0.
  size_t poll(uint32_t now, uint8_t* out, size_t cap) {
    if (out == nullptr) return 0;
    if (state_ == ST_UNSUPPORTED) {
      if (now - enteredMs_ < UNSUPPORTED_RETRY_MS) return 0;
      restart(now);
      return 0;
    }
    if (state_ == ST_STREAMING) {
      if (now - lastDataMs_ >= STREAM_TIMEOUT_MS) {
        restart(now);
        return 0;
      }
      if (now - lastTxMs_ < KEEPALIVE_MS) return 0;
      size_t n = encode(KEEPALIVE_HEADER, nullptr, 0, out, cap);
      if (n > 0) lastTxMs_ = now;
      return n;
    }
    if (awaiting_) {
      if (now - lastTxMs_ < RETRY_MS) return 0;
      if (++retries_ > MAX_RETRIES && state_ != ST_GET_INFO) {
        restart(now);
        return 0;
      }
    }
    uint8_t hdr = 0;
    uint8_t req[MAX_DATA];
    size_t len = buildRequest(&hdr, req);
    size_t n = encode(hdr, req, len, out, cap);
    if (n == 0) return 0;
    awaiting_ = true;
    lastTxMs_ = now;
    return n;
  }

  void onDatagram(uint32_t now, const uint8_t* dg, size_t n) {
    Packet p;
    if (!decode(dg, n, &p)) {
      ++bad_;
      return;
    }
    if (p.port != PORT_LOG) return;  // console, link echo: not ours
    if (p.channel == CH_TOC) {
      onToc(now, p);
    } else if (p.channel == CH_CONTROL) {
      onControl(now, p);
    } else if (p.channel == CH_DATA) {
      onData(now, p);
    }
  }

  State state() const { return state_; }
  bool hasSample() const { return hasSample_; }
  uint32_t lastSampleMs() const { return lastSampleMs_; }
  uint32_t samples() const { return samples_; }
  uint32_t restarts() const { return restarts_; }
  uint32_t badDatagrams() const { return bad_; }
  uint16_t tocCount() const { return tocCount_; }
  const Telemetry& telemetry() const { return t_; }
  bool has(VarIndex v) const { return ids_[v] >= 0; }

  int missing() const {
    int m = 0;
    for (int i = 0; i < V_COUNT; ++i)
      if (ids_[i] < 0) ++m;
    return m;
  }

  // True once for each new fast-block sample: the sketch prints one line per.
  bool takeFreshSample() {
    bool f = fresh_;
    fresh_ = false;
    return f;
  }

 private:
  enum Step { STEP_RESET, STEP_CREATE_FAST, STEP_CREATE_SLOW, STEP_START_FAST, STEP_START_SLOW, STEP_DONE };

  void reset(uint32_t now) {
    state_ = ST_GET_INFO;
    awaiting_ = false;
    retries_ = 0;
    lastTxMs_ = now;
    enteredMs_ = now;
    tocCount_ = 0;
    nextItem_ = 0;
    step_ = STEP_RESET;
    for (int i = 0; i < V_COUNT; ++i) ids_[i] = -1;
  }

  // A restart keeps the last telemetry: the sketch shows it as stale, by age,
  // rather than blanking a screen that was correct two seconds ago.
  //
  // It also waits one RETRY_MS before asking again. Without that, a drone that
  // rejects every block (another client holding them, a changed table) answers
  // fast enough to spin the client through the whole table and back in one
  // loop() call, for ever.
  void restart(uint32_t now) {
    ++restarts_;
    reset(now);
    awaiting_ = true;
  }

  void enter(State s, uint32_t now) {
    state_ = s;
    awaiting_ = false;
    retries_ = 0;
    enteredMs_ = now;
  }

  bool blockUsed(uint8_t block) const {
    for (int i = 0; i < V_COUNT; ++i)
      if (ids_[i] >= 0 && varSpec(i).block == block) return true;
    return false;
  }

  // Skips setup steps for a block with nothing in it.
  void advanceStep(uint32_t now) {
    for (;;) {
      step_ = (Step)(step_ + 1);
      if ((step_ == STEP_CREATE_SLOW || step_ == STEP_START_SLOW) && !blockUsed(BLOCK_SLOW)) continue;
      break;
    }
    awaiting_ = false;
    retries_ = 0;
    if (step_ == STEP_DONE) {
      enter(ST_STREAMING, now);
      lastDataMs_ = now;
    }
  }

  size_t buildRequest(uint8_t* hdr, uint8_t* req) const {
    switch (state_) {
      case ST_GET_INFO:
        *hdr = header(PORT_LOG, CH_TOC);
        req[0] = CMD_GET_INFO_V2;
        return 1;
      case ST_READ_TOC:
        *hdr = header(PORT_LOG, CH_TOC);
        req[0] = CMD_GET_ITEM_V2;
        req[1] = (uint8_t)(nextItem_ & 0xFF);
        req[2] = (uint8_t)(nextItem_ >> 8);
        return 3;
      default:  // ST_SETUP: the other states never reach here
        *hdr = header(PORT_LOG, CH_CONTROL);
        return buildSetup(req);
    }
  }

  size_t buildSetup(uint8_t* req) const {
    switch (step_) {
      case STEP_RESET:
        req[0] = CONTROL_RESET;
        return 1;
      case STEP_CREATE_FAST:
      case STEP_CREATE_SLOW: {
        uint8_t block = step_ == STEP_CREATE_FAST ? BLOCK_FAST : BLOCK_SLOW;
        req[0] = CONTROL_CREATE_BLOCK_V2;
        req[1] = block;
        size_t n = 2;
        for (int i = 0; i < V_COUNT; ++i) {
          if (ids_[i] < 0 || varSpec(i).block != block) continue;
          req[n++] = varSpec(i).type;
          req[n++] = (uint8_t)(ids_[i] & 0xFF);
          req[n++] = (uint8_t)((ids_[i] >> 8) & 0xFF);
        }
        return n;
      }
      default: {  // STEP_START_FAST, STEP_START_SLOW
        bool fast = step_ == STEP_START_FAST;
        req[0] = CONTROL_START_BLOCK;
        req[1] = fast ? BLOCK_FAST : BLOCK_SLOW;
        req[2] = fast ? FAST_PERIOD_10MS : SLOW_PERIOD_10MS;
        return 3;
      }
    }
  }

  void onToc(uint32_t now, const Packet& p) {
    if (p.size < 1) return;
    if (state_ == ST_GET_INFO && p.data[0] == CMD_GET_INFO_V2 && p.size >= 3) {
      tocCount_ = (uint16_t)(p.data[1] | (p.data[2] << 8));
      nextItem_ = 0;
      enter(ST_READ_TOC, now);
      if (tocCount_ == 0) finishToc(now);
      return;
    }
    if (state_ != ST_READ_TOC || p.data[0] != CMD_GET_ITEM_V2) return;
    if (p.size == 1) {  // "index out of range": the table is shorter than claimed
      finishToc(now);
      return;
    }
    if (p.size < 6) return;  // truncated; the retry asks again
    uint16_t id = (uint16_t)(p.data[1] | (p.data[2] << 8));
    if (id != nextItem_) return;  // a late reply to an earlier request
    const char* group = (const char*)&p.data[4];
    size_t room = p.size - 4;
    size_t glen = strnlen(group, room);
    if (glen + 1 >= room) return;  // no terminator, or no room for a name
    const char* name = group + glen + 1;
    size_t nroom = room - glen - 1;
    if (strnlen(name, nroom) == nroom) return;  // name not terminated
    for (int i = 0; i < V_COUNT; ++i) {
      if (ids_[i] < 0 && strcmp(varSpec(i).group, group) == 0 && strcmp(varSpec(i).name, name) == 0) {
        ids_[i] = id;
      }
    }
    ++nextItem_;
    awaiting_ = false;
    retries_ = 0;
    if (nextItem_ >= tocCount_ || missing() == 0) finishToc(now);
  }

  void finishToc(uint32_t now) {
    if (!blockUsed(BLOCK_FAST)) {
      // Nothing the screen needs is logged by this firmware. Back off rather
      // than re-reading the whole table in a tight loop.
      enter(ST_UNSUPPORTED, now);
      return;
    }
    enter(ST_SETUP, now);
    step_ = STEP_RESET;
  }

  void onControl(uint32_t now, const Packet& p) {
    if (state_ != ST_SETUP || p.size < 3) return;
    uint8_t cmd = p.data[0];
    switch (step_) {
      case STEP_RESET:
        // The drone does not set data[1] on a reset reply, so only the command
        // byte identifies it.
        if (cmd != CONTROL_RESET) return;
        break;
      case STEP_CREATE_FAST:
      case STEP_CREATE_SLOW:
        if (cmd != CONTROL_CREATE_BLOCK_V2 ||
            p.data[1] != (step_ == STEP_CREATE_FAST ? BLOCK_FAST : BLOCK_SLOW)) return;
        break;
      default:  // STEP_START_FAST, STEP_START_SLOW
        if (cmd != CONTROL_START_BLOCK ||
            p.data[1] != (step_ == STEP_START_FAST ? BLOCK_FAST : BLOCK_SLOW)) return;
        break;
    }
    if (p.data[2] != 0) {
      // ENOENT, E2BIG, EEXIST, ENOMEM: the drone's table changed under us or
      // another client holds blocks. Start over from the table.
      restart(now);
      return;
    }
    advanceStep(now);
  }

  static uint32_t le32(const uint8_t* b) {
    return (uint32_t)b[0] | ((uint32_t)b[1] << 8) | ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
  }

  // Keeps the previous value for a non-finite float: one bad sample must not
  // put NaN on the screen or into the relay's JSON.
  static void setFinite(float* dst, const uint8_t* b) {
    uint32_t u = le32(b);
    float f;
    memcpy(&f, &u, 4);
    if (isfinite(f)) *dst = f;
  }

  void store(int var, const uint8_t* b) {
    switch (var) {
      case V_ROLL: setFinite(&t_.roll, b); break;
      case V_PITCH: setFinite(&t_.pitch, b); break;
      case V_YAW: setFinite(&t_.yaw, b); break;
      case V_THRUST: setFinite(&t_.thrust, b); break;
      case V_VBAT: setFinite(&t_.vbat, b); break;
      case V_ZRANGE: {
        uint16_t mm = (uint16_t)(b[0] | (b[1] << 8));
        t_.altValid = mm <= ZRANGE_MAX_VALID_MM;
        t_.altM = t_.altValid ? mm / 1000.0f : 0.0f;
        break;
      }
      case V_PCT: t_.pct = b[0] > 100 ? 100 : b[0]; break;
      default:  // V_M1..V_M4
        t_.motor[var - V_M1] = (uint16_t)(b[0] | (b[1] << 8));
        break;
    }
  }

  void onData(uint32_t now, const Packet& p) {
    if ((state_ != ST_SETUP && state_ != ST_STREAMING) || p.size < 4) return;
    uint8_t block = p.data[0];
    if (block != BLOCK_FAST && block != BLOCK_SLOW) return;
    size_t need = 4;
    for (int i = 0; i < V_COUNT; ++i)
      if (ids_[i] >= 0 && varSpec(i).block == block) need += typeSize(varSpec(i).type);
    if (need == 4 || p.size < need) return;  // not a block we built, or short
    size_t off = 4;
    for (int i = 0; i < V_COUNT; ++i) {
      if (ids_[i] < 0 || varSpec(i).block != block) continue;
      store(i, &p.data[off]);
      off += typeSize(varSpec(i).type);
    }
    t_.droneMs = (uint32_t)p.data[1] | ((uint32_t)p.data[2] << 8) | ((uint32_t)p.data[3] << 16);
    t_.armed = t_.thrust > 0.0f || t_.motor[0] || t_.motor[1] || t_.motor[2] || t_.motor[3];
    if (block == BLOCK_SLOW) slowSeen_ = true;
    if (block == BLOCK_FAST) lastDataMs_ = now;  // the link is alive either way
    // Nothing is published until every block in use has reported once: the
    // 10 Hz block lands first, and publishing then showed 0% battery for the
    // half second before the 2 Hz block arrived.
    if (block == BLOCK_FAST && (slowSeen_ || !blockUsed(BLOCK_SLOW))) {
      lastSampleMs_ = now;
      hasSample_ = true;
      fresh_ = true;
      ++samples_;
    }
  }

  State state_;
  Step step_;
  bool awaiting_;
  uint32_t retries_;
  uint32_t lastTxMs_ = 0;
  uint32_t enteredMs_ = 0;
  uint32_t lastDataMs_ = 0;
  uint16_t tocCount_;
  uint16_t nextItem_;
  int32_t ids_[V_COUNT];

  Telemetry t_ = Telemetry();
  bool hasSample_ = false;
  bool fresh_ = false;
  bool slowSeen_ = false;
  uint32_t lastSampleMs_ = 0;
  uint32_t samples_ = 0;
  uint32_t restarts_;
  uint32_t bad_ = 0;
};

}  // namespace crtplog

#endif  // CRTP_LOG_H
