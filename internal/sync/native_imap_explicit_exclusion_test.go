package sync

import (
	"net"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeIMAPExplicitExclusionRetiresMailbox(t *testing.T) {
	for _, mode := range []string{"configured_allowlist", "explicit_skip"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			env := newTestEnv(t)
			configureNativeIMAPEventCapture(t, env.Store)
			addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Work": 0, "Quiet": 0})
			host, portText, err := net.SplitHostPort(addr)
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			cfg := &imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
			opts := DefaultOptions()
			opts.SourceType = sourceTypeIMAP
			run := func(exclude bool) {
				current := *cfg
				var nativeOpts []imapclient.Option
				if exclude {
					if mode == "configured_allowlist" {
						current.Folders = []string{"Work"}
					} else {
						nativeOpts = append(nativeOpts, imapclient.WithFolderFilter(nil, []string{"Quiet"}))
					}
				}
				native := imapclient.NewClient(&current, testutil.IMAPTestPassword, nativeOpts...)
				defer func() { Assert.NoError(t, native.Close()) }()
				_, err := New(native, env.Store, opts).Full(t.Context(), cfg.Identifier())
				Require.NoError(t, err)
			}
			run(false)
			var quietID, workID int64
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Quiet'`).Scan(&quietID))
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Work'`).Scan(&workID))
			quiet, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
			require.NoError(err)
			work, err := env.Store.GetMCPResource(t.Context(), "inbox", workID)
			require.NoError(err)
			run(true)
			_, err = env.Store.GetMCPResource(t.Context(), "inbox", quietID)
			require.Error(err, "explicit configured exclusion retires this mailbox's authority")
			currentWork, err := env.Store.GetMCPResource(t.Context(), "inbox", workID)
			require.NoError(err)
			assert.Equal(work.Reference, currentWork.Reference)
			run(false)
			restored, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
			require.NoError(err)
			assert.NotEqual(quiet.Reference, restored.Reference, "restoration cannot revive previously excluded receipt or subscription bindings")
		})
	}
}
