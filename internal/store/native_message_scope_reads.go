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

// NativeMessageScopeRead binds a provider response to the exact source and
// scope observations made before its request. Parent scopes precede children.
type NativeMessageScopeRead struct {
	SourceID                                    int64
	SourceReference, SourceType, NativeIdentity string
	Scopes                                      []NativeMessageScopeLookup
}

// ConfirmNativeMessageScopeRead restores only unchanged observations and
// returns the lifetimes the coherent message transaction must still match.
// Provider IO stays outside identity -> sync generation -> Events clock.
func (s *Store) ConfirmNativeMessageScopeRead(ctx context.Context, read NativeMessageScopeRead) ([]NativeMessageScopeIdentity, error) {
	return s.applyNativeMessageScopeRead(ctx, read, false)
}

// RejectNativeMessageScopeRead retires only the exact lifetime observed before
// the denied request. A stale negative response cannot revoke a replacement.
func (s *Store) RejectNativeMessageScopeRead(ctx context.Context, read NativeMessageScopeRead) error {
	_, err := s.applyNativeMessageScopeRead(ctx, read, true)
	return err
}
func (s *Store) applyNativeMessageScopeRead(ctx context.Context, read NativeMessageScopeRead, denied bool) ([]NativeMessageScopeIdentity, error) {
	var result []NativeMessageScopeIdentity
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		source, lifetime, err := s.mcpResource(ctx, tx, "source", read.SourceID, false)
		if err != nil {
			return err
		}
		if source.SourceType != read.SourceType || !EqualIdentifier(source.ProviderID, read.NativeIdentity) || !hmac.Equal([]byte(source.Reference), []byte(read.SourceReference)) {
			return mcpStoreError("resource_unavailable")
		}
		allowed, err := s.nativeSourceMessagePermission(ctx, tx, read.SourceID)
		if err != nil {
			return err
		}
		if !allowed && !denied {
			return mcpStoreError("native_authority_unavailable")
		}
		ids := make([]int64, len(read.Scopes))
		seen := make(map[int64]bool, len(read.Scopes))
		// Validate every pinned lifetime before changing any of them.
		for i, pin := range read.Scopes {
			identity := pin.Identity
			if !nativeMessageScopeKind(read.SourceType, identity.Kind) || identity.ProviderID == "" || !nativeMessageScopeParent(read.SourceType, identity.Kind, identity.ProviderID, identity.ParentProviderID) || !nativeScopeEpoch(read.SourceType, identity.Epoch) || len(identity.Generation) != 64 {
				return mcpStoreError("invalid_native_scope_membership")
			}
			// #nosec G701 -- Fixed SQL; the native identity, lifetime and presence are bound.
			err := tx.QueryRowContext(ctx, s.Rebind(`SELECT id FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND provider_id=? AND parent_provider_id=? AND native_epoch=? AND generation=? AND present=?`), read.SourceID, lifetime.source, identity.Kind, identity.ProviderID, identity.ParentProviderID, identity.Epoch, identity.Generation, pin.Present).Scan(&ids[i])
			if errors.Is(err, sql.ErrNoRows) {
				return mcpStoreError("resource_unavailable")
			}
			if err != nil {
				return err
			}
			if seen[ids[i]] {
				return mcpStoreError("invalid_native_scope_membership")
			}
			seen[ids[i]] = true
		}
		for i, pin := range read.Scopes {
			identity := pin.Identity
			if denied {
				// #nosec G701 -- Fixed SQL; the transaction-validated native ID is bound.
				if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE id=?`), ids[i]); err != nil {
					return err
				}
				observation := NativeMessageScopeInventory{SourceID: read.SourceID, SourceType: read.SourceType, Kind: identity.Kind, ParentProviderID: identity.ParentProviderID, retiredOrigins: []string{identity.ProviderID}}
				if err := s.retireNativeMessageDependentsTx(ctx, tx, observation, lifetime.source); err != nil {
					return err
				}
				continue
			}
			switch identity.Kind {
			case "discord_thread", "slack_thread", "matrix_thread":
				parentKind := "discord_channel"
				switch identity.Kind {
				case "slack_thread":
					parentKind = "slack_channel"
				case "matrix_thread":
					parentKind = "matrix_room"
				}
				var present bool
				// #nosec G701 -- Fixed SQL; the exact parent and native source lifetime are bound.
				if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND provider_id=? AND present)`), read.SourceID, lifetime.source, parentKind, identity.ParentProviderID).Scan(&present); err != nil {
					return err
				}
				if !present {
					return mcpStoreError("resource_unavailable")
				}
			}
			if !pin.Present {
				var generation [32]byte
				if _, err := rand.Read(generation[:]); err != nil {
					return fmt.Errorf("create native scope generation: %w", err)
				}
				identity.Generation = hex.EncodeToString(generation[:])
				// #nosec G701 -- Fixed SQL; transaction-validated scope ID and fresh generation are bound.
				if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=TRUE,generation=? WHERE id=?`), identity.Generation, ids[i]); err != nil {
					return err
				}
			}
			result = append(result, identity)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
