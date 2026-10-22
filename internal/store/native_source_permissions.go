package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"time"
)

// NativeSourcePermission is native adapter evidence, never client-supplied
// authorization. SourceReference pins the account lifetime observed before
// provider work. Unknown legacy permission metadata cannot grant message read.
type NativeSourcePermission struct {
	SourceID        int64
	SourceReference string
	SourceType      string
	NativeIdentity  string
	Known           bool
	MessageRead     bool
	// DeniedScopeDomains is positive native evidence of a missing read grant.
	// It retires only matching Slack channels, including quiet omitted ones.
	DeniedScopeDomains []string
}

// ObserveNativeSourcePermission persists an adapter's current permission
// observation under identity -> sync generation -> Events clock fencing.
// No credentials, scopes, provider calls or archive content enter this table.
func (s *Store) ObserveNativeSourcePermission(ctx context.Context, observation NativeSourcePermission) error {
	if observation.SourceID <= 0 || !CanonicalMCPResourceReference(observation.SourceReference) || observation.MessageRead && !observation.Known {
		return mcpStoreError("invalid_native_authority")
	}
	for _, domain := range observation.DeniedScopeDomains {
		if !observation.Known || !nativeMessageScopePermissionDomain(observation.SourceType, "slack_channel", domain) || observation.SourceType != sourceTypeSlack {
			return mcpStoreError("invalid_native_authority")
		}
	}
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		resource, lifetime, err := s.mcpResource(ctx, tx, "source", observation.SourceID, false)
		if err != nil {
			return err
		}
		if resource.SourceType != observation.SourceType || !EqualIdentifier(resource.ProviderID, observation.NativeIdentity) || !hmac.Equal([]byte(resource.Reference), []byte(observation.SourceReference)) {
			return mcpStoreError("resource_unavailable")
		}
		var alreadyDenied bool
		// #nosec G701 -- Fixed SQL; the native source ID is a bound parameter.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM native_source_permissions WHERE source_id=? AND permission_known AND NOT message_read)`), observation.SourceID).Scan(&alreadyDenied)
		if err != nil {
			return err
		}
		// #nosec G701 -- Fixed SQL; native observation fields are bound values.
		_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO native_source_permissions (source_id,source_generation,permission_known,message_read) VALUES (?,?,?,?) ON CONFLICT (source_id) DO UPDATE SET source_generation=excluded.source_generation,permission_known=excluded.permission_known,message_read=excluded.message_read`), observation.SourceID, lifetime.source, observation.Known, observation.MessageRead)
		if err != nil {
			return err
		}
		for _, domain := range observation.DeniedScopeDomains {
			// Retained source-wide occurrences also bind their native conversation.
			// Revoke those origins before restoring a channel can create a new lifetime.
			// #nosec G701 -- Fixed SQL; native source lifetime and domain are bound.
			if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_conversation_resources WHERE source_id=? AND EXISTS (SELECT 1 FROM conversations c WHERE c.id=mcp_conversation_resources.conversation_id AND c.source_id=mcp_conversation_resources.source_id AND EXISTS (SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.source_generation=? AND n.scope_kind='slack_channel' AND n.provider_id=c.source_conversation_id AND n.permission_domain=?))`), observation.SourceID, lifetime.source, domain); err != nil {
				return err
			}
			// #nosec G701 -- Fixed SQL; the exact native permission domain is bound.
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE native_message_scopes SET present=FALSE WHERE source_id=? AND source_generation=? AND scope_kind='slack_channel' AND permission_domain=?`), observation.SourceID, lifetime.source, domain); err != nil {
				return err
			}
		}
		if len(observation.DeniedScopeDomains) > 0 {
			if err := s.retireNativeSlackChildrenTx(ctx, tx, NativeMessageScopeInventory{SourceID: observation.SourceID, SourceType: sourceTypeSlack, Kind: "slack_channel"}, lifetime.source); err != nil {
				return err
			}
		}
		if observation.Known && !observation.MessageRead && !alreadyDenied {
			// A positive native denial retires retained account/scope authority.
			// Later restoration creates a new lifetime and cannot revive old
			// subscriptions or receipt bindings. Unknown evidence asserts no
			// removal, while still making native message access unavailable.
			for _, query := range []string{
				`DELETE FROM mcp_conversation_resources WHERE source_id=?`,
				`DELETE FROM mcp_source_resources WHERE source_id=?`,
			} {
				// #nosec G701 -- Queries are the fixed internal statements above.
				if _, err := tx.ExecContext(ctx, s.Rebind(query), observation.SourceID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// NativeSourceMessagePermission intersects persisted permission evidence with
// the current native account lifetime. Archive rows remain readable separately.
func (s *Store) NativeSourceMessagePermission(ctx context.Context, sourceID int64) (bool, error) {
	return s.nativeSourceMessagePermission(ctx, s.DB(), sourceID)
}

func (s *Store) nativeSourceMessagePermission(ctx context.Context, db SQLReader, sourceID int64) (bool, error) {
	var allowed bool
	// #nosec G701 -- Fixed SQL; the source ID is a bound parameter.
	err := db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM native_source_permissions p WHERE p.source_id=? AND p.permission_known AND p.message_read AND EXISTS (SELECT 1 FROM mcp_source_resources r WHERE r.source_id=p.source_id AND r.generation=p.source_generation))`), sourceID).Scan(&allowed)
	return allowed, mcpSafeError(err)
}

// mcpNativeMessageAccess checks native authority separately from identity
// lifetimes. Unknown permission pauses use without asserting native removal.
// Import notifications have their own explicit local authorization contract.
func (s *Store) mcpNativeMessageAccess(ctx context.Context, db SQLReader, family string, sourceID int64) (bool, error) {
	switch family {
	case "msgvault.message_archived", mcpSourceMessageFamily, mcpInboxMessageFamily:
	default:
		return true, nil
	}
	var native bool
	// #nosec G701 -- Fixed SQL; source ID is a bound parameter.
	if err := db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM sources WHERE id=? AND source_type IN ('gmail','msmail','imap','slack','teams','discord','matrix','beeper'))`), sourceID).Scan(&native); err != nil {
		return false, err
	}
	if !native {
		return true, nil
	}
	return s.nativeSourceMessagePermission(ctx, db, sourceID)
}

// mcpNativeDeliveryReady keeps paused queues read-only. Expired or invalid
// authority and retention still use the normal fenced cleanup. The hint is
// advisory: preparation and network guards repeat their permission checks.
func (s *Store) mcpNativeDeliveryReady(ctx context.Context, id string, generation int64, now time.Time) (bool, error) {
	sub, err := s.mcpSubscription(ctx, s.DB(), id)
	if err != nil {
		return false, mcpSafeError(err)
	}
	if sub == nil || sub.Generation != generation {
		return false, nil
	}
	allowed, err := s.mcpNativeMessageAccess(ctx, s.DB(), sub.Name, sub.SourceID)
	if err != nil || allowed {
		return allowed, mcpSafeError(err)
	}
	valid, err := s.mcpSubscriptionResourceValid(ctx, s.DB(), sub)
	if err != nil {
		return false, mcpSafeError(err)
	}
	if !valid {
		return true, nil
	}
	var cleanup bool
	// #nosec G701 -- Fixed SQL; subscription state and times are bound values.
	err = s.DB().QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM mcp_event_clock clock WHERE singleton=1 AND (?<=? OR ?<>clock.capture_epoch OR ?<clock.pruned_through_seq OR EXISTS (SELECT 1 FROM mcp_event_log event WHERE event.seq=? AND event.recorded_at<?)))`), mcpTime(sub.ExpiresAt), mcpTime(now), sub.CursorEpoch, sub.CursorSeq, sub.PendingSeq, mcpTime(now.Add(-mcpEventRetention(s.mcpRoot().mcpConfig.Load())))).Scan(&cleanup)
	return cleanup, mcpSafeError(err)
}
