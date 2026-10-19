// telemetry_frames.h -- the NRF24 telemetry wire format, shared by the receiver
// (esp.ino) and the flight simulator (flight_sim.h).
//
// One definition on purpose. The receiver decodes a frame by casting the raw
// 32-byte radio payload to one of these structs, so a simulator that kept its
// own copy of the layout could drift by one byte and the display would show
// plausible garbage. Everything here is plain C++ with no Arduino headers, so
// the host unit tests compile exactly the same layout the board does.

#ifndef TELEMETRY_FRAMES_H
#define TELEMETRY_FRAMES_H

#include <stddef.h>
#include <stdint.h>

#define PACKET_MAGIC 0xAA
#define FRAME_BASIC 0
#define FRAME_POWER_SBC 1
#define FRAME_ATTITUDE 2
#define FRAME_MOTORS 3

// Every multi-frame payload is exactly this long on the wire, with an XOR
// checksum of bytes 0..30 in byte 31. See validateFrameChecksum.
#define FRAME_SIZE 32

// Legacy single packet (20 bytes), still accepted by the receiver.
struct __attribute__((packed)) TelemetryPacket {
  uint8_t  magic;       // Byte 0:     0xAA Sync Header
  uint8_t  seq;         // Byte 1:     Packet sequence counter (0-255)
  uint16_t bat_mv;      // Bytes 2-3:  Battery Voltage in millivolts (e.g. 12580 = 12.58V)
  int16_t  rssi;        // Bytes 4-5:  FlySky Receiver RSSI (e.g. 85 % or -65 dBm)
  int32_t  alt_cm;      // Bytes 6-9:  Altitude in centimeters (e.g. 4520 = 45.20m)
  int32_t  lat_e7;      // Bytes 10-13: Latitude * 10^7 (e.g. 123456780 = 12.3456780 deg)
  int32_t  lon_e7;      // Bytes 14-17: Longitude * 10^7 (e.g. 771234560 = 77.1234560 deg)
  uint8_t  satellites;  // Byte 18:    GPS Satellite count (e.g. 14)
  uint8_t  checksum;    // Byte 19:    8-bit XOR Checksum
};

struct __attribute__((packed)) BasicFrame {
  uint8_t magic;
  uint8_t seq;
  uint8_t frameType;
  uint8_t flags;
  uint16_t bat_mv;
  int16_t rssi;
  int32_t alt_cm;
  int32_t lat_e7;
  int32_t lon_e7;
  uint8_t satellites;
  uint16_t heading_cd;
  uint8_t battery_remaining;
  uint8_t gps_fix;
  uint8_t flight_mode;
  uint8_t system_status;
  uint8_t link_quality;
  uint8_t mission_progress;
  uint8_t cell_count;
  uint8_t pad;
  uint8_t checksum;
};

// NOTE: this struct is 30 bytes, not FRAME_SIZE. Its `checksum` member sits at
// byte 29, but the receiver validates byte 31 of the 32-byte payload, so the
// member is never read. The fields up to byte 28 decode correctly either way.
// It is left as found rather than padded: changing it would change how bytes
// from a real transmitter are interpreted, and that transmitter's packer is not
// in this repo.
struct __attribute__((packed)) PowerSbcFrame {
  uint8_t magic;
  uint8_t seq;
  uint8_t frameType;
  uint8_t flags;
  int16_t battery_current_ca;
  uint16_t cell_mv[6];
  uint16_t cell_delta_mv;
  uint8_t cpu_load_percent;
  int16_t cpu_temp_dc;
  uint8_t ram_percent;
  uint8_t disk_percent;
  uint16_t uptime_minutes;
  uint16_t reserved;
  uint8_t checksum;
};

struct __attribute__((packed)) AttitudeFrame {
  uint8_t magic;
  uint8_t seq;
  uint8_t frameType;
  uint8_t flags;
  int16_t roll_cd;
  int16_t pitch_cd;
  uint16_t yaw_cd;
  int16_t target_roll_cd;
  int16_t target_pitch_cd;
  uint16_t target_yaw_cd;
  int16_t error_roll_cd;
  int16_t error_pitch_cd;
  int16_t gyro_x_cdps;
  int16_t gyro_y_cdps;
  int16_t gyro_z_cdps;
  int16_t climb_cms;
  int16_t altitude_msl_dm;
  uint8_t pad;
  uint8_t checksum;
};

struct __attribute__((packed)) MotorsFrame {
  uint8_t magic;
  uint8_t seq;
  uint8_t frameType;
  uint8_t flags;
  uint8_t motor_percent[4];
  uint16_t motor_pwm[4];
  uint16_t mission_current_seq;
  uint16_t mission_total_items;
  uint16_t dist_to_target_wp_dm;
  uint16_t ground_speed_cms;
  int16_t accel_x_centi;
  int16_t accel_y_centi;
  int16_t accel_z_centi;
  uint8_t mission_state;
  uint8_t checksum;
};

// The layouts the receiver casts to. A wrong size here means a field moved, so
// it fails the build instead of the display.
static_assert(sizeof(TelemetryPacket) == 20, "TelemetryPacket must be 20 bytes");
static_assert(sizeof(BasicFrame) == FRAME_SIZE, "BasicFrame must be 32 bytes");
static_assert(sizeof(PowerSbcFrame) == 30, "PowerSbcFrame is 30 bytes; see note");
static_assert(sizeof(AttitudeFrame) == FRAME_SIZE, "AttitudeFrame must be 32 bytes");
static_assert(sizeof(MotorsFrame) == FRAME_SIZE, "MotorsFrame must be 32 bytes");

// Flag bits in BasicFrame.flags (and the other frames' flags byte).
#define FRAME_FLAG_ARMED 0x01
#define FRAME_FLAG_MAVLINK 0x02

inline uint8_t calculateChecksum(const uint8_t* buffer, size_t length) {
  uint8_t c = 0;
  for (size_t i = 0; i < length; i++) {
    c ^= buffer[i];
  }
  return c;
}

inline bool validateFrameChecksum(const uint8_t* buffer, size_t size) {
  if (size != FRAME_SIZE) return false;
  return buffer[FRAME_SIZE - 1] == calculateChecksum(buffer, FRAME_SIZE - 1);
}

#endif  // TELEMETRY_FRAMES_H
