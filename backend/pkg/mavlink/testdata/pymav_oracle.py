#!/usr/bin/env python3
"""pymavlink oracle for conformance_test.go.

This script is a TEST FIXTURE, not part of the build. It is driven by
conformance_test.go, which finds an interpreter via the MAVLINK_PY environment
variable and skips cleanly when there is none, so neither the Go package nor CI
depends on Python being installed.

Run it by hand like this:

    echo '{"info_ids":[0]}' | MAVLINK_PY_UNUSED= python3 pymav_oracle.py

Protocol: one JSON object on stdin, one JSON object on stdout.

Request:
  {
    "info_ids": [0, 33, ...],          # report the reference's view of these
    "encode":   [{"msgid":0,"seq":1,"sysid":7,"compid":1,"values":[...]}],
    "decode":   ["<hex frame>", ...]
  }

Response:
  {
    "info": {"0": {"fields": [...], "types": [...], "crc_extra": 50,
                   "wire_len": 9, "name": "HEARTBEAT"}},
    "encoded": ["<hex frame>", ...],
    "decoded": [{"msgid":0,"seq":1,"sysid":7,"compid":1,
                 "values":[...], "reencoded":"<hex frame>"}]
  }

Values are positional and in WIRE order (pymavlink's ordered_fieldnames), which
is also the field order of the generated Go structs. Matching by position avoids
teaching this script the Go naming rules, which is the one place a conformance
test could agree with the implementation by sharing its bug.

Encoding of a single value:
  char[N]   -> a JSON string (pymavlink hands these back decoded, cut at the
               first NUL, so the Go side compares with mavlink.Chars)
  other[N]  -> a JSON array of numbers
  scalar    -> a JSON number
"""

import json
import sys

from pymavlink.dialects.v20 import common


def mav(sysid, compid, seq):
    """A MAVLink codec instance with no file attached.

    pymavlink takes the sysid, compid and sequence number from this object
    rather than from the message, which is why one is built per frame.
    """
    link = common.MAVLink(None, srcSystem=sysid, srcComponent=compid)
    link.seq = seq
    return link


def message_info(msgid):
    cls = common.mavlink_map[msgid]
    return {
        "name": cls.msgname,
        "fields": list(cls.ordered_fieldnames),
        "types": [
            cls.fieldtypes[cls.fieldnames.index(name)] for name in cls.ordered_fieldnames
        ],
        # Careful: pymavlink indexes fieldtypes by DECLARATION order and
        # array_lengths by WIRE order. Mixing the two up produces an int16[16].
        "array_lengths": list(cls.array_lengths),
        "crc_extra": cls.crc_extra,
    }


def to_kwargs(cls, values):
    """Map positional wire-order values onto constructor keyword arguments."""
    kwargs = {}
    for pos, (name, value) in enumerate(zip(cls.ordered_fieldnames, values)):
        idx = cls.fieldnames.index(name)
        if cls.fieldtypes[idx] == "char" and cls.array_lengths[pos]:
            # The constructor splits char arrays on b"\x00", so it wants bytes.
            value = value.encode("ascii")
        kwargs[name] = value
    return kwargs


def read_values(msg):
    cls = type(msg)
    out = []
    for name in cls.ordered_fieldnames:
        value = getattr(msg, name)
        if isinstance(value, (bytes, bytearray)):
            value = value.decode("ascii", errors="replace")
        out.append(value)
    return out


def main():
    req = json.loads(sys.stdin.read())
    resp = {}

    resp["info"] = {str(i): message_info(i) for i in req.get("info_ids", [])}

    encoded = []
    for case in req.get("encode", []):
        cls = common.mavlink_map[case["msgid"]]
        msg = cls(**to_kwargs(cls, case["values"]))
        link = mav(case["sysid"], case["compid"], case["seq"])
        encoded.append(msg.pack(link).hex())
    resp["encoded"] = encoded

    decoded = []
    for frame_hex in req.get("decode", []):
        raw = bytes.fromhex(frame_hex)
        # A fresh codec per frame: parse_buffer keeps per-link state, and the
        # test wants each frame judged on its own.
        msgs = common.MAVLink(None).parse_buffer(raw) or []
        if len(msgs) != 1:
            raise SystemExit(
                "frame %s parsed into %d messages, want exactly 1" % (frame_hex, len(msgs))
            )
        msg = msgs[0]
        link = mav(msg.get_srcSystem(), msg.get_srcComponent(), msg.get_seq())
        decoded.append(
            {
                "msgid": msg.get_msgId(),
                "name": msg.get_type(),
                "seq": msg.get_seq(),
                "sysid": msg.get_srcSystem(),
                "compid": msg.get_srcComponent(),
                "values": read_values(msg),
                "reencoded": msg.pack(link).hex(),
            }
        )
    resp["decoded"] = decoded

    json.dump(resp, sys.stdout)


if __name__ == "__main__":
    main()
