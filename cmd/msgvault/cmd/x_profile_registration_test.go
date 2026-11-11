package cmd

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestRegisterXDelayedPublicCannotRestoreNewerProtectedProfile(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := xTestConfig(t)
			st := testutil.NewTestStore(t)
			if existing {
				_, err := st.GetOrCreateSource("x", "1001")
				require.NoError(err)
			}
			foreign, err := st.GetOrCreateSource("x", "2001")
			require.NoError(err)
			at := time.Now().UTC().Truncate(time.Second)
			_, err = st.PersistXPostContext(t.Context(), store.XPersistPostInput{
				SourceID: foreign.ID, OwnerID: "2001", PostID: "700", AuthorID: "1001", ConversationID: "700",
				EditHistory: []string{"700"}, Text: "synthetic public post", CreatedAt: at.Add(-time.Hour), FetchedAt: at.Add(-time.Minute),
				Availability: "public", Author: &store.XAuthorInput{UserID: "1001", Username: "former_handle", DisplayName: "Former Public Label", FetchedAt: at.Add(-time.Minute)},
			})
			require.NoError(err)
			var pid int64
			require.NoError(st.DB().QueryRow(`SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1001'`).Scan(&pid))
			require.NoError(xapi.NewTokenManager(xStagedTokensDir(cfg)).Save(t.Context(), xTestRecord("example-access")))
			var clock atomic.Int64
			clock.Store(at.UnixNano())
			cleaner := NewXContentCleanup(st, cfg, nil)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/2/users/me", r.URL.Path)
				if !assert.NoError(st.QueueXAuthorRemovalContext(r.Context(), foreign.ID, "1001", at.Add(time.Minute))) {
					http.Error(w, "synthetic protection failed", http.StatusInternalServerError)
					return
				}
				if !assert.NoError(cleaner.Resume(r.Context())) {
					http.Error(w, "synthetic cleanup failed", http.StatusInternalServerError)
					return
				}
				clock.Store(at.Add(2 * time.Minute).UnixNano())
				_, err := fmt.Fprint(w, `{"data":{"id":"1001","username":"former_handle","name":"Former Public Label","protected":false}}`)
				assert.NoError(err)
			}))
			t.Cleanup(provider.Close)
			deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client(), cleanup: cleaner, oauthOptions: xapi.OAuthOptions{Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}}
			_, err = registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
			require.NoError(err)
			var name sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			assert.Empty(name.String, "the older public response cannot recreate an erased label")
			var usernames int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND source='archive_observation'`), pid).Scan(&usernames))
			assert.Zero(usernames, "no current or historical provider username may be recreated")
		})
	}
}

func TestRegisterXProtectedWithoutPostsErasesUnsupportedProfile(t *testing.T) {
	for _, curated := range []bool{false, true} {
		t.Run(fmt.Sprintf("curated=%t", curated), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := xTestConfig(t)
			st := testutil.NewTestStore(t)
			var protected atomic.Bool
			var clock atomic.Int64
			at := time.Now().UTC().Truncate(time.Second)
			clock.Store(at.UnixNano())
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := fmt.Fprintf(w, `{"data":{"id":"1001","username":"former_handle","name":"Former Public Label","protected":%t}}`, protected.Load())
				assert.NoError(err)
			}))
			t.Cleanup(provider.Close)
			deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client(), oauthOptions: xapi.OAuthOptions{Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}}
			stage := xapi.NewTokenManager(xStagedTokensDir(cfg))
			require.NoError(stage.Save(t.Context(), xTestRecord("first-example-access")))
			source, err := registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
			require.NoError(err)
			var pid int64
			require.NoError(st.DB().QueryRow(`SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1001'`).Scan(&pid))
			if curated {
				_, _, err := st.CreatePersonFromParticipant(pid)
				require.NoError(err)
			}
			protected.Store(true)
			clock.Store(at.Add(time.Minute).UnixNano())
			require.NoError(stage.Save(t.Context(), xTestRecord("replacement-example-access")))
			_, err = registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
			require.NoError(err)
			var posts, usernames, anchors int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM x_post_versions WHERE source_id=?`), source.ID).Scan(&posts))
			assert.Zero(posts, "registration never collected a post")
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND source='archive_observation'`), pid).Scan(&usernames))
			assert.Zero(usernames, "including historical superseded usernames")
			var name sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			if curated {
				assert.Equal("Former Public Label", name.String)
			} else {
				assert.Empty(name.String)
			}
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id=? AND identifier_type='x_user_id' AND identifier_value='1001'`), pid).Scan(&anchors))
			assert.Equal(1, anchors)
			pending, err := st.XCleanupPendingContext(t.Context())
			require.NoError(err)
			assert.False(pending)
		})
	}
}
