package api

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopFreshnessUsesCurrentConfiguredAccounts(t *testing.T) {
	fixture := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Loops.Enabled = true
	service, err := loops.NewService(fixture.Store, cfg, nil)
	require.NoError(t, err)
	server := NewServerWithOptions(ServerOptions{
		Config:      cfg,
		Store:       fixture.Store,
		LoopService: service,
		Logger:      slog.New(slog.DiscardHandler),
	})

	account := config.AccountSchedule{Email: "new@example.test", Enabled: true}
	server.cfgMu.Lock()
	server.cfg.Accounts = append(server.cfg.Accounts, account)
	server.cfgMu.Unlock()

	report, err := service.Freshness(t.Context(), time.Now().UTC(), nil)
	require.NoError(t, err)
	var found bool
	for _, source := range report.Sources {
		if source.Identifier == account.Email {
			found = true
			assert.Nil(t, source.SourceID)
			assert.True(t, source.Stale)
		}
	}
	assert.True(t, found, "freshness must include configured accounts added after service construction")
}
