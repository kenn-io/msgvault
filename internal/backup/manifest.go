package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
)

// Manifest types (docs/architecture/backup-format.md)

type Manifest struct {
	FormatVersion    int                 `json:"format_version"`
	MinReaderVersion int                 `json:"min_reader_version"`
	MsgvaultVersion  string              `json:"msgvault_version"`
	SnapshotID       string              `json:"snapshot_id"`
	ParentID         string              `json:"parent_id"`
	CreatedAt        string              `json:"created_at"`
	Options          ManifestOptions     `json:"options"`
	DB               ManifestDB          `json:"db"`
	Attachments      ManifestAttachments `json:"attachments"`
	Extras           ManifestExtras      `json:"extras"`
	Excluded         []string            `json:"excluded"`
	Stats            ManifestStats       `json:"stats"`
	NewPacks         []string            `json:"new_packs"`
	NewIndex         string              `json:"new_index"`
	DurationSeconds  float64             `json:"duration_seconds"`
	BytesAdded       int64               `json:"bytes_added"`
}

type ManifestOptions struct {
	IncludeConfig bool   `json:"include_config"`
	IncludeTokens bool   `json:"include_tokens"`
	ZstdLevel     int    `json:"zstd_level"`
	Tag           string `json:"tag"`
}

type ManifestDB struct {
	Engine        string `json:"engine"`
	PageSize      uint32 `json:"page_size"`
	PageCount     uint64 `json:"page_count"`
	PageMap       string `json:"page_map"`
	PageHashMap   string `json:"page_hash_map"`
	MapChainDepth int    `json:"map_chain_depth"`
}

type ManifestAttachments struct {
	Layout    []string `json:"layout"`
	Rows      int64    `json:"rows"`
	Blobs     int64    `json:"blobs"`
	BlobBytes int64    `json:"blob_bytes"`
	Recipes   []string `json:"recipes"`
	Lists     []string `json:"lists"`
}

type ManifestExtras struct {
	Tree string `json:"tree"`
}

type ManifestStats struct {
	Messages        int64     `json:"messages"`
	Conversations   int64     `json:"conversations"`
	Sources         int64     `json:"sources"`
	Accounts        int64     `json:"accounts"`
	AttachmentRows  int64     `json:"attachment_rows"`
	AttachmentBlobs int64     `json:"attachment_blobs"`
	Labels          int64     `json:"labels"`
	DateRange       [2]string `json:"date_range"`
}

const manifestExt = ".mvmanifest"

// ComputeSnapshotID derives the time-ordered, content-derived snapshot ID
// (docs/architecture/backup-format.md): UTC timestamp plus the first 32 hex
// chars (128 bits) of the SHA-256 of the manifest JSON with snapshot_id
// blanked. The digest must be long enough that crafting a different
// manifest with the same ID is infeasible: LoadManifest's recompute check
// is what stops a forged manifest from being served under a known snapshot
// ID, and a short, brute-forceable suffix would defeat it.
func ComputeSnapshotID(createdAt time.Time, m *Manifest) (string, error) {
	cp := *m
	cp.SnapshotID = ""
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("backup: marshaling manifest for snapshot id: %w", err)
	}
	sum := sha256.Sum256(data)
	return createdAt.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(sum[:16]), nil
}

// WriteManifest fills the snapshot ID and publishes the manifest. It must be
// the final write of a backup: a manifest's existence asserts closure.
func (r *Repo) WriteManifest(m *Manifest) (string, error) {
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return "", fmt.Errorf("backup: manifest created_at %q: %w", m.CreatedAt, err)
	}
	id, err := ComputeSnapshotID(createdAt, m)
	if err != nil {
		return "", err
	}
	m.SnapshotID = id
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("backup: marshaling manifest: %w", err)
	}
	if err := writeFileAtomic(r, filepath.Join(snapshotsDirName, id+manifestExt), data); err != nil {
		return "", err
	}
	return id, nil
}

// LoadManifest reads one manifest by snapshot ID.
func (r *Repo) LoadManifest(id string) (*Manifest, error) {
	data, err := os.ReadFile(r.Path(snapshotsDirName, id+manifestExt))
	if err != nil {
		return nil, fmt.Errorf("backup: loading snapshot %s: %w", id, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("backup: parsing snapshot %s: %w", id, err)
	}
	// Unknown JSON fields are silently ignored above, so a manifest written
	// by a newer format could otherwise be misread as current (e.g. an
	// encrypted snapshot treated as plaintext). The per-snapshot
	// min_reader_version gate turns that into an explicit refusal. It must
	// run before the ID check below: a newer manifest's dropped fields would
	// fail the ID recomputation with a misleading corruption error.
	if m.MinReaderVersion > SupportedReaderVersion {
		return nil, fmt.Errorf(
			"backup: snapshot %s requires reader version %d but this "+
				"msgvault supports %d; upgrade msgvault",
			id, m.MinReaderVersion, SupportedReaderVersion)
	}
	// The snapshot ID is content-derived, so recomputing it authenticates
	// every manifest field against the filename. Without this, corrupted or
	// hand-edited manifest metadata would be accepted by list, latest, and
	// verify.
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot %s created_at %q: %w", id, m.CreatedAt, err)
	}
	computed, err := ComputeSnapshotID(createdAt, &m)
	if err != nil {
		return nil, err
	}
	if computed != id || m.SnapshotID != id {
		return nil, fmt.Errorf(
			"backup: snapshot %s failed its content-derived ID check "+
				"(computed %s, embedded %q); the manifest file is corrupted, renamed, or forged",
			id, computed, m.SnapshotID)
	}
	return &m, nil
}

// ListSnapshots returns every manifest sorted ascending by snapshot ID
// (IDs are time-prefixed, so this is chronological). Create enforces
// strictly increasing CreatedAt timestamps per repo (see nextCreatedAt in
// create.go), so even snapshots created within the same wall-clock second
// still sort chronologically by ID. Lock-free by design.
func (r *Repo) ListSnapshots() ([]*Manifest, error) {
	entries, err := os.ReadDir(r.Path(snapshotsDirName))
	if err != nil {
		return nil, fmt.Errorf("backup: reading snapshots dir: %w", err)
	}
	var out []*Manifest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), manifestExt) {
			continue
		}
		m, err := r.LoadManifest(strings.TrimSuffix(e.Name(), manifestExt))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SnapshotID < out[j].SnapshotID })
	return out, nil
}

// LatestSnapshot returns the newest manifest, or nil for an empty repo.
func (r *Repo) LatestSnapshot() (*Manifest, error) {
	list, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil //nolint:nilnil // empty repo -> no manifest, not an error
	}
	return list[len(list)-1], nil
}

// HashMapChain collects the newest-to-oldest page-hash-map blob chain from
// head down to (and including) its keyframe manifest.
func (r *Repo) HashMapChain(head *Manifest) ([]pack.BlobID, error) {
	return r.mapChain(head, func(m *Manifest) string { return m.DB.PageHashMap })
}

// PageMapChain collects the newest-to-oldest page-map blob chain.
func (r *Repo) PageMapChain(head *Manifest) ([]pack.BlobID, error) {
	return r.mapChain(head, func(m *Manifest) string { return m.DB.PageMap })
}

func (r *Repo) mapChain(head *Manifest, field func(*Manifest) string) ([]pack.BlobID, error) {
	var chain []pack.BlobID
	m := head
	visited := make(map[string]struct{})
	iterations := 0

	for {
		id, err := pack.ParseBlobID(field(m))
		if err != nil {
			return nil, fmt.Errorf("backup: snapshot %s map blob: %w", m.SnapshotID, err)
		}
		chain = append(chain, id)
		if m.DB.MapChainDepth == 0 {
			return chain, nil
		}

		// Detect cycles. LoadManifest's content-derived ID check makes an
		// on-disk parent cycle infeasible to construct (each ID would have to
		// be a SHA-256 fixed point over the other's), so this is pure
		// defense-in-depth behind the iteration cap below.
		if _, seen := visited[m.SnapshotID]; seen {
			return nil, fmt.Errorf("backup: snapshot chain cycle at %s", m.SnapshotID)
		}
		visited[m.SnapshotID] = struct{}{}

		// Enforce depth limit
		iterations++
		if iterations > keyframeChainMax {
			return nil, fmt.Errorf("backup: snapshot chain exceeds limit of %d", keyframeChainMax)
		}

		if m.ParentID == "" {
			return nil, fmt.Errorf("backup: snapshot %s has chain depth %d but no parent", m.SnapshotID, m.DB.MapChainDepth)
		}
		parent, err := r.LoadManifest(m.ParentID)
		if err != nil {
			return nil, fmt.Errorf("backup: walking map chain: %w", err)
		}
		m = parent
	}
}
