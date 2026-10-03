package byodb

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Column types. The type code doubles as a 1-byte tag in front of each
// encoded value; no tag is 0xff, which is reserved as "+infinity".
const (
	TYPE_ERROR = 0
	TYPE_BYTES = 1 // string of arbitrary bytes
	TYPE_INT64 = 2 // signed 64-bit integer
)

// Value is a table cell: a tagged union.
type Value struct {
	Type uint32
	I64  int64
	Str  []byte
}

func (v Value) String() string {
	switch v.Type {
	case TYPE_INT64:
		return fmt.Sprint(v.I64)
	case TYPE_BYTES:
		return string(v.Str)
	default:
		return "<invalid>"
	}
}

func typeName(t uint32) string {
	switch t {
	case TYPE_INT64:
		return "int64"
	case TYPE_BYTES:
		return "bytes"
	default:
		return "invalid"
	}
}

// Strings end with a 0x00 terminator, so 0x00 and the escape byte 0x01 are
// escaped: 00 -> 01 01, 01 -> 01 02. This keeps byte-wise sort order, and a
// string sorts before any longer string it is a prefix of.
func escapeString(in []byte) []byte {
	n := 0
	for _, ch := range in {
		if ch <= 1 {
			n++
		}
	}
	if n == 0 {
		return in
	}
	out := make([]byte, 0, len(in)+n)
	for _, ch := range in {
		if ch <= 1 {
			out = append(out, 0x01, ch+1)
		} else {
			out = append(out, ch)
		}
	}
	return out
}

var errDecode = errors.New("byodb: malformed encoded value")

// unescapeString decodes a terminated string and returns the rest of the input.
func unescapeString(in []byte) ([]byte, []byte, error) {
	out := []byte{}
	for i := 0; i < len(in); i++ {
		switch ch := in[i]; ch {
		case 0x00:
			return out, in[i+1:], nil
		case 0x01:
			if i+1 >= len(in) || (in[i+1] != 1 && in[i+1] != 2) {
				return nil, nil, errDecode
			}
			out = append(out, in[i+1]-1)
			i++
		default:
			out = append(out, ch)
		}
	}
	return nil, nil, errDecode
}

// encodeValues concatenates values so that bytes.Compare on the result
// orders tuples column by column.
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		out = append(out, byte(v.Type))
		switch v.Type {
		case TYPE_INT64:
			// big-endian with the sign bit flipped: negatives sort first
			out = binary.BigEndian.AppendUint64(out, uint64(v.I64)^(1<<63))
		case TYPE_BYTES:
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0)
		default:
			panic("byodb: encoding an invalid value")
		}
	}
	return out
}

// decodeValues fills `out`, whose types must be set by the caller.
func decodeValues(in []byte, out []Value) ([]byte, error) {
	for i := range out {
		if len(in) == 0 || uint32(in[0]) != out[i].Type {
			return nil, errDecode
		}
		in = in[1:]
		switch out[i].Type {
		case TYPE_INT64:
			if len(in) < 8 {
				return nil, errDecode
			}
			out[i].I64 = int64(binary.BigEndian.Uint64(in) ^ (1 << 63))
			in = in[8:]
		case TYPE_BYTES:
			var err error
			if out[i].Str, in, err = unescapeString(in); err != nil {
				return nil, err
			}
		}
	}
	return in, nil
}

// encodeKey prefixes the values with a 4-byte table/index prefix, so many
// tables and indexes can share a single B+tree.
func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	out = binary.BigEndian.AppendUint32(out, prefix)
	return encodeValues(out, vals)
}

// encodeKeyPartial encodes a range bound that may cover only a prefix of the
// index columns. The missing columns act as -infinity (nothing appended: the
// shorter key sorts first) or +infinity (0xff, larger than any type tag),
// chosen so that the comparison includes or excludes the whole prefix group:
//
//	a >  1  ->  (a, b) >  (1, +inf)      a <= 1  ->  (a, b) <= (1, +inf)
//	a >= 1  ->  (a, b) >= (1, -inf)      a <  1  ->  (a, b) <  (1, -inf)
func encodeKeyPartial(out []byte, prefix uint32, vals []Value, cmp int) []byte {
	out = encodeKey(out, prefix, vals)
	if cmp == CMP_GT || cmp == CMP_LE {
		out = append(out, 0xff)
	}
	return out
}
