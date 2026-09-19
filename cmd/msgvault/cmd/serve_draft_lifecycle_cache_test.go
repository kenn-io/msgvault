package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftReplyOutputFailureRefreshesCache(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache rebuild uses a SQLite snapshot")
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	cacheRoot := t.TempDir()
	cacheDB := filepath.Join(cacheRoot, "cache.db")
	analyticsDir := filepath.Join(cacheRoot, "analytics")
	checkSourceUnlocked := adapter.draftCacheRefresh
	adapter.draftCacheRefresh = func(ctx context.Context, label string) error {
		if err := checkSourceUnlocked(ctx, label); err != nil {
			return err
		}
		if err := os.Remove(cacheDB); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := fixture.store.BackupDatabase(cacheDB); err != nil {
			return err
		}
		_, err := buildCache(cacheDB, analyticsDir, true)
		return err
	}
	requirements.NoError(adapter.draftCacheRefresh(t.Context(), fixture.source.Identifier))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var created draftReplyOutput
	err := adapter.runCLIReplyDraft(ctx, api.CLIRunRequest{
		Args: []string{"draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "createdcache", "--json"},
	}, func(event api.CLIRunEvent) error {
		requirements.NoError(json.Unmarshal([]byte(event.Data), &created))
		cancel()
		return errors.New("output disconnected")
	})
	requirements.ErrorContains(err, "output_failed")
	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	results, err := engine.SearchFast(t.Context(), search.Parse("createdcache"), query.MessageFilter{}, 100, 0)
	requirements.NoError(err)
	requirements.Len(results, 1, "created draft must reach search despite output failure and cancellation")
	assertions.Equal(draft.CurrentMessageID, results[0].ID)
}

func TestDraftEditCancelledAfterPublicationRefreshesCache(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache rebuild uses a SQLite snapshot")
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	created := createReviewDraft(t, fixture, adapter, "original body")
	cacheRoot := t.TempDir()
	cacheDB := filepath.Join(cacheRoot, "cache.db")
	analyticsDir := filepath.Join(cacheRoot, "analytics")
	checkSourceUnlocked := adapter.draftCacheRefresh
	adapter.draftCacheRefresh = func(ctx context.Context, label string) error {
		if err := checkSourceUnlocked(ctx, label); err != nil {
			return err
		}
		if err := os.Remove(cacheDB); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := fixture.store.BackupDatabase(cacheDB); err != nil {
			return err
		}
		_, err := buildCache(cacheDB, analyticsDir, true)
		return err
	}
	requirements.NoError(adapter.draftCacheRefresh(t.Context(), fixture.source.Identifier))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	previous := slog.Default()
	slog.SetDefault(slog.New(reviewDraftCommitHandler{
		Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
		onCommit: func() {
			draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
			if err == nil && draft.Revision == 2 {
				cancel()
			}
		},
	}))
	defer slog.SetDefault(previous)
	err := adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{
		Args: []string{api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "publishedcache", "--json"},
	}, func(api.CLIRunEvent) error { return nil })
	requirements.ErrorContains(err, "cancelled")
	requirements.ErrorIs(ctx.Err(), context.Canceled)
	slog.SetDefault(previous)
	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	requirements.NoError(err)
	requirements.NotNil(draft.Pending)
	assertions.Equal(store.IMAPDraftCodeCleanup, draft.Pending.Code)
	assertions.Equal(int64(2), draft.Revision)
	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	results, err := engine.SearchFast(t.Context(), search.Parse("publishedcache"), query.MessageFilter{}, 100, 0)
	requirements.NoError(err)
	requirements.Len(results, 1)
	assertions.Equal(draft.CurrentMessageID, results[0].ID)
	rows, err := engine.Aggregate(t.Context(), query.ViewLabels, query.DefaultAggregateOptions())
	requirements.NoError(err)
	var draftCount int64
	for _, row := range rows {
		if row.Key == "Drafts" {
			draftCount = row.Count
		}
	}
	assertions.Equal(int64(2), draftCount, "both published and pending predecessor drafts remain visible")
}

func TestDraftDeleteOutputFailureRefreshesCache(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache rebuild uses a SQLite snapshot")
	for _, resume := range []bool{false, true} {
		name := "normal deletion"
		if resume {
			name = "pending completion"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture := newDraftReplyFixture(t)
			adapter := fixture.grantedAdapter()
			created := createReviewDraft(t, fixture, adapter, "deletedcache")
			if resume {
				_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), created.DraftID, 1, store.IMAPDraftOperationDelete, nil)
				requirements.NoError(err)
				requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), created.DraftID, 1, store.IMAPDraftCodeRemoved, nil))
			}
			cacheRoot := t.TempDir()
			cacheDB := filepath.Join(cacheRoot, "cache.db")
			analyticsDir := filepath.Join(cacheRoot, "analytics")
			checkSourceUnlocked := adapter.draftCacheRefresh
			adapter.draftCacheRefresh = func(ctx context.Context, label string) error {
				if err := checkSourceUnlocked(ctx, label); err != nil {
					return err
				}
				if err := os.Remove(cacheDB); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if err := fixture.store.BackupDatabase(cacheDB); err != nil {
					return err
				}
				_, err := buildCache(cacheDB, analyticsDir, true)
				return err
			}
			requirements.NoError(adapter.draftCacheRefresh(t.Context(), fixture.source.Identifier))
			engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
			requirements.NoError(err)
			results, err := engine.SearchFast(t.Context(), search.Parse("deletedcache"), query.MessageFilter{HideDeletedFromSource: true}, 100, 0)
			requirements.NoError(err)
			requirements.Len(results, 1)
			requirements.NoError(engine.Close())

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err = adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{
				Args: []string{api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "1", "--json"},
			}, func(api.CLIRunEvent) error {
				cancel()
				return errors.New("output disconnected")
			})
			requirements.ErrorContains(err, "output_failed")
			draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
			requirements.NoError(err)
			requirements.NotNil(draft.DiscardedAt)
			engine, err = query.NewDuckDBEngine(analyticsDir, "", nil)
			requirements.NoError(err)
			t.Cleanup(func() { _ = engine.Close() })
			results, err = engine.SearchFast(t.Context(), search.Parse("deletedcache"), query.MessageFilter{HideDeletedFromSource: true}, 100, 0)
			requirements.NoError(err)
			assertions.Empty(results, "completed deletion must reach search despite output failure and cancellation")
		})
	}
}
