package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Removing request selection would overwrite the default binding, config and
// discovery; using an unscoped candidate would overwrite its account's books.
func TestCardDAVMultipleConnectionsSaveAndRestart(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"default", "work"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
		require.NoError(err)
	}
	assertions.Equal("default", controller.cfg.CardDAV.Username)
	assertions.Equal("work", controller.cfg.CardDAVConnections["work"].Username)
	accounts, err := controller.store.ListCardDAVAccountsContext(t.Context())
	require.NoError(err)
	require.Len(accounts, 2)
	for _, name := range []string{"default", "work"} {
		dir, err := carddav.ConnectionTokenDir(controller.cfg.TokensDir(), name)
		require.NoError(err)
		credential, err := carddav.LoadCredential(dir)
		require.NoError(err)
		assertions.Equal(name+"-synthetic-secret", credential.Password)
		selected, err := controller.Select(name, false)
		require.NoError(err)
		books, err := selected.Current().ListBooks(t.Context())
		require.NoError(err)
		require.Len(books, 1)
		owner, err := controller.store.GetCardDAVAccountForBookContext(t.Context(), books[0].ID)
		require.NoError(err)
		assertions.Equal(name, owner.ConnectionName)
	}
}

func TestCardDAVMultipleConnectionsStartup(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.CardDAV = config.CardDAVConfig{BaseURL: "https://contacts.example/dav", Username: "default"}
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{"work": {BaseURL: "https://contacts.example/dav", Username: "work"}}
	st := testutil.NewTestStore(t)
	for _, name := range []string{"default", "work"} {
		_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: name,
			BaseURL: "https://contacts.example/dav", Username: name, PrincipalURL: "https://contacts.example/principal/", HomeURL: "https://contacts.example/books/"})
		require.NoError(err)
		tokenDir, err := carddav.ConnectionTokenDir(cfg.TokensDir(), name)
		require.NoError(err)
		require.NoError(carddav.SaveCredential(tokenDir, carddav.Credential{BaseURL: "https://contacts.example/dav", Username: name, Password: name + "-synthetic-secret", ConnectionGeneration: 1}))
	}
	require.NoError(cfg.Save())
	loaded, err := config.LoadConfigFile(mustReadMultipleConfig(t, cfg.ConfigFilePath()), cfg.HomeDir)
	require.NoError(err)
	restarted, err := NewCardDAVController(loaded, st, slog.New(slog.DiscardHandler))
	require.NoError(err)
	work, err := restarted.Select("work", false)
	require.NoError(err)
	assertions.NotNil(restarted.Current())
	assertions.NotNil(work.Current())
	require.NoError(carddav.RemoveCredential(loaded.TokensDir()))
	restarted, err = NewCardDAVController(loaded, st, slog.New(slog.DiscardHandler))
	require.NoError(err)
	assertions.Nil(restarted.Current())
	work, err = restarted.Select("work", false)
	require.NoError(err)
	assertions.NotNil(work.Current())
}

func TestCardDAVMultipleConnectionsFailedSaveRemovesOnlyNewTable(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	_, err := controller.Save(t.Context(), CardDAVAccountRequest{BaseURL: baseURL, Username: "default", Password: "default-synthetic-secret", Enabled: new(true)})
	require.NoError(err)
	before, err := os.ReadFile(controller.cfg.ConfigFilePath())
	require.NoError(err)
	controller.persistDiscovery = func(_ context.Context, _ cardDAVCandidate, _, _ string, _ carddav.Discovery, _ bool) error {
		return errors.New("synthetic storage failure")
	}
	_, err = controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL, Username: "work", Password: "work-synthetic-secret", Enabled: new(true)})
	require.Error(err)
	after, err := config.LoadConfigFile(mustReadMultipleConfig(t, controller.cfg.ConfigFilePath()), controller.cfg.HomeDir)
	require.NoError(err)
	assertions.Empty(after.CardDAVConnections)
	assertions.Equal("default", after.CardDAV.Username)
	assertions.NotEmpty(before)
	dir, err := carddav.ConnectionTokenDir(controller.cfg.TokensDir(), "work")
	require.NoError(err)
	_, err = carddav.LoadCredential(dir)
	require.ErrorIs(err, os.ErrNotExist)
	credential, err := carddav.LoadCredential(controller.cfg.TokensDir())
	require.NoError(err)
	assertions.Equal("default-synthetic-secret", credential.Password)
}

func TestCardDAVMultipleConnectionsAggregateSyncReportsActiveAndUnavailable(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"default", "work", "offline"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
		require.NoError(err)
	}
	work, err := controller.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	active, err := controller.store.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: work.ID, Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	offline, err := controller.Select("offline", false)
	require.NoError(err)
	controller.mu.Lock()
	offline.service = nil
	controller.mu.Unlock()
	result, err := controller.Sync(t.Context(), CardDAVSyncRequest{})
	require.NoError(err, "aggregate outcomes retain each failure")
	assertions.Equal("partial", result.Status)
	require.Len(result.Connections, 3)
	assertions.Equal("succeeded", result.Connections[0].Status)
	assertions.Equal("default", result.Connections[0].Connection)
	require.NotNil(result.Connections[0].RunID)
	assertions.Equal("offline", result.Connections[1].Connection)
	assertions.Equal("connection_unavailable", result.Connections[1].ErrorCode)
	assertions.Nil(result.Connections[1].RunID)
	assertions.Equal("work", result.Connections[2].Connection)
	assertions.Equal("sync_active", result.Connections[2].ErrorCode)
	assertions.Nil(result.Connections[2].RunID, "another request's run must not be reported as this request's run")
	assertions.NotEqual(active.ID, *result.Connections[0].RunID)
	runs, err := controller.store.ListCardDAVSyncRunsContext(t.Context(), 25, nil, store.AllCardDAVAccounts)
	require.NoError(err)
	assertions.Len(runs, 2, "unavailable and leased connections do not insert additional runs")
	// Explicitly selected, disabled connections retain manual sync behavior.
	_, err = controller.Save(t.Context(), CardDAVAccountRequest{Connection: "default", BaseURL: baseURL,
		Username: "default", Enabled: new(false)})
	require.NoError(err)
	selected, err := controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
	require.NoError(err)
	assertions.Empty(selected.Connections)
	assertions.Equal(1, selected.Books)
	server := cardDAVReadServer(t, controller.cfg, controller, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/sync", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	assertions.Equal("failed", result.Status, "every selected enabled connection failed, and diagnostics remain available")
	assertions.Len(result.Connections, 2)
	assertions.NotContains(response.Body.String(), "synthetic-secret")
	beforeRuns, err := controller.store.ListCardDAVSyncRunsContext(t.Context(), 25, nil, store.AllCardDAVAccounts)
	require.NoError(err)
	for _, name := range []string{"work", "offline"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Enabled: new(false)})
		require.NoError(err)
	}
	_, err = controller.Sync(t.Context(), CardDAVSyncRequest{})
	require.ErrorIs(err, carddav.ErrConnectionUnavailable)
	afterRuns, err := controller.store.ListCardDAVSyncRunsContext(t.Context(), 25, nil, store.AllCardDAVAccounts)
	require.NoError(err)
	assertions.Equal(beforeRuns, afterRuns, "no-enabled sync never creates a run")
}

func TestCardDAVMultipleConnectionsReadSelectorsAndNamedOnlyStatus(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL,
		Username: "work", Password: "work-synthetic-secret", Enabled: new(true)})
	require.NoError(err)
	server := cardDAVReadServer(t, controller.cfg, controller, nil)
	response := getCardDAVRead(t, server, "/api/v1/carddav/status")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	assertions.True(status.Available)
	assertions.True(status.Configured)
	response = getCardDAVRead(t, server, "/api/v1/carddav/connections")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var summaries CardDAVConnectionsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &summaries))
	require.Len(summaries.Connections, 1)
	assertions.Equal("work", summaries.Connections[0].Connection)
	assertions.NotContains(response.Body.String(), "synthetic-secret")
	for _, path := range []string{"status", "runs", "books"} {
		for _, selector := range []string{"unknown", "../work", "WORK"} {
			response = getCardDAVRead(t, server, "/api/v1/carddav/"+path+"?connection="+url.QueryEscape(selector))
			assertions.Equal(http.StatusBadRequest, response.Code, path+": "+response.Body.String())
		}
	}
	response = getCardDAVRead(t, server, "/api/v1/carddav/books?connection=default")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var books CardDAVBooksResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &books))
	assertions.Empty(books.Books)
	response = getCardDAVRead(t, server, "/api/v1/carddav/books")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NoError(json.Unmarshal(response.Body.Bytes(), &books))
	require.Len(books.Books, 1)
	assertions.Equal("work", books.Books[0].Connection)
}

func TestCardDAVMultipleConnectionsRouteGlobalTarget(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"default", "work"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
		require.NoError(err)
	}
	work, err := controller.Select("work", false)
	require.NoError(err)
	books, err := work.Current().ListBooks(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	global := controller.globalOperations()
	require.NoError(global.SetBookRoles(t.Context(), books[0].ID, carddav.BookRoles{WriteTarget: true, Subscribed: true, LookupSource: true}))
	var personID int64
	require.NoError(controller.store.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('example-global-target', 'Example Person') RETURNING id`).Scan(&personID))
	preview, err := global.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assertions.Equal(books[0].ID, preview.AddressBook.ID)
	assertions.Contains(preview.VCard, "UID:example-global-target")
	controller.mu.Lock()
	controller.service, work.service = nil, nil
	controller.mu.Unlock()
	view, err := controller.globalOperations().PublicationView(t.Context(), personID)
	require.NoError(err, "store-only publication views remain readable while credentials need repair")
	assertions.Equal(personID, view.PersonID)
}

func TestCardDAVConcurrentNamedSaveAndReads(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	controller.ensureDependencies()
	var group sync.WaitGroup
	var errorsMu sync.Mutex
	var operationErrors []error
	recordError := func(err error) {
		if err != nil {
			errorsMu.Lock()
			operationErrors = append(operationErrors, err)
			errorsMu.Unlock()
		}
	}
	for _, name := range []string{"work", "personal", "work", "personal"} {
		group.Go(func() {
			_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
				Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
			recordError(err)
		})
	}
	for range 4 {
		group.Go(func() {
			for range 4 {
				_, err := controller.Connections(t.Context())
				recordError(err)
				_, statusErr := controller.Status(t.Context(), "")
				recordError(statusErr)
			}
		})
	}
	group.Wait()
	require.Empty(operationErrors, "concurrent saves and reads must succeed")
	accounts, err := controller.store.ListCardDAVAccountsContext(t.Context())
	require.NoError(err)
	assertions.Len(accounts, 2)
	loaded, err := config.LoadConfigFile(mustReadMultipleConfig(t, controller.cfg.ConfigFilePath()), controller.cfg.HomeDir)
	require.NoError(err)
	assertions.Len(loaded.CardDAVConnections, 2)
}

func TestCardDAVMultipleConnectionsGoogleBindingsReuseRootOAuth(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	cfg, st := savedGoogleCardDAVFixture(t)
	workConfig := cfg.CardDAV
	workConfig.Username = "work@example.com"
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{"work": workConfig}
	require.NoError(cfg.Save())
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "work",
		BaseURL: carddav.GoogleDiscoveryURL, Username: workConfig.Username, PrincipalURL: "https://www.googleapis.com/principal/", HomeURL: "https://www.googleapis.com/contacts/"})
	require.NoError(err)
	dir, err := carddav.ConnectionTokenDir(cfg.TokensDir(), "work")
	require.NoError(err)
	require.NoError(carddav.SaveCredential(dir, carddav.Credential{Google: true, BaseURL: carddav.GoogleDiscoveryURL,
		Username: workConfig.Username, ConnectionGeneration: 1}))
	token := `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","client_id":"synthetic-client","scopes":["` + oauth.ScopeCardDAV + `"]}`
	for _, email := range []string{cfg.CardDAV.Username, workConfig.Username} {
		require.NoError(os.WriteFile(filepath.Join(cfg.TokensDir(), email+".json"), []byte(token), 0600))
	}
	controller, err := NewCardDAVController(cfg, st, testLogger())
	require.NoError(err)
	for _, name := range []string{"default", "work"} {
		status, err := controller.Status(t.Context(), name)
		require.NoError(err)
		assertions.True(status.Available)
		assertions.True(status.CredentialConfigured, name)
	}
	require.NoError(carddav.RemoveCredential(dir))
	controller, err = NewCardDAVController(cfg, st, testLogger())
	require.NoError(err)
	status, err := controller.Status(t.Context(), "default")
	require.NoError(err)
	assertions.True(status.CredentialConfigured)
	status, err = controller.Status(t.Context(), "work")
	require.NoError(err)
	assertions.Equal("credential_missing", status.RepairReason)
}

func TestCardDAVMultipleConnectionsGoogleAuthorizationSelection(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	cfg, st := savedGoogleCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st, testLogger())
	require.NoError(err)
	server := cardDAVReadServer(t, cfg, controller, nil)
	request := httptest.NewRequest(http.MethodPost, "https://archive.example/api/v1/carddav/google/authorize",
		strings.NewReader(`{"connection":"work","email":"person@example.com","redirect_uri":"https://archive.example/"}`))
	request.Header.Set("Origin", "https://archive.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var authorization CardDAVGoogleAuthorizeResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &authorization))
	assertions.Equal("work", authorization.Connection)
	assertions.Empty(cfg.CardDAVConnections, "starting a flow does not persist a new connection")
	assertions.Empty(controller.connections, "authorization does not retain a runtime for an unsaved name")
	entry, err := controller.takeGoogleAuthorizationEntry(authorization.State)
	require.NoError(err)
	assertions.Equal("work", entry.connection)
	assertions.Equal("person@example.com", entry.email)
	_, err = controller.takeGoogleAuthorizationEntry(authorization.State)
	assertions.Error(err, "each flow is consumed once")
}

func TestCardDAVMultipleConnectionsOperationAttribution(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL,
		Username: "work", Password: "work-synthetic-secret", Enabled: new(true)})
	require.NoError(err)
	account, err := controller.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	run, err := controller.store.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: account.ID, Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	server := NewServerWithOptions(ServerOptions{Config: controller.cfg, Store: controller.store, CardDAV: controller,
		OperationHistoryReader: controller.store, Logger: testLogger()})
	response := getCardDAVRead(t, server, "/api/v1/operations/status")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var status OperationStatusResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	lane := status.Lanes[0]
	assertions.True(lane.Configured, "named-only contacts are available")
	require.NotNil(lane.Active)
	assertions.Equal(account.ID, lane.Active.AccountID)
	assertions.Equal("work", lane.Active.Connection)
	assertions.Empty(lane.SupportedActions, "an active connection disables aggregate sync")
	_, err = controller.store.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded})
	require.NoError(err)
	response = getCardDAVRead(t, server, "/api/v1/operations/status")
	require.Equal(http.StatusOK, response.Code)
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	assertions.Equal([]operations.ActionID{operations.ActionCardDAVSync}, status.Lanes[0].SupportedActions)
	_, err = controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL,
		Username: "work", Enabled: new(false)})
	require.NoError(err)
	response = getCardDAVRead(t, server, "/api/v1/operations/status")
	require.Equal(http.StatusOK, response.Code)
	require.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	assertions.Empty(status.Lanes[0].SupportedActions, "an aggregate action needs an enabled usable connection")
}

func TestCardDAVMultipleConnectionsScheduleReconciliation(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"personal", "work"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Password: name + "-synthetic-secret", Enabled: new(true), Schedule: "0 3 * * *"})
		require.NoError(err)
	}
	seen := map[string]config.CardDAVConfig{}
	controller.SetConnectionScheduleReconciler(func(name string, cfg config.CardDAVConfig, service CardDAVOperations) error {
		if cfg.BaseURL != "" {
			assertions.NotNil(service)
		}
		seen[name] = cfg
		return nil
	})
	require.NoError(controller.ReconcileSchedule())
	assertions.True(seen["personal"].Enabled)
	assertions.True(seen["work"].Enabled)
	clear(seen)
	_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: "work", BaseURL: baseURL,
		Username: "work", Enabled: new(false), Schedule: "0 4 * * *"})
	require.NoError(err)
	require.Len(seen, 1, "saving a connection reconciles only its job")
	assertions.False(seen["work"].Enabled)
	assertions.Equal("0 4 * * *", seen["work"].Schedule)
}

func TestCardDAVMultipleConnectionsSelectedRetryHeader(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"default", "work"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL,
			Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
		require.NoError(err)
	}
	work, err := controller.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	require.NoError(controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(5*time.Minute), store.DefaultCardDAVAccountID))
	require.NoError(controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(90*time.Second), work.ID))
	server := cardDAVReadServer(t, controller.cfg, controller, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/sync", strings.NewReader(`{"connection":"work"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	seconds, err := strconv.ParseInt(response.Header().Get("Retry-After"), 10, 64)
	require.NoError(err)
	assertions.GreaterOrEqual(seconds, int64(1))
	assertions.LessOrEqual(seconds, int64(90), "the default connection's longer gate must not be used")
}

func TestCardDAVMultipleConnectionsUnboundSummaryOmitsAccountID(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{"work": {BaseURL: "https://contacts.example/dav", Username: "work", Enabled: true}}
	require.NoError(cfg.Save())
	controller, err := NewCardDAVController(cfg, testutil.NewTestStore(t), testLogger())
	require.NoError(err)
	response := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/connections")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "account_id", "there is no account yet; zero must not be presented as an archive identity")
}

func multipleCardDAVController(t *testing.T) (*CardDAVController, string) {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		username, password, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, username+"-synthetic-secret", password)
		switch r.URL.Path {
		case "/dav":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response><D:response><D:href>/books/shared/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Shared path</D:displayname><D:current-user-privilege-set><D:privilege><D:bind/></D:privilege><D:privilege><D:write-content/></D:privilege><D:privilege><D:unbind/></D:privilege></D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/shared/":
			writeCardDAVMultiStatus(w, "")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	dir := t.TempDir()
	controller := &CardDAVController{cfg: &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}, store: testutil.NewTestStore(t)}
	controller.factory = fixtureCardDAVFactory(t, serverURL)
	return controller, "http://contacts.example:" + serverURL.Port() + "/dav"
}

func mustReadMultipleConfig(t *testing.T, path string) config.ConfigFile {
	t.Helper()
	file, err := config.ReadConfigFile(path)
	require.NoError(t, err)
	return file
}

func TestCardDAVMultipleConnectionsTransactionalRetryHeader(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	controller, baseURL := multipleCardDAVController(t)
	for _, name := range []string{"default", "work"} {
		_, err := controller.Save(t.Context(), CardDAVAccountRequest{Connection: name, BaseURL: baseURL, Username: name, Password: name + "-synthetic-secret", Enabled: new(true)})
		require.NoError(err)
	}
	work, err := controller.store.GetCardDAVAccountByNameContext(t.Context(), "work")
	require.NoError(err)
	books, err := controller.store.ListCardDAVAddressBooksContext(t.Context(), work.ID)
	require.NoError(err)
	require.NotEmpty(books)
	require.NoError(controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(5*time.Minute), store.DefaultCardDAVAccountID))
	require.NoError(controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(90*time.Second), work.ID))
	_, err = controller.store.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: 1, AddressBookID: books[0].ID, Href: books[0].CanonicalURL + "synthetic.vcf"})
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	server := cardDAVReadServer(t, controller.cfg, controller, nil)
	response := httptest.NewRecorder()
	server.writeCardDAVOperationError(response, err, "Publication failed")
	require.Equal(http.StatusServiceUnavailable, response.Code)
	seconds, parseErr := strconv.ParseInt(response.Header().Get("Retry-After"), 10, 64)
	require.NoError(parseErr)
	assertions.GreaterOrEqual(seconds, int64(1))
	assertions.LessOrEqual(seconds, int64(90), "the transaction's owning connection must supply the retry delay")
}
