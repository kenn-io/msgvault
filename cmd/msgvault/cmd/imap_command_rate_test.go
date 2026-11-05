package cmd

import (
	"database/sql"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildAPIClientIMAPCommandRate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	imapCfg := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
	encoded, err := imapCfg.ToJSON()
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Sync.IMAPMaxCommandsPerMinute = 600
	source := &store.Source{SourceType: sourceTypeIMAP, Identifier: imapCfg.Identifier(), SyncConfig: sql.NullString{String: encoded, Valid: true}}
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), source.Identifier, testutil.IMAPTestPassword))
	ctx := withTestConfig(t, cfg)
	api, err := buildAPIClient(ctx, source, nil, nil)
	require.NoError(err)
	client, ok := api.(*imaplib.Client)
	require.True(ok)
	t.Cleanup(func() { _ = client.Close() })
	started := time.Now()
	profile, err := client.GetProfile(ctx)
	require.NoError(err)
	assert.Equal(int64(1), profile.MessagesTotal)
	assert.GreaterOrEqual(time.Since(started), 100*time.Millisecond, "factory must pace STATUS after LOGIN")
}
