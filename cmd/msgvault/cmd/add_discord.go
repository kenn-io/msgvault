package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

type addDiscordOptions struct {
	OAuthApp string
	GuildIDs []string
}

// addDiscordAfterCredentialSaveHook lets the lifecycle concurrency test pause
// after the inner token-store mutation while the outer lifecycle lock remains
// held.
var addDiscordAfterCredentialSaveHook func()

func newAddDiscordCmd(deps discordCommandDeps) *cobra.Command {
	cmd := newAddDiscordLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			token, err := readDiscordBotToken(cmd.InOrStdin())
			if err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
				clirun.EnvDiscordToken: strings.TrimSpace(token),
			})
		}
		token := strings.TrimSpace(os.Getenv(clirun.EnvDiscordToken))
		if token == "" {
			return errors.New("missing Discord bot token in daemon subprocess")
		}
		cmd.SetIn(strings.NewReader(token))
		return runLocal(cmd, args)
	}
	return cmd
}

func newAddDiscordLocalCmd(deps discordCommandDeps) *cobra.Command {
	opts := addDiscordOptions{}
	cmd := &cobra.Command{
		Use:   "add-discord",
		Short: "Register Discord guilds for bot-based archival",
		Long: `Validate a Discord bot token and register one or more accessible guilds.

The token is read through a masked terminal prompt or piped stdin. It is never
accepted as a command-line flag. When the bot can access one guild, that guild
is selected automatically; otherwise repeat --guild with the desired guild IDs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAddDiscord(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.OAuthApp, "oauth-app", "", "Discord bot credential binding label")
	cmd.Flags().StringSliceVar(&opts.GuildIDs, "guild", nil, "Discord guild ID to register (repeatable)")
	return cmd
}

func runAddDiscord(cmd *cobra.Command, deps discordCommandDeps, opts addDiscordOptions) error {
	token, err := readDiscordBotToken(cmd.InOrStdin())
	if err != nil {
		return err
	}
	accessToken := strings.TrimSpace(token)
	client, err := deps.client(accessToken)
	if err != nil {
		return err
	}
	me, err := client.Me(cmd.Context())
	if err != nil {
		return fmt.Errorf("authenticate Discord bot: %w", err)
	}
	if !me.Bot {
		return errors.New("discord credential identifies a user account, not a bot; user tokens are not supported")
	}
	guilds, err := client.Guilds(cmd.Context())
	if err != nil {
		return fmt.Errorf("list accessible Discord guilds: %w", err)
	}
	if len(guilds) > 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Accessible Discord guilds:")
		for _, guild := range guilds {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s (%s)\n", guild.Name, guild.ID)
		}
	}
	selected, autoSelected, err := selectDiscordGuilds(guilds, opts.GuildIDs)
	if err != nil {
		return usageErr(cmd, err)
	}
	if autoSelected {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Selected sole accessible guild: %s (%s)\n", selected[0].Name, selected[0].ID)
	}

	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	manager := deps.tokenManager()
	record := discord.TokenRecord{
		BotUserID: me.ID, BotUsername: me.Username, AccessToken: accessToken, Binding: opts.OAuthApp,
	}
	if err := saveAndRegisterDiscordCredential(st, manager, record, selected, deps); err != nil {
		return err
	}
	for _, guild := range selected {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered Discord guild: %s (%s)\n", guild.Name, guild.ID)
		diagnoseDiscordGuild(cmd, client, guild, cmd.OutOrStdout())
	}
	return nil
}

func saveAndRegisterDiscordCredential(
	st *store.Store,
	manager *discord.TokenManager,
	record discord.TokenRecord,
	selected []discord.Guild,
	deps discordCommandDeps,
) error {
	return manager.WithLifecycleLock(func() error {
		// Credential persistence is the first durable setup phase. If later
		// source registration fails, rerunning the same bot binding rotates the
		// protected record and resumes idempotent source upserts.
		if err := saveDiscordCredential(st, manager, record); err != nil {
			return err
		}
		if addDiscordAfterCredentialSaveHook != nil {
			addDiscordAfterCredentialSaveHook()
		}
		for _, guild := range selected {
			if err := deps.registerGuild(st, guild, record.Binding); err != nil {
				return err
			}
		}
		if err := deps.postSourceMigrations(st); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
		return nil
	})
}

func registerDiscordGuild(st *store.Store, guild discord.Guild, binding string) error {
	source, err := st.GetOrCreateSource(sourceTypeDiscord, guild.ID)
	if err != nil {
		return fmt.Errorf("register Discord guild %s: %w", guild.ID, err)
	}
	if err := st.UpdateSourceDisplayName(source.ID, guild.Name); err != nil {
		return fmt.Errorf("set Discord guild name %s: %w", guild.ID, err)
	}
	if err := st.UpdateSourceOAuthApp(source.ID, nullableDiscordBinding(binding)); err != nil {
		return fmt.Errorf("bind Discord guild credential %s: %w", guild.ID, err)
	}
	return nil
}

func selectDiscordGuilds(accessible []discord.Guild, requested []string) ([]discord.Guild, bool, error) {
	if len(accessible) == 0 {
		return nil, false, errors.New("the Discord bot cannot access any guilds")
	}
	if len(requested) == 0 {
		if len(accessible) != 1 {
			return nil, false, fmt.Errorf("the Discord bot can access %d guilds; select one or more with --guild", len(accessible))
		}
		return []discord.Guild{accessible[0]}, true, nil
	}
	byID := make(map[string]discord.Guild, len(accessible))
	for _, guild := range accessible {
		byID[guild.ID] = guild
	}
	selected := make([]discord.Guild, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, guildID := range requested {
		guild, ok := byID[guildID]
		if !ok {
			return nil, false, fmt.Errorf("requested guild %s is not accessible to this bot", guildID)
		}
		if _, duplicate := seen[guildID]; duplicate {
			continue
		}
		seen[guildID] = struct{}{}
		selected = append(selected, guild)
	}
	return selected, false, nil
}

func saveDiscordCredential(st *store.Store, manager *discord.TokenManager, record discord.TokenRecord) error {
	records, err := manager.List()
	if err != nil {
		return fmt.Errorf("inspect Discord credentials: %w", err)
	}
	sources, err := st.ListSources(sourceTypeDiscord)
	if err != nil {
		return fmt.Errorf("inspect Discord source bindings: %w", err)
	}
	nullBound := make([]*store.Source, 0)
	for _, source := range sources {
		if !source.OAuthApp.Valid {
			nullBound = append(nullBound, source)
		}
	}
	newBot := len(records) > 0 && !slices.ContainsFunc(records, func(existing discord.TokenRecord) bool {
		return existing.BotUserID == record.BotUserID
	})
	if newBot && len(nullBound) > 0 {
		return errors.New("cannot add another Discord bot while NULL-bound guild sources exist; promote the existing credential with --oauth-app first")
	}
	if err := manager.Save(record); err != nil {
		return fmt.Errorf("store Discord bot credential: %w", err)
	}
	if record.Binding == "" {
		return nil
	}
	soleMatchingBot := len(records) <= 1 && (len(records) == 0 || records[0].BotUserID == record.BotUserID)
	if !soleMatchingBot {
		return nil
	}
	for _, source := range nullBound {
		if err := st.UpdateSourceOAuthApp(source.ID, nullableDiscordBinding(record.Binding)); err != nil {
			return fmt.Errorf("promote Discord source %s credential binding: %w", source.Identifier, err)
		}
	}
	return nil
}

func diagnoseDiscordGuild(cmd *cobra.Command, api discord.API, guild discord.Guild, out io.Writer) {
	channels, err := api.GuildChannels(cmd.Context(), guild.ID)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  Channel access unavailable: %s\n", discordDiagnostic(err))
		return
	}
	if _, err := api.GuildMembers(cmd.Context(), guild.ID, ""); err != nil {
		_, _ = fmt.Fprintf(out, "  Member enrichment unavailable: %s\n", discordDiagnostic(err))
	}
	for _, channel := range channels {
		if channel.Type != 0 && channel.Type != 5 {
			continue
		}
		messages, historyErr := api.Messages(cmd.Context(), channel.ID, discord.MessageQuery{Limit: 1})
		if historyErr != nil {
			_, _ = fmt.Fprintf(out, "  Message history unavailable for channel %s: %s\n", channel.ID, discordDiagnostic(historyErr))
		} else if len(messages) > 0 && messageContentUnavailable(messages[0]) {
			_, _ = fmt.Fprintf(out, "  Message Content Intent may be unavailable in channel %s\n", channel.ID)
		}
		if _, privateErr := api.ArchivedThreads(cmd.Context(), channel.ID, true, time.Time{}); privateErr != nil {
			_, _ = fmt.Fprintf(out, "  Private archived threads unavailable for channel %s: %s\n", channel.ID, discordDiagnostic(privateErr))
		}
	}
}

func messageContentUnavailable(message discord.Message) bool {
	return message.Type == 0 && message.Content == "" && len(message.Attachments) == 0 && len(message.Embeds) == 0
}

func discordDiagnostic(err error) string {
	var apiErr *discord.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			return "authentication failed"
		case http.StatusForbidden:
			return "permission denied"
		case http.StatusNotFound:
			return "resource not found"
		}
	}
	return "request failed"
}

func init() {
	rootCmd.AddCommand(newAddDiscordCmd(defaultDiscordCommandDeps()))
}
