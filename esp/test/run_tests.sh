#!/usr/bin/env bash
# Builds and runs the host unit tests for the ground unit's plain-C++ headers
# (the flight simulator, the frame format and the LiteWing CRTP log client),
# with line and branch coverage of every header the board compiles.
#
#   esp/test/run_tests.sh
#
# Needs only g++ and gcov. The board's toolchain is not involved: the headers
# under test are plain C++ precisely so they can be checked here.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

# The board's core decides the language: arduino-esp32 2.x builds gnu++11,
# 3.x a recent standard. The headers must compile, link and pass under both.
# The harness itself is C++17; this C++11 build exercises only the headers.
g++ -std=c++11 -Wall -Wextra -Werror -I"$here/.." -o "$out/cxx11" -x c++ - <<'CXX11'
#include "flight_sim.h"
#include "crtp_log.h"
int main() {
  flightsim::FlightSim sim;
  uint8_t buf[FRAME_SIZE];
  for (uint32_t t = 0; t < 10000; t += 10) sim.nextFrame(t, buf);
  crtplog::Client client;
  uint8_t dg[crtplog::MAX_DATAGRAM];
  bool asked = client.poll(0, dg, sizeof dg) > 0;
  return sim.framesSent() > 0 && asked ? 0 : 1;
}
CXX11
"$out/cxx11" && echo "C++11 link and run: ok"

g++ -std=c++17 -O0 -g --coverage -Wall -Wextra -Werror \
  -fno-inline -o "$out/tests" "$here/test_flight_sim.cpp"
g++ -std=c++17 -O0 -g --coverage -Wall -Wextra -Werror \
  -fno-inline -o "$out/crtp" "$here/test_crtp_log.cpp"

(cd "$out" && ./tests && ./crtp)

echo
echo "coverage (headers compiled into the firmware):"
cd "$out"
# GCC names coverage data after the output binary: tests-test_flight_sim.gcda.
# Each header is measured from the suite that owns it.
status=0
for pair in tests-test_flight_sim:flight_sim.h tests-test_flight_sim:telemetry_frames.h \
            crtp-test_crtp_log:crtp_log.h; do
  data="${pair%%:*}" f="${pair#*:}"
  report="$(gcov -b -c "$data.gcda" 2>/dev/null)"
  block="$(printf '%s\n' "$report" | grep -A3 "^File '.*/$f'" || true)"
  if [ -z "$block" ]; then
    echo "  $f: no coverage data"
    status=1
    continue
  fi
  lines="$(printf '%s\n' "$block" | sed -n 's/^Lines executed:\([0-9.]*\)% of \([0-9]*\)/\1 \2/p')"
  taken="$(printf '%s\n' "$block" | sed -n 's/^Taken at least once:\([0-9.]*\)%.*/\1/p')"
  read -r pct total <<<"$lines"
  echo "  $f: ${pct}% of $total lines, ${taken:-n/a}% of branches taken"
  if awk -v p="$pct" 'BEGIN { exit !(p < 95) }'; then
    echo "  $f is under the 95% line bar; uncovered:"
    grep -nE '^[[:space:]]*#####:' "$f.gcov" | sed 's/^/    /' | head -40
    status=1
  fi
done
exit $status
