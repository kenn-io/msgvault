package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

// LogicalMeetingKeysContext resolves transcript identities under owner scope.
func (s *Store) LogicalMeetingKeysContext(ctx context.Context, ids []int64) (map[int64]string, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.ExportLogicalMeetingKeysContext(ctx, ReadDBContext(ctx, s.db.DB), ids)
}

// ExportLogicalMeetingKeysContext is an owner-only cache export helper. The
// caller owns a pinned reader from the same archive and must keep it alive.
// At most 100 IDs are accepted; each member gets its own recurrence budget.
func (s *Store) ExportLogicalMeetingKeysContext(ctx context.Context, reader SQLReader, ids []int64) (map[int64]string, error) {
	if len(ids) > 100 {
		return nil, errors.New("export meeting keys: batch exceeds 100 messages")
	}
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, ctx.Err()
	}
	r := reboundReader{reader: reader, rebind: s.dialect.Rebind}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	members, err := meetingCountMembers(ctx, r, args)
	if err != nil {
		return nil, fmt.Errorf("read transcript key batch: %w", err)
	}
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		key, err := logicalMeetingKey(ctx, r, OwnerScope(), id, &budget)
		if err != nil {
			return nil, fmt.Errorf("resolve meeting key for message %d: %w", id, err)
		}
		out[id] = key
	}
	return out, ctx.Err()
}

func meetingCountMembers(ctx context.Context, reader SQLReader, args []any) (members []int64, err error) {
	rows, err := reader.QueryContext(ctx, `SELECT id FROM messages WHERE message_type='meeting_transcript' AND deleted_at IS NULL AND id IN (`+placeholders(len(args))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		members = append(members, id)
	}
	return members, rows.Err()
}
