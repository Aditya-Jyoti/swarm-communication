// What the ground unit's OLED shows while it connects, and whether the OLED is
// there at all.
//
// Plain C++ with no Arduino dependencies, so it is tested on the host
// (esp/test/test_boot_anim.cpp). The sketch only draws what this returns and
// does the I2C probe; every decision lives here.
//
// The progress bar follows the real handshake (docs/specs/
// 2026-10-22-handheld-boot-animation-design.md), never a timer: a bar that
// moves while nothing happens would hide a stuck link, which is the one thing
// a ground unit must not do.
#pragma once

#include <stdint.h>

#include "crtp_log.h"

namespace bootanim {

enum Stage { STG_WIFI, STG_GET_INFO, STG_READ_TOC, STG_SETUP, STG_STREAMING, STG_UNSUPPORTED };

// Stage dots on the link screen: Wi-Fi, info, TOC, setup, streaming.
const int DOTS = 5;
// Spinner frame time, and the activity sweep's time per pixel.
const uint32_t SPINNER_FRAME_MS = 125;
const uint32_t SWEEP_STEP_MS = 15;
// How often the sketch probes the panel's I2C address.
const uint32_t PANEL_PROBE_MS = 1000;

// No Wi-Fi beats any client state: the client keeps its last state while the
// link is down, and the screen must say why nothing arrives.
inline Stage stageOf(bool wifiUp, crtplog::State s) {
  if (!wifiUp) return STG_WIFI;
  switch (s) {
    case crtplog::ST_GET_INFO: return STG_GET_INFO;
    case crtplog::ST_READ_TOC: return STG_READ_TOC;
    case crtplog::ST_SETUP: return STG_SETUP;
    case crtplog::ST_STREAMING: return STG_STREAMING;
    case crtplog::ST_UNSUPPORTED: return STG_UNSUPPORTED;
  }
  return STG_GET_INFO;
}

// Percent for the bar. Reading the TOC is the long stage (one round trip per
// entry), so it gets most of the bar. The scan stops once every wanted
// variable is found, so the bar may jump from part-way to setup: the entries
// it skips are never read.
inline uint8_t progressPct(Stage st, uint16_t tocRead, uint16_t tocCount) {
  switch (st) {
    case STG_WIFI: return 0;
    case STG_GET_INFO: return 20;
    case STG_READ_TOC: {
      if (tocCount == 0) return 30;
      uint32_t r = tocRead > tocCount ? tocCount : tocRead;
      return (uint8_t)(30 + 55u * r / tocCount);
    }
    case STG_SETUP: return 90;
    case STG_STREAMING: return 100;
    case STG_UNSUPPORTED: return 0;
  }
  return 0;
}

// At most 13 characters, so "Reading TOC 316/316" fits a 6x12 line.
inline const char* stageLabel(Stage st) {
  switch (st) {
    case STG_WIFI: return "Joining Wi-Fi";
    case STG_GET_INFO: return "Asking drone";
    case STG_READ_TOC: return "Reading TOC";
    case STG_SETUP: return "Setting up";
    case STG_STREAMING: return "Streaming";
    case STG_UNSUPPORTED: return "Unsupported";
  }
  return "?";
}

// out gets DOTS characters and a terminator: 'o' done, '*' current, '.' to
// come. Unsupported means the TOC had nothing to stream, so that dot is 'x'.
inline void stageDots(Stage st, char out[DOTS + 1]) {
  int cur;
  switch (st) {
    case STG_WIFI: cur = 0; break;
    case STG_GET_INFO: cur = 1; break;
    case STG_READ_TOC: cur = 2; break;
    case STG_SETUP: cur = 3; break;
    case STG_STREAMING: cur = 4; break;
    case STG_UNSUPPORTED: cur = 2; break;
    default: cur = -1; break;
  }
  for (int i = 0; i < DOTS; ++i) {
    if (cur < 0 || i > cur) {
      out[i] = '.';
    } else if (i < cur) {
      out[i] = 'o';
    } else {
      out[i] = st == STG_UNSUPPORTED ? 'x' : '*';
    }
  }
  out[DOTS] = '\0';
}

inline char spinnerFrame(uint32_t ms) {
  static const char frames[] = "|/-\\";
  return frames[(ms / SPINNER_FRAME_MS) & 3u];
}

// A dot bouncing across width pixels, one pixel per SWEEP_STEP_MS.
inline uint8_t sweepX(uint32_t ms, uint8_t width) {
  if (width < 2) return 0;
  uint32_t period = 2u * (width - 1u);
  uint32_t step = (ms / SWEEP_STEP_MS) % period;
  return (uint8_t)(step < width ? step : period - step);
}

enum Event { EV_NONE, EV_FOUND, EV_LOST };

inline const char* eventName(Event e) {
  switch (e) {
    case EV_NONE: return "none";
    case EV_FOUND: return "found";
    case EV_LOST: return "not responding";
  }
  return "?";
}

// Tracks the panel across probes. The first probe reports either way, so the
// boot log always says whether the panel answered; after that only changes
// are reported. EV_FOUND after a loss tells the sketch to re-initialise the
// panel, which lost its configuration if it lost power or saw a garbled bus.
class PanelWatch {
 public:
  PanelWatch() : known_(false), present_(false), probed_(false), lastProbe_(0), found_(0) {}

  // True when a probe is due; the call that returns true starts the next wait.
  bool due(uint32_t now) {
    if (probed_ && now - lastProbe_ < PANEL_PROBE_MS) return false;
    probed_ = true;
    lastProbe_ = now;
    return true;
  }

  Event update(bool ack) {
    bool changed = !known_ || ack != present_;
    known_ = true;
    present_ = ack;
    if (!changed) return EV_NONE;
    if (ack) {
      ++found_;
      return EV_FOUND;
    }
    return EV_LOST;
  }

  bool known() const { return known_; }
  bool present() const { return present_; }
  uint32_t found() const { return found_; }

 private:
  bool known_;
  bool present_;
  bool probed_;
  uint32_t lastProbe_;
  uint32_t found_;
};

}  // namespace bootanim
