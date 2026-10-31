package cmd

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXVerifiedProtectedReconnectRemovesKnownOwnerChains(t *testing.T) {
	for _, tc := range []struct{ name, protection string }{{"protected", `,"protected":true`}, {"unknown", ``}} {
		t.Run(tc.name, func(t *testing.T) {
			protection := tc.protection
			f := newXCollectionFixture(t)
			// Cleanup uses the same real archive as registration, including any cache
			// rebuild or reopened backend. The provider fixture's Store is isolated.
			if f.st.IsPostgreSQL() {
				var database, schema string
				require.NoError(t, f.st.DB().QueryRow("SELECT current_database(),current_schema()").Scan(&database, &schema))
				dsn, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
				require.NoError(t, err)
				dsn.Path = "/" + database
				params := dsn.Query()
				params.Set("search_path", schema)
				dsn.RawQuery = params.Encode()
				f.cfg.Data.DatabaseURL = dsn.String()
			} else {
				var seq int
				var name, path string
				require.NoError(t, f.st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
				f.cfg.Data.DatabaseURL = path
			}
			at := time.Now().UTC().Add(-time.Hour)
			var ids []int64
			for _, post := range []struct{ id, author string }{{"301", "1001"}, {"401", "1001"}, {"501", "1002"}} {
				saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", AuthorID: post.author, PostID: post.id, ConversationID: post.id, EditHistory: []string{post.id}, Text: "synthetic private constellation", CreatedAt: at, FetchedAt: at, Availability: "public"})
				require.NoError(t, err)
				ids = append(ids, saved.MessageID)
			}
			foreign, err := f.st.GetOrCreateSource("x", "1003")
			require.NoError(t, err)
			saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "1003", AuthorID: "1001", PostID: "601", ConversationID: "601", EditHistory: []string{"601"}, Text: "foreign-only owner constellation", CreatedAt: at, FetchedAt: at, Availability: "public"})
			require.NoError(t, err)
			_, err = f.st.ResolveXPostContext(t.Context(), f.source.ID, "601")
			require.ErrorIs(t, err, store.ErrXPostNotFound, "foreign native ID must be absent from selected owner source")
			foreignBefore, err := f.st.LoadXControlContext(t.Context(), foreign.ID)
			require.NoError(t, err)
			generation := int64(7)
			disabled := "invalid_grant"
			hold := store.XModelErrorResourceShape
			foreignBefore, err = f.st.UpdateXControlContext(t.Context(), foreign.ID, foreignBefore.Revision, store.XControlPatch{OwnerID: "1003", InitializeInstance: true, Generation: &generation, CredentialError: &disabled, ModelError: &hold})
			require.NoError(t, err)
			state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			require.NoError(t, err)
			model := store.XModelErrorResourceShape
			// Preserve the independent cost-model hold, spend and source incarnation.
			state, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", InitializeInstance: true, ModelError: &model})
			require.NoError(t, err)
			f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/2/users/me", r.URL.Path)
				fmt.Fprintf(w, `{"data":{"id":"1001","username":"example_owner","name":"Example Owner"%s}}`, protection)
			}))
			t.Cleanup(provider.Close)
			d := f.deps
			d.apiBaseURL = provider.URL + "/2"
			d.httpClient = provider.Client()
			d.cleanup = NewXContentCleanup(f.st, f.cfg, nil)
			source, err := registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", d)
			require.NoError(t, err)
			assert.Equal(t, f.source.ID, source.ID)
			assert.Equal(t, int32(1), calls.Load(), "verified Me must be reused without another resource or token request")
			assert.Zero(t, f.calls.Load())
			for i, id := range ids {
				var body string
				err := f.st.DB().QueryRow(f.st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), id).Scan(&body)
				if protection != "" && i < 2 {
					assert.ErrorIs(t, err, sql.ErrNoRows)
				} else {
					require.NoError(t, err)
					assert.Equal(t, "synthetic private constellation", body)
				}
			}
			var foreignBody string
			err = f.st.DB().QueryRow(f.st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), saved.MessageID).Scan(&foreignBody)
			if protection != "" {
				assert.ErrorIs(t, err, sql.ErrNoRows, "protected Me must remove a known author post even when only another source knows its native ID")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "foreign-only owner constellation", foreignBody)
			}
			foreignAfter, err := f.st.LoadXControlContext(t.Context(), foreign.ID)
			require.NoError(t, err)
			assert.Equal(t, foreignBefore, foreignAfter)
			after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
			require.NoError(t, err)
			assert.Equal(t, state.ModelError, after.ModelError)
			assert.Equal(t, state.InstanceID, after.InstanceID)
			assert.Equal(t, state.SpendBuckets, after.SpendBuckets)
			pending, err := f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.False(t, pending)
		})
	}
}
