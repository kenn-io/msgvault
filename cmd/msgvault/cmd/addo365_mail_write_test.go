package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "synthetic-client"
	ctx := withStoreResolverConfig(t, cfg)
	account := "owner@example.test"
	tokenPath := newGraphMailManager(invocationFromContext(ctx)).TokenPath(account)
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
	err := preflightAddO365Authorize(cmd, account)
	require.ErrorIs(err, requestErr)
	assert.Equal(1, requests)
	after, err := os.ReadFile(tokenPath)
	require.NoError(err)
	assert.Equal(original, after)
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
	err := preflightAddO365Authorize(cmd, "owner@example.test")
	require.ErrorContains(err, "--mail-write requires --graph")
}
