package query

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveDuckDBEngineCanceledStartupCleansOwnedSpill(t *testing.T) {
	requirements := require.New(t)
	spillDir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	engine, err := NewArchiveDuckDBEngine(ctx, t.TempDir(), DuckDBOptions{
		TempDirectory: spillDir, OwnTempDirectory: true,
	})
	requirements.ErrorIs(err, context.Canceled)
	requirements.Nil(engine)
	_, err = os.Stat(spillDir)
	requirements.ErrorIs(err, os.ErrNotExist)
}

func TestArchiveDuckDBEngineStartupHonorsDeadlineDuringPublication(t *testing.T) {
	requirements := require.New(t)
	builder := NewTestDataBuilder(t)
	builder.AddSource("owner@example.com")
	builder.AddMessage(MessageOpt{Subject: "Published message"})
	dir, cleanup := builder.Build()
	t.Cleanup(cleanup)
	publicationLock := flock.New(CacheBuildLockPath(dir))
	requirements.NoError(publicationLock.Lock())
	spillDir := filepath.Join(t.TempDir(), "spill")
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		engine, err := NewArchiveDuckDBEngine(ctx, dir, DuckDBOptions{
			TempDirectory: spillDir, OwnTempDirectory: true,
		})
		if engine != nil {
			_ = engine.Close()
		}
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		requirements.NoError(publicationLock.Unlock())
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			requirements.FailNow("archive engine startup did not settle")
		}
	})
	select {
	case err := <-done:
		requirements.ErrorIs(err, context.DeadlineExceeded)
	case <-time.After(10 * time.Second):
		requirements.FailNow("archive engine startup ignored its deadline while publication was locked")
	}
	_, err := os.Stat(spillDir)
	requirements.ErrorIs(err, os.ErrNotExist, "failed startup must clean up its owned spill directory")
}

func TestArchiveDuckDBEngineRestrictsSQLToAnalytics(t *testing.T) {
	requirements := require.New(t)
	t.Run("missing analytics directory", func(t *testing.T) {
		_, err := NewArchiveDuckDBEngine(t.Context(), "")
		require.Error(t, err)
	})

	builder := NewTestDataBuilder(t)
	source := builder.AddSource("owner@example.com")
	sender := builder.AddParticipant("sender@example.com", "example.com", "Sender")
	message := builder.AddMessage(MessageOpt{Subject: "Published message", SourceID: source})
	builder.AddFrom(message, sender, "Sender")
	dir, cleanup := builder.Build()
	t.Cleanup(cleanup)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	requirements.NoError(os.WriteFile(outside, []byte("synthetic outside content"), 0o600))
	engine, err := NewArchiveDuckDBEngine(t.Context(), dir)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })

	result, err := engine.QuerySQL(t.Context(), "SELECT from_email, message_count FROM v_senders")
	requirements.NoError(err)
	assert.Equal(t, [][]any{{"sender@example.com", int64(1)}}, result.Rows)

	for _, reader := range []string{"read_text", "read_blob", "read_csv_auto"} {
		t.Run(reader, func(t *testing.T) {
			_, err := engine.QuerySQL(t.Context(), fmt.Sprintf("SELECT * FROM %s('%s')", reader, escapePath(outside)))
			require.ErrorContains(t, err, "Permission Error")
		})
	}
	t.Run("escaping symlinks", func(t *testing.T) {
		for _, target := range []string{outside, filepath.Dir(outside)} {
			link := filepath.Join(dir, filepath.Base(target)+"-link")
			err := os.Symlink(target, link)
			if err != nil && runtime.GOOS == "windows" {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			require.NoError(t, err)
			if target != outside {
				link = filepath.Join(link, filepath.Base(outside))
			}
			_, err = engine.QuerySQL(t.Context(), fmt.Sprintf("SELECT content FROM read_text('%s')", escapePath(link)))
			require.ErrorContains(t, err, "Permission Error")
		}
	})
	t.Run("configuration remains locked", func(t *testing.T) {
		for _, setting := range []string{"SET enable_external_access = true", "SET memory_limit = '1GB'"} {
			_, err := engine.db.Exec(setting)
			require.Error(t, err)
		}
	})
	t.Run("owner SQL remains privileged", func(t *testing.T) {
		owner, err := NewDuckDBEngine(dir, "", nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, owner.Close()) })
		result, err := owner.QuerySQL(t.Context(), fmt.Sprintf("SELECT content FROM read_text('%s')", escapePath(outside)))
		require.NoError(t, err)
		assert.Equal(t, [][]any{{"synthetic outside content"}}, result.Rows)
	})
}

func TestArchiveDuckDBEngineFollowsPublication(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	builder.AddSource("owner@example.com")
	builder.AddMessage(MessageOpt{Subject: "Original publication"})
	dir, cleanup := builder.Build()
	t.Cleanup(cleanup)
	engine, err := NewArchiveDuckDBEngine(t.Context(), dir)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	before, err := engine.QuerySQL(t.Context(), "SELECT subject FROM messages")
	requirements.NoError(err)
	requirements.NotNil(before.Cache)
	assertions.Equal([][]any{{"Original publication"}}, before.Rows)

	writer, err := NewDuckDBEngine(dir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(writer.Close()) })
	files, err := filepath.Glob(filepath.Join(dir, "messages", "*", "*.parquet"))
	requirements.NoError(err)
	requirements.Len(files, 1)
	replacement := filepath.Join(t.TempDir(), "replacement.parquet")
	_, err = writer.db.Exec(fmt.Sprintf(
		"COPY (SELECT * EXCLUDE (message_type) REPLACE ('New publication' AS subject) FROM read_parquet('%s')) TO '%s' (FORMAT PARQUET)",
		escapePath(files[0]), escapePath(replacement)))
	requirements.NoError(err)
	publicationLock := flock.New(CacheBuildLockPath(dir))
	requirements.NoError(publicationLock.Lock())
	t.Cleanup(func() { requirements.NoError(publicationLock.Unlock()) })
	requirements.NoError(os.Rename(replacement, files[0]))
	state, err := ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.DatasetFingerprint, err = CacheDatasetFingerprint(dir)
	requirements.NoError(err)
	state.PublishedAt = state.PublishedAt.Add(time.Second)
	marker, err := json.Marshal(state)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(CacheStatePath(dir), marker, 0o600))
	requirements.NoError(publicationLock.Unlock())

	// Keep the real DuckDB connection occupied until the refreshed schema
	// probe is waiting for it, then cancel that query. The next query must
	// retry the refresh, including views that referenced the removed column.
	conn, err := engine.db.Conn(t.Context())
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	waitCount := engine.db.Stats().WaitCount
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := engine.QuerySQL(ctx, "SELECT subject FROM messages")
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			requirements.FailNow("canceled archive query did not settle")
		}
	})
	requirements.Eventually(func() bool {
		return engine.db.Stats().WaitCount > waitCount
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		requirements.ErrorIs(err, context.Canceled)
	case <-time.After(10 * time.Second):
		requirements.FailNow("archive schema refresh ignored cancellation")
	}
	requirements.NoError(conn.Close())

	after, err := engine.QuerySQL(t.Context(), "SELECT subject FROM messages")
	requirements.NoError(err)
	requirements.NotNil(after.Cache)
	assertions.Equal([][]any{{"New publication"}}, after.Rows)
	assertions.NotEqual(before.Cache.Generation, after.Cache.Generation)
	assertions.Equal(state.PublishedAt, after.Cache.PublishedAt)
}
