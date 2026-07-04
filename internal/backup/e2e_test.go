package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

func materializeDB(t *testing.T, r *Repo, m *Manifest) []byte {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	fetch := func(id pack.BlobID) ([]byte, error) { return r.ReadBlob(known, id, nil) }
	chain, err := r.PageMapChain(m)
	require.NoError(t, err)
	pm, err := MaterializePageMap(fetch, chain)
	require.NoError(t, err)
	require.NoError(t, pm.CheckCoverage())
	require.Equal(t, m.DB.PageCount, pm.PageCount)

	blobCache := map[pack.BlobID][]byte{}
	out := make([]byte, pm.PageCount*uint64(pm.PageSize))
	for page := range pm.PageCount {
		id, off, err := pm.Lookup(page)
		require.NoError(t, err)
		blob, ok := blobCache[id]
		if !ok {
			blob, err = fetch(id)
			require.NoError(t, err)
			blobCache[id] = blob
		}
		copy(out[page*uint64(pm.PageSize):], blob[off:off+uint64(pm.PageSize)])
	}
	return out
}

func snapshotDBFile(t *testing.T, dbPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	return data
}

func TestBackupChainEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()
	opts := createOpts(dbPath, attachmentsDir, dataDir, cacheDir)

	// Snapshot 1.
	m1, err := Create(ctx, r, opts)
	require.NoError(err)
	img1 := snapshotDBFile(t, dbPath) // WAL was truncated by the freeze protocol

	// Mutate the archive: rows and a new attachment.
	refC := writeLooseAttachment(t, attachmentsDir, []byte("post-snapshot attachment"))
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-05-01T00:00:00Z')`)
	require.NoError(err)
	_, err = db.Exec(
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', '')`,
		refC.Hash, refC.Hash[:2]+"/"+refC.Hash, refC.Size)
	require.NoError(err)

	// Snapshot 2 (incremental).
	m2, err := Create(ctx, r, opts)
	require.NoError(err)
	img2 := snapshotDBFile(t, dbPath)
	assert.Equal(m1.SnapshotID, m2.ParentID)

	// Byte-identical materialization of BOTH snapshots (spec 11 restore proof).
	assert.Equal(img1, materializeDB(t, r, m1), "snapshot 1 materializes byte-identically")
	assert.Equal(img2, materializeDB(t, r, m2), "snapshot 2 materializes byte-identically")

	// Full verify across all snapshots is clean.
	res, err := Verify(ctx, r, VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(res.Problems)
	assert.Equal([]string{m1.SnapshotID, m2.SnapshotID}, res.Snapshots)

	// Attachment content round-trips from the repo by content hash.
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	blobID, err := pack.ParseBlobID(refC.Hash)
	require.NoError(err)
	content, err := r.ReadBlob(known, blobID, nil)
	require.NoError(err)
	assert.Equal([]byte("post-snapshot attachment"), content)

	// Crash debris: a leftover staging file disappears on the next create.
	debris := filepath.Join(r.Path("staging"), "crash-leftover.tmp")
	require.NoError(os.WriteFile(debris, []byte("junk"), 0o600))
	_, err = db.Exec(`INSERT INTO messages (sent_at) VALUES ('2026-06-01T00:00:00Z')`)
	require.NoError(err)
	m3, err := Create(ctx, r, opts)
	require.NoError(err)
	assert.Equal(m2.SnapshotID, m3.ParentID)
	_, statErr := os.Stat(debris)
	assert.True(os.IsNotExist(statErr))

	// Corrupt one byte in a snapshot-2 pack: verify names the affected
	// snapshots and the pack. (Done after m3 so the corruption cannot break
	// Create itself — Create reads parent list/map blobs from packs.)
	require.NotEmpty(m2.NewPacks)
	packID := m2.NewPacks[0]
	packPath := r.Path("packs", packID[:2], packID+".mvpack")
	data, err := os.ReadFile(packPath)
	require.NoError(err)
	data[len(data)/2] ^= 0x01
	require.NoError(os.WriteFile(packPath, data, 0o600))

	res, err = Verify(ctx, r, VerifyOptions{All: true})
	require.NoError(err)
	require.NotEmpty(res.Problems)

	// At least one Problem must actually identify the pack this test
	// corrupted, not just report some unspecified damage.
	namesCorruptPack := false
	for _, p := range res.Problems {
		if strings.Contains(p.Detail, packID) {
			namesCorruptPack = true
			break
		}
	}
	assert.True(namesCorruptPack, "a problem must name the corrupted pack %s; got %+v", packID, res.Problems)

	// Snapshot 1 cannot reference m2's new pack (m2.NewPacks was created
	// strictly after m1), so no problem may name m1's snapshot ID, and m1
	// must verify clean on its own — unconditionally, not just when nothing
	// happens to mention it.
	for _, p := range res.Problems {
		assert.NotEqual(m1.SnapshotID, p.SnapshotID,
			"snapshot 1 cannot reference m2's new pack and must not be named by a problem")
	}
	res1, err := Verify(ctx, r, VerifyOptions{SnapshotID: m1.SnapshotID})
	require.NoError(err)
	assert.Empty(res1.Problems, "undamaged snapshot verifies clean")
}
