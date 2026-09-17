---
title: "Latency as a Statistic"
description: "Why one probe is not a measurement: EWMA smoothing, penalising failed probes, and NaN as 'no score'."
outline: deep
---

# Latency as a Statistic

Latency is not one number. Most pings are fast, and a few are very slow (GC pauses, busy
CPUs, retransmits). If you rank peers on a single probe, the ranking changes every second.
So we keep a smoothed value per peer, an **exponentially weighted moving average (EWMA)**:

$$v_{n} = \alpha s_{n} + (1 - \alpha) v_{n-1}$$

$s_n$ is the new sample and $v_{n-1}$ the old average. With $\alpha = 0.3$, the newest sample
counts 30% and older ones fade out quickly. It needs only one float per peer.

Analogy: a restaurant rating. One bad meal moves it a little. Ten bad meals in a row move it
a lot.

Two more rules matter as much as the average itself:

- **A failed probe must make the score worse, not stay silent.** Otherwise the sicker a peer
  gets, the fewer bad samples it produces, and the better it looks.
- **"No score" must never look like a good score.** A dead peer scored `0` would win every
  election, because lower is better.

## How swarm-net uses it

- `LatencyHealthStrategy` in `backend/pkg/health/latency.go` keeps one EWMA per peer
  (`DefaultAlpha = 0.3`). The first sample seeds the average.
- A failed probe records a penalty sample of `ProbeTimeout x FailurePenalty`
  (2 s x 2.0 = 4000 ms) in `recordFailure`. One miss pushes the peer behind every healthy node.
- A cancelled probe (our own shutdown) records nothing. It says nothing about the peer.
- "No score" is `NaN` (`ScoreUnavailable` in `backend/pkg/health/strategy.go`).
  `IsValidScore` rejects it, and `Better` never lets it win.
- Samples come from `MeshProber` (`backend/pkg/network/prober.go`), a PING/PONG over the mesh
  connection. That measures the peer's application, not just its kernel.

```go
v := 0.8 // ms, current EWMA
for _, s := range []float64{0.9, 4000, 4000} { // one good, two failed probes
    v = 0.3*s + 0.7*v
}
// v is about 0.83, then 1200.6, then 2040.4
```

## Common pitfalls

- **"Cleaning up" NaN to 0.** The dead peer now has the best possible score and becomes leader.
- **Alpha too high.** Scores jump with every sample and leadership flaps. **Too low:** a dying
  leader takes a long time to look bad.
- **Showing the EWMA as a percentile.** It is a smoothed average, not a p50 or p99.
- **Comparing very young scores.** A peer with one sample is not comparable to one with fifty.

## Further reading

- [Interface Polymorphism](/concepts/interface-polymorphism) -- the `HealthStrategy` contract.
- [Idempotence and Hysteresis](/concepts/idempotence-and-hysteresis) -- the other half of the
  anti-flapping story.
- Gil Tene, [How NOT to Measure Latency](https://www.youtube.com/watch?v=lJ8ydIuPFeU)
