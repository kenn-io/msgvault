package store

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingLink is a resolved calendar occurrence linked to a meeting note.
type MeetingLink struct {
	Ref     string    `json:"ref"`
	Subject string    `json:"subject,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// MeetingLinksContext returns the owner's resolved links for one visible note.
func (s *Store) MeetingLinksContext(ctx context.Context, messageID int64) ([]MeetingLink, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader(ctx)
	var found int64
	if err := r.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, messageID).Scan(&found); err != nil {
		return nil, err
	}
	budget := calrecur.RequestReadBudget
	occurrences, err := s.meetingMemberOccurrences(ctx, r, messageID, &budget)
	if err != nil {
		return nil, err
	}
	links := make([]MeetingLink, 0, len(occurrences))
	for _, occurrence := range occurrences {
		links = append(links, MeetingLink{Ref: formatMeetingOccurrence(occurrence.Ref), Subject: occurrence.Row.Subject, Start: occurrence.Start, End: occurrence.End})
	}
	return links, nil
}
