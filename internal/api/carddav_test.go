package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVAccountSaveDiscoversBeforePublishingConfigAndCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("Basic YWxpY2U6c3ludGhldGljLXBhc3N3b3Jk", r.Header.Get("Authorization"))
		if failing {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeCardDAVMultiStatus(w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response><D:response><D:href>/books/personal/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Personal</D:displayname><D:current-user-privilege-set><D:privilege><D:bind/></D:privilege></D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(err)
	baseURL := "http://contacts.example:" + serverURL.Port() + "/dav"
	home := t.TempDir()
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: dataDir}}
	st := testutil.NewTestStore(t)
	controller := &CardDAVController{cfg: cfg, store: st}
	controller.factory = fixtureCardDAVFactory(t, serverURL)
	req := CardDAVAccountRequest{BaseURL: baseURL, Username: "alice", Password: "synthetic-password", Enabled: new(true), Schedule: "0 */6 * * *"}
	_, err = controller.Save(t.Context(), req)
	require.Error(err)
	assert.NoFileExists(cfg.ConfigFilePath())
	assert.NoFileExists(filepath.Join(cfg.TokensDir(), "carddav.json"))
	failing = false
	result, err := controller.Save(t.Context(), req)
	require.NoError(err)
	assert.Equal(1, result.Books)
	password, err := carddav.LoadPassword(cfg.TokensDir())
	require.NoError(err)
	assert.Equal("synthetic-password", password)
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(filepath.Join(cfg.TokensDir(), "carddav.json"))
		require.NoError(statErr)
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	assert.NoFileExists(filepath.Join(home, "tokens", "carddav.json"))
	content, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)
	assert.NotContains(string(content), "synthetic-password")
}

func TestNewCardDAVControllerLoadsCredentialFromConfiguredDataDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	dataDir := t.TempDir()
	cfg := &config.Config{
		HomeDir: home, Data: config.DataConfig{DataDir: dataDir},
		CardDAV: config.CardDAVConfig{BaseURL: "https://contacts.example/dav", Username: "alice"},
	}
	st := testutil.NewTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books:        []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/alice/personal/"}},
	})
	require.NoError(err)
	require.NoError(carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		Password: "synthetic-password", BaseURL: cfg.CardDAV.BaseURL,
		Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}))

	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	assert.NotNil(controller.Current())
	assert.FileExists(filepath.Join(cfg.TokensDir(), "carddav.json"))
	assert.NoFileExists(filepath.Join(home, "tokens", "carddav.json"))
}

type controlledCardDAVCandidate struct {
	cardDAVListFixture

	discovery     carddav.Discovery
	discover      error
	discoverCalls atomic.Int32
}

func (f *controlledCardDAVCandidate) DiscoverConnection(context.Context, string) (carddav.Discovery, error) {
	f.discoverCalls.Add(1)
	return f.discovery, f.discover
}

func (f *controlledCardDAVCandidate) PersistDiscovery(
	context.Context, string, string, carddav.Discovery, bool,
) error {
	return nil
}

func savedCardDAVFixture(t *testing.T) (*config.Config, *store.Store, *controlledCardDAVCandidate) {
	t.Helper()
	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cfg.CardDAV = config.CardDAVConfig{
		BaseURL: "https://old.example/dav", Username: "old-user", Enabled: true, Schedule: "0 1 * * *",
	}
	require.NoError(t, cfg.Save())
	st := testutil.NewTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://old.example/dav", Username: "old-user",
		PrincipalURL: "https://old.example/principal/", HomeURL: "https://old.example/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://old.example/books/live/", DisplayName: "Old", CanCreate: new(true)}},
	})
	require.NoError(t, err)
	require.NoError(t, carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		Password: "old-password", BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}))
	service := &controlledCardDAVCandidate{discovery: carddav.Discovery{
		PrincipalURL: mustURL(t, "https://new.example/principal/"),
		HomeURL:      mustURL(t, "https://new.example/books/"),
		Books:        []carddav.DiscoveredBook{{URL: mustURL(t, "https://new.example/books/live/"), DisplayName: "New"}},
	}}
	return cfg, st, service
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}

func TestCardDAVControllerConfigurationSnapshotsDoNotMixConcurrentPublications(t *testing.T) {
	assert := assert.New(t)
	first := config.CardDAVConfig{BaseURL: "https://first.example/dav", Username: "first", Enabled: true}
	second := config.CardDAVConfig{BaseURL: "https://second.example/dav", Username: "second", Schedule: "0 2 * * *"}
	cfg := &config.Config{CardDAV: first}
	controller := &CardDAVController{cfg: cfg}

	const iterations = 10_000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range iterations {
			controller.publishCardDAVConfig(second)
			controller.publishCardDAVConfig(first)
		}
	}()
	for range iterations {
		got := controller.cardDAVConfigSnapshot()
		assert.True(got == first || got == second, "configuration snapshot was mixed: %+v", got)
	}
	<-done
}

func TestCardDAVAccountSaveRollsBackPublishedFilesWhenDiscoveryStoreFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, oldService := savedCardDAVFixture(t)
	newService := &controlledCardDAVCandidate{discovery: oldService.discovery}
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return newService, nil }
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		return errors.New("injected database failure")
	}
	beforeConfig, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: "new-user", Password: "new-password", Enabled: new(true), Schedule: "0 2 * * *",
	})
	require.ErrorContains(err, "injected database failure")

	afterConfig, readErr := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(readErr)
	assert.Equal(beforeConfig, afterConfig)
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("old-password", credential.Password)
	account, accountErr := st.GetCardDAVAccountContext(t.Context())
	require.NoError(accountErr)
	require.NotNil(account)
	assert.Equal("https://old.example/dav", account.BaseURL)
	books, booksErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(booksErr)
	require.Len(books, 1)
	assert.Equal("Old", books[0].DisplayName)
	assert.NotSame(newService, controller.Current())
}

func TestCardDAVAccountSavePreservesOldStateWhenConfigPublicationFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	persisted := false
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		persisted = true
		return nil
	}
	controller.saveConfig = func(*config.CardDAVConfig, config.CardDAVConfig) (config.CardDAVConfig, error) {
		return cfg.CardDAV, errors.New("injected config publication failure")
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: "new-user", Password: "new-password", Enabled: new(true),
	})
	require.ErrorContains(err, "injected config publication failure")
	assert.False(persisted)
	assert.Equal(config.CardDAVConfig{BaseURL: "https://old.example/dav", Username: "old-user", Enabled: true, Schedule: "0 1 * * *"}, cfg.CardDAV)
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("old-password", credential.Password)
}

func TestCardDAVAccountSaveRepublishesPreviousConfigAfterPublishThenError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	persisted := false
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		persisted = true
		return nil
	}
	beforeConfig, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)
	saveCalls := 0
	controller.saveConfig = func(expected *config.CardDAVConfig, next config.CardDAVConfig) (config.CardDAVConfig, error) {
		saveCalls++
		previous, publishErr := controller.saveCardDAVConfig(expected, next)
		require.NoError(publishErr, "exercise the real atomic config publication path")
		if saveCalls == 1 {
			return previous, errors.Join(config.ErrConfigChanged, errors.New("injected displaced-file retirement failure"))
		}
		return previous, errors.New("injected rollback retirement failure")
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: "new-user", Password: "new-password", Enabled: new(true),
	})
	require.ErrorContains(err, "injected displaced-file retirement failure")
	require.ErrorContains(err, "injected rollback retirement failure")
	assert.Equal(2, saveCalls)
	assert.False(persisted)
	assert.Equal(config.CardDAVConfig{BaseURL: "https://old.example/dav", Username: "old-user", Enabled: true, Schedule: "0 1 * * *"}, cfg.CardDAV)
	afterConfig, readErr := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(readErr)
	assert.Equal(beforeConfig, afterConfig)
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("old-password", credential.Password)
	assert.NotSame(candidate, controller.Current())
}

func TestCardDAVAccountSavePreservesConcurrentNonCardDAVConfigEdits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error { return nil }

	before, err := config.ReadConfigFile(cfg.ConfigFilePath())
	require.NoError(err)
	_, err = config.EditConfigFile(cfg.ConfigFilePath(), before.ETag, []config.Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(err)
	assert.Equal(config.WebThemeSystem, cfg.Web.Theme, "the daemon snapshot should remain stale for this regression")

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: "new-user", Password: "new-password",
		Enabled: new(false), Schedule: "0 4 * * *",
	})
	require.NoError(err)

	persisted, err := config.Load(cfg.ConfigFilePath(), "")
	require.NoError(err)
	assert.Equal(config.WebThemeDark, persisted.Web.Theme)
	assert.Equal(config.CardDAVConfig{
		BaseURL: "https://new.example/dav", Username: "new-user", Enabled: false, Schedule: "0 4 * * *",
	}, persisted.CardDAV)
}

func TestCardDAVAccountSavePreservesOldStateWhenCredentialPublicationFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	persisted := false
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		persisted = true
		return nil
	}
	controller.saveCredential = func(_ string, credential carddav.Credential) error {
		if credential.Password == "new-password" {
			return errors.New("injected credential publication failure")
		}
		return carddav.SaveCredential(cfg.TokensDir(), credential)
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: "new-user", Password: "new-password", Enabled: new(true),
	})
	require.ErrorContains(err, "injected credential publication failure")
	assert.False(persisted)
	assert.Equal("https://old.example/dav", cfg.CardDAV.BaseURL)
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("old-password", credential.Password)
}

func TestCardDAVControllerKeepsSetupAvailableForCredentialMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, st, _ := savedCardDAVFixture(t)
	require.NoError(carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		Password: "wrong-origin-password", BaseURL: "https://different.example/dav",
		Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}))

	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	assert.NotNil(controller)
	assert.Nil(controller.Current())
}

func TestCardDAVControllerMigratesLegacyPasswordWhenDurableIdentityMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, _ := savedCardDAVFixture(t)
	require.NoError(carddav.SavePassword(cfg.TokensDir(), "legacy-password"))
	tokenPath := filepath.Join(cfg.HomeDir, "tokens", "carddav.json")
	before, err := os.Stat(tokenPath)
	require.NoError(err)

	controller, err := NewCardDAVController(cfg, st)

	require.NoError(err)
	assert.NotNil(controller.Current())
	credential, err := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(err)
	assert.Equal(carddav.Credential{
		Password: "legacy-password", BaseURL: cfg.CardDAV.BaseURL,
		Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}, credential)
	after, err := os.Stat(tokenPath)
	require.NoError(err)
	assert.Equal(before.Mode().Perm(), after.Mode().Perm())
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), after.Mode().Perm())
	}
}

func TestCardDAVControllerLeavesLegacyPasswordUnboundWhenIdentityDoesNotMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, _ := savedCardDAVFixture(t)
	require.NoError(carddav.SavePassword(cfg.TokensDir(), "legacy-password"))
	cfg.CardDAV.Username = "different-user"
	require.NoError(cfg.Save())

	controller, err := NewCardDAVController(cfg, st)

	require.NoError(err)
	assert.NotNil(controller)
	assert.Nil(controller.Current())
	password, loadErr := carddav.LoadPassword(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("legacy-password", password)
	_, loadErr = carddav.LoadCredential(cfg.TokensDir())
	require.ErrorContains(loadErr, "not bound")
}

func TestCardDAVAccountSaveRepairsUnboundLegacyCredentialWithExplicitPassword(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	require.NoError(carddav.SavePassword(cfg.TokensDir(), "legacy-password"))
	cfg.CardDAV.Username = "replacement-user"
	require.NoError(cfg.Save())
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	require.Nil(controller.Current())
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
		return candidate, nil
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		Password: "replacement-password", Enabled: new(true),
	})
	require.NoError(err)
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("replacement-password", credential.Password)
	assert.Equal("replacement-user", credential.Username)
	assert.Same(candidate, controller.Current())
}

func TestCardDAVAccountSaveRestoresUnboundLegacyCredentialOnRollback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	require.NoError(carddav.SavePassword(cfg.TokensDir(), "legacy-password"))
	cfg.CardDAV.Username = "replacement-user"
	require.NoError(cfg.Save())
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
		return candidate, nil
	}
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		return errors.New("injected discovery persistence failure")
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		Password: "replacement-password", Enabled: new(true),
	})
	require.ErrorContains(err, "injected discovery persistence failure")
	password, loadErr := carddav.LoadLegacyPassword(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("legacy-password", password)
	_, loadErr = carddav.LoadCredential(cfg.TokensDir())
	require.ErrorIs(loadErr, carddav.ErrCredentialNotBound)
}

func TestCardDAVControllerConstructsManualServiceWhenSchedulingDisabled(t *testing.T) {
	cfg, st, _ := savedCardDAVFixture(t)
	cfg.CardDAV.Enabled = false
	require.NoError(t, cfg.Save())

	controller, err := NewCardDAVController(cfg, st)
	require.NoError(t, err)
	assert.NotNil(t, controller.Current())
}

func TestCardDAVAccountSaveUpdatesSchedulingWithoutDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	candidate.discover = errors.New("injected upstream outage")
	controller.service = candidate
	factoryCalled := false
	controller.factory = func(_ *store.Store, _, _, password string) (cardDAVCandidate, error) {
		factoryCalled = true
		return candidate, nil
	}
	controller.saveCredential = func(string, carddav.Credential) error {
		return errors.New("unchanged credential must not be rewritten")
	}
	var scheduledConfig config.CardDAVConfig
	var scheduledService CardDAVOperations
	controller.SetScheduleReconciler(func(cfg config.CardDAVConfig, service CardDAVOperations) error {
		scheduledConfig, scheduledService = cfg, service
		return nil
	})

	response, err := controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, Enabled: new(false), Schedule: "0 3 * * *",
	})
	require.NoError(err)
	assert.False(factoryCalled)
	assert.Equal(int32(0), candidate.discoverCalls.Load())
	assert.Equal(1, response.Books)
	assert.Equal(config.CardDAVConfig{BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, Enabled: false, Schedule: "0 3 * * *"}, scheduledConfig)
	assert.Same(candidate, scheduledService)
	assert.Same(candidate, controller.Current())
	credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(loadErr)
	assert.Equal("old-password", credential.Password)
}

func TestCardDAVAccountSaveExplicitPasswordRefreshesDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
		return candidate, nil
	}

	response, err := controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		Password: "old-password", Enabled: new(true), Schedule: cfg.CardDAV.Schedule,
	})
	require.NoError(err)
	assert.Equal(int32(1), candidate.discoverCalls.Load())
	assert.Equal(len(candidate.discovery.Books), response.Books)
	assert.Same(candidate, controller.Current())
}

func TestCardDAVAccountSaveRepairsMissingCredentialAfterStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	require.NoError(carddav.RemoveCredential(cfg.TokensDir()))
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	assert.Nil(controller.Current())
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
		return candidate, nil
	}
	controller.persistDiscovery = func(
		ctx context.Context, _ cardDAVCandidate, baseURL, username string,
		discovery carddav.Discovery, credentialsChanged bool,
	) error {
		_, _, persistErr := st.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
			BaseURL: baseURL, Username: username, CredentialsChanged: credentialsChanged,
			PrincipalURL: discovery.PrincipalURL.String(), HomeURL: discovery.HomeURL.String(),
			Books: []store.CardDAVDiscoveredBook{{
				CanonicalURL: discovery.Books[0].URL.String(),
				DisplayName:  discovery.Books[0].DisplayName,
			}},
		})
		return persistErr
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		Password: "replacement-password", Enabled: new(true), Schedule: cfg.CardDAV.Schedule,
	})
	require.NoError(err)
	assert.Same(candidate, controller.Current())
	credential, err := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(err)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(account)
	assert.Equal(account.ConnectionGeneration, credential.ConnectionGeneration)
}

func TestCardDAVAccountSavePasswordChangeAdvancesConnectionGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, candidate := savedCardDAVFixture(t)
	before, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(before)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	controller.persistDiscovery = func(ctx context.Context, _ cardDAVCandidate, baseURL, username string, discovery carddav.Discovery, credentialsChanged bool) error {
		_, _, persistErr := st.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
			BaseURL: baseURL, Username: username, CredentialsChanged: credentialsChanged,
			PrincipalURL: discovery.PrincipalURL.String(), HomeURL: discovery.HomeURL.String(),
			Books: []store.CardDAVDiscoveredBook{{
				CanonicalURL: discovery.Books[0].URL.String(), DisplayName: discovery.Books[0].DisplayName,
			}},
		})
		return persistErr
	}

	_, err = controller.Save(t.Context(), CardDAVAccountRequest{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		Password: "replacement-password", Enabled: new(true),
	})
	require.NoError(err)
	saved, err := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(err)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(account)
	assert.Equal(int64(2), saved.ConnectionGeneration)
	assert.Equal(saved.ConnectionGeneration, account.ConnectionGeneration)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: books[0].ID, ConnectionGeneration: before.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision,
	})
	require.ErrorIs(err, store.ErrCardDAVStalePlan,
		"work authenticated with the prior password must fail the advanced connection fence")
}

func TestCardDAVAccountSaveRejectsCredentialRotationBeforeDiscoveryWhenIntentIsPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *store.Store, store.CardDAVAddressBook)
	}{
		{name: "publication", seed: func(t *testing.T, st *store.Store, book store.CardDAVAddressBook) {
			t.Helper()
			var personID int64
			require.NoError(t, st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
				VALUES (?, ?) RETURNING id`), "api-rotation-publication", "API Rotation").Scan(&personID))
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
			require.NoError(t, err)
			_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
				PersonID: personID, Desired: true, AddressBookID: book.ID,
				Href:                 book.CanonicalURL + "api-rotation-publication.vcf",
				OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:api-rotation-publication\r\nFN:API Rotation\r\nEND:VCARD\r\n"),
				OutgoingSemanticHash: "semantic-api-rotation", LocalHash: snapshot.Fingerprint,
			})
			require.NoError(t, err)
		}},
		{name: "conflict", seed: func(t *testing.T, st *store.Store, book store.CardDAVAddressBook) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_conflicts (
				address_book_id, href, base_local_hash, local_hash, base_remote_hash,
				base_remote_etag, remote_etag, mapping_revision, local_body, remote_body,
				local_tombstone, remote_tombstone, pending_operation, connection_generation,
				book_sync_revision, previous_mapping_revision, pending_started_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, TRUE, FALSE, 'delete', 1, ?, ?, ?)`),
				book.ID, book.CanonicalURL+"pending-conflict.vcf", "local", "local", "remote",
				`"one"`, `"two"`, int64(2), []byte("remote-body"), book.SyncRevision, int64(1), time.Now().UTC())
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			cfg, st, candidate := savedCardDAVFixture(t)
			books, err := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			require.Len(books, 1)
			tc.seed(t, st, books[0])
			controller, err := NewCardDAVController(cfg, st)
			require.NoError(err)
			oldService := controller.Current()
			controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
				return candidate, nil
			}
			beforeConfig, err := os.ReadFile(cfg.ConfigFilePath())
			require.NoError(err)

			_, err = controller.Save(t.Context(), CardDAVAccountRequest{
				BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
				Password: "replacement-password", Enabled: new(true),
			})

			require.ErrorIs(err, store.ErrCardDAVCredentialChangePending)
			assert.Zero(candidate.discoverCalls.Load(), "blocked rotation must make no network discovery")
			afterConfig, readErr := os.ReadFile(cfg.ConfigFilePath())
			require.NoError(readErr)
			assert.Equal(beforeConfig, afterConfig)
			credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
			require.NoError(loadErr)
			assert.Equal("old-password", credential.Password)
			account, getErr := st.GetCardDAVAccountContext(t.Context())
			require.NoError(getErr)
			require.NotNil(account)
			assert.Equal(int64(1), account.ConnectionGeneration)
			assert.Same(oldService, controller.Current())
		})
	}
}

func TestCardDAVAccountSaveRejectsIdentityChangeBeforeDiscoveryWhenRemoteStateIsOwned(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *store.Store, store.CardDAVAddressBook)
	}{
		{name: "publication", seed: func(t *testing.T, st *store.Store, book store.CardDAVAddressBook) {
			t.Helper()
			var personID int64
			require.NoError(t, st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
				VALUES (?, ?) RETURNING id`), "api-identity-publication", "API Identity").Scan(&personID))
			_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications
				(person_id, desired, address_book_id, href) VALUES (?, TRUE, ?, ?)`),
				personID, book.ID, book.CanonicalURL+"api-identity-publication.vcf")
			require.NoError(t, err)
		}},
		{name: "conflict", seed: func(t *testing.T, st *store.Store, book store.CardDAVAddressBook) {
			t.Helper()
			_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_conflicts (
				address_book_id, href, base_local_hash, local_hash, base_remote_hash,
				base_remote_etag, remote_etag, mapping_revision, local_body, remote_body,
				local_tombstone, remote_tombstone
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, FALSE, FALSE)`),
				book.ID, book.CanonicalURL+"unresolved-conflict.vcf", "local", "local", "remote",
				`"one"`, `"two"`, int64(1), []byte("local-body"), []byte("remote-body"))
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			cfg, st, candidate := savedCardDAVFixture(t)
			books, err := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			require.Len(books, 1)
			tc.seed(t, st, books[0])
			controller, err := NewCardDAVController(cfg, st)
			require.NoError(err)
			oldService := controller.Current()
			controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
				return candidate, nil
			}
			beforeConfig, err := os.ReadFile(cfg.ConfigFilePath())
			require.NoError(err)

			_, err = controller.Save(t.Context(), CardDAVAccountRequest{
				BaseURL: cfg.CardDAV.BaseURL, Username: "different-user",
				Password: "replacement-password", Enabled: new(true),
			})

			require.ErrorIs(err, store.ErrCardDAVIdentityChangeOwned)
			assert.Zero(candidate.discoverCalls.Load(), "blocked identity change must make no network discovery")
			credential, loadErr := carddav.LoadCredential(cfg.TokensDir())
			require.NoError(loadErr)
			assert.Equal("old-password", credential.Password)
			afterConfig, readErr := os.ReadFile(cfg.ConfigFilePath())
			require.NoError(readErr)
			assert.Equal(beforeConfig, afterConfig)
			account, getErr := st.GetCardDAVAccountContext(t.Context())
			require.NoError(getErr)
			require.NotNil(account)
			assert.Equal("old-user", account.Username)
			assert.Same(oldService, controller.Current())
		})
	}
}

type blockingCardDAVCandidate struct {
	cardDAVListFixture

	discovery carddav.Discovery
	started   chan<- struct{}
	release   <-chan struct{}
}

func (f *blockingCardDAVCandidate) DiscoverConnection(ctx context.Context, _ string) (carddav.Discovery, error) {
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return carddav.Discovery{}, ctx.Err()
	}
	select {
	case <-f.release:
		return f.discovery, nil
	case <-ctx.Done():
		return carddav.Discovery{}, ctx.Err()
	}
}

func (f *blockingCardDAVCandidate) PersistDiscovery(context.Context, string, string, carddav.Discovery, bool) error {
	return nil
}

func TestCardDAVAccountSaveSerializesCompleteCredentialTransition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, st, fixture := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(err)
	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	secondFactoryCalled := make(chan struct{}, 1)
	first := &blockingCardDAVCandidate{discovery: fixture.discovery, started: firstStarted, release: firstRelease}
	var factoryCalls atomic.Int32
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) {
		if factoryCalls.Add(1) == 1 {
			return first, nil
		}
		secondFactoryCalled <- struct{}{}
		return fixture, nil
	}
	controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error { return nil }
	baseURL, username := cfg.CardDAV.BaseURL, cfg.CardDAV.Username

	results := make(chan error, 2)
	go func() {
		_, saveErr := controller.Save(t.Context(), CardDAVAccountRequest{
			BaseURL: baseURL, Username: username,
			Password: "first-password", Enabled: new(true),
		})
		results <- saveErr
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		require.FailNow("first save did not enter discovery")
	}
	go func() {
		_, saveErr := controller.Save(t.Context(), CardDAVAccountRequest{
			BaseURL: baseURL, Username: username,
			Password: "second-password", Enabled: new(true),
		})
		results <- saveErr
	}()
	select {
	case <-secondFactoryCalled:
		assert.Fail("second save entered the transition while the first save was in discovery")
	case <-time.After(50 * time.Millisecond):
	}
	close(firstRelease)
	for range 2 {
		require.NoError(<-results)
	}
	credential, err := carddav.LoadCredential(cfg.TokensDir())
	require.NoError(err)
	assert.Equal("second-password", credential.Password)
}

func TestCardDAVAccountRequiresPasswordWhenConnectionIdentityChanges(t *testing.T) {
	cfg, st, _ := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(t, err)

	_, err = controller.Test(t.Context(), CardDAVAccountRequest{
		BaseURL: "https://new.example/dav", Username: cfg.CardDAV.Username, Enabled: new(true),
	})
	require.ErrorIs(t, err, errCardDAVValidation)
	assert.ErrorContains(t, err, "password")
}

func writeCardDAVMultiStatus(w http.ResponseWriter, body string) {
	_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + body + `</D:multistatus>`))
}

func fixtureCardDAVFactory(t *testing.T, target *url.URL) cardDAVServiceFactory {
	t.Helper()
	resolver := fixtureCardDAVResolver(t, netip.MustParseAddr("203.0.113.9"))
	dialer := net.Dialer{}
	return func(st *store.Store, baseURL, username, password string) (cardDAVCandidate, error) {
		origin, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("parse fixture CardDAV base URL: %w", err)
		}
		client, err := carddav.NewClient(carddav.ClientOptions{CredentialOrigin: origin, Username: username, Password: password, Resolver: resolver, AllowInsecureCredentials: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target.Host)
		}})
		if err != nil {
			return nil, err
		}
		return carddav.NewService(st, client), nil
	}
}

func fixtureCardDAVResolver(t *testing.T, address netip.Addr) *net.Resolver {
	t.Helper()
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		buffer := make([]byte, 512)
		for {
			n, remote, readErr := listener.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			_, _ = listener.WriteTo(fixtureCardDAVDNSResponse(buffer[:n], address), remote)
		}
	}()
	dialer := net.Dialer{}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, listener.LocalAddr().String())
	}}
}
func fixtureCardDAVDNSResponse(request []byte, address netip.Addr) []byte {
	questionEnd := 12
	for questionEnd < len(request) && request[questionEnd] != 0 {
		questionEnd += int(request[questionEnd]) + 1
	}
	questionEnd += 5
	response := append([]byte{}, request[:2]...)
	response = append(response, 0x81, 0x80, 0x00, 0x01)
	// #nosec G602 -- questionEnd >= 4 proves both question-type indexes are in bounds.
	if address.Is4() && questionEnd >= 4 && request[questionEnd-4] == 0 && request[questionEnd-3] == 1 {
		response = append(response, 0x00, 0x01)
	} else {
		response = append(response, 0, 0)
	}
	response = append(response, 0, 0, 0, 0)
	response = append(response, request[12:questionEnd]...)
	// #nosec G602 -- questionEnd >= 4 proves both question-type indexes are in bounds.
	if address.Is4() && questionEnd >= 4 && request[questionEnd-4] == 0 && request[questionEnd-3] == 1 {
		response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0x3c, 0, 4)
		response = append(response, address.AsSlice()...)
	}
	return response
}

type cardDAVListFixture struct{ conflict store.CardDAVConflict }

func (f cardDAVListFixture) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return carddav.SyncResult{}, nil
}
func (f cardDAVListFixture) ListBooks(context.Context) ([]store.CardDAVAddressBook, error) {
	return nil, nil
}
func (f cardDAVListFixture) SetBookRoles(context.Context, int64, carddav.BookRoles) error { return nil }
func (f cardDAVListFixture) Publication(context.Context, int64) (*store.CardDAVPublication, error) {
	return nil, store.ErrCardDAVPublicationNotFound
}
func (f cardDAVListFixture) PublishPerson(context.Context, int64) error   { return nil }
func (f cardDAVListFixture) UnpublishPerson(context.Context, int64) error { return nil }
func (f cardDAVListFixture) ListConflicts(context.Context) ([]store.CardDAVConflict, error) {
	return []store.CardDAVConflict{f.conflict}, nil
}
func (f cardDAVListFixture) GetConflict(context.Context, int64) (*store.CardDAVConflict, error) {
	conflict := f.conflict
	return &conflict, nil
}
func (f cardDAVListFixture) ResolveConflict(context.Context, int64, carddav.ResolutionChoice) error {
	return errors.New("unused")
}

func TestCardDAVConflictListNeverExposesSnapshots(t *testing.T) {
	assert := assert.New(t)

	controller := &CardDAVController{service: cardDAVListFixture{conflict: store.CardDAVConflict{ID: 7, AddressBookID: 2, Href: "/books/personal/alice.vcf", Status: store.CardDAVConflictUnresolved, LocalBody: []byte("private-local-card"), RemoteBody: []byte("private-remote-card")}}}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/carddav/conflicts", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.NotContains(resp.Body.String(), "private-local-card")
	assert.NotContains(resp.Body.String(), "private-remote-card")
	assert.Contains(resp.Body.String(), `"id":7`)
}

func TestCardDAVConflictDetailExposesOnlyRequestedSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	conflict := store.CardDAVConflict{
		ID: 7, AddressBookID: 2, Href: "/books/personal/alice.vcf",
		Status:     store.CardDAVConflictUnresolved,
		LocalBody:  []byte("BEGIN:VCARD\r\nFN:Alice Local\r\nEND:VCARD\r\n"),
		RemoteBody: []byte("BEGIN:VCARD\r\nFN:Alice Remote\r\nEND:VCARD\r\n"),
	}
	resp := cardDAVRouteResponse(t, cardDAVListFixture{conflict: conflict}, http.MethodGet,
		"/api/v1/carddav/conflicts/7", "")

	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assert.Contains(resp.Body.String(), "Alice Local")
	assert.Contains(resp.Body.String(), "Alice Remote")
	assert.Contains(resp.Body.String(), `"href":"/books/personal/alice.vcf"`)
}

func TestCardDAVConflictResolutionReturnsOnlyResolvedIdentity(t *testing.T) {
	resp := cardDAVRouteResponse(t, cardDAVErrorFixture{}, http.MethodPost,
		"/api/v1/carddav/conflicts/7/resolve", `{"choice":"keep_remote"}`)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.JSONEq(t, `{"id":7,"status":"resolved"}`, resp.Body.String())
}

type cardDAVErrorFixture struct {
	cardDAVListFixture

	books       []store.CardDAVAddressBook
	booksErr    error
	rolesErr    error
	publication *store.CardDAVPublication
	pubErr      error
	mutateErr   error
	conflictErr error
	resolveErr  error
	syncResult  carddav.SyncResult
	syncErr     error
}

func (f cardDAVErrorFixture) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return f.syncResult, f.syncErr
}

func (f cardDAVErrorFixture) ListBooks(context.Context) ([]store.CardDAVAddressBook, error) {
	return f.books, f.booksErr
}
func (f cardDAVErrorFixture) SetBookRoles(context.Context, int64, carddav.BookRoles) error {
	return f.rolesErr
}
func (f cardDAVErrorFixture) Publication(context.Context, int64) (*store.CardDAVPublication, error) {
	return f.publication, f.pubErr
}
func (f cardDAVErrorFixture) PublishPerson(context.Context, int64) error   { return f.mutateErr }
func (f cardDAVErrorFixture) UnpublishPerson(context.Context, int64) error { return f.mutateErr }
func (f cardDAVErrorFixture) GetConflict(context.Context, int64) (*store.CardDAVConflict, error) {
	if f.conflictErr != nil {
		return nil, f.conflictErr
	}
	conflict := f.conflict
	return &conflict, nil
}
func (f cardDAVErrorFixture) ResolveConflict(context.Context, int64, carddav.ResolutionChoice) error {
	return f.resolveErr
}

func cardDAVRouteResponse(t *testing.T, service CardDAVOperations, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	controller := &CardDAVController{service: service}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller})
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

func TestCardDAVRoutesMapValidationMissingConflictAndStorageStatuses(t *testing.T) {
	const roles = `{"write_target":false,"subscribed":true,"lookup_source":false}`
	tests := []struct {
		name, method, path, body string
		service                  CardDAVOperations
		want                     int
	}{
		{name: "role required fields", method: http.MethodPatch, path: "/api/v1/carddav/books/7", body: `{"subscribed":true}`, service: cardDAVErrorFixture{}, want: http.StatusBadRequest},
		{name: "role missing", method: http.MethodPatch, path: "/api/v1/carddav/books/7", body: roles, service: cardDAVErrorFixture{rolesErr: store.ErrCardDAVAddressBookNotFound}, want: http.StatusNotFound},
		{name: "role conflict", method: http.MethodPatch, path: "/api/v1/carddav/books/7", body: roles, service: cardDAVErrorFixture{rolesErr: store.ErrCardDAVReadOnlyAddressBook}, want: http.StatusConflict},
		{name: "role pending mutation", method: http.MethodPatch, path: "/api/v1/carddav/books/7", body: roles, service: cardDAVErrorFixture{rolesErr: store.ErrCardDAVRoleChangePending}, want: http.StatusConflict},
		{name: "role follow-up storage", method: http.MethodPatch, path: "/api/v1/carddav/books/7", body: roles, service: cardDAVErrorFixture{booksErr: errors.New("database unavailable")}, want: http.StatusInternalServerError},
		{name: "publication missing", method: http.MethodGet, path: "/api/v1/carddav/publications/7", service: cardDAVErrorFixture{pubErr: store.ErrCardDAVPublicationNotFound}, want: http.StatusNotFound},
		{name: "person missing", method: http.MethodPost, path: "/api/v1/carddav/publications/7", service: cardDAVErrorFixture{mutateErr: store.ErrPersonNotFound}, want: http.StatusNotFound},
		{name: "publication follow-up missing", method: http.MethodPost, path: "/api/v1/carddav/publications/7", service: cardDAVErrorFixture{pubErr: store.ErrCardDAVPublicationNotFound}, want: http.StatusInternalServerError},
		{name: "conflict detail missing", method: http.MethodGet, path: "/api/v1/carddav/conflicts/7", service: cardDAVErrorFixture{conflictErr: store.ErrCardDAVConflictNotFound}, want: http.StatusNotFound},
		{name: "resolution validation", method: http.MethodPost, path: "/api/v1/carddav/conflicts/7/resolve", body: `{"choice":"invalid"}`, service: cardDAVErrorFixture{resolveErr: carddav.ErrInvalidResolutionChoice}, want: http.StatusBadRequest},
		{name: "conflict missing", method: http.MethodPost, path: "/api/v1/carddav/conflicts/7/resolve", body: `{"choice":"keep_remote"}`, service: cardDAVErrorFixture{resolveErr: store.ErrCardDAVConflictNotFound}, want: http.StatusNotFound},
		{name: "conflict stale", method: http.MethodPost, path: "/api/v1/carddav/conflicts/7/resolve", body: `{"choice":"keep_remote"}`, service: cardDAVErrorFixture{resolveErr: store.ErrCardDAVConflictStale}, want: http.StatusConflict},
		{name: "conflict storage", method: http.MethodPost, path: "/api/v1/carddav/conflicts/7/resolve", body: `{"choice":"keep_remote"}`, service: cardDAVErrorFixture{resolveErr: errors.New("database unavailable")}, want: http.StatusInternalServerError},
		{name: "sync stale", method: http.MethodPost, path: "/api/v1/carddav/sync", body: `{}`, service: cardDAVErrorFixture{syncErr: store.ErrCardDAVStalePlan}, want: http.StatusConflict},
		{name: "sync retry gate", method: http.MethodPost, path: "/api/v1/carddav/sync", body: `{}`, service: cardDAVErrorFixture{syncErr: store.ErrCardDAVRetryAfter}, want: http.StatusServiceUnavailable},
		{name: "sync storage", method: http.MethodPost, path: "/api/v1/carddav/sync", body: `{}`, service: cardDAVErrorFixture{syncErr: errors.New("database unavailable")}, want: http.StatusInternalServerError},
		{name: "sync upstream", method: http.MethodPost, path: "/api/v1/carddav/sync", body: `{}`, service: cardDAVErrorFixture{syncErr: &carddav.StatusError{StatusCode: http.StatusBadGateway}}, want: http.StatusBadGateway},
		{name: "sync upstream rate limit", method: http.MethodPost, path: "/api/v1/carddav/sync", body: `{}`, service: cardDAVErrorFixture{syncErr: &carddav.StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: 17 * time.Second}}, want: http.StatusServiceUnavailable},
		{name: "zero id", method: http.MethodGet, path: "/api/v1/carddav/publications/0", service: cardDAVErrorFixture{}, want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := cardDAVRouteResponse(t, tt.service, tt.method, tt.path, tt.body)
			assert.Equal(t, tt.want, resp.Code, resp.Body.String())
		})
	}
}

func TestCardDAVRetryGateResponseCarriesSafeRetryAfter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, st, _ := savedCardDAVFixture(t)
	retryAt := time.Now().UTC().Add(90 * time.Second)
	require.NoError(st.SetCardDAVRetryAfterContext(t.Context(), retryAt))
	controller := &CardDAVController{
		store:   st,
		service: cardDAVErrorFixture{syncErr: store.ErrCardDAVRetryAfter},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/sync", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusServiceUnavailable, resp.Code, resp.Body.String())
	retrySeconds, err := strconv.ParseInt(resp.Header().Get("Retry-After"), 10, 64)
	require.NoError(err)
	assert.GreaterOrEqual(retrySeconds, int64(1))
	assert.LessOrEqual(retrySeconds, int64(90))
}

func TestCardDAVFreshRateLimitsMapToServiceUnavailableWithRetryAfter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := &Server{}
	statusErr := &carddav.StatusError{
		StatusCode: http.StatusTooManyRequests, RetryAfter: 17 * time.Second,
	}

	operation := httptest.NewRecorder()
	srv.writeCardDAVOperationError(t.Context(), operation, statusErr, "CardDAV sync failed")
	require.Equal(http.StatusServiceUnavailable, operation.Code, operation.Body.String())
	assert.Equal("17", operation.Header().Get("Retry-After"))

	account := httptest.NewRecorder()
	srv.writeCardDAVAccountError(t.Context(), account,
		errors.Join(errCardDAVUpstream, statusErr), "CardDAV discovery failed")
	require.Equal(http.StatusServiceUnavailable, account.Code, account.Body.String())
	assert.Equal("17", account.Header().Get("Retry-After"))
}

func TestCardDAVAccountTestMapsValidationAndUpstreamFailures(t *testing.T) {
	st := testutil.NewTestStore(t)
	candidate := &controlledCardDAVCandidate{discover: errors.New("remote unavailable")}
	controller := &CardDAVController{cfg: &config.Config{HomeDir: t.TempDir()}, store: st}
	controller.factory = func(*store.Store, string, string, string) (cardDAVCandidate, error) { return candidate, nil }
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller})

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{name: "validation", body: `{"base_url":"","username":"alice","password":"secret"}`, want: http.StatusBadRequest},
		{name: "enabled required", body: `{"base_url":"https://contacts.example/dav","username":"alice","password":"secret"}`, want: http.StatusBadRequest},
		{name: "upstream", body: `{"base_url":"https://contacts.example/dav","username":"alice","password":"secret","enabled":true}`, want: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/account/test", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tc.want, resp.Code, resp.Body.String())
		})
	}
}

func TestCardDAVAccountChangeOwnershipErrorsMapConflict(t *testing.T) {
	srv := &Server{}
	for _, err := range []error{
		store.ErrCardDAVCredentialChangePending,
		store.ErrCardDAVIdentityChangeOwned,
	} {
		resp := httptest.NewRecorder()
		srv.writeCardDAVAccountError(t.Context(), resp, err, "CardDAV account change blocked")
		assert.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	}
}

func TestCardDAVAccountTestMapsCredentialStorageFailure(t *testing.T) {
	cfg, st, _ := savedCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st)
	require.NoError(t, err)
	controller.loadCredential = func(string) (carddav.Credential, error) {
		return carddav.Credential{}, errors.New("database-backed credential lookup unavailable")
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/account/test", strings.NewReader(`{
		"base_url":"https://old.example/dav","username":"old-user","enabled":true
	}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(t, http.StatusInternalServerError, resp.Code, resp.Body.String())
}

func TestCardDAVSyncResponseUsesLowercaseJSONFields(t *testing.T) {
	resp := cardDAVRouteResponse(t, cardDAVErrorFixture{syncResult: carddav.SyncResult{
		Books: 1, Created: 2, Updated: 3, Removed: 4,
	}}, http.MethodPost, "/api/v1/carddav/sync", `{}`)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.JSONEq(t, `{"books":1,"created":2,"updated":3,"removed":4}`, resp.Body.String())
}

func TestCardDAVPublicationStateRouteRequiresAuthAndReturnsState(t *testing.T) {
	controller := &CardDAVController{service: cardDAVErrorFixture{publication: &store.CardDAVPublication{
		PersonID: 11, Desired: true, PendingOperation: store.CardDAVMutationCreate, Href: "/books/personal/11.vcf",
	}}}
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-api-key"}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{}, Logger: testLogger(), CardDAV: controller})

	unauthorized := httptest.NewRecorder()
	srv.Router().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/carddav/publications/11", nil))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/carddav/publications/11", nil)
	req.Header.Set("X-Api-Key", "synthetic-api-key")
	authorized := httptest.NewRecorder()
	srv.Router().ServeHTTP(authorized, req)
	require.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())
	assert.JSONEq(t, `{"person_id":11,"desired":true,"pending_operation":"create","href":"/books/personal/11.vcf"}`, authorized.Body.String())
}

func TestCardDAVUnpublishReturnsDesiredFalseWhenPublicationIsGone(t *testing.T) {
	resp := cardDAVRouteResponse(t, cardDAVErrorFixture{
		pubErr: store.ErrCardDAVPublicationNotFound,
	}, http.MethodDelete, "/api/v1/carddav/publications/11", "")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.JSONEq(t, `{"person_id":11,"desired":false}`, resp.Body.String())
}
