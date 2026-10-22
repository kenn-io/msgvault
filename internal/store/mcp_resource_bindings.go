package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Lifetime keys are private, persistent random identities. Display names are
// excluded. The database removes them on account or provider-identity changes,
// including changes away and back while Events capture is disabled.
type mcpScopeLifetime struct{ source, scope string }

func (s *Store) ensureMCPScopeLifetime(ctx context.Context, tx *sql.Tx, kind string, id, sourceID int64) (mcpScopeLifetime, error) {
	existing, err := s.mcpScopeLifetime(ctx, tx, kind, id, sourceID)
	if err == nil {
		return existing, nil
	}
	if kind == mcpScopeInbox {
		return existing, err
	}
	if !errors.Is(err, mcpStoreError("resource_unavailable")) {
		return mcpScopeLifetime{}, err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return mcpScopeLifetime{}, fmt.Errorf("generate source lifetime: %w", err)
	}
	// #nosec G701 -- Fixed SQL; generated lifetime and IDs are bound parameters.
	if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_source_resources (source_id,generation) SELECT id,? FROM sources WHERE id=? ON CONFLICT (source_id) DO NOTHING`), hex.EncodeToString(random[:]), sourceID); err != nil {
		return mcpScopeLifetime{}, err
	}
	if kind == mcpScopeConversation {
		if _, err := rand.Read(random[:]); err != nil {
			return mcpScopeLifetime{}, fmt.Errorf("generate conversation lifetime: %w", err)
		}
		// #nosec G701 -- Fixed SQL; generated lifetime and IDs are bound parameters.
		if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_conversation_resources (conversation_id,source_id,generation) SELECT id,source_id,? FROM conversations WHERE id=? AND source_id=? ON CONFLICT (conversation_id) DO NOTHING`), hex.EncodeToString(random[:]), id, sourceID); err != nil {
			return mcpScopeLifetime{}, err
		}
	}
	return s.mcpScopeLifetime(ctx, tx, kind, id, sourceID)
}

// Pure reads never create a replacement lifetime. Receipt authorization uses
// this query in the same read-only snapshot as the subsequent content read.
func (s *Store) mcpScopeLifetime(ctx context.Context, db SQLReader, kind string, id, sourceID int64) (mcpScopeLifetime, error) {
	var result mcpScopeLifetime
	var err error
	switch kind {
	case mcpScopeSource:
		if id != sourceID {
			return result, mcpStoreError("resource_unavailable")
		}
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT generation,generation FROM mcp_source_resources WHERE source_id=?`), sourceID).Scan(&result.source, &result.scope)
	case mcpScopeConversation:
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT sr.generation,cr.generation FROM mcp_source_resources sr JOIN mcp_conversation_resources cr ON cr.source_id=sr.source_id WHERE sr.source_id=? AND cr.conversation_id=? AND EXISTS (SELECT 1 FROM conversations c WHERE c.id=cr.conversation_id AND c.source_id=sr.source_id)`), sourceID, id).Scan(&result.source, &result.scope)
	case mcpScopeInbox:
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT sr.generation,n.generation FROM mcp_source_resources sr JOIN native_message_scopes n ON n.source_id=sr.source_id AND n.source_generation=sr.generation WHERE n.id=? AND n.source_id=? AND n.present`), id, sourceID).Scan(&result.source, &result.scope)
	default:
		return result, mcpStoreError("resource_unavailable")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return result, mcpStoreError("resource_unavailable")
	}
	return result, err
}

func (s *Store) bindMCPSubscriptionResource(ctx context.Context, tx *sql.Tx, sub *MCPSubscription, adopt bool) error {
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, sub.ScopeKind, sub.ScopeID, sub.SourceID)
	valid := err == nil && (!adopt || mcpAdoptableResourceFamily(sub.Name, sub.ScopeKind))
	if adopt && sub.ScopeKind == mcpScopeConversation && sub.ScopeReferenceID > 0 {
		exists, referenceErr := s.mcpSubscriptionScopeExists(ctx, tx, sub)
		if referenceErr != nil {
			return referenceErr
		}
		valid = valid && exists
	}
	if err != nil && (!adopt || !errors.Is(err, mcpStoreError("resource_unavailable"))) {
		return err
	}
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_subscription_resource_bindings (subscription_id,source_generation,scope_generation,valid) VALUES (?,?,?,?)`), sub.ID, lifetime.source, lifetime.scope, valid)
	return err
}

func mcpAdoptableResourceFamily(family, kind string) bool {
	switch family {
	case "msgvault.message_archived", "msgvault.draft_changed":
		return kind == mcpScopeConversation
	case "msgvault.calendar_event_changed":
		return kind == mcpScopeSource
	default:
		return false
	}
}

func (s *Store) mcpEventMessageAssociation(ctx context.Context, db SQLReader, event MCPEvent) (bool, error) {
	var valid bool
	err := db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM messages m WHERE m.id=? AND m.source_id=? AND m.conversation_id=? AND EXISTS (SELECT 1 FROM mcp_event_message_refs r WHERE r.message_id=m.id AND r.reference_seq=?))`), event.MessageID, event.SourceID, event.ConversationID, event.MessageReferenceSeq).Scan(&valid)
	return valid, err
}

func (s *Store) bindMCPEventResource(ctx context.Context, tx *sql.Tx, event MCPEvent, adopt bool) error {
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, event.ScopeKind, event.ScopeID, event.SourceID)
	valid := err == nil && (!adopt || mcpAdoptableResourceFamily(event.Family, event.ScopeKind))
	if event.ScopeKind == mcpScopeConversation && event.ScopeID != event.ConversationID {
		valid = false
	}
	if err != nil && (!adopt || !errors.Is(err, mcpStoreError("resource_unavailable"))) {
		return err
	}
	messageValid := false
	if event.MessageID > 0 {
		messageValid, err = s.mcpEventMessageAssociation(ctx, tx, event)
		if err != nil {
			return err
		}
	}
	if !adopt && (!valid || event.MessageID > 0 && !messageValid) {
		return mcpStoreError("resource_unavailable")
	}
	// #nosec G701 -- Fixed SQL; every retained binding value is a bound parameter.
	_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_event_resource_bindings (seq,source_generation,scope_generation,valid,message_valid) VALUES (?,?,?,?,?)`), event.Seq, lifetime.source, lifetime.scope, valid, messageValid)
	if err != nil {
		return err
	}
	if (event.Family == mcpSourceMessageFamily || event.Family == mcpInboxMessageFamily) && !adopt {
		origin, err := s.ensureMCPScopeLifetime(ctx, tx, mcpScopeConversation, event.ConversationID, event.SourceID)
		if err != nil {
			return err
		}
		// #nosec G701 -- Fixed SQL; validated event sequence and origin generation are bound.
		_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_event_origin_bindings (seq,generation) VALUES (?,?)`), event.Seq, origin.scope)
		return err
	}
	return nil
}

func (s *Store) mcpSubscriptionResourceValid(ctx context.Context, db SQLReader, sub *MCPSubscription) (bool, error) {
	if sub.ScopeKind == mcpScopeConversation && sub.ScopeReferenceID > 0 {
		exists, err := s.mcpSubscriptionScopeExists(ctx, db, sub)
		if err != nil || !exists {
			return false, err
		}
	}
	lifetime, err := s.mcpScopeLifetime(ctx, db, sub.ScopeKind, sub.ScopeID, sub.SourceID)
	if errors.Is(err, mcpStoreError("resource_unavailable")) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var valid bool
	err = db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM mcp_subscription_resource_bindings WHERE subscription_id=? AND valid AND source_generation=? AND scope_generation=?)`), sub.ID, lifetime.source, lifetime.scope).Scan(&valid)
	return valid, err
}

func (s *Store) mcpEventResourceValid(ctx context.Context, db SQLReader, event MCPEvent) (bool, error) {
	lifetime, err := s.mcpScopeLifetime(ctx, db, event.ScopeKind, event.ScopeID, event.SourceID)
	if errors.Is(err, mcpStoreError("resource_unavailable")) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var valid bool
	err = db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM mcp_event_resource_bindings WHERE seq=? AND valid AND source_generation=? AND scope_generation=?)`), event.Seq, lifetime.source, lifetime.scope).Scan(&valid)
	if err != nil || !valid || (event.Family != mcpSourceMessageFamily && event.Family != mcpInboxMessageFamily) {
		return valid, err
	}
	origin, err := s.mcpScopeLifetime(ctx, db, mcpScopeConversation, event.ConversationID, event.SourceID)
	if errors.Is(err, mcpStoreError("resource_unavailable")) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM mcp_event_origin_bindings WHERE seq=? AND generation=?)`), event.Seq, origin.scope).Scan(&valid)
	if err != nil || !valid {
		return valid, err
	}
	return s.mcpNativeConversationAllowed(ctx, db, event.Family, event.SourceID, event.ConversationID)
}

// ValidateMCPSubscriptionResource rejects a retained identity before any
// outbound renewal challenge. Activation repeats the guard under its fences.
func (s *Store) ValidateMCPSubscriptionResource(ctx context.Context, sub *MCPSubscription) error {
	if sub == nil {
		return mcpStoreError("subscription_unavailable")
	}
	valid, err := s.mcpSubscriptionResourceValid(ctx, s.DB(), sub)
	if err != nil {
		return mcpSafeError(err)
	}
	if !valid {
		return mcpStoreError("subscription_unavailable")
	}
	if allowed, err := s.mcpNativeMessageAccess(ctx, s.DB(), sub.Name, sub.SourceID); err != nil {
		return mcpSafeError(err)
	} else if !allowed {
		return mcpStoreError("native_authority_unavailable")
	}
	return nil
}

func (s *Store) ensureMCPResourceTriggers(ctx context.Context) error {
	filename := "schema_mcp_resources_sqlite.sql"
	if s.IsPostgreSQL() {
		filename = "schema_mcp_resources_pg.sql"
	}
	schema, err := schemaFS.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read resource lifetime triggers: %w", err)
	}
	if _, err = s.db.ExecContext(ctx, string(schema)); err != nil {
		return fmt.Errorf("install resource lifetime triggers: %w", err)
	}
	return nil
}

// MCPResourceBinding pins the resource observed before callback verification.
// Its opaque fields are internal authority, never client input or wire output.
type MCPResourceBinding struct {
	family       string
	kind         string
	id, sourceID int64
	lifetime     mcpScopeLifetime
}

// ResolveMCPSubscriptionResource validates capability and pins a lifetime in
// one fenced transaction. The transaction ends before any callback network IO.
func (s *Store) ResolveMCPSubscriptionResource(ctx context.Context, family, kind string, id int64, kinds []string, reference string) (int64, *MCPResourceBinding, error) {
	if reference != "" && !CanonicalMCPResourceReference(reference) {
		return 0, nil, mcpStoreError("invalid_resource_reference")
	}
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	runtime := root.mcpConfig.Load()
	var binding MCPResourceBinding
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		_, sourceID, err := s.validateMCPEventScope(ctx, tx, runtime, family, kind, id, kinds)
		if err != nil {
			return err
		}
		lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, kind, id, sourceID)
		if err != nil {
			return err
		}
		if reference != "" {
			current, _, err := s.mcpResource(ctx, tx, kind, id, false)
			if err != nil {
				return err
			}
			if !hmac.Equal([]byte(reference), []byte(current.Reference)) {
				return mcpStoreError("resource_unavailable")
			}
		}
		binding = MCPResourceBinding{family: family, kind: kind, id: id, sourceID: sourceID, lifetime: lifetime}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return binding.sourceID, &binding, nil
}

func (s *Store) checkMCPResourceBinding(ctx context.Context, db SQLReader, binding *MCPResourceBinding) error {
	if binding == nil {
		return mcpStoreError("subscription_unavailable")
	}
	lifetime, err := s.mcpScopeLifetime(ctx, db, binding.kind, binding.id, binding.sourceID)
	if errors.Is(err, mcpStoreError("resource_unavailable")) {
		return mcpStoreError("subscription_unavailable")
	}
	if err != nil {
		return err
	}
	if lifetime != binding.lifetime {
		return mcpStoreError("subscription_unavailable")
	}
	if allowed, err := s.mcpNativeMessageAccess(ctx, db, binding.family, binding.sourceID); err != nil {
		return err
	} else if !allowed {
		return mcpStoreError("native_authority_unavailable")
	}
	return nil
}

// CheckMCPResourceBinding rejects a changed resource before a callback attempt.
// Activation repeats the check under the identity/source/Events transaction.
func (s *Store) CheckMCPResourceBinding(ctx context.Context, binding *MCPResourceBinding) error {
	return mcpSafeError(s.checkMCPResourceBinding(ctx, s.DB(), binding))
}
