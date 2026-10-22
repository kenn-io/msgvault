// Package sourceinventory unifies configured providers with archived sources.
package sourceinventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

type Item = freshness.SourceInventoryItem

type configuredSource struct {
	kind, identifier, schedule, command string
	enabled                             bool
	provider                            bool
	accountURL                          string
	accountID                           int64
	accountSourceIDs                    []int64
}

// Build unions archive rows with configured accounts and provider sections.
// It reads scheduling intent, not scheduler runtime state; a never-imported
// configured source must remain visible in freshness decisions.
func Build(ctx context.Context, cfg *config.Config, st *store.Store) ([]Item, error) {
	if cfg == nil || st == nil {
		return nil, errors.New("source inventory: configuration and store are required")
	}
	sources, err := st.ListSourcesContext(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list freshness sources: %w", err)
	}
	configured := configuration(cfg)
	matched := make([]bool, len(configured))
	for i, entry := range configured {
		if entry.kind != "account" {
			continue
		}
		rows, err := st.GetSourcesByIdentifierOrDisplayNameContext(ctx, entry.identifier)
		if err != nil {
			return nil, fmt.Errorf("resolve freshness account: %w", err)
		}
		// Follow findScheduledSyncSources: resolve through the Store's casefolded
		// identifier/display-name/alias lookup, then take one row per syncable type.
		seen := map[string]bool{}
		for _, row := range rows {
			switch row.SourceType {
			case "gmail", "imap", "msmail", "teams":
			case "discord":
				if row.Identifier != entry.identifier {
					continue
				}
			default:
				continue
			}
			if row.MergedIntoSourceID != 0 {
				matched[i] = true
				continue
			}
			if seen[row.SourceType] {
				continue
			}
			seen[row.SourceType] = true
			configured[i].accountSourceIDs = append(configured[i].accountSourceIDs, row.ID)
		}
	}
	items := make([]Item, 0, len(sources)+len(configured))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if source.MergedIntoSourceID != 0 {
			for i, entry := range configured {
				if matches(source, entry) {
					matched[i] = true
				}
			}
			continue
		}
		id := source.ID
		item := Item{SourceID: &id, SourceType: source.SourceType, Identifier: source.Identifier, Scheduling: "manual", Outcome: "never"}
		item.SyncCommand = syncCommand(item)
		for i, entry := range configured {
			if matches(source, entry) {
				matched[i] = true
				if entry.kind == "beeper" && !cfg.Beeper.AccountIncluded(source.Identifier) {
					item.Scheduling = "disabled"
				} else {
					item.Scheduling = scheduling(entry.enabled, entry.schedule)
				}
				if entry.command != "" {
					item.SyncCommand = entry.command
				} else if entry.kind == "account" && source.SourceType != "teams" {
					// Teams keeps its canonical email for the CLI's token lookup.
					item.SyncCommand = syncCommand(Item{SourceType: source.SourceType, Identifier: entry.identifier})
				}
			}
		}
		// History-only Beeper archives are excluded from synchronization even
		// when the provider itself has a scheduled job.
		if source.HistoryOnly || staticSource(item.SourceType) {
			item.Scheduling = "static"
			item.SyncCommand = ""
		}
		item.Outcome, err = st.LatestSyncOutcomeContext(ctx, source.ID)
		if err != nil {
			return nil, fmt.Errorf("read latest freshness sync: %w", err)
		}
		last, err := st.GetLastSuccessfulSourceSyncContext(ctx, source.ID)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("read successful freshness sync: %w", err)
		}
		if last != nil {
			item.LastSyncErrors = last.ErrorsCount
			if last.CompletedAt.Valid {
				at := last.CompletedAt.Time
				item.LastSuccessfulSyncAt = &at
			}
		}
		items = append(items, item)
	}
	seen := map[string]bool{}
	for i, entry := range configured {
		// A configured placeholder has no source ID to authorize. Never add
		// one when the Store read carries a source restriction.
		if sourcescope.Restricted(ctx) || matched[i] || (entry.provider && entry.identifier == "") {
			continue
		}
		kind := entry.kind
		if kind == "account" {
			kind = "gmail"
		} // Same token-first fallback as runScheduledSync.
		key := kind + "\x00" + entry.identifier
		if seen[key] {
			continue
		}
		seen[key] = true
		item := Item{SourceType: kind, Identifier: entry.identifier, Scheduling: scheduling(entry.enabled, entry.schedule), Outcome: "never", SyncCommand: entry.command}
		if item.SyncCommand == "" {
			item.SyncCommand = syncCommand(item)
		}
		items = append(items, item)
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		if n := cmp.Compare(a.SourceType, b.SourceType); n != 0 {
			return n
		}
		return cmp.Compare(a.Identifier, b.Identifier)
	})
	return items, nil
}

func matches(source *store.Source, entry configuredSource) bool {
	if entry.kind == "account" {
		return slices.Contains(entry.accountSourceIDs, source.ID)
	}
	if source.SourceType != entry.kind {
		return false
	}
	if entry.kind == "chatwoot" {
		base, account, _, ok := chatwoot.ParseSourceIdentifier(source.Identifier)
		canonical, err := chatwoot.CanonicalURL(entry.accountURL)
		return ok && err == nil && base == canonical && account == entry.accountID
	}
	if entry.provider {
		return true
	}
	if entry.kind == "gcal" {
		identifier := normalizeEmail(source.Identifier)
		return identifier == entry.identifier || strings.HasPrefix(identifier, entry.identifier+"/")
	}
	return source.Identifier == entry.identifier
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func scheduling(enabled bool, schedule string) string {
	if !enabled {
		return "disabled"
	}
	if schedule != "" {
		return "scheduled"
	}
	return "manual"
}

func configuration(cfg *config.Config) []configuredSource {
	var entries []configuredSource
	add := func(kind, id string, enabled bool, schedule, command string) {
		entries = append(entries, configuredSource{kind: kind, identifier: id, enabled: enabled, schedule: schedule, command: command})
	}
	for _, a := range cfg.Accounts {
		add("account", a.Email, a.Enabled, a.Schedule, "")
	}
	for _, p := range []struct {
		kind       string
		enabled    bool
		schedule   string
		configured bool
	}{
		{"beeper", cfg.Beeper.Enabled, cfg.Beeper.Schedule, cfg.Beeper.Enabled || cfg.Beeper.Schedule != "" || cfg.Beeper.URL != "" || len(cfg.Beeper.Accounts) > 0},
		{"slack", cfg.Slack.Enabled, cfg.Slack.Schedule, cfg.Slack.Enabled || cfg.Slack.Schedule != "" || len(cfg.Slack.Channels) > 0},
		{"matrix", cfg.Matrix.Enabled, cfg.Matrix.Schedule, cfg.Matrix.Enabled || cfg.Matrix.Schedule != "" || len(cfg.Matrix.Rooms) > 0},
	} {
		// Existing provider rows still need their disabled state even when the
		// section is absent. Only configured providers get a missing-row item.
		entries = append(entries, configuredSource{kind: p.kind, identifier: p.kind, enabled: p.enabled, schedule: p.schedule, provider: true})
		if !p.configured {
			entries[len(entries)-1].identifier = ""
		}
	}
	for _, s := range cfg.SynctechSMS.Sources {
		add("synctech_sms", s.OwnerPhone, s.Enabled, s.Schedule, "msgvault sync-synctech-sms "+shellArgument(s.Name))
	}
	for _, s := range cfg.GCal {
		name := s.Name
		if name == "" {
			name = s.Email
		}
		add("gcal", normalizeEmail(s.Email), s.Enabled, s.Schedule, "msgvault sync-calendar "+shellArgument(name))
	}
	for _, s := range cfg.Chatwoot {
		entries = append(entries, configuredSource{kind: "chatwoot", identifier: s.Identifier,
			enabled: s.Enabled, schedule: s.Schedule, command: "msgvault sync-chatwoot " + shellArgument(s.Identifier),
			accountURL: s.URL, accountID: s.AccountID})
	}
	for _, s := range cfg.Omi {
		add("omi", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Granola {
		add("granola", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Plaud {
		add("plaud", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Circleback {
		add("circleback", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.NotionMeetings {
		add("notion_meetings", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Twilio {
		add("twilio", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Twenty {
		add("twenty", s.Identifier, s.Enabled, s.Schedule, "")
	}
	for _, s := range cfg.Muesli {
		add("muesli", s.Identifier, s.Enabled, s.Schedule, "")
	}
	return entries
}

func staticSource(kind string) bool {
	return slices.Contains([]string{
		"apple-mail", "mbox", "pst", "eml", "maildir", "hey", "google-groups", "google_voice",
		"facebook_messenger", "imazing_csv", "slackdump", "synctech-sms",
		"whatsapp", "apple_messages", "google_messages", "imessage", "meeting_import",
	}, kind)
}

func syncCommand(item Item) string {
	if staticSource(item.SourceType) {
		return ""
	}
	arg := shellArgument(item.Identifier)
	switch item.SourceType {
	case "gmail", "imap", "msmail", "o365":
		return "msgvault sync-incremental " + arg
	case "teams", "discord":
		return "msgvault sync-" + item.SourceType + " " + arg
	case "beeper", "matrix":
		command := "msgvault sync-" + item.SourceType
		if item.SourceID != nil {
			command += " --account " + arg
		}
		return command
	case "slack":
		return "msgvault sync-slack"
	case "omi", "granola", "plaud", "circleback", "notion_meetings", "twilio", "twenty", "muesli":
		return "msgvault sync-" + strings.ReplaceAll(item.SourceType, "_", "-") + " " + arg
	case "gcal":
		account, _, _ := strings.Cut(item.Identifier, "/")
		return "msgvault sync-calendar " + shellArgument(account)
	default:
		return ""
	}
}

func shellArgument(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"`$;&|<>()*?!#\\") && !strings.HasPrefix(value, "-") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
