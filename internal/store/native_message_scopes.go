package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// NativeMessageScope is adapter-observed identity. Presentation names never
// establish membership or identify an inbox. Epoch is required by adapters
// whose native identities can be reused without an explicit removal.
type NativeMessageScope struct {
	ProviderID, DisplayName, Epoch string
	// PermissionDomain records the native channel kind whose current read grant
	// authorizes it. It is derived from the parser, never from an ID prefix.
	PermissionDomain string
}

// NativeMessageScopeInventory covers one exact native identity domain. Complete
// means authoritative absence in that domain, not simply successful pagination.
// Activity-window or incremental listings must leave Complete false.
type NativeMessageScopeInventory struct {
	SourceID                                                            int64
	SourceReference, SourceType, NativeIdentity, Kind, ParentProviderID string
	Complete                                                            bool
	// IdentityOnly inventories establish identity/presence coverage, but cannot
	// admit a new or previously denied native content-read lifetime. A verified
	// read observation restores it separately before coherent message capture.
	IdentityOnly bool
	Scopes       []NativeMessageScope
	// RetiredProviderIDs names positively observed removal, denial, or exclusion.
	// It applies independently of whether the remaining inventory is complete.
	RetiredProviderIDs []string
	// Transaction-derived positive revocation/rebind evidence. Newly observed
	// identity-only rows cannot retire a pre-existing conversation origin.
	retiredOrigins []string
}

// ObserveNativeMessageScopes pins provider observations to their account lifetime
// under identity -> sync/source -> Events clock fencing. No provider IO occurs
// here. Ordinary archive labels/conversations are neither authority nor deleted.
func (s *Store) ObserveNativeMessageScopes(ctx context.Context, observation NativeMessageScopeInventory) error {
	if observation.SourceID <= 0 || !CanonicalMCPResourceReference(observation.SourceReference) || !nativeMessageScopeKind(observation.SourceType, observation.Kind) || !nativeMessageScopeParent(observation.SourceType, observation.Kind, "", observation.ParentProviderID) {
		return mcpStoreError("invalid_native_scope_inventory")
	}
	seen := make(map[string]bool, len(observation.Scopes))
	for _, scope := range observation.Scopes {
		if scope.ProviderID == "" || seen[scope.ProviderID] || !nativeMessageScopeParent(observation.SourceType, observation.Kind, scope.ProviderID, observation.ParentProviderID) || !nativeScopeEpoch(observation.SourceType, scope.Epoch) || !nativeMessageScopePermissionDomain(observation.SourceType, observation.Kind, scope.PermissionDomain) {
			return mcpStoreError("invalid_native_scope_inventory")
		}
		seen[scope.ProviderID] = true
	}
	for _, providerID := range observation.RetiredProviderIDs {
		if providerID == "" || seen[providerID] || !nativeMessageScopeParent(observation.SourceType, observation.Kind, providerID, observation.ParentProviderID) {
			return mcpStoreError("invalid_native_scope_inventory")
		}
		seen[providerID] = true
	}
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		observation.retiredOrigins = append([]string(nil), observation.RetiredProviderIDs...)
		source, lifetime, err := s.mcpResource(ctx, tx, "source", observation.SourceID, false)
		if err != nil {
			return err
		}
		if source.SourceType != observation.SourceType || !EqualIdentifier(source.ProviderID, observation.NativeIdentity) || !hmac.Equal([]byte(source.Reference), []byte(observation.SourceReference)) {
			return mcpStoreError("resource_unavailable")
		}
		if (observation.SourceType == sourceTypeSlack && observation.Kind == "slack_thread" || observation.SourceType == sourceTypeDiscord && observation.Kind == "discord_thread" || observation.SourceType == sourceTypeMatrix && observation.Kind == "matrix_thread") && !observation.IdentityOnly {
			parentKind := "slack_channel"
			switch observation.SourceType {
			case sourceTypeDiscord:
				parentKind = "discord_channel"
			case sourceTypeMatrix:
				parentKind = "matrix_room"
			}
			var parentPresent bool
			// #nosec G701 -- Fixed SQL; the exact native parent and source lifetime are bound.
			err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND provider_id=? AND present)`), observation.SourceID, lifetime.source, parentKind, observation.ParentProviderID).Scan(&parentPresent)
			if err != nil {
				return err
			}
			if !parentPresent {
				return mcpStoreError("resource_unavailable")
			}
		}
		for _, scope := range observation.Scopes {
			var priorSource, priorParent, priorEpoch string
			// #nosec G701 -- Fixed SQL; the exact native identity is bound.
			priorErr := tx.QueryRowContext(ctx, s.Rebind(`SELECT source_generation,parent_provider_id,native_epoch FROM native_message_scopes WHERE source_id=? AND scope_kind=? AND provider_id=?`), observation.SourceID, observation.Kind, scope.ProviderID).Scan(&priorSource, &priorParent, &priorEpoch)
			if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
				return priorErr
			}
			if priorErr == nil && (priorSource != lifetime.source || priorParent != observation.ParentProviderID || priorEpoch != scope.Epoch) {
				observation.retiredOrigins = append(observation.retiredOrigins, scope.ProviderID)
			}
			var key [32]byte
			if _, err := rand.Read(key[:]); err != nil {
				return fmt.Errorf("create native scope generation: %w", err)
			}
			// #nosec G701 -- Fixed SQL; native observation fields are bound values.
			_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO native_message_scopes (source_id,source_generation,scope_kind,provider_id,parent_provider_id,native_epoch,permission_domain,display_name,generation,present) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT (source_id,scope_kind,provider_id) DO UPDATE SET generation=CASE WHEN (native_message_scopes.present OR NOT excluded.present) AND native_message_scopes.source_generation=excluded.source_generation AND native_message_scopes.parent_provider_id=excluded.parent_provider_id AND native_message_scopes.native_epoch=excluded.native_epoch THEN native_message_scopes.generation ELSE excluded.generation END,source_generation=excluded.source_generation,parent_provider_id=excluded.parent_provider_id,native_epoch=excluded.native_epoch,permission_domain=excluded.permission_domain,display_name=excluded.display_name,present=CASE WHEN excluded.present THEN TRUE WHEN native_message_scopes.source_generation=excluded.source_generation AND native_message_scopes.parent_provider_id=excluded.parent_provider_id AND native_message_scopes.native_epoch=excluded.native_epoch THEN native_message_scopes.present ELSE FALSE END`), observation.SourceID, lifetime.source, observation.Kind, scope.ProviderID, observation.ParentProviderID, scope.Epoch, scope.PermissionDomain, scope.DisplayName, hex.EncodeToString(key[:]), !observation.IdentityOnly)
			if err != nil {
				return err
			}
		}
		for _, providerID := range observation.RetiredProviderIDs {
			// #nosec G701 -- Fixed SQL; the exact positive native denial is bound.
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE source_id=? AND source_generation=? AND scope_kind=? AND parent_provider_id=? AND provider_id=?`), observation.SourceID, lifetime.source, observation.Kind, observation.ParentProviderID, providerID); err != nil {
				return err
			}
		}
		if !observation.Complete {
			return s.retireNativeMessageDependentsTx(ctx, tx, observation, lifetime.source)
		}
		// #nosec G701 -- Fixed SQL; the exact observed domain is bound.
		rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT id,provider_id FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND parent_provider_id=?`), observation.SourceID, lifetime.source, observation.Kind, observation.ParentProviderID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var retired []int64
		for rows.Next() {
			var id int64
			var providerID string
			if err := rows.Scan(&id, &providerID); err != nil {
				_ = rows.Close()
				return err
			}
			if !seen[providerID] {
				retired = append(retired, id)
				observation.retiredOrigins = append(observation.retiredOrigins, providerID)
			}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for _, id := range retired {
			// #nosec G701 -- Fixed SQL; the retired native resource ID is bound.
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE id=?`), id); err != nil {
				return err
			}
		}
		return s.retireNativeMessageDependentsTx(ctx, tx, observation, lifetime.source)
	})
}

// Native email containers have exact account-bound IDs and no parent. IMAP
// additionally requires a verified mailbox epoch; other providers introduce
// their native identity rules with their first working consumer.
func nativeMessageScopeKind(sourceType, kind string) bool {
	return sourceType == "beeper" && kind == "beeper_member" || sourceType == sourceTypeMatrix && (kind == "matrix_room" || kind == "matrix_thread") || sourceType == sourceTypeDiscord && (kind == "discord_channel" || kind == "discord_thread") || sourceType == "teams" && kind == "teams_chat" || sourceType == "gmail" && kind == "gmail_label" || sourceType == "msmail" && kind == "msmail_folder" || sourceType == sourceTypeIMAP && kind == "imap_mailbox" || sourceType == sourceTypeSlack && (kind == "slack_channel" || kind == "slack_thread")
}

func nativeScopeEpoch(sourceType, epoch string) bool {
	if sourceType != sourceTypeIMAP {
		return epoch == ""
	}
	value, err := strconv.ParseUint(epoch, 10, 32)
	return err == nil && value > 0 && strconv.FormatUint(value, 10) == epoch
}

// Slack root timestamps are unique only inside their channel. The compound
// provider ID retains the native tuple and must agree with its exact parent.
func nativeMessageScopeParent(sourceType, kind, providerID, parent string) bool {
	if sourceType == "beeper" {
		return kind == "beeper_member" && parent != "" && !strings.ContainsAny(parent, "\r\n\x00") && !strings.ContainsAny(providerID, "\r\n\x00")
	}
	if sourceType == sourceTypeMatrix {
		valid := func(value, prefix string) bool {
			return strings.HasPrefix(value, prefix) && len(value) > 1 && !strings.ContainsAny(value, " \t\r\n")
		}
		if kind == "matrix_thread" {
			return valid(parent, "!") && (providerID == "" || valid(providerID, "$"))
		}
		return parent == "" && (providerID == "" || valid(providerID, "!"))
	}
	if sourceType == sourceTypeDiscord {
		valid := func(id string) bool {
			n, err := strconv.ParseUint(id, 10, 64)
			return err == nil && n > 0 && strconv.FormatUint(n, 10) == id
		}
		if providerID != "" && !valid(providerID) {
			return false
		}
		if kind == "discord_thread" {
			return valid(parent) && (providerID == "" || providerID != parent)
		}
		return parent == ""
	}

	if sourceType != sourceTypeSlack || kind != "slack_thread" {
		return parent == ""
	}
	if parent == "" || strings.Contains(parent, ":") {
		return false
	}
	if providerID == "" {
		return true
	} // Validate an empty inventory's parent domain.
	root, ok := strings.CutPrefix(providerID, parent+":")
	return ok && root != "" && !strings.Contains(root, ":")
}

func (s *Store) retireNativeSlackChildrenTx(ctx context.Context, tx *sql.Tx, observation NativeMessageScopeInventory, generation string) error {
	if observation.SourceType != sourceTypeSlack || observation.Kind != "slack_channel" {
		return nil
	}
	// #nosec G701 -- Fixed SQL; source lifetime values are bound parameters.
	_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE source_id=? AND source_generation=? AND scope_kind='slack_thread' AND present AND NOT EXISTS (SELECT 1 FROM native_message_scopes parent WHERE parent.source_id=native_message_scopes.source_id AND parent.source_generation=native_message_scopes.source_generation AND parent.scope_kind='slack_channel' AND parent.provider_id=native_message_scopes.parent_provider_id AND parent.present)`), observation.SourceID, generation)
	return err
}

func nativeMessageScopePermissionDomain(source, kind, domain string) bool {
	if source != sourceTypeSlack || kind != "slack_channel" {
		return domain == ""
	}
	switch domain {
	case "channels", "groups", "im", "mpim":
		return true
	default:
		return false
	}
}

func (s *Store) retireNativeMessageDependentsTx(ctx context.Context, tx *sql.Tx, observation NativeMessageScopeInventory, generation string) error {
	if err := s.retireNativeSlackChildrenTx(ctx, tx, observation, generation); err != nil {
		return err
	}
	origins := append([]string(nil), observation.retiredOrigins...)
	legacyOrigins, err := s.retiredNativeDiscordLegacyOriginsTx(ctx, tx, observation, generation)
	if err != nil {
		return err
	}
	origins = append(origins, legacyOrigins...)
	if observation.SourceType == sourceTypeDiscord && observation.Kind == "discord_channel" || observation.SourceType == sourceTypeMatrix && observation.Kind == "matrix_room" {
		childKind := "discord_thread"
		if observation.SourceType == sourceTypeMatrix {
			childKind = "matrix_thread"
		}
		for _, parentID := range observation.retiredOrigins {
			// #nosec G701 -- Fixed SQL; exact native parent and source lifetime are bound.
			rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT provider_id FROM native_message_scopes WHERE source_id=? AND source_generation=? AND scope_kind=? AND parent_provider_id=?`), observation.SourceID, generation, childKind, parentID)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var child string
				if err := rows.Scan(&child); err != nil {
					_ = rows.Close()
					return err
				}
				origins = append(origins, child)
			}
			rowErr, closeErr := rows.Err(), rows.Close()
			if rowErr != nil {
				return rowErr
			}
			if closeErr != nil {
				return closeErr
			}
			// #nosec G701 -- Fixed SQL; the positively retired native parent is bound.
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE source_id=? AND source_generation=? AND scope_kind=? AND parent_provider_id=?`), observation.SourceID, generation, childKind, parentID); err != nil {
				return err
			}
		}
	}
	if observation.SourceType != sourceTypeDiscord && observation.SourceType != sourceTypeMatrix && observation.SourceType != "beeper" && (observation.SourceType != "teams" || observation.Kind != "teams_chat") {
		return nil
	}
	// Retire origins only for positive absence, denial or rebind, including an
	// archived conversation with no native scope row yet. Unknown first metadata
	// must preserve the identity adopted by existing subscriptions and receipts.
	for _, providerID := range origins {
		// #nosec G701 -- Fixed SQL; exact native source and container IDs are bound.
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_conversation_resources WHERE source_id=? AND EXISTS(SELECT 1 FROM conversations c WHERE c.id=mcp_conversation_resources.conversation_id AND c.source_id=mcp_conversation_resources.source_id AND c.source_conversation_id=?)`), observation.SourceID, providerID); err != nil {
			return err
		}
	}
	return nil
}
