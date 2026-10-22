package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/callhistory"
)

// SlackHuddleObservationContext derives an independent snapshot from one
// archived Slack message. It resolves existing identifiers without creating
// identities or inferring attendance, and never reads message bodies.
func (s *Store) SlackHuddleObservationContext(ctx context.Context, messageID int64) (callhistory.Observation, error) {
	invalid := fmt.Errorf("%w: archived Slack huddle unavailable", callhistory.ErrInvalid)
	if messageID <= 0 {
		return callhistory.Observation{}, invalid
	}
	var account, sourceType, format string
	var stored []byte
	var compression sql.NullString
	var retired bool
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{callhistory.MaxRawBytes + 64<<10, messageID}, args...)
	err := s.db.QueryRowContext(ctx, s.Rebind(`
  SELECT src.identifier, src.source_type, COALESCE(r.raw_format, ''),
   CASE WHEN length(r.raw_data) <= ? THEN r.raw_data ELSE NULL END, r.compression,
   EXISTS (SELECT 1 FROM source_settings ss WHERE ss.source_id = src.id AND ss.merged_into_source_id IS NOT NULL)
  FROM messages m JOIN sources src ON src.id = m.source_id
  LEFT JOIN message_raw r ON r.message_id = m.id
  WHERE m.id = ? AND m.message_type = 'slack' AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages archive_only WHERE archive_only.message_id = m.id)`+scope), args...).Scan(&account, &sourceType, &format, &stored, &compression, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return callhistory.Observation{}, invalid
	}
	if err != nil {
		return callhistory.Observation{}, fmt.Errorf("read archived Slack huddle: %w", err)
	}
	if retired {
		return callhistory.Observation{}, ErrSourceRetired
	}
	if (sourceType != "slack" && sourceType != "slackdump") || format != "slack_json" {
		return callhistory.Observation{}, invalid
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, callhistory.MaxRawBytes, invalid)
	if err != nil {
		return callhistory.Observation{}, fmt.Errorf("%w: decode archived Slack huddle: %w", callhistory.ErrInvalid, err)
	}
	peers, err := callhistory.SlackHuddlePeerIDs(account, raw)
	if err != nil {
		return callhistory.Observation{}, err
	}
	team, _, _ := strings.Cut(account, ":")
	bindings := map[string]int64{}
	for _, peer := range peers {
		id, _, err := s.ParticipantByIdentifierContext(ctx, "slack", team+":"+peer)
		if err != nil {
			return callhistory.Observation{}, fmt.Errorf("resolve archived Slack peer: %w", err)
		}
		if id != 0 {
			bindings[peer] = id
		}
		if len(bindings) > 100 {
			return callhistory.Observation{}, invalid
		}
	}
	return (callhistory.ImportRequest{Format: "slack_huddle", Account: account, Record: raw, Bindings: bindings}).Observation()
}
