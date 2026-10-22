package sourceinventory

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConfiguredIMAPDisplayNamePreservesFreshness(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			const identifier = "imaps://owner@example.test@imap.example.test:993"
			source, err := st.GetOrCreateSource("imap", identifier)
			requirements.NoError(err)
			requirements.NoError(st.UpdateSourceDisplayName(source.ID, "owner@example.test"))
			run, err := st.StartSync(source.ID, "incremental")
			requirements.NoError(err)
			requirements.NoError(st.CompleteSync(run, "cursor"))
			cfg := config.NewDefaultConfig()
			cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: enabled, Schedule: "0 * * * *"}}
			items, err := Build(t.Context(), cfg, st)
			requirements.NoError(err)
			requirements.Len(items, 1, "resolved accounts must not create a phantom Gmail source")
			assertions.Equal(identifier, items[0].Identifier)
			requirements.NotNil(items[0].SourceID)
			assertions.Equal(source.ID, *items[0].SourceID)
			assertions.Equal("completed", items[0].Outcome)
			assertions.Equal("msgvault sync-incremental owner@example.test", items[0].SyncCommand)
			assertions.Equal(map[bool]string{true: "scheduled", false: "disabled"}[enabled], items[0].Scheduling)
			now := time.Now().UTC()
			if !enabled {
				now = now.Add(48 * time.Hour)
			}
			report, err := freshness.Build(t.Context(), freshness.Inputs{Sources: items, Now: now, StaleAfter: 24 * time.Hour})
			requirements.NoError(err)
			assertions.False(report.Stale, "a healthy or disabled resolved account must not falsely block ranked views")
		})
	}
}

func TestConfiguredAccountUsesDaemonSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, kind, identifier, display, alias, configured, command string
		matched                                                     bool
	}{
		{name: "casefolded display", kind: "imap", identifier: "imaps://mail.example.test:993", display: "Owner@Example.Test", configured: "owner@example.test", command: "msgvault sync-incremental owner@example.test", matched: true},
		{name: "casefolded identifier", kind: "gmail", identifier: "Owner@Example.Test", configured: "owner@example.test", command: "msgvault sync-incremental owner@example.test", matched: true},
		{name: "archive alias", kind: "teams", identifier: "owner@example.test", alias: "Work", configured: "work", command: "msgvault sync-teams owner@example.test", matched: true},
		{name: "Discord exact guild", kind: "discord", identifier: "123456789", configured: "123456789", command: "msgvault sync-discord 123456789", matched: true},
		{name: "Discord display rejected", kind: "discord", identifier: "123456789", display: "Community", configured: "community", matched: false},
		{name: "Discord alias rejected", kind: "discord", identifier: "123456789", alias: "Community", configured: "community", matched: false},
		{name: "unsupported account type", kind: "o365", identifier: "owner@example.test", configured: "owner@example.test", matched: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.kind, tc.identifier)
			requirements.NoError(err)
			if tc.display != "" {
				requirements.NoError(st.UpdateSourceDisplayName(source.ID, tc.display))
			}
			if tc.alias != "" {
				_, err := st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &tc.alias})
				requirements.NoError(err)
			}
			cfg := config.NewDefaultConfig()
			cfg.Accounts = []config.AccountSchedule{{Email: tc.configured, Enabled: true, Schedule: "0 * * * *"}}
			items, err := Build(t.Context(), cfg, st)
			requirements.NoError(err)
			if tc.matched {
				requirements.Len(items, 1)
				assertions.Equal("scheduled", items[0].Scheduling)
				assertions.Equal(tc.identifier, items[0].Identifier)
				assertions.Equal(tc.command, items[0].SyncCommand)
				return
			}
			requirements.Len(items, 2)
			for _, item := range items {
				if item.SourceID != nil {
					assertions.Equal("manual", item.Scheduling)
				} else {
					assertions.Equal("gmail", item.SourceType)
					assertions.Equal("scheduled", item.Scheduling)
				}
			}
		})
	}
}

func TestConfiguredAccountSelectsFirstSourcePerSupportedType(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	for _, identifier := range []string{"imaps://first.example.test:993", "imaps://second.example.test:993"} {
		source, err := st.GetOrCreateSource("imap", identifier)
		requirements.NoError(err)
		requirements.NoError(st.UpdateSourceDisplayName(source.ID, "owner@example.test"))
	}
	// The daemon selects the first row of each supported type from this resolver.
	resolved, err := st.GetSourcesByIdentifierOrDisplayNameContext(t.Context(), "owner@example.test")
	requirements.NoError(err)
	requirements.Len(resolved, 2)
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: true, Schedule: "0 * * * *"}}
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 2)
	for _, item := range items {
		requirements.NotNil(item.SourceID)
		if *item.SourceID == resolved[0].ID {
			assertions.Equal("scheduled", item.Scheduling)
		} else {
			assertions.Equal("manual", item.Scheduling)
		}
	}
}

func TestConfiguredAccountWithoutArchiveRow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: true, Schedule: "0 * * * *"}}
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Nil(items[0].SourceID)
	assertions.Equal("gmail", items[0].SourceType)
	assertions.Equal("never", items[0].Outcome)
	assertions.Equal("scheduled", items[0].Scheduling)
	assertions.Equal("msgvault sync-incremental owner@example.test", items[0].SyncCommand)
}

func TestRestrictedSourceScopeOmitsConfiguredSourcePlaceholders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	visible, err := st.GetOrCreateSource("gmail", "visible@example.test")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "hidden@example.test")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("slack", "hidden-workspace")
	requirements.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{
		{Email: "visible@example.test", Enabled: true, Schedule: "0 * * * *"},
		{Email: "hidden@example.test", Enabled: true, Schedule: "0 * * * *"},
	}
	cfg.Slack.Enabled = true
	cfg.Slack.Schedule = "0 * * * *"
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.ID})

	items, err := Build(ctx, cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 1, "configured sources outside the authorized set must not reappear as placeholders")
	requirements.NotNil(items[0].SourceID)
	assertions.Equal(visible.ID, *items[0].SourceID)
	assertions.Equal("visible@example.test", items[0].Identifier)
	assertions.Equal("scheduled", items[0].Scheduling)
}

func TestConfiguredRetiredAccountDoesNotScheduleSurvivor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	retired, err := st.GetOrCreateSource("gmail", "former@example.test")
	requirements.NoError(err)
	survivor, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired.ID, IntoSourceID: survivor.ID})
	requirements.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: "former@example.test", Enabled: true, Schedule: "0 * * * *"}}
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 1, "a retired account must not reappear as a never-synced placeholder")
	requirements.NotNil(items[0].SourceID)
	assertions.Equal(survivor.ID, *items[0].SourceID)
	assertions.Equal("manual", items[0].Scheduling)
	assertions.Equal("msgvault sync-incremental owner@example.test", items[0].SyncCommand)
}

func TestRetiredConfiguredProviderDoesNotCreatePlaceholder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	retired, err := st.GetOrCreateSource("slack", "retired-workspace")
	requirements.NoError(err)
	survivor, err := st.GetOrCreateSource("slack", "active-workspace")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired.ID, IntoSourceID: survivor.ID})
	requirements.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Slack.Enabled = true
	cfg.Slack.Schedule = "0 * * * *"
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{retired.ID})
	items, err := Build(ctx, cfg, st)
	requirements.NoError(err)
	assertions.Empty(items, "a retired source in a scoped archive must not become a configured placeholder")
}

func TestConfiguredCalendarEmailMatchesNormalizedArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "OWNER@EXAMPLE.TEST/primary")
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.GCal = []config.GCalSource{{Email: " Owner@Example.Test ", Enabled: true, Schedule: "0 * * * *"}}

	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 1, "normalized calendar account matching must not create a phantom")
	requirements.NotNil(items[0].SourceID)
	assertions.Equal(source.ID, *items[0].SourceID)
	assertions.Equal("scheduled", items[0].Scheduling)
	assertions.Equal("msgvault sync-calendar ' Owner@Example.Test '", items[0].SyncCommand)
}

func TestHistoryOnlyBeeperSourceIsStatic(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "history-example")
	requirements.NoError(err)
	historyOnly := true
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{HistoryOnly: &historyOnly})
	requirements.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Beeper.Enabled = true
	cfg.Beeper.Schedule = "*/15 * * * *"
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 1)
	requirements.Equal("static", items[0].Scheduling)
	assertions.Empty(items[0].SyncCommand)
}

func TestBeeperAccountFiltersControlFreshnessScheduling(t *testing.T) {
	for _, tc := range []struct {
		name, identifier, wantScheduling string
		accounts, excludeAccounts        []string
		wantStale                        bool
	}{
		{name: "allowlist includes", identifier: "included", accounts: []string{"included"}, wantScheduling: "scheduled", wantStale: true},
		{name: "allowlist excludes", identifier: "excluded", accounts: []string{"included"}, wantScheduling: "disabled"},
		{name: "denylist excludes", identifier: "excluded", excludeAccounts: []string{"excluded"}, wantScheduling: "disabled"},
		{name: "denylist keeps other accounts", identifier: "included", excludeAccounts: []string{"excluded"}, wantScheduling: "scheduled", wantStale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource("beeper", tc.identifier)
			requirements.NoError(err)

			cfg := config.NewDefaultConfig()
			cfg.Beeper.Enabled = true
			cfg.Beeper.Schedule = "*/15 * * * *"
			cfg.Beeper.Accounts = tc.accounts
			cfg.Beeper.ExcludeAccounts = tc.excludeAccounts
			items, err := Build(t.Context(), cfg, st)
			requirements.NoError(err)
			requirements.Len(items, 1)
			assertions.Equal(tc.wantScheduling, items[0].Scheduling)

			report, err := freshness.Build(t.Context(), freshness.Inputs{
				Sources: items, Now: time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC), StaleAfter: 24 * time.Hour,
			})
			requirements.NoError(err)
			assertions.Equal(tc.wantStale, report.Stale)
		})
	}
}

func TestImportOnlySourcesAreStatic(t *testing.T) {
	for _, sourceType := range []string{"whatsapp", "apple_messages", "google_messages", "imessage", "google_voice", "meeting_import"} {
		t.Run(sourceType, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(sourceType, "archive-example")
			requirements.NoError(err)

			items, err := Build(t.Context(), config.NewDefaultConfig(), st)
			requirements.NoError(err)
			requirements.Len(items, 1)
			requirements.NotNil(items[0].SourceID)
			assertions.Equal("static", items[0].Scheduling)
			assertions.Empty(items[0].SyncCommand)

			report, err := freshness.Build(t.Context(), freshness.Inputs{
				Sources:    items,
				Now:        time.Date(2025, time.January, 2, 0, 0, 0, 0, time.UTC),
				StaleAfter: 24 * time.Hour,
			})
			requirements.NoError(err)
			assertions.False(report.Stale, "an import-only archive cannot be refreshed by a sync command")
		})
	}
}

func TestArchiveAndConfigurationUnion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	imap, err := st.GetOrCreateSource("imap", "owner@example.test")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("mbox", "owner@example.test")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("slack", "workspace")
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: false, Schedule: "0 * * * *"}}
	cfg.Slack.Enabled = true
	cfg.Slack.Schedule = "*/15 * * * *"
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 3, "existing sources must prevent phantom account/provider placeholders")
	for _, item := range items {
		switch item.SourceType {
		case "imap":
			assertions.Equal("disabled", item.Scheduling)
			requirements.NotNil(item.SourceID)
			assertions.Equal(imap.ID, *item.SourceID)
		case "mbox":
			assertions.Equal("static", item.Scheduling)
			assertions.Empty(item.SyncCommand)
		case "slack":
			assertions.Equal("scheduled", item.Scheduling)
			assertions.Equal("msgvault sync-slack", item.SyncCommand)
		}
	}
}

func TestConfiguredProvidersWithoutArchiveRows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Beeper.Enabled = true
	cfg.Matrix.Enabled = true
	cfg.Matrix.Schedule = "*/15 * * * *"
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Equal("beeper", items[0].SourceType)
	assertions.Equal("manual", items[0].Scheduling)
	assertions.Equal("matrix", items[1].SourceType)
	assertions.Equal("scheduled", items[1].Scheduling)
	assertions.Nil(items[0].SourceID)
	assertions.Nil(items[1].SourceID)
}

func TestSyncOutcomeIncludesPartialCompletedRun(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	run, err := st.StartSync(source.ID, "incremental")
	requirements.NoError(err)
	requirements.NoError(st.CompleteSync(run, "cursor"))
	_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET errors_count=2 WHERE id=?"), run)
	requirements.NoError(err)
	items, err := Build(t.Context(), config.NewDefaultConfig(), st)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal("partial", items[0].Outcome)
	assertions.Equal(int64(2), items[0].LastSyncErrors)
	assertions.NotNil(items[0].LastSuccessfulSyncAt)
}

func TestAttachmentSyncRunsDoNotReplaceSourceFreshness(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, mediaSyncType string
		completeMedia                   bool
	}{
		{name: "Beeper media success", sourceType: "beeper", mediaSyncType: "beeper_media", completeMedia: true},
		{name: "Slack media failure", sourceType: "slack", mediaSyncType: "slack_media"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.sourceType, "archive-example")
			requirements.NoError(err)
			mainRun, err := st.StartSync(source.ID, tc.sourceType)
			requirements.NoError(err)
			requirements.NoError(st.CompleteSync(mainRun, "source-cursor"))
			mainAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
			_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET started_at=?, completed_at=? WHERE id=?"), mainAt.Add(-time.Minute), mainAt, mainRun)
			requirements.NoError(err)
			mediaRun, err := st.StartSync(source.ID, tc.mediaSyncType)
			requirements.NoError(err)
			if tc.completeMedia {
				requirements.NoError(st.CompleteSync(mediaRun, "media-cursor"))
			} else {
				requirements.NoError(st.FailSync(mediaRun, "synthetic media failure"))
			}

			cfg := config.NewDefaultConfig()
			if tc.sourceType == "beeper" {
				cfg.Beeper.Enabled = true
				cfg.Beeper.Schedule = "0 * * * *"
			} else {
				cfg.Slack.Enabled = true
				cfg.Slack.Schedule = "0 * * * *"
			}
			items, err := Build(t.Context(), cfg, st)
			requirements.NoError(err)
			requirements.Len(items, 1)
			assertions.Equal("scheduled", items[0].Scheduling)
			assertions.Equal("completed", items[0].Outcome, "media work is not a source sync outcome")
			requirements.NotNil(items[0].LastSuccessfulSyncAt)
			assertions.True(mainAt.Equal(*items[0].LastSuccessfulSyncAt), "media work must not replace the source sync timestamp")

			report, err := freshness.Build(t.Context(), freshness.Inputs{
				Sources:    items,
				Now:        time.Now().UTC(),
				StaleAfter: 24 * time.Hour,
			})
			requirements.NoError(err)
			assertions.True(report.Stale, "media completion must not make an old source sync appear fresh")
		})
	}
}

func TestChatwootInventoryMatchesAccountScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("chatwoot", "https://support.example.test/accounts/7/inboxes/2")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("chatwoot", "https://other.example.test/accounts/7/inboxes/2")
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Chatwoot = []config.ChatwootSource{
		{Identifier: "support", URL: "https://support.example.test", AccountID: 7, Enabled: true, Schedule: "0 * * * *"},
		{Identifier: "new", URL: "https://new.example.test", AccountID: 3, Enabled: false},
	}
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 3)
	for _, item := range items {
		switch item.Identifier {
		case "https://support.example.test/accounts/7/inboxes/2":
			assertions.Equal("scheduled", item.Scheduling)
			assertions.Equal("msgvault sync-chatwoot support", item.SyncCommand)
		case "https://other.example.test/accounts/7/inboxes/2":
			assertions.Equal("manual", item.Scheduling)
		case "new":
			assertions.Equal("disabled", item.Scheduling)
			assertions.Nil(item.SourceID)
		default:
			assertions.Fail("unexpected inventory item", item.Identifier)
		}
	}
}
