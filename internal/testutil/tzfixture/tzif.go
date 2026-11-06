package tzfixture

import "encoding/binary"

// FixedOffset encodes a TZif v1 zone with one fixed-offset local time type.
func FixedOffset(offset int32) []byte {
	data := make([]byte, 54)
	copy(data, "TZif")
	binary.BigEndian.PutUint32(data[36:40], 1)              // type count
	binary.BigEndian.PutUint32(data[40:44], 4)              // abbreviation bytes
	binary.BigEndian.PutUint32(data[44:48], uint32(offset)) //nolint:gosec // TZif stores signed offsets as two's-complement bits.
	copy(data[50:], "TST\x00")
	return data
}
