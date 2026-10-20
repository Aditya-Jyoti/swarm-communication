// Host unit tests for crtp_log.h.
//
//   esp/test/run_tests.sh      # build with coverage, run, report
//
// The drone side is a FakeDrone written from the ESP-Drone firmware source
// (components/core/crazyflie/modules/src/log.c and
// components/drivers/general/wifi/wifi_esp32.c at db0f656): the same reply
// layouts, the same checksum, the same 32-byte datagram limit. A pass here
// means the handheld speaks the protocol the drone implements, not one we
// imagined.

#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <limits>
#include <string>
#include <vector>

#include "../crtp_log.h"

using namespace crtplog;

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

typedef std::vector<uint8_t> Bytes;

// ---------------------------------------------------------------- codec

static void test_checksum_is_8bit_sum() {
  const uint8_t a[] = {0x01, 0x02, 0x03};
  CHECK(checksum(a, 3) == 0x06);
  const uint8_t b[] = {0xFF, 0x02};
  CHECK(checksum(b, 2) == 0x01);  // wraps, like the firmware's unsigned char
  CHECK(checksum(b, 0) == 0x00);
}

static void test_encode_layout() {
  const uint8_t data[] = {0x03};
  uint8_t out[MAX_DATAGRAM];
  size_t n = encode(header(PORT_LOG, CH_TOC), data, 1, out, sizeof out);
  CHECK(n == 3);
  CHECK(out[0] == 0x50);
  CHECK(out[1] == 0x03);
  CHECK(out[2] == 0x53);
}

static void test_encode_empty_payload() {
  uint8_t out[MAX_DATAGRAM];
  size_t n = encode(KEEPALIVE_HEADER, nullptr, 0, out, sizeof out);
  CHECK(n == 2);
  CHECK(out[0] == 0xF2);
  CHECK(out[1] == 0xF2);
}

static void test_encode_refuses_oversize_and_small_buffer() {
  uint8_t data[MAX_DATA + 1] = {0};
  uint8_t out[MAX_DATAGRAM + 4];
  CHECK(encode(0x50, data, MAX_DATA + 1, out, sizeof out) == 0);
  CHECK(encode(0x50, data, MAX_DATA, out, sizeof out) == MAX_DATAGRAM);
  CHECK(encode(0x50, data, 4, out, 5) == 0);  // needs 6
  CHECK(encode(0x50, data, 4, out, 6) == 6);
  CHECK(encode(0x50, data, 4, nullptr, 6) == 0);
}

static void test_decode_round_trip() {
  const uint8_t data[] = {9, 8, 7, 6, 5};
  uint8_t out[MAX_DATAGRAM];
  size_t n = encode(header(PORT_LOG, CH_DATA), data, 5, out, sizeof out);
  Packet p;
  CHECK(decode(out, n, &p));
  CHECK(p.port == PORT_LOG);
  CHECK(p.channel == CH_DATA);
  CHECK(p.size == 5);
  CHECK(std::memcmp(p.data, data, 5) == 0);
}

static void test_decode_masks_link_bits() {
  // Bits 2-3 of the header are link bits in newer Crazyflie stacks. The
  // channel is the low two bits only.
  uint8_t dg[] = {0x5E, 0x00};
  dg[1] = checksum(dg, 1);
  Packet p;
  CHECK(decode(dg, 2, &p));
  CHECK(p.port == 5);
  CHECK(p.channel == 2);
  CHECK(p.size == 0);
}

static void test_decode_rejects_bad_input() {
  Packet p;
  uint8_t dg[MAX_DATAGRAM + 1] = {0x50, 0x01, 0x51};
  CHECK(!decode(dg, 0, &p));
  CHECK(!decode(dg, 1, &p));  // a checksum with no header
  CHECK(!decode(nullptr, 3, &p));
  CHECK(!decode(dg, 3, nullptr));
  CHECK(decode(dg, 3, &p));
  dg[2] ^= 0x01;
  CHECK(!decode(dg, 3, &p));  // bad checksum
  for (size_t i = 0; i < MAX_DATAGRAM; ++i) dg[i] = 0;
  dg[MAX_DATAGRAM] = 0;
  CHECK(decode(dg, MAX_DATAGRAM, &p));       // 32 bytes is the limit
  CHECK(!decode(dg, MAX_DATAGRAM + 1, &p));  // the firmware drops 33
}

static void test_header_helper() {
  CHECK(header(PORT_LOG, CH_TOC) == 0x50);
  CHECK(header(PORT_LOG, CH_CONTROL) == 0x51);
  CHECK(header(PORT_LOG, CH_DATA) == 0x52);
  CHECK(header(0x1F, 0x07) == 0xF3);  // masked to 4 and 2 bits
}

// ---------------------------------------------------------------- fake drone

// Firmware log type codes (log.h) and errno values the drone returns.
enum { T_U8 = 1, T_U16 = 2, T_U32 = 3, T_I8 = 4, T_I16 = 5, T_I32 = 6, T_FLOAT = 7 };
enum { E_NOENT = 2, E_2BIG = 7, E_EXIST = 17 };

struct TocEntry {
  std::string group, name;
  uint8_t type;
  double value;
};

// The drone's log subsystem, reimplemented from log.c closely enough to drive
// the client: TOC_GET_INFO_V2 / TOC_GET_ITEM_V2, CONTROL_RESET /
// CREATE_BLOCK_V2 / START_BLOCK, and timed data packets with the firmware's
// type conversion (a float logged as an integer is truncated).
struct FakeDrone {
  struct Op { uint16_t id; uint8_t logType; };
  struct Block { int id; std::vector<Op> ops; uint32_t periodMs; bool started; uint32_t lastMs; };

  std::vector<TocEntry> toc;
  std::vector<Block> blocks;
  std::vector<uint8_t> sentHeaders;  // first byte of every datagram received
  std::vector<uint16_t> itemRequests;
  int infoRequests = 0, keepalives = 0, resets = 0, creates = 0, starts = 0;
  // Fault injection.
  int dropInfoReplies = 0;
  int dropItemReplies = 0;
  bool dropAllItemReplies = false;
  bool truncateNextItem = false;
  bool unterminatedNextItem = false;
  int forceCreateError = 0;
  bool silent = false;  // a drone that was switched off: no replies, no data
  int reportExtraItems = 0;  // count larger than the table: out-of-range reply
  uint32_t timestamp = 0x00ABCDEF;
  // The drone's clock, set by Link. A started block's timer runs from its start,
  // as xTimerStart does, so a 2 Hz block first reports 500 ms after starting.
  uint32_t clock = 0;

  FakeDrone() {
    toc = {
        {"acc", "x", T_FLOAT, 0.01},          {"acc", "y", T_FLOAT, 0.02},
        {"stabilizer", "roll", T_FLOAT, 1.5}, {"stabilizer", "pitch", T_FLOAT, -2.25},
        {"stabilizer", "yaw", T_FLOAT, 90.0}, {"stabilizer", "thrust", T_FLOAT, 0.0},
        {"gyro", "z", T_FLOAT, 0.0},          {"range", "front", T_U16, 0},
        {"range", "zrange", T_U16, 123},      {"pm", "vbat", T_FLOAT, 3.97},
        {"pm", "state", T_I8, 0},             {"pm", "batteryLevel", T_U8, 87},
        {"motor", "m1", T_U32, 0},            {"motor", "m2", T_U32, 0},
        {"motor", "m3", T_U32, 0},            {"motor", "m4", T_U32, 0},
        {"sys", "armed", T_I8, 1},
    };
  }

  int indexOf(const char* g, const char* n) const {
    for (size_t i = 0; i < toc.size(); ++i)
      if (toc[i].group == g && toc[i].name == n) return (int)i;
    return -1;
  }
  void set(const char* g, const char* n, double v) {
    int i = indexOf(g, n);
    if (i >= 0) toc[(size_t)i].value = v;
  }
  void remove(const char* g, const char* n) {
    int i = indexOf(g, n);
    if (i >= 0) toc.erase(toc.begin() + i);
  }

  static Bytes frame(uint8_t ch, const Bytes& data) {
    uint8_t out[MAX_DATAGRAM];
    size_t n = encode(header(PORT_LOG, ch), data.data(), data.size(), out, sizeof out);
    return Bytes(out, out + n);
  }

  static int typeLen(uint8_t t) {
    switch (t) {
      case T_U8: case T_I8: return 1;
      case T_U16: case T_I16: return 2;
      default: return 4;
    }
  }

  Block* find(int id) {
    for (size_t i = 0; i < blocks.size(); ++i)
      if (blocks[i].id == id) return &blocks[i];
    return nullptr;
  }

  // One received datagram in, zero or more replies out.
  std::vector<Bytes> handle(const uint8_t* dg, size_t n) {
    std::vector<Bytes> out;
    sentHeaders.push_back(dg[0]);
    Packet p;
    if (!decode(dg, n, &p)) return out;
    if (silent) return out;  // switched off: hears nothing, says nothing
    if (p.port == PORT_LINK) {
      if (p.channel == CH_LINK_SINK) ++keepalives;
      return out;
    }
    if (p.port != PORT_LOG) return out;
    if (p.channel == CH_TOC && p.size >= 1 && p.data[0] == 3) {
      ++infoRequests;
      if (dropInfoReplies > 0) { --dropInfoReplies; return out; }
      uint16_t count = (uint16_t)(toc.size() + (size_t)reportExtraItems);
      out.push_back(frame(CH_TOC, {3, (uint8_t)(count & 0xFF), (uint8_t)(count >> 8),
                                   0xDE, 0xAD, 0xBE, 0xEF, 16, 128}));
    } else if (p.channel == CH_TOC && p.size >= 3 && p.data[0] == 2) {
      uint16_t id = (uint16_t)(p.data[1] | (p.data[2] << 8));
      itemRequests.push_back(id);
      if (dropAllItemReplies) return out;
      if (dropItemReplies > 0) { --dropItemReplies; return out; }
      if (id >= toc.size()) {
        out.push_back(frame(CH_TOC, {2}));  // "Index out of range", size 1
        return out;
      }
      const TocEntry& e = toc[id];
      Bytes d = {2, (uint8_t)(id & 0xFF), (uint8_t)(id >> 8), e.type};
      d.insert(d.end(), e.group.begin(), e.group.end());
      d.push_back(0);
      d.insert(d.end(), e.name.begin(), e.name.end());
      d.push_back(0);
      if (truncateNextItem) { truncateNextItem = false; d.resize(5); }
      if (unterminatedNextItem) { unterminatedNextItem = false; d.pop_back(); }
      out.push_back(frame(CH_TOC, d));
    } else if (p.channel == CH_CONTROL && p.size >= 1) {
      uint8_t cmd = p.data[0];
      uint8_t id = p.size >= 2 ? p.data[1] : 0xEE;  // reset leaves data[1] stale
      uint8_t ret = 0;
      if (cmd == 5) {
        ++resets;
        blocks.clear();
      } else if (cmd == 6) {
        ++creates;
        if (forceCreateError) {
          ret = (uint8_t)forceCreateError;
        } else if (find(id)) {
          ret = E_EXIST;
        } else {
          Block b{id, {}, 1000, false, 0};
          int len = 0;
          for (size_t i = 2; i + 3 <= (size_t)p.size; i += 3) {
            Op op{(uint16_t)(p.data[i + 1] | (p.data[i + 2] << 8)), p.data[i]};
            if (op.id >= toc.size()) { ret = E_NOENT; break; }
            len += typeLen(op.logType);
            if (len > 26) { ret = E_2BIG; break; }
            b.ops.push_back(op);
          }
          if (ret == 0) blocks.push_back(b);
        }
      } else if (cmd == 3) {
        ++starts;
        Block* b = find(id);
        if (!b) {
          ret = E_NOENT;
        } else {
          b->periodMs = (uint32_t)p.data[2] * 10;
          b->started = true;
          b->lastMs = clock;
        }
      }
      out.push_back(frame(CH_CONTROL, {cmd, id, ret}));
    }
    return out;
  }

  Bytes dataPacket(const Block& b) {
    Bytes d = {(uint8_t)b.id, (uint8_t)(timestamp & 0xFF), (uint8_t)((timestamp >> 8) & 0xFF),
               (uint8_t)((timestamp >> 16) & 0xFF)};
    for (size_t i = 0; i < b.ops.size(); ++i) {
      double v = toc[b.ops[i].id].value;
      uint8_t t = b.ops[i].logType;
      if (t == T_FLOAT) {
        float f = (float)v;
        uint8_t raw[4];
        memcpy(raw, &f, 4);
        d.insert(d.end(), raw, raw + 4);
      } else {
        int64_t iv = std::isfinite(v) ? (int64_t)v : 0;
        uint32_t u = (uint32_t)iv;
        for (int k = 0; k < typeLen(t); ++k) d.push_back((uint8_t)(u >> (8 * k)));
      }
    }
    return frame(CH_DATA, d);
  }

  // Data packets due at `now`, as the firmware's per-block timers emit them.
  std::vector<Bytes> tick(uint32_t now) {
    std::vector<Bytes> out;
    if (silent) return out;
    for (size_t i = 0; i < blocks.size(); ++i) {
      Block& b = blocks[i];
      if (!b.started) continue;
      if (now - b.lastMs >= b.periodMs) {
        b.lastMs = now;
        out.push_back(dataPacket(b));
      }
    }
    return out;
  }
};

// Runs client and drone together for `ms` of millis(), in `stepMs` steps.
struct Link {
  Client client;
  FakeDrone drone;
  uint32_t now;
  std::vector<Bytes> sent;
  explicit Link(uint32_t start = 1000) : now(start) {}
  void deliver(const Bytes& b) { client.onDatagram(now, b.data(), b.size()); }
  void run(uint32_t ms, uint32_t stepMs = 5) {
    uint8_t buf[MAX_DATAGRAM];
    for (uint32_t t = 0; t < ms; t += stepMs) {
      now += stepMs;
      drone.clock = now;
      size_t n;
      while ((n = client.poll(now, buf, sizeof buf)) > 0) {
        sent.push_back(Bytes(buf, buf + n));
        std::vector<Bytes> replies = drone.handle(buf, n);
        for (size_t i = 0; i < replies.size(); ++i) deliver(replies[i]);
      }
      std::vector<Bytes> data = drone.tick(now);
      for (size_t i = 0; i < data.size(); ++i) deliver(data[i]);
    }
  }
  int countSent(uint8_t hdr, int cmd = -1) const {
    int c = 0;
    for (size_t i = 0; i < sent.size(); ++i)
      if (sent[i][0] == hdr && (cmd < 0 || (sent[i].size() > 2 && sent[i][1] == cmd))) ++c;
    return c;
  }
};

// ---------------------------------------------------------------- client

static void test_happy_path_streams_real_values() {
  Link l;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.missing() == 0);
  CHECK(l.client.hasSample());
  CHECK(l.client.samples() >= 5);
  CHECK(l.drone.resets == 1);
  CHECK(l.drone.creates == 2);
  CHECK(l.drone.starts == 2);
  const Telemetry& t = l.client.telemetry();
  CHECK_NEAR(t.roll, 1.5, 1e-6);
  CHECK_NEAR(t.pitch, -2.25, 1e-6);
  CHECK_NEAR(t.yaw, 90.0, 1e-6);
  CHECK_NEAR(t.thrust, 0.0, 1e-6);
  CHECK(t.altValid);
  CHECK_NEAR(t.altM, 0.123, 1e-6);
  CHECK_NEAR(t.vbat, 3.97, 1e-5);
  CHECK(t.pct == 87);
  CHECK(!t.armed);
  CHECK(t.droneMs == 0x00ABCDEF);
  for (int i = 0; i < 4; ++i) CHECK(t.motor[i] == 0);
  // Block 1 at 10 Hz: ~0.1 s apart. 2 Hz block 2 does not count as a sample.
  uint32_t before = l.client.samples();
  l.run(1000);
  CHECK(l.client.samples() - before >= 9 && l.client.samples() - before <= 11);
}

// Regression, seen on the real drone: block 1 (10 Hz) arrives before block 2
// (2 Hz), so the first published sample said 0% battery. No sample may be
// published until every block in use has reported once.
static void test_first_sample_waits_for_the_slow_block() {
  Link l;
  for (int i = 0; i < 400 && !l.client.hasSample(); ++i) l.run(5);
  CHECK(l.client.hasSample());
  CHECK(l.client.takeFreshSample());
  CHECK(l.client.telemetry().pct == 87);
  CHECK_NEAR(l.client.telemetry().roll, 1.5, 1e-6);
}

static void test_values_track_the_drone() {
  Link l;
  l.run(1500);
  l.drone.set("stabilizer", "roll", -12.5);
  l.drone.set("stabilizer", "thrust", 30000);
  l.drone.set("motor", "m3", 41000);
  l.drone.set("pm", "batteryLevel", 42);
  l.drone.set("range", "zrange", 1450);
  l.run(700);
  const Telemetry& t = l.client.telemetry();
  CHECK_NEAR(t.roll, -12.5, 1e-6);
  CHECK_NEAR(t.thrust, 30000, 1e-3);
  CHECK(t.motor[2] == 41000);
  CHECK(t.pct == 42);
  CHECK_NEAR(t.altM, 1.45, 1e-6);
  CHECK(t.armed);
}

static void test_armed_from_motors_alone() {
  Link l;
  l.run(1500);
  CHECK(!l.client.telemetry().armed);
  l.drone.set("motor", "m1", 500);
  l.run(700);
  CHECK(l.client.telemetry().armed);
  l.drone.set("motor", "m1", 0);
  l.run(700);
  CHECK(!l.client.telemetry().armed);
  l.drone.set("stabilizer", "thrust", 1);
  l.run(200);
  CHECK(l.client.telemetry().armed);
}

static void test_sys_armed_is_not_requested() {
  // sys.armed defaults to true in ESP-Drone, so asking for it would show a
  // grounded drone as armed.
  Link l;
  l.run(1500);
  int armedId = l.drone.indexOf("sys", "armed");
  for (size_t b = 0; b < l.drone.blocks.size(); ++b)
    for (size_t i = 0; i < l.drone.blocks[b].ops.size(); ++i)
      CHECK(l.drone.blocks[b].ops[i].id != armedId);
}

static void test_lost_info_reply_is_retried_after_300ms() {
  Link l;
  l.drone.dropInfoReplies = 2;
  l.run(295, 5);
  CHECK(l.drone.infoRequests == 1);
  l.run(10, 5);
  CHECK(l.drone.infoRequests == 2);
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
}

static void test_lost_item_reply_rerequests_same_index() {
  Link l;
  l.drone.dropItemReplies = 1;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.drone.itemRequests.size() >= 2);
  CHECK(l.drone.itemRequests[0] == 0);
  CHECK(l.drone.itemRequests[1] == 0);
}

static void test_truncated_and_unterminated_items_are_retried() {
  Link l;
  l.drone.truncateNextItem = true;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.missing() == 0);
  Link u;
  u.drone.unterminatedNextItem = true;
  u.run(1500);
  CHECK(u.client.state() == ST_STREAMING);
  CHECK(u.client.missing() == 0);
}

static void test_missing_variable_is_left_out() {
  Link l;
  l.drone.remove("pm", "vbat");
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.missing() == 1);
  CHECK(!l.client.has(V_VBAT));
  CHECK(l.client.has(V_ROLL));
  const Telemetry& t = l.client.telemetry();
  CHECK_NEAR(t.roll, 1.5, 1e-6);
  CHECK_NEAR(t.altM, 0.123, 1e-6);
  CHECK_NEAR(t.vbat, 0.0, 1e-9);
  CHECK(t.pct == 87);
}

static void test_no_block2_variables_still_streams() {
  Link l;
  const char* names[] = {"m1", "m2", "m3", "m4"};
  for (int i = 0; i < 4; ++i) l.drone.remove("motor", names[i]);
  l.drone.remove("pm", "batteryLevel");
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.missing() == 5);
  CHECK(l.drone.blocks.size() == 1);
  CHECK(l.client.hasSample());
}

static void test_no_block1_variables_is_unsupported_then_retried() {
  Link l;
  const char* st[] = {"roll", "pitch", "yaw", "thrust"};
  for (int i = 0; i < 4; ++i) l.drone.remove("stabilizer", st[i]);
  l.drone.remove("range", "zrange");
  l.drone.remove("pm", "vbat");
  l.run(1500);
  CHECK(l.client.state() == ST_UNSUPPORTED);
  CHECK(!l.client.hasSample());
  int info = l.drone.infoRequests;
  l.run(3000);
  CHECK(l.drone.infoRequests == info);  // waits, does not hammer the drone
  l.run(3000);
  CHECK(l.drone.infoRequests > info);
}

static void test_empty_toc_is_unsupported() {
  Link l;
  l.drone.toc.clear();
  l.run(500);
  CHECK(l.client.state() == ST_UNSUPPORTED);
}

static void test_out_of_range_item_ends_the_toc_early() {
  Link l;
  l.drone.reportExtraItems = 5;
  l.drone.remove("motor", "m4");  // so the scan cannot stop on "all found"
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.missing() == 1);
}

static void test_scan_stops_once_everything_is_found() {
  Link l;
  l.run(1500);
  // motor.m4 and pm.batteryLevel are the last wanted entries; sys.armed after
  // them is never requested.
  int armedId = l.drone.indexOf("sys", "armed");
  for (size_t i = 0; i < l.drone.itemRequests.size(); ++i)
    CHECK(l.drone.itemRequests[i] != armedId);
}

static void test_duplicate_name_keeps_the_first() {
  Link l;
  l.drone.toc.push_back({"stabilizer", "roll", T_FLOAT, 99.0});
  l.drone.remove("motor", "m4");  // force a full scan past the duplicate
  l.run(1500);
  CHECK_NEAR(l.client.telemetry().roll, 1.5, 1e-6);
}

static void test_create_error_restarts() {
  Link l;
  l.drone.forceCreateError = E_EXIST;
  l.run(800);
  CHECK(l.client.restarts() >= 1);
  CHECK(l.client.state() != ST_STREAMING);
  l.drone.forceCreateError = 0;
  l.run(3000);
  CHECK(l.client.state() == ST_STREAMING);
}

// Regression: a drone rejecting every block made the client restart, re-read
// the table and fail again inside one poll() loop, for ever -- the replies come
// back faster than millis() moves. Each restart must wait a retry interval.
static void test_persistent_create_error_does_not_spin() {
  Link l;
  l.drone.forceCreateError = E_2BIG;
  l.run(3000);
  CHECK(l.drone.creates >= 5);
  CHECK(l.drone.creates <= 11);  // at most one attempt per 300 ms
  CHECK(l.sent.size() < 1000);
}

static void test_item_retries_exhausted_restarts() {
  Link l;
  l.drone.dropAllItemReplies = true;
  l.run(5000);
  CHECK(l.client.restarts() >= 1);
  CHECK(l.client.state() == ST_READ_TOC || l.client.state() == ST_GET_INFO);
  l.drone.dropAllItemReplies = false;
  l.run(3000);
  CHECK(l.client.state() == ST_STREAMING);
}

static void test_silent_drone_restarts_after_2s_and_recovers() {
  Link l;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  uint32_t lastSample = l.client.lastSampleMs();
  l.drone.silent = true;
  l.run(1850);
  CHECK(l.client.state() == ST_STREAMING);
  l.run(250);
  CHECK(l.client.state() != ST_STREAMING);
  CHECK(l.client.restarts() == 1);
  CHECK(l.client.lastSampleMs() == lastSample);
  l.drone.silent = false;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.lastSampleMs() > lastSample);
}

static void test_keepalive_every_250ms_while_streaming() {
  Link l;
  l.run(1500);
  int before = l.drone.keepalives;
  l.run(2000, 5);
  int k = l.drone.keepalives - before;
  CHECK(k >= 7 && k <= 9);
  // Never more than 250 ms of silence from the client, including the gap
  // between the last setup command and the first keepalive.
}

static void test_never_sends_anything_but_log_and_sink() {
  Link l;
  l.drone.dropInfoReplies = 1;
  l.drone.dropItemReplies = 3;
  l.run(1500);
  l.drone.silent = true;
  l.run(3000);
  l.drone.silent = false;
  l.run(3000);
  CHECK(!l.sent.empty());
  for (size_t i = 0; i < l.sent.size(); ++i) {
    uint8_t h = l.sent[i][0];
    CHECK(h == 0x50 || h == 0x51 || h == 0xF2);
    CHECK(h != 'n');
    // 7 bytes starting "now" is a flight setpoint to this firmware.
    CHECK(!(l.sent[i].size() == 7 && l.sent[i][0] == 'n'));
    CHECK(l.sent[i].size() <= MAX_DATAGRAM);
  }
}

static void test_millis_wrap() {
  Link l(0xFFFFF000u);  // 4 s before the 49.7-day wrap
  l.run(10000);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK(l.client.samples() >= 80);
  CHECK(l.client.restarts() == 0);
}

static void test_non_finite_float_keeps_previous_value() {
  Link l;
  l.run(1500);
  l.drone.set("stabilizer", "pitch", std::numeric_limits<double>::quiet_NaN());
  l.drone.set("pm", "vbat", std::numeric_limits<double>::infinity());
  l.run(500);
  CHECK_NEAR(l.client.telemetry().pitch, -2.25, 1e-6);
  CHECK_NEAR(l.client.telemetry().vbat, 3.97, 1e-5);
  CHECK(l.client.state() == ST_STREAMING);
}

static void test_out_of_range_altitude() {
  Link l;
  l.drone.set("range", "zrange", 8190);  // VL53L0X "nothing in range"
  l.run(1500);
  CHECK(!l.client.telemetry().altValid);
  l.drone.set("range", "zrange", 8000);
  l.run(300);
  CHECK(l.client.telemetry().altValid);
  CHECK_NEAR(l.client.telemetry().altM, 8.0, 1e-6);
}

static void test_battery_percent_is_clamped() {
  Link l;
  l.drone.set("pm", "batteryLevel", 150);
  l.run(1500);
  CHECK(l.client.telemetry().pct == 100);
}

static void test_garbage_and_strays_are_ignored() {
  Link l;
  l.run(1500);
  uint32_t samples = l.client.samples();
  const Telemetry before = l.client.telemetry();
  // Bad checksum.
  uint8_t bad[] = {0x52, 1, 0, 0, 0, 0x00};
  l.client.onDatagram(l.now, bad, sizeof bad);
  CHECK(l.client.badDatagrams() == 1);
  // Short data packet: header plus 3 bytes.
  l.deliver(FakeDrone::frame(CH_DATA, {1, 0, 0}));
  // Block 1 too short for its layout.
  l.deliver(FakeDrone::frame(CH_DATA, {1, 0, 0, 0, 1, 2, 3}));
  // Unknown block.
  l.deliver(FakeDrone::frame(CH_DATA, {9, 0, 0, 0, 1, 2, 3, 4}));
  // Another port entirely (console).
  uint8_t out[MAX_DATAGRAM];
  const uint8_t txt[] = {'h', 'i'};
  size_t n = encode(header(0, 0), txt, 2, out, sizeof out);
  l.client.onDatagram(l.now, out, n);
  // A stray TOC info reply while streaming.
  l.deliver(FakeDrone::frame(CH_TOC, {3, 1, 0, 0, 0, 0, 0, 16, 128}));
  // A stray control reply.
  l.deliver(FakeDrone::frame(CH_CONTROL, {6, 1, 0}));
  CHECK(l.client.samples() == samples);
  CHECK(l.client.state() == ST_STREAMING);
  CHECK_NEAR(l.client.telemetry().roll, before.roll, 1e-9);
}

static void test_short_info_and_mismatched_replies_are_ignored() {
  Client c;
  uint8_t buf[MAX_DATAGRAM];
  CHECK(c.poll(0, buf, sizeof buf) > 0);  // GET_INFO out
  // Info reply too short.
  Bytes s = FakeDrone::frame(CH_TOC, {3, 5});
  c.onDatagram(1, s.data(), s.size());
  CHECK(c.state() == ST_GET_INFO);
  // Item reply in the wrong state.
  Bytes it = FakeDrone::frame(CH_TOC, {2, 0, 0, 7, 'a', 0, 'b', 0});
  c.onDatagram(2, it.data(), it.size());
  CHECK(c.state() == ST_GET_INFO);
  // Data before setup.
  Bytes d = FakeDrone::frame(CH_DATA, {1, 0, 0, 0, 1, 2, 3, 4});
  c.onDatagram(3, d.data(), d.size());
  CHECK(!c.hasSample());
  // Now a real info reply moves it on; a reply for the wrong index is dropped.
  Bytes info = FakeDrone::frame(CH_TOC, {3, 2, 0, 0, 0, 0, 0, 16, 128});
  c.onDatagram(4, info.data(), info.size());
  CHECK(c.state() == ST_READ_TOC);
  CHECK(c.poll(5, buf, sizeof buf) > 0);  // item 0 requested
  Bytes wrong = FakeDrone::frame(CH_TOC, {2, 1, 0, 7, 'a', 0, 'b', 0});
  c.onDatagram(6, wrong.data(), wrong.size());
  CHECK(c.poll(7, buf, sizeof buf) == 0);  // still waiting for item 0
}

static void test_control_reply_for_other_command_is_ignored() {
  Link l;
  // Run until setup has sent its reset, then inject mismatched replies.
  l.drone.forceCreateError = 0;
  l.run(1500);
  CHECK(l.client.state() == ST_STREAMING);
  Client c;
  uint8_t buf[MAX_DATAGRAM];
  // Drive a bare client into setup with a two-entry TOC.
  c.poll(0, buf, sizeof buf);
  Bytes info = FakeDrone::frame(CH_TOC, {3, 1, 0, 0, 0, 0, 0, 16, 128});
  c.onDatagram(1, info.data(), info.size());
  c.poll(2, buf, sizeof buf);
  Bytes roll = FakeDrone::frame(CH_TOC, {2, 0, 0, T_FLOAT, 's', 't', 'a', 'b', 'i', 'l', 'i',
                                         'z', 'e', 'r', 0, 'r', 'o', 'l', 'l', 0});
  c.onDatagram(3, roll.data(), roll.size());
  CHECK(c.state() == ST_SETUP);
  CHECK(c.poll(4, buf, sizeof buf) > 0);  // RESET out
  CHECK(buf[0] == 0x51 && buf[1] == 5);
  Bytes other = FakeDrone::frame(CH_CONTROL, {6, 1, 0});
  c.onDatagram(5, other.data(), other.size());
  CHECK(c.poll(6, buf, sizeof buf) == 0);  // still waiting on the reset
  Bytes shortReply = FakeDrone::frame(CH_CONTROL, {5, 0});
  c.onDatagram(7, shortReply.data(), shortReply.size());
  CHECK(c.poll(8, buf, sizeof buf) == 0);
  Bytes ok = FakeDrone::frame(CH_CONTROL, {5, 0xEE, 0});
  c.onDatagram(9, ok.data(), ok.size());
  CHECK(c.poll(10, buf, sizeof buf) > 0);  // CREATE block 1 out
  CHECK(buf[1] == 6 && buf[2] == 1);
  Bytes wrongBlock = FakeDrone::frame(CH_CONTROL, {6, 2, 0});
  c.onDatagram(11, wrongBlock.data(), wrongBlock.size());
  CHECK(c.poll(12, buf, sizeof buf) == 0);
}

static void test_small_buffer_sends_nothing() {
  Client c;
  uint8_t buf[2];
  CHECK(c.poll(0, buf, sizeof buf) == 0);  // GET_INFO needs 3
  CHECK(c.poll(0, nullptr, 32) == 0);
  uint8_t big[MAX_DATAGRAM];
  CHECK(c.poll(1, big, sizeof big) == 3);  // not marked as waiting earlier
}

static void test_take_fresh_sample_once_per_sample() {
  Link l;
  CHECK(!l.client.takeFreshSample());
  l.run(1500);
  CHECK(l.client.takeFreshSample());
  CHECK(!l.client.takeFreshSample());
  l.run(150);
  CHECK(l.client.takeFreshSample());
}

static void test_restart_counts_and_state_names() {
  CHECK(std::strcmp(stateName(ST_GET_INFO), "get-info") == 0);
  CHECK(std::strcmp(stateName(ST_READ_TOC), "read-toc") == 0);
  CHECK(std::strcmp(stateName(ST_SETUP), "setup") == 0);
  CHECK(std::strcmp(stateName(ST_STREAMING), "streaming") == 0);
  CHECK(std::strcmp(stateName(ST_UNSUPPORTED), "unsupported") == 0);
  CHECK(std::strcmp(stateName((State)99), "?") == 0);
  Client c;
  CHECK(c.restarts() == 0);
  CHECK(c.state() == ST_GET_INFO);
  CHECK(c.tocCount() == 0);
}

static void test_toc_progress_is_reported() {
  Link l;
  l.run(1500);
  CHECK(l.client.tocCount() == l.drone.toc.size());
}

// Names each test before it runs, so a hang or a crash points at its test.
#define RUN(fn)                                   \
  do {                                            \
    if (std::getenv("CRTP_TRACE")) std::printf("%s\n", #fn); \
    std::fflush(stdout);                          \
    fn();                                         \
  } while (0)

int main() {
  RUN(test_checksum_is_8bit_sum);
  RUN(test_encode_layout);
  RUN(test_encode_empty_payload);
  RUN(test_encode_refuses_oversize_and_small_buffer);
  RUN(test_decode_round_trip);
  RUN(test_decode_masks_link_bits);
  RUN(test_decode_rejects_bad_input);
  RUN(test_header_helper);
  RUN(test_happy_path_streams_real_values);
  RUN(test_first_sample_waits_for_the_slow_block);
  RUN(test_values_track_the_drone);
  RUN(test_armed_from_motors_alone);
  RUN(test_sys_armed_is_not_requested);
  RUN(test_lost_info_reply_is_retried_after_300ms);
  RUN(test_lost_item_reply_rerequests_same_index);
  RUN(test_truncated_and_unterminated_items_are_retried);
  RUN(test_missing_variable_is_left_out);
  RUN(test_no_block2_variables_still_streams);
  RUN(test_no_block1_variables_is_unsupported_then_retried);
  RUN(test_empty_toc_is_unsupported);
  RUN(test_out_of_range_item_ends_the_toc_early);
  RUN(test_scan_stops_once_everything_is_found);
  RUN(test_duplicate_name_keeps_the_first);
  RUN(test_create_error_restarts);
  RUN(test_persistent_create_error_does_not_spin);
  RUN(test_item_retries_exhausted_restarts);
  RUN(test_silent_drone_restarts_after_2s_and_recovers);
  RUN(test_keepalive_every_250ms_while_streaming);
  RUN(test_never_sends_anything_but_log_and_sink);
  RUN(test_millis_wrap);
  RUN(test_non_finite_float_keeps_previous_value);
  RUN(test_out_of_range_altitude);
  RUN(test_battery_percent_is_clamped);
  RUN(test_garbage_and_strays_are_ignored);
  RUN(test_short_info_and_mismatched_replies_are_ignored);
  RUN(test_control_reply_for_other_command_is_ignored);
  RUN(test_small_buffer_sends_nothing);
  RUN(test_take_fresh_sample_once_per_sample);
  RUN(test_restart_counts_and_state_names);
  RUN(test_toc_progress_is_reported);
  std::printf("crtp_log: %d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
