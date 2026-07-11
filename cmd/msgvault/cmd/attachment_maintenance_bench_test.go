package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

type attachmentMaintenanceBenchmark struct {
	b           *testing.B
	store       *store.Store
	dir         string
	maintenance *attachmentMaintenance
	messageID   int64
	sequence    int
}

func newAttachmentMaintenanceBenchmark(b *testing.B) *attachmentMaintenanceBenchmark {
	b.Helper()
	root := b.TempDir()
	s, err := store.OpenForTest(filepath.Join(root, "archive.db"))
	require.NoError(b, err)
	require.NoError(b, s.InitSchema())
	source, err := s.GetOrCreateSource("gmail", "benchmark@example.com")
	require.NoError(b, err)
	conversationID, err := s.EnsureConversation(source.ID, "benchmark-thread", "Benchmark")
	require.NoError(b, err)
	messageID, err := s.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "benchmark-message",
		MessageType:     "email",
	})
	require.NoError(b, err)
	dir := filepath.Join(root, "attachments")
	maintenance, err := newAttachmentMaintenance(s, dir, nil)
	require.NoError(b, err)
	b.Cleanup(func() {
		require.NoError(b, maintenance.close())
		require.NoError(b, s.Close())
	})
	return &attachmentMaintenanceBenchmark{
		b: b, store: s, dir: dir, messageID: messageID, maintenance: maintenance,
	}
}

func (f *attachmentMaintenanceBenchmark) addLoose(content []byte) string {
	f.b.Helper()
	f.sequence++
	hash := fmt.Sprintf("%x", sha256Bytes(content))
	path := filepath.Join(f.dir, hash[:2], hash)
	require.NoError(f.b, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(f.b, os.WriteFile(path, content, 0o600))
	require.NoError(f.b, f.store.UpsertAttachment(
		f.messageID, fmt.Sprintf("blob-%d.bin", f.sequence),
		"application/octet-stream", hash[:2]+"/"+hash, hash, len(content),
	))
	return hash
}

func BenchmarkAttachmentMaintenancePack(b *testing.B) {
	b.Helper()
	b.ReportAllocs()
	for i := range b.N {
		b.StopTimer()
		fixture := newAttachmentMaintenanceBenchmark(b)
		addMaintenanceBenchmarkPackCorpus(fixture, i)
		require.NoError(b, os.MkdirAll(filepath.Join(fixture.dir, "packs"), 0o700))
		b.StartTimer()
		stats, err := fixture.maintenance.pack(context.Background(), 0)
		b.StopTimer()
		require.NoError(b, err)
		require.Equal(b, 259, stats.BlobsPacked)
	}
}

func addMaintenanceBenchmarkPackCorpus(fixture *attachmentMaintenanceBenchmark, iteration int) {
	for small := range 256 {
		fixture.addLoose(maintenanceBenchmarkBytes(4<<10, uint64(iteration*1000+small+1)))
	}
	fixture.addLoose(maintenanceBenchmarkBytes(1<<20, uint64(iteration*1000+301)))
	fixture.addLoose(maintenanceBenchmarkBytes(8<<20, uint64(iteration*1000+302)))
	fixture.addLoose(maintenanceBenchmarkBytes(64<<20, uint64(iteration*1000+303)))
}

func BenchmarkAttachmentMaintenanceRepack(b *testing.B) {
	b.ReportAllocs()
	for i := range b.N {
		b.StopTimer()
		fixture := newAttachmentMaintenanceBenchmark(b)
		liveHash := fixture.addLoose(maintenanceBenchmarkBytes(1<<20, uint64(i*1000+401)))
		deadHash := fixture.addLoose(maintenanceBenchmarkBytes((8<<20)+(256<<10), uint64(i*1000+402)))
		deadSmallHash := fixture.addLoose(maintenanceBenchmarkBytes(4<<10, uint64(i*1000+403)))
		_, err := fixture.maintenance.pack(context.Background(), 0)
		require.NoError(b, err)
		entry, err := fixture.store.GetAttachmentPackEntry(liveHash)
		require.NoError(b, err)
		require.NotNil(b, entry)
		_, err = fixture.store.DB().Exec(fixture.store.Rebind(
			`DELETE FROM attachments WHERE content_hash IN (?, ?)`), deadHash, deadSmallHash)
		require.NoError(b, err)
		_, err = fixture.store.DB().Exec(fixture.store.Rebind(
			`UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
			time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339), entry.PackID)
		require.NoError(b, err)
		b.StartTimer()
		stats, err := fixture.maintenance.repack(context.Background(), 0)
		b.StopTimer()
		require.NoError(b, err)
		require.Equal(b, 1, stats.BlobsRepacked)
	}
}

func maintenanceBenchmarkBytes(size int, seed uint64) []byte {
	data := make([]byte, size)
	state := seed*0x9e3779b97f4a7c15 + 0xd1b54a32d192ed03
	for offset := 0; offset+8 <= len(data); offset += 8 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(data[offset:], state)
	}
	return data
}

func sha256Bytes(content []byte) [32]byte {
	return sha256.Sum256(content)
}
