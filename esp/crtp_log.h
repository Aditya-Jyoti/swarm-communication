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

}  // namespace crtplog

#endif  // CRTP_LOG_H
