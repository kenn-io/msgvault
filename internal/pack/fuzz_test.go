package pack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzOpenReader feeds arbitrary bytes to the full open-and-read path. Any
// panic is a bug; errors are the expected outcome for damaged input.
//
// The pack ID is itself a fuzz argument because encrypted packs bind their
// footer AAD to the ID derived from the filename (see OpenReader): a fixed
// filename would let the encrypted seed's AAD never match, so the encrypted
// read path (Entries/ReadBlob/VerifyStored) would never be reached.
func FuzzOpenReader(f *testing.F) {
	const fixedID = "01hzzzzzzzzzzzzzzzzzzzzzzz"

	// Seed with a valid plain pack, a valid encrypted pack (each under its
	// real pack ID so the encrypted seed's AAD matches), and edge shapes.
	c, err := NewCrypter(testKey(31))
	require.NoError(f, err)
	for _, crypter := range []*Crypter{nil, c} {
		dir := f.TempDir()
		w, err := NewWriter(dir, WriterOptions{Crypter: crypter})
		require.NoError(f, err)
		_, err = w.Append([]byte("seed blob one"))
		require.NoError(f, err)
		_, err = w.Append(make([]byte, 4096))
		require.NoError(f, err)
		path := filepath.Join(dir, w.ID()+".mvpack")
		_, err = w.Seal(path)
		require.NoError(f, err)
		data, err := os.ReadFile(path)
		require.NoError(f, err)
		f.Add(data, w.ID())
	}
	f.Add([]byte{}, fixedID)
	f.Add([]byte("MVPK\x01\x00"), fixedID)

	key := testKey(31)
	f.Fuzz(func(t *testing.T, data []byte, id string) {
		if !IsValidPackID(id) {
			t.Skip()
		}
		dir := t.TempDir()
		path := filepath.Join(dir, id+".mvpack")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip()
		}
		crypter, err := NewCrypter(key)
		if err != nil {
			t.Skip()
		}
		for _, c := range []*Crypter{nil, crypter} {
			r, err := OpenReader(path, c)
			if err != nil {
				continue
			}
			defer func() { _ = r.Close() }()
			for _, e := range r.Entries() {
				_, _ = r.ReadBlob(e)
				_ = r.VerifyStored(e)
			}
		}
	})
}

// FuzzParseFooterRegion targets the footer table parser directly.
func FuzzParseFooterRegion(f *testing.F) {
	f.Add(encodeFooterRegion(nil), uint64(6))
	f.Add(encodeFooterRegion([]Entry{{ID: ComputeBlobID([]byte("x")),
		Offset: 6, StoredLen: 1, RawLen: 1}}), uint64(7))
	f.Fuzz(func(t *testing.T, region []byte, footerStart uint64) {
		_, _ = parseFooterRegion(region, footerStart)
	})
}
