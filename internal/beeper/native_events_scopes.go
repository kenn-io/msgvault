package beeper

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/store"
)

func (imp *Importer) pinNativeBeeperAuthority(ctx context.Context, sourceID int64, opts ImportOptions, state *SyncState) error {
	source, err := imp.store.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return err
	}
	imp.nativePermission = &store.NativeSourcePermission{SourceID: sourceID, SourceReference: source.Reference, SourceType: sourceTypeBeeper, NativeIdentity: opts.AccountID}
	imp.nativeRoutePins = make(map[string]store.NativeMessageScopeLookup)
	imp.nativeRouteReads = make(map[string]store.NativeMessageScopeLookup)
	imp.nativeReadPaused = false
	if !imp.store.MCPMessageCaptureEnabled(sourceTypeBeeper) {
		observation := *imp.nativePermission
		imp.nativePermission = nil
		return imp.store.ObserveNativeSourcePermission(ctx, observation)
	}
	for chatID := range state.Chats {
		pin, found, err := imp.store.LookupNativeMessageScope(ctx, sourceID, source.Reference, "beeper_member", chatID)
		if err != nil {
			return err
		}
		if found {
			imp.nativeRoutePins[chatID] = pin
		}
	}
	return nil
}

func (imp *Importer) pauseNativeBeeperAuthority(ctx context.Context, cause error) error {
	if imp.nativePermission == nil {
		return cause
	}
	imp.nativeReadPaused = true
	observation := *imp.nativePermission
	observation.Known, observation.MessageRead = false, false
	return errors.Join(cause, imp.store.ObserveNativeSourcePermission(ctx, observation))
}

// Native Chat is one exact account/chat route. Network names and archive
// rows cannot choose a member route for a merged or foreign response.
//
//nolint:nilnil // No native authority is a valid archive-only result.
func (imp *Importer) nativeBeeperRoute(ctx context.Context, sourceID int64, ch *Chat, opts ImportOptions) (*store.NativeMessageScopeMembership, error) {
	if imp.nativePermission == nil {
		return nil, nil
	}
	if imp.nativeReadPaused {
		return nil, errors.New("native_authority_unavailable")
	}
	pin, pinned := imp.nativeRoutePins[ch.ID]
	if !pinned {
		var err error
		pin, pinned, err = imp.store.LookupNativeMessageScope(ctx, sourceID, imp.nativePermission.SourceReference, "beeper_member", ch.ID)
		if err != nil {
			return nil, err
		}
	}
	request, cancel := opts.requestContext(ctx)
	detail, err := imp.client.GetChat(request, ch.ID)
	cancel()
	if err != nil {
		return nil, imp.nativeBeeperReadError(ctx, ch.ID, err)
	}
	if detail.ID != ch.ID || detail.AccountID == "" || ch.AccountID != opts.AccountID {
		return nil, imp.pauseNativeBeeperAuthority(ctx, errors.New("native_member_route_mismatch"))
	}
	if detail.AccountID != opts.AccountID {
		// A positively changed route revokes its old origin without touching
		// another login on the same network.
		observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: opts.AccountID, Kind: "beeper_member", ParentProviderID: opts.AccountID, RetiredProviderIDs: []string{ch.ID}}
		if pinned {
			err = imp.store.RejectNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: sourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: opts.AccountID, Scopes: []store.NativeMessageScopeLookup{pin}})
		} else {
			err = imp.store.ObserveNativeMessageScopes(ctx, observation)
		}
		return nil, errors.Join(errors.New("native_member_route_mismatch"), err)
	}
	imp.nativePermission.Known, imp.nativePermission.MessageRead = true, true
	if err = imp.store.ObserveNativeSourcePermission(ctx, *imp.nativePermission); err != nil {
		return nil, err
	}
	observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: opts.AccountID, Kind: "beeper_member", ParentProviderID: opts.AccountID, IdentityOnly: true, Scopes: []store.NativeMessageScope{{ProviderID: ch.ID, DisplayName: detail.Title}}}
	if err = imp.store.ObserveNativeMessageScopes(ctx, observation); err != nil {
		return nil, err
	}
	if !pinned {
		pin, pinned, err = imp.store.LookupNativeMessageScope(ctx, sourceID, imp.nativePermission.SourceReference, "beeper_member", ch.ID)
		if err != nil {
			return nil, err
		}
	}
	if !pinned {
		return nil, errors.New("native_member_route_unavailable")
	}
	confirmed, err := imp.store.ConfirmNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: sourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: opts.AccountID, Scopes: []store.NativeMessageScopeLookup{pin}})
	if err != nil {
		return nil, err
	}
	if len(confirmed) != 1 {
		return nil, errors.New("native_member_route_unavailable")
	}
	imp.nativeRouteReads[ch.ID] = store.NativeMessageScopeLookup{Identity: confirmed[0], Present: true}
	*ch = *detail
	return &store.NativeMessageScopeMembership{SourceReference: imp.nativePermission.SourceReference, Scopes: confirmed}, nil
}

func (imp *Importer) nativeBeeperReadError(ctx context.Context, chatID string, cause error) error {
	if cause == nil || imp.nativePermission == nil {
		return cause
	}
	if response, ok := errors.AsType[nativeResponseError](cause); ok {
		if response.status == 401 {
			imp.nativeReadPaused = true
			observation := *imp.nativePermission
			observation.Known, observation.MessageRead = true, false
			return errors.Join(cause, imp.store.ObserveNativeSourcePermission(ctx, observation))
		}
		if chatID != "" && (response.status == 403 || response.status == 404) {
			pin, pinned := imp.nativeRouteReads[chatID]
			if !pinned {
				pin, pinned = imp.nativeRoutePins[chatID]
			}
			if pinned {
				return errors.Join(cause, imp.store.RejectNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: imp.nativePermission.NativeIdentity, Scopes: []store.NativeMessageScopeLookup{pin}}))
			}
			observation := store.NativeMessageScopeInventory{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: sourceTypeBeeper, NativeIdentity: imp.nativePermission.NativeIdentity, Kind: "beeper_member", ParentProviderID: imp.nativePermission.NativeIdentity, RetiredProviderIDs: []string{chatID}}
			return errors.Join(cause, imp.store.ObserveNativeMessageScopes(ctx, observation))
		}
	}
	return imp.pauseNativeBeeperAuthority(ctx, cause)
}
