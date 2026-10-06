package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.TriageSnapshotStore = (*Store)(nil)

// InboxTriageSnapshot reads only selected committed identities and metadata.
// Mapping, observation, and arrival evidence all come from one read snapshot.
func (s *Store) InboxTriageSnapshot(ctx context.Context, source inboxcontrol.SourceIdentity, targets []inboxcontrol.Target) (*inboxcontrol.TriageSnapshot, error) {
	if source.Validate() != nil || len(targets) < 1 || len(targets) > 100 {
		return nil, inboxcontrol.ErrInvalid
	}
	seen := map[inboxcontrol.Target]bool{}
	for _, target := range targets {
		if target.Validate() != nil || seen[target] {
			return nil, inboxcontrol.ErrInvalid
		}
		if target.SourceID != source.SourceID || target.SourceType != source.SourceType || target.SourceIdentifier != source.SourceIdentifier || target.AccountID != source.AccountID {
			return nil, inboxcontrol.ErrDenied
		}
		seen[target] = true
	}
	result := &inboxcontrol.TriageSnapshot{Source: source, Candidates: make([]inboxcontrol.Candidate, 0, len(targets))}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		var err error
		result.Mappings, result.MappingRevision, err = readInboxTriageMappings(ctx, tx, source)
		if err != nil {
			return err
		}
		result.ArchiveRevision, _, err = inboxCandidateRevision(ctx, tx, source, targets[0].Scope)
		if err != nil {
			return err
		}
		var count, maxID int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM messages WHERE source_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, source.SourceID).Scan(&count, &maxID); err != nil {
			return fmt.Errorf("read triage incoming watermark: %w", err)
		}
		encoded, err := json.Marshal(struct {
			Source       inboxcontrol.SourceIdentity
			Count, MaxID int64
		}{source, count, maxID})
		if err != nil {
			return fmt.Errorf("encode triage incoming watermark: %w", err)
		}
		digest := sha256.Sum256(encoded)
		result.IncomingWatermark = hex.EncodeToString(digest[:])
		for _, target := range targets {
			key, err := inboxStateKey(target)
			if err != nil {
				return err
			}
			table, providerID, title, snippet, active := inboxCandidateRecordSQL(target.Scope)
			contextMessage := "0"
			if target.Scope == inboxcontrol.ScopeChat {
				contextMessage = `COALESCE((SELECT m.id FROM messages m WHERE m.source_id=p.source_id AND m.conversation_id=p.item_id AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL ORDER BY COALESCE(m.sent_at,m.received_at,m.internal_date) DESC NULLS LAST,m.id DESC LIMIT 1),0)`
			}
			query := fmt.Sprintf(`SELECT p.state_json,COALESCE(r.%s,''),COALESCE(r.%s,''),%s FROM inbox_provider_states p JOIN %s r ON r.id=p.item_id AND r.source_id=p.source_id AND r.%s=p.provider_id WHERE p.target_key=? AND p.source_id=? AND p.scope=? AND p.is_inbox=TRUE AND %s AND %s`, title, snippet, contextMessage, table, providerID, active, inboxCandidateMembershipSQL(source))
			var candidate inboxcontrol.Candidate
			var stateJSON string
			err = tx.QueryRowContext(ctx, query, key, source.SourceID, target.Scope).Scan(&stateJSON, &candidate.Title, &candidate.Snippet, &candidate.ContextMessageID)
			if errors.Is(err, sql.ErrNoRows) {
				return inboxcontrol.ErrPlanChanged
			}
			if err != nil {
				return fmt.Errorf("read triage candidate: %w", err)
			}
			if json.Unmarshal([]byte(stateJSON), &candidate.State) != nil || candidate.State.Target != target || candidate.State.Source != (inboxcontrol.SourceIdentity{}) || candidate.State.Inbox == nil || !*candidate.State.Inbox || candidate.State.Read == nil || (source.SourceType == "beeper" && candidate.State.MarkedUnread == nil) || candidate.State.ObservedAt.IsZero() {
				return inboxcontrol.ErrUnavailable
			}
			candidate.Available = true
			result.Candidates = append(result.Candidates, candidate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
