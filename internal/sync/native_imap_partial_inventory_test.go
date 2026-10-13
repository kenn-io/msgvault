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

func TestNativeIMAPPartialInventoryPreservesQuietMailbox(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	env := newTestEnv(t)
	configureNativeIMAPEventCapture(t, env.Store)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Work": 0, "Quiet": 0})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	cfg := &imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	run := func(filtered bool) {
		var nativeOpts []imapclient.Option
		if filtered {
			nativeOpts = append(nativeOpts, imapclient.WithFolderFilter([]string{"Work"}, nil))
		}
		native := imapclient.NewClient(cfg, testutil.IMAPTestPassword, nativeOpts...)
		defer func() { Assert.NoError(t, native.Close()) }()
		_, err := New(native, env.Store, opts).Full(t.Context(), cfg.Identifier())
		Require.NoError(t, err)
	}
	run(false)
	var quietID int64
	require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Quiet'`).Scan(&quietID))
	quiet, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
	require.NoError(err)
	run(true)
	current, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
	require.NoError(err, "filtered enumeration does not prove quiet mailbox removal")
	assert.Equal(quiet.Reference, current.Reference)
	require.NoError(user.Delete("Quiet"))
	run(true)
	current, err = env.Store.GetMCPResource(t.Context(), "inbox", quietID)
	require.NoError(err, "absence outside the scanned domain is not authoritative")
	assert.Equal(quiet.Reference, current.Reference)
	run(false)
	_, err = env.Store.GetMCPResource(t.Context(), "inbox", quietID)
	assert.Error(err, "complete native inventory observes actual removal")
}
