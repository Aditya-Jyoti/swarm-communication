// Host unit tests for boot_anim.h.
//
//   esp/test/run_tests.sh      # build with coverage, run, report

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <set>
#include <string>

#include "../boot_anim.h"

using namespace bootanim;

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

// ---------------------------------------------------------------- stages

static void test_no_wifi_wins_over_any_client_state() {
  const crtplog::State all[] = {crtplog::ST_GET_INFO, crtplog::ST_READ_TOC, crtplog::ST_SETUP,
                                crtplog::ST_STREAMING, crtplog::ST_UNSUPPORTED};
  for (size_t i = 0; i < sizeof all / sizeof all[0]; ++i) CHECK(stageOf(false, all[i]) == STG_WIFI);
}

static void test_client_state_maps_to_stage() {
  CHECK(stageOf(true, crtplog::ST_GET_INFO) == STG_GET_INFO);
  CHECK(stageOf(true, crtplog::ST_READ_TOC) == STG_READ_TOC);
  CHECK(stageOf(true, crtplog::ST_SETUP) == STG_SETUP);
  CHECK(stageOf(true, crtplog::ST_STREAMING) == STG_STREAMING);
  CHECK(stageOf(true, crtplog::ST_UNSUPPORTED) == STG_UNSUPPORTED);
  CHECK(stageOf(true, (crtplog::State)99) == STG_GET_INFO);  // unknown: start of handshake
}

// ---------------------------------------------------------------- progress

static void test_progress_bands() {
  CHECK(progressPct(STG_WIFI, 0, 0) == 0);
  CHECK(progressPct(STG_GET_INFO, 0, 0) == 20);
  CHECK(progressPct(STG_READ_TOC, 0, 316) == 30);
  CHECK(progressPct(STG_READ_TOC, 316, 316) == 85);
  CHECK(progressPct(STG_READ_TOC, 158, 316) == 57);  // 30 + 55/2, truncated
  CHECK(progressPct(STG_SETUP, 0, 0) == 90);
  CHECK(progressPct(STG_STREAMING, 0, 0) == 100);
  CHECK(progressPct(STG_UNSUPPORTED, 5, 10) == 0);
  CHECK(progressPct((Stage)42, 0, 0) == 0);
}

static void test_progress_toc_edges_are_clamped() {
  CHECK(progressPct(STG_READ_TOC, 0, 0) == 30);       // count not known yet
  CHECK(progressPct(STG_READ_TOC, 7, 0) == 30);       // no divide by zero
  CHECK(progressPct(STG_READ_TOC, 500, 316) == 85);   // read past count
  CHECK(progressPct(STG_READ_TOC, 65535, 65535) == 85);
  CHECK(progressPct(STG_READ_TOC, 1, 65535) == 30);
}

static void test_progress_never_goes_backwards_through_the_handshake() {
  uint8_t prev = 0;
  prev = progressPct(STG_WIFI, 0, 0);
  CHECK(progressPct(STG_GET_INFO, 0, 0) >= prev);
  prev = progressPct(STG_GET_INFO, 0, 0);
  for (uint16_t r = 0; r <= 316; ++r) {
    uint8_t p = progressPct(STG_READ_TOC, r, 316);
    CHECK(p >= prev);
    prev = p;
  }
  CHECK(progressPct(STG_SETUP, 0, 0) >= prev);
  CHECK(progressPct(STG_STREAMING, 0, 0) >= progressPct(STG_SETUP, 0, 0));
}

// ---------------------------------------------------------------- text

static void test_labels_fit_the_screen() {
  const Stage all[] = {STG_WIFI, STG_GET_INFO, STG_READ_TOC, STG_SETUP, STG_STREAMING, STG_UNSUPPORTED};
  std::set<std::string> seen;
  for (size_t i = 0; i < sizeof all / sizeof all[0]; ++i) {
    const char* s = stageLabel(all[i]);
    CHECK(s != nullptr);
    CHECK(std::strlen(s) > 0);
    CHECK(std::strlen(s) <= 13);  // leaves room for " 316/316" in a 6x12 font
    seen.insert(s);
  }
  CHECK(seen.size() == 6);
  CHECK(std::strcmp(stageLabel(STG_READ_TOC), "Reading TOC") == 0);
  CHECK(std::strcmp(stageLabel((Stage)42), "?") == 0);
}

static void test_stage_dots() {
  char d[DOTS + 1];
  stageDots(STG_WIFI, d);
  CHECK(std::strcmp(d, "*....") == 0);
  stageDots(STG_GET_INFO, d);
  CHECK(std::strcmp(d, "o*...") == 0);
  stageDots(STG_READ_TOC, d);
  CHECK(std::strcmp(d, "oo*..") == 0);
  stageDots(STG_SETUP, d);
  CHECK(std::strcmp(d, "ooo*.") == 0);
  stageDots(STG_STREAMING, d);
  CHECK(std::strcmp(d, "oooo*") == 0);
  stageDots(STG_UNSUPPORTED, d);
  CHECK(std::strcmp(d, "oox..") == 0);
  stageDots((Stage)42, d);
  CHECK(std::strcmp(d, ".....") == 0);
  CHECK(std::strlen(d) == DOTS);
}

// ---------------------------------------------------------------- motion

static void test_spinner_cycles_every_125ms() {
  CHECK(spinnerFrame(0) == '|');
  CHECK(spinnerFrame(124) == '|');
  CHECK(spinnerFrame(125) == '/');
  CHECK(spinnerFrame(250) == '-');
  CHECK(spinnerFrame(375) == '\\');
  CHECK(spinnerFrame(500) == '|');
  // Valid across millis() wrap.
  const char* ok = "|/-\\";
  CHECK(std::strchr(ok, spinnerFrame(0xFFFFFFFFu)) != nullptr);
  CHECK(std::strchr(ok, spinnerFrame(0xFFFFFF00u)) != nullptr);
}

static void test_sweep_bounces_inside_width() {
  CHECK(sweepX(0, 0) == 0);
  CHECK(sweepX(12345, 0) == 0);
  CHECK(sweepX(12345, 1) == 0);
  CHECK(sweepX(0, 10) == 0);
  CHECK(sweepX(SWEEP_STEP_MS * 9, 10) == 9);   // right edge
  CHECK(sweepX(SWEEP_STEP_MS * 10, 10) == 8);  // and back
  CHECK(sweepX(SWEEP_STEP_MS * 18, 10) == 0);  // full period
  int prev = sweepX(0, 128);
  bool smooth = true;
  for (uint32_t t = 0; t < 20000; t += SWEEP_STEP_MS) {
    int x = sweepX(t, 128);
    if (x < 0 || x > 127) smooth = false;
    int d = x - prev;
    if (d > 1 || d < -1) smooth = false;
    prev = x;
  }
  CHECK(smooth);
  CHECK(sweepX(0xFFFFFFFFu, 128) <= 127);
  CHECK(sweepX(0xFFFFFFFFu, 255) <= 254);
}

// ---------------------------------------------------------------- panel watch

static void test_panel_watch_probe_schedule() {
  PanelWatch w;
  CHECK(w.due(5000));    // first call is always due
  CHECK(!w.due(5000));
  CHECK(!w.due(5999));
  CHECK(w.due(6000));
  CHECK(!w.due(6500));
  CHECK(w.due(7100));    // late is fine; next is counted from here
  CHECK(!w.due(8000));
  CHECK(w.due(8100));
}

static void test_panel_watch_schedule_across_millis_wrap() {
  PanelWatch w;
  CHECK(w.due(0xFFFFFE00u));
  CHECK(!w.due(0xFFFFFFFFu));
  CHECK(!w.due(0x00000100u));  // 0x301 ms later: not yet
  CHECK(w.due(0x000001E8u));   // 1000 ms later, across the wrap
}

static void test_panel_watch_first_probe_reports_either_way() {
  PanelWatch a;
  CHECK(!a.known());
  CHECK(a.update(true) == EV_FOUND);
  CHECK(a.known());
  CHECK(a.present());
  PanelWatch b;
  CHECK(b.update(false) == EV_LOST);
  CHECK(b.known());
  CHECK(!b.present());
}

static void test_panel_watch_transitions_only() {
  PanelWatch w;
  CHECK(w.update(true) == EV_FOUND);
  CHECK(w.update(true) == EV_NONE);
  CHECK(w.update(false) == EV_LOST);
  CHECK(w.update(false) == EV_NONE);
  CHECK(!w.present());
  CHECK(w.update(true) == EV_FOUND);  // came back: the sketch re-inits the panel
  CHECK(w.present());
  CHECK(w.found() == 2);
}

static void test_panel_watch_flapping_wire() {
  PanelWatch w;
  int found = 0, lost = 0;
  for (int i = 0; i < 100; ++i) {
    Event e = w.update(i % 3 != 0);
    if (e == EV_FOUND) ++found;
    if (e == EV_LOST) ++lost;
  }
  // Pattern F,T,T,F,T,T...: the first probe fails, then every failure is a loss
  // and every recovery a find.
  CHECK(lost == 34);
  CHECK(found == 33);
  CHECK(w.found() == 33);
}

static void test_event_names() {
  CHECK(std::strcmp(eventName(EV_NONE), "none") == 0);
  CHECK(std::strcmp(eventName(EV_FOUND), "found") == 0);
  CHECK(std::strcmp(eventName(EV_LOST), "not responding") == 0);
  CHECK(std::strcmp(eventName((Event)9), "?") == 0);
}

#define RUN(fn)                                                \
  do {                                                         \
    if (std::getenv("CRTP_TRACE")) std::printf("%s\n", #fn);   \
    std::fflush(stdout);                                       \
    fn();                                                      \
  } while (0)

int main() {
  RUN(test_no_wifi_wins_over_any_client_state);
  RUN(test_client_state_maps_to_stage);
  RUN(test_progress_bands);
  RUN(test_progress_toc_edges_are_clamped);
  RUN(test_progress_never_goes_backwards_through_the_handshake);
  RUN(test_labels_fit_the_screen);
  RUN(test_stage_dots);
  RUN(test_spinner_cycles_every_125ms);
  RUN(test_sweep_bounces_inside_width);
  RUN(test_panel_watch_probe_schedule);
  RUN(test_panel_watch_schedule_across_millis_wrap);
  RUN(test_panel_watch_first_probe_reports_either_way);
  RUN(test_panel_watch_transitions_only);
  RUN(test_panel_watch_flapping_wire);
  RUN(test_event_names);
  std::printf("boot_anim: %d checks, %d failures\n", g_checks, g_failures);
  return g_failures == 0 ? 0 : 1;
}
