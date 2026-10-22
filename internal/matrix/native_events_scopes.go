package matrix

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/store"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (imp *Importer) pinNativeMatrixAuthority(ctx context.Context, sourceID int64, state *SyncState, opts ImportOptions) error {
	source, err := imp.store.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return err
	}
	imp.nativePermission = &store.NativeSourcePermission{SourceID: sourceID, SourceReference: source.Reference, SourceType: SourceType, NativeIdentity: opts.UserID}
	imp.nativeRoomMemberships = map[id.RoomID]store.NativeMessageScopeIdentity{}
	if !imp.store.MCPMessageCaptureEnabled(SourceType) {
		observation := *imp.nativePermission
		imp.nativePermission = nil
		return imp.store.ObserveNativeSourcePermission(ctx, observation)
	}
	// Pin already-known room lifetimes before the first native request. New
	// rooms have no live coverage yet; their initial history stays silent.
	imp.nativeRoomPins = make(map[id.RoomID]store.NativeMessageScopeLookup)
	for roomID := range state.Rooms {
		pin, found, err := imp.store.LookupNativeMessageScope(ctx, sourceID, source.Reference, "matrix_room", roomID)
		if err != nil {
			return err
		}
		if found {
			imp.nativeRoomPins[id.RoomID(roomID)] = pin
		}
	}
	// A standard thread has its own lifetime, independent of the room. Pin
	// existing roots before the sync response that may contain their replies.
	imp.nativeThreadPins = make(map[id.EventID]store.NativeMessageScopeLookup)
	pins, err := imp.store.ListNativeMessageScopes(ctx, sourceID, source.Reference)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		if pin.Identity.Kind == "matrix_thread" {
			imp.nativeThreadPins[id.EventID(pin.Identity.ProviderID)] = pin
		}
	}
	me, err := imp.runtime.Client.Whoami(ctx)
	if err != nil {
		return imp.nativeMatrixAuthorityError(ctx, err, false)
	}
	if me.UserID.String() != opts.UserID || imp.runtime.Client.DeviceID != "" && me.DeviceID != imp.runtime.Client.DeviceID {
		return imp.nativeMatrixAuthorityError(ctx, errors.New("native_account_identity_mismatch"), true)
	}
	return nil
}
func (imp *Importer) nativeMatrixAuthorityError(ctx context.Context, cause error, identityMismatch bool) error {
	if imp.nativePermission == nil {
		return cause
	}
	observation := *imp.nativePermission
	observation.MessageRead = false
	var response mautrix.HTTPError
	observation.Known = identityMismatch || errors.Is(cause, mautrix.MForbidden) || errors.Is(cause, mautrix.MUnknownToken) || errors.As(cause, &response) && response.IsStatus(http.StatusUnauthorized)
	return errors.Join(cause, imp.store.ObserveNativeSourcePermission(ctx, observation))
}
func (imp *Importer) observeNativeMatrixSync(ctx context.Context, response *mautrix.RespSync, complete bool, state *SyncState, opts ImportOptions) error {
	if imp.nativePermission == nil {
		return nil
	}
	imp.nativePermission.Known = true
	imp.nativePermission.MessageRead = true
	if err := imp.store.ObserveNativeSourcePermission(ctx, *imp.nativePermission); err != nil {
		return err
	}
	observation := store.NativeMessageScopeInventory{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: SourceType, NativeIdentity: opts.UserID, Kind: "matrix_room", Complete: complete, IdentityOnly: true}
	retired := map[string]bool{}
	for roomID := range response.Rooms.Leave {
		retired[roomID.String()] = true
	}
	for roomID := range state.Rooms {
		if !roomIncluded(roomID, opts.Rooms, opts.ExcludeRooms) {
			retired[roomID] = true
		}
	}
	for roomID, room := range response.Rooms.Join {
		if !roomIncluded(roomID.String(), opts.Rooms, opts.ExcludeRooms) {
			retired[roomID.String()] = true
			continue
		}
		name, present := roomTitle(room.State.Events)
		if !present {
			old, found, err := imp.store.LookupNativeMessageScope(ctx, observation.SourceID, observation.SourceReference, "matrix_room", roomID.String())
			if err != nil {
				return err
			}
			if found {
				name = old.DisplayName
			}
		}
		if name == "" {
			name = roomID.String()
		}
		observation.Scopes = append(observation.Scopes, store.NativeMessageScope{ProviderID: roomID.String(), DisplayName: name})
	}
	for roomID := range retired {
		observation.RetiredProviderIDs = append(observation.RetiredProviderIDs, roomID)
	}
	return imp.store.ObserveNativeMessageScopes(ctx, observation)
}

// A nil membership is the valid archive-only result when capture has no admitted permission.
//
//nolint:nilnil // Optional scope evidence must not manufacture authority or reject archive-only ingestion.
func (imp *Importer) nativeMatrixMessageMembership(ctx context.Context, evt *event.Event, content *event.MessageEventContent) (*store.NativeMessageScopeMembership, error) {
	if imp.nativePermission == nil || content == nil {
		return nil, nil
	}
	room, ok := imp.nativeRoomMemberships[evt.RoomID]
	if !ok {
		return nil, errors.New("native_room_identity_unavailable")
	}
	result := &store.NativeMessageScopeMembership{SourceReference: imp.nativePermission.SourceReference, Scopes: []store.NativeMessageScopeIdentity{room}}
	root := content.RelatesTo.GetThreadParent()
	if root == "" {
		if content.RelatesTo != nil && content.RelatesTo.Type != "" {
			return result, nil
		}
		root = evt.ID
	}
	thread, pinned := imp.nativeThreadPins[root]
	if !pinned {
		observation := store.NativeMessageScopeInventory{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: SourceType, NativeIdentity: imp.nativePermission.NativeIdentity, Kind: "matrix_thread", ParentProviderID: evt.RoomID.String(), IdentityOnly: true, Scopes: []store.NativeMessageScope{{ProviderID: root.String(), DisplayName: root.String()}}}
		if err := imp.store.ObserveNativeMessageScopes(ctx, observation); err != nil {
			return nil, err
		}
		var found bool
		var err error
		thread, found, err = imp.store.LookupNativeMessageScope(ctx, observation.SourceID, observation.SourceReference, "matrix_thread", root.String())
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("native_thread_identity_unavailable")
		}
	}
	if thread.Identity.ParentProviderID != evt.RoomID.String() {
		return nil, errors.New("native_thread_identity_unavailable")
	}
	confirmed, err := imp.store.ConfirmNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: SourceType, NativeIdentity: imp.nativePermission.NativeIdentity, Scopes: []store.NativeMessageScopeLookup{{Identity: room, Present: true}, thread}})
	if err != nil {
		return nil, err
	}
	result.Scopes = confirmed
	imp.nativeThreadPins[root] = store.NativeMessageScopeLookup{Identity: confirmed[1], Present: true}
	return result, nil
}

func (imp *Importer) nativeMatrixRoomReadError(ctx context.Context, pin store.NativeMessageScopeLookup, pinned bool, cause error, missingSelf bool) error {
	if imp.nativePermission == nil {
		return cause
	}
	var response mautrix.HTTPError
	if errors.Is(cause, mautrix.MUnknownToken) || errors.As(cause, &response) && response.IsStatus(http.StatusUnauthorized) {
		return imp.nativeMatrixAuthorityError(ctx, cause, false)
	}
	denied := missingSelf || errors.As(cause, &response) && response.IsStatus(http.StatusForbidden)
	if denied && pinned {
		read := store.NativeMessageScopeRead{SourceID: imp.nativePermission.SourceID, SourceReference: imp.nativePermission.SourceReference, SourceType: SourceType, NativeIdentity: imp.nativePermission.NativeIdentity, Scopes: []store.NativeMessageScopeLookup{pin}}
		return errors.Join(cause, imp.store.RejectNativeMessageScopeRead(ctx, read))
	}
	return imp.pauseNativeMatrixAuthority(ctx, cause)
}

func (imp *Importer) pauseNativeMatrixAuthority(ctx context.Context, cause error) error {
	if imp.nativePermission == nil {
		return cause
	}
	observation := *imp.nativePermission
	observation.Known, observation.MessageRead = false, false
	return errors.Join(cause, imp.store.ObserveNativeSourcePermission(ctx, observation))
}
