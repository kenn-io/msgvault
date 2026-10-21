package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func configSnippetFromHint(t *testing.T, hint string) string {
	t.Helper()
	var lines []string
	started := false
	for line := range strings.SplitSeq(hint, "\n") {
		if strings.HasPrefix(line, "  ") {
			started = true
			lines = append(lines, strings.TrimPrefix(line, "  "))
			continue
		}
		if started && strings.TrimSpace(line) != "" {
			break
		}
	}
	require.NotEmpty(t, lines, "configuration hint must contain an indented TOML snippet")
	return strings.Join(lines, "\n") + "\n"
}

func TestMeetingConfigurationHintsLoad(t *testing.T) {
	for _, tt := range []struct {
		name string
		hint string
	}{
		{name: "Granola", hint: granolaConfigHint},
		{name: "Circleback", hint: circlebackConfigHint},
		{name: "Notion", hint: notionMeetingsConfigHint},
		{name: "Muesli", hint: muesliConfigHint},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(configSnippetFromHint(t, tt.hint)), 0600))

			cfg, err := config.Load(path, "")

			require.NoError(err)
			switch tt.name {
			case "Granola":
				require.Len(cfg.Granola, 1)
				assert.Equal("you@example.com", cfg.Granola[0].AccountEmail)
			case "Circleback":
				require.Len(cfg.Circleback, 1)
				assert.Equal("you@example.com", cfg.Circleback[0].AccountEmail)
			case "Notion":
				require.Len(cfg.NotionMeetings, 1)
				assert.Equal("you@example.com", cfg.NotionMeetings[0].AccountEmail)
			case "Muesli":
				require.Len(cfg.Muesli, 1)
				assert.Equal("you@example.com", cfg.Muesli[0].AccountEmail)
			}
		})
	}
}

var durationLine = regexp.MustCompile(`(?m)^(  Duration:\s+)\S+$`)

func maskDuration(out string) string {
	return durationLine.ReplaceAllString(out, "${1}<duration>")
}

func TestSyncGranolaOutputMatchesBase(t *testing.T) {
	require := require.New(t)
	server := newGranolaSyncTestServer(t, true, false)
	installGranolaClientFactory(t, server.URL)
	markDaemonCLISubprocessForTest(t)
	testCfg := lifecycleTestConfig(t.TempDir())
	testCfg.Granola = []config.GranolaSource{{Identifier: "work", AccountEmail: "user-a@example.com", APIKey: "grn_test"}}
	testCtx := withStoreResolverConfig(t, testCfg)
	savedRefresh := rebuildGranolaCacheAfterWrite
	rebuildGranolaCacheAfterWrite = func(string, *invocation) error { return nil }
	t.Cleanup(func() { rebuildGranolaCacheAfterWrite = savedRefresh })
	oldLimit, oldAfter, oldFull := syncGranolaLimit, syncGranolaAfter, syncGranolaFull
	syncGranolaLimit, syncGranolaAfter, syncGranolaFull = 0, "", false
	t.Cleanup(func() { syncGranolaLimit, syncGranolaAfter, syncGranolaFull = oldLimit, oldAfter, oldFull })
	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "sync-granola"}
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})

	require.NoError(syncGranolaCmd.RunE(cmd, []string{"work"}))

	assert.Equal(t, "Syncing Granola for work\n"+
		"\n"+
		"  imported \"Partial import success\" (note-success)\n"+
		"\n"+
		"Granola sync complete!\n"+
		"  Duration:        <duration>\n"+
		"  Notes processed: 1\n"+
		"  Notes added:     1\n", maskDuration(stdout.String()))
}

func TestSyncMuesliOutputMatchesBase(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT, start_time TEXT, created_at TEXT, raw_transcript TEXT);
		INSERT INTO meetings VALUES (1, 'Planning', '2026-09-01T14:00:00Z', '2026-09-01 14:00:03', 'Synthetic meeting notes')`)
	require.NoError(err)
	require.NoError(db.Close())
	markDaemonCLISubprocessForTest(t)
	contacts := false
	testCfg := lifecycleTestConfig(t.TempDir())
	testCfg.Muesli = []config.MuesliSource{{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: path, Contacts: &contacts,
	}}
	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource(muesli.SourceType, "mac")
	require.NoError(err)
	require.NoError(st.Close())
	testCtx := withStoreResolverConfig(t, testCfg)
	savedRefresh := rebuildMuesliCacheAfterWrite
	rebuildMuesliCacheAfterWrite = func(string, *invocation) error { return nil }
	t.Cleanup(func() { rebuildMuesliCacheAfterWrite = savedRefresh })
	oldLimit, oldAfter, oldFull := syncMuesliLimit, syncMuesliAfter, syncMuesliFull
	syncMuesliLimit, syncMuesliAfter, syncMuesliFull = 0, "", false
	t.Cleanup(func() { syncMuesliLimit, syncMuesliAfter, syncMuesliFull = oldLimit, oldAfter, oldFull })
	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "sync-muesli"}
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})

	require.NoError(syncMuesliCmd.RunE(cmd, nil))

	assert.Equal(t, "Syncing Muesli for mac\n"+
		"\n"+
		"  added Muesli meeting 1\n"+
		"\n"+
		"Muesli sync complete!\n"+
		"  Duration:           <duration>\n"+
		"  Meetings processed: 1\n"+
		"  Meetings added:     1\n"+
		"  Meetings updated:   0\n"+
		"  Contacts:           off\n", maskDuration(stdout.String()))
}

// meetingSourceIDs adapts each provider's resolver to identifiers or an error
// so one table pins the resolver text for all four providers.
func meetingSourceIDs[T any](sources []T, err error, identifier func(T) string) ([]string, error) {
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, source := range sources {
		ids = append(ids, identifier(source))
	}
	return ids, nil
}

func oneMeetingSource[T any](source *T, err error) ([]T, error) {
	if err != nil {
		return nil, err
	}
	return []T{*source}, nil
}

func TestMeetingProviderErrorsMatchBase(t *testing.T) {
	type resolver func(args []string, cfg *config.Config) ([]string, error)
	granolaOne := func(args []string, cfg *config.Config) ([]string, error) {
		sources, err := oneMeetingSource(granolaSources(cfg).one(args))
		return meetingSourceIDs(sources, err, func(s config.GranolaSource) string { return s.Identifier })
	}
	circlebackOne := func(args []string, cfg *config.Config) ([]string, error) {
		sources, err := oneMeetingSource(circlebackSources(cfg).one(args))
		return meetingSourceIDs(sources, err, func(s config.CirclebackSource) string { return s.Identifier })
	}
	notionOne := func(args []string, cfg *config.Config) ([]string, error) {
		sources, err := oneMeetingSource(notionMeetingsSources(cfg).one(args))
		return meetingSourceIDs(sources, err, func(s config.NotionMeetingsSource) string { return s.Identifier })
	}
	muesliSelected := func(args []string, cfg *config.Config) ([]string, error) {
		sources, err := muesliSources(cfg).selected(args)
		return meetingSourceIDs(sources, err, func(s config.MuesliSource) string { return s.Identifier })
	}
	twoGranola := &config.Config{Granola: []config.GranolaSource{{Identifier: "Work"}, {Identifier: "home"}}}
	twoCircleback := &config.Config{Circleback: []config.CirclebackSource{{Identifier: "Work"}, {Identifier: "home"}}}
	twoNotion := &config.Config{NotionMeetings: []config.NotionMeetingsSource{{Identifier: "Work"}, {Identifier: "home"}}}
	twoMuesli := &config.Config{Muesli: []config.MuesliSource{{Identifier: "Work"}, {Identifier: "home"}}}
	for _, tc := range []struct {
		name    string
		resolve resolver
		args    []string
		cfg     *config.Config
		wantIDs []string
		wantErr string
	}{
		{name: "granola nil config", resolve: granolaOne, wantErr: "configuration is unavailable"},
		{name: "granola empty", resolve: granolaOne, cfg: &config.Config{}, wantErr: "no [[granola]] sources configured\n\n" + granolaConfigHint},
		{name: "granola unknown", resolve: granolaOne, args: []string{"laptop"}, cfg: twoGranola, wantErr: `no [[granola]] entry with identifier "laptop" (configured: Work, home)`},
		{name: "granola multiple", resolve: granolaOne, cfg: twoGranola, wantErr: "multiple [[granola]] sources configured; pass an identifier"},
		{name: "granola named", resolve: granolaOne, args: []string{"WORK"}, cfg: twoGranola, wantIDs: []string{"Work"}},
		{name: "circleback nil config", resolve: circlebackOne, wantErr: "configuration is unavailable"},
		{name: "circleback empty", resolve: circlebackOne, cfg: &config.Config{}, wantErr: "no [[circleback]] sources configured\n\n" + circlebackConfigHint},
		{name: "circleback unknown", resolve: circlebackOne, args: []string{"laptop"}, cfg: twoCircleback, wantErr: `no [[circleback]] entry with identifier "laptop" (configured: Work, home)`},
		{name: "circleback multiple", resolve: circlebackOne, cfg: twoCircleback, wantErr: "multiple [[circleback]] sources configured; pass an identifier"},
		{name: "circleback named", resolve: circlebackOne, args: []string{"WORK"}, cfg: twoCircleback, wantIDs: []string{"Work"}},
		{name: "notion nil config", resolve: notionOne, wantErr: "configuration is unavailable"},
		{name: "notion empty", resolve: notionOne, cfg: &config.Config{}, wantErr: "no [[notion_meetings]] sources configured\n\n" + notionMeetingsConfigHint},
		{name: "notion unknown", resolve: notionOne, args: []string{"laptop"}, cfg: twoNotion, wantErr: `no [[notion_meetings]] entry with identifier "laptop" (configured: Work, home)`},
		{name: "notion multiple", resolve: notionOne, cfg: twoNotion, wantErr: "multiple [[notion_meetings]] sources configured; pass an identifier"},
		{name: "notion named", resolve: notionOne, args: []string{"WORK"}, cfg: twoNotion, wantIDs: []string{"Work"}},
		{name: "muesli nil config", resolve: muesliSelected, wantErr: "configuration is unavailable"},
		{name: "muesli empty", resolve: muesliSelected, cfg: &config.Config{}, wantErr: "no [[muesli]] sources configured\n\n" + muesliConfigHint},
		{name: "muesli unknown", resolve: muesliSelected, args: []string{"laptop"}, cfg: twoMuesli, wantErr: `no [[muesli]] entry with identifier "laptop" (configured: Work, home)`},
		{name: "muesli all", resolve: muesliSelected, cfg: twoMuesli, wantIDs: []string{"Work", "home"}},
		{name: "muesli named", resolve: muesliSelected, args: []string{"WORK"}, cfg: twoMuesli, wantIDs: []string{"Work"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := tc.resolve(tc.args, tc.cfg)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantIDs, ids)
		})
	}

	t.Run("muesli add with multiple sources", func(t *testing.T) {
		markDaemonCLISubprocessForTest(t)
		cmd := &cobra.Command{}
		cmd.SetContext(testInvocationContext(t.Context(), twoMuesli, invocationOptions{}))

		err := addMuesliCmd.RunE(cmd, nil)

		require.EqualError(t, err, "multiple [[muesli]] sources configured; pass an identifier")
	})

	t.Run("scheduled sync of an unregistered source", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		ctx := testInvocationContext(context.Background(), config.NewDefaultConfig(), invocationOptions{})

		require.EqualError(runConfiguredGranolaSync(ctx, st, config.GranolaSource{Identifier: "work", APIKey: "grn_test"}),
			`granola source "work" is not registered; run msgvault add-granola work`)
		require.EqualError(runConfiguredCirclebackSync(ctx, st, config.CirclebackSource{Identifier: "work"}),
			`circleback source "work" is not registered; run msgvault add-circleback work first`)
		require.EqualError(runConfiguredMuesliSync(ctx, st, config.MuesliSource{Identifier: "work"}),
			`muesli source "work" is not registered; run msgvault add-muesli work first`)
		require.EqualError(runConfiguredNotionMeetingsSync(ctx, st, config.NotionMeetingsSource{Identifier: "work", Token: "ntn_test"}),
			`notion meeting source "work" is not registered; run msgvault add-notion-meetings work first`)
	})
}

func TestMeetingSourcesResolve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := &config.Config{
		Granola:    []config.GranolaSource{{Identifier: "Work"}},
		Circleback: []config.CirclebackSource{{Identifier: "Work"}, {Identifier: "home"}},
	}
	granola, err := granolaSources(cfg).selected(nil)
	require.NoError(err)
	ids, _ := meetingSourceIDs(granola, err, func(s config.GranolaSource) string { return s.Identifier })
	assert.Equal([]string{"Work"}, ids, "a single entry is selected without an argument")
	circleback, err := circlebackSources(cfg).selected(nil)
	require.NoError(err)
	ids, _ = meetingSourceIDs(circleback, err, func(s config.CirclebackSource) string { return s.Identifier })
	assert.Equal([]string{"Work", "home"}, ids, "several entries are all selected without an argument")
	only, err := granolaSources(cfg).one(nil)
	require.NoError(err)
	only.Identifier = "changed"
	assert.Equal("Work", cfg.Granola[0].Identifier, "one returns a copy, not the configured entry")
}

func TestMeetingSyncRunFinish(t *testing.T) {
	importErr := errors.New("provider failed")
	refreshErr := errors.New("refresh failed")
	live := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name          string
		provider      string
		writes        int64
		importErr     error
		cancelErr     error
		refreshErr    error
		wantErr       string
		wantRefreshes int
	}{
		{name: "clean run", provider: "granola", writes: 1},
		{name: "granola failure with writes", provider: "granola", writes: 1, importErr: importErr,
			wantErr: "granola sync work failed: provider failed", wantRefreshes: 1},
		{name: "muesli failure without writes", provider: "muesli", importErr: importErr,
			wantErr: "muesli sync work failed: provider failed"},
		{name: "notion failure with refresh error", provider: "notion meetings", writes: 2, importErr: importErr, refreshErr: refreshErr,
			wantErr: "notion meetings sync work failed: provider failed\nrefresh failed", wantRefreshes: 1},
		{name: "circleback canceled context", provider: "circleback", writes: 1, cancelErr: circlebackCanceled(canceled, nil),
			wantErr: "circleback sync work canceled: context canceled", wantRefreshes: 1},
		{name: "circleback wrapped cancel under a live context", provider: "circleback",
			importErr: fmt.Errorf("get transcripts: %w", context.Canceled),
			cancelErr: circlebackCanceled(live, fmt.Errorf("get transcripts: %w", context.Canceled)),
			wantErr:   "circleback sync work canceled: context canceled"},
		{name: "circleback failure", provider: "circleback", importErr: importErr, cancelErr: circlebackCanceled(live, importErr),
			wantErr: "circleback sync work failed: provider failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			refreshes := 0

			run := meetingSyncRun{
				provider: tc.provider, identifier: "work", writes: tc.writes,
				err: tc.importErr, canceled: tc.cancelErr,
			}
			err := run.finish(func() error {
				refreshes++
				return tc.refreshErr
			})

			if tc.wantErr == "" {
				require.NoError(err)
			} else {
				require.EqualError(err, tc.wantErr)
			}
			if tc.cancelErr != nil {
				require.ErrorIs(err, context.Canceled)
			}
			assert.Equal(t, tc.wantRefreshes, refreshes)
		})
	}
}
