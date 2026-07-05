package pack

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/require"
)

// genBlob produces byte slices spanning the interesting shapes: empty, tiny,
// compressible (repeated pattern), and incompressible (arbitrary bytes).
func genBlob() gopter.Gen {
	return gen.OneGenOf(
		gen.Const([]byte{}),
		gen.SliceOfN(1, gen.UInt8()).Map(func(b []uint8) []byte { return b }),
		gen.IntRange(1, 2048).Map(func(n int) []byte {
			return bytes.Repeat([]byte("pattern!"), n)
		}),
		gen.SliceOf(gen.UInt8()).Map(func(b []uint8) []byte { return b }),
	)
}

func TestPackRoundTripProperty(t *testing.T) {
	key := testKey(21)
	crypter, err := NewCrypter(key)
	require.NoError(t, err)

	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 40
	properties := gopter.NewProperties(params)

	roundTrip := func(c *Crypter) func(blobs [][]byte) bool {
		return func(blobs [][]byte) bool {
			dir := t.TempDir()
			w, err := NewWriter(dir, WriterOptions{Crypter: c})
			if err != nil {
				return false
			}
			wrote := make([]Entry, 0, len(blobs))
			for _, b := range blobs {
				e, err := w.Append(b)
				if err != nil {
					return false
				}
				wrote = append(wrote, e)
			}
			path := filepath.Join(dir, w.ID()+".mvpack")
			sealedEntries, err := w.Seal(path)
			if err != nil || len(sealedEntries) != len(wrote) {
				return false
			}
			r, err := OpenReader(path, c)
			if err != nil {
				return false
			}
			defer func() { _ = r.Close() }()
			got := r.Entries()
			if len(got) != len(blobs) {
				return false
			}
			for i, e := range got {
				if e != wrote[i] {
					return false
				}
				raw, err := r.ReadBlob(e)
				if err != nil || !bytes.Equal(raw, blobs[i]) {
					return false
				}
			}
			return true
		}
	}

	blobSets := gen.SliceOf(genBlob())
	properties.Property("plain pack round-trips any blob set",
		prop.ForAll(roundTrip(nil), blobSets))
	properties.Property("encrypted pack round-trips any blob set",
		prop.ForAll(roundTrip(crypter), blobSets))
	properties.TestingRun(t)
}
