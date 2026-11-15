package cmd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type committingCardDAVService struct {
	scheduledCardDAVFixture

	store       *store.Store
	participant int64
}

func (f *committingCardDAVService) Sync(ctx context.Context, _ carddav.SyncOptions) (carddav.SyncResult, error) {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	if err != nil {
		return carddav.SyncResult{}, err
	}
	return carddav.SyncResult{Books: 1}, errors.New("later book failed")
}

func TestCardDAVIdentityRefreshUpdatesCache(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msgvault.db")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	cfg.Data.DatabaseURL = dbPath
	analytics := cfg.AnalyticsDir()
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}}})
	req.NoError(err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:card\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEND:VCARD\r\n")
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: books[0].CanonicalURL + "alex.vcf", RemoteUID: "card", RemoteETag: "one", RemoteBody: body, SemanticHash: "hash", DisplayName: "Alex Example", Emails: []string{"alex@example.test"}}}})
	req.NoError(err)
	id, err := st.EnsureParticipant("alex@example.test", "Alex", "example.test")
	req.NoError(err)
	other, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	req.NoError(err)
	req.NoError(st.Close())
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	req.NoError(err)
	st, err = store.Open(dbPath)
	req.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(ctx context.Context, dir string) error {
		_, err := buildCacheDerivedOnly(ctx, dbPath, dir)
		return err
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = old })
	logger := slog.New(slog.DiscardHandler)
	adapter := &storeAPIAdapter{store: st, config: cfg, analyticsDir: analytics, logger: logger}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: logger})
	duck, err := sql.Open("duckdb", "")
	req.NoError(err)
	t.Cleanup(func() { _ = duck.Close() })
	check := func(participant int64, want int) {
		t.Helper()
		var count int
		req.NoError(duck.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?) WHERE participant_id = ?`, filepath.Join(analytics, "person_display_names", "*.parquet"), participant).Scan(&count))
		req.Equal(want, count)
	}
	call := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/carddav/participant-bindings", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		srv.Router().ServeHTTP(w, request)
		req.Equal(http.StatusOK, w.Code, w.Body.String())
	}
	check(id, 0)
	call(`{}`)
	check(id, 1)
	call(`{"undo":true}`)
	check(id, 0)

	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &committingCardDAVService{store: st, participant: other}
	state := testInvocationWithConfig(cfg)
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, logger, config.DefaultCardDAVConnection, state, st))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	check(other, 1)
}
