package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestXRemovalDuplicateNativeContentAcrossSources(t *testing.T) {
	f := newXCleanupFixture(t)
	other, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	at := time.Now().UTC()
	duplicate, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: at, FetchedAt: at, Availability: "public", Raw: []byte(`{"post":{"id":"102","text":"erased constellation secret"}}`), Author: &store.XAuthorInput{UserID: "1002", Username: "erased_handle", DisplayName: "Erased Label", FetchedAt: at}})
	require.NoError(t, err)
	require.NotEqual(t, f.message, duplicate.MessageID)
	// Populate actual duplicate embeddings in retired, active and building
	// generations before lifecycle transitions, without running a model.
	for i := 0; i < 3; i++ {
		gen, err := f.backend.CreateGeneration(t.Context(), "duplicate-local", 3, f.cfg.Vector.GenerationFingerprint())
		require.NoError(t, err)
		f.generations = append(f.generations, gen)
		require.NoError(t, f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: f.message, Vector: []float32{1, 0, 0}}, {MessageID: duplicate.MessageID, Vector: []float32{1, 0, 0}}}))
		if i < 2 {
			require.NoError(t, f.backend.ActivateGeneration(t.Context(), gen, true))
		}
	}
	var engine query.Engine = query.NewEngine(f.st.DB(), f.st.IsPostgreSQL())
	if !f.st.IsPostgreSQL() {
		_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		require.NoError(t, err)
		duck, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		require.NoError(t, err)
		engine = duck
		t.Cleanup(func() { assert.NoError(t, duck.Close()) })
	}
	f.cfg.Server.APIKey = "example-owner-key"
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xCleanup: cleaner}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	host := httptest.NewServer(server.Router())
	defer host.Close()
	read := func(path string) (int, string) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
		response, err := host.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return response.StatusCode, string(body)
	}
	detail := fmt.Sprintf("/api/v1/messages/%d", duplicate.MessageID)
	raw := fmt.Sprintf("/api/v1/cli/message/raw?id=%d", duplicate.MessageID)
	for _, path := range []string{detail, raw} {
		status, body := read(path)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, "erased constellation secret")
	}
	args, err := json.Marshal(api.CLIRunRequest{Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source), "--remove-post", "100", "--json"}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host.URL+"/api/v1/cli/run", bytes.NewReader(args))
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := host.Client().Do(request)
	require.NoError(t, err)
	output, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode, string(output))
	status, body := read(detail)
	assert.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, "erased constellation secret")
	status, body = read(raw)
	assert.Equal(t, http.StatusNotFound, status, body)
	assert.NotContains(t, body, "erased constellation secret")
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		assert.Zero(t, stats.EmbeddingCount, "generation %d", gen)
	}
	if !f.st.IsPostgreSQL() {
		fresh, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		require.NoError(t, err)
		defer func() { assert.NoError(t, fresh.Close()) }()
		rows, err := fresh.QuerySQL(t.Context(), `SELECT * FROM read_parquet('`+strings.ReplaceAll(filepath.ToSlash(filepath.Join(f.cfg.AnalyticsDir(), "messages", "**", "*.parquet")), "'", "''")+`')`)
		require.NoError(t, err)
		encoded, err := json.Marshal(rows)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "erased constellation secret")
	}
	target, err := f.st.ResolveXPostContext(t.Context(), other.ID, "102")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalSourceOwnedCallbackDefersDuplicatesWithoutNestedLocks(t *testing.T) {
	f := newXCleanupFixture(t)
	other, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	at := time.Now().UTC()
	duplicate, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: at, FetchedAt: at, Availability: "public", Raw: []byte(`{"post":{"id":"102","text":"erased constellation secret"}}`), Author: &store.XAuthorInput{UserID: "1002", Username: "erased_handle", DisplayName: "Erased Label", FetchedAt: at}})
	require.NoError(t, err)
	f.st.DB().SetMaxOpenConns(2)
	f.st.DB().SetMaxIdleConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	execution, err := f.st.AcquireSyncExecutionContext(ctx, f.source)
	require.NoError(t, err)
	run, err := execution.StartSyncContext(ctx, "full", "")
	require.NoError(t, err)
	scoped := f.st.ScopedToSync(f.source, run)
	work, err := scoped.RemoveXPostContentContext(ctx, f.source, "100", "removed")
	require.NoError(t, err)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	_, err = cleaner.removeXContent(ctx, work)
	require.ErrorIs(t, err, store.ErrXRemovalNeedsOwner)
	// The callback's generation fence cannot mutate the foreign source. Its
	// existing readable copy remains gated by the durable global pending flag.
	body, err := f.st.GetMessageBodyText(duplicate.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "erased constellation secret", body)
	_, err = cleaner.AcquireRead(ctx)
	require.Error(t, err)
	require.NoError(t, f.st.CompleteSyncContext(ctx, run, ""))
	require.NoError(t, execution.Release())
	require.NoError(t, cleaner.Resume(ctx))
	body, err = f.st.GetMessageBodyText(duplicate.MessageID)
	require.NoError(t, err)
	assert.Empty(t, body)
	pending, err := f.st.XCleanupPendingContext(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestXRemovalNewEditLinkQueuesForeignCopyCleanup(t *testing.T) {
	for _, timing := range []string{"during cleanup", "after cleanup"} {
		t.Run(timing, func(t *testing.T) {
			f := newXCleanupFixture(t)
			other, err := f.st.GetOrCreateSource("x", "2001")
			require.NoError(t, err)
			bridge, err := f.st.GetOrCreateSource("x", "3001")
			require.NoError(t, err)
			at := time.Now().UTC()
			post := store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "104", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"104"}, Text: "newly linked native copy", CreatedAt: at, FetchedAt: at, Availability: "public", Raw: []byte(`{"post":{"id":"104","text":"newly linked native copy"}}`)}
			copy, err := f.st.PersistXPostContext(t.Context(), post)
			require.NoError(t, err)
			gen, err := f.backend.CreateGeneration(t.Context(), "late-link-local", 3, f.cfg.Vector.GenerationFingerprint())
			require.NoError(t, err)
			require.NoError(t, f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: copy.MessageID, Vector: []float32{1, 0, 0}}}))
			// A separate actual importer owns its own source/session while the
			// owner is outside SQL, doing derived cleanup under its source group.
			writer, err := store.Open(f.cfg.DatabaseDSN())
			require.NoError(t, err)
			defer func() { assert.NoError(t, writer.Close()) }()
			publishLink := func() error {
				execution, err := writer.AcquireSyncExecutionContext(t.Context(), bridge.ID)
				if err != nil {
					return err
				}
				defer func() { assert.NoError(t, execution.Release()) }()
				run, err := execution.StartSyncContext(t.Context(), "full", "")
				require.NoError(t, err)
				post.SourceID, post.OwnerID = bridge.ID, "3001"
				post.EditHistory = []string{"102", "104"}
				result, err := writer.ScopedToSync(bridge.ID, run).PersistXPostContext(t.Context(), post)
				require.NoError(t, err)
				assert.True(t, result.Skipped)
				assert.Zero(t, result.MessageID)
				return writer.CompleteSyncContext(t.Context(), run, "")
			}
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			if timing == "during cleanup" {
				cleaner.afterStore = func() error {
					cleaner.afterStore = nil
					return publishLink()
				}
				_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
				assert.ErrorIs(t, err, store.ErrXRemovalNeedsOwner)
			} else {
				_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
				require.NoError(t, err)
				require.NoError(t, publishLink())
			}
			pending, err := f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.True(t, pending, "an ID-only scoped writer must queue its newly connected foreign copy")
			release, readErr := cleaner.AcquireRead(t.Context())
			if release != nil {
				release()
			}
			assert.Error(t, readErr)
			require.NoError(t, cleaner.Resume(t.Context()))
			body, err := f.st.GetMessageBodyText(copy.MessageID)
			require.NoError(t, err)
			assert.Empty(t, body)
			stats, err := f.backend.Stats(t.Context(), gen)
			require.NoError(t, err)
			assert.Zero(t, stats.EmbeddingCount)
			pending, err = f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.False(t, pending)
		})
	}
}
