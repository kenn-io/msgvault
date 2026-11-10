package tumblr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

type contextReaderAt struct {
	ctx context.Context
	r   io.ReaderAt
}

func (r *contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.ReadAt(p, off)
}

// checkZIP bounds the standard decoder's eager central-directory allocations.
func checkZIP(ctx context.Context, input io.ReaderAt, size int64, maxCount, maxBytes int) error {
	r := &contextReaderAt{ctx, input}
	read := func(off int64, n int) ([]byte, error) {
		if off < 0 || off > size || int64(n) > size-off {
			return nil, zip.ErrFormat
		}
		b := make([]byte, n)
		_, err := r.ReadAt(b, off)
		return b, err
	}
	u16, u32 := binary.LittleEndian.Uint16, binary.LittleEndian.Uint32
	zip64Int := func(p []byte) int64 {
		v := binary.LittleEndian.Uint64(p)
		if v > math.MaxInt64 {
			return -1
		}
		return int64(v)
	}
	tail := min(size, 65*1024)
	b, err := read(size-tail, int(tail))
	if err != nil {
		return err
	}
	i := bytes.LastIndex(b[:max(len(b)-18, 0)], []byte("PK\x05\x06"))
	if i < 0 || int(u16(b[i+20:])) > len(b)-i-22 {
		return zip.ErrFormat
	}
	footer, end := b[i:], size-tail+int64(i)
	count, length, offset := int64(u16(footer[10:])), int64(u32(footer[12:])), int64(u32(footer[16:]))
	if count == 0xffff || length == 0xffffffff || offset == 0xffffffff {
		loc, err := read(end-20, 20)
		if err != nil {
			return err
		}
		if u32(loc) == 0x07064b50 && u32(loc[4:]) == 0 && u32(loc[16:]) == 1 {
			pos := zip64Int(loc[8:])
			b, err := read(pos, 56)
			if err != nil {
				return err
			}
			if u32(b) != 0x06064b50 {
				return zip.ErrFormat
			}
			count, length, offset, end = zip64Int(b[32:]), zip64Int(b[40:]), zip64Int(b[48:]), pos
		}
	}
	if count < 0 || count > int64(maxCount) || length < 0 || length > int64(maxBytes) {
		return errors.New("tumblr: ZIP metadata limit exceeded")
	}
	start := end - length
	// archive/zip reads from the absolute offset instead when a header is there.
	if h, err := read(offset, 4); offset < 0 || start < 0 || (start > offset && err == nil && u32(h) == 0x02014b50) {
		return zip.ErrFormat
	}
	for n, pos := 0, start; ; n++ {
		h, err := read(pos, 46)
		if err != nil || u32(h) != 0x02014b50 {
			return ctx.Err()
		}
		length := int64(46) + int64(u16(h[28:])) + int64(u16(h[30:])) + int64(u16(h[32:]))
		if n >= maxCount || length > int64(maxBytes)-(pos-start) {
			return errors.New("tumblr: ZIP metadata limit exceeded")
		}
		pos += length
	}
}
