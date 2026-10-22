package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
)

// NativeMessageScopeIdentity is a parser-verified exact native membership.
// Kind, parent and epoch are identities, not display names or prefix matches.
// Generation pins the lifetime confirmed by a native read. Capture must not
// borrow a replacement scope observed after that read.
type NativeMessageScopeIdentity struct{ Kind, ProviderID, ParentProviderID, Epoch, Generation string }
type NativeMessageScopeMembership struct {
	SourceReference string
	Scopes          []NativeMessageScopeIdentity
}
type nativeMessageScopeBinding struct {
	id                                int64
	sourceGeneration, scopeGeneration string
}

func (s *Store) resolveNativeMessageScopesTx(ctx context.Context, tx *sql.Tx, data *MessagePersistData) ([]nativeMessageScopeBinding, error) {
	if data.NativeScopes == nil {
		return nil, nil
	}
	source, lifetime, err := s.mcpResource(ctx, tx, "source", data.Message.SourceID, false)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(source.Reference), []byte(data.NativeScopes.SourceReference)) {
		return nil, mcpStoreError("resource_unavailable")
	}
	result := make([]nativeMessageScopeBinding, 0, len(data.NativeScopes.Scopes))
	seen := make(map[int64]bool)
	for _, identity := range data.NativeScopes.Scopes {
		if !nativeMessageScopeKind(source.SourceType, identity.Kind) || identity.ProviderID == "" || !nativeMessageScopeParent(source.SourceType, identity.Kind, identity.ProviderID, identity.ParentProviderID) || !nativeScopeEpoch(source.SourceType, identity.Epoch) {
			return nil, mcpStoreError("invalid_native_scope_membership")
		}
		binding := nativeMessageScopeBinding{sourceGeneration: lifetime.source}
		// #nosec G701 -- Fixed SQL; exact parser identities and lifetime are bound.
		err := tx.QueryRowContext(ctx, s.Rebind(`SELECT id,generation FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND provider_id=? AND parent_provider_id=? AND native_epoch=? AND present`), data.Message.SourceID, lifetime.source, identity.Kind, identity.ProviderID, identity.ParentProviderID, identity.Epoch).Scan(&binding.id, &binding.scopeGeneration)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, mcpStoreError("resource_unavailable")
		}
		if err != nil {
			return nil, err
		}
		if identity.Generation != "" && !hmac.Equal([]byte(identity.Generation), []byte(binding.scopeGeneration)) {
			return nil, mcpStoreError("resource_unavailable")
		}
		if !seen[binding.id] {
			result = append(result, binding)
			seen[binding.id] = true
		}
	}
	return result, nil
}

func (s *Store) replaceNativeMessageScopesTx(ctx context.Context, tx *sql.Tx, messageID int64, bindings []nativeMessageScopeBinding) error {
	// #nosec G701 -- Fixed SQL; the native message ID is bound.
	if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM native_message_scope_memberships WHERE message_id=?`), messageID); err != nil {
		return err
	}
	for _, binding := range bindings {
		// #nosec G701 -- Fixed SQL; transaction-resolved bindings are bound values.
		if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO native_message_scope_memberships (message_id,scope_id,source_generation,scope_generation) VALUES (?,?,?,?)`), messageID, binding.id, binding.sourceGeneration, binding.scopeGeneration); err != nil {
			return err
		}
	}
	return nil
}
