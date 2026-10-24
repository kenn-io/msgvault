package store

import (
	"context"
	"go.kenn.io/msgvault/internal/personobs"
)

func (s *Store) DeletePersonObservationForTest(ctx context.Context, id int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error { return s.deletePersonObservationTx(ctx, tx, id) })
}
func (s *Store) DecideObservationSuppressionForTest(ctx context.Context, personID int64, key string, c SuppressionCandidate) (SuppressionVerdict, error) {
	var verdict SuppressionVerdict
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonMutationTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		verdict, err = s.DecideSuppressionTx(ctx, tx, personID, key, c)
		return err
	})
	return verdict, err
}

func (s *Store) RejectObservationForTest(ctx context.Context, id int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		row, err := s.loadObservationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.lockPersonMutationTx(ctx, tx, row.PersonID); err != nil {
			return err
		}
		if err := s.transitionObservationTx(ctx, tx, id, personobs.StatusRejected, personobs.StatusReasonOwnerRejected, "rejected", "owner", ""); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor) VALUES(?,?,'owner-rejected','owner') ON CONFLICT DO NOTHING`, row.PersonID, row.ContentKey)
		if err != nil {
			return err
		}
		return s.notifyObservationSupportChangedTx(ctx, tx, row.PersonID, []int64{id}, "rejected")
	})
}
