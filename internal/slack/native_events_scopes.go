package slack

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

// Pin before provider IO and apply only through this run's scoped Store. An
// authenticated response for another workspace/user cannot authorize the route
// selected by configuration or command arguments.
func (imp *Importer) pinNativeSlackAccount(ctx context.Context, source *store.Source, opts ImportOptions) error {
	resource, err := imp.store.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return err
	}
	observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeSlack, NativeIdentity: source.Identifier}
	auth, probeErr := imp.client.AuthTest(ctx)
	if probeErr != nil {
		observation.Known = errors.Is(probeErr, ErrAuth) || errors.Is(probeErr, ErrCredentialRevoked)
		return errors.Join(probeErr, imp.store.ObserveNativeSourcePermission(ctx, observation))
	}
	if auth.TeamID != opts.TeamID || auth.UserID != opts.UserID {
		observation.Known = true
		return errors.Join(errors.New("native Slack account does not match selected source"), imp.store.ObserveNativeSourcePermission(ctx, observation))
	}
	observation.Known = imp.client.scopesKnown
	observation.MessageRead = observation.Known && nativeSlackMessageRead(imp.client.scopes)
	observation.DeniedScopeDomains = nativeSlackDeniedReadDomains(observation.Known, imp.client.scopes)
	if err := imp.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return err
	}
	imp.nativeSlackPermissionKnown, imp.nativeSlackMessageRead = observation.Known, observation.MessageRead
	imp.nativeSlackDeniedDomains = observation.DeniedScopeDomains
	imp.nativeSlackSourceReference = resource.Reference
	imp.nativeSlackScopePins = make(map[string]store.NativeMessageScopeLookup)
	return nil
}

func nativeSlackMessageRead(scopes []string) bool {
	for _, scope := range []string{"channels:history", "groups:history", "im:history", "mpim:history"} {
		if slices.Contains(scopes, scope) {
			return true
		}
	}
	return false
}

func nativeSlackChannelRead(channel Conversation, known bool, scopes []string) bool {
	if !known {
		return false
	}
	prefix := nativeSlackPermissionDomain(channel)
	return slices.Contains(scopes, prefix+":read") && slices.Contains(scopes, prefix+":history")
}

func nativeSlackPermissionDomain(channel Conversation) string {
	prefix := "channels"
	switch {
	case channel.IsIM:
		prefix = "im"
	case channel.IsMpim:
		prefix = "mpim"
	case channel.IsPrivate:
		prefix = "groups"
	}
	return prefix
}

func nativeSlackDeniedReadDomains(known bool, scopes []string) []string {
	if !known {
		return nil
	}
	var denied []string
	for _, domain := range []string{"channels", "groups", "im", "mpim"} {
		if !slices.Contains(scopes, domain+":read") || !slices.Contains(scopes, domain+":history") {
			denied = append(denied, domain)
		}
	}
	return denied
}

func (imp *Importer) refreshNativeSlackPermission(ctx context.Context, sourceID int64, identity string) error {
	known, allowed := imp.client.scopesKnown, imp.client.scopesKnown && nativeSlackMessageRead(imp.client.scopes)
	denied := nativeSlackDeniedReadDomains(known, imp.client.scopes)
	if imp.nativeSlackPermissionKnown == known && imp.nativeSlackMessageRead == allowed && slices.Equal(imp.nativeSlackDeniedDomains, denied) {
		return nil
	}
	observation := store.NativeSourcePermission{SourceID: sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Known: known, MessageRead: allowed, DeniedScopeDomains: denied}
	if err := imp.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return err
	}
	imp.nativeSlackPermissionKnown, imp.nativeSlackMessageRead = known, allowed
	imp.nativeSlackDeniedDomains = denied
	return nil
}

func (imp *Importer) observeNativeSlackChannels(ctx context.Context, sourceID int64, opts ImportOptions, channels []Conversation, complete bool) error {
	// Credential replacement preserves the archived owner. AuthTest separately
	// verifies the current credential before any native permission observation.
	identity := opts.TeamID + ":" + imp.ownerID
	if err := imp.refreshNativeSlackPermission(ctx, sourceID, identity); err != nil {
		return err
	}
	inventory := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Kind: "slack_channel", Complete: complete && imp.client.scopesKnown}
	imp.nativeSlackChannels = map[string]Conversation{}
	for _, channel := range channels {
		imp.nativeSlackChannels[channel.ID] = channel
		included := includeConversation(&channel, &opts)
		if included && nativeSlackChannelRead(channel, imp.client.scopesKnown, imp.client.scopes) {
			inventory.Scopes = append(inventory.Scopes, store.NativeMessageScope{ProviderID: channel.ID, DisplayName: conversationTitle(&channel, imp.res.displayName), PermissionDomain: nativeSlackPermissionDomain(channel)})
		} else if !included || imp.client.scopesKnown {
			inventory.RetiredProviderIDs = append(inventory.RetiredProviderIDs, channel.ID)
		}
	}
	if !inventory.Complete && len(inventory.Scopes) == 0 && len(inventory.RetiredProviderIDs) == 0 {
		return nil
	}
	if err := imp.store.ObserveNativeMessageScopes(ctx, inventory); err != nil {
		return err
	}
	pins, err := imp.store.ListNativeMessageScopes(ctx, sourceID, imp.nativeSlackSourceReference)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		imp.nativeSlackScopePins[pin.Identity.Kind+"/"+pin.Identity.ProviderID] = pin
	}
	return nil
}

// The membership lookup and event capture share PersistMessage's transaction.
// Native scope observation happens before it, with no provider IO under fences.
func (imp *Importer) nativeSlackMessageScopes(ctx context.Context, cc *convScope, m *Message) (*store.NativeMessageScopeMembership, bool, error) {
	if imp.nativeSlackSourceReference == "" {
		return nil, true, nil
	} // Offline exports keep their separate import contract.
	identity := cc.opts.TeamID + ":" + imp.ownerID
	if err := imp.refreshNativeSlackPermission(ctx, cc.sourceID, identity); err != nil {
		return nil, false, err
	}
	channel, exists := imp.nativeSlackChannels[cc.channelID]
	if !exists {
		return nil, false, errors.New("native Slack channel unavailable")
	}
	if !nativeSlackChannelRead(channel, imp.client.scopesKnown, imp.client.scopes) {
		if imp.client.scopesKnown {
			inventory := store.NativeMessageScopeInventory{SourceID: cc.sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Kind: "slack_channel", RetiredProviderIDs: []string{cc.channelID}}
			if err := imp.store.ObserveNativeMessageScopes(ctx, inventory); err != nil {
				return nil, false, err
			}
		}
		return nil, false, nil
	}
	root := m.ThreadTS
	if root == "" {
		root = m.TS
	}
	if tsTime(root).IsZero() {
		return nil, false, errors.New("native Slack thread identity unavailable")
	}
	threadID := sourceMessageID(cc.channelID, root)
	parent, pinned := imp.nativeSlackScopePins["slack_channel/"+cc.channelID]
	if !pinned {
		return nil, false, errors.New("native_parent_unavailable")
	}
	pins := []store.NativeMessageScopeLookup{parent}
	thread, pinned := imp.nativeSlackScopePins["slack_thread/"+threadID]
	if pinned {
		pins = append(pins, thread)
	}
	confirmed, err := imp.store.ConfirmNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: cc.sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Scopes: pins})
	if err != nil {
		return nil, false, err
	}
	for _, scope := range confirmed {
		imp.nativeSlackScopePins[scope.Kind+"/"+scope.ProviderID] = store.NativeMessageScopeLookup{Identity: scope, Present: true}
	}
	if !pinned {
		inventory := store.NativeMessageScopeInventory{SourceID: cc.sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Kind: "slack_thread", ParentProviderID: cc.channelID, IdentityOnly: true, Scopes: []store.NativeMessageScope{{ProviderID: threadID, DisplayName: "Thread " + root}}}
		if err := imp.store.ObserveNativeMessageScopes(ctx, inventory); err != nil {
			return nil, false, err
		}
		thread, found, err := imp.store.LookupNativeMessageScope(ctx, cc.sourceID, imp.nativeSlackSourceReference, "slack_thread", threadID)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, errors.New("native_thread_unavailable")
		}
		confirmed, err = imp.store.ConfirmNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: cc.sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: identity, Scopes: []store.NativeMessageScopeLookup{{Identity: confirmed[0], Present: true}, thread}})
		if err != nil {
			return nil, false, err
		}
		imp.nativeSlackScopePins["slack_thread/"+threadID] = store.NativeMessageScopeLookup{Identity: confirmed[1], Present: true}
	}
	return &store.NativeMessageScopeMembership{SourceReference: imp.nativeSlackSourceReference, Scopes: confirmed}, true, nil
}

// A native read can positively prove disappearance after enumeration. Retire
// only that exact channel or root; archive rows and other scopes remain intact.
func (imp *Importer) retireNativeSlackReadScope(ctx context.Context, channelID, root string) error {
	if imp.nativeSlackSourceReference == "" {
		return nil
	}
	kind, providerID := "slack_channel", channelID
	if root != "" {
		kind, providerID = "slack_thread", sourceMessageID(channelID, root)
	}
	pin, pinned := imp.nativeSlackScopePins[kind+"/"+providerID]
	if !pinned {
		return nil
	}
	if err := imp.store.RejectNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: imp.sourceID, SourceReference: imp.nativeSlackSourceReference, SourceType: sourceTypeSlack, NativeIdentity: imp.opts.TeamID + ":" + imp.ownerID, Scopes: []store.NativeMessageScopeLookup{pin}}); err != nil {
		return err
	}
	// Repeated positive failures in this run refer to the same denied lifetime.
	// Update its expected presence without adopting any replacement generation.
	for key, observed := range imp.nativeSlackScopePins {
		if key == kind+"/"+providerID || kind == "slack_channel" && observed.Identity.Kind == "slack_thread" && observed.Identity.ParentProviderID == channelID {
			observed.Present = false
			imp.nativeSlackScopePins[key] = observed
		}
	}
	return nil
}
