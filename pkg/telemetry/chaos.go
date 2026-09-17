package telemetry

import (
	"errors"
	"fmt"
	"time"

	"swarm-net/pkg/protocol"
)

// ChaosAction names a fault. It mirrors ChaosPayload.Action.
type ChaosAction string

// The chaos actions the contract defines.
const (
	// ChaosKill makes the node exit so Compose restarts it as a new incarnation.
	ChaosKill ChaosAction = "kill"
	// ChaosDelay delays the node's PONG and HEARTBEAT_ACK replies.
	ChaosDelay ChaosAction = "delay"
	// ChaosClear removes any injected delay.
	ChaosClear ChaosAction = "clear"
)

// MaxChaosDelay is the largest delay a node accepts. Beyond it a delay stops
// being "slow" and becomes "dead" (probe and idle timeouts are a few seconds),
// which is what kill is for.
const MaxChaosDelay = 5 * time.Second

// ErrInvalidChaos is wrapped by every ParseChaos rejection.
var ErrInvalidChaos = errors.New("telemetry: invalid chaos instruction")

// Chaos is a validated fault instruction.
type Chaos struct {
	Action ChaosAction
	// Delay is the delay to install. Always 0 for kill and clear, so a handler
	// can apply it unconditionally for both delay and clear.
	Delay time.Duration
}

// ParseChaos validates a CHAOS payload. It is exported so the Control Center
// rejects a bad request at the HTTP edge with the same rule the node applies:
// ChaosPayload leaves validation to the receiver, and there should be one
// receiver-side rule, not two that drift.
func ParseChaos(p protocol.ChaosPayload) (Chaos, error) {
	switch a := ChaosAction(p.Action); a {
	case ChaosKill, ChaosClear:
		return Chaos{Action: a}, nil
	case ChaosDelay:
		d := time.Duration(p.DelayMS) * time.Millisecond
		if p.DelayMS < 0 || d > MaxChaosDelay {
			return Chaos{}, fmt.Errorf("%w: delay_ms %d outside 0..%d", ErrInvalidChaos, p.DelayMS, MaxChaosDelay.Milliseconds())
		}
		return Chaos{Action: a, Delay: d}, nil
	default:
		return Chaos{}, fmt.Errorf("%w: unknown action %q", ErrInvalidChaos, p.Action)
	}
}
