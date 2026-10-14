package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

func meetingRecordItemHeaders(ctx context.Context, r rowReader, scope SourceScope, links []EffectiveLink) ([]MeetingItem, error) {
	items := make([]MeetingItem, 0, len(links))
	for _, link := range links {
		clause, args := linkScopeSQL(scope)
		item := MeetingItem{Basis: link.Basis, Origin: link.Origin, Recordings: []MeetingRecording{}}
		var sent nullableTimestamp
		err := r.QueryRowContext(ctx, `SELECT m.id,m.source_id,COALESCE(m.subject,''),COALESCE(m.sent_at,m.received_at,m.internal_date),m.deleted_from_source_at IS NOT NULL FROM messages m WHERE m.id = ? AND m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL`+clause, append([]any{link.MemberMessageID}, args...)...).Scan(&item.MessageID, &item.SourceID, &item.Title, &sent, &item.DeletedFromSource)
		if err != nil {
			return nil, meetingRecordError(err)
		}
		if sent.Valid {
			item.SentAt = sent.Time.UTC()
		}
		item.FollowUp = fmt.Sprintf("/api/v1/messages/%d", item.MessageID)
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b MeetingItem) int {
		if c := a.SentAt.Compare(b.SentAt); c != 0 {
			return c
		}
		if a.MessageID < b.MessageID {
			return -1
		}
		if a.MessageID > b.MessageID {
			return 1
		}
		return 0
	})
	return items, nil
}

func (s *Store) populateMeetingRecordItem(ctx context.Context, item *MeetingItem, full bool) error {
	var entry meetingcontent.Entry
	var archiveUID string
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`, item.MessageID).Scan(&count); err != nil {
			return err
		}
		if count > meetingRecordMaxPeople {
			return ErrQueryTooBroad
		}
		row, err := s.loadMeetingContextRow(ctx, tx, item.MessageID, full)
		if err != nil {
			return err
		}
		participants, err := loadMeetingContextParticipants(ctx, tx, item.MessageID)
		if err != nil {
			return err
		}
		if len(row.content.SourceParticipants) > meetingRecordMaxPeople {
			return ErrQueryTooBroad
		}
		entry = meetingcontent.Entry{Meeting: row.meeting, Content: row.content, Participants: participants}
		archiveUID, err = archiveUIDFromMeetingSnapshot(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	budget := meetingRecordFullBytes
	if !full {
		budget = meetingRecordSummaryBytes
		entry.Content.Notes = meetingcontent.Section{State: meetingcontent.StateOmittedByRequest}
		entry.Content.Transcript = meetingcontent.Transcript{State: meetingcontent.StateOmittedByRequest}
	}
	result, err := meetingcontent.Render(archiveUID, []meetingcontent.Entry{entry}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: full, MaxBytes: budget})
	if err != nil {
		return err
	}
	item.Content, err = meetingcontent.PacketFromResult(result)
	if err != nil {
		return err
	}
	item.ContentOmitted = !full || len(item.Content.Meetings) == 0
	rows, err := s.snapshotReader(ctx).QueryContext(ctx, `SELECT id,COALESCE(mime_type,''),COALESCE(size,0),duration_ms FROM attachments WHERE message_id = ? AND (LOWER(COALESCE(mime_type,'')) LIKE 'audio/%' OR LOWER(COALESCE(mime_type,'')) LIKE 'video/%' OR COALESCE(media_type,'') IN ('audio','video','voice_note')) ORDER BY id LIMIT 101`, item.MessageID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var recording MeetingRecording
		var duration sql.NullFloat64
		if err := rows.Scan(&recording.AttachmentID, &recording.MimeType, &recording.Size, &duration); err != nil {
			return err
		}
		if duration.Valid {
			seconds := duration.Float64 / 1000
			recording.DurationSeconds = &seconds
		}
		item.Recordings = append(item.Recordings, recording)
	}
	if len(item.Recordings) > meetingRecordMaxRecordings {
		return ErrQueryTooBroad
	}
	return rows.Err()
}

func appendMeetingRecordEvidence(record *MeetingRecord, item MeetingItem) {
	if item.Content == nil {
		return
	}
	seen := map[string]bool{}
	for _, p := range record.Participants {
		seen[meetingRecordParticipantKey(p)] = true
	}
	appendParticipant := func(p MeetingParticipant) {
		key := meetingRecordParticipantKey(p)
		if !seen[key] {
			seen[key] = true
			record.Participants = append(record.Participants, p)
		}
	}
	for _, entry := range item.Content.Meetings {
		for _, participant := range entry.Participants {
			p := MeetingParticipant{Name: participant.Name, Email: participant.Email, Phone: participant.Phone, Provenance: participant.Provenance}
			if participant.ParticipantID != nil {
				p.ParticipantID = *participant.ParticipantID
			}
			if p.Provenance == "" {
				p.Provenance = "provider_attendee"
			}
			appendParticipant(p)
		}
		for _, segment := range entry.Content.Transcript.Segments {
			if speaker := strings.TrimSpace(segment.Speaker); speaker != "" {
				appendParticipant(MeetingParticipant{Name: speaker, Provenance: "transcript_speaker"})
			}
		}
		for _, action := range entry.Content.Actions {
			record.ActionItems = append(record.ActionItems, meetingcontent.ActionRow{Meeting: entry.Meeting, Action: action})
		}
	}
}

func meetingRecordParticipantKey(p MeetingParticipant) string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", p.ParticipantID, strings.ToLower(p.Email), p.Phone, p.Name, p.Provenance)
}
