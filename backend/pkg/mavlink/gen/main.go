//go:build ignore

// Command gen turns a MAVLink dialect XML definition into Go source.
//
// # Re-running it
//
// From backend/pkg/mavlink/, which is where the go:generate directive in doc.go
// runs it:
//
//	go generate ./...
//
// or, equivalently and by hand:
//
//	go run ./gen/main.go -out dialect.go ./gen/dialect/common.xml
//
// Note the explicit main.go: this file is behind a //go:build ignore tag so that
// it never becomes part of the mavlink package, and `go run ./gen` would
// therefore report that build constraints exclude every file in the directory.
//
// The XML path is the only positional argument. <include> elements are followed
// relative to the directory of the file that contains them, so the vendored
// dialect/ directory must hold the whole include chain (common -> standard ->
// minimal). The XML is vendored rather than fetched so that the generator is
// hermetic: regenerating on a machine with no network, or two years from now,
// must produce byte-identical output or the CRC_EXTRA bytes silently stop
// matching whatever the ground station believes.
//
// Output is deterministic: the message set and the enum allowlist below are
// explicit, ordered lists, and nothing iterates a map while emitting.
//
// # Why a generator at all, rather than hand-written codecs
//
// The wire layout of a MAVLink message is not a property anyone can read off
// the message definition by eye. Fields are reordered by descending type size
// before they are serialised, and each message carries a CRC_EXTRA byte that is
// a checksum over its own field names and types in that reordered sequence. Both
// are mechanical, both are easy to get subtly wrong, and both fail in the same
// way when wrong: a frame that is accepted by nothing and reports no reason. The
// generator derives them from the same XML the ground station's own bindings are
// built from, which is the only way to be sure the two agree.
//
// # Why not pymavlink's mavgen
//
// mavgen has no Go backend, and adding pymavlink to the build would put a Python
// toolchain between a clone of this repo and a compiling binary. pymavlink is
// used in the conformance test instead, where it belongs: as an independent
// oracle rather than as the source of the code under test.
package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Configuration: what gets generated
// ---------------------------------------------------------------------------

// wantMessages is the message set the bridge speaks, in the order the generated
// file will declare them. Message ids, not names, because the id is the thing
// that appears on the wire and in a packet capture.
//
// The set is deliberately small. A full common.xml binding is ~250 messages and
// well over 100k lines of Go, almost all of it describing hardware this swarm
// does not have. Everything here is either emitted by the bridge or sent to it
// by QGroundControl; see docs/architecture/mavlink-bridge.md section 4.
var wantMessages = []uint32{
	0,   // HEARTBEAT
	1,   // SYS_STATUS
	20,  // PARAM_REQUEST_READ
	21,  // PARAM_REQUEST_LIST
	22,  // PARAM_VALUE
	24,  // GPS_RAW_INT
	30,  // ATTITUDE
	33,  // GLOBAL_POSITION_INT
	39,  // MISSION_ITEM
	40,  // MISSION_REQUEST
	41,  // MISSION_SET_CURRENT
	42,  // MISSION_CURRENT
	43,  // MISSION_REQUEST_LIST
	44,  // MISSION_COUNT
	45,  // MISSION_CLEAR_ALL
	46,  // MISSION_ITEM_REACHED
	47,  // MISSION_ACK
	51,  // MISSION_REQUEST_INT
	73,  // MISSION_ITEM_INT
	76,  // COMMAND_LONG
	77,  // COMMAND_ACK
	148, // AUTOPILOT_VERSION
	253, // STATUSTEXT
}

// enumSelection names an enum and, optionally, the subset of its entries to
// emit. An empty entries list means "the whole enum", which is only used for
// enums that are small and wholly relevant (MAV_RESULT, MAV_STATE, ...).
//
// The allowlist exists because MAV_CMD alone has several hundred entries for
// gimbals, winches and cameras. Emitting them would imply the bridge understands
// them, and a reader grepping for what the bridge actually handles would have to
// filter the noise themselves.
type enumSelection struct {
	name    string
	entries []string // unqualified suffixes are not allowed; full MAVLink names
}

var wantEnums = []enumSelection{
	{name: "MAV_AUTOPILOT", entries: []string{
		"MAV_AUTOPILOT_GENERIC",
		"MAV_AUTOPILOT_ARDUPILOTMEGA",
		"MAV_AUTOPILOT_PX4",
		"MAV_AUTOPILOT_INVALID",
	}},
	{name: "MAV_TYPE", entries: []string{
		"MAV_TYPE_GENERIC",
		"MAV_TYPE_FIXED_WING",
		"MAV_TYPE_QUADROTOR",
		"MAV_TYPE_HEXAROTOR",
		"MAV_TYPE_OCTOROTOR",
		"MAV_TYPE_GCS",
		"MAV_TYPE_ONBOARD_CONTROLLER",
	}},
	{name: "MAV_STATE"},
	{name: "MAV_MODE_FLAG"},
	{name: "MAV_COMPONENT", entries: []string{
		"MAV_COMP_ID_AUTOPILOT1",
	}},
	{name: "MAV_FRAME", entries: []string{
		"MAV_FRAME_GLOBAL",
		"MAV_FRAME_LOCAL_NED",
		"MAV_FRAME_MISSION",
		"MAV_FRAME_GLOBAL_RELATIVE_ALT",
		"MAV_FRAME_GLOBAL_INT",
		"MAV_FRAME_GLOBAL_RELATIVE_ALT_INT",
		"MAV_FRAME_GLOBAL_TERRAIN_ALT",
		"MAV_FRAME_GLOBAL_TERRAIN_ALT_INT",
	}},
	{name: "MAV_CMD", entries: []string{
		"MAV_CMD_NAV_WAYPOINT",
		"MAV_CMD_NAV_TAKEOFF",
		"MAV_CMD_NAV_LAND",
		"MAV_CMD_NAV_RETURN_TO_LAUNCH",
		"MAV_CMD_DO_SET_MODE",
		"MAV_CMD_REQUEST_MESSAGE",
		"MAV_CMD_COMPONENT_ARM_DISARM",
		"MAV_CMD_MISSION_START",
	}},
	{name: "MAV_RESULT"},
	{name: "MAV_MISSION_RESULT"},
	{name: "MAV_PARAM_TYPE"},
}

// ---------------------------------------------------------------------------
// XML model
// ---------------------------------------------------------------------------

// xmlChild is every child element of a <message> or <enum>, captured in document
// order via `xml:",any"`.
//
// Document order is the whole reason for this shape. <extensions/> is a marker
// element, not an attribute: the fields before it are the base fields (sorted by
// size, covered by CRC_EXTRA) and the fields after it are extensions (never
// sorted, never in the CRC). A decoder that unmarshals fields into a flat slice
// loses the position of the marker and therefore cannot tell the two apart.
type xmlChild struct {
	XMLName xml.Name
	Type    string `xml:"type,attr"`
	Name    string `xml:"name,attr"`
	Enum    string `xml:"enum,attr"`
	Value   string `xml:"value,attr"`
	Units   string `xml:"units,attr"`
	// Desc is an <entry>'s nested <description> child. A <field> has no such
	// child -- its prose is chardata -- so the two are read from different
	// places and both are captured.
	Desc string `xml:"description"`
	Text string `xml:",chardata"`
}

type xmlMessage struct {
	ID       uint32     `xml:"id,attr"`
	Name     string     `xml:"name,attr"`
	Children []xmlChild `xml:",any"`
}

type xmlEnum struct {
	Name     string     `xml:"name,attr"`
	Bitmask  string     `xml:"bitmask,attr"`
	Children []xmlChild `xml:",any"`
}

type xmlDialect struct {
	Include  []string     `xml:"include"`
	Version  int          `xml:"version"`
	Messages []xmlMessage `xml:"messages>message"`
	Enums    []xmlEnum    `xml:"enums>enum"`
}

// dialect is the merged result of following the include chain.
type dialect struct {
	version  int
	messages map[uint32]xmlMessage
	enums    map[string]xmlEnum
}

// load parses path and everything it includes. Includes are resolved relative to
// the including file and visited once; the chain is a DAG in practice but is
// treated as possibly cyclic because nothing in the XML forbids a cycle.
func load(path string) (*dialect, error) {
	d := &dialect{
		messages: make(map[uint32]xmlMessage),
		enums:    make(map[string]xmlEnum),
	}
	seen := make(map[string]bool)
	if err := d.loadInto(path, seen); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *dialect) loadInto(path string, seen map[string]bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	if seen[abs] {
		return nil
	}
	seen[abs] = true

	raw, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read dialect: %w", err)
	}
	var parsed xmlDialect
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse %s: %w", abs, err)
	}

	// The including file wins on a name collision, which is MAVLink's own rule:
	// a dialect may redefine a message it inherits. Parsing the includes after
	// the parent and refusing to overwrite is how that is implemented here.
	if d.version == 0 {
		d.version = parsed.Version
	}
	for _, m := range parsed.Messages {
		if _, dup := d.messages[m.ID]; !dup {
			d.messages[m.ID] = m
		}
	}
	for _, e := range parsed.Enums {
		if _, dup := d.enums[e.Name]; !dup {
			d.enums[e.Name] = e
		}
	}

	dir := filepath.Dir(abs)
	for _, inc := range parsed.Include {
		if err := d.loadInto(filepath.Join(dir, strings.TrimSpace(inc)), seen); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Field model
// ---------------------------------------------------------------------------

// typeInfo describes one MAVLink primitive type.
type typeInfo struct {
	size    int    // bytes on the wire, per element
	goType  string // Go scalar
	isFloat bool
}

var types = map[string]typeInfo{
	"char":     {1, "byte", false},
	"uint8_t":  {1, "uint8", false},
	"int8_t":   {1, "int8", false},
	"uint16_t": {2, "uint16", false},
	"int16_t":  {2, "int16", false},
	"uint32_t": {4, "uint32", false},
	"int32_t":  {4, "int32", false},
	"uint64_t": {8, "uint64", false},
	"int64_t":  {8, "int64", false},
	"float":    {4, "float32", true},
	"double":   {8, "float64", true},
}

type field struct {
	name     string // MAVLink name, e.g. "custom_mode"
	goName   string // Go name, e.g. "CustomMode"
	mavType  string // canonical MAVLink type name, e.g. "uint8_t"
	arrayLen int    // 0 for a scalar
	enum     string // referenced MAVLink enum, "" if none
	units    string // MAVLink units attribute, e.g. "degE7"; "" if dimensionless
	doc      string
	info     typeInfo
	offset   int // wire offset, filled in after sorting
}

func (f field) wireLen() int {
	if f.arrayLen > 0 {
		return f.arrayLen * f.info.size
	}
	return f.info.size
}

// goDecl is the Go type of the struct field.
func (f field) goDecl() string {
	if f.arrayLen > 0 {
		return fmt.Sprintf("[%d]%s", f.arrayLen, f.info.goType)
	}
	return f.info.goType
}

// parseField converts a <field> element. The type attribute carries both the
// primitive and, for arrays, the length: "char[50]".
func parseField(c xmlChild) (field, error) {
	f := field{
		name:  strings.TrimSpace(c.Name),
		enum:  strings.TrimSpace(c.Enum),
		doc:   shorten(collapse(c.Text), 200),
		units: strings.TrimSpace(c.Units),
	}
	t := strings.TrimSpace(c.Type)

	// uint8_t_mavlink_version is a pseudo-type used by exactly one field, in
	// HEARTBEAT. It is a plain uint8 on the wire and in CRC_EXTRA; pymavlink
	// treats it as a constant so that callers cannot set it wrong. Here it stays
	// a normal field and the caller is told to use ProtocolVersion, because the
	// bridge also has to be able to *decode* a peer's heartbeat and report what
	// version it claimed.
	if t == "uint8_t_mavlink_version" {
		t = "uint8_t"
	}

	if i := strings.IndexByte(t, '['); i >= 0 {
		if !strings.HasSuffix(t, "]") {
			return f, fmt.Errorf("field %s: malformed array type %q", f.name, c.Type)
		}
		n, err := strconv.Atoi(t[i+1 : len(t)-1])
		if err != nil || n <= 0 {
			return f, fmt.Errorf("field %s: bad array length in %q", f.name, c.Type)
		}
		f.arrayLen = n
		t = t[:i]
	}
	info, ok := types[t]
	if !ok {
		return f, fmt.Errorf("field %s: unknown type %q", f.name, t)
	}
	f.mavType = t
	f.info = info
	f.goName = goFieldName(f.name)
	return f, nil
}

// message is one message ready to emit.
type message struct {
	id       uint32
	mavName  string // HEARTBEAT
	goName   string // Heartbeat
	doc      string
	fields   []field // WIRE order: base fields size-sorted, then extensions
	baseN    int     // number of base fields; fields[baseN:] are extensions
	payload  int     // full payload length including extensions
	crcExtra byte
}

// buildMessage computes the wire order, offsets and CRC_EXTRA for a message.
//
// The sort is by descending type size and MUST be stable: fields of equal size
// keep their XML order, and pymavlink's sorted() is stable, so an unstable sort
// here would produce a different -- and wrong -- layout for any message with two
// same-sized fields, which is almost all of them.
func buildMessage(m xmlMessage) (message, error) {
	out := message{id: m.ID, mavName: m.Name, goName: goTypeName(m.Name)}

	var base, ext []field
	inExt := false
	for _, c := range m.Children {
		switch c.XMLName.Local {
		case "description":
			if out.doc == "" {
				out.doc = shorten(collapse(c.Text), 400)
			}
		case "extensions":
			inExt = true
		case "field":
			f, err := parseField(c)
			if err != nil {
				return out, fmt.Errorf("%s: %w", m.Name, err)
			}
			if inExt {
				ext = append(ext, f)
			} else {
				base = append(base, f)
			}
		}
	}

	sort.SliceStable(base, func(i, j int) bool {
		return base[i].info.size > base[j].info.size
	})

	out.baseN = len(base)
	out.fields = append(base, ext...)
	off := 0
	for i := range out.fields {
		out.fields[i].offset = off
		off += out.fields[i].wireLen()
	}
	out.payload = off
	if out.payload > 255 {
		return out, fmt.Errorf("%s: payload is %d bytes, over the 255-byte MAVLink limit", m.Name, out.payload)
	}
	out.crcExtra = crcExtra(out)
	return out, nil
}

// crcExtra is MAVLink's message-definition checksum: a CRC-16/MCRF4XX over the
// message name and the name and type of every BASE field in wire order, folded
// into one byte.
//
// Extension fields are excluded on purpose. That exclusion is what makes
// extensions backwards compatible: an old node and a new node compute the same
// CRC_EXTRA for a message whose definition grew, so the old node still accepts
// the new node's frames (and zero-fills the fields it has never heard of).
func crcExtra(m message) byte {
	crc := uint16(0xFFFF)
	crc = crcString(crc, m.mavName+" ")
	for _, f := range m.fields[:m.baseN] {
		crc = crcString(crc, f.mavType+" ")
		crc = crcString(crc, f.name+" ")
		if f.arrayLen > 0 {
			// The array length is accumulated as a raw byte, not as digits.
			crc = crcAccumulate(crc, byte(f.arrayLen))
		}
	}
	return byte(crc&0xFF) ^ byte(crc>>8)
}

func crcString(crc uint16, s string) uint16 {
	for i := 0; i < len(s); i++ {
		crc = crcAccumulate(crc, s[i])
	}
	return crc
}

func crcAccumulate(crc uint16, b byte) uint16 {
	tmp := b ^ byte(crc)
	tmp ^= tmp << 4
	return (crc >> 8) ^ (uint16(tmp) << 8) ^ (uint16(tmp) << 3) ^ (uint16(tmp) >> 4)
}

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

// initialisms are the snake_case segments that Go style wants fully
// capitalised. Kept short on purpose: "sw", "os" and "hw" are NOT in here,
// because AUTOPILOT_VERSION would then produce OSSWVersion, which is worse than
// the non-idiomatic OsSwVersion it replaces.
var initialisms = map[string]string{
	"id":   "ID",
	"uid":  "UID",
	"uid2": "UID2",
	"gps":  "GPS",
	"hdop": "HDOP",
	"vdop": "VDOP",
	"cog":  "COG",
	"rssi": "RSSI",
	"url":  "URL",
}

func camel(s string, upperFirst bool) string {
	var b strings.Builder
	for i, seg := range strings.Split(strings.ToLower(s), "_") {
		if seg == "" {
			continue
		}
		if i == 0 && !upperFirst {
			b.WriteString(seg)
			continue
		}
		if up, ok := initialisms[seg]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]))
		b.WriteString(seg[1:])
	}
	return b.String()
}

func goTypeName(mavName string) string  { return camel(mavName, true) }
func goFieldName(mavName string) string { return camel(mavName, true) }

// goConstName keeps the full MAVLink name, camel-cased, rather than stripping
// the enum prefix. MAV_COMP_ID_AUTOPILOT1 lives in the MAV_COMPONENT enum and
// MAV_MISSION_ACCEPTED lives in MAV_MISSION_RESULT, so prefix stripping does not
// work in general -- and the unstripped name is what the spec, the ground
// station's logs and every packet capture call it.
func goConstName(entryName string) string { return camel(entryName, true) }

// collapse folds XML prose into a single line fit for a Go doc comment.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// shorten caps a doc comment at n characters, cutting on a word boundary.
//
// The XML prose is written for the online message reference and some of it runs
// to a dozen lines; MAV_COMPONENT's description alone is a paragraph about
// historical component-id practice. Pasted verbatim, it buries the generated
// code under documentation of hardware this package will never meet. The link to
// the full text is https://mavlink.io/en/messages/common.html.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndexByte(s[:n], ' ')
	if cut <= 0 {
		cut = n
	}
	return strings.TrimRight(s[:cut], " ,;:.") + " ..."
}

// ---------------------------------------------------------------------------
// Emit
// ---------------------------------------------------------------------------

func main() {
	out := flag.String("out", "-", "output file, or - for stdout")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./pkg/mavlink/gen [-out FILE] DIALECT.xml")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(xmlPath, outPath string) error {
	d, err := load(xmlPath)
	if err != nil {
		return err
	}

	msgs := make([]message, 0, len(wantMessages))
	for _, id := range wantMessages {
		xm, ok := d.messages[id]
		if !ok {
			return fmt.Errorf("message id %d is not defined by %s or its includes", id, xmlPath)
		}
		m, err := buildMessage(xm)
		if err != nil {
			return err
		}
		msgs = append(msgs, m)
	}

	src, err := emit(xmlPath, d, msgs)
	if err != nil {
		return err
	}
	formatted, err := format.Source(src)
	if err != nil {
		// Write the unformatted source to stderr context so the syntax error in
		// the generator's template is findable, instead of reporting a line
		// number against a file nobody has.
		return fmt.Errorf("gofmt the generated source: %w", err)
	}
	if outPath == "-" {
		_, err = os.Stdout.Write(formatted)
		return err
	}
	return os.WriteFile(outPath, formatted, 0o644)
}

func emit(xmlPath string, d *dialect, msgs []message) ([]byte, error) {
	var b bytes.Buffer
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	// The header names the generator and the dialect, but deliberately NOT a
	// timestamp or a tool version: either would make the output differ on every
	// run and turn "is dialect.go in sync with the XML?" from a diff into a
	// guess.
	p("// Code generated by pkg/mavlink/gen from %s (dialect version %d). DO NOT EDIT.\n\n",
		filepath.Base(xmlPath), d.version)
	p("package mavlink\n\n")
	p("import (\n\t\"encoding/binary\"\n\t\"fmt\"\n\t\"math\"\n)\n\n")

	if err := emitEnums(p, d); err != nil {
		return nil, err
	}
	emitIDs(p, msgs)
	for _, m := range msgs {
		emitMessage(p, m)
	}
	emitRegistry(p, msgs)
	return b.Bytes(), nil
}

func emitEnums(p func(string, ...any), d *dialect) error {
	p("// ---------------------------------------------------------------------------\n")
	p("// Enums\n")
	p("//\n")
	p("// Emitted as untyped constants rather than defined types. A MAVLink enum is\n")
	p("// not a type on the wire -- MAV_TYPE travels in a uint8 field, MAV_CMD in a\n")
	p("// uint16 in COMMAND_LONG and a uint16 in MISSION_ITEM -- so a defined type\n")
	p("// would buy no safety and cost a conversion at every single use.\n")
	p("// ---------------------------------------------------------------------------\n\n")

	for _, sel := range wantEnums {
		e, ok := d.enums[sel.name]
		if !ok {
			return fmt.Errorf("enum %s is not defined by the dialect", sel.name)
		}

		// Index the entries so the allowlist controls both membership and order.
		byName := make(map[string]xmlChild)
		var order []string
		var doc string
		for _, c := range e.Children {
			switch c.XMLName.Local {
			case "description":
				if doc == "" {
					doc = collapse(c.Text)
				}
			case "entry":
				name := strings.TrimSpace(c.Name)
				byName[name] = c
				order = append(order, name)
			}
		}
		if len(sel.entries) > 0 {
			order = sel.entries
		}

		p("// %s", sel.name)
		if doc != "" {
			p(": %s", doc)
		}
		p("\n")
		if len(sel.entries) > 0 {
			p("//\n// Only the entries the bridge uses are generated.\n")
		}
		p("const (\n")
		for _, name := range order {
			c, ok := byName[name]
			if !ok {
				return fmt.Errorf("enum %s has no entry %s", sel.name, name)
			}
			v, err := strconv.ParseUint(strings.TrimSpace(c.Value), 0, 64)
			if err != nil {
				return fmt.Errorf("enum %s entry %s: bad value %q: %w", sel.name, name, c.Value, err)
			}
			if doc := shorten(collapse(c.Desc), 160); doc != "" {
				p("\t// %s: %s\n", name, doc)
			}
			p("\t%s = %d\n", goConstName(name), v)
		}
		p(")\n\n")
	}
	return nil
}

func emitIDs(p func(string, ...any), msgs []message) {
	p("// Message ids. These are the numbers that appear in the 24-bit msgid field of\n")
	p("// a v2 frame and in every packet capture, so they are the primary identity of\n")
	p("// a message in this package.\n")
	p("const (\n")
	for _, m := range msgs {
		p("\tMsgID%s MsgID = %d // %s\n", m.goName, m.id, m.mavName)
	}
	p(")\n\n")
}

func emitMessage(p func(string, ...any), m message) {
	p("// ---------------------------------------------------------------------------\n")
	p("// %s (%d)\n", m.mavName, m.id)
	p("// ---------------------------------------------------------------------------\n\n")

	p("// %s is MAVLink message %s (%d).", m.goName, m.mavName, m.id)
	if m.doc != "" {
		p("\n//\n// %s", m.doc)
	}
	p("\n//\n// Fields are in WIRE order, not the order common.xml declares them: base\n")
	p("// fields sorted by descending type size, then %d extension field(s)\n", len(m.fields)-m.baseN)
	p("// unsorted. Payload is %d bytes.\n", m.payload)
	p("type %s struct {\n", m.goName)
	for i, f := range m.fields {
		if i == m.baseN {
			p("\n\t// Extension fields. Absent from CRC_EXTRA, so a sender that predates\n")
			p("\t// them produces frames this build still accepts; they arrive zeroed.\n")
		}
		switch {
		case f.doc != "" && f.units != "":
			p("\t// %s: %s [%s]\n", f.name, f.doc, f.units)
		case f.doc != "":
			p("\t// %s: %s\n", f.name, f.doc)
		case f.units != "":
			p("\t// %s [%s]\n", f.name, f.units)
		default:
			p("\t// %s\n", f.name)
		}
		if f.enum != "" {
			p("\t// Values come from the %s enum.\n", f.enum)
		}
		p("\t%s %s\n", f.goName, f.goDecl())
	}
	p("}\n\n")

	p("// MsgID implements Message.\n")
	p("func (*%s) MsgID() MsgID { return MsgID%s }\n\n", m.goName, m.goName)
	p("// MsgName implements Message.\n")
	p("func (*%s) MsgName() string { return %q }\n\n", m.goName, m.mavName)
	p("// CRCExtra implements Message.\n")
	p("func (*%s) CRCExtra() byte { return %d }\n\n", m.goName, m.crcExtra)
	p("// PayloadLen implements Message.\n")
	p("func (*%s) PayloadLen() int { return %d }\n\n", m.goName, m.payload)

	// Marshal. A stack array, written field by field, then one append. The array
	// does not escape (append copies), so this allocates only when dst has to
	// grow -- which the frame encoder arranges never to happen in steady state.
	p("// Marshal implements Message: it appends the little-endian wire payload of m\n")
	p("// to dst and returns the extended slice. Every one of the %d bytes is\n", m.payload)
	p("// written, so no zeroing is needed; MAVLink payloads have no padding.\n")
	p("func (m *%s) Marshal(dst []byte) []byte {\n", m.goName)
	p("\tvar b [%d]byte\n", m.payload)
	for _, f := range m.fields {
		emitPut(p, f)
	}
	p("\treturn append(dst, b[:]...)\n}\n\n")

	p("// Unmarshal implements Message. A payload longer than %d bytes is accepted\n", m.payload)
	p("// and the excess ignored, so a newer peer that has grown an extension field\n")
	p("// does not break this build. A SHORTER payload is rejected: zero-extending a\n")
	p("// truncated v2 payload is the frame decoder's job, because only it knows the\n")
	p("// payload was trimmed rather than corrupted.\n")
	p("func (m *%s) Unmarshal(p []byte) error {\n", m.goName)
	p("\tif len(p) < %d {\n", m.payload)
	p("\t\treturn fmt.Errorf(\"mavlink: %s payload is %%d bytes, want %d: %%w\", len(p), ErrShortPayload)\n", m.mavName, m.payload)
	p("\t}\n")
	for _, f := range m.fields {
		emitGet(p, f)
	}
	p("\treturn nil\n}\n\n")
}

// emitPut writes one field into the stack array b.
func emitPut(p func(string, ...any), f field) {
	if f.arrayLen > 0 {
		if f.info.size == 1 {
			p("\tcopy(b[%d:%d], m.%s[:])\n", f.offset, f.offset+f.wireLen(), f.goName)
			return
		}
		p("\tfor i := range m.%s {\n", f.goName)
		p("\t\t%s\n", putExpr(f, fmt.Sprintf("%d+i*%d", f.offset, f.info.size), fmt.Sprintf("m.%s[i]", f.goName)))
		p("\t}\n")
		return
	}
	p("\t%s\n", putExpr(f, strconv.Itoa(f.offset), "m."+f.goName))
}

// putExpr is one little-endian store. Signed integers are converted to their
// unsigned counterpart of the same width: that conversion is defined in Go as a
// reinterpretation of the bits, which is exactly two's complement, which is what
// MAVLink specifies.
func putExpr(f field, at, val string) string {
	switch f.mavType {
	case "char", "uint8_t":
		return fmt.Sprintf("b[%s] = %s", at, val)
	case "int8_t":
		return fmt.Sprintf("b[%s] = byte(%s)", at, val)
	case "uint16_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint16(b[%s:], %s)", at, val)
	case "int16_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint16(b[%s:], uint16(%s))", at, val)
	case "uint32_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint32(b[%s:], %s)", at, val)
	case "int32_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint32(b[%s:], uint32(%s))", at, val)
	case "uint64_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint64(b[%s:], %s)", at, val)
	case "int64_t":
		return fmt.Sprintf("binary.LittleEndian.PutUint64(b[%s:], uint64(%s))", at, val)
	case "float":
		return fmt.Sprintf("binary.LittleEndian.PutUint32(b[%s:], math.Float32bits(%s))", at, val)
	case "double":
		return fmt.Sprintf("binary.LittleEndian.PutUint64(b[%s:], math.Float64bits(%s))", at, val)
	}
	panic("unreachable: unknown type " + f.mavType)
}

func emitGet(p func(string, ...any), f field) {
	if f.arrayLen > 0 {
		if f.info.size == 1 {
			p("\tcopy(m.%s[:], p[%d:%d])\n", f.goName, f.offset, f.offset+f.wireLen())
			return
		}
		p("\tfor i := range m.%s {\n", f.goName)
		p("\t\tm.%s[i] = %s\n", f.goName, getExpr(f, fmt.Sprintf("%d+i*%d", f.offset, f.info.size)))
		p("\t}\n")
		return
	}
	p("\tm.%s = %s\n", f.goName, getExpr(f, strconv.Itoa(f.offset)))
}

func getExpr(f field, at string) string {
	switch f.mavType {
	case "char", "uint8_t":
		return fmt.Sprintf("p[%s]", at)
	case "int8_t":
		return fmt.Sprintf("int8(p[%s])", at)
	case "uint16_t":
		return fmt.Sprintf("binary.LittleEndian.Uint16(p[%s:])", at)
	case "int16_t":
		return fmt.Sprintf("int16(binary.LittleEndian.Uint16(p[%s:]))", at)
	case "uint32_t":
		return fmt.Sprintf("binary.LittleEndian.Uint32(p[%s:])", at)
	case "int32_t":
		return fmt.Sprintf("int32(binary.LittleEndian.Uint32(p[%s:]))", at)
	case "uint64_t":
		return fmt.Sprintf("binary.LittleEndian.Uint64(p[%s:])", at)
	case "int64_t":
		return fmt.Sprintf("int64(binary.LittleEndian.Uint64(p[%s:]))", at)
	case "float":
		return fmt.Sprintf("math.Float32frombits(binary.LittleEndian.Uint32(p[%s:]))", at)
	case "double":
		return fmt.Sprintf("math.Float64frombits(binary.LittleEndian.Uint64(p[%s:]))", at)
	}
	panic("unreachable: unknown type " + f.mavType)
}

func emitRegistry(p func(string, ...any), msgs []message) {
	p("// ---------------------------------------------------------------------------\n")
	p("// Registry\n")
	p("// ---------------------------------------------------------------------------\n\n")

	p("// messageFactory maps a message id to a constructor for an empty message of\n")
	p("// that type.\n")
	p("//\n")
	p("// A constructor rather than a prototype value: handing out a shared *Heartbeat\n")
	p("// and letting the decoder unmarshal into it would alias one struct across\n")
	p("// every link and every goroutine in the bridge. The map itself is written once\n")
	p("// at init and only read afterwards, so it needs no lock.\n")
	p("var messageFactory = map[MsgID]func() Message{\n")
	for _, m := range msgs {
		p("\tMsgID%s: func() Message { return new(%s) },\n", m.goName, m.goName)
	}
	p("}\n\n")

	p("// messageNames is for diagnostics: an error about msgid 73 is far less useful\n")
	p("// than one about MISSION_ITEM_INT.\n")
	p("var messageNames = map[MsgID]string{\n")
	for _, m := range msgs {
		p("\tMsgID%s: %q,\n", m.goName, m.mavName)
	}
	p("}\n\n")

	p("// knownPayloadLen maps a message id to its full, untruncated payload length.\n")
	p("// The frame decoder needs this before it can unmarshal anything, because a v2\n")
	p("// sender may have trimmed trailing zero bytes off the payload.\n")
	p("var knownPayloadLen = map[MsgID]int{\n")
	for _, m := range msgs {
		p("\tMsgID%s: %d,\n", m.goName, m.payload)
	}
	p("}\n\n")

	p("// crcExtras maps a message id to its CRC_EXTRA byte. Needed to VERIFY a\n")
	p("// frame's checksum, which is why an unknown message id cannot be checked at\n")
	p("// all and must be dropped rather than trusted.\n")
	p("var crcExtras = map[MsgID]byte{\n")
	for _, m := range msgs {
		p("\tMsgID%s: %d,\n", m.goName, m.crcExtra)
	}
	p("}\n")
}
