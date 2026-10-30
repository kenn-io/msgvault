package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// HasBeeperTelegramOwnerContext reports an overlap only when a Beeper source
// recorded the same native Telegram identity as its own conversation member.
// An ordinary contact or an unverified bridge identity is not owner evidence.
func (s *Store) HasBeeperTelegramOwnerContext(ctx context.Context, ownerID int64) (bool, error) {
	if ownerID <= 0 {
		return false, errors.New("invalid Telegram owner ID")
	}
	key := beeperidentity.TelegramStableProviderKey(strconv.FormatInt(ownerID, 10))
	rows, err := s.db.QueryContext(ctx, `SELECT o.participant_id, CAST(c.metadata AS TEXT), s.identifier
 FROM participant_contact_observations o
 JOIN sources s ON s.id = o.source_id
 JOIN conversation_participants cp ON cp.participant_id = o.participant_id
 JOIN conversations c ON c.id = cp.conversation_id AND c.source_id = s.id
 WHERE o.provider_user_id = ? AND o.active_until IS NULL AND o.superseded_at IS NULL
 AND s.source_type = ? AND c.metadata IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM source_settings ss WHERE ss.source_id = s.id AND ss.merged_into_source_id IS NOT NULL)`, key, "beeper")
	if err != nil {
		return false, fmt.Errorf("find Beeper Telegram owner evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var participantID int64
		var metadata, account string
		if err := rows.Scan(&participantID, &metadata, &account); err != nil {
			return false, err
		}
		var value struct {
			Route MessagingRouteEvidence `json:"messaging_route"`
		}
		if json.Unmarshal([]byte(metadata), &value) != nil {
			continue
		}
		if value.Route.AccountID == account && strings.EqualFold(value.Route.Network, "telegram") && slices.Contains(value.Route.SelfParticipantIDs, participantID) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read Beeper Telegram owner evidence: %w", err)
	}
	return false, nil
}
