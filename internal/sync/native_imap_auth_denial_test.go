package sync

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/testutil"
	"net"
	"strconv"
	"testing"
)

func TestNativeIMAPAuthenticationDenialRetiresAuthority(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	env := newTestEnv(t)
	configureNativeIMAPEventCapture(t, env.Store)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Work": 0})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	cfg := &imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	native := imapclient.NewClient(cfg, testutil.IMAPTestPassword)
	_, err = New(native, env.Store, opts).Full(t.Context(), cfg.Identifier())
	require.NoError(err)
	require.NoError(native.Close())
	source, err := env.Store.GetSourceByTypeAndIdentifier("imap", cfg.Identifier())
	require.NoError(err)
	resource, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	require.True(allowed)
	rejected := imapclient.NewClient(cfg, "synthetic-wrong-password")
	defer func() { Assert.NoError(t, rejected.Close()) }()
	_, err = New(rejected, env.Store, opts).Full(t.Context(), cfg.Identifier())
	require.Error(err)
	allowed, err = env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "native AUTHENTICATIONFAILED cannot retain old Events authority")
	current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(resource.Reference, current.Reference, "positive native denial retires the source resource lifetime")
}
