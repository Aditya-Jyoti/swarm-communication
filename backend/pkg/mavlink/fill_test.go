package mavlink

import (
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// Shared test helpers
// ---------------------------------------------------------------------------

// allMessages returns one zero value of every message type this build knows,
// ordered by message id.
//
// Ordered, not map order, because several tests below are table-driven over this
// slice and a failure report that names a different subtest on every run is
// nearly useless. Driving off the registry rather than a hand-written list means
// adding a message to gen/main.go cannot add an untested message.
func allMessages() []Message {
	ids := make([]int, 0, len(messageFactory))
	for id := range messageFactory {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)

	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		out = append(out, messageFactory[MsgID(id)]())
	}
	return out
}

// fillMessage writes a deterministic, non-zero value into the first nfields
// struct fields of m. A negative nfields means every field.
//
// Non-zero matters: a zero field is invisible on the wire, because the v2
// encoder trims trailing zeroes, so a codec bug in the last field of a message
// would never show up in a round-trip test built from zero values.
//
// The fill is by field INDEX, and the generated structs declare their fields in
// wire order, so index i here is wire field i everywhere -- including in the
// pymavlink conformance test, which identifies fields by position rather than by
// trying to reproduce this package's Go naming rules in Python.
//
// Byte arrays are filled with printable ASCII. A MAVLink char array is not
// distinguishable from a uint8 array in Go (both are [N]byte), and the
// conformance oracle hands back char arrays as decoded strings cut at the first
// NUL, so NUL and non-ASCII bytes would make the comparison a test of Python's
// error handling rather than of the codec.
func fillMessage(m Message, nfields int) {
	v := reflect.ValueOf(m).Elem()
	n := v.NumField()
	if nfields >= 0 && nfields < n {
		n = nfields
	}
	for i := 0; i < n; i++ {
		fillValue(v.Field(i), i)
	}
}

func fillValue(f reflect.Value, i int) {
	switch f.Kind() {
	case reflect.Array:
		for j := 0; j < f.Len(); j++ {
			e := f.Index(j)
			if e.Kind() == reflect.Uint8 {
				// 'A'..'Z', wrapping: printable, never NUL.
				e.SetUint(uint64('A' + (i*7+j)%26))
				continue
			}
			fillValue(e, i*31+j)
		}
	case reflect.Uint8:
		f.SetUint(uint64(1 + (i*13)%200))
	case reflect.Uint16:
		f.SetUint(uint64(1001 + i*7))
	case reflect.Uint32:
		f.SetUint(uint64(100003 + i*911))
	case reflect.Uint64:
		f.SetUint(0x0102030405060000 + uint64(i))
	case reflect.Int8:
		f.SetInt(int64(-1 - i%120))
	case reflect.Int16:
		f.SetInt(int64(-2003 - i*3))
	case reflect.Int32:
		f.SetInt(int64(-100019 - i*77))
	case reflect.Int64:
		f.SetInt(-0x0102030405060000 - int64(i))
	case reflect.Float32:
		// Exactly representable in both float32 and float64, so a value that has
		// been through JSON on the way to the oracle compares exactly and the
		// test does not need an epsilon it would then have to justify.
		f.SetFloat(float64(float32(1.5 + float32(i))))
	case reflect.Float64:
		f.SetFloat(2.25 + float64(i))
	default:
		panic("fillValue: unhandled kind " + f.Kind().String())
	}
}

func TestFillMessageLeavesNoZeroField(t *testing.T) {
	// Guards the helper itself: if fillValue ever stops covering a kind the
	// generator emits, every round-trip test below silently weakens instead of
	// failing.
	for _, m := range allMessages() {
		t.Run(m.MsgName(), func(t *testing.T) {
			fillMessage(m, -1)
			v := reflect.ValueOf(m).Elem()
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).IsZero() {
					t.Errorf("field %d (%s) is still zero after fillMessage",
						i, v.Type().Field(i).Name)
				}
			}
		})
	}
}
