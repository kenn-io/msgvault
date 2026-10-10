package store

import "context"

// LoadCardDAVPublicationReviewSourceAuthorizedContext binds the returned
// rendering inputs and grant selection to one native read snapshot.
func (s *Store) LoadCardDAVPublicationReviewSourceAuthorizedContext(ctx context.Context, personID int64, authorize PersonEditAuthorizer) (*CardDAVPublicationReviewSource, error) {
	if personID <= 0 || authorize == nil {
		return nil, ErrCardDAVInvalidPlan
	}
	var source *CardDAVPublicationReviewSource
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		source, err = s.loadCardDAVPublicationReviewSourceTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		scope, err := s.identityGrantSelectionTx(ctx, tx, []int64{personID}, []int64{source.Book.ID})
		if err != nil {
			return err
		}
		return authorize(ctx, scope)
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}
