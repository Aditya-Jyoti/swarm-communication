package mavlink

import (
	"context"
	"errors"
	"math"
	"net"
	"time"

	"swarm-net/pkg/litewing"
	"swarm-net/pkg/protocol"
)

// The live vehicle: one swarm node whose instruments come from a real aircraft.
//
// A LiteWing quadcopter streams its telemetry to the handheld ground unit,
// which prints it over USB; cmd/litewing-relay forwards each sample here as a
// UDP datagram (docs/specs/2026-10-20-litewing-live-vehicle-design.md). For the
// configured node, everything the drone actually measures replaces the
// synthesised value: attitude, battery, range-sensor altitude, throttle, and
// the armed state. Position stays the swarm's -- the drone has no GPS -- so the
// node still flies missions, joins clusters and fails over like the others.
//
// Read only, in both directions that matter: the drone's numbers never move the
// swarm (SetPositions is unaffected), and nothing a ground station sends to
// this vehicle reaches the drone.
//
// # When the feed stops
//
// There is NO fallback to synthetic values. A ground station shown a healthy
// battery for an aircraft whose link just died is worse than one shown nothing.
// Instead, once the newest sample is older than StaleAfter, the vehicle keeps
// its last real values, withdraws the health bits for what only the drone could
// vouch for (liveSensors), and says "LiteWing link lost" once. The first sample
// back says "LiteWing link up". Before the first sample ever arrives the node
// is fully synthetic, so the stack can come up before the drone does.

const (
	// DefaultLiveBind is where the live feed is received.
	DefaultLiveBind = ":14560"
	// DefaultLiveStaleAfter is how old the newest sample may be before the
	// feed counts as lost. The drone streams at 10 Hz, so 1.5 s is fifteen
	// samples missed in a row: a dead link, not a dropped datagram.
	DefaultLiveStaleAfter = 1500 * time.Millisecond

	// liveDepth bounds samples queued from the reader to the loop. Ten per
	// second arrive, so this is most of a second of backlog.
	liveDepth = 8
)

// liveSensors are the health bits a stale feed withdraws: the drone's own
// IMU-based attitude and its battery monitor. GPS and the rest stay as the
// swarm reports them, because the swarm still knows where the node is.
const liveSensors = MavSysStatusSensor3dGyro |
	MavSysStatusSensor3dAccel |
	MavSysStatusSensorAttitudeStabilization |
	MavSysStatusSensorBattery

// LiveConfig drives one vehicle from a live telemetry feed. Off when Node is
// empty.
type LiveConfig struct {
	// Node is the swarm node the feed belongs to. MAV_LIVE_NODE.
	Node protocol.NodeID
	// Bind is the feed's UDP address. MAV_LIVE_BIND, default DefaultLiveBind.
	Bind string
	// StaleAfter is the feed's freshness limit. Default DefaultLiveStaleAfter.
	StaleAfter time.Duration
}

func (c LiveConfig) withDefaults() LiveConfig {
	if c.Node == "" {
		return c
	}
	if c.Bind == "" {
		c.Bind = DefaultLiveBind
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = DefaultLiveStaleAfter
	}
	return c
}

// liveState is the feed as the loop sees it. Loop-owned.
type liveState struct {
	// have is set by the first sample and never cleared: from then on this
	// vehicle's instruments are the drone's, fresh or not.
	have bool
	// up is whether "link up" has been announced and "link lost" not yet.
	up bool
	s  litewing.Sample
	at time.Time
	// altM is the last VALID range reading. An out-of-range reading (nothing
	// below the sensor) keeps it rather than reporting the drone at 0 m.
	altM float64
	// Rates, differentiated across samples: rad/s for attitude, m/s for climb.
	rollRate, pitchRate, yawRate, climb float64
}

// listenLive opens the feed socket, or returns nil when the feed is off.
func listenLive(c LiveConfig) (*net.UDPConn, error) {
	if c.Node == "" {
		return nil, nil
	}
	addr, err := net.ResolveUDPAddr("udp", c.Bind)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", addr)
}

// LiveAddr is the bound feed address, or nil when the feed is off.
func (b *Bridge) LiveAddr() net.Addr {
	if b.liveConn == nil {
		return nil
	}
	return b.liveConn.LocalAddr()
}

// liveLoop owns the feed socket's read side. Unlike readLoop it parses: a
// sample is self-contained and stateless to validate, and validating here
// keeps garbage out of the loop's queue.
func (b *Bridge) liveLoop(ctx context.Context) {
	buf := make([]byte, litewing.MaxLine+1)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := b.liveConn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return // socket closed
		}
		n, _, err := b.liveConn.ReadFromUDP(buf)
		if n > 0 {
			s, perr := litewing.Parse(buf[:n])
			if perr != nil {
				b.liveRejected.Add(1)
			} else {
				b.liveAccepted.Add(1)
				select {
				case b.liveIn <- s:
				case <-ctx.Done():
					return
				default:
					b.dropped.Add(1) // the loop is behind; the next sample supersedes it
				}
			}
		}
		// A closed socket or a cancelled context ends the reader. Anything else
		// -- the read deadline, or an ICMP error surfacing on this unconnected
		// socket -- is just another reason to read again.
		if err != nil && (errors.Is(err, net.ErrClosed) || ctx.Err() != nil) {
			return
		}
	}
}

// onLive takes one sample into the loop's state.
func (b *Bridge) onLive(s litewing.Sample) {
	if b.cfg.Live.Node == "" {
		return
	}
	now := b.cfg.Now()
	l := &b.live
	if l.have {
		if dt := now.Sub(l.at).Seconds(); dt > 0 {
			const k = math.Pi / 180
			l.rollRate = (s.Roll - l.s.Roll) * k / dt
			l.pitchRate = (s.Pitch - l.s.Pitch) * k / dt
			// The shortest turn: 179 -> -179 degrees is 2 degrees, not 358.
			l.yawRate = wrapPi((s.Yaw-l.s.Yaw)*k) / dt
			l.climb = 0
			if s.AltValid && l.s.AltValid {
				l.climb = (s.Alt - l.s.Alt) / dt
			}
		}
	}
	if s.AltValid {
		l.altM = s.Alt
	}
	l.s, l.at, l.have = s, now, true
	if !l.up && b.announceLive("LiteWing link up", SeverityInfo) {
		l.up = true
	}
}

// checkLive notices a feed that has gone quiet. Called once per status tick,
// so "link lost" is said at most once a second -- and, through l.up, once per
// loss.
func (b *Bridge) checkLive(now time.Time) {
	l := &b.live
	if !l.have || !l.up || b.liveFresh(now) {
		return
	}
	if b.announceLive("LiteWing link lost", SeverityCritical) {
		l.up = false
	}
}

func (b *Bridge) liveFresh(now time.Time) bool {
	return now.Sub(b.live.at) <= b.cfg.Live.StaleAfter
}

// announceLive sends a STATUSTEXT from the live vehicle. False when there is no
// connected vehicle to speak for yet, so the caller retries on the next sample.
func (b *Bridge) announceLive(text string, sev uint8) bool {
	v := b.vs[b.cfg.Live.Node]
	if v == nil || !v.connected {
		return false
	}
	m := &Statustext{Severity: sev}
	SetChars(m.Text[:], text)
	b.send(v.sysID, m)
	return true
}

// liveFor is the feed state for v, or nil when v is not the live vehicle or no
// sample has arrived yet.
func (b *Bridge) liveFor(v *vehicle) *liveState {
	if b.cfg.Live.Node == "" || v.id != b.cfg.Live.Node || !b.live.have {
		return nil
	}
	return &b.live
}

// chargeStateOf maps a battery percentage onto MAV_BATTERY_CHARGE_STATE. The
// thresholds are the ordinary ones an operator expects: a warning with a lap
// of reserve left, critical when it is time to land now.
func chargeStateOf(pct float64) uint8 {
	switch {
	case pct >= 30:
		return MavBatteryChargeStateOk
	case pct >= 20:
		return MavBatteryChargeStateLow
	case pct >= 10:
		return MavBatteryChargeStateCritical
	case pct > 0:
		return MavBatteryChargeStateEmergency
	default:
		return MavBatteryChargeStateFailed
	}
}

// liveBatteryStatus is BATTERY_STATUS for a 1S pack measured by the drone.
// Current and consumption are "not provided" (-1): the LiteWing has no current
// sensor, and inventing one would put a made-up number next to real ones.
func liveBatteryStatus(l *liveState) *BatteryStatus {
	m := &BatteryStatus{
		BatteryFunction:  MavBatteryFunctionAll,
		Type:             MavBatteryTypeLipo,
		Temperature:      math.MaxInt16,
		CurrentBattery:   -1,
		CurrentConsumed:  -1,
		EnergyConsumed:   -1,
		BatteryRemaining: int8(l.s.Pct),
		ChargeState:      chargeStateOf(float64(l.s.Pct)),
	}
	for i := range m.Voltages {
		m.Voltages[i] = math.MaxUint16
	}
	m.Voltages[0] = liveVoltageMV(l)
	return m
}

func liveVoltageMV(l *liveState) uint16 {
	return saturateU16(uint64(math.Round(l.s.Vbat * 1000)))
}
