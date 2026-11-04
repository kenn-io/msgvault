package hex

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/hex/evidence"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

func (imp *Importer) prepareArchive(ctx context.Context, m Meeting, sourceID int64, identifier, email string) (meetingarchive.Snapshot, int64, error) {
	key := "meeting:" + m.Manifest.ID
	records, err := imp.store.MessageMetadataBatch(sourceID, []string{key})
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
		if (stored.Meeting.HasTranscript() && (m.TranscriptState == "missing" || m.TranscriptState == "unreadable")) || (stored.Meeting.TranscriptState == "final" && m.TranscriptState == "rolling") {
			return meetingarchive.Snapshot{}, previous.ID, nil
		}
	}
	snapshot, err := m.ArchiveSnapshot(sourceID, identifier, email)
	return snapshot, 0, err
}
