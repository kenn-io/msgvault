package archive

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportCancellationDuringDatabaseWork(t *testing.T) {
	for _, provider := range []string{"slack", "discord"} {
		for _, phase := range []string{"statistics", "message", "cleanup", "repair"} {
			if phase == "repair" && provider != "discord" {
				continue
			}
			t.Run(provider+"/"+phase, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st := testutil.NewTestStore(t)
				if !st.IsPostgreSQL() {
					t.Skip("requires PostgreSQL locks")
				}
				a := &Archive{store: st}
				identifier := "TEXAMPLE:UEXAMPLE"
				if provider == "discord" {
					identifier = "200"
				}
				source, err := st.GetOrCreateSource(provider, identifier)
				require.NoError(err)
				ctx, cancelWait := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancelWait()
				tx, err := st.DB().BeginTx(ctx, nil)
				require.NoError(err)
				defer func() { _ = tx.Rollback() }()
				var blockerPID int
				require.NoError(tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
				if phase == "statistics" {
					_, err = tx.ExecContext(ctx, "LOCK TABLE conversations IN EXCLUSIVE MODE")
				}
				if phase == "message" {
					_, err = tx.ExecContext(ctx, "LOCK TABLE message_bodies IN EXCLUSIVE MODE")
				}
				require.NoError(err)
				if phase == "repair" {
					_, err = tx.ExecContext(ctx, "LOCK TABLE applied_migrations IN ACCESS EXCLUSIVE MODE")
					require.NoError(err)
				}
				var guildCalls atomic.Int32
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if phase == "cleanup" && (r.URL.Path == "/users.list" || r.URL.Path == "/guilds/200" && guildCalls.Add(1) == 2) {
						// The first checkpoint is committed. Block later writes and failure
						// recording, so cancellation must also bound the cleanup attempt.
						_, lockErr := tx.ExecContext(ctx, "LOCK TABLE sync_runs IN EXCLUSIVE MODE")
						if !assert.NoError(lockErr) {
							http.Error(w, "lock failed", http.StatusInternalServerError)
							return
						}
					}
					var body any
					switch r.URL.Path {
					case "/auth.test":
						body = map[string]any{"ok": true, "team_id": "TEXAMPLE", "user_id": "UEXAMPLE"}
					case "/users.list":
						body = map[string]any{"ok": true, "members": []any{}}
					case "/conversations.list":
						channels := []any{}
						if phase == "message" {
							channels = append(channels, map[string]any{"id": "CEXAMPLE", "name": "example", "is_channel": true})
						}
						body = map[string]any{"ok": true, "channels": channels}
					case "/conversations.members":
						body = map[string]any{"ok": true, "members": []string{}}
					case "/conversations.history":
						body = map[string]any{"ok": true, "has_more": false, "messages": []any{map[string]any{"type": "message", "ts": "1704110400.000001", "user": "UAUTHOR", "text": "Example message"}}}
					case "/users/@me":
						body = map[string]any{"id": "101", "bot": true}
					case "/guilds/200":
						body = map[string]any{"id": "200", "name": "Example"}
					case "/guilds/200/channels":
						body = []any{}
						if phase == "message" {
							body = []any{map[string]any{"id": "301", "guild_id": "200", "type": 0, "name": "example", "last_message_id": "1200000000000000301"}}
						}
					case "/guilds/200/threads/active":
						body = map[string]any{"threads": []any{}}
					case "/channels/301/threads/archived/public", "/channels/301/threads/archived/private", "/channels/301/users/@me/threads/archived/private":
						body = map[string]any{"threads": []any{}, "has_more": false}
					case "/channels/301/messages":
						body = []any{map[string]any{"id": "1200000000000000301", "channel_id": "301", "guild_id": "200", "type": 0, "timestamp": "2024-01-01T12:00:00Z", "content": "Example message", "author": map[string]any{"id": "102", "username": "example-author"}}}
					default:
						assert.Fail("unexpected provider request", r.URL.Path)
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-OAuth-Scopes", "channels:read,channels:history,users:read")
					_ = json.MarshalWrite(w, body)
				}))
				defer peer.Close()
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					defer close(done)
					var runErr error
					if provider == "slack" {
						_, runErr = a.SyncSlack(runCtx, SlackSync{Credential: SlackCredential{Token: "synthetic", BaseURL: peer.URL}, Options: SlackOptions{SourceID: source.ID, NoThreads: true, NoMedia: true}})
					} else {
						after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
						if phase == "repair" {
							after = time.Time{}
						}
						_, runErr = a.SyncDiscord(runCtx, DiscordSync{Credential: DiscordCredential{Token: "synthetic", BaseURL: peer.URL}, Options: DiscordOptions{GuildID: "200", SourceID: source.ID, After: after}})
					}
					done <- runErr
				}()
				defer func() { cancel(); _ = tx.Rollback(); <-done }()
				waitForAcquisitionQueryLock(ctx, t, st.DB(), blockerPID)
				cancel()
				select {
				case runErr := <-done:
					require.ErrorIs(runErr, context.Canceled)
					if phase == "cleanup" {
						require.ErrorIs(runErr, context.DeadlineExceeded, "failure recording must have a bounded lifetime and report its timeout")
					}
				case <-ctx.Done():
					_ = tx.Rollback()
					<-done
					require.NoError(ctx.Err(), "cancellation must return while the database lock remains held")
				}
				require.NoError(tx.Rollback())
				if phase == "message" || phase == "statistics" {
					run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
					require.NoError(err)
					assert.Equal("failed", run.Status, "failure recording must survive caller cancellation")
				}
				// Recovery must be able to acquire the released ownership even if the
				// bounded failure write could not complete while sync_runs was locked.
				execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
				require.NoError(err)
				require.NoError(execution.Release())
				run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
				if phase == "repair" {
					require.ErrorIs(err, store.ErrSyncRunNotFound)
					return
				}
				require.NoError(err)
				assert.Equal("failed", run.Status)
				if phase == "message" {
					var body sql.NullString
					err = st.DB().QueryRow("SELECT body_text FROM message_bodies LIMIT 1").Scan(&body)
					assert.ErrorIs(err, sql.ErrNoRows, "canceled persistence must not write the body after the lock is released")
				}
			})
		}
	}
}
