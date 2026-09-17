---
title: "Monotonic vs Wall Clocks: Measuring Duration Without Lying"
description: "The wall clock tells you what time it is and can jump. The monotonic clock tells you how much time has passed and never goes back. Use the right one."
outline: deep
---

# Monotonic vs Wall Clocks: Measuring Duration Without Lying

A computer has two clocks. The **wall clock** (`CLOCK_REALTIME`) answers "what time is it?".
NTP or an admin can move it forward or backward at any moment. The **monotonic clock**
(`CLOCK_MONOTONIC`) answers "how much time has passed?". It only moves forward, but its
starting point is arbitrary (roughly boot time), so its raw value means nothing on its own.

Analogy: the wall clock is the kitchen clock that anyone can reset. The monotonic clock is a
stopwatch in your pocket. Time things with the stopwatch. Never compare your stopwatch with
someone else's.

In Go, `time.Now()` returns a `time.Time` that holds **both** readings. `time.Since(start)` and
`t2.Sub(t1)` use the monotonic part when both values have one.

## How swarm-net uses it

- RTT is always two clock reads on the **same** machine, around a PING and its matching PONG
  (`MeshProber.Probe` in `backend/pkg/network/prober.go`).
- `Envelope.SentAtUnixNano` (`backend/pkg/protocol/message.go`) is a wall-clock value for logs
  and the dashboard only. It is never subtracted across nodes. The containers have no clock
  sync.
- `EvaluateScore` in `backend/pkg/health/latency.go` refuses a negative duration instead of
  feeding it into the average.
- Tickers, timers and context deadlines all run on the monotonic clock, so the heartbeat
  detector is safe from clock jumps.

```go
start := time.Now()          // wall + monotonic
doWork()
elapsed := time.Since(start) // uses the monotonic part, never negative

// Wrong: Round(0) strips the monotonic reading,
// so this subtraction uses the wall clock and can go negative.
bad := time.Now().Sub(start.Round(0))
```

## Common pitfalls

- **Subtracting timestamps from two nodes.** Their clocks can differ by seconds, so the result
  can be negative. A latency-based election fed that number would pick the node with the most
  wrong clock.
- **Losing the monotonic reading.** `Round(0)`, `UTC()`, `In()`, and a JSON round trip all drop
  it. The subtraction then silently uses the wall clock.
- **Trusting log order.** Log timestamps are wall-clock. After an NTP step they can go
  backwards. Order events with counters instead (see
  [Logical Clocks](/concepts/logical-clocks)).

## Further reading

- [Heartbeat Intervals, Jitter and Timers](/concepts/heartbeat-intervals-and-timers)
- [Latency as a Statistic](/concepts/latency-as-a-statistic)
- Go docs: [Monotonic Clocks](https://pkg.go.dev/time#hdr-Monotonic_Clocks)
