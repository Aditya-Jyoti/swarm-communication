package litewing

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// valid is a line exactly as esp/esp.ino's printTlmLine writes it, captured
// from the real drone on 2026-10-20.
const valid = `TLM {"src":"litewing","t":2385102,"roll":2.35,"pitch":1.92,"yaw":-69.82,` +
	`"thrust":0,"alt":0.032,"vbat":4.168,"pct":90,"armed":0,"rssi":-33,"m":[0,0,0,0]}`

func TestParseValidLine(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Sample{
		DroneMs: 2385102, Roll: 2.35, Pitch: 1.92, Yaw: -69.82, Thrust: 0,
		Alt: 0.032, AltValid: true, Vbat: 4.168, Pct: 90, Armed: false, RSSI: -33,
	}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestParseWithoutPrefix(t *testing.T) {
	// The relay strips "TLM " before forwarding, so the bridge parses the bare
	// object. Both forms are the same sample.
	bare := strings.TrimPrefix(valid, Prefix)
	a, err := Parse([]byte(bare))
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	b, _ := Parse([]byte(valid))
	if a != b {
		t.Fatalf("bare %+v != prefixed %+v", a, b)
	}
}

func TestParseTrailingNewlineAndCR(t *testing.T) {
	// Serial lines end "\r\n" from Serial.println.
	if _, err := Parse([]byte(valid + "\r\n")); err != nil {
		t.Fatalf("CRLF: %v", err)
	}
}

func TestParseArmedMotorsAndNullAlt(t *testing.T) {
	line := `{"src":"litewing","t":1,"roll":-10,"pitch":5,"yaw":180,"thrust":30000,` +
		`"alt":null,"vbat":3.7,"pct":50,"armed":1,"rssi":-60,"m":[1,2,65535,4]}`
	s, err := Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !s.Armed || s.AltValid || s.Alt != 0 || s.Motors != [4]uint16{1, 2, 65535, 4} || s.Thrust != 30000 {
		t.Fatalf("got %+v", s)
	}
}

func TestParseBoundsAreInclusive(t *testing.T) {
	line := `{"src":"litewing","t":16777215,"roll":180,"pitch":-180,"yaw":-360,"thrust":65535,` +
		`"alt":8,"vbat":6,"pct":100,"armed":1,"rssi":-127,"m":[65535,0,0,0]}`
	if _, err := Parse([]byte(line)); err != nil {
		t.Fatalf("Parse at the bounds: %v", err)
	}
	line = `{"src":"litewing","t":0,"roll":-180,"pitch":180,"yaw":360,"thrust":0,` +
		`"alt":0,"vbat":0,"pct":0,"armed":0,"rssi":0,"m":[0,0,0,0]}`
	if _, err := Parse([]byte(line)); err != nil {
		t.Fatalf("Parse at the other bounds: %v", err)
	}
}

// mutate swaps one field's value in the valid line.
func mutate(field, value string) string {
	obj := strings.TrimPrefix(valid, Prefix)
	key := `"` + field + `":`
	i := strings.Index(obj, key)
	if i < 0 {
		panic("no field " + field)
	}
	j := i + len(key)
	end := j
	depth := 0
	for ; end < len(obj); end++ {
		c := obj[end]
		if c == '[' {
			depth++
		}
		if c == ']' {
			depth--
		}
		if depth == 0 && (c == ',' || c == '}') {
			break
		}
	}
	return obj[:j] + value + obj[end:]
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, line string
	}{
		{"empty", ""},
		{"prefix only", "TLM "},
		{"not json", "TLM hello"},
		{"other log line", "[LW] setup -> streaming"},
		{"wrong src", mutate("src", `"esp32-sim"`)},
		{"roll high", mutate("roll", "180.01")},
		{"roll low", mutate("roll", "-181")},
		{"pitch high", mutate("pitch", "181")},
		{"yaw high", mutate("yaw", "360.5")},
		{"thrust negative", mutate("thrust", "-1")},
		{"thrust high", mutate("thrust", "65536")},
		{"alt negative", mutate("alt", "-0.001")},
		{"alt over sensor range", mutate("alt", "8.001")},
		{"vbat negative", mutate("vbat", "-0.1")},
		{"vbat absurd", mutate("vbat", "7")},
		{"pct high", mutate("pct", "101")},
		{"pct negative", mutate("pct", "-1")},
		{"pct fractional", mutate("pct", "50.5")},
		{"armed 2", mutate("armed", "2")},
		{"armed bool", mutate("armed", "true")},
		{"rssi positive", mutate("rssi", "1")},
		{"rssi low", mutate("rssi", "-128")},
		{"t negative", mutate("t", "-1")},
		{"t over 24 bits", mutate("t", "16777216")},
		{"motors short", mutate("m", "[0,0,0]")},
		{"motors long", mutate("m", "[0,0,0,0,0]")},
		{"motor high", mutate("m", "[0,70000,0,0]")},
		{"motor negative", mutate("m", "[0,-1,0,0]")},
		{"motors null", mutate("m", "null")},
		{"nan string", mutate("roll", `"nan"`)},
		{"nan bare", mutate("roll", "nan")},
		{"overflow", mutate("vbat", "1e400")},
		{"string number", mutate("vbat", `"4.1"`)},
		{"src null", mutate("src", "null")},
		{"roll null", mutate("roll", "null")},
		{"trailing garbage", strings.TrimPrefix(valid, Prefix) + "x"},
		{"two objects", strings.TrimPrefix(valid, Prefix) + strings.TrimPrefix(valid, Prefix)},
		{"unknown field", strings.Replace(valid, `"src"`, `"extra":1,"src"`, 1)},
		{"array", "[1,2,3]"},
		{"too long", valid + strings.Repeat(" ", MaxLine)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if s, err := Parse([]byte(c.line)); err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", c.line, s)
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

func TestParseRejectsEachMissingField(t *testing.T) {
	for _, f := range []string{"src", "t", "roll", "pitch", "yaw", "thrust", "alt", "vbat", "pct", "armed", "rssi", "m"} {
		t.Run(f, func(t *testing.T) {
			// Drop the field and its value, keeping the JSON well formed.
			obj := strings.TrimPrefix(mutate(f, "0"), Prefix)
			key := `"` + f + `":0`
			obj = strings.Replace(obj, key+",", "", 1)
			obj = strings.Replace(obj, ","+key, "", 1)
			if strings.Contains(obj, `"`+f+`"`) {
				t.Fatalf("test bug: %s still present in %s", f, obj)
			}
			if _, err := Parse([]byte(obj)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("missing %s: err = %v, want ErrInvalid", f, err)
			}
		})
	}
}

func TestIsLine(t *testing.T) {
	if !IsLine([]byte(valid)) || IsLine([]byte("[LW] joined")) || IsLine([]byte("TL")) {
		t.Fatal("IsLine misclassified")
	}
}

func TestSampleRadiansAndThrottle(t *testing.T) {
	s := Sample{Roll: 90, Pitch: -45, Yaw: 270, Thrust: 65535.0 / 2}
	r, p, y := s.AttitudeRad()
	if math.Abs(r-math.Pi/2) > 1e-12 || math.Abs(p+math.Pi/4) > 1e-12 {
		t.Fatalf("roll %v pitch %v", r, p)
	}
	// Yaw is folded into [-pi, pi], MAVLink's ATTITUDE range.
	if math.Abs(y+math.Pi/2) > 1e-12 {
		t.Fatalf("yaw %v, want -pi/2", y)
	}
	// The drone reports yaw down to -360; that folds to +90 here.
	if _, _, y := (Sample{Yaw: -270}).AttitudeRad(); math.Abs(y-math.Pi/2) > 1e-12 {
		t.Fatalf("yaw -270 -> %v, want pi/2", y)
	}
	if got := s.ThrottlePercent(); got != 50 {
		t.Fatalf("throttle %v, want 50", got)
	}
	if got := (Sample{Thrust: 65535}).ThrottlePercent(); got != 100 {
		t.Fatalf("full throttle %v", got)
	}
	if got := (Sample{Yaw: -180}); true {
		_, _, y := got.AttitudeRad()
		if math.Abs(math.Abs(y)-math.Pi) > 1e-12 {
			t.Fatalf("yaw -180 -> %v", y)
		}
	}
}
