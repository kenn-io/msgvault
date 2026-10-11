package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
)

// NativeMessageScopeLookup retains a previously observed native identity.
// Present is deliberately not required: only a fresh verified provider read
// may use this identity to restore a denied scope. Archive rows are not a fallback.
type NativeMessageScopeLookup struct {
	Identity    NativeMessageScopeIdentity
	DisplayName string
	Present     bool
}

func (s *Store) LookupNativeMessageScope(ctx context.Context, sourceID int64, reference, kind, providerID string) (NativeMessageScopeLookup, bool, error) {
	var out NativeMessageScopeLookup
	source, err := s.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return out, false, err
	}
	if !hmac.Equal([]byte(source.Reference), []byte(reference)) || !nativeMessageScopeKind(source.SourceType, kind) {
		return out, false, mcpStoreError("resource_unavailable")
	}
	out.Identity.Kind = kind
	out.Identity.ProviderID = providerID
	// #nosec G701 -- Fixed SQL; exact native identity and current account are bound.
	err = s.DB().QueryRowContext(ctx, s.Rebind(`SELECT parent_provider_id,native_epoch,display_name,generation,present FROM native_message_scopes n WHERE n.source_id=? AND n.scope_kind=? AND n.provider_id=? AND EXISTS (SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation)`), sourceID, kind, providerID).Scan(&out.Identity.ParentProviderID, &out.Identity.Epoch, &out.DisplayName, &out.Identity.Generation, &out.Present)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	return out, err == nil, mcpSafeError(err)
}

// ListNativeMessageScopes snapshots known current and denied native identities
// before provider IO. It exposes no archive content and does not grant access;
// subsequent confirmation and message capture must still match each lifetime.
func (s *Store) ListNativeMessageScopes(ctx context.Context, sourceID int64, reference string) ([]NativeMessageScopeLookup, error) {
	source, err := s.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(source.Reference), []byte(reference)) {
		return nil, mcpStoreError("resource_unavailable")
	}
	// #nosec G701 -- Fixed SQL; the exact source identity is bound.
	rows, err := s.DB().QueryContext(ctx, s.Rebind(`SELECT scope_kind,provider_id,parent_provider_id,native_epoch,display_name,generation,present FROM native_message_scopes n WHERE n.source_id=? AND EXISTS (SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation) ORDER BY n.id`), sourceID)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []NativeMessageScopeLookup
	for rows.Next() {
		var scope NativeMessageScopeLookup
		if err := rows.Scan(&scope.Identity.Kind, &scope.Identity.ProviderID, &scope.Identity.ParentProviderID, &scope.Identity.Epoch, &scope.DisplayName, &scope.Identity.Generation, &scope.Present); err != nil {
			return nil, mcpSafeError(err)
		}
		out = append(out, scope)
	}
	return out, mcpSafeError(errors.Join(rows.Err(), rows.Close()))
}
