package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// BindMeetingSourceOwner claims an unbound source once. Concurrent registrations
// cannot replace an existing account owner under the same source identifier.
func (s *Store) BindMeetingSourceOwner(sourceID int64, email string) error {
	raw, err := json.Marshal(map[string]string{"account_email": email})
	if err != nil {
		return err
	}
	ctx := context.Background()
	return s.withIdentityTxContext(ctx, nil, func(tx *loggedTx) error {
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET sync_config = %s WHERE id = ? AND sync_config IS NULL`, s.dialect.JSONBindExpr()), string(raw), sourceID)
		if err != nil {
			return fmt.Errorf("bind meeting source owner: %w", err)
		}
		var config sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id = ?`, sourceID).Scan(&config); err != nil {
			return err
		}
		var owner struct {
			Email string `json:"account_email"`
		}
		if !config.Valid || json.Unmarshal([]byte(config.String), &owner) != nil || owner.Email != email {
			return errors.New("meeting source is already bound to another account; use a new identifier")
		}
		return nil
	})
}
