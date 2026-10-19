package mavlink

import (
	"fmt"
	"math"
	"testing"

	"swarm-net/pkg/protocol"
)

// lab is the projection every test below uses: the contract defaults.
var lab = DefaultProjection()

// haversine returns the great-circle distance in metres between two WGS-84
// points, and the initial bearing in degrees clockwise from north.
//
// This is deliberately a SECOND, independent formulation of the geometry: the
// projection is a linear flat-earth map, and checking it with the spherical
// formula is what makes the "20 metres north" assertions below mean something.
// Checking it against itself would only prove the code is self-consistent.
func haversine(lat1, lon1, lat2, lon2 float64) (metres, bearingDeg float64) {
	const rad = math.Pi / 180
	p1, p2 := lat1*rad, lat2*rad
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	metres = 2 * EarthRadiusM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	y := math.Sin(dLon) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dLon)
	bearingDeg = math.Mod(math.Atan2(y, x)/rad+360, 360)
	return metres, bearingDeg
}

func TestProjectOrigin(t *testing.T) {
	// The cube's (0,0,0) corner must land exactly on the configured origin, to
	// the integer. Anything else means a sign or scaling error that would be
	// invisible in a round-trip test.
	got := lab.Project(protocol.Position{})
	want := Global{LatE7: 473979710, LonE7: 85461640, AltMM: 488000}
	if got != want {
		t.Errorf("Project(0,0,0) = %+v, want %+v", got, want)
	}
}

// TestProjectDistanceAndBearing is the hand-checked case.
//
// At k = 20 m per unit, one unit north is 20 m north. The expected latitude
// delta is 20 / 6378137 radians = 1.796631e-4 degrees, i.e. 1797 in the 1e7
// integer units MAVLink uses -- which is the arithmetic that the "east" cases
// then exercise through the cos(lat0) term.
func TestProjectDistanceAndBearing(t *testing.T) {
	tests := []struct {
		name        string
		pos         protocol.Position
		wantMetres  float64
		wantBearing float64
		// bearingTol allows for meridian convergence: the great-circle initial
		// bearing of a north-east leg is not quite 45 degrees once the leg is long
		// enough for the meridians to have converged measurably. At 2.8 km that is
		// 0.015 degrees, which is real geometry rather than projection error.
		bearingTol float64
	}{
		{"one unit north", protocol.Position{Y: 1}, 20, 0, 0.01},
		{"one unit east", protocol.Position{X: 1}, 20, 90, 0.01},
		{"one unit south", protocol.Position{Y: -1}, 20, 180, 0.01},
		{"one unit west", protocol.Position{X: -1}, 20, 270, 0.01},
		{"3-4-5 triangle", protocol.Position{X: 3, Y: 4}, 100, 36.8699, 0.01},
		{"diagonal is 45 degrees", protocol.Position{X: 10, Y: 10}, 282.8427, 45, 0.01},
		{"far corner of the cube", protocol.Position{X: 100, Y: 100}, 2828.427, 45, 0.02},
	}

	lat0, lon0, _ := lab.ProjectFloat(protocol.Position{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lat, lon, _ := lab.ProjectFloat(tc.pos)
			metres, bearing := haversine(lat0, lon0, lat, lon)

			// 1 cm on a 20 m baseline, 1 mm per metre on the longer ones: the
			// flat-earth approximation's error, not float noise.
			tol := math.Max(0.01, tc.wantMetres/1000)
			if math.Abs(metres-tc.wantMetres) > tol {
				t.Errorf("distance = %.4f m, want %.4f +/- %.4f", metres, tc.wantMetres, tol)
			}
			if math.Abs(bearing-tc.wantBearing) > tc.bearingTol {
				t.Errorf("bearing = %.4f deg, want %.4f +/- %.4f", bearing, tc.wantBearing, tc.bearingTol)
			}
		})
	}
}

func TestProjectNorthDeltaIsExact(t *testing.T) {
	// The integer-unit form of the same hand calculation: 1797 e7-degrees per
	// cube unit north. Pinning it catches a wrong earth radius or a degree/radian
	// slip, neither of which changes the shape of anything the round-trip tests
	// look at.
	base := lab.Project(protocol.Position{})
	north := lab.Project(protocol.Position{Y: 1})
	if got, want := north.LatE7-base.LatE7, int32(1797); got != want {
		t.Errorf("one unit north = %d e7-degrees, want %d", got, want)
	}
	if north.LonE7 != base.LonE7 {
		t.Errorf("moving north changed longitude by %d", north.LonE7-base.LonE7)
	}

	// Altitude is plain metres: one unit up is k millimetres times 1000.
	up := lab.Project(protocol.Position{Z: 1})
	if got, want := up.AltMM-base.AltMM, int32(20000); got != want {
		t.Errorf("one unit up = %d mm, want %d", got, want)
	}
}

// TestLongitudeScalesWithLatitude pins the cos(lat0) term, the one piece of
// spherical geometry the model keeps. Without it the cube would be stretched
// east-west by 1/cos(lat0) -- about 1.5x at the default origin -- and every
// distance the latency model derives from a position would be wrong with it.
func TestLongitudeScalesWithLatitude(t *testing.T) {
	equator := Projection{OriginLat: 0, MetresPerUnit: 20}
	sixty := Projection{OriginLat: 60, MetresPerUnit: 20}

	_, lonEq, _ := equator.ProjectFloat(protocol.Position{X: 1})
	_, lon60, _ := sixty.ProjectFloat(protocol.Position{X: 1})

	// cos(60 deg) = 0.5 exactly, so a degree of longitude is half as wide and one
	// cube unit east must span twice as many degrees.
	if ratio := lon60 / lonEq; math.Abs(ratio-2) > 1e-9 {
		t.Errorf("longitude delta at 60N / at equator = %.9f, want 2", ratio)
	}
}

func TestProjectUnprojectRoundTrip(t *testing.T) {
	positions := []protocol.Position{
		{},
		{X: 1, Y: 1, Z: 1},
		{X: 50, Y: 50, Z: 10},
		{X: 100, Y: 100, Z: 100},
		{X: 0.125, Y: 99.875, Z: 0.5},
		{X: -10, Y: -10, Z: -2}, // outside the cube: the map is still linear
		{X: 37.3, Y: 62.9, Z: 4.75},
	}

	for _, pos := range positions {
		t.Run(posName(pos), func(t *testing.T) {
			// The float path is an exact linear inverse, so the only error is
			// float64 rounding: nanometres, checked at a micrometre.
			lat, lon, altM := lab.ProjectFloat(pos)
			back := lab.UnprojectFloat(lat, lon, altM)
			const exact = 1e-6 / DefaultMetresPerUnit // 1 micrometre, in cube units
			if math.Abs(back.X-pos.X) > exact || math.Abs(back.Y-pos.Y) > exact || math.Abs(back.Z-pos.Z) > exact {
				t.Errorf("float round trip: %+v -> %+v", pos, back)
			}

			// The integer path cannot be exact, and the bound is arithmetic rather
			// than empirical: a latitude quantised to 1e-7 degrees loses up to
			// half a unit, which is 1.1 cm / 2 = 5.6 mm, and an altitude in whole
			// millimetres loses up to 0.5 mm. Allow 1.2 cm horizontally and 1 mm
			// vertically -- in cube units, where 1 unit is 20 m.
			g := lab.Project(pos)
			qback := lab.Unproject(g)
			const horizTol = 0.012 / DefaultMetresPerUnit
			const vertTol = 0.001 / DefaultMetresPerUnit
			if math.Abs(qback.X-pos.X) > horizTol || math.Abs(qback.Y-pos.Y) > horizTol {
				t.Errorf("integer round trip horizontal: %+v -> %+v (via %+v)", pos, qback, g)
			}
			if math.Abs(qback.Z-pos.Z) > vertTol {
				t.Errorf("integer round trip altitude: %+v -> %+v (via %+v)", pos, qback, g)
			}
		})
	}
}

func posName(p protocol.Position) string {
	return fmt.Sprintf("x%g_y%g_z%g", p.X, p.Y, p.Z)
}

// TestUnprojectQGCWaypoint is the direction the mission code needs: a waypoint
// as a ground station sends it (MISSION_ITEM_INT, degrees * 1e7, altitude in
// metres) has to become a cube position the Control Center can store.
func TestUnprojectQGCWaypoint(t *testing.T) {
	// 25 units east, 75 north, 10 up, as it would arrive back from a ground
	// station that received it from us.
	want := protocol.Position{X: 25, Y: 75, Z: 10}
	g := lab.Project(want)

	got := lab.UnprojectFloat(float64(g.LatE7)/1e7, float64(g.LonE7)/1e7, float64(g.AltMM)/1000)
	if math.Abs(got.X-want.X) > 0.001 || math.Abs(got.Y-want.Y) > 0.001 || math.Abs(got.Z-want.Z) > 0.001 {
		t.Errorf("unprojected waypoint = %+v, want %+v", got, want)
	}
}

func TestProjectionZeroValueIsHarmless(t *testing.T) {
	// A Projection built from an unset config must not produce NaN: a NaN
	// position propagates into the latency model and the dashboard, and the
	// symptom (no vehicles anywhere) points nowhere near the cause.
	var zero Projection
	g := zero.Project(protocol.Position{X: 10, Y: 10, Z: 10})
	if g != (Global{}) {
		t.Errorf("zero projection produced %+v, want all zeroes", g)
	}
	back := zero.Unproject(Global{LatE7: 473979710})
	if math.IsNaN(back.X) || math.IsNaN(back.Y) || math.IsNaN(back.Z) {
		t.Errorf("zero projection unprojected to %+v, want no NaN", back)
	}
}

func TestToInt32Saturates(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want int32
	}{
		{"zero", 0, 0},
		{"truncates toward zero", 1.5, 1}, // callers round before calling
		{"negative", -17, -17},
		{"max", math.MaxInt32, math.MaxInt32},
		{"over max saturates", 1e18, math.MaxInt32},
		{"under min saturates", -1e18, math.MinInt32},
		{"nan is zero", math.NaN(), 0},
		{"inf saturates", math.Inf(1), math.MaxInt32},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt32(tc.in); got != tc.want {
				t.Errorf("toInt32(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestProjectAbsurdInputDoesNotWrap: a misconfigured origin must pin a vehicle
// at the edge of the world rather than flip it to the far hemisphere, which is
// what a plain int32 conversion of an out-of-range float does.
func TestProjectAbsurdInputDoesNotWrap(t *testing.T) {
	p := Projection{OriginLat: 47, OriginLon: 8, MetresPerUnit: 1e12}
	g := p.Project(protocol.Position{X: 1e6, Y: 1e6, Z: 1e6})
	if g.LatE7 != math.MaxInt32 || g.LonE7 != math.MaxInt32 || g.AltMM != math.MaxInt32 {
		t.Errorf("got %+v, want saturation at MaxInt32", g)
	}
}
