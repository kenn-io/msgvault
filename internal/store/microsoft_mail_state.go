package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const microsoftStatePrefix = "msmail-state:"

// MicrosoftMailState contains only observed provider fields. Nil preserves the
// previous observation; provider state never changes the local is_read column.
type MicrosoftMailState struct {
	IsRead     *bool
	FlagStatus *string
}

func microsoftStateLabelBase(providerID string) string {
	switch providerID {
	case microsoftStatePrefix + "unread":
		return "Provider: Unread"
	case microsoftStatePrefix + "flagged":
		return "Provider: Flagged"
	default:
		return "Provider: " + strings.TrimPrefix(providerID, microsoftStatePrefix)
	}
}

func microsoftMailObservedState(state MicrosoftMailState) map[string]bool {
	observed := make(map[string]bool, 2)
	if state.IsRead != nil {
		observed[microsoftStatePrefix+"unread"] = !*state.IsRead
	}
	if state.FlagStatus != nil {
		observed[microsoftStatePrefix+"flagged"] = *state.FlagStatus == "flagged"
	}
	return observed
}

// ReconcileMicrosoftMailStateContext independently reconciles the native folder,
// categories and optional state in one attribution transaction.
func (s *Store) ReconcileMicrosoftMailStateContext(ctx context.Context, messageID int64, folderID *int64, categories *[]string, state MicrosoftMailState) (bool, error) {
	if state.FlagStatus != nil {
		switch *state.FlagStatus {
		case "flagged", "notFlagged", "complete":
		default:
			return false, errors.New("unsupported Microsoft follow-up state")
		}
	}
	var folderChanged bool
	err := s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM messages
 WHERE id=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=messages.source_id AND s.source_type='msmail')`, messageID).Scan(&sourceID); err != nil {
			return fmt.Errorf("resolve Microsoft message state: %w", err)
		}
		var err error
		folderChanged, err = s.reconcileMicrosoftMailLabelsAndStateTx(ctx, tx, sourceID, messageID, folderID, categories, state)
		return err
	})
	return folderChanged, err
}

func (s *Store) reconcileMicrosoftMailStateTx(ctx context.Context, tx *loggedTx, sourceID int64, state MicrosoftMailState) ([]int64, bool, error) {
	observed := microsoftMailObservedState(state)
	if len(observed) == 0 {
		return nil, false, nil
	}
	q := boundQuerier{ctx: ctx, q: tx}
	var stateLabels []int64
	catalogChanged := false
	for providerID, active := range observed {
		if !active {
			continue
		}
		var id int64
		var prior string
		var kind sql.NullString
		err := q.QueryRow(`SELECT id,name,label_type FROM labels WHERE source_id=? AND source_label_id=?`, sourceID, providerID).Scan(&id, &prior, &kind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		name, nameErr := microsoftMailLabelName(q, sourceID, id, microsoftStateLabelBase(providerID), nil)
		if nameErr != nil {
			return nil, false, nameErr
		}
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = q.QueryRow(`INSERT INTO labels(source_id,source_label_id,name,label_type) VALUES (?,?,?,'system') RETURNING id`, sourceID, providerID, name).Scan(&id)
			catalogChanged = true
		case prior != name || kind.String != "system":
			_, err = q.Exec(`UPDATE labels SET name=?,label_type='system',system_role=NULL WHERE id=?`, name, id)
			catalogChanged = true
		}
		if err != nil {
			return nil, false, fmt.Errorf("save Microsoft state label: %w", err)
		}
		stateLabels = append(stateLabels, id)
	}
	return stateLabels, catalogChanged, nil
}
