package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// GetMeetingPersonContext keeps person resolution on the request's archive snapshot.
func (s *Store) GetMeetingPersonContext(ctx context.Context, id int64) (*Person, error) {
	var person *Person
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error { var err error; person, err = s.getPersonTx(ctx, tx, id); return err })
	return person, err
}

func (s *Store) ListMeetingPersonContactPointsContext(ctx context.Context, id int64) ([]PersonContactPoint, error) {
	var points []PersonContactPoint
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		points, err = s.listPersonContactPointsTx(ctx, tx, id, true)
		return err
	})
	return points, err
}

func (s *Store) MeetingClusterMembersContext(ctx context.Context, id int64) ([]int64, error) {
	var members []int64
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		edges, err := s.loadLinkEdgesTxContext(ctx, tx)
		if err != nil {
			return err
		}
		for member := range componentOf(id, edges) {
			members = append(members, member)
		}
		slices.Sort(members)
		return nil
	})
	return members, err
}

func (s *Store) MeetingPersonForParticipantsContext(ctx context.Context, ids []int64) (*Person, error) {
	var person *Person
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		people, err := personIDsForParticipantsTx(ctx, tx, ids)
		if err != nil {
			return err
		}
		if len(people) > 1 {
			return newPersonBindingConflict(people)
		}
		if len(people) == 0 {
			return nil
		}
		person, err = s.getPersonTx(ctx, tx, people[0])
		return err
	})
	return person, err
}

func (s *Store) MeetingParticipantEmailsContext(ctx context.Context, ids []int64) ([]string, error) {
	emails := []string{}
	if len(ids) == 0 {
		return emails, nil
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	args := appendInt64Args(nil, normalizedSignedIDs(ids))
	rows, err := s.snapshotReader(ctx).QueryContext(ctx, `SELECT COALESCE(email_address,'') FROM participants WHERE id IN (`+placeholders(len(args))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read meeting person emails: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			emails = append(emails, email)
		}
	}
	slices.Sort(emails)
	return slices.Compact(emails), rows.Err()
}
