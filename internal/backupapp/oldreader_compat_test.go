package backupapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

// --- Frozen copy of the pre-extraction manifest reader. DO NOT UPDATE. ---
//
// This is a verbatim copy of the type definitions and ComputeSnapshotID from
// internal/backup/manifest.go as they exist before the backup engine
// generalization. It exists to prove that manifests written by the
// generalized engine remain byte-compatible with a reader that only knows
// about today's wire format. Never edit these types to track future changes
// in manifest.go -- doing so would defeat the entire point of this test.

type oldManifest struct {
	FormatVersion    int                    `json:"format_version"`
	MinReaderVersion int                    `json:"min_reader_version"`
	MsgvaultVersion  string                 `json:"msgvault_version"`
	SnapshotID       string                 `json:"snapshot_id"`
	ParentID         string                 `json:"parent_id"`
	CreatedAt        string                 `json:"created_at"`
	Options          oldManifestOptions     `json:"options"`
	DB               oldManifestDB          `json:"db"`
	Attachments      oldManifestAttachments `json:"attachments"`
	Extras           oldManifestExtras      `json:"extras"`
	Excluded         []string               `json:"excluded"`
	Stats            oldManifestStats       `json:"stats"`
	NewPacks         []string               `json:"new_packs"`
	NewIndex         string                 `json:"new_index"`
	DurationSeconds  float64                `json:"duration_seconds"`
	BytesAdded       int64                  `json:"bytes_added"`
}

type oldManifestOptions struct {
	IncludeConfig bool   `json:"include_config"`
	IncludeTokens bool   `json:"include_tokens"`
	ZstdLevel     int    `json:"zstd_level"`
	Tag           string `json:"tag"`
}

type oldManifestDB struct {
	Engine        string `json:"engine"`
	PageSize      uint32 `json:"page_size"`
	PageCount     uint64 `json:"page_count"`
	PageMap       string `json:"page_map"`
	PageHashMap   string `json:"page_hash_map"`
	MapChainDepth int    `json:"map_chain_depth"`
}

type oldManifestAttachments struct {
	Layout    []string `json:"layout"`
	Rows      int64    `json:"rows"`
	Blobs     int64    `json:"blobs"`
	BlobBytes int64    `json:"blob_bytes"`
	Recipes   []string `json:"recipes"`
	Lists     []string `json:"lists"`
}

type oldManifestExtras struct {
	Tree string `json:"tree"`
}

type oldManifestStats struct {
	Messages        int64     `json:"messages"`
	Conversations   int64     `json:"conversations"`
	Sources         int64     `json:"sources"`
	Accounts        int64     `json:"accounts"`
	AttachmentRows  int64     `json:"attachment_rows"`
	AttachmentBlobs int64     `json:"attachment_blobs"`
	Labels          int64     `json:"labels"`
	DateRange       [2]string `json:"date_range"`
}

// computeOldSnapshotID is a verbatim copy of ComputeSnapshotID
// (internal/backup/manifest.go:87-96) operating on *oldManifest.
func computeOldSnapshotID(createdAt time.Time, m *oldManifest) (string, error) {
	cp := *m
	cp.SnapshotID = ""
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("backup: marshaling manifest for snapshot id: %w", err)
	}
	sum := sha256.Sum256(data)
	return createdAt.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(sum[:16]), nil
}

// --- End frozen copy. ---

// TestNewManifestReadableByOldReader proves a manifest written by the
// current code is byte-compatible with the pre-extraction reader:
//  1. strict decode (DisallowUnknownFields) -- no added/renamed keys;
//  2. re-marshal equality -- no removed keys, no order changes;
//  3. old snapshot-ID recomputation matches -- the forgery check old
//     readers run on load still passes.
func TestNewManifestReadableByOldReader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	archive := t.TempDir()
	dbPath, attDir := seedCompatArchive(t, archive)
	r, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	m, err := backup.Create(context.Background(), r, backupapp.New("golden-test"), backup.CreateOptions{
		DBPath:     dbPath,
		ContentDir: attDir,
		DataDir:    archive,
	})
	require.NoError(err)

	raw, err := os.ReadFile(
		filepath.Join(r.Root(), "snapshots", m.SnapshotID+".mvmanifest"))
	require.NoError(err)

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var old oldManifest
	require.NoError(dec.Decode(&old),
		"new manifest has fields the old reader does not know")

	remarshaled, err := json.MarshalIndent(&old, "", "  ")
	require.NoError(err)
	assert.Equal(string(raw), string(remarshaled),
		"old reader re-marshal differs: key set, order, or encoding changed")

	// The old reader gates on min_reader_version before anything else
	// (LoadManifest refuses manifests requiring a newer reader). Freeze the
	// version the pre-extraction reader supported: a new writer must not
	// emit manifests old readers would refuse.
	const oldSupportedReaderVersion = 2
	assert.LessOrEqual(old.MinReaderVersion, oldSupportedReaderVersion,
		"old reader would refuse this manifest: min_reader_version too new")

	// The old reader's forgery check compares the embedded snapshot_id to
	// the recomputed one; both must also match the manifest's filename ID.
	assert.Equal(m.SnapshotID, old.SnapshotID,
		"embedded snapshot_id differs from the ID the manifest was written under")
	createdAt, err := time.Parse(time.RFC3339, old.CreatedAt)
	require.NoError(err)
	oldID, err := computeOldSnapshotID(createdAt, &old)
	require.NoError(err)
	assert.Equal(m.SnapshotID, oldID,
		"old reader would reject this manifest as forged/corrupt")
}
