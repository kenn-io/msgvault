package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/sourcecatalog"
	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeDiscord = sourcecatalog.Discord

type discordCommandDeps struct {
	bind                 func(context.Context) discordCommandDeps
	openStore            func() (*store.Store, func(), error)
	tokenManager         func() *discord.TokenManager
	apiBaseURL           func() string
	providerConfig       func() config.DiscordConfig
	attachmentsDir       func() string
	databaseDSN          func() string
	rebuildCache         func(string) error
	postSourceMigrations func(*store.Store) error
	registerGuild        func(*store.Store, discord.Guild, string) error
}

func defaultDiscordCommandDeps() discordCommandDeps {
	return defaultDiscordCommandDepsForContext(context.Background())
}

func defaultDiscordCommandDepsForContext(ctx context.Context) discordCommandDeps {
	state := invocationFromContext(ctx)
	var currentCfg *config.Config
	if state != nil {
		currentCfg = state.cfg
	}
	return discordCommandDeps{
		bind: defaultDiscordCommandDepsForContext,
		openStore: func() (*store.Store, func(), error) {
			if state == nil || currentCfg == nil {
				return nil, nil, errors.New("configuration is unavailable")
			}
			return openWritableStoreAndInitForIngestInvocation(state)
		},
		tokenManager: func() *discord.TokenManager {
			if currentCfg == nil {
				return discord.NewTokenManager("")
			}
			return discord.NewTokenManager(currentCfg.TokensDir())
		},
		apiBaseURL: func() string { return discord.DefaultBaseURL },
		providerConfig: func() config.DiscordConfig {
			if currentCfg == nil {
				return config.DiscordConfig{}
			}
			return currentCfg.Discord
		},
		attachmentsDir: func() string {
			if currentCfg == nil {
				return ""
			}
			return currentCfg.AttachmentsDir()
		},
		databaseDSN: func() string {
			if currentCfg == nil {
				return ""
			}
			return currentCfg.DatabaseDSN()
		},
		rebuildCache: func(dbPath string) error {
			if currentCfg == nil {
				return errors.New("configuration is unavailable")
			}
			return rebuildCacheAfterManualSync(dbPath, state)
		},
		postSourceMigrations: func(st *store.Store) error {
			return runPostSourceCreateMigrationsForInvocation(st, state)
		},
		registerGuild: registerDiscordGuild,
	}
}

func (d discordCommandDeps) client(token string) (*discord.Client, error) {
	return discord.NewClient(d.apiBaseURL(), token)
}

func resolveDiscordSources(st *store.Store, selector string) ([]*store.Source, error) {
	sources, err := st.ListSources(sourceTypeDiscord)
	if err != nil {
		return nil, fmt.Errorf("list Discord sources: %w", err)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	if selector == "" {
		if len(sources) == 0 {
			return nil, errors.New("no Discord guilds are registered; run 'msgvault add-discord' first")
		}
		return sources, nil
	}

	for _, source := range sources {
		if source.Identifier == selector {
			return []*store.Source{source}, nil
		}
	}
	var matches []*store.Source
	for _, source := range sources {
		if source.DisplayName.Valid && strings.EqualFold(source.DisplayName.String, selector) {
			matches = append(matches, source)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("discord guild %q is not registered", selector)
	case 1:
		return matches, nil
	default:
		return nil, fmt.Errorf("discord guild name %q is ambiguous; use a guild ID", selector)
	}
}

func discordSourceLabel(source *store.Source) string {
	if source.DisplayName.Valid && source.DisplayName.String != "" {
		return fmt.Sprintf("%s (%s)", source.DisplayName.String, source.Identifier)
	}
	return source.Identifier
}

func newDiscordClientForSource(source *store.Source, deps discordCommandDeps) (*discord.Client, error) {
	client, _, err := resolveDiscordClientForSource(source, deps)
	return client, err
}

func resolveDiscordClientForSource(source *store.Source, deps discordCommandDeps) (*discord.Client, string, error) {
	record, err := deps.tokenManager().Resolve(sourceOAuthApp(source))
	if err != nil {
		return nil, "", fmt.Errorf("resolve Discord credential for %s: %w", discordSourceLabel(source), err)
	}
	client, err := deps.client(record.AccessToken())
	if err != nil {
		return nil, "", fmt.Errorf("configure Discord client for %s: %w", discordSourceLabel(source), err)
	}
	return client, record.BotUserID, nil
}

func newDiscordImporterForSource(ctx context.Context, st *store.Store, source *store.Source, deps discordCommandDeps) (*discord.Importer, string, error) {
	permission, err := pinNativeMessagePermission(ctx, source.Identifier, source, st)
	if err != nil {
		return nil, "", err
	}
	client, botID, err := resolveDiscordClientForSource(source, deps)
	if err != nil {
		return nil, "", pauseNativeMessagePermission(ctx, st, permission, err)
	}
	me, err := client.Me(ctx)
	if err != nil {
		cause := fmt.Errorf("verify Discord credential identity: %w", err)
		var apiErr *discord.APIError
		if permission != nil && errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound) {
			permission.Known = true
		}
		return nil, "", pauseNativeMessagePermission(ctx, st, permission, cause)
	}
	if me.ID != botID || !me.Bot {
		if permission != nil {
			permission.Known = true
		}
		return nil, "", pauseNativeMessagePermission(ctx, st, permission, errors.New("discord current user does not match the bound bot identity"))
	}
	return discord.NewImporter(st, client), botID, nil
}

func discordImportOptions(source *store.Source, deps discordCommandDeps, full bool, after time.Time, progress func(string)) discord.ImportOptions {
	provider := deps.providerConfig()
	policy := provider.MediaPolicy(source.Identifier)
	return discord.ImportOptions{
		GuildID:          source.Identifier,
		GuildConfig:      provider.Guilds[source.Identifier],
		AttachmentsDir:   deps.attachmentsDir(),
		MaxMediaBytes:    policy.MaxBytes,
		MediaPolicy:      policy,
		EditRescanWindow: provider.EditRescanWindow,
		After:            after,
		Full:             full,
		Progress:         progress,
	}
}

func parseDiscordAfter(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD or RFC3339): %w", value, err)
	}
	return parsed, nil
}

func readDiscordBotToken(in io.Reader) (string, error) {
	if in != os.Stdin {
		return readPasswordFromPipe(in)
	}
	method, output := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()),
		isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive("Discord bot token:", output)
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	case passwordNoPrompt:
		return "", errors.New("cannot read Discord bot token: no terminal is available; pipe the token via stdin")
	default:
		return "", errors.New("cannot determine Discord bot token input method")
	}
}

func writeDiscordProgress(out io.Writer) func(string) {
	return func(message string) {
		_, _ = fmt.Fprintln(out, message)
	}
}

func nullableDiscordBinding(binding string) sql.NullString {
	return sql.NullString{String: binding, Valid: binding != ""}
}

func runDiscordSources(
	ctx context.Context,
	sources []*store.Source,
	run func(context.Context, *store.Source) error,
) error {
	var errs []error
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := run(ctx, source); err != nil {
			errs = append(errs, fmt.Errorf("discord guild %s: %w", source.Identifier, err))
		}
	}
	return errors.Join(errs...)
}
