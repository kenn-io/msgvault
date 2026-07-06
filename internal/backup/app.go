package backup

import (
	"context"
	"database/sql"
	"encoding/json"
)

// ContentInfo is what the engine needs to know about the application's
// content-addressed files, computed inside the frozen snapshot.
type ContentInfo struct {
	Refs []ContentRef // one per unique hash, first-seen order
	Rows int64        // DB rows referencing content (manifest attachments.rows)
	// NonCanonicalPaths reports any ref recorded at a path other than the
	// canonical "<hash[:2]>/<hash>" layout; such snapshots require a
	// path-aware restore and a higher manifest reader version.
	NonCanonicalPaths bool
}

// FrozenView answers the application-schema questions Create asks, against
// the pinned read transaction of a FrozenSession.
type FrozenView interface {
	ContentInfo(ctx context.Context) (*ContentInfo, error)
	Stats(ctx context.Context) (json.RawMessage, error)
}

// App supplies every application-specific behavior the engine needs. The
// engine treats stats payloads as opaque bytes: it records them at create
// and byte-compares them at restore.
type App interface {
	FrozenView(s *FrozenSession) FrozenView
	DBFileName() string     // e.g. "msgvault.db"
	ContentDirName() string // e.g. "attachments"
	// RestoredContentPaths re-derives hash → relative paths from a restored
	// DB so restore can materialize and verify every referenced file.
	RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error)
	// RestoredStats recomputes stats from a restored DB for the fidelity proof.
	RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error)
	// CheckManifest returns app-level manifest consistency problems (verify).
	CheckManifest(m *Manifest) []string
	ExcludedPaths() []string
	Version() string // recorded as the manifest's app version
}
