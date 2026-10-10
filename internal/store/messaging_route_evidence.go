package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
)

// SetConversationMessagingRouteEvidence preserves other provider metadata.
// Sync calls it with incomplete evidence before roster writes, then with the
// exact completed roster afterward. Discovery never calls this write method.
func (s *Store) SetConversationMessagingRouteEvidence(ctx context.Context, conversationID int64, e MessagingRouteEvidence) error {
	for _, ids := range []*[]int64{&e.ParticipantIDs, &e.SelfParticipantIDs} {
		*ids = slices.Clone(*ids)
		slices.Sort(*ids)
		*ids = slices.Compact(*ids)
		if len(*ids) > maxRouteEvidenceIDs {
			e.Truncated = true
			*ids = (*ids)[:maxRouteEvidenceIDs]
		}
	}
	e.MemberChatIDs = slices.Clone(e.MemberChatIDs)
	if len(e.MemberChatIDs) > maxRouteEvidenceIDs {
		e.Truncated = true
		e.MemberChatIDs = e.MemberChatIDs[:maxRouteEvidenceIDs]
	}
	return s.withSyncConversationWriteContext(ctx, conversationID, func(q querier) error {
		var raw sql.NullString
		if err := q.QueryRow(`SELECT metadata FROM conversations WHERE id=?`, conversationID).Scan(&raw); err != nil {
			return fmt.Errorf("read route metadata: %w", err)
		}
		metadata := map[string]jsontext.Value{}
		if raw.Valid && raw.String != "" {
			if err := json.Unmarshal([]byte(raw.String), &metadata); err != nil {
				return fmt.Errorf("decode route metadata: %w", err)
			}
		}
		value, err := json.Marshal(e, json.Deterministic(true))
		if err != nil {
			return err
		}
		metadata["messaging_route"] = value
		encoded, err := json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return err
		}
		_, err = q.Exec(`UPDATE conversations SET metadata=`+s.dialect.JSONBindExpr()+` WHERE id=?`, string(encoded), conversationID)
		return err
	})
}

// SetSourceMessagingRouteFailureContext records an account-level route proof
// failure so quiet conversations from that source fail closed too. An empty
// failure clears the marker after the account response provides valid proof.
func (s *Store) SetSourceMessagingRouteFailureContext(ctx context.Context, sourceID int64, failure string) error {
	if sourceID <= 0 {
		return errors.New("invalid source messaging route failure")
	}
	switch failure {
	case "", "account_lookup_failed", "account_not_connected", "account_binding_mismatch", "network_unverified":
	default:
		return errors.New("invalid source messaging route failure")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if failure == "" {
			_, err := tx.ExecContext(ctx, `DELETE FROM source_messaging_route_failures WHERE source_id=?`, sourceID)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO source_messaging_route_failures(source_id,failure)
 VALUES (?,?) ON CONFLICT(source_id) DO UPDATE SET failure=excluded.failure,updated_at=CURRENT_TIMESTAMP`, sourceID, failure)
		return err
	})
	if err != nil {
		return fmt.Errorf("set source messaging route failure: %w", err)
	}
	return nil
}

// InvalidateConversationMessagingRouteEvidence marks the last captured roster
// incomplete while preserving the remaining provider evidence.
func (s *Store) InvalidateConversationMessagingRouteEvidence(ctx context.Context, conversationID int64) error {
	return s.invalidateConversationMessagingRouteEvidence(ctx, conversationID, "")
}

func (s *Store) invalidateConversationMessagingRouteEvidence(ctx context.Context, conversationID int64, failure string) error {
	return s.withSyncConversationWriteContext(ctx, conversationID, func(q querier) error {
		var raw sql.NullString
		if err := q.QueryRow(`SELECT metadata FROM conversations WHERE id=?`, conversationID).Scan(&raw); err != nil {
			return fmt.Errorf("read route metadata: %w", err)
		}
		metadata := map[string]jsontext.Value{}
		if raw.Valid && raw.String != "" {
			if err := json.Unmarshal([]byte(raw.String), &metadata); err != nil {
				return fmt.Errorf("decode route metadata: %w", err)
			}
		}
		if err := invalidateMessagingRouteMembership(metadata, failure); err != nil {
			return err
		}
		encoded, err := json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return err
		}
		_, err = q.Exec(`UPDATE conversations SET metadata=`+s.dialect.JSONBindExpr()+` WHERE id=?`, string(encoded), conversationID)
		return err
	})
}

// InvalidateSourceConversationMessagingRouteEvidenceContext marks route proof
// incomplete after the provider confirms a known chat no longer exists.
func (s *Store) InvalidateSourceConversationMessagingRouteEvidenceContext(ctx context.Context, sourceID int64, providerChatID string) error {
	return s.invalidateSourceConversationMessagingRouteEvidenceContext(ctx, sourceID, providerChatID, "")
}

// InvalidateSourceConversationMessagingRouteEvidenceWithFailureContext marks
// the last captured roster incomplete after a failed chat detail refresh and
// records the per-chat failure on the existing route evidence.
func (s *Store) InvalidateSourceConversationMessagingRouteEvidenceWithFailureContext(ctx context.Context, sourceID int64, providerChatID, failure string) error {
	if failure != "membership_fetch_failed" {
		return errors.New("invalid source conversation route evidence failure")
	}
	return s.invalidateSourceConversationMessagingRouteEvidenceContext(ctx, sourceID, providerChatID, failure)
}

func (s *Store) invalidateSourceConversationMessagingRouteEvidenceContext(ctx context.Context, sourceID int64, providerChatID, failure string) error {
	if sourceID <= 0 || providerChatID == "" {
		return errors.New("invalid source conversation route evidence identity")
	}
	var conversationID int64
	err := s.DB().QueryRowContext(ctx, s.Rebind(
		`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`,
	), sourceID, providerChatID).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find conversation for Beeper chat route evidence: %w", err)
	}
	return s.invalidateConversationMessagingRouteEvidence(ctx, conversationID, failure)
}

func invalidateMessagingRouteMembership(metadata map[string]jsontext.Value, failure string) error {
	value, ok := metadata["messaging_route"]
	if !ok {
		return nil
	}
	route := map[string]jsontext.Value{}
	if err := json.Unmarshal(value, &route); err != nil {
		return fmt.Errorf("decode messaging route evidence for invalidation: %w", err)
	}
	if route == nil {
		return nil
	}
	route["membership_complete"] = jsontext.Value("false")
	if failure != "" {
		encodedFailure, err := json.Marshal(failure, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode route evidence failure: %w", err)
		}
		route["failure"] = jsontext.Value(encodedFailure)
	}
	encoded, err := json.Marshal(route, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode incomplete messaging route evidence: %w", err)
	}
	metadata["messaging_route"] = encoded
	return nil
}
