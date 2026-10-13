package discord

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"go.kenn.io/msgvault/internal/store"
)

// NativeGuildMember and NativeGuildRole retain only the read permission inputs
// returned by Discord. No requested scopes or archive rows establish authority.
type NativeGuildMember struct {
	User  User     `json:"user"`
	Roles []string `json:"roles"`
}
type NativeGuildRole struct {
	ID          string `json:"id"`
	Permissions string `json:"permissions"`
}

type nativeGuildPermissionAPI interface {
	GuildMember(ctx context.Context, guildID, userID string) (NativeGuildMember, error)
	GuildRoles(ctx context.Context, guildID string) ([]NativeGuildRole, error)
}

func (c *Client) GuildMember(ctx context.Context, guildID, userID string) (NativeGuildMember, error) {
	var out NativeGuildMember
	guild, err := snowflakePathValue("guild ID", guildID)
	if err != nil {
		return out, err
	}
	user, err := snowflakePathValue("user ID", userID)
	if err != nil {
		return out, err
	}
	err = c.getJSON(ctx, discordRoute{"guild member", "/guilds/" + guild + "/members/" + user, "GET /guilds/:guild/members/:user", guildID}, nil, &out)
	return out, err
}
func (c *Client) GuildRoles(ctx context.Context, guildID string) ([]NativeGuildRole, error) {
	var out []NativeGuildRole
	guild, err := snowflakePathValue("guild ID", guildID)
	if err != nil {
		return nil, err
	}
	err = c.getJSON(ctx, discordRoute{"guild roles", "/guilds/" + guild + "/roles", "GET /guilds/:guild/roles", guildID}, nil, &out)
	return out, err
}

var errNativeDiscordIdentityMismatch = errors.New("native_account_identity_mismatch")

type nativeDiscordAuthority struct {
	permission store.NativeSourcePermission
	member     NativeGuildMember
	roles      map[string]uint64
	base       uint64
	parents    map[string]Channel
}

const (
	nativeDiscordAdministrator uint64 = 1 << 3
	nativeDiscordViewChannel   uint64 = 1 << 10
	nativeDiscordReadHistory   uint64 = 1 << 16
)

func (imp *Importer) pinNativeDiscordAuthority(ctx context.Context, sourceID int64, opts ImportOptions) error {
	resource, err := imp.store.GetMCPResource(ctx, "source", sourceID)
	if err != nil {
		return err
	}
	imp.nativeAuthority = &nativeDiscordAuthority{permission: store.NativeSourcePermission{SourceID: sourceID, SourceReference: resource.Reference, SourceType: sourceTypeDiscord, NativeIdentity: opts.GuildID}, roles: map[string]uint64{}, parents: map[string]Channel{}}
	return nil
}

func (imp *Importer) observeNativeDiscordAuthority(ctx context.Context, sourceID int64, opts ImportOptions, guild Guild) error {
	a := imp.nativeAuthority
	if a == nil || a.permission.SourceID != sourceID {
		return errors.New("native_authority_unavailable")
	}

	if !imp.store.MCPMessageCaptureEnabled(sourceTypeDiscord) {
		return imp.store.ObserveNativeSourcePermission(ctx, a.permission)
	}
	api, ok := imp.api.(nativeGuildPermissionAPI)
	if opts.BotUserID == "" || !ok {
		return imp.store.ObserveNativeSourcePermission(ctx, a.permission)
	}
	me, err := imp.api.Me(ctx)
	if err != nil {
		return imp.nativeDiscordAuthorityError(ctx, err)
	}
	if !me.Bot || me.ID != opts.BotUserID || guild.ID != opts.GuildID {
		return imp.nativeDiscordAuthorityError(ctx, errNativeDiscordIdentityMismatch)
	}
	a.member, err = api.GuildMember(ctx, opts.GuildID, me.ID)
	if err != nil {
		return imp.nativeDiscordAuthorityError(ctx, err)
	}
	if a.member.User.ID != me.ID {
		return imp.nativeDiscordAuthorityError(ctx, errNativeDiscordIdentityMismatch)
	}
	roles, err := api.GuildRoles(ctx, opts.GuildID)
	if err != nil {
		return imp.nativeDiscordAuthorityError(ctx, err)
	}
	for _, role := range roles {
		bits, err := strconv.ParseUint(role.Permissions, 10, 64)
		if err != nil || role.ID == "" {
			return imp.nativeDiscordAuthorityError(ctx, errors.New("native_permission_unavailable"))
		}
		if _, duplicate := a.roles[role.ID]; duplicate {
			return imp.nativeDiscordAuthorityError(ctx, errors.New("native_permission_unavailable"))
		}
		a.roles[role.ID] = bits
	}
	base, ok := a.roles[opts.GuildID]
	if !ok {
		return imp.nativeDiscordAuthorityError(ctx, errors.New("native_permission_unavailable"))
	}
	for _, id := range a.member.Roles {
		bits, ok := a.roles[id]
		if !ok {
			return imp.nativeDiscordAuthorityError(ctx, errors.New("native_permission_unavailable"))
		}
		base |= bits
	}
	if guild.OwnerID == me.ID {
		base |= nativeDiscordAdministrator
	}
	a.base = base
	a.permission.Known = true
	a.permission.MessageRead = true
	return imp.store.ObserveNativeSourcePermission(ctx, a.permission)
}

func (imp *Importer) nativeDiscordAuthorityError(ctx context.Context, err error) error {
	a := imp.nativeAuthority
	if a == nil {
		return err
	}
	a.permission.MessageRead = false
	var apiErr *APIError
	a.permission.Known = errors.Is(err, errNativeDiscordIdentityMismatch) || errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound)
	return errors.Join(err, imp.store.ObserveNativeSourcePermission(ctx, a.permission))
}

// Discord's documented overwrite order is everyone, aggregate member roles,
// then the member. Threads inherit their exact parent's read permissions.
func (a *nativeDiscordAuthority) canRead(channel Channel) bool {
	if a == nil || !a.permission.Known || !a.permission.MessageRead {
		return false
	}
	bits := a.base
	if bits&nativeDiscordAdministrator != 0 {
		return true
	}
	roleIDs := map[string]bool{}
	for _, id := range a.member.Roles {
		roleIDs[id] = true
	}
	var everyoneAllow, everyoneDeny, roleAllow, roleDeny, memberAllow, memberDeny uint64
	for _, overwrite := range channel.PermissionOverwrites {
		allow, err := strconv.ParseUint(overwrite.Allow, 10, 64)
		if err != nil {
			return false
		}
		deny, err := strconv.ParseUint(overwrite.Deny, 10, 64)
		if err != nil {
			return false
		}
		switch {
		case overwrite.Type == 0 && overwrite.ID == a.permission.NativeIdentity:
			everyoneAllow |= allow
			everyoneDeny |= deny
		case overwrite.Type == 0 && roleIDs[overwrite.ID]:
			roleAllow |= allow
			roleDeny |= deny
		case overwrite.Type == 1 && overwrite.ID == a.member.User.ID:
			memberAllow |= allow
			memberDeny |= deny
		}
	}
	bits = (bits &^ everyoneDeny) | everyoneAllow
	bits = (bits &^ roleDeny) | roleAllow
	bits = (bits &^ memberDeny) | memberAllow
	return bits&(nativeDiscordViewChannel|nativeDiscordReadHistory) == nativeDiscordViewChannel|nativeDiscordReadHistory
}

func (imp *Importer) observeNativeDiscordCatalog(ctx context.Context, channels []Channel, catalog CatalogResult, opts ImportOptions) error {
	a := imp.nativeAuthority
	if a == nil || !a.permission.Known || !a.permission.MessageRead {
		return nil
	}
	// Validate the whole native observation before changing any lifetime. A
	// malformed overwrite is unknown permission, never proof of channel removal.
	for _, channel := range channels {
		for _, overwrite := range channel.PermissionOverwrites {
			if overwrite.Type != 0 && overwrite.Type != 1 {
				return errors.New("native_permission_unavailable")
			}
			if _, err := strconv.ParseUint(overwrite.Allow, 10, 64); err != nil {
				return errors.New("native_permission_unavailable")
			}
			if _, err := strconv.ParseUint(overwrite.Deny, 10, 64); err != nil {
				return errors.New("native_permission_unavailable")
			}
		}
	}
	var verifiedParents []store.NativeMessageScope
	observation := store.NativeMessageScopeInventory{SourceID: a.permission.SourceID, SourceReference: a.permission.SourceReference, SourceType: sourceTypeDiscord, NativeIdentity: opts.GuildID, Kind: "discord_channel", Complete: true, IdentityOnly: true}
	for _, channel := range channels {
		if channel.ID == "" || (channel.GuildID != "" && channel.GuildID != opts.GuildID) {
			return errors.New("native_channel_identity_mismatch")
		}
		if !IsThreadCatalogParent(channel.Type) {
			continue
		}
		a.parents[channel.ID] = channel
		if !ContainerIncluded(opts.GuildConfig, channel.ID, "") || !a.canRead(channel) {
			observation.RetiredProviderIDs = append(observation.RetiredProviderIDs, channel.ID)
			continue
		}
		observation.Scopes = append(observation.Scopes, store.NativeMessageScope{ProviderID: channel.ID, DisplayName: channel.Name})
		// Forum/media parents have no message-list endpoint. Native guild member,
		// role and channel-overwrite evidence is their current read observation.
		if channel.Type == channelTypeGuildForum || channel.Type == channelTypeGuildMedia {
			verifiedParents = append(verifiedParents, store.NativeMessageScope{ProviderID: channel.ID, DisplayName: channel.Name})
		}
	}
	if err := imp.store.ObserveNativeMessageScopes(ctx, observation); err != nil {
		return err
	}
	if len(verifiedParents) > 0 {
		verified := observation
		verified.Complete = false
		verified.IdentityOnly = false
		verified.Scopes = verifiedParents
		verified.RetiredProviderIDs = nil
		if err := imp.store.ObserveNativeMessageScopes(ctx, verified); err != nil {
			return err
		}
	}
	for _, container := range catalog.Containers {
		if !isThreadMessageContainer(container.Channel.Type) {
			continue
		}
		parent, ok := a.parents[container.Channel.ParentID]
		if !ok || !a.canRead(parent) || !ContainerIncluded(opts.GuildConfig, container.Channel.ID, parent.ID) {
			continue
		}
		// Metadata alone never restores a positively denied thread. A successful
		// current content read restores it before coherent message persistence.
		if err := imp.store.ObserveNativeMessageScopes(ctx, store.NativeMessageScopeInventory{SourceID: a.permission.SourceID, SourceReference: a.permission.SourceReference, SourceType: sourceTypeDiscord, NativeIdentity: opts.GuildID, Kind: "discord_thread", ParentProviderID: parent.ID, IdentityOnly: true, Scopes: []store.NativeMessageScope{{ProviderID: container.Channel.ID, DisplayName: container.Channel.Name}}}); err != nil {
			return err
		}
	}
	return nil
}

func (imp *Importer) pinNativeDiscordRead(ctx context.Context, channelID string) ([]store.NativeMessageScopeLookup, error) {
	a := imp.nativeAuthority
	if a == nil || !a.permission.Known || !a.permission.MessageRead {
		return nil, nil
	}
	for _, kind := range []string{"discord_channel", "discord_thread"} {
		binding, found, err := imp.store.LookupNativeMessageScope(ctx, a.permission.SourceID, a.permission.SourceReference, kind, channelID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		parentID := channelID
		if kind == "discord_thread" {
			parentID = binding.Identity.ParentProviderID
		}
		parent, ok := a.parents[parentID]
		if !ok || !a.canRead(parent) {
			continue
		}
		if kind == "discord_thread" {
			parentBinding, found, err := imp.store.LookupNativeMessageScope(ctx, a.permission.SourceID, a.permission.SourceReference, "discord_channel", parentID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, errors.New("native_parent_unavailable")
			}
			return []store.NativeMessageScopeLookup{parentBinding, binding}, nil
		}
		return []store.NativeMessageScopeLookup{binding}, nil
	}
	return nil, nil
}
func (imp *Importer) observeNativeDiscordRead(ctx context.Context, channelID string, pins []store.NativeMessageScopeLookup) error {
	if len(pins) == 0 {
		return nil
	}
	a := imp.nativeAuthority
	scopes, err := imp.store.ConfirmNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: a.permission.SourceID, SourceReference: a.permission.SourceReference, SourceType: sourceTypeDiscord, NativeIdentity: a.permission.NativeIdentity, Scopes: pins})
	if err != nil {
		return err
	}
	imp.nativeMemberships[channelID] = scopes
	return nil
}

// A nil membership is the valid archive-only result when no native scope was admitted.
//
//nolint:nilnil // Optional scope evidence must not manufacture authority or reject archive-only ingestion.
func (imp *Importer) nativeDiscordMembership(message *Message) (*store.NativeMessageScopeMembership, error) {
	a := imp.nativeAuthority
	if a == nil {
		return nil, nil
	}
	if message.GuildID != "" && message.GuildID != a.permission.NativeIdentity {
		return nil, errors.New("native_message_guild_mismatch")
	}
	scopes := imp.nativeMemberships[message.ChannelID]
	if len(scopes) == 0 {
		return nil, nil
	}
	return &store.NativeMessageScopeMembership{SourceReference: a.permission.SourceReference, Scopes: scopes}, nil
}

func (imp *Importer) nativeDiscordMessages(ctx context.Context, channelID string, query MessageQuery) ([]Message, error) {
	pins, err := imp.pinNativeDiscordRead(ctx, channelID)
	if err != nil {
		return nil, err
	}
	messages, err := imp.api.Messages(ctx, channelID, query)
	if err != nil {
		if _, classified := newContainerIssue(channelID, err); classified {
			if retireErr := imp.retireNativeDiscordContainer(ctx, channelID, pins); retireErr != nil {
				return nil, errors.Join(err, retireErr)
			}
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
			return nil, imp.nativeDiscordAuthorityError(ctx, err)
		}
		return nil, err
	}
	for _, message := range messages {
		if err := validateDiscordMessage(channelID, message); err != nil {
			return nil, err
		}
		if a := imp.nativeAuthority; a != nil && a.permission.Known && a.permission.MessageRead {
			if message.ChannelID != channelID || (message.GuildID != "" && message.GuildID != a.permission.NativeIdentity) {
				return nil, errors.New("native_message_identity_mismatch")
			}
		}
	}
	if err := imp.observeNativeDiscordRead(ctx, channelID, pins); err != nil {
		return nil, err
	}
	return messages, nil
}
func (imp *Importer) retireNativeDiscordContainer(ctx context.Context, channelID string, pins []store.NativeMessageScopeLookup) error {
	if len(pins) == 0 {
		return nil
	}
	a := imp.nativeAuthority
	var denied []store.NativeMessageScopeLookup
	for _, pin := range pins {
		if pin.Identity.ProviderID == channelID {
			denied = append(denied, pin)
		}
	}
	return imp.store.RejectNativeMessageScopeRead(ctx, store.NativeMessageScopeRead{SourceID: a.permission.SourceID, SourceReference: a.permission.SourceReference, SourceType: sourceTypeDiscord, NativeIdentity: a.permission.NativeIdentity, Scopes: denied})
}
