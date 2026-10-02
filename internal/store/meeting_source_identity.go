package store

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// BindMeetingSourceIdentity claims an unbound source once. The stable
// provider account ID prevents another subject reusing the source's label.
func (s *Store) BindMeetingSourceIdentity(sourceID int64, email, userID string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || a.Name != "" || strings.TrimSpace(userID) == "" {
		return errors.New("meeting source requires an explicit email and provider account ID")
	}
	raw, err := json.Marshal(map[string]string{"account_email": email, "account_user_id": userID}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`UPDATE sources SET sync_config = %s WHERE id = ? AND source_type = 'pocket' AND sync_config IS NULL`, s.dialect.JSONBindExpr()), string(raw), sourceID)
	if err != nil {
		return fmt.Errorf("bind meeting source identity: %w", err)
	}
	var saved sql.NullString
	if err = s.db.QueryRow(`SELECT sync_config FROM sources WHERE id = ? AND source_type = 'pocket'`, sourceID).Scan(&saved); err != nil {
		return fmt.Errorf("read meeting source identity: %w", err)
	}
	var owner struct {
		Email  string `json:"account_email"`
		UserID string `json:"account_user_id"`
	}
	if !saved.Valid || json.Unmarshal([]byte(saved.String), &owner) != nil || owner.Email != email || owner.UserID != userID {
		return errors.New("meeting source is bound to another or unconfirmed account; use a new identifier")
	}
	return nil
}
