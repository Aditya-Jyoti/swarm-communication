package mavlink

import (
	"math"

	"swarm-net/pkg/protocol"
)

// EarthRadiusM is the WGS-84 semi-major axis, the radius MAVLink and every
// ground station assume.
const EarthRadiusM = 6378137.0

// Contract defaults, from docs/architecture/mavlink-bridge.md section 2. The
// origin is the standard ArduPilot SITL location, so a ground station opens on a
// sensible map before anyone configures anything.
const (
	DefaultOriginLat     = 47.397971
	DefaultOriginLon     = 8.546164
	DefaultOriginAltM    = 488.0
	DefaultMetresPerUnit = 20.0
)

// Global is a WGS-84 position in the integer units MAVLink puts on the wire.
//
// Integers, not degrees, because that is what GLOBAL_POSITION_INT and
// MISSION_ITEM_INT carry. Keeping the conversion in one type means the 1e7
// scaling cannot be applied twice or forgotten, which is the classic way to put
// a vehicle in the Gulf of Guinea.
type Global struct {
	// LatE7 is latitude in degrees * 1e7. One unit is about 1.1 cm.
	LatE7 int32
	// LonE7 is longitude in degrees * 1e7.
	LonE7 int32
	// AltMM is altitude above mean sea level in millimetres.
	AltMM int32
}

// Projection maps the swarm's abstract cube onto the earth.
//
// # The model
//
// Equirectangular (flat-earth) about a fixed origin:
//
//	lat = lat0 + (y*k / R) * 180/pi
//	lon = lon0 + (x*k / (R*cos(lat0))) * 180/pi
//	alt = alt0 + z*k
//
// with k = MetresPerUnit. X is east, Y is north, Z is up, matching pkg/geo's
// cube and the Blender exporter's axes.
//
// # Why not a real geodesic projection
//
// The cube is 100 units across, so at the default k = 20 the world is 2 km
// wide. Over that span the flat-earth approximation is off by well under a
// metre -- the error grows with the square of the distance from the origin and
// only becomes interesting at tens of kilometres. The thing this projection
// feeds is a map display and a latency model whose own distance term is a tuning
// knob, so sub-metre error is already below the noise. A proper transverse
// Mercator would add a page of coefficients to be wrong in more interesting ways.
//
// The cos(lat0) term is NOT dropped, though, and that is the one piece of
// honesty the model needs: without it a degree of longitude would be treated as
// a degree of latitude, and at the default origin's 47 degrees north the cube
// would come out 1.5 times too wide. Using cos(lat0) rather than cos(lat) is
// what makes the projection a linear map with an exact inverse.
//
// # Zero value
//
// The zero Projection maps everything to lat/lon/alt 0 with k = 0; it is a
// valid, if useless, transform rather than a panic. Use DefaultProjection, or
// build one from configuration.
type Projection struct {
	// OriginLat and OriginLon are the cube's (0, 0) corner, in degrees.
	OriginLat float64
	OriginLon float64
	// OriginAltM is the altitude of z = 0, in metres above mean sea level.
	OriginAltM float64
	// MetresPerUnit is k: how big one cube unit is on the ground. It must match
	// world.metres_per_unit in the Blender scene, or the swarm and the ground
	// station will disagree about how far apart two nodes are.
	MetresPerUnit float64
}

// DefaultProjection returns the contract defaults.
func DefaultProjection() Projection {
	return Projection{
		OriginLat:     DefaultOriginLat,
		OriginLon:     DefaultOriginLon,
		OriginAltM:    DefaultOriginAltM,
		MetresPerUnit: DefaultMetresPerUnit,
	}
}

// degPerMetreLat is the latitude change, in degrees, per metre north.
func (p Projection) degPerMetreLat() float64 {
	return (180.0 / math.Pi) / EarthRadiusM
}

// degPerMetreLon is the longitude change, in degrees, per metre east at the
// origin's latitude.
//
// cos(lat0) is floored at a small positive value. That branch is unreachable for
// any plausible lab origin, but a projection that returns +Inf at the pole would
// propagate NaN into every position in the swarm, and the symptom (a map with no
// vehicles on it) points nowhere near the cause.
func (p Projection) degPerMetreLon() float64 {
	cos := math.Cos(p.OriginLat * math.Pi / 180.0)
	const minCos = 1e-9
	if cos < minCos {
		cos = minCos
	}
	return (180.0 / math.Pi) / (EarthRadiusM * cos)
}

// ProjectFloat converts cube units to degrees and metres without quantising.
//
// Exposed separately from Project because the rounding to 1e-7 degrees is lossy
// at the centimetre level, and anything doing further arithmetic -- a distance, a
// bearing, a round-trip check -- wants the unrounded value.
func (p Projection) ProjectFloat(pos protocol.Position) (lat, lon, altM float64) {
	lat = p.OriginLat + pos.Y*p.MetresPerUnit*p.degPerMetreLat()
	lon = p.OriginLon + pos.X*p.MetresPerUnit*p.degPerMetreLon()
	altM = p.OriginAltM + pos.Z*p.MetresPerUnit
	return lat, lon, altM
}

// Project converts a cube position to MAVLink integer units.
func (p Projection) Project(pos protocol.Position) Global {
	lat, lon, altM := p.ProjectFloat(pos)
	return Global{
		LatE7: toInt32(math.Round(lat * 1e7)),
		LonE7: toInt32(math.Round(lon * 1e7)),
		AltMM: toInt32(math.Round(altM * 1000)),
	}
}

// UnprojectFloat converts degrees and metres back to cube units. It is the exact
// inverse of ProjectFloat; the transform is linear, so there is no iteration
// here and no approximation beyond float64 rounding.
//
// This is the direction the mission code needs: a waypoint arrives from the
// ground station as lat/lon/alt and has to become a position the Control Center
// can store and the latency model can measure.
func (p Projection) UnprojectFloat(lat, lon, altM float64) protocol.Position {
	var pos protocol.Position
	if p.MetresPerUnit != 0 {
		pos.X = (lon - p.OriginLon) / p.degPerMetreLon() / p.MetresPerUnit
		pos.Y = (lat - p.OriginLat) / p.degPerMetreLat() / p.MetresPerUnit
		pos.Z = (altM - p.OriginAltM) / p.MetresPerUnit
	}
	return pos
}

// Unproject converts MAVLink integer units back to cube units.
//
// Project followed by Unproject is NOT bit-exact, and cannot be: a latitude
// quantised to 1e-7 degrees has lost up to 5.6 mm, and an altitude in whole
// millimetres up to 0.5 mm. The round trip is accurate to roughly a centimetre,
// which is four orders of magnitude finer than the 20 m cube unit it is
// expressed in. Tests assert that bound rather than equality.
func (p Projection) Unproject(g Global) protocol.Position {
	return p.UnprojectFloat(
		float64(g.LatE7)/1e7,
		float64(g.LonE7)/1e7,
		float64(g.AltMM)/1000,
	)
}

// toInt32 saturates instead of wrapping.
//
// Wrapping is what a plain int32() conversion does to an out-of-range float in
// Go -- the result is implementation-defined and in practice garbage -- and a
// wrapped latitude flips the hemisphere. Saturation keeps a misconfigured origin
// or an absurd position visibly pinned at the edge of the world instead of
// teleporting a vehicle to its antipode. Legitimate values are nowhere near the
// bound: the whole default cube spans about 0.018 degrees.
func toInt32(f float64) int32 {
	switch {
	case math.IsNaN(f):
		return 0
	case f > math.MaxInt32:
		return math.MaxInt32
	case f < math.MinInt32:
		return math.MinInt32
	}
	return int32(f)
}
