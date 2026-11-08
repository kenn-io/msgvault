package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type iCloudAccountFixture struct {
	controller   *CardDAVController
	partition    string
	discovered   []string
	syncCalls    int
	syncOptions  carddav.SyncOptions
	syncErr      error
	partitionErr error
	discoverHook func(string)
}

type iCloudCandidateFixture struct {
	*carddav.Service

	fixture *iCloudAccountFixture
}

func parseCardDAVFixtureURL(raw string) *url.URL {
	value, _ := url.Parse(raw) // All callers construct fixed, synthetic HTTPS URLs.
	return value
}

func (f *iCloudCandidateFixture) DiscoverConnection(_ context.Context, base string) (carddav.Discovery, error) {
	f.fixture.discovered = append(f.fixture.discovered, base)
	if f.fixture.discoverHook != nil {
		f.fixture.discoverHook(base)
	}
	if base != carddav.ICloudDiscoveryURL && f.fixture.partitionErr != nil {
		return carddav.Discovery{}, f.fixture.partitionErr
	}
	return iCloudDiscoveryFixture(f.fixture.partition), nil
}

func (f *iCloudCandidateFixture) Sync(_ context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	f.fixture.syncCalls++
	f.fixture.syncOptions = options
	if options.OnRunStarted != nil {
		options.OnRunStarted(123)
	}
	return carddav.SyncResult{}, f.fixture.syncErr
}

func iCloudDiscoveryFixture(partition string) carddav.Discovery {
	home := "https://" + partition + "-contacts.icloud.com/synthetic/home/"
	return carddav.Discovery{
		PrincipalURL: parseCardDAVFixtureURL(carddav.ICloudDiscoveryURL + "synthetic/principal/"),
		HomeURL:      parseCardDAVFixtureURL(home),
		Books: []carddav.DiscoveredBook{{URL: parseCardDAVFixtureURL(home + "book/"), DisplayName: "Synthetic contacts",
			Capabilities: carddav.BookCapabilities{CreateKnown: true, Create: true, UpdateKnown: true, Update: true, DeleteKnown: true, Delete: true}}},
	}
}

func newICloudAccountFixture(t *testing.T, connection ...string) *iCloudAccountFixture {
	t.Helper()
	home := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = home, home
	st := testutil.NewTestStore(t)
	f := &iCloudAccountFixture{partition: "p42"}
	c := &CardDAVController{cfg: cfg, store: st}
	if len(connection) > 0 {
		c.ensureDependencies()
		selected, err := c.Select(connection[0], true)
		require.NoError(t, err)
		c = selected
	}
	c.factory = func(st *store.Store, _ config.CardDAVConfig, _ string) (cardDAVCandidate, error) {
		return &iCloudCandidateFixture{Service: carddav.NewService(st, nil).ForConnection(c.connection(), 0), fixture: f}, nil
	}
	f.controller = c
	return f
}

func iCloudAccountRequest() CardDAVAccountRequest {
	return CardDAVAccountRequest{BaseURL: carddav.ICloudDiscoveryURL, Username: "synthetic@example.net", Password: "synthetic-app-password", Enabled: new(true)}
}

func TestICloudAccountSaveBindsResolvedPartition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newICloudAccountFixture(t)
	result, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	assert.Equal("https://p42-contacts.icloud.com", result.BaseURL)
	assert.Equal(result.BaseURL, f.controller.cardDAVConfigSnapshot().BaseURL)
	credential, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.Equal(result.BaseURL, credential.BaseURL)
	assert.True(credential.ICloud)
	account, err := f.controller.store.GetCardDAVAccountByNameContext(t.Context(), config.DefaultCardDAVConnection)
	require.NoError(err)
	require.NotNil(account)
	assert.Equal(result.BaseURL, account.BaseURL)
	assert.Equal(account.ConnectionGeneration, credential.ConnectionGeneration)
	assert.Equal([]string{carddav.ICloudDiscoveryURL}, f.discovered, "reuse the complete bootstrap discovery")
}

func TestICloudAccountTestDoesNotPersist(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newICloudAccountFixture(t)
	result, err := f.controller.Test(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	assert.Equal("https://p42-contacts.icloud.com", result.BaseURL)
	assert.Empty(f.controller.cfg.CardDAV.BaseURL)
	_, err = carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.Error(err)
	account, err := f.controller.store.GetCardDAVAccountByNameContext(t.Context(), config.DefaultCardDAVConnection)
	require.NoError(err)
	assert.Nil(account)
}

func TestICloudExactReconnectAndOfflineSchedulePreserveBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newICloudAccountFixture(t)
	_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	before, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	_, err = f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	after, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.Equal(before, after)
	f.discovered = nil
	req := iCloudAccountRequest()
	req.BaseURL = after.BaseURL
	req.Password = ""
	req.Schedule = "0 */6 * * *"
	_, err = f.controller.Save(t.Context(), req)
	require.NoError(err)
	assert.Empty(f.discovered)
	after, err = carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.True(after.ICloud)
}

func TestICloudSaveRollbackKeepsPreviousOriginAndMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newICloudAccountFixture(t)
	_, err := f.controller.Save(t.Context(), iCloudAccountRequest())
	require.NoError(err)
	before, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	f.partition = "p43"
	f.controller.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		return errors.New("synthetic storage failure")
	}
	_, err = f.controller.Save(t.Context(), iCloudAccountRequest())
	require.Error(err)
	after, err := carddav.LoadCredential(f.controller.cfg.TokensDir())
	require.NoError(err)
	assert.Equal(before, after)
	assert.Equal(before.BaseURL, f.controller.cardDAVConfigSnapshot().BaseURL)
}

func TestCardDAVAccountErrorExposesOnlyTypedDiscoveryDiagnostic(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).writeCardDAVAccountError(w, errors.Join(errCardDAVUpstream, &carddav.DiscoveryOriginError{}), "generic")
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "CardDAV discovery refused an unsafe href")
}
