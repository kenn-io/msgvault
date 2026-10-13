package inboxcontrol

import "context"

// TriageSnapshot binds explicit committed inbox targets and owner mappings in
// one archive snapshot. IncomingWatermark excludes the daemon's own tag writes.
type TriageSnapshot struct {
	Source            SourceIdentity    `json:"source"`
	Mappings          map[string]string `json:"mappings"`
	MappingRevision   int64             `json:"mapping_revision"`
	ArchiveRevision   string            `json:"archive_revision"`
	IncomingWatermark string            `json:"incoming_watermark"`
	Candidates        []Candidate       `json:"candidates"`
}

type TriageSnapshotStore interface {
	InboxTriageSnapshot(ctx context.Context, source SourceIdentity, targets []Target) (*TriageSnapshot, error)
}
