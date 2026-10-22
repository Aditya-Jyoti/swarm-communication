/*
 * ======================================================================================
 * Project: ESP32 Drone Telemetry Receiver (Ground Unit)
 * File: esp/esp.ino
 *
 * Hardware Modules:
 *   - ESP32 Development Board (ESP-WROOM-32)
 *   - 1.3-inch I2C OLED Display (JMD1.3A / SH1106 Driver, 128x64)
 *   - NRF24L01+ PA + LNA Transceiver Module (with external antenna)
 *   - HW-200 3.3V Base Adapter Board (powered via ESP32 VIN/5V)
 *
 * Hardware Pinout Connections:
 *   [NRF24L01+ via HW-200 Adapter -> ESP32 Hardware VSPI]
 *     - VCC  -> ESP32 VIN (5V) [Regulated to stable 3.3V by HW-200 AMS1117]
 *     - GND  -> ESP32 GND
 *     - CE   -> GPIO 4
 *     - CSN  -> GPIO 5
 *     - SCK  -> GPIO 18 (VSPI SCK)
 *     - MOSI -> GPIO 23 (VSPI MOSI)
 *     - MISO -> GPIO 19 (VSPI MISO)
 *
 *   [1.3" SH1106 OLED Display -> ESP32 Hardware I2C]
 *     - VCC  -> ESP32 3.3V (or 5V)
 *     - GND  -> ESP32 GND
 *     - SCL  -> GPIO 22 (I2C Clock)
 *     - SDA  -> GPIO 21 (I2C Data)
 *
 * Serial Monitor Baud Rate: 115200
 * ======================================================================================
 */

// ======================================================================================
// FRAME SOURCE
// ======================================================================================
// Where the telemetry on the screen comes from:
//
// 2 = DRONE_SOURCE_LITEWING (default): a LiteWing / ESP-Drone quadcopter. The
//     board joins the drone's own Wi-Fi access point and reads its log stream
//     (crtp_log.h). READ ONLY: nothing sent can command the drone. Each sample
//     is also printed as a "TLM {json}" line for backend/cmd/litewing-relay.
// 1 = DRONE_SOURCE_SIM: no drone needed. flight_sim.h flies a repeating survey
//     mission and produces the same 32-byte frames a drone transmits, through
//     the normal receive path.
// 0 = DRONE_SOURCE_NRF: frames come from the NRF24L01+.
//
// NEVER USE SIM WITH A REAL AIRCRAFT. In sim mode the display shows a healthy,
// flying drone whatever the real link is doing -- an operator watching it would
// not see a lost link or a dying battery. The JSON reports "source":"esp32-sim"
// and the serial log prints [SIM] lines, so anything reading either can tell.
//
// Override from the command line with -DDRONE_SOURCE=0, 1 or 2. The older
// -DDRONE_SIM_MODE=0 / =1 still works and means NRF / SIM.
#define DRONE_SOURCE_NRF 0
#define DRONE_SOURCE_SIM 1
#define DRONE_SOURCE_LITEWING 2

#ifndef DRONE_SOURCE
#ifdef DRONE_SIM_MODE
#if DRONE_SIM_MODE
#define DRONE_SOURCE DRONE_SOURCE_SIM
#else
#define DRONE_SOURCE DRONE_SOURCE_NRF
#endif
#else
#define DRONE_SOURCE DRONE_SOURCE_LITEWING
#endif
#endif

#if DRONE_SOURCE != DRONE_SOURCE_NRF && DRONE_SOURCE != DRONE_SOURCE_SIM && \
    DRONE_SOURCE != DRONE_SOURCE_LITEWING
#error "DRONE_SOURCE must be 0 (NRF), 1 (SIM) or 2 (LITEWING)"
#endif

#include <Arduino.h>
#include <SPI.h>
#include <Wire.h>
#include <WiFi.h>
#include <WebServer.h>
#include <ESPmDNS.h>
#include <RF24.h>
#include <U8g2lib.h>

// Wire format shared with the simulator: PACKET_MAGIC, FRAME_*, the frame
// structs, calculateChecksum() and validateFrameChecksum().
#include "telemetry_frames.h"
// Link-screen stages, progress, spinner and sweep frames, and the OLED's
// presence on the bus (PanelWatch). Pure C++, tested on the host.
#include "boot_anim.h"

#if DRONE_SOURCE == DRONE_SOURCE_SIM
#include "flight_sim.h"
#endif

#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
#include <WiFiUdp.h>
#include "crtp_log.h"
#endif

// ======================================================================================
// PIN DEFINITIONS
// ======================================================================================
#define PIN_NRF_CE    4
#define PIN_NRF_CSN   5
#define PIN_NRF_SCK   18
#define PIN_NRF_MISO  19
#define PIN_NRF_MOSI  23

#define PIN_OLED_SDA  21
#define PIN_OLED_SCL  22

// ======================================================================================
// WI-FI CONFIGURATION
// ======================================================================================
#ifndef DRONE_WIFI_STA_SSID
#define DRONE_WIFI_STA_SSID ""
#endif

#ifndef DRONE_WIFI_STA_PASSWORD
#define DRONE_WIFI_STA_PASSWORD ""
#endif

#ifndef DRONE_WIFI_HOSTNAME
#define DRONE_WIFI_HOSTNAME "drone-esp32"
#endif

#ifndef DRONE_WIFI_FALLBACK_AP_SSID
#define DRONE_WIFI_FALLBACK_AP_SSID "DroneTelemetryESP32"
#endif

#ifndef DRONE_WIFI_FALLBACK_AP_PASSWORD
#define DRONE_WIFI_FALLBACK_AP_PASSWORD ""
#endif

const char* WIFI_STA_SSID = DRONE_WIFI_STA_SSID;
const char* WIFI_STA_PASSWORD = DRONE_WIFI_STA_PASSWORD;
const char* WIFI_HOSTNAME = DRONE_WIFI_HOSTNAME;
const char* WIFI_FALLBACK_AP_SSID = DRONE_WIFI_FALLBACK_AP_SSID;
const char* WIFI_FALLBACK_AP_PASSWORD = DRONE_WIFI_FALLBACK_AP_PASSWORD;
const uint16_t HTTP_PORT = 80;

// ======================================================================================
// DISPLAY & RADIO INSTANTIATION
// ======================================================================================
// SH1106 128x64 Full Buffer Hardware I2C (Address 0x3C)
U8G2_SH1106_128X64_NONAME_F_HW_I2C u8g2(U8G2_R0, /* reset=*/ U8X8_PIN_NONE);
const uint8_t OLED_I2C_ADDR = 0x3C;
// The panel is owned by its own task (displayTask): probed once a second at
// each bus clock, drawn at its own pace. A slow or absent panel can then never
// hold up the drone link, and the link can never freeze the screen.
// Everything below is touched by that task only, except the log hand-off.
bootanim::PanelWatch g_panel;
bootanim::BusLadder g_bus;
TaskHandle_t g_displayTask = nullptr;
// The task's [OLED] lines are printed by loop(): two tasks writing Serial can
// split a TLM line in half, and the relay would reject it.
portMUX_TYPE g_oledLogMux = portMUX_INITIALIZER_UNLOCKED;
char g_oledLog[64] = "";
volatile uint32_t g_oledLogSeq = 0;

// NRF24L01 Radio on VSPI (CE=4, CSN=5)
RF24 radio(PIN_NRF_CE, PIN_NRF_CSN);
WebServer server(HTTP_PORT);

// Radio Configuration
const byte rfAddresses[][6] = {"1Node", "2Node"};
const uint8_t RF_CHANNEL = 90; // 2.490 GHz

#if DRONE_SOURCE == DRONE_SOURCE_SIM
flightsim::FlightSim g_sim;
#endif

#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
// The drone's access point. ESP-Drone names it after its MAC, so another
// LiteWing needs -DDRONE_LITEWING_SSID="LiteWing_xxxxxxxxxxxx".
#ifndef DRONE_LITEWING_SSID
#define DRONE_LITEWING_SSID "LiteWing_80456B54AA55"
#endif
#ifndef DRONE_LITEWING_PASSWORD
#define DRONE_LITEWING_PASSWORD "12345678"
#endif

const uint16_t LITEWING_PORT = 2390;        // the drone's CRTP UDP port
// Fixed, not ephemeral: the drone replies to the source of the LAST datagram it
// received, so a source port that changed would split the conversation.
const uint16_t LITEWING_LOCAL_PORT = 2399;
const unsigned long LITEWING_WIFI_RETRY_MS = 5000;

WiFiUDP g_lwUdp;
crtplog::Client g_lw;
bool g_lwUdpOpen = false;
unsigned long g_lwWifiAttemptMs = 0;
crtplog::State g_lwLastState = crtplog::ST_GET_INFO;
#endif

// ======================================================================================
// STATE VARIABLES
// ======================================================================================
float g_batVoltage = 0.0;    // Volts
float g_batteryCurrent = 0.0;
uint8_t g_batteryRemaining = 0;
float g_cellVoltages[6] = {0, 0, 0, 0, 0, 0};
uint16_t g_cellDeltaMv = 0;
uint8_t g_cellCount = 0;
int16_t g_rssiVal = 0;       // %
uint8_t g_linkQuality = 0;
float g_altitude = 0.0;      // Meters
bool g_altitudeValid = true; // false when a range sensor sees nothing
float g_thrustPercent = 0.0;
float g_altitudeMsl = 0.0;
float g_climbRate = 0.0;
double g_latitude = 0.0;     // Degrees
double g_longitude = 0.0;    // Degrees
uint8_t g_satellites = 0;    // Count
uint8_t g_gpsFix = 0;
float g_heading = 0.0;
float g_groundSpeed = 0.0;
uint8_t g_lastSeq = 0;       // Sequence
uint8_t g_flightMode = 0;
uint8_t g_systemStatus = 4;
uint8_t g_missionState = 0;
uint8_t g_missionProgress = 0;
uint16_t g_missionCurrentSeq = 0;
uint16_t g_missionTotalItems = 0;
bool g_armed = false;
bool g_piMavlinkConnected = false;
float g_roll = 0.0;
float g_pitch = 0.0;
float g_yaw = 0.0;
float g_targetRoll = 0.0;
float g_targetPitch = 0.0;
float g_targetYaw = 0.0;
float g_errorRoll = 0.0;
float g_errorPitch = 0.0;
float g_gyroX = 0.0;
float g_gyroY = 0.0;
float g_gyroZ = 0.0;
float g_accelX = 0.0;
float g_accelY = 0.0;
float g_accelZ = 0.0;
uint8_t g_motorPercent[4] = {0, 0, 0, 0};
uint16_t g_motorPwm[4] = {1000, 1000, 1000, 1000};
uint8_t g_cpuLoad = 0;
float g_cpuTemp = 0.0;
uint8_t g_ramPercent = 0;
uint8_t g_diskPercent = 0;
uint32_t g_sbcUptime = 0;

bool g_hasReceivedData = false;
bool g_heartbeatState = false;
bool g_radioHardwareOk = false;
bool g_wifiApMode = false;
// A fixed buffer, not a String: the display task reads it while loop() may
// rewrite it, and a String's heap buffer can be freed under the reader. The
// last byte is never written, so a torn read is still a terminated string.
char g_wifiIpText[20] = "0.0.0.0";
unsigned long g_lastPacketTime = 0;
// 10 s suits the NRF link's bursty losses. The LiteWing streams at 10 Hz over
// Wi-Fi, so 2 s of nothing is already a lost drone, and saying so sooner is
// the point of a ground unit.
const unsigned long DISCONNECT_TIMEOUT_MS =
    DRONE_SOURCE == DRONE_SOURCE_LITEWING ? 2000 : 10000;

// Display Refresh Timer (display task only)
unsigned long g_lastDisplayRefresh = 0;
const unsigned long DISPLAY_REFRESH_INTERVAL_MS = 60; // ~16 FPS at 400 kHz; slower clocks draw slower

void setIpText(const char* text) {
  snprintf(g_wifiIpText, sizeof g_wifiIpText - 1, "%s", text);
}

// ======================================================================================
// LABEL HELPERS
// ======================================================================================
const char* gpsFixLabel(uint8_t value) {
  switch (value) {
    case 2: return "2D FIX";
    case 3: return "3D FIX";
    case 4: return "DGPS";
    case 5: return "RTK FLT";
    case 6: return "RTK FIX";
    default: return "NO FIX";
  }
}

const char* flightModeLabel(uint8_t value) {
  switch (value) {
    case 1: return "STABILIZE";
    case 2: return "ALT_HOLD";
    case 3: return "AUTO";
    case 4: return "GUIDED";
    case 5: return "LOITER";
    case 6: return "RTL";
    case 7: return "LAND";
    case 8: return "POSHOLD";
    case 9: return "BRAKE";
    case 10: return "NO HEARTBEAT";
    case 255: return "CUSTOM";
    default: return "DISCONNECTED";
  }
}

const char* systemStatusLabel(uint8_t value) {
  switch (value) {
    case 0: return "UNINIT";
    case 1: return "BOOT";
    case 2: return "CALIBRATING";
    case 3: return "STANDBY";
    case 5: return "CRITICAL";
    case 6: return "EMERGENCY";
    default: return "ACTIVE";
  }
}

const char* missionStateLabel(uint8_t value) {
  switch (value) {
    case 1: return "READY";
    case 2: return "TAKEOFF";
    case 3: return "WAYPOINT NAV";
    case 4: return "RTL";
    case 5: return "LANDING";
    case 6: return "PAUSED";
    case 7: return "MISSION COMPLETE";
    default: return "STANDBY";
  }
}

// ======================================================================================
// PACKET PROCESSORS
// ======================================================================================
bool processTelemetryFrame(const uint8_t* buffer, size_t size, bool* shouldPrint) {
  if (!validateFrameChecksum(buffer, size)) return false;

  uint8_t frameType = buffer[2];
  *shouldPrint = false;

  if (frameType == FRAME_BASIC) {
    const BasicFrame* frame = (const BasicFrame*)buffer;
    g_batVoltage = frame->bat_mv / 1000.0f;
    g_rssiVal = frame->rssi;
    g_altitude = frame->alt_cm / 100.0f;
    g_latitude = frame->lat_e7 / 10000000.0;
    g_longitude = frame->lon_e7 / 10000000.0;
    g_satellites = frame->satellites;
    g_heading = frame->heading_cd / 100.0f;
    g_batteryRemaining = frame->battery_remaining;
    g_gpsFix = frame->gps_fix;
    g_flightMode = frame->flight_mode;
    g_systemStatus = frame->system_status;
    g_linkQuality = frame->link_quality;
    g_missionProgress = frame->mission_progress;
    g_cellCount = frame->cell_count;
    g_lastSeq = frame->seq;
    g_armed = (frame->flags & 0x01) != 0;
    g_piMavlinkConnected = (frame->flags & 0x02) != 0;
    *shouldPrint = true;
    return true;
  }

  if (frameType == FRAME_POWER_SBC) {
    const PowerSbcFrame* frame = (const PowerSbcFrame*)buffer;
    g_batteryCurrent = frame->battery_current_ca / 100.0f;
    for (uint8_t i = 0; i < 6; i++) {
      g_cellVoltages[i] = frame->cell_mv[i] / 1000.0f;
    }
    g_cellDeltaMv = frame->cell_delta_mv;
    g_cpuLoad = frame->cpu_load_percent;
    g_cpuTemp = frame->cpu_temp_dc / 10.0f;
    g_ramPercent = frame->ram_percent;
    g_diskPercent = frame->disk_percent;
    g_sbcUptime = (uint32_t)frame->uptime_minutes * 60UL;
    return true;
  }

  if (frameType == FRAME_ATTITUDE) {
    const AttitudeFrame* frame = (const AttitudeFrame*)buffer;
    g_roll = frame->roll_cd / 100.0f;
    g_pitch = frame->pitch_cd / 100.0f;
    g_yaw = frame->yaw_cd / 100.0f;
    g_targetRoll = frame->target_roll_cd / 100.0f;
    g_targetPitch = frame->target_pitch_cd / 100.0f;
    g_targetYaw = frame->target_yaw_cd / 100.0f;
    g_errorRoll = frame->error_roll_cd / 100.0f;
    g_errorPitch = frame->error_pitch_cd / 100.0f;
    g_gyroX = frame->gyro_x_cdps / 100.0f;
    g_gyroY = frame->gyro_y_cdps / 100.0f;
    g_gyroZ = frame->gyro_z_cdps / 100.0f;
    g_climbRate = frame->climb_cms / 100.0f;
    g_altitudeMsl = frame->altitude_msl_dm / 10.0f;
    return true;
  }

  if (frameType == FRAME_MOTORS) {
    const MotorsFrame* frame = (const MotorsFrame*)buffer;
    for (uint8_t i = 0; i < 4; i++) {
      g_motorPercent[i] = frame->motor_percent[i];
      g_motorPwm[i] = frame->motor_pwm[i];
    }
    g_missionCurrentSeq = frame->mission_current_seq;
    g_missionTotalItems = frame->mission_total_items;
    g_groundSpeed = frame->ground_speed_cms / 100.0f;
    g_accelX = frame->accel_x_centi / 100.0f;
    g_accelY = frame->accel_y_centi / 100.0f;
    g_accelZ = frame->accel_z_centi / 100.0f;
    g_missionState = frame->mission_state;
    return true;
  }

  return false;
}

bool processBinaryPacket(const uint8_t* buffer, size_t size) {
  if (size < sizeof(TelemetryPacket)) return false;

  const TelemetryPacket* pkt = (const TelemetryPacket*)buffer;
  if (pkt->magic != PACKET_MAGIC) return false;

  // Validate XOR Checksum over bytes 0 to 18
  uint8_t expectedChecksum = calculateChecksum(buffer, sizeof(TelemetryPacket) - 1);
  if (pkt->checksum != expectedChecksum) {
    Serial.println(F("[NRF24] Corrupted packet dropped (Checksum mismatch)."));
    return false;
  }

  // Unpack fixed-point integers into engineering units
  g_batVoltage = pkt->bat_mv / 1000.0f;
  g_rssiVal    = pkt->rssi;
  g_altitude   = pkt->alt_cm / 100.0f;
  g_latitude   = pkt->lat_e7 / 10000000.0;
  g_longitude  = pkt->lon_e7 / 10000000.0;
  g_satellites = pkt->satellites;
  g_lastSeq    = pkt->seq;
  g_piMavlinkConnected = true;

  return true;
}

// Fallback CSV Parser
bool processCsvPacket(const char* text) {
  char temp[33];
  strncpy(temp, text, sizeof(temp));
  temp[sizeof(temp) - 1] = '\0';

  char* token = strtok(temp, ",");
  if (!token) return false;
  g_batVoltage = atof(token);

  token = strtok(NULL, ",");
  if (token) g_rssiVal = atoi(token);

  token = strtok(NULL, ",");
  if (token) g_altitude = atof(token);

  token = strtok(NULL, ",");
  if (token) g_latitude = atof(token);

  token = strtok(NULL, ",");
  if (token) g_longitude = atof(token);

  return true;
}

// One received payload, from the radio or from the simulator. Both go through
// here so that sim mode exercises exactly the decode and display path a real
// link does -- including checksum rejection.
void handleRadioPayload(const uint8_t* rawPayload, uint8_t payloadSize) {
  bool packetDecoded = false;
  bool shouldPrintTelemetry = false;

  if (rawPayload[0] == PACKET_MAGIC && payloadSize == 32) {
    packetDecoded = processTelemetryFrame(rawPayload, payloadSize, &shouldPrintTelemetry);
    if (!packetDecoded) {
      Serial.println(F("[NRF24] Corrupted packet dropped (Frame checksum mismatch)."));
    }
  } else if (rawPayload[0] == PACKET_MAGIC) {
    packetDecoded = processBinaryPacket(rawPayload, payloadSize);
    shouldPrintTelemetry = packetDecoded;
  } else {
    packetDecoded = processCsvPacket((const char*)rawPayload);
    shouldPrintTelemetry = packetDecoded;
  }

  if (packetDecoded) {
    g_hasReceivedData = true;
    g_lastPacketTime = millis();
    g_heartbeatState = !g_heartbeatState;

    // Safe serial printing
    if (shouldPrintTelemetry) {
      Serial.print(DRONE_SOURCE == DRONE_SOURCE_SIM ? F("[SIM] BAT: ") : F("[RX] BAT: "));
      Serial.print(g_batVoltage, 2);
      Serial.print(F("V | RSSI: "));
      Serial.print(g_rssiVal);
      Serial.print(F("% | ALT: "));
      Serial.print(g_altitude, 1);
      Serial.print(F("m | LAT: "));
      Serial.print(g_latitude, 6);
      Serial.print(F(" | LON: "));
      Serial.print(g_longitude, 6);
      Serial.print(F(" | SATS: "));
      Serial.println(g_satellites);
    }
  }
}

// ======================================================================================
// OLED DRAWING FUNCTIONS
// ======================================================================================
void drawHardwareErrorScreen() {
  if (!g_panel.present()) return;
  u8g2.clearBuffer();
  u8g2.drawRFrame(0, 0, 128, 64, 3);

  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.drawBox(2, 2, 124, 15);
  u8g2.setDrawColor(0);
  u8g2.setCursor(12, 13);
  u8g2.print(F("! HARDWARE ERROR !"));
  u8g2.setDrawColor(1);

  u8g2.setFont(u8g2_font_ncenB08_tr);
  u8g2.setCursor(14, 34);
  u8g2.print(F("NRF24 Not Found"));

  u8g2.setFont(u8g2_font_5x8_tr);
  u8g2.setCursor(12, 50);
  u8g2.print(F("Check SPI & 5V Power"));
  u8g2.sendBuffer();
}

// A short bar sliding along the header line: the screen moves even when the
// numbers do not, so a live board never looks hung.
void drawActivitySweep() {
  u8g2.drawBox(bootanim::sweepX(millis(), 124), 15, 4, 3);
}

// Hands one line to loop() for printing (see g_oledLog).
void oledLog(const char* text) {
  portENTER_CRITICAL(&g_oledLogMux);
  snprintf(g_oledLog, sizeof g_oledLog, "%s", text);
  g_oledLogSeq = g_oledLogSeq + 1;
  portEXIT_CRITICAL(&g_oledLogMux);
}

// Called from loop(): prints the display task's latest line, if any.
void printOledLog() {
  static uint32_t printed = 0;
  if (g_oledLogSeq == printed) return;
  char line[sizeof g_oledLog];
  portENTER_CRITICAL(&g_oledLogMux);
  memcpy(line, g_oledLog, sizeof line);
  printed = g_oledLogSeq;
  portEXIT_CRITICAL(&g_oledLogMux);
  Serial.println(line);
}

// The fastest rung at which the panel answers every probe, or -1.
int probeBestRung() {
  for (int r = 0; r < bootanim::BUS_RUNGS; ++r) {
    Wire.setClock(bootanim::rungHz(r));
    int ok = 0;
    while (ok < bootanim::PROBES_PER_RUNG) {
      Wire.beginTransmission(OLED_I2C_ADDR);
      if (Wire.endTransmission() != 0) break;
      ++ok;
    }
    if (ok == bootanim::PROBES_PER_RUNG) return r;
  }
  return -1;
}

// Re-sends the panel's settings as bare commands (boot_anim.h), each command
// and its argument in one transfer, as u8g2's own sequences do. No reset, no
// delay, no display-off: a healthy panel shows no change.
void refreshPanel() {
  u8x8_t* u8x8 = u8g2.getU8x8();
  u8x8_cad_StartTransfer(u8x8);
  size_t i = 0;
  while (i < bootanim::PANEL_REFRESH_LEN) {
    uint8_t cmd = bootanim::PANEL_REFRESH_CMDS[i++];
    u8x8_cad_SendCmd(u8x8, cmd);
    for (int k = bootanim::refreshArgCount(cmd); k > 0 && i < bootanim::PANEL_REFRESH_LEN; --k) {
      u8x8_cad_SendArg(u8x8, bootanim::PANEL_REFRESH_CMDS[i++]);
    }
  }
  u8x8_cad_EndTransfer(u8x8);
}

// One probe round when due: pick the bus clock, track the panel, and begin or
// refresh it (boot_anim.h). True when the screen must be redrawn now.
bool panelRound(unsigned long now) {
  if (!g_panel.due(now)) return false;
  bool clockChanged = g_bus.update(probeBestRung());
  bootanim::Event e = g_panel.update(g_bus.rung() >= 0);
  if (g_bus.rung() >= 0) u8g2.setBusClock(g_bus.hz());  // u8g2 applies it per transfer

  char line[64];
  if (e == bootanim::EV_LOST) {
    oledLog("[OLED] not responding at 0x3C (SDA 21, SCL 22)");
  } else if (e == bootanim::EV_FOUND || clockChanged) {
    snprintf(line, sizeof line, "[OLED] %s at 0x3C, bus %lu kHz",
             e == bootanim::EV_FOUND ? "found" : "answering", (unsigned long)(g_bus.hz() / 1000));
    oledLog(line);
  }

  bootanim::PanelAction a = bootanim::panelAction(e, g_panel.present(), g_bus.slowedDown());
  if (a == bootanim::PA_REFRESH) {
    refreshPanel();
    return false;  // nothing to redraw: a healthy panel did not change
  }
  if (a != bootanim::PA_BEGIN) return false;
  u8g2.begin();
  u8g2.setContrast(255);
  return true;
}

void drawDisconnectScreen(unsigned long elapsedMs) {
  u8g2.clearBuffer();

  // Outer rounded border frame
  u8g2.drawRFrame(0, 0, 128, 64, 3);

  // Warning Header Banner
  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.drawBox(2, 2, 124, 15);
  u8g2.setDrawColor(0);
  u8g2.setCursor(14, 13);
  u8g2.print(F("! NO CONNECTION !"));
  u8g2.setDrawColor(1);

  u8g2.setFont(u8g2_font_ncenB08_tr);
  u8g2.setCursor(16, 32);
  u8g2.print(F("Telemetry Lost"));

  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(10, 46);
  if (!g_hasReceivedData) {
#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
    u8g2.print(WiFi.status() == WL_CONNECTED ? F("Waiting LiteWing") : F("Joining LiteWing"));
#else
    u8g2.print(F("Waiting NRF link"));
#endif
  } else {
    unsigned long elapsedSec = elapsedMs / 1000;
    u8g2.print(F("Lost "));
    u8g2.print(elapsedSec);
    u8g2.print(F("s"));
  }
  // Moves while the board is alive, so "lost" never looks like "frozen".
  u8g2.setCursor(112, 46);
  u8g2.print(bootanim::spinnerFrame(millis()));

  u8g2.setCursor(10, 59);
  u8g2.print(F("IP:"));
  u8g2.print(g_wifiIpText);

  u8g2.sendBuffer();
}

void drawWifiStatusScreen(const __FlashStringHelper* title, const char* ssid, const String& ipText) {
  if (!g_panel.present()) return;  // drawn from setup(), before the display task
  u8g2.clearBuffer();
  u8g2.drawRFrame(0, 0, 128, 64, 4);
  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(10, 14);
  u8g2.print(title);
  u8g2.setCursor(10, 30);
  u8g2.print(F("SSID:"));
  u8g2.print(ssid);
  u8g2.setCursor(10, 46);
  u8g2.print(F("IP:"));
  u8g2.print(ipText);
  u8g2.setCursor(10, 59);
  u8g2.print(F("/telemetry.json"));
  u8g2.sendBuffer();
}

void drawTelemetryScreen() {
  u8g2.clearBuffer();

  // LEVEL 1: Battery Voltage & Satellites / Heartbeat Indicator
  u8g2.setFont(u8g2_font_7x14B_tr);
  u8g2.setCursor(2, 13);
  u8g2.print(F("BAT:"));
  u8g2.print(g_batVoltage, 2);
  u8g2.print(F("V"));

  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(76, 12);
  u8g2.print(F("SAT:"));
  u8g2.print(g_satellites);

  // Heartbeat Dot
  if (g_heartbeatState) {
    u8g2.drawDisc(122, 9, 3);
  } else {
    u8g2.drawCircle(122, 9, 3);
  }

  u8g2.drawHLine(0, 16, 128);
  drawActivitySweep();

  // LEVEL 2: FlySky RSSI & Altitude in Meters
  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(2, 29);
  u8g2.print(F("RSSI:"));
  u8g2.print(g_rssiVal);
  u8g2.print(F("%"));

  u8g2.setCursor(68, 29);
  u8g2.print(F("ALT:"));
  u8g2.print(g_altitude, 1);
  u8g2.print(F("m"));

  u8g2.drawHLine(0, 33, 128);

  // LEVEL 3: High-Precision GPS Coordinates
  u8g2.setFont(u8g2_font_5x8_tr);

  u8g2.setCursor(2, 45);
  u8g2.print(F("LAT: "));
  if (g_latitude != 0.0) {
    u8g2.print(g_latitude, 6);
  } else {
    u8g2.print(F("NO FIX"));
  }

  u8g2.setCursor(2, 58);
  u8g2.print(F("LON: "));
  if (g_longitude != 0.0) {
    u8g2.print(g_longitude, 6);
  } else {
    u8g2.print(F("NO FIX"));
  }

  u8g2.sendBuffer();
}

#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
// ======================================================================================
// LITEWING LINK
// ======================================================================================
void liteWingBegin() {
  WiFi.mode(WIFI_STA);
  // Modem sleep parks the radio between beacons and adds 100 ms or more to
  // every reply, which is most of the client's retry budget.
  WiFi.setSleep(false);
  WiFi.begin(DRONE_LITEWING_SSID, DRONE_LITEWING_PASSWORD);
  g_lwWifiAttemptMs = millis();
  setIpText("joining");
  Serial.print(F("[LW] Joining "));
  Serial.println(F(DRONE_LITEWING_SSID));
}

// One line per fast-block sample, for backend/cmd/litewing-relay. The field
// set and ranges are what backend/pkg/litewing validates; change both together.
void printTlmLine(const crtplog::Telemetry& t, int rssi) {
  char alt[16];
  if (t.altValid) {
    snprintf(alt, sizeof alt, "%.3f", t.altM);
  } else {
    snprintf(alt, sizeof alt, "null");
  }
  char line[256];
  snprintf(line, sizeof line,
           "TLM {\"src\":\"litewing\",\"t\":%lu,\"roll\":%.2f,\"pitch\":%.2f,\"yaw\":%.2f,"
           "\"thrust\":%.0f,\"alt\":%s,\"vbat\":%.3f,\"pct\":%u,\"armed\":%d,\"rssi\":%d,"
           "\"m\":[%u,%u,%u,%u]}",
           (unsigned long)t.droneMs, t.roll, t.pitch, t.yaw, t.thrust, alt, t.vbat,
           (unsigned)t.pct, t.armed ? 1 : 0, rssi, (unsigned)t.motor[0], (unsigned)t.motor[1],
           (unsigned)t.motor[2], (unsigned)t.motor[3]);
  Serial.println(line);
}

void liteWingPump() {
  unsigned long now = millis();
  if (WiFi.status() != WL_CONNECTED) {
    if (g_lwUdpOpen) {
      g_lwUdp.stop();
      g_lwUdpOpen = false;
      setIpText("joining");
      Serial.println(F("[LW] Wi-Fi lost, rejoining"));
    }
    if (now - g_lwWifiAttemptMs >= LITEWING_WIFI_RETRY_MS) {
      WiFi.disconnect();
      WiFi.begin(DRONE_LITEWING_SSID, DRONE_LITEWING_PASSWORD);
      g_lwWifiAttemptMs = now;
    }
    return;
  }
  if (!g_lwUdpOpen) {
    g_lwUdpOpen = g_lwUdp.begin(LITEWING_LOCAL_PORT) == 1;
    setIpText(WiFi.localIP().toString().c_str());
    Serial.print(F("[LW] Joined, ip "));
    Serial.print(g_wifiIpText);
    Serial.print(F(", drone "));
    Serial.println(WiFi.gatewayIP());
    if (!g_lwUdpOpen) return;
  }

  // Receive everything waiting. A datagram over the link's 32 bytes is read
  // into a larger buffer so decode() sees its true size and rejects it.
  while (g_lwUdp.parsePacket() > 0) {
    uint8_t buf[64];
    int n = g_lwUdp.read(buf, sizeof buf);
    if (n > 0) g_lw.onDatagram(now, buf, (size_t)n);
  }

  // Send whatever the client wants sent: requests, retries, keepalives.
  uint8_t out[crtplog::MAX_DATAGRAM];
  size_t n;
  IPAddress drone = WiFi.gatewayIP();
  while ((n = g_lw.poll(now, out, sizeof out)) > 0) {
    g_lwUdp.beginPacket(drone, LITEWING_PORT);
    g_lwUdp.write(out, n);
    g_lwUdp.endPacket();
  }

  if (g_lw.state() != g_lwLastState) {
    Serial.printf("[LW] %s -> %s (toc %u, missing %d, restarts %lu, bad %lu)\n",
                  crtplog::stateName(g_lwLastState), crtplog::stateName(g_lw.state()),
                  (unsigned)g_lw.tocCount(), g_lw.missing(), (unsigned long)g_lw.restarts(),
                  (unsigned long)g_lw.badDatagrams());
    g_lwLastState = g_lw.state();
  }

  if (!g_lw.takeFreshSample()) return;
  const crtplog::Telemetry& t = g_lw.telemetry();
  g_batVoltage = t.vbat;
  g_batteryRemaining = t.pct;
  g_altitudeValid = t.altValid;
  g_altitude = t.altM;
  g_roll = t.roll;
  g_pitch = t.pitch;
  g_yaw = t.yaw;
  g_thrustPercent = t.thrust * 100.0f / 65535.0f;
  g_armed = t.armed;
  g_rssiVal = WiFi.RSSI();  // dBm on this source, not percent
  g_hasReceivedData = true;
  g_lastPacketTime = now;
  g_heartbeatState = !g_heartbeatState;
  printTlmLine(t, g_rssiVal);
}

// The link screen: shown from boot until the first sample, and again whenever
// the link is lost. Every element follows the real handshake (boot_anim.h).
void drawLinkScreen(unsigned long now, unsigned long lostMs) {
  bool wifiUp = WiFi.status() == WL_CONNECTED;
  bootanim::Stage st = bootanim::stageOf(wifiUp, g_lw.state());
  uint8_t pct = bootanim::progressPct(st, g_lw.tocRead(), g_lw.tocCount());

  u8g2.clearBuffer();

  // Header: what this screen is, or how long the link has been gone.
  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(2, 12);
  if (g_hasReceivedData) {
    u8g2.print(F("LINK LOST "));
    u8g2.print(lostMs / 1000);
    u8g2.print(F("s"));
  } else {
    u8g2.print(F("LITEWING LINK"));
  }
  u8g2.setCursor(110, 12);
  u8g2.print('[');
  u8g2.print(bootanim::spinnerFrame(now));
  u8g2.print(']');
  u8g2.drawHLine(0, 15, 128);

  // Stage, with the TOC position while it is being read.
  u8g2.setCursor(2, 29);
  u8g2.print(bootanim::stageLabel(st));
  if (st == bootanim::STG_READ_TOC && g_lw.tocCount() > 0) {
    u8g2.print(' ');
    u8g2.print(g_lw.tocRead());
    u8g2.print('/');
    u8g2.print(g_lw.tocCount());
  }

  // Progress bar and percent.
  u8g2.drawFrame(2, 34, 100, 9);
  u8g2.drawBox(4, 36, (uint8_t)(96u * pct / 100u), 5);
  u8g2.setCursor(106, 42);
  u8g2.print(pct);
  u8g2.print('%');

  // Stage dots: filled done, blinking current, dot to come, X failed.
  char dots[bootanim::DOTS + 1];
  bootanim::stageDots(st, dots);
  bool blinkOn = (now / 250) % 2 == 0;
  for (int i = 0; i < bootanim::DOTS; ++i) {
    int x = 5 + i * 9, y = 56;
    switch (dots[i]) {
      case 'o': u8g2.drawDisc(x, y, 2); break;
      case '*':
        if (blinkOn) u8g2.drawDisc(x, y, 3);
        else u8g2.drawCircle(x, y, 3);
        break;
      case 'x':
        u8g2.drawLine(x - 2, y - 2, x + 2, y + 2);
        u8g2.drawLine(x - 2, y + 2, x + 2, y - 2);
        break;
      default: u8g2.drawPixel(x, y); break;
    }
  }

  u8g2.setFont(u8g2_font_5x8_tr);
  u8g2.setCursor(50, 60);
  u8g2.print(F("ip "));
  u8g2.print(g_wifiIpText);

  u8g2.sendBuffer();
}

// Same layout language as drawTelemetryScreen. No GPS on this airframe, so the
// rows that carry coordinates elsewhere carry attitude and motor state.
void drawLiteWingScreen() {
  u8g2.clearBuffer();

  // LEVEL 1: battery volts and percent, heartbeat dot
  u8g2.setFont(u8g2_font_7x14B_tr);
  u8g2.setCursor(2, 13);
  u8g2.print(F("BAT:"));
  u8g2.print(g_batVoltage, 2);
  u8g2.print(F("V"));

  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(84, 12);
  u8g2.print(g_batteryRemaining);
  u8g2.print(F("%"));

  if (g_heartbeatState) {
    u8g2.drawDisc(122, 9, 3);
  } else {
    u8g2.drawCircle(122, 9, 3);
  }

  u8g2.drawHLine(0, 16, 128);
  drawActivitySweep();

  // LEVEL 2: Wi-Fi link strength and range-sensor altitude
  u8g2.setFont(u8g2_font_6x12_tf);
  u8g2.setCursor(2, 29);
  u8g2.print(F("RSSI:"));
  u8g2.print(g_rssiVal);

  u8g2.setCursor(68, 29);
  u8g2.print(F("ALT:"));
  if (g_altitudeValid) {
    u8g2.print(g_altitude, 2);
    u8g2.print(F("m"));
  } else {
    u8g2.print(F("---"));
  }

  u8g2.drawHLine(0, 33, 128);

  // LEVEL 3: attitude, degrees
  u8g2.setFont(u8g2_font_5x8_tr);
  char row[32];
  snprintf(row, sizeof row, "R:%+.1f P:%+.1f Y:%.0f", g_roll, g_pitch, g_yaw);
  u8g2.setCursor(2, 45);
  u8g2.print(row);

  // LEVEL 4: thrust and motor state; ARMED is inverted so it cannot be missed
  snprintf(row, sizeof row, "THR:%3.0f%%", g_thrustPercent);
  u8g2.setCursor(2, 58);
  u8g2.print(row);
  if (g_armed) {
    u8g2.drawBox(70, 50, 56, 10);
    u8g2.setDrawColor(0);
    u8g2.setCursor(80, 58);
    u8g2.print(F("ARMED"));
    u8g2.setDrawColor(1);
  } else {
    u8g2.setCursor(74, 58);
    u8g2.print(F("DISARMED"));
  }

  u8g2.sendBuffer();
}
#endif


void addCorsHeaders() {
  server.sendHeader(F("Access-Control-Allow-Origin"), F("*"));
  server.sendHeader(F("Access-Control-Allow-Methods"), F("GET, OPTIONS"));
  server.sendHeader(F("Access-Control-Allow-Headers"), F("Content-Type"));
}

String makeTelemetryJson() {
  bool radioFresh = g_hasReceivedData && (millis() - g_lastPacketTime < DISCONNECT_TIMEOUT_MS);
  String json;
  json.reserve(1800);

  // "source" says where the numbers came from. Anything consuming this API can
  // tell simulated telemetry from a real link by this field alone.
  json += DRONE_SOURCE == DRONE_SOURCE_SIM        ? F("{\"source\":\"esp32-sim\",\"updated_ms\":")
          : DRONE_SOURCE == DRONE_SOURCE_LITEWING ? F("{\"source\":\"esp32-litewing\",\"updated_ms\":")
                                                  : F("{\"source\":\"esp32-nrf\",\"updated_ms\":");
  json += millis();
  json += F(",\"connected\":");
  json += (radioFresh ? F("true") : F("false"));
  json += F(",\"mavlink_connected\":");
  json += (g_piMavlinkConnected ? F("true") : F("false"));
  json += F(",\"seq\":");
  json += String((int)g_lastSeq);
  json += F(",\"battery_voltage\":");
  json += String(g_batVoltage, 2);
  json += F(",\"battery_current\":");
  json += String(g_batteryCurrent, 2);
  json += F(",\"battery_remaining\":");
  json += String((int)g_batteryRemaining);
  json += F(",\"cell_voltages\":[");
  for (uint8_t i = 0; i < 6; i++) {
    if (i) json += ',';
    json += String(g_cellVoltages[i], 3);
  }
  json += F("],\"cell_delta_mv\":");
  json += g_cellDeltaMv;
  json += F(",\"altitude_relative\":");
  json += String(g_altitude, 2);
  json += F(",\"altitude_msl\":");
  json += String(g_altitudeMsl, 2);
  json += F(",\"climb_rate\":");
  json += String(g_climbRate, 2);
  json += F(",\"latitude\":");
  json += String(g_latitude, 7);
  json += F(",\"longitude\":");
  json += String(g_longitude, 7);
  json += F(",\"satellites\":");
  json += String((int)g_satellites);
  json += F(",\"gps_fix_type\":\"");
  json += gpsFixLabel(g_gpsFix);
  json += F("\",\"rc_rssi\":");
  json += g_rssiVal;
  json += F(",\"radio_link_quality\":");
  json += String((int)g_linkQuality);
  json += F(",\"heading\":");
  json += String(g_heading, 2);
  json += F(",\"ground_speed\":");
  json += String(g_groundSpeed, 2);
  json += F(",\"armed\":");
  json += (g_armed ? F("true") : F("false"));
  json += F(",\"flight_mode\":\"");
  json += flightModeLabel(g_flightMode);
  json += F("\",\"system_status\":\"");
  json += systemStatusLabel(g_systemStatus);
  json += F("\",\"mission_state\":\"");
  json += missionStateLabel(g_missionState);
  json += F("\",\"mission_progress_percent\":");
  json += String((int)g_missionProgress);
  json += F(",\"mission_current_seq\":");
  json += g_missionCurrentSeq;
  json += F(",\"mission_total_items\":");
  json += g_missionTotalItems;
  json += F(",\"attitude_roll\":");
  json += String(g_roll, 2);
  json += F(",\"attitude_pitch\":");
  json += String(g_pitch, 2);
  json += F(",\"attitude_yaw\":");
  json += String(g_yaw, 2);
  json += F(",\"target_roll\":");
  json += String(g_targetRoll, 2);
  json += F(",\"target_pitch\":");
  json += String(g_targetPitch, 2);
  json += F(",\"target_yaw\":");
  json += String(g_targetYaw, 2);
  json += F(",\"error_roll\":");
  json += String(g_errorRoll, 2);
  json += F(",\"error_pitch\":");
  json += String(g_errorPitch, 2);
  json += F(",\"gyro_x\":");
  json += String(g_gyroX, 2);
  json += F(",\"gyro_y\":");
  json += String(g_gyroY, 2);
  json += F(",\"gyro_z\":");
  json += String(g_gyroZ, 2);
  json += F(",\"accel_x\":");
  json += String(g_accelX, 2);
  json += F(",\"accel_y\":");
  json += String(g_accelY, 2);
  json += F(",\"accel_z\":");
  json += String(g_accelZ, 2);
  json += F(",\"motor_percent\":[");
  for (uint8_t i = 0; i < 4; i++) {
    if (i) json += ',';
    json += String((int)g_motorPercent[i]);
  }
  json += F("],\"motor_pwm\":[");
  for (uint8_t i = 0; i < 4; i++) {
    if (i) json += ',';
    json += g_motorPwm[i];
  }
  json += F("],\"sbc\":{\"cpu_load_percent\":");
  json += String((int)g_cpuLoad);
  json += F(",\"cpu_temp_c\":");
  json += String(g_cpuTemp, 1);
  json += F(",\"ram_percent\":");
  json += String((int)g_ramPercent);
  json += F(",\"disk_percent\":");
  json += String((int)g_diskPercent);
  json += F(",\"uptime_seconds\":");
  json += g_sbcUptime;
  json += F("}}");

  return json;
}

void handleTelemetryJson() {
  addCorsHeaders();
  server.send(200, F("application/json"), makeTelemetryJson());
}

void handleRoot() {
  addCorsHeaders();
  String message = F("ESP32 drone telemetry JSON: http://");
  message += g_wifiIpText;
  message += F("/telemetry.json");
  server.send(200, F("text/plain"), message);
}

void handleOptions() {
  addCorsHeaders();
  server.send(204);
}

bool hasStationCredentials() {
  return strlen(WIFI_STA_SSID) > 0 && strlen(WIFI_STA_PASSWORD) > 0;
}

bool hasFallbackAccessPointCredentials() {
  return strlen(WIFI_FALLBACK_AP_SSID) > 0 &&
         strlen(WIFI_FALLBACK_AP_PASSWORD) >= 12 &&
         strcmp(WIFI_FALLBACK_AP_PASSWORD, "CHANGE_ME") != 0;
}

void startFallbackAccessPoint() {
  if (!hasFallbackAccessPointCredentials()) {
    g_wifiApMode = false;
    setIpText("NO WIFI");
    WiFi.mode(WIFI_OFF);
    Serial.println(F("[WIFI] Fallback AP disabled. Define DRONE_WIFI_FALLBACK_AP_PASSWORD with a per-device strong password."));
    drawWifiStatusScreen(F("WIFI NOT CONFIG"), "NO CREDENTIALS", g_wifiIpText);
    return;
  }

  g_wifiApMode = true;
  WiFi.mode(WIFI_AP);
  WiFi.softAP(WIFI_FALLBACK_AP_SSID, WIFI_FALLBACK_AP_PASSWORD);
  setIpText(WiFi.softAPIP().toString().c_str());

  Serial.print(F("[WIFI] Fallback AP started: "));
  Serial.print(WIFI_FALLBACK_AP_SSID);
  Serial.print(F(" / "));
  Serial.println(g_wifiIpText);

  drawWifiStatusScreen(F("WIFI AP MODE"), WIFI_FALLBACK_AP_SSID, g_wifiIpText);
}

void setupWifiApi() {
  g_wifiApMode = false;
  WiFi.setHostname(WIFI_HOSTNAME);

  if (hasStationCredentials()) {
    WiFi.mode(WIFI_STA);
    WiFi.begin(WIFI_STA_SSID, WIFI_STA_PASSWORD);
    drawWifiStatusScreen(F("WIFI CONNECTING"), WIFI_STA_SSID, F("..."));

    Serial.print(F("[WIFI] Connecting to "));
    Serial.println(WIFI_STA_SSID);

    unsigned long startMs = millis();
    while (WiFi.status() != WL_CONNECTED && millis() - startMs < 15000) {
      delay(300);
      Serial.print('.');
    }
    Serial.println();

    if (WiFi.status() == WL_CONNECTED) {
      setIpText(WiFi.localIP().toString().c_str());
      Serial.print(F("[WIFI] Connected: "));
      Serial.print(WIFI_STA_SSID);
      Serial.print(F(" / "));
      Serial.println(g_wifiIpText);
      drawWifiStatusScreen(F("WIFI CONNECTED"), WIFI_STA_SSID, g_wifiIpText);
    } else {
      Serial.println(F("[WIFI] Station connection failed. Starting fallback AP."));
      startFallbackAccessPoint();
    }
  } else {
    Serial.println(F("[WIFI] Station credentials not set. Starting fallback AP."));
    startFallbackAccessPoint();
  }

  if (strcmp(g_wifiIpText, "NO WIFI") == 0) {
    Serial.println(F("[HTTP] Telemetry JSON disabled until Wi-Fi credentials are configured."));
    return;
  }

  server.on("/", HTTP_GET, handleRoot);
  server.on("/telemetry.json", HTTP_GET, handleTelemetryJson);
  server.on("/api/telemetry", HTTP_GET, handleTelemetryJson);
  server.on("/telemetry.json", HTTP_OPTIONS, handleOptions);
  server.on("/api/telemetry", HTTP_OPTIONS, handleOptions);
  server.onNotFound(handleRoot);
  server.begin();

  if (MDNS.begin(WIFI_HOSTNAME)) {
    MDNS.addService("http", "tcp", HTTP_PORT);
    Serial.print(F("[MDNS] http://"));
    Serial.print(WIFI_HOSTNAME);
    Serial.println(F(".local/telemetry.json"));
  }

  Serial.print(F("[HTTP] Telemetry JSON: http://"));
  Serial.print(g_wifiIpText);
  Serial.println(F("/telemetry.json"));
}

// ======================================================================================
// DISPLAY TASK
// ======================================================================================
void drawCurrentScreen() {
  if (!g_radioHardwareOk) {
    drawHardwareErrorScreen();
    return;
  }
  // Read the packet time BEFORE the clock: loop() may stamp a packet between
  // the two reads, and the other order would make "since" underflow to ~49
  // days and flash the lost screen for a frame.
  unsigned long lastPacket = g_lastPacketTime;
  unsigned long now = millis();
  unsigned long since = now - lastPacket;
  if (!g_hasReceivedData || since >= DISCONNECT_TIMEOUT_MS) {
#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
    drawLinkScreen(now, since);
#else
    drawDisconnectScreen(since);
#endif
  } else {
#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
    drawLiteWingScreen();
#else
    drawTelemetryScreen();
#endif
  }
}

void displayTask(void*) {
  const unsigned long started = millis();
  bool stackReported = false;
  unsigned long maxRoundMs = 0, maxFrameMs = 0;
  for (;;) {
    unsigned long now = millis();
    bool redrawNow = panelRound(now);
    unsigned long roundMs = millis() - now;
    if (roundMs > maxRoundMs) maxRoundMs = roundMs;
    // Once, after every screen has had time to draw: how close the stack got,
    // and the longest stall a probe round or a frame caused (a freeze shows here).
    if (!stackReported && now - started >= 30000) {
      char line[64];
      snprintf(line, sizeof line, "[OLED] stack %u B free, max round %lu ms, max frame %lu ms",
               (unsigned)uxTaskGetStackHighWaterMark(nullptr), maxRoundMs, maxFrameMs);
      oledLog(line);
      stackReported = true;
    }
    // An absent panel would NACK every frame; skip drawing until it answers.
    if (g_panel.present() && (redrawNow || now - g_lastDisplayRefresh >= DISPLAY_REFRESH_INTERVAL_MS)) {
      g_lastDisplayRefresh = now;
      unsigned long t0 = millis();
      drawCurrentScreen();
      if (millis() - t0 > maxFrameMs) maxFrameMs = millis() - t0;
    }
    vTaskDelay(pdMS_TO_TICKS(10));
  }
}

// ======================================================================================
// ARDUINO SETUP
// ======================================================================================
void setup() {
  Serial.begin(115200);
  delay(1000);

  Serial.println(F("\n\n=============================================="));
  Serial.println(F("    ESP32 Drone Telemetry Receiver Booting    "));
  Serial.println(F("=============================================="));
#if DRONE_SOURCE == DRONE_SOURCE_SIM
  Serial.println(F("[SIM] SIMULATION MODE: telemetry is generated on this board."));
  Serial.println(F("[SIM] Not a real link. Build with -DDRONE_SOURCE=0 or 2 for a drone."));
#elif DRONE_SOURCE == DRONE_SOURCE_LITEWING
  Serial.println(F("[LW] LITEWING MODE: reading the drone's log stream over its Wi-Fi."));
  Serial.println(F("[LW] Read only. Samples are printed as TLM {json} lines."));
#endif

  // 1. Initialize Hardware I2C and SH1106 OLED. The first probe round picks a
  // bus clock the wiring can carry and begins the panel at it; u8g2's own
  // 400 kHz fails outright on a poor SDA contact.
  Wire.begin(PIN_OLED_SDA, PIN_OLED_SCL);
  panelRound(millis());

  // Splash Screen
  if (g_panel.present()) {
    u8g2.clearBuffer();
    u8g2.setFont(u8g2_font_ncenB08_tr);
    u8g2.drawRFrame(0, 0, 128, 64, 4);
    u8g2.setCursor(12, 25);
    u8g2.print(F("DRONE TELEMETRY"));
    u8g2.setFont(u8g2_font_6x12_tf);
    u8g2.setCursor(22, 45);
    u8g2.print(F("Initializing..."));
    u8g2.sendBuffer();
  }

#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
  // The Wi-Fi radio belongs to the drone link, so there is no HTTP API in this
  // mode: the laptop gets the same data from the serial TLM lines instead.
  liteWingBegin();
  g_radioHardwareOk = true;
#else
  setupWifiApi();
#endif

#if DRONE_SOURCE != DRONE_SOURCE_NRF
  // No NRF24 in sim or LiteWing mode, so a missing or unwired radio does not
  // put the HARDWARE ERROR screen in front of a working link.
  g_radioHardwareOk = true;
#else
  // 2. Initialize VSPI Bus
  SPI.begin(PIN_NRF_SCK, PIN_NRF_MISO, PIN_NRF_MOSI, -1);
  pinMode(PIN_NRF_CE, OUTPUT);
  pinMode(PIN_NRF_CSN, OUTPUT);
  digitalWrite(PIN_NRF_CSN, HIGH);

  // 3. Initialize NRF24L01+ Radio
  if (!radio.begin()) {
    Serial.println(F("[ERROR] NRF24L01 hardware not detected! Check SPI wiring & HW-200 5V power."));
    g_radioHardwareOk = false;
    drawHardwareErrorScreen();
  } else {
    g_radioHardwareOk = true;
    radio.setChannel(RF_CHANNEL);
    radio.setDataRate(RF24_250KBPS);      // Long range mode (250 kbps)
    radio.setPALevel(RF24_PA_HIGH);
    radio.setCRCLength(RF24_CRC_16);
    radio.setAutoAck(false);
    radio.enableDynamicPayloads();

    radio.openReadingPipe(1, rfAddresses[1]); // "2Node"
    radio.openWritingPipe(rfAddresses[0]);
    radio.startListening();

    Serial.print(F("[NRF24] Radio initialized successfully on Channel "));
    Serial.println(RF_CHANNEL);
  }
#endif

  // From here on only the display task touches Wire and u8g2. Core 0, beside
  // the Wi-Fi stack: its I2C waits block, they do not spin, and loop() keeps
  // core 1 to itself.
  xTaskCreatePinnedToCore(displayTask, "oled", 6144, nullptr, 1, &g_displayTask, 0);
}

// ======================================================================================
// MAIN ARDUINO LOOP
// ======================================================================================
void loop() {
  printOledLog();
  server.handleClient();

  if (!g_radioHardwareOk) {
    delay(500);
    return;
  }

#if DRONE_SOURCE == DRONE_SOURCE_LITEWING
  // 1. Exchange datagrams with the drone and publish any new sample.
  liteWingPump();
#elif DRONE_SOURCE == DRONE_SOURCE_SIM
  // 1. Drain the simulated link, exactly as the radio is drained below.
  uint8_t simFrame[FRAME_SIZE];
  while (g_sim.nextFrame(millis(), simFrame)) {
    handleRadioPayload(simFrame, FRAME_SIZE);
  }
#else
  // 1. Non-blocking Radio Packet Polling
  while (radio.available()) {
    uint8_t rawPayload[32] = {0};
    uint8_t payloadSize = radio.getDynamicPayloadSize();
    if (payloadSize == 0 || payloadSize > 32) {
      payloadSize = sizeof(TelemetryPacket);
    }

    radio.read(&rawPayload, payloadSize);
    handleRadioPayload(rawPayload, payloadSize);
  }
#endif
  // The OLED is drawn by displayTask.
}
