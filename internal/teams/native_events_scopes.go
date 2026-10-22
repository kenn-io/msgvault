package teams

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

func nativeTeamsGrant(c *Client) (known, read bool) {
	if c.grantedScopes == nil {
		return false, false
	}
	scopes, err := c.grantedScopes()
	if err != nil {
		return false, false
	}
	known = len(scopes) > 0
	for _, scope := range scopes {
		switch scope {
		case "Chat.Read", "Chat.ReadWrite", "https://graph.microsoft.com/Chat.Read", "https://graph.microsoft.com/Chat.ReadWrite":
			read = true
		}
	}
	return known, read
}

func nativeTeamsDenied(err error) bool {
	if errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, msgraph.ErrForbidden) {
		return true
	}
	return nativeTeamsInvalidGrant(err)
}

func nativeTeamsInvalidGrant(err error) bool {
	refresh, ok := errors.AsType[*oauth2.RetrieveError](err)
	return ok && refresh.ErrorCode == "invalid_grant"
}

// Bind saved native grant evidence to the actual Graph account before any
// message work. Unknown legacy metadata can archive without Events authority.
func (imp *Importer) observeNativeTeamsPermission(ctx context.Context, source *store.Source) error {
	resource, err := imp.store.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return err
	}
	permission := &store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeTeams, NativeIdentity: source.Identifier}
	imp.nativePermission = permission
	imp.nativeSourceRef = ""
	imp.nativeSelfMember = nil
	permission.Known, permission.MessageRead = nativeTeamsGrant(imp.client)
	if permission.MessageRead {
		var profile GraphUser
		profileErr := imp.client.GetJSON(ctx, "/me?$select=id,mail,userPrincipalName,proxyAddresses", &profile)
		if profileErr != nil {
			return errors.Join(profileErr, imp.recheckNativeTeamsPermission(ctx))
		}
		permission.Known, permission.MessageRead = nativeTeamsGrant(imp.client)
		if profile.ID == "" || !microsoft.MatchesMailboxIdentity(source.Identifier, profile.Mail, profile.UserPrincipalName, profile.ProxyAddresses) {
			permission.Known, permission.MessageRead = true, false
			return errors.Join(errors.New("native_authority_unavailable"), imp.store.ObserveNativeSourcePermission(ctx, *permission))
		}
		imp.nativeSelfMember = &ChatMember{UserID: profile.ID, DisplayName: profile.DisplayName, Email: source.Identifier}
	}
	if err := imp.store.ObserveNativeSourcePermission(ctx, *permission); err != nil {
		return err
	}
	if permission.MessageRead {
		imp.nativeSourceRef = resource.Reference
	}
	return nil
}

// New authority is admitted only by the next account-verified sync. Reductions
// observed after token refresh pause publication before its SQL transaction.
func (imp *Importer) recheckNativeTeamsPermission(ctx context.Context) error {
	if imp.nativePermission == nil {
		return nil
	}
	known, read := nativeTeamsGrant(imp.client)
	if read && !imp.nativePermission.MessageRead {
		return nil
	}
	if known == imp.nativePermission.Known && read == imp.nativePermission.MessageRead {
		return nil
	}
	next := *imp.nativePermission
	next.Known, next.MessageRead = known, read
	if err := imp.store.ObserveNativeSourcePermission(ctx, next); err != nil {
		return err
	}
	*imp.nativePermission = next
	if !read {
		imp.nativeSourceRef = ""
	}
	return nil
}

// ListChats covers native chat identities independently of message budgets.
// The self chat has a separate probe; a failed probe cannot assert absence.
func (imp *Importer) observeNativeTeamsChats(ctx context.Context, sourceID int64, identity string, chats []Chat, complete bool) error {
	if err := imp.recheckNativeTeamsPermission(ctx); err != nil {
		return err
	}
	if imp.nativeSourceRef == "" {
		return nil
	}
	observation := store.NativeMessageScopeInventory{SourceID: sourceID, SourceReference: imp.nativeSourceRef, SourceType: sourceTypeTeams, NativeIdentity: identity, Kind: "teams_chat", Complete: complete, IdentityOnly: true}
	for _, chat := range chats {
		observation.Scopes = append(observation.Scopes, store.NativeMessageScope{ProviderID: chat.ID, DisplayName: chat.Topic})
	}
	return imp.store.ObserveNativeMessageScopes(ctx, observation)
}

// A positive native gone/denied read retires this exact chat, independently of
// an earlier listing. Transient failures do not establish native removal.
func (imp *Importer) retireNativeTeamsChat(ctx context.Context, read *store.NativeMessageScopeRead) error {
	if read == nil {
		return nil
	}
	return imp.store.RejectNativeMessageScopeRead(ctx, *read)
}

// Pin the chat lifetime before message IO. Metadata enumeration may identify
// a chat, but cannot make a stale content response authorize its replacement.
//
//nolint:nilnil // Unselected native capture has no optional read binding and remains archive-only.
func (imp *Importer) pinNativeTeamsChatRead(ctx context.Context, sourceID int64, identity, chat string) (*store.NativeMessageScopeRead, error) {
	if imp.nativeSourceRef == "" {
		return nil, nil
	}
	pin, found, err := imp.store.LookupNativeMessageScope(ctx, sourceID, imp.nativeSourceRef, "teams_chat", chat)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("native_chat_unavailable")
	}
	return &store.NativeMessageScopeRead{SourceID: sourceID, SourceReference: imp.nativeSourceRef, SourceType: sourceTypeTeams, NativeIdentity: identity, Scopes: []store.NativeMessageScopeLookup{pin}}, nil
}

// Only an unchanged pre-request lifetime can be restored by a successful read.
// Reductions of the native grant still preserve ordinary archive behavior.
func (imp *Importer) observeNativeTeamsChatRead(ctx context.Context, read *store.NativeMessageScopeRead) ([]store.NativeMessageScopeIdentity, error) {
	if err := imp.recheckNativeTeamsPermission(ctx); err != nil {
		return nil, err
	}
	if imp.nativeSourceRef == "" || read == nil {
		return nil, nil
	}
	return imp.store.ConfirmNativeMessageScopeRead(ctx, *read)
}
