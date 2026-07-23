package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeDiscord = "discord"

type discordCommandDeps struct {
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
	return discordCommandDeps{
		openStore:    openWritableStoreAndInitForIngest,
		tokenManager: func() *discord.TokenManager { return discord.NewTokenManager(cfg.TokensDir()) },
		apiBaseURL:   func() string { return discord.DefaultBaseURL },
		providerConfig: func() config.DiscordConfig {
			return cfg.Discord
		},
		attachmentsDir:       func() string { return cfg.AttachmentsDir() },
		databaseDSN:          func() string { return cfg.DatabaseDSN() },
		rebuildCache:         rebuildCacheAfterWrite,
		postSourceMigrations: runPostSourceCreateMigrations,
		registerGuild:        registerDiscordGuild,
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

func newDiscordClientForSource(
	source *store.Source, deps discordCommandDeps,
) (*discord.Client, error) {
	record, err := deps.tokenManager().Resolve(sourceOAuthApp(source))
	if err != nil {
		return nil, fmt.Errorf("resolve Discord credential for %s: %w", discordSourceLabel(source), err)
	}
	client, err := deps.client(record.AccessToken())
	if err != nil {
		return nil, fmt.Errorf("configure Discord client for %s: %w", discordSourceLabel(source), err)
	}
	return client, nil
}

func newDiscordImporterForSource(
	st *store.Store, source *store.Source, deps discordCommandDeps,
) (*discord.Importer, error) {
	client, err := newDiscordClientForSource(source, deps)
	if err != nil {
		return nil, err
	}
	return discord.NewImporter(st, client), nil
}

func discordImportOptions(source *store.Source, deps discordCommandDeps, full bool, after time.Time, progress func(string)) discord.ImportOptions {
	provider := deps.providerConfig()
	return discord.ImportOptions{
		GuildID:          source.Identifier,
		GuildConfig:      provider.Guilds[source.Identifier],
		AttachmentsDir:   deps.attachmentsDir(),
		MaxMediaBytes:    provider.MaxMediaBytes,
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
