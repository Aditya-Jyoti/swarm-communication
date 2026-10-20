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

int main() {
  test_checksum_is_8bit_sum();
  test_encode_layout();
  test_encode_empty_payload();
  test_encode_refuses_oversize_and_small_buffer();
  test_decode_round_trip();
  test_decode_masks_link_bits();
  test_decode_rejects_bad_input();
  test_header_helper();
  std::printf("crtp_log: %d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
