package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestAddO365GraphMailWriteAuthorization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	previousGraph, previousHeadless, previousTenant, previousIdentity := o365Graph, o365Headless, o365TenantID, noDefaultIdentityAddO365
	t.Cleanup(func() {
		o365Graph, o365Headless, o365TenantID, noDefaultIdentityAddO365 = previousGraph, previousHeadless, previousTenant, previousIdentity
	})
	cmd := newAddO365LocalCmd()
	require.NoError(cmd.Flags().Parse([]string{"--graph", "--headless", "--mail-write"}))
	dir := t.TempDir()
	cfg := lifecycleTestConfig(dir)
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	startStoreAPIDaemon(t, dir, st, nil)
	cfg.Microsoft.ClientID = "synthetic-client"
	ctx := withStoreResolverConfig(t, cfg)
	account := "owner@example.test"
	mgr, err := newGraphMailManager(invocationFromContext(ctx), nil)
	require.NoError(err)
	tokenPath := mgr.TokenPath(account)
	require.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	original := []byte(`{"access_token":"existing-read-token"}`)
	require.NoError(os.WriteFile(tokenPath, original, 0o600))
	requestErr := errors.New("synthetic device endpoint failure")
	requests := 0
	transport := testTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		assert.Equal("https://login.microsoftonline.com/common/oauth2/v2.0/devicecode", req.URL.String())
		if err := req.ParseForm(); err != nil {
			assert.Fail("parse Graph OAuth request", "%v", err)
			return nil, err
		}
		assert.Contains(strings.Fields(req.Form.Get("scope")), "https://graph.microsoft.com/Mail.ReadWrite")
		return nil, requestErr
	})
	cmd.SetContext(context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport}))
	_, err = preflightAddO365Authorize(cmd, account)
	require.ErrorIs(err, requestErr)
	assert.Equal(1, requests)
}

func TestAddO365MailWriteRequiresGraph(t *testing.T) {
	require := require.New(t)
	previousGraph, previousHeadless, previousTenant, previousIdentity := o365Graph, o365Headless, o365TenantID, noDefaultIdentityAddO365
	t.Cleanup(func() {
		o365Graph, o365Headless, o365TenantID, noDefaultIdentityAddO365 = previousGraph, previousHeadless, previousTenant, previousIdentity
	})
	cmd := newAddO365LocalCmd()
	require.NoError(cmd.Flags().Set("mail-write", "true"))
	cmd.SetContext(withStoreResolverConfig(t, lifecycleTestConfig(t.TempDir())))
	_, err := preflightAddO365Authorize(cmd, "owner@example.test")
	require.ErrorContains(err, "--mail-write requires --graph")
}

func TestAddO365RejectsOutlookPasswordSourceBeforeConsent(t *testing.T) {
	previousGraph, previousHeadless, previousApp := o365Graph, o365Headless, o365OAuthApp
	t.Cleanup(func() { o365Graph, o365Headless, o365OAuthApp = previousGraph, previousHeadless, previousApp })
	dir := t.TempDir()
	cfg := lifecycleTestConfig(dir)
	cfg.Microsoft.ClientID = "client"
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	src, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.com@outlook.office365.com:993")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), src.ID, `{"host":"outlook.office365.com","username":"user@example.com","auth_method":"password"}`))
	startStoreAPIDaemon(t, dir, st, nil)
	ctx := withStoreResolverConfig(t, cfg)
	requests := 0
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected authorization")
	})})
	cmd := newAddO365LocalCmd()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("headless", "true"))
	_, err = preflightAddO365Authorize(cmd, "user@example.com")
	require.ErrorContains(t, err, "already uses password authentication")
	assert.Zero(t, requests)
	err = runAddO365Local(cmd, []string{"user@example.com"})
	require.ErrorContains(t, err, "already uses password authentication")
	assert.Zero(t, requests)
}

func TestAddO365RejectsAmbiguousSourcesBeforeConsent(t *testing.T) {
	previousGraph, previousHeadless, previousApp := o365Graph, o365Headless, o365OAuthApp
	t.Cleanup(func() { o365Graph, o365Headless, o365OAuthApp = previousGraph, previousHeadless, previousApp })
	dir := t.TempDir()
	cfg := lifecycleTestConfig(dir)
	cfg.Microsoft.ClientID = "client"
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	for _, host := range []string{"outlook.office.com", "outlook.office365.com"} {
		src, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.com@"+host+":993")
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), src.ID, `{"username":"user@example.com","auth_method":"xoauth2"}`))
	}
	startStoreAPIDaemon(t, dir, st, nil)
	cmd := newAddO365LocalCmd()
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	_, err := preflightAddO365Authorize(cmd, "user@example.com")
	require.ErrorContains(t, err, "multiple Microsoft mail sources")
	err = runAddO365Local(cmd, []string{"user@example.com"})
	require.ErrorContains(t, err, "multiple Microsoft mail sources")
}

func TestAddO365IgnoresMergedSourceBeforeConsent(t *testing.T) {
	previousGraph, previousHeadless, previousApp := o365Graph, o365Headless, o365OAuthApp
	t.Cleanup(func() { o365Graph, o365Headless, o365OAuthApp = previousGraph, previousHeadless, previousApp })
	dir := t.TempDir()
	cfg := lifecycleTestConfig(dir)
	cfg.Microsoft.ClientID = "client"
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	var sources []*store.Source
	for _, host := range []string{"outlook.office.com", "outlook.office365.com"} {
		src, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.com@"+host+":993")
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), src.ID, `{"username":"user@example.com","auth_method":"xoauth2"}`))
		sources = append(sources, src)
	}
	_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: sources[0].ID, IntoSourceID: sources[1].ID})
	require.NoError(t, err)
	startStoreAPIDaemon(t, dir, st, nil)
	consentErr := errors.New("consent reached")
	ctx := context.WithValue(withStoreResolverConfig(t, cfg), oauth2.HTTPClient, &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) { return nil, consentErr })})
	cmd := newAddO365LocalCmd()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("headless", "true"))
	_, err = preflightAddO365Authorize(cmd, "user@example.com")
	require.ErrorIs(t, err, consentErr)
	err = runAddO365Local(cmd, []string{"user@example.com"})
	require.ErrorIs(t, err, consentErr)
}

func TestAddO365ConsentUsesExplicitApp(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(fmt.Sprintf("graph=%t", graph), func(t *testing.T) {
			oldGraph, oldHeadless, oldApp := o365Graph, o365Headless, o365OAuthApp
			t.Cleanup(func() { o365Graph, o365Headless, o365OAuthApp = oldGraph, oldHeadless, oldApp })
			dir := t.TempDir()
			cfg := lifecycleTestConfig(dir)
			cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"orgb": {ClientID: "client-b"}, "other": {ClientID: "client-c"}}
			st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
			kind, identifier := sourceTypeMSMail, "user@example.com"
			if !graph {
				kind, identifier = sourceTypeIMAP, "imaps://user@example.com@outlook.office365.com:993"
			}
			src, err := st.GetOrCreateSource(kind, identifier)
			require.NoError(t, err)
			require.NoError(t, st.UpdateSourceOAuthApp(t.Context(), src.ID, sql.NullString{String: "orgb", Valid: true}))
			if !graph {
				require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), src.ID, `{"host":"outlook.office365.com","username":"user@example.com","auth_method":"xoauth2"}`))
			}
			startStoreAPIDaemon(t, dir, st, nil)
			requestErr := errors.New("consent failed")
			ctx := withStoreResolverConfig(t, cfg)
			ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
				require.NoError(t, req.ParseForm())
				assert.Equal(t, "client-c", req.Form.Get("client_id"))
				return nil, requestErr
			})})
			cmd := newAddO365LocalCmd()
			cmd.SetContext(ctx)
			require.NoError(t, cmd.Flags().Set("headless", "true"))
			require.NoError(t, cmd.Flags().Set("oauth-app", "other"))
			if graph {
				require.NoError(t, cmd.Flags().Set("graph", "true"))
			}
			_, err = preflightAddO365Authorize(cmd, "USER@example.com")
			require.ErrorIs(t, err, requestErr)
		})
	}
}
func TestAddO365PreflightUndeterminedIMAPBinding(t *testing.T) {
	previousGraph, previousHeadless, previousTenant, previousApp := o365Graph, o365Headless, o365TenantID, o365OAuthApp
	t.Cleanup(func() {
		o365Graph, o365Headless, o365TenantID, o365OAuthApp = previousGraph, previousHeadless, previousTenant, previousApp
	})
	dir := t.TempDir()
	cfg := lifecycleTestConfig(dir)
	cfg.Microsoft.ClientID = "default"
	cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"orgb": {ClientID: "orgb-client"}}
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	_, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://owner@example.test@outlook.office365.com:993")
	require.NoError(t, err)
	startStoreAPIDaemon(t, dir, st, nil)
	ctx := withStoreResolverConfig(t, cfg)
	requestErr := errors.New("device endpoint reached")
	requests := 0
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		require.NoError(t, req.ParseForm())
		assert.Equal(t, "orgb-client", req.Form.Get("client_id"))
		return nil, requestErr
	})})
	cmd := newAddO365LocalCmd()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("headless", "true"))
	_, err = preflightAddO365Authorize(cmd, "owner@example.test")
	require.ErrorContains(t, err, "does not report the IMAP authentication method")
	assert.Zero(t, requests)
	err = runAddO365Local(cmd, []string{"owner@example.test"})
	require.ErrorContains(t, err, "cannot determine the IMAP authentication method")
	assert.Zero(t, requests)
	require.NoError(t, cmd.Flags().Set("oauth-app", "orgb"))
	_, err = preflightAddO365Authorize(cmd, "owner@example.test")
	require.ErrorIs(t, err, requestErr)
	assert.Equal(t, 1, requests)
}
