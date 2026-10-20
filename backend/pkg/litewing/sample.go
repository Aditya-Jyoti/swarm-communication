// Package litewing is the one definition of a valid LiteWing telemetry sample:
// the "TLM {json}" line the ground unit (esp/esp.ino, printTlmLine) prints for
// every sample it reads from the drone.
//
// Both ends of the host side use it. cmd/litewing-relay validates each serial
// line before forwarding it, and the MAVLink bridge validates each datagram
// before letting it touch a vehicle, so neither trusts the other and a garbled
// serial line can never become a number on a ground station.
//
// Validation is strict on purpose: every field present, nothing extra, every
// value inside the physical range of this airframe. A field added on the board
// has to be added here too, in the same change.
package litewing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Prefix starts every telemetry line on the ground unit's serial port. Lines
// without it are the board's own log output.
const Prefix = "TLM "

// MaxLine bounds a line. A real one is about 170 bytes; anything near this is
// two lines run together by a dropped newline, or noise.
const MaxLine = 512

// ErrInvalid wraps every rejection, so a caller can count rejections without
// matching messages.
var ErrInvalid = errors.New("litewing: invalid sample")

// Sample is one telemetry sample from the drone.
type Sample struct {
	// DroneMs is the drone's own 24-bit millisecond timestamp for the sample.
	DroneMs uint32
	// Roll, Pitch and Yaw are the drone's stabilizer estimate, in degrees.
	Roll, Pitch, Yaw float64
	// Thrust is the drone's thrust command, 0..65535.
	Thrust float64
	// Alt is the downward range sensor's distance, in metres. Meaningless when
	// AltValid is false (nothing in range).
	Alt      float64
	AltValid bool
	// Vbat is the battery voltage. The LiteWing flies a 1S pack.
	Vbat float64
	// Pct is the drone's own battery percentage estimate.
	Pct int
	// Armed is the ground unit's derivation: thrust or any motor above zero.
	Armed bool
	// RSSI is the ground unit's Wi-Fi signal from the drone's AP, in dBm.
	RSSI int
	// Motors is each motor's output, 0..65535.
	Motors [4]uint16
}

// IsLine reports whether a serial line is a telemetry line at all.
func IsLine(line []byte) bool { return bytes.HasPrefix(line, []byte(Prefix)) }

// wire is the JSON shape. Pointers, so a missing field is distinguishable from
// a zero one: a sample that silently lacked "vbat" would read as a flat battery.
type wire struct {
	Src    *string   `json:"src"`
	T      *float64  `json:"t"`
	Roll   *float64  `json:"roll"`
	Pitch  *float64  `json:"pitch"`
	Yaw    *float64  `json:"yaw"`
	Thrust *float64  `json:"thrust"`
	Alt    *float64  `json:"alt"` // null is legal: out of range
	Vbat   *float64  `json:"vbat"`
	Pct    *float64  `json:"pct"`
	Armed  *float64  `json:"armed"`
	RSSI   *float64  `json:"rssi"`
	M      []float64 `json:"m"`
}

// Parse validates one sample, with or without the "TLM " prefix and trailing
// line ending.
func Parse(line []byte) (Sample, error) {
	if len(line) > MaxLine {
		return Sample{}, fmt.Errorf("%w: line of %d bytes", ErrInvalid, len(line))
	}
	line = bytes.TrimPrefix(line, []byte(Prefix))
	line = bytes.TrimRight(line, "\r\n")

	// "alt" is the one field that may be null, and a null pointer is also what
	// a MISSING field looks like, so its presence is checked separately. This
	// pass also rejects trailing data: Unmarshal accepts exactly one value.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(line, &keys); err != nil {
		return Sample{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, ok := keys["alt"]; !ok {
		return Sample{}, fmt.Errorf("%w: missing alt", ErrInvalid)
	}

	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return Sample{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return w.sample()
}

type field struct {
	name     string
	v        *float64
	min, max float64
	integer  bool
}

func (w *wire) sample() (Sample, error) {
	if w.Src == nil || *w.Src != "litewing" {
		return Sample{}, fmt.Errorf("%w: src is not litewing", ErrInvalid)
	}
	// Ranges are this airframe's physics, not the field types': attitude in
	// degrees as the stabilizer reports it, a 1S pack, the VL53L0X's 8 m.
	fields := []field{
		{"t", w.T, 0, 1<<24 - 1, true},
		{"roll", w.Roll, -180, 180, false},
		{"pitch", w.Pitch, -180, 180, false},
		{"yaw", w.Yaw, -360, 360, false},
		{"thrust", w.Thrust, 0, 65535, false},
		{"vbat", w.Vbat, 0, 6, false},
		{"pct", w.Pct, 0, 100, true},
		{"armed", w.Armed, 0, 1, true},
		{"rssi", w.RSSI, -127, 0, true},
	}
	if w.Alt != nil {
		fields = append(fields, field{"alt", w.Alt, 0, 8, false})
	}
	for _, f := range fields {
		if err := f.check(); err != nil {
			return Sample{}, err
		}
	}
	if len(w.M) != 4 {
		return Sample{}, fmt.Errorf("%w: m has %d motors, want 4", ErrInvalid, len(w.M))
	}
	var motors [4]uint16
	for i, m := range w.M {
		f := field{fmt.Sprintf("m[%d]", i), &m, 0, 65535, true}
		if err := f.check(); err != nil {
			return Sample{}, err
		}
		motors[i] = uint16(m)
	}
	s := Sample{
		DroneMs: uint32(*w.T),
		Roll:    *w.Roll,
		Pitch:   *w.Pitch,
		Yaw:     *w.Yaw,
		Thrust:  *w.Thrust,
		Vbat:    *w.Vbat,
		Pct:     int(*w.Pct),
		Armed:   *w.Armed == 1,
		RSSI:    int(*w.RSSI),
		Motors:  motors,
	}
	if w.Alt != nil {
		s.Alt, s.AltValid = *w.Alt, true
	}
	return s, nil
}

// check rejects a missing value, one outside [min, max], and a fractional one
// where an integer is meant. The negated comparison also rejects NaN, which
// encoding/json cannot produce but a future decoder might.
func (f field) check() error {
	if f.v == nil {
		return fmt.Errorf("%w: missing %s", ErrInvalid, f.name)
	}
	v := *f.v
	if !(v >= f.min && v <= f.max) {
		return fmt.Errorf("%w: %s %v outside [%v, %v]", ErrInvalid, f.name, v, f.min, f.max)
	}
	if f.integer && v != math.Trunc(v) {
		return fmt.Errorf("%w: %s %v is not an integer", ErrInvalid, f.name, v)
	}
	return nil
}

// AttitudeRad is roll, pitch and yaw in radians, with yaw folded into
// [-pi, pi]: the units and range of MAVLink's ATTITUDE message.
func (s Sample) AttitudeRad() (roll, pitch, yaw float64) {
	const k = math.Pi / 180
	y := math.Mod(s.Yaw+180, 360)
	if y < 0 {
		y += 360
	}
	return s.Roll * k, s.Pitch * k, (y - 180) * k
}

// ThrottlePercent is thrust as a percentage of full scale.
func (s Sample) ThrottlePercent() float64 { return s.Thrust / 65535 * 100 }
