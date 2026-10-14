package mmdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Data section field types, see "Data Section" in the MaxMind DB specification.
const (
	typeExtended  = 0
	typePointer   = 1
	typeString    = 2
	typeDouble    = 3
	typeBytes     = 4
	typeUint16    = 5
	typeUint32    = 6
	typeMap       = 7
	typeInt32     = 8
	typeUint64    = 9
	typeUint128   = 10
	typeArray     = 11
	typeContainer = 12
	typeEndMarker = 13
	typeBool      = 14
	typeFloat     = 15
)

// Keys extracted by decodeCountry.
const (
	keyCountry   = "country"
	keyContinent = "continent"
	keyISOCode   = "iso_code"
	keyCode      = "code"
)

// maxDepth bounds the nesting of the generic decoder (metadata/debug only).
const maxDepth = 64

var (
	errTruncated       = errors.New("mmdb: data section is truncated")
	errUnexpectedType  = errors.New("mmdb: unexpected data type")
	errPointerToPtr    = errors.New("mmdb: pointer to pointer")
	errInvalidPointer  = errors.New("mmdb: pointer out of range")
	errTooDeeplyNested = errors.New("mmdb: data is too deeply nested")
)

// readControl decodes the control byte(s) at off and returns the field type,
// its size and the offset of the payload.
//
// For pointers, size holds the resolved target offset (relative to data) and
// next points right after the pointer bytes. For booleans, size holds the
// value. Maps and arrays report their number of entries.
func readControl(data []byte, off int) (typ, size, next int, err error) {
	if off < 0 || off >= len(data) {
		return 0, 0, 0, errTruncated
	}

	ctrl := int(data[off])
	off++
	typ = ctrl >> 5

	if typ == typePointer {
		ss := (ctrl >> 3) & 3
		vvv := ctrl & 7

		if off+ss+1 > len(data) {
			return 0, 0, 0, errTruncated
		}

		switch ss {
		case 0:
			size = vvv<<8 | int(data[off])
		case 1:
			size = (vvv<<16 | int(data[off])<<8 | int(data[off+1])) + 2048
		case 2:
			size = (vvv<<24 | int(data[off])<<16 | int(data[off+1])<<8 | int(data[off+2])) + 526336
		default:
			size = int(data[off])<<24 | int(data[off+1])<<16 | int(data[off+2])<<8 | int(data[off+3])
		}

		if size >= len(data) {
			return 0, 0, 0, errInvalidPointer
		}

		return typePointer, size, off + ss + 1, nil
	}

	if typ == typeExtended {
		if off >= len(data) {
			return 0, 0, 0, errTruncated
		}

		typ = int(data[off]) + 7
		off++
	}

	size = ctrl & 0x1f

	switch {
	case size < 29:
	case size == 29:
		if off+1 > len(data) {
			return 0, 0, 0, errTruncated
		}

		size = 29 + int(data[off])
		off++
	case size == 30:
		if off+2 > len(data) {
			return 0, 0, 0, errTruncated
		}

		size = 285 + (int(data[off])<<8 | int(data[off+1]))
		off += 2
	default:
		if off+3 > len(data) {
			return 0, 0, 0, errTruncated
		}

		size = 65821 + (int(data[off])<<16 | int(data[off+1])<<8 | int(data[off+2]))
		off += 3
	}

	return typ, size, off, nil
}

// skipValue returns the offset right after the value starting at off, without
// decoding it. Pointers are not followed.
func skipValue(data []byte, off int) (int, error) {
	remaining := 1

	for remaining > 0 {
		typ, size, next, err := readControl(data, off)
		if err != nil {
			return 0, err
		}

		remaining--

		switch typ {
		case typePointer, typeBool:
			off = next
		case typeMap:
			remaining += 2 * size
			off = next
		case typeArray:
			remaining += size
			off = next
		case typeContainer, typeEndMarker:
			return 0, errUnexpectedType
		default:
			off = next + size
			if off > len(data) {
				return 0, errTruncated
			}
		}
	}

	return off, nil
}

// readStringAt returns the position and length of the string value starting at
// off, following one level of pointer indirection, together with the offset of
// the next field. The bytes are not copied.
func readStringAt(data []byte, off int) (start, length, next int, err error) {
	typ, size, next, err := readControl(data, off)
	if err != nil {
		return 0, 0, 0, err
	}

	if typ == typePointer {
		typ, size, start, err = readControl(data, size)
		if err != nil {
			return 0, 0, 0, err
		}

		if typ == typePointer {
			return 0, 0, 0, errPointerToPtr
		}
	} else {
		start = next
		next += size
	}

	if typ != typeString {
		return 0, 0, 0, fmt.Errorf("%w: expected a string, got type %d", errUnexpectedType, typ)
	}

	if start+size > len(data) {
		return 0, 0, 0, errTruncated
	}

	return start, size, next, nil
}

// openMap decodes the control bytes of a map at off, following one level of
// pointer indirection. It returns the number of entries, the offset of the
// first entry and the offset of the field following the map *when it was
// reached through a pointer* (otherwise -1, meaning the caller has to walk the
// entries to find out).
func openMap(data []byte, off int) (count, first, afterPtr int, err error) {
	typ, size, next, err := readControl(data, off)
	if err != nil {
		return 0, 0, 0, err
	}

	afterPtr = -1

	if typ == typePointer {
		afterPtr = next

		typ, size, next, err = readControl(data, size)
		if err != nil {
			return 0, 0, 0, err
		}

		if typ == typePointer {
			return 0, 0, 0, errPointerToPtr
		}
	}

	if typ != typeMap {
		return 0, 0, 0, fmt.Errorf("%w: expected a map, got type %d", errUnexpectedType, typ)
	}

	return size, next, afterPtr, nil
}

// findString walks the map at off looking for key and returns its string
// value. When stopEarly is set, it returns as soon as the key is found and the
// returned next offset is meaningless; otherwise next is the offset following
// the whole map.
func findString(data []byte, off int, key string, stopEarly bool) (val string, found bool, next int, err error) {
	count, off, afterPtr, err := openMap(data, off)
	if err != nil {
		return "", false, 0, err
	}

	for i := 0; i < count; i++ {
		kstart, klen, vstart, err := readStringAt(data, off)
		if err != nil {
			return "", false, 0, err
		}

		if !found && klen == len(key) && string(data[kstart:kstart+klen]) == key {
			sstart, slen, n, err := readStringAt(data, vstart)
			if err != nil {
				return "", false, 0, err
			}

			val = string(data[sstart : sstart+slen])
			found = true

			if stopEarly {
				return val, true, 0, nil
			}

			off = n

			continue
		}

		off, err = skipValue(data, vstart)
		if err != nil {
			return "", false, 0, err
		}
	}

	if afterPtr >= 0 {
		off = afterPtr
	}

	return val, found, off, nil
}

// decodeCountry extracts country.iso_code and continent.code from the GeoIP2
// record at off. Decoding stops as soon as both values are known.
func decodeCountry(data []byte, off int) (Country, error) {
	var (
		c            Country
		gotCountry   bool
		gotContinent bool
	)

	count, off, _, err := openMap(data, off)
	if err != nil {
		return Country{}, err
	}

	for i := 0; i < count; i++ {
		kstart, klen, vstart, err := readStringAt(data, off)
		if err != nil {
			return Country{}, err
		}

		switch {
		case !gotCountry && klen == len(keyCountry) && string(data[kstart:kstart+klen]) == keyCountry:
			gotCountry = true

			c.ISOCode, _, off, err = findString(data, vstart, keyISOCode, gotContinent)
			if err != nil {
				return Country{}, err
			}

			if gotContinent {
				return c, nil
			}
		case !gotContinent && klen == len(keyContinent) && string(data[kstart:kstart+klen]) == keyContinent:
			gotContinent = true

			c.ContinentCode, _, off, err = findString(data, vstart, keyCode, gotCountry)
			if err != nil {
				return Country{}, err
			}

			if gotCountry {
				return c, nil
			}
		default:
			off, err = skipValue(data, vstart)
			if err != nil {
				return Country{}, err
			}
		}
	}

	return c, nil
}

// decodeValue generically decodes the value at off. Maps become
// map[string]interface{}, arrays []interface{}, uint128 *big.Int, and the
// other types their natural Go counterpart.
func decodeValue(data []byte, off, depth int) (interface{}, int, error) {
	if depth > maxDepth {
		return nil, 0, errTooDeeplyNested
	}

	typ, size, next, err := readControl(data, off)
	if err != nil {
		return nil, 0, err
	}

	if typ == typePointer {
		typ2, _, _, err := readControl(data, size)
		if err != nil {
			return nil, 0, err
		}

		if typ2 == typePointer {
			return nil, 0, errPointerToPtr
		}

		v, _, err := decodeValue(data, size, depth+1)

		return v, next, err
	}

	switch typ {
	case typeMap:
		m := make(map[string]interface{}, size)

		for i := 0; i < size; i++ {
			kstart, klen, vstart, err := readStringAt(data, next)
			if err != nil {
				return nil, 0, err
			}

			var v interface{}

			v, next, err = decodeValue(data, vstart, depth+1)
			if err != nil {
				return nil, 0, err
			}

			m[string(data[kstart:kstart+klen])] = v
		}

		return m, next, nil
	case typeArray:
		a := make([]interface{}, 0, size)

		for i := 0; i < size; i++ {
			var v interface{}

			v, next, err = decodeValue(data, next, depth+1)
			if err != nil {
				return nil, 0, err
			}

			a = append(a, v)
		}

		return a, next, nil
	case typeBool:
		// Assigning through a typed variable keeps Yaegi happy (returning a
		// comparison straight into an interface{} result panics there).
		var b interface{} = size != 0

		return b, next, nil
	case typeContainer, typeEndMarker:
		return nil, 0, errUnexpectedType
	}

	if next+size > len(data) {
		return nil, 0, errTruncated
	}

	payload := data[next : next+size]
	next += size

	switch typ {
	case typeString:
		return string(payload), next, nil
	case typeBytes:
		return append([]byte(nil), payload...), next, nil
	case typeDouble:
		if size != 8 {
			return nil, 0, fmt.Errorf("%w: invalid double size %d", errUnexpectedType, size)
		}

		return math.Float64frombits(binary.BigEndian.Uint64(payload)), next, nil
	case typeFloat:
		if size != 4 {
			return nil, 0, fmt.Errorf("%w: invalid float size %d", errUnexpectedType, size)
		}

		return math.Float32frombits(binary.BigEndian.Uint32(payload)), next, nil
	case typeUint16:
		if size > 2 {
			return nil, 0, fmt.Errorf("%w: invalid uint16 size %d", errUnexpectedType, size)
		}

		return uint16(bigEndianUint(payload)), next, nil
	case typeUint32:
		if size > 4 {
			return nil, 0, fmt.Errorf("%w: invalid uint32 size %d", errUnexpectedType, size)
		}

		return uint32(bigEndianUint(payload)), next, nil
	case typeInt32:
		if size > 4 {
			return nil, 0, fmt.Errorf("%w: invalid int32 size %d", errUnexpectedType, size)
		}

		return int32(uint32(bigEndianUint(payload))), next, nil
	case typeUint64:
		if size > 8 {
			return nil, 0, fmt.Errorf("%w: invalid uint64 size %d", errUnexpectedType, size)
		}

		return bigEndianUint(payload), next, nil
	case typeUint128:
		if size > 16 {
			return nil, 0, fmt.Errorf("%w: invalid uint128 size %d", errUnexpectedType, size)
		}

		return new(big.Int).SetBytes(payload), next, nil
	default:
		return nil, 0, fmt.Errorf("%w: unknown type %d", errUnexpectedType, typ)
	}
}

// bigEndianUint decodes a variable-length (0 to 8 bytes) big-endian unsigned
// integer.
func bigEndianUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}

	return v
}
