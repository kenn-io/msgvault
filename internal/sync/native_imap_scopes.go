package sync

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"go.kenn.io/msgvault/internal/gmail"

	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
)

type nativeIMAPMailboxScopes interface {
	NativeMailboxSourceIdentity() string
	NativeMailboxInventoryComplete() bool
	NativeMailboxInventory(ctx context.Context) (string, []imapclient.NativeMailboxScope, []string, error)
	NativeMailboxSelectedEpochs() map[string]uint32
	NativeMailboxMemberships(sourceMessageID string, raw []byte) ([]imapclient.NativeMailboxScope, error)
}

func (s *Syncer) observeNativeIMAPMailboxInventory(ctx context.Context, sourceID int64) error {
	if s.opts.SourceType != sourceTypeIMAP || !s.store.MCPMessageCaptureEnabled(sourceTypeIMAP) {
		return nil
	}
	native, ok := s.client.(nativeIMAPMailboxScopes)
	if !ok {
		return nil
	}
	source, err := s.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return err
	}
	resource, err := s.store.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return err
	}
	account, scopes, retired, probeErr := native.NativeMailboxInventory(ctx)
	permission := store.NativeSourcePermission{SourceID: sourceID, SourceReference: resource.Reference, SourceType: sourceTypeIMAP, NativeIdentity: source.Identifier, Known: len(scopes) > 0, MessageRead: len(scopes) > 0}
	if !store.EqualIdentifier(source.Identifier, account) {
		permission.Known, permission.MessageRead = true, false
		// Ordinary archive aliases do not grant Events to a different account.
		return s.store.ObserveNativeSourcePermission(ctx, permission)
	}
	inventory := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: resource.Reference, SourceType: sourceTypeIMAP, NativeIdentity: source.Identifier, Kind: "imap_mailbox", Complete: probeErr == nil && native.NativeMailboxInventoryComplete(), RetiredProviderIDs: retired}
	if probeErr != nil {
		pinned := s.nativeSourcePermission
		denialErr := s.observeNativeSourceDenial(ctx, probeErr)
		if denialErr != nil || (pinned != nil && s.nativeSourcePermission == nil) {
			return errors.Join(probeErr, denialErr)
		}
		if len(retired) > 0 {
			return errors.Join(probeErr, s.store.ObserveNativeMessageScopes(ctx, inventory))
		}
		return probeErr
	}
	// An empty bounded view cannot revoke or invent account-wide read authority.
	if len(scopes) > 0 || native.NativeMailboxInventoryComplete() {
		if err := s.store.ObserveNativeSourcePermission(ctx, permission); err != nil {
			return err
		}
	}
	for _, scope := range scopes {
		inventory.Scopes = append(inventory.Scopes, store.NativeMessageScope{ProviderID: scope.Mailbox, DisplayName: scope.Mailbox, Epoch: strconv.FormatUint(uint64(scope.UIDValidity), 10)})
	}
	if err := s.store.ObserveNativeMessageScopes(ctx, inventory); err != nil {
		return err
	}
	s.nativeIMAPSourceReference = resource.Reference
	s.nativeIMAPAuthority = &nativeIMAPAuthorityState{sourceID: sourceID, sourceReference: resource.Reference, identity: source.Identifier, epochs: map[string]uint32{}, pins: map[string]store.NativeMessageScopeIdentity{}}
	for _, scope := range scopes {
		s.nativeIMAPAuthority.epochs[scope.Mailbox] = scope.UIDValidity
		pin, found, err := s.store.LookupNativeMessageScope(ctx, sourceID, resource.Reference, "imap_mailbox", scope.Mailbox)
		if err != nil {
			return err
		}
		if !found || !pin.Present {
			return errors.New("native_mailbox_unavailable")
		}
		s.nativeIMAPAuthority.pins[scope.Mailbox] = pin.Identity
	}
	return nil
}

//nolint:nilnil // No admitted mailbox authority is a valid archive-only snapshot without Events membership.
func (s *Syncer) nativeIMAPSnapshotScopes(data *messageData) (*store.NativeMessageScopeMembership, error) {
	if s.nativeIMAPSourceReference == "" {
		return nil, nil
	}
	native, ok := s.client.(nativeIMAPMailboxScopes)
	if !ok {
		return nil, errors.New("native_scope_membership_unavailable")
	}
	scopes, err := native.NativeMailboxMemberships(data.message.SourceMessageID, data.rawMIME)
	if err != nil {
		return nil, err
	}
	membership := &store.NativeMessageScopeMembership{SourceReference: s.nativeIMAPSourceReference}
	state := s.nativeIMAPAuthority
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, scope := range scopes {
		pin, found := state.pins[scope.Mailbox]
		if !found || pin.Epoch != strconv.FormatUint(uint64(scope.UIDValidity), 10) {
			return nil, errors.New("native_mailbox_unavailable")
		}
		membership.Scopes = append(membership.Scopes, pin)
	}
	return membership, nil
}

// Positive native login denial retires the pinned account lifetime. A failed
// network request or unavailable mailbox is not proof of account revocation.
func (s *Syncer) nativeIMAPProfileWithPermission(ctx context.Context, source *store.Source) (*gmail.Profile, error) {
	native, ok := s.client.(nativeIMAPMailboxScopes)
	if !ok {
		return s.client.GetProfile(ctx)
	}
	resource, err := s.store.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeIMAP, NativeIdentity: source.Identifier}
	if resource.SourceType != sourceTypeIMAP || !store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		return nil, errors.New("native_authority_unavailable")
	}
	if !store.EqualIdentifier(native.NativeMailboxSourceIdentity(), source.Identifier) {
		// Legacy archive aliases can still sync. Their selected native route cannot
		// authorize Events for a different account identity, including old grants.
		observation.Known = true
		if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
			return nil, err
		}
		return s.client.GetProfile(ctx)
	}
	s.nativeSourcePermission = &observation
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		return nil, errors.Join(err, s.observeNativeSourceDenial(ctx, err))
	}
	return profile, err
}

// Per-run state serializes snapshots of native SELECT evidence before applying
// them under Store fences. This prevents concurrent workers from applying an
// older snapshot after a newer one. The client mutex is released before SQL.
type nativeIMAPAuthorityState struct {
	mu                        sync.Mutex
	sourceID                  int64
	sourceReference, identity string
	epochs                    map[string]uint32
	pins                      map[string]store.NativeMessageScopeIdentity
}

func (s *Syncer) observeNativeIMAPSelectedEpochs(ctx context.Context) error {
	state := s.nativeIMAPAuthority
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	native, ok := s.client.(nativeIMAPMailboxScopes)
	if !ok {
		return errors.New("native_scope_membership_unavailable")
	}
	observed := native.NativeMailboxSelectedEpochs()
	inventory := store.NativeMessageScopeInventory{SourceID: state.sourceID, SourceReference: state.sourceReference, SourceType: sourceTypeIMAP, NativeIdentity: state.identity, Kind: "imap_mailbox"}
	for mailbox, epoch := range observed {
		prior, eligible := state.epochs[mailbox]
		if eligible && epoch != 0 && prior != epoch {
			inventory.Scopes = append(inventory.Scopes, store.NativeMessageScope{ProviderID: mailbox, DisplayName: mailbox, Epoch: strconv.FormatUint(uint64(epoch), 10)})
		}
	}
	if len(inventory.Scopes) == 0 {
		return nil
	}
	if err := s.store.ObserveNativeMessageScopes(ctx, inventory); err != nil {
		return err
	}
	for _, scope := range inventory.Scopes {
		pin, found, err := s.store.LookupNativeMessageScope(ctx, state.sourceID, state.sourceReference, "imap_mailbox", scope.ProviderID)
		if err != nil {
			return err
		}
		if !found || !pin.Present || pin.Identity.Epoch != scope.Epoch {
			return errors.New("native_mailbox_unavailable")
		}
		state.epochs[scope.ProviderID] = observed[scope.ProviderID]
		state.pins[scope.ProviderID] = pin.Identity
	}
	return nil
}
