package packcompat_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
)

const benchmarkBlobBytes = 256 << 10
const benchmarkLargeBlobBytes = 32 << 20

func BenchmarkLooseRead(b *testing.B) {
	blobs, hash, content := benchmarkLooseStore(b)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		readBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkLooseStream(b *testing.B) {
	blobs, hash, content := benchmarkLooseStore(b)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		streamBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedWarmRead(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStore(b)
	readBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		readBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedWarmStream(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStore(b)
	streamBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		streamBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedLargeRead(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStoreWithContent(b, benchmarkContentSize(benchmarkLargeBlobBytes))
	readBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		readBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedLargeStream(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStoreWithContent(b, benchmarkContentSize(benchmarkLargeBlobBytes))
	streamBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		streamBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedLargeRawRead(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStoreWithContent(b, benchmarkNoise(benchmarkLargeBlobBytes))
	readBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		readBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedLargeRawStream(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStoreWithContent(b, benchmarkNoise(benchmarkLargeBlobBytes))
	streamBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		streamBenchmarkBlob(b, blobs, hash)
	}
}

func BenchmarkPackedConcurrentRead(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStore(b)
	readBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	var firstErr error
	var once sync.Once
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			reader, _, err := blobs.Open(hash)
			if err == nil {
				_, err = io.Copy(io.Discard, reader)
				err = errors.Join(err, reader.Close())
			}
			if err != nil {
				once.Do(func() { firstErr = err })
				return
			}
		}
	})
	require.NoError(b, firstErr)
}

func BenchmarkPackedConcurrentStream(b *testing.B) {
	blobs, hash, content, _ := benchmarkPackedStore(b)
	streamBenchmarkBlob(b, blobs, hash)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	var firstErr error
	var once sync.Once
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			reader, _, err := blobs.OpenStream(context.Background(), hash)
			if err == nil {
				_, err = io.Copy(io.Discard, reader)
				err = errors.Join(err, reader.Close())
			}
			if err != nil {
				once.Do(func() { firstErr = err })
				return
			}
		}
	})
	require.NoError(b, firstErr)
}

func BenchmarkBackupPackedCapture(b *testing.B) {
	blobs, hash, content, dir := benchmarkPackedStore(b)
	source := backupapp.NewContentSource(blobs, dir)
	ref := backup.ContentRef{Hash: hash, Size: int64(len(content)), StoragePath: hash[:2] + "/" + hash}
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		reader, err := source.Open(context.Background(), ref)
		require.NoError(b, err)
		_, readErr := io.Copy(io.Discard, reader)
		require.NoError(b, errors.Join(readErr, reader.Close()))
	}
}

func benchmarkLooseStore(b *testing.B) (*attachmentstore.Store, string, []byte) {
	b.Helper()
	dir := b.TempDir()
	content := benchmarkContent()
	hash := benchmarkHash(content)
	require.NoError(b, os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
	require.NoError(b, os.WriteFile(filepath.Join(dir, hash[:2], hash), content, 0o600))
	parsed, err := packstore.ParseHash(hash)
	require.NoError(b, err)
	blobs, err := attachmentstore.New(benchmarkResolver{parsed: {Member: true}}, dir)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, blobs.Close()) })
	return blobs, hash, content
}

func benchmarkPackedStore(b *testing.B) (*attachmentstore.Store, string, []byte, string) {
	b.Helper()
	return benchmarkPackedStoreWithContent(b, benchmarkContent())
}

func benchmarkPackedStoreWithContent(b *testing.B, content []byte) (*attachmentstore.Store, string, []byte, string) {
	b.Helper()
	dir := b.TempDir()
	hash := benchmarkHash(content)
	writer, err := pack.NewWriter(b.TempDir(), pack.WriterOptions{})
	require.NoError(b, err)
	_, err = writer.Append(content)
	require.NoError(b, err)
	packID := writer.ID()
	path := filepath.Join(dir, "packs", packID[:2], packID+packstore.PackExt)
	require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o700))
	entries, err := writer.Seal(path)
	require.NoError(b, err)
	require.Len(b, entries, 1)
	entry := entries[0]
	parsed, err := packstore.ParseHash(hash)
	require.NoError(b, err)
	indexEntry := packstore.IndexEntry{Hash: parsed, PackID: packID, Offset: int64(entry.Offset),
		StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
		Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	blobs, err := attachmentstore.New(benchmarkResolver{parsed: {Member: true, Pack: &indexEntry}}, dir)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, blobs.Close()) })
	return blobs, hash, content, dir
}

type benchmarkResolver map[packstore.Hash]packstore.Location

func (r benchmarkResolver) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	return r[hash], nil
}

func benchmarkContent() []byte {
	return benchmarkContentSize(benchmarkBlobBytes)
}

func benchmarkContentSize(size int) []byte {
	block := []byte("msgvault packed CAS performance compatibility payload\n")
	return bytes.Repeat(block, size/len(block)+1)[:size]
}

func benchmarkNoise(size int) []byte {
	content := make([]byte, size)
	var state uint32 = 1
	for i := range content {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		content[i] = byte(state)
	}
	return content
}

func benchmarkHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func readBenchmarkBlob(b *testing.B, blobs *attachmentstore.Store, hash string) {
	b.Helper()
	reader, _, err := blobs.Open(hash)
	require.NoError(b, err)
	_, readErr := io.Copy(io.Discard, reader)
	require.NoError(b, errors.Join(readErr, reader.Close()))
}

func streamBenchmarkBlob(b *testing.B, blobs *attachmentstore.Store, hash string) {
	b.Helper()
	reader, _, err := blobs.OpenStream(context.Background(), hash)
	require.NoError(b, err)
	_, readErr := io.Copy(io.Discard, reader)
	require.NoError(b, errors.Join(readErr, reader.Close()))
}
