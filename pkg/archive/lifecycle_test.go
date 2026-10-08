package archive

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func waitForAcquisitionQueryLock(ctx context.Context, t *testing.T, db *sql.DB, blockerPID int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND wait_event_type='Lock' AND $1 = ANY(pg_blocking_pids(pid))
		)`, blockerPID).Scan(&waiting))
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "source acquisition never reached the database lock")
		}
	}
}

func TestSourceDiscoveryAndAcquisitionCancellation(t *testing.T) {
	for _, phase := range []string{"source", "collection", "recovery"} {
		for _, operation := range []string{"slack", "slack media", "discord", "bind slack", "bind discord"} {
			if phase == "recovery" && strings.HasPrefix(operation, "bind ") {
				continue
			}
			t.Run(phase+"/"+operation, func(t *testing.T) {
				require := require.New(t)
				st := testutil.NewTestStore(t)
				if !st.IsPostgreSQL() {
					t.Skip("requires PostgreSQL locks")
				}
				a := &Archive{store: st}
				sourceType, identifier := "slack", "TEXAMPLE:UEXAMPLE"
				if strings.Contains(operation, "discord") {
					sourceType, identifier = "discord", "200"
				}
				source, err := st.GetOrCreateSource(sourceType, identifier)
				require.NoError(err)
				ctx, cancelWait := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancelWait()
				tx, err := st.DB().BeginTx(ctx, nil)
				require.NoError(err)
				defer func() { _ = tx.Rollback() }()
				var blockerPID int
				require.NoError(tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
				switch phase {
				case "source":
					var id int64
					require.NoError(tx.QueryRowContext(ctx, st.Rebind("SELECT id FROM sources WHERE id=? FOR UPDATE"), source.ID).Scan(&id))
				case "collection":
					_, err = tx.ExecContext(ctx, "LOCK TABLE collection_sources IN ACCESS EXCLUSIVE MODE")
				case "recovery":
					_, err = tx.ExecContext(ctx, "LOCK TABLE sync_runs IN ACCESS EXCLUSIVE MODE")
				}
				require.NoError(err)
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/auth.test":
						_, _ = fmt.Fprint(w, `{"ok":true,"team_id":"TEXAMPLE","user_id":"UEXAMPLE"}`)
					case "/users/@me":
						_, _ = fmt.Fprint(w, `{"id":"101","bot":true}`)
					case "/guilds/200":
						_, _ = fmt.Fprint(w, `{"id":"200","name":"Example"}`)
					default:
						http.Error(w, "unexpected provider request", http.StatusBadRequest)
					}
				}))
				defer peer.Close()
				slackCredential := SlackCredential{Token: "synthetic", BaseURL: peer.URL}
				discordCredential := DiscordCredential{Token: "synthetic", BaseURL: peer.URL}
				opts := slack.ImportOptions{TeamID: "TEXAMPLE", UserID: "UEXAMPLE", AttachmentsDir: t.TempDir()}
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					defer close(done)
					var runErr error
					switch operation {
					case "slack":
						_, runErr = a.SyncSlack(runCtx, SlackSync{Credential: slackCredential, Options: opts})
					case "slack media":
						_, runErr = slack.NewImporter(st, slack.NewClient(peer.URL, "synthetic"), "TEXAMPLE").BackfillMedia(runCtx, opts)
					case "discord":
						_, runErr = a.SyncDiscord(runCtx, DiscordSync{Credential: discordCredential, Options: DiscordOptions{GuildID: "200"}})
					case "bind slack":
						_, runErr = a.BindSlack(runCtx, slackCredential, "TEXAMPLE", 0)
					case "bind discord":
						_, runErr = a.BindDiscord(runCtx, discordCredential, "200")
					}
					done <- runErr
				}()
				defer func() { cancel(); _ = tx.Rollback(); <-done }()
				waitForAcquisitionQueryLock(ctx, t, st.DB(), blockerPID)
				cancel()
				select {
				case err := <-done:
					require.ErrorIs(err, context.Canceled)
				case <-ctx.Done():
					_ = tx.Rollback()
					<-done
					require.NoError(ctx.Err(), "cancellation must return while the database lock remains held")
				}
				require.NoError(tx.Rollback())
				execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
				require.NoError(err, "cancellation must release source ownership")
				require.NoError(execution.Release())
			})
		}
	}
}

func TestPurgeSourceDeletedDuringAcquisition(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row locks")
	}
	a := &Archive{store: st}
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	var blockerPID int
	require.NoError(tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	// The initial lookup sees the committed row. Ownership acquisition waits
	// for this concurrent deletion, then observes that the source is gone.
	_, err = tx.ExecContext(ctx, st.Rebind("DELETE FROM sources WHERE id=?"), source.ID)
	require.NoError(err)
	done := make(chan error, 1)
	go func() { done <- a.PurgeSource(ctx, source.ID) }()
	waitForAcquisitionQueryLock(ctx, t, st.DB(), blockerPID)
	require.NoError(tx.Commit())
	require.NoError(<-done)
	require.NoError(a.PurgeSource(t.Context(), source.ID))
	_, err = st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.ErrorIs(err, store.ErrSourceNotFound)
}
