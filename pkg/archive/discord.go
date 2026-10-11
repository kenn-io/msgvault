package archive

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
)

// DiscordCredential is a caller-supplied bot credential. BaseURL is empty for
// Discord's API; the archive does not read or write token files.
type DiscordCredential struct {
	Token   string
	BaseURL string
}

// DiscordIdentity identifies a bot and the guild it can access.
type DiscordIdentity struct {
	BotID     string
	GuildID   string
	GuildName string
}

// PermissionOverwrite describes Discord channel permissions.
type PermissionOverwrite = discord.PermissionOverwrite

func discordClient(ctx context.Context, credential DiscordCredential, guildID string) (*discord.Client, DiscordIdentity, error) {
	base := credential.BaseURL
	if base == "" {
		base = discord.DefaultBaseURL
	}
	c, err := discord.NewClient(base, credential.Token)
	if err != nil {
		return nil, DiscordIdentity{}, err
	}
	user, err := c.Me(ctx)
	if err != nil {
		return nil, DiscordIdentity{}, err
	}
	if !user.Bot || user.ID == "" {
		return nil, DiscordIdentity{}, errors.New("discord collection requires a bot credential")
	}
	guild, err := c.Guild(ctx, guildID)
	if err != nil {
		return nil, DiscordIdentity{}, err
	}
	if guild.ID != guildID || guild.Unavailable {
		return nil, DiscordIdentity{}, errors.New("discord credential cannot access the selected guild")
	}
	return c, DiscordIdentity{BotID: user.ID, GuildID: guild.ID, GuildName: guild.Name}, nil
}

// InspectDiscord validates the bot and access to the exact requested guild.
func InspectDiscord(ctx context.Context, credential DiscordCredential, guildID string) (DiscordIdentity, error) {
	_, identity, err := discordClient(ctx, credential, guildID)
	return identity, err
}

// DiscordChannels lists guild text, announcement, forum and media parents.
func DiscordChannels(ctx context.Context, credential DiscordCredential, guildID string) ([]Channel, error) {
	c, _, err := discordClient(ctx, credential, guildID)
	if err != nil {
		return nil, err
	}
	channels, err := c.GuildChannels(ctx, guildID)
	if err != nil {
		return nil, err
	}
	out := []Channel{}
	for _, channel := range channels {
		if discord.IsThreadCatalogParent(channel.Type) {
			out = append(out, Channel{ID: channel.ID, Name: channel.Name, PermissionOverwrites: channel.PermissionOverwrites})
		}
	}
	return out, nil
}

// BindDiscord creates or reuses one archive source per guild. Credential
// replacement leaves this source and its history intact.
func (a *Archive) BindDiscord(ctx context.Context, credential DiscordCredential, guildID string) (int64, error) {
	_, _, err := discordClient(ctx, credential, guildID)
	if err != nil {
		return 0, err
	}
	source, err := a.store.GetOrCreateSourceContext(ctx, "discord", guildID)
	if err != nil {
		return 0, err
	}
	return source.ID, nil
}

// DiscordOptions configures the existing importer, including channel selection,
// history bounds, repair, attachments, and progress callbacks.
type DiscordOptions = discord.ImportOptions

// DiscordGuildConfig includes or excludes conversations by channel ID. Threads
// follow their parent unless their own ID is listed.
type DiscordGuildConfig = config.DiscordGuildConfig

// DiscordSync supplies a credential and the options for one synchronization run.
type DiscordSync struct {
	Credential DiscordCredential
	Options    DiscordOptions
}

// DiscordSummary reports committed work and provider catalog/container issues.
type DiscordSummary = discord.ImportSummary

// SyncDiscord performs one cancellable run with the supplied importer options.
func (a *Archive) SyncDiscord(ctx context.Context, opts DiscordSync) (*DiscordSummary, error) {
	c, _, err := discordClient(ctx, opts.Credential, opts.Options.GuildID)
	if err != nil {
		return nil, err
	}
	return discord.NewImporter(a.store, c).Import(ctx, opts.Options)
}
