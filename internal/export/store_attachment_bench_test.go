package export

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/mime"
)

func BenchmarkStoreAttachmentFileNew(b *testing.B) {
	dir := b.TempDir()
	base := bytes.Repeat([]byte("attachment ingest benchmark payload\n"), 4096)
	b.SetBytes(int64(len(base)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		content := append([]byte(nil), base...)
		binary.LittleEndian.PutUint64(content, uint64(i))
		_, err := StoreAttachmentFile(dir, &mime.Attachment{Content: content})
		require.NoError(b, err)
	}
}

func BenchmarkStoreAttachmentFromPathNew(b *testing.B) {
	dir := b.TempDir()
	src := filepath.Join(b.TempDir(), "source.bin")
	content := bytes.Repeat([]byte("path attachment ingest benchmark payload\n"), 4096)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		binary.LittleEndian.PutUint64(content, uint64(i))
		require.NoError(b, os.WriteFile(src, content, 0o600))
		b.StartTimer()
		_, _, _, err := StoreAttachmentFromPath(dir, src, 0)
		require.NoError(b, err)
	}
}

func BenchmarkStoreAttachmentFromPathDuplicate(b *testing.B) {
	dir := b.TempDir()
	src := filepath.Join(b.TempDir(), "source.bin")
	content := bytes.Repeat([]byte("duplicate path attachment ingest payload\n"), 4096)
	require.NoError(b, os.WriteFile(src, content, 0o600))
	storagePath, contentHash, storedSize, err := StoreAttachmentFromPath(dir, src, 0)
	require.NoError(b, err)
	require.NotEmpty(b, storagePath)
	require.NotEmpty(b, contentHash)
	require.Positive(b, storedSize)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _, _, err := StoreAttachmentFromPath(dir, src, 0)
		require.NoError(b, err)
	}
}

func BenchmarkStoreAttachmentFileDuplicate(b *testing.B) {
	dir := b.TempDir()
	content := bytes.Repeat([]byte("duplicate attachment ingest payload\n"), 4096)
	_, err := StoreAttachmentFile(dir, &mime.Attachment{Content: content})
	require.NoError(b, err)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := StoreAttachmentFile(dir, &mime.Attachment{Content: content})
		require.NoError(b, err)
	}
}
