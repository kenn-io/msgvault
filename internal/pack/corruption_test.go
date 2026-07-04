package pack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openAndReadAll opens the pack and reads every blob, returning the first
// error encountered. It must never panic regardless of file damage.
func openAndReadAll(path string, c *Crypter) error {
	r, err := OpenReader(path, c)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, e := range r.Entries() {
		if _, err := r.ReadBlob(e); err != nil {
			return err
		}
	}
	return nil
}

func TestCorruptionMatrix(t *testing.T) {
	c, err := NewCrypter(testKey(11))
	require.NoError(t, err)

	for _, mode := range []struct {
		name    string
		crypter *Crypter
	}{
		{name: "plain", crypter: nil},
		{name: "encrypted", crypter: c},
	} {
		path, entries := buildTestPack(t, testBlobs(t), mode.crypter)
		pristine, err := os.ReadFile(path)
		require.NoError(t, err)
		blobStart := int(entries[0].Offset)
		footerStart := int(entries[len(entries)-1].Offset +
			entries[len(entries)-1].StoredLen)

		cases := []struct {
			name   string
			mutate func([]byte) []byte
		}{
			{"flip header magic", flipAt(0)},
			{"flip version byte", flipAt(4)},
			{"flip first blob byte", flipAt(blobStart)},
			{"flip footer byte", flipAt(footerStart)},
			{"flip last byte",
				func(b []byte) []byte { return flipAt(len(b) - 1)(b) }},
			{"truncate to header",
				func(b []byte) []byte { return b[:headerSize] }},
			{"truncate mid blob",
				func(b []byte) []byte { return b[:blobStart+1] }},
			{"truncate mid footer",
				func(b []byte) []byte { return b[:footerStart+1] }},
			{"truncate one byte",
				func(b []byte) []byte { return b[:len(b)-1] }},
			{"empty file", func([]byte) []byte { return nil }},
		}
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				damaged := tc.mutate(append([]byte(nil), pristine...))
				p := filepath.Join(t.TempDir(), filepath.Base(path))
				require.NoError(t, os.WriteFile(p, damaged, 0o600))
				// Must return an error and must not panic. Which typed error
				// depends on the damage site; all are acceptable failures.
				assert.Error(t, openAndReadAll(p, mode.crypter))
			})
		}
	}
}

func flipAt(i int) func([]byte) []byte {
	return func(b []byte) []byte {
		if i < len(b) {
			b[i] ^= 0x01
		}
		return b
	}
}
