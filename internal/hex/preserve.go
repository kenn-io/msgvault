package hex

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/hex/evidence"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

func (imp *Importer) prepareArchive(ctx context.Context, m Meeting, sourceID int64, identifier, email string) (meetingarchive.Snapshot, int64, error) {
	key := "meeting:" + m.Manifest.ID
	records, err := imp.store.MessageMetadataBatch(ctx, sourceID, []string{key})
	if err != nil {
		return meetingarchive.Snapshot{}, 0, err
	}
	if previous, ok := records[key]; ok {
		raw, err := imp.store.GetMessageRawContext(ctx, previous.ID)
		if err != nil {
			return meetingarchive.Snapshot{}, 0, fmt.Errorf("read archived Hex evidence: %w", err)
		}
		stored, err := evidence.Decode(raw)
		if err != nil {
			return meetingarchive.Snapshot{}, 0, fmt.Errorf("decode archived Hex evidence: %w", err)
		}
		if m.TitleSourceInferred && m.Manifest.Title == stored.Meeting.Manifest.Title && m.Manifest.App == stored.Meeting.Manifest.App && m.Manifest.CreatedAtMS == stored.Meeting.Manifest.CreatedAtMS && stored.Meeting.Manifest.TitleSource != "" {
			m.Manifest.TitleSource = stored.Meeting.Manifest.TitleSource
		}
		if (stored.Meeting.HasTranscript() && (m.TranscriptState == "missing" || m.TranscriptPartial)) || (stored.Meeting.TranscriptState == "final" && m.TranscriptState == "rolling") {
			return meetingarchive.Snapshot{}, previous.ID, nil
		}
	}
	snapshot, err := m.ArchiveSnapshot(sourceID, identifier, email)
	return snapshot, 0, err
}
